package bankingcircle

import (
	"context"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// Environments (internal/tenant): each record has an owner, and a reset of
// one environment touches nothing of another's.

func TestTheDefaultSafeguardingAccountsAreTheLabsOwn(t *testing.T) {
	if SGAAccountEUR != tenant.SafeguardingAccount(tenant.Default, "EUR") ||
		SGAAccountGBP != tenant.SafeguardingAccount(tenant.Default, "GBP") {
		t.Fatalf("SGAAccountEUR/GBP = %s/%s, want tenant's %s/%s", SGAAccountEUR, SGAAccountGBP,
			tenant.DefaultSafeguardingEUR, tenant.DefaultSafeguardingGBP)
	}
}

func TestAccountIDInKeepsTheDefaultsAndKeepsEnvironmentsApart(t *testing.T) {
	key := "beneficiary-42"
	if got := AccountIDIn(tenant.Default, key); got != AccountIDFor(key) {
		t.Fatalf("the default's account for %s = %s, want AccountIDFor's %s", key, got, AccountIDFor(key))
	}
	if got := AccountIDIn("", key); got != AccountIDFor(key) {
		t.Fatalf("an unnamed environment's account = %s, want the default's", got)
	}
	sbx, dev := AccountIDIn("sbx", key), AccountIDIn("dev", key)
	if sbx == AccountIDFor(key) || dev == AccountIDFor(key) || sbx == dev {
		t.Fatalf("sbx %s, dev %s, default %s: want three accounts", sbx, dev, AccountIDFor(key))
	}
	if !ValidAccountID(sbx) {
		t.Fatalf("AccountIDIn gave %q, not a UUID", sbx)
	}
	if again := AccountIDIn("sbx", AccountIDFor(key)); again != sbx {
		t.Fatalf("AccountIDIn(sbx, AccountIDFor(key)) = %s, want AccountIDIn(sbx, key) = %s", again, sbx)
	}
}

func TestARunResetOfOneEnvironmentLeavesTheOthers(t *testing.T) {
	r := NewRun()
	started := r.Current()

	// A sleeper of each; only sbx's is woken.
	woke := map[string]chan bool{"sbx": make(chan bool, 1), tenant.Default: make(chan bool, 1)}
	for env, ch := range woke {
		go func() { ch <- r.Sleep(env, started, 300*time.Millisecond) }()
	}
	time.Sleep(10 * time.Millisecond)

	r.stop("sbx")
	release := r.hold("sbx")
	// The default's effects still run while sbx is held.
	ran := make(chan bool, 1)
	go func() { ran <- r.Do(tenant.Default, started, func(context.Context) {}) }()
	select {
	case ok := <-ran:
		if !ok {
			t.Fatal("the default's work was refused during sbx's reset")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the default's effect waited on sbx's reset")
	}
	r.next("sbx")
	release()

	select {
	case going := <-woke["sbx"]:
		if going {
			t.Fatal("sbx's sleeper was told its run is still going")
		}
	case <-time.After(time.Second):
		t.Fatal("sbx's reset did not wake sbx's sleeper")
	}
	if going := <-woke[tenant.Default]; !going {
		t.Fatal("sbx's reset ended the default's sleeper")
	}
	if !r.Moved("sbx", started) || r.Moved(tenant.Default, started) || r.Moved("dev", started) {
		t.Fatalf("moved: sbx %v, default %v, dev %v; want only sbx", r.Moved("sbx", started), r.Moved(tenant.Default, started), r.Moved("dev", started))
	}
	if r.Do("sbx", started, func(context.Context) { t.Fatal("sbx's old work ran") }) {
		t.Fatal("Do ran sbx's work from before its reset")
	}
	if !r.Do("sbx", r.Current(), func(context.Context) {}) {
		t.Fatal("Do refused sbx's work from after its reset")
	}

	// A reset of every environment ends them all, even one never seen.
	endRun(r)
	if !r.Moved(tenant.Default, started) || !r.Moved("never-seen", started) {
		t.Fatal("a reset of every environment left some work going")
	}
}

// twoEnvBank is a test bank with sbx a customer: its safeguarding account,
// a subscription of each, and a creditor account of each.
func twoEnvBank(t *testing.T) (*testBank, *Subscription) {
	t.Helper()
	b := newTestBank(t, time.Millisecond, quiet())
	b.engine.onTransition = func(p *Payment) {
		for _, rec := range b.subs.RecipientsIn(p.Env, string(p.State), p.ToAccountID, p.FromAccountID) {
			b.disp.EnqueueFrom(p.Epoch(), rec.Subscription, Notification{EventID: p.ID, NotificationType: string(p.State)})
		}
	}
	if _, err := b.ledger.OpenSafeguarding("sbx", "EUR", "Sandbox"); err != nil {
		t.Fatal(err)
	}
	sbxSub, err := b.subs.Create(CreateParams{Env: "sbx", Endpoint: "https://example.test/seed", // the seed's endpoint: no conflict across customers
		EncryptionKey: "0123456789abcdef0123456789abcdef", Status: StatusActive, MaxNotificationsPerMessage: 5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.subs.AddEvent(sbxSub.ID, string(NotificationIncomingPaymentBooked), TargetCompany, nil); err != nil {
		t.Fatal(err)
	}
	return b, sbxSub
}

func TestResetInClearsOneEnvironmentsRecordsOnly(t *testing.T) {
	b, sbxSub := twoEnvBank(t)
	sbxSGA := tenant.SafeguardingAccount("sbx", "EUR")
	for env, sga := range map[string]string{tenant.Default: SGAAccountEUR, "sbx": sbxSGA} {
		creditor := AccountIDIn(env, "merchant")
		if _, err := b.ledger.GetOrCreateIn(env, creditor, "EUR"); err != nil {
			t.Fatal(err)
		}
		if _, err := b.engine.CreditIncoming(sga, "EUR", "100.00", "lump"); err != nil {
			t.Fatal(err)
		}
		b.engine.Create(&Payment{ID: "bcp_" + env, Env: env, FromAccountID: sga, ToAccountID: creditor, Amount: "10.00", Currency: "EUR"})
	}
	b.engine.Wait()
	b.disp.Pause(sbxSub.ID)
	b.mail.Send(Email{Env: "sbx", Kind: "warning", SubscriptionID: sbxSub.ID})
	b.mail.Send(Email{Kind: "warning", SubscriptionID: b.seed.ID})

	// Payments are their owners', and so are the notifications.
	if p, _ := b.engine.Get("bcp_sbx"); p.Env != "sbx" || p.State != NotificationOutgoingPaymentProcessed {
		t.Fatalf("sbx's payout = %+v", p)
	}
	if in := b.engine.ListIn("sbx"); len(in) != 2 {
		t.Fatalf("sbx has %d payments, want its money in and its payout", len(in))
	}
	if got := len(b.engine.ListIn(tenant.Default)); got != 2 {
		t.Fatalf("the default has %d payments, want 2", got)
	}
	if _, err := b.engine.GetIn(tenant.Default, "bcp_sbx"); err == nil {
		t.Fatal("the default reads sbx's payout")
	}

	cleared := b.bank.ResetIn("sbx", true, nil)
	if cleared["payments"] != 2 || cleared["accounts"] != 1 || cleared["subscriptions"] != 1 || cleared["emails"] != 1 || cleared["pauses"] != 1 {
		t.Fatalf("sbx's all reset cleared %v; want its 2 payments, its creditor account, its subscription, its email and its pause", cleared)
	}
	if got := len(b.engine.ListIn(tenant.Default)); got != 2 {
		t.Fatalf("the default has %d payments after sbx's reset, want 2", got)
	}
	if got := balance(t, b.ledger, SGAAccountEUR); got != "90.00" {
		t.Fatalf("the default's safeguarding account after sbx's reset = %s, want 90.00", got)
	}
	if got := balance(t, b.ledger, sbxSGA); got != "0.00" {
		t.Fatalf("sbx's safeguarding account after its reset = %s, want 0.00, kept", got)
	}
	if _, err := b.ledger.Get(AccountIDIn("sbx", "merchant")); err == nil {
		t.Fatal("sbx's creditor account survived its all reset")
	}
	if _, err := b.ledger.Get(AccountIDFor("merchant")); err != nil {
		t.Fatal("sbx's reset dropped the default's creditor account")
	}
	if _, err := b.subs.Get(b.seed.ID); err != nil {
		t.Fatal("sbx's reset dropped the default's subscription")
	}
	if got := len(b.mail.AllIn(tenant.All)); got != 1 {
		t.Fatalf("%d emails after sbx's reset, want the default's one", got)
	}
	// The default's payments remain, so references keep counting.
	p, err := b.engine.CreditIncoming(sbxSGA, "EUR", "1.00", "after")
	if err != nil {
		t.Fatal(err)
	}
	if p.ReferenceNumber == "010F100000000001" || p.Env != "sbx" {
		t.Fatalf("money in after sbx's reset = %+v, want sbx's with the next reference", p)
	}
	b.engine.Wait()

	// Forgetting sbx takes its safeguarding account too.
	b.bank.Forget("sbx", nil)
	if _, err := b.ledger.Get(sbxSGA); err == nil {
		t.Fatal("sbx's safeguarding account survived Forget")
	}
	if got := len(b.engine.List()); got != 2 {
		t.Fatalf("%d payments after Forget(sbx), want the default's 2", got)
	}
}

func TestSubscriptionsAreScopedToTheirOwner(t *testing.T) {
	s := NewSubscriptionStore(nil)
	mk := func(env, endpoint string) *Subscription {
		t.Helper()
		sub, err := s.Create(CreateParams{Env: env, Endpoint: endpoint, EncryptionKey: "0123456789abcdef0123456789abcdef", Status: StatusActive})
		if err != nil {
			t.Fatalf("%s %s: %v", env, endpoint, err)
		}
		if _, err := s.AddEvent(sub.ID, "OutgoingPaymentProcessed", TargetCompany, nil); err != nil {
			t.Fatal(err)
		}
		return sub
	}
	def, sbx := mk("", "https://x.test/hook"), mk("sbx", "https://x.test/hook")
	if _, err := s.Create(CreateParams{Env: "sbx", Endpoint: "https://x.test/hook", EncryptionKey: "0123456789abcdef0123456789abcdef", Status: StatusActive}); err != ErrDuplicateEndpoint {
		t.Fatalf("sbx's second subscription to its own endpoint = %v, want ErrDuplicateEndpoint", err)
	}
	if got := s.RecipientsIn("sbx", "OutgoingPaymentProcessed"); len(got) != 1 || got[0].Subscription != sbx {
		t.Fatalf("sbx's recipients = %v", got)
	}
	if got := s.Recipients("OutgoingPaymentProcessed"); len(got) != 1 || got[0].Subscription != def {
		t.Fatalf("the default's recipients = %v", got)
	}
	if _, err := s.GetIn("sbx", def.ID); err != ErrSubscriptionNotFound {
		t.Fatalf("sbx reading the default's subscription = %v", err)
	}
	if _, err := s.GetEventIn("sbx", def.Events[0].ID); err != ErrSubscriptionNotFound {
		t.Fatalf("sbx reading the default's event = %v", err)
	}
	if subs, total := s.ListIn("sbx", 1, 50); total != 1 || subs[0] != sbx {
		t.Fatalf("sbx's list = %v (%d)", subs, total)
	}
	if _, total := s.ListIn(tenant.All, 1, 50); total != 2 {
		t.Fatalf("every list = %d, want 2", total)
	}
}
