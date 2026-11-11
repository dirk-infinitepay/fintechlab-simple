package bankingcircle

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/money"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// throughFile is s written to a file and read back, as a restart reads it.
func throughFile(t *testing.T, s State) State {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, s); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := LoadState(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return out
}

func TestAMissingOrEmptyStateFileIsAFirstStart(t *testing.T) {
	dir := t.TempDir()
	for name, path := range map[string]string{"no path": "", "no file": filepath.Join(dir, "missing.json")} {
		s, err := LoadState(path)
		if err != nil || !s.FirstStart() {
			t.Fatalf("%s: %+v, %v; want a first start", name, s, err)
		}
	}
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s, err := LoadState(empty); err != nil || !s.FirstStart() {
		t.Fatalf("empty file: %+v, %v; want a first start", s, err)
	}
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(broken); err == nil {
		t.Fatal("a file that does not parse loaded without an error")
	}
	if err := SaveState("", State{}); err != nil {
		t.Fatalf("an empty path is in memory only: %v", err)
	}
	// A bank's file always has a seed, so it is never taken for a first
	// start, even with no subscriptions in it.
	b := newTestBank(t, time.Millisecond, fastConfig(1))
	var s State
	b.bank.Snapshot(&s)
	if got := throughFile(t, s); got.FirstStart() {
		t.Fatal("a file the bank wrote reads as a first start")
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json.tmp")); !os.IsNotExist(err) {
		t.Fatalf("the temporary file was left behind: %v", err)
	}
}

func TestTheLedgerComesBackWithItsBalancesDaysAndSeeds(t *testing.T) {
	l := NewLedger()
	if _, err := l.Credit(SGAAccountEUR, 25000); err != nil {
		t.Fatal(err)
	}
	sbx, err := l.OpenSafeguarding("sbx", "EUR", "Sandbox Safeguarding Account EUR")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Credit(sbx.ID, 10000); err != nil {
		t.Fatal(err)
	}
	creditor, err := l.GetOrCreateIn("sbx", AccountIDIn("sbx", AccountIDFor("merchant")), "EUR")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Move(sbx.ID, creditor.ID, 1250); err != nil {
		t.Fatal(err)
	}
	// An account that opened the day with money already on it.
	l.mu.Lock()
	l.accts[SGAAccountGBP].cents, l.accts[SGAAccountGBP].Balance = 700, "7.00"
	l.accts[SGAAccountGBP].openDay, l.accts[SGAAccountGBP].openCents = "2026-11-10", 500
	l.mu.Unlock()

	var s State
	l.Snapshot(&s)
	s = throughFile(t, s)
	l2 := NewLedger()
	if err := l2.Restore(s); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(accountsByID(l.ListIn(tenant.All)), accountsByID(l2.ListIn(tenant.All))) {
		t.Fatalf("accounts after restart:\n%+v\nwant\n%+v", accountsByID(l2.ListIn(tenant.All)), accountsByID(l.ListIn(tenant.All)))
	}
	today := FinancialDay(labclock.Now())
	for _, day := range []string{today, "2026-11-10"} {
		for _, id := range []string{SGAAccountEUR, SGAAccountGBP, sbx.ID, creditor.ID} {
			was, _ := l.Get(id)
			is, err := l2.Get(id)
			if err != nil {
				t.Fatalf("%s after restart: %v", id, err)
			}
			wo, wi := was.DayAmounts(day)
			io, ii := is.DayAmounts(day)
			if wo != io || wi != ii {
				t.Fatalf("%s on %s: beginOfDay/intraDay %d/%d after restart, want %d/%d", id, day, io, ii, wo, wi)
			}
		}
	}
	if got, err := l2.GetIn("sbx", sbx.VIBAN); err != nil || got.ID != sbx.ID {
		t.Fatalf("by vIBAN after restart = %+v, %v", got, err)
	}
	if _, err := l2.GetIn(tenant.Default, sbx.ID); err == nil {
		t.Fatal("sbx's account came back as the default's")
	}

	// What a reset puts back came back too: sbx's safeguarding account is
	// seeded (kept, at zero), the account its payout opened is not.
	l2.mu.Lock()
	balances, dropped := l2.resetLocked("sbx", true, false)
	l2.mu.Unlock()
	if balances != 2 || dropped != 1 {
		t.Fatalf("reset all of sbx after restart: %d balance(s), %d account(s) dropped; want 2, 1", balances, dropped)
	}
	if got, err := l2.Get(sbx.ID); err != nil || got.Balance != "0.00" {
		t.Fatalf("sbx's safeguarding account after the reset = %+v, %v", got, err)
	}
}

func accountsByID(list []*Account) map[string]Account {
	out := map[string]Account{}
	for _, a := range list {
		out[a.ID] = *a
	}
	return out
}

func TestTheSubscriptionsComeBackWithTheirIDsKeysAndRowVersions(t *testing.T) {
	s := NewSubscriptionStore(nil)
	sub, err := s.Create(CreateParams{Env: "sbx", Endpoint: "https://platform.sbx.test/hooks",
		EncryptionKey: "0123456789abcdef0123456789abcdef", Status: StatusActive, Email: "ops@sbx.test", MaxNotificationsPerMessage: 7})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := s.AddEvent(sub.ID, string(NotificationOutgoingPaymentProcessed), TargetAccount, []string{AccountIDFor("a"), AccountIDFor("b")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Deactivate(sub.ID, "Subscription deactivated after the final unsuccessful delivery retry"); err != nil {
		t.Fatal(err)
	}
	other := testSubscription(t, s, "https://example.test/default", 5)

	var st State
	s.Snapshot(&st)
	st = throughFile(t, st)
	s2 := NewSubscriptionStore(nil)
	s2.Restore(st)

	for _, id := range []string{sub.ID, other.ID} {
		was, _ := s.Get(id)
		is, err := s2.Get(id)
		if err != nil {
			t.Fatalf("%s after restart: %v", id, err)
		}
		if !reflect.DeepEqual(was, is) {
			t.Fatalf("%s after restart:\n%+v\nwant\n%+v", id, is, was)
		}
	}
	if got, err := s2.GetEventIn("sbx", ev.ID); err != nil || len(got.Targets) != 2 {
		t.Fatalf("the event by its id after restart = %+v, %v", got, err)
	}
	if _, err := s2.GetIn(tenant.Default, sub.ID); err == nil {
		t.Fatal("sbx's subscription came back as the default's")
	}
	// The counters go on from where they were: no rowVersion or id is
	// handed out twice.
	was, _ := s.Get(sub.ID)
	is, err := s2.SetActive(sub.ID, was.RowVersion, true)
	if err != nil {
		t.Fatalf("activate with the rowVersion from before the restart: %v", err)
	}
	if is.RowVersion <= was.RowVersion {
		t.Fatalf("rowVersion after restart %q, not past %q", is.RowVersion, was.RowVersion)
	}
	if fresh := testSubscription(t, s2, "https://example.test/new", 5); fresh.ID == sub.ID || fresh.ID == other.ID {
		t.Fatalf("a new subscription after restart reused id %s", fresh.ID)
	}
}

// A payout still booked when the bank stopped is processed when it comes
// back, and so is money in; a payout a trap held stays where the trap had
// it, and a finished one stays finished.
func TestThePaymentsComeBackAndWalkOnFromWhereTheyStopped(t *testing.T) {
	a := newTestBank(t, time.Hour, fastConfig(1))
	if _, err := a.engine.CreditIncoming(SGAAccountEUR, "EUR", "100.00", "lump sum"); err != nil {
		t.Fatal(err)
	}
	creditor := AccountIDFor("merchant")
	if _, err := a.ledger.GetOrCreate(creditor, "EUR"); err != nil {
		t.Fatal(err)
	}
	payout := func(id string) *Booking {
		return a.engine.Book(&Payment{ID: id, FromAccountID: SGAAccountEUR, ToAccountID: creditor, Amount: "10.00", Currency: "EUR"})
	}
	payout("walking").Process()
	payout("held").Caught("trp_pending")
	done := payout("done")
	done.Caught("trp_processed")
	if err := done.Step(Step{To: string(NotificationOutgoingPaymentProcessed), Trap: "trp_processed"}, nil); err != nil {
		t.Fatal(err)
	}
	last := lastReference(a.engine)

	// The process stops: everything sleeping wakes and drops what it was
	// about to do, and nothing moves while the snapshot is taken.
	within(t, 2*time.Second, "Stop", a.bank.Stop)
	within(t, 2*time.Second, "the engine's walkers", a.engine.Wait)
	var s State
	a.bank.Snapshot(&s)
	s = throughFile(t, s)
	if got := stateOf(s, "walking"); got != NotificationOutgoingPaymentBooked {
		t.Fatalf("the walking payout was written %s, want still booked", got)
	}

	b := restoredBank(t, s, 5*time.Millisecond)
	within(t, 2*time.Second, "the resumed walkers", b.engine.Wait)
	for id, want := range map[string]NotificationType{
		"walking": NotificationOutgoingPaymentProcessed,
		"held":    NotificationOutgoingPaymentBooked,
		"done":    NotificationOutgoingPaymentProcessed,
	} {
		if p, err := b.engine.Get(id); err != nil || p.State != want {
			t.Fatalf("%s after restart = %+v, %v; want %s", id, p, err, want)
		}
	}
	var incoming int
	for _, p := range b.engine.List() {
		if p.FromAccountID == "external_worldline" {
			incoming++
			if p.State != NotificationIncomingPaymentProcessed {
				t.Fatalf("money in after restart is %s, want processed", p.State)
			}
		}
	}
	if incoming != 1 {
		t.Fatalf("%d incoming payment(s) after restart, want 1", incoming)
	}
	// Only what had not happened yet fired: walking's processing and the
	// money in's, nothing for the held or the finished payout.
	if got := strings.Join(fired(b), ","); got != "IncomingPaymentProcessed,OutgoingPaymentProcessed" {
		t.Fatalf("transitions after restart = %s", got)
	}
	if acc, _ := b.ledger.Get(SGAAccountEUR); acc.Balance != "80.00" {
		t.Fatalf("safeguarding account after restart %s, want 80.00 (100 in, done's 10 and walking's 10 out)", acc.Balance)
	}
	if p, _ := b.engine.Get("done"); p.Trap() != "" {
		t.Fatalf("a payout's trap mark leaked into the stored payment: %q", p.Trap())
	}
	b.engine.Book(&Payment{ID: "next", FromAccountID: SGAAccountEUR, ToAccountID: creditor, Amount: "1.00", Currency: "EUR"})
	if p, _ := b.engine.Get("next"); p.ReferenceNumber <= last {
		t.Fatalf("reference %s after restart, not past %s", p.ReferenceNumber, last)
	}
}

func lastReference(e *Engine) string {
	last := ""
	for _, p := range e.List() {
		if p.ReferenceNumber > last {
			last = p.ReferenceNumber
		}
	}
	return last
}

func stateOf(s State, id string) NotificationType {
	for _, p := range s.Payments {
		if p.ID == id {
			return p.State
		}
	}
	return ""
}

func fired(b *testBank) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, p := range b.transitions {
		out = append(out, string(p.State))
	}
	sort.Strings(out)
	return out
}

// restoredBank is newTestBank's wiring with no seed of its own, restored
// from s as a restart restores it.
func restoredBank(t *testing.T, s State, delay time.Duration) *testBank {
	t.Helper()
	b := &testBank{ledger: NewLedger(), subs: NewSubscriptionStore(nil), mail: &MailBox{}, rec: &recorder{}}
	b.disp = NewDispatcher(fastConfig(1), b.subs, b.mail, nil)
	b.disp.Send = b.rec.send
	b.engine = NewEngine(b.ledger, delay, func(p *Payment) {
		b.mu.Lock()
		b.transitions = append(b.transitions, *p)
		b.mu.Unlock()
	})
	b.bank = NewBank(b.engine, b.disp)
	if err := b.bank.Restore(s); err != nil {
		t.Fatal(err)
	}
	return b
}

// Every notification the subscriber had not had comes back: the batch
// mid-retry is sent again, the queue is flushed, the retained ones wait for
// the reactivation and the parked ones for the resume. The payment detail
// goes out as the bytes it would have gone out as.
func TestTheNotificationsNotYetDeliveredComeBack(t *testing.T) {
	cfg := fastConfig(1)
	cfg.Schedule[0].After = Duration(time.Hour) // a failed send waits an hour to retry
	cfg.BatchFlushInterval = Duration(time.Hour)
	subs := NewSubscriptionStore(nil)
	batching := testSubscription(t, subs, "https://example.test/batching", 5)
	sending := testSubscription(t, subs, "https://example.test/sending", 5)
	retained := testSubscription(t, subs, "https://example.test/retained", 5)
	parked := testSubscription(t, subs, "https://example.test/parked", 5)
	gone := testSubscription(t, subs, "https://example.test/gone", 5)
	rec := &recorder{failFor: 1}
	d := NewDispatcher(cfg, subs, &MailBox{}, nil)
	d.Send = rec.send
	bank := NewBank(NewEngine(NewLedger(), time.Hour, nil), d)

	note := func(sub *Subscription, i int) Notification {
		return Notification{EventID: sub.ID + "-" + string(rune('a'+i)), SubscriptionID: sub.ID, NotificationType: "OutgoingPaymentProcessed",
			Timestamp: "2026-11-11T10:00:0" + string(rune('0'+i)) + "Z", Trap: "trp_x",
			Payment: map[string]any{"paymentId": "p" + string(rune('0'+i)), "amount": json.Number("12.50")}}
	}
	d.Enqueue(batching, note(batching, 0))
	d.Enqueue(batching, note(batching, 1))
	for i := range 5 {
		d.Enqueue(sending, note(sending, i)) // a full batch: out at once, refused, waiting to retry
	}
	d.retain(retained, []Notification{note(retained, 0)})
	d.Pause(parked.ID)
	d.Enqueue(parked, note(parked, 0))
	d.Enqueue(gone, note(gone, 0))
	waitFor(t, "the first attempt", func() bool { return rec.attemptCount() == 1 })

	within(t, 2*time.Second, "Stop", bank.Stop)
	var s State
	bank.Snapshot(&s)
	if err := subs.Delete(gone.ID, gone.RowVersion); err != nil {
		t.Fatal(err)
	}
	subs.Snapshot(&s) // gone is not in the store the restart reads
	s = throughFile(t, s)
	held := map[string]string{}
	for _, hb := range s.Notifications {
		held[hb.SubscriptionID] = hb.Held
	}
	want := map[string]string{batching.ID: HeldBatching, sending.ID: HeldSending, retained.ID: HeldRetained, parked.ID: HeldPaused, gone.ID: HeldBatching}
	if !reflect.DeepEqual(held, want) {
		t.Fatalf("held = %v, want %v", held, want)
	}

	subs2 := NewSubscriptionStore(nil)
	subs2.Restore(s)
	rec2 := &recorder{}
	cfg2 := fastConfig(1)
	d2 := NewDispatcher(cfg2, subs2, &MailBox{}, nil)
	d2.Send = rec2.send
	if err := NewBank(NewEngine(NewLedger(), time.Millisecond, nil), d2).Restore(s); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the batch mid-retry and the queue's flush", func() bool { return len(rec2.envelopes()) == 2 })

	got := map[string]int{}
	for _, env := range rec2.envelopes() {
		for _, n := range env.Notifications {
			got[n.SubscriptionID]++
			if n.Trap != "trp_x" {
				t.Fatalf("the trap mark did not come back: %+v", n)
			}
		}
		body, _ := json.Marshal(env)
		if !strings.Contains(string(body), `"amount":12.50`) {
			t.Fatalf("the payment detail changed on the way through the file: %s", body)
		}
	}
	if !reflect.DeepEqual(got, map[string]int{sending.ID: 5, batching.ID: 2}) {
		t.Fatalf("delivered after restart = %v, want the batch mid-retry and the queue", got)
	}
	if !d2.Paused(parked.ID) || d2.Queued(parked.ID) != 1 || d2.Pending(retained.ID) != 1 {
		t.Fatalf("paused %v, queued %d, retained %d; want the pause and both held", d2.Paused(parked.ID), d2.Queued(parked.ID), d2.Pending(retained.ID))
	}
	p2, _ := subs2.Get(parked.ID)
	r2, _ := subs2.Get(retained.ID)
	if d2.Resume(p2) != 1 || d2.Redeliver(r2) != 1 {
		t.Fatal("resume and reactivation released nothing")
	}
	waitFor(t, "the released ones", func() bool { return len(rec2.envelopes()) == 4 })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestTheEmailsComeBack(t *testing.T) {
	m := &MailBox{}
	m.Send(Email{Env: "sbx", To: "ops@sbx.test", Kind: "deactivation", SubscriptionID: "sub_1", Attempt: 11})
	m.Send(Email{To: "alerts@example.test", Kind: "warning", SubscriptionID: "sub_2", Attempt: 7})
	var s State
	m.Snapshot(&s)
	m2 := &MailBox{}
	m2.Restore(throughFile(t, s))
	was, is := m.All(), m2.All()
	for i := range was {
		was[i].At, is[i].At = was[i].At.UTC(), is[i].At.UTC()
	}
	if !reflect.DeepEqual(was, is) {
		t.Fatalf("emails after restart = %+v, want %+v", is, was)
	}
	if got := m2.AllIn("sbx"); len(got) != 1 || got[0].Kind != "deactivation" {
		t.Fatalf("sbx's emails after restart = %+v", got)
	}
}

// The seed a reset of scope all puts back is the first start's, not
// whatever a restart would seed again: the same subscription, the same id.
func TestARestartKeepsWhatAResetPutsBack(t *testing.T) {
	a := newTestBank(t, time.Millisecond, fastConfig(1))
	added := testSubscription(t, a.subs, "https://example.test/added", 5)
	var s State
	a.bank.Snapshot(&s)

	b := restoredBank(t, throughFile(t, s), time.Millisecond)
	if _, err := b.subs.Get(added.ID); err != nil {
		t.Fatalf("the platform's own subscription after restart: %v", err)
	}
	b.bank.Reset(true, nil)
	list, total := b.subs.List(1, 10)
	if total != 1 || list[0].ID != a.seed.ID || list[0].RowVersion != a.seed.RowVersion {
		t.Fatalf("after a reset of scope all the store is %+v; want the first start's seed %s", list, a.seed.ID)
	}
}

// A snapshot is one instant: money in and the payment that put it there,
// a payout's state and its ledger move, are written together or not at
// all. All the money on the books came in as money in, so every snapshot's
// balances add up to its incoming payments, however busy the bank is.
func TestASnapshotIsOneInstantWhileTheBankWorks(t *testing.T) {
	b := newTestBank(t, time.Millisecond, fastConfig(1))
	creditor := AccountIDFor("merchant")
	if _, err := b.ledger.GetOrCreate(creditor, "EUR"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ctx.Err() == nil; i++ {
			if _, err := b.engine.CreditIncoming(SGAAccountEUR, "EUR", "5.00", "lump"); err != nil {
				t.Error(err)
				return
			}
			b.engine.Create(&Payment{ID: "p" + strconv.Itoa(i), FromAccountID: SGAAccountEUR, ToAccountID: creditor, Amount: "3.00", Currency: "EUR"})
			time.Sleep(100 * time.Microsecond)
		}
	}()
	for range 50 {
		var s State
		b.bank.Snapshot(&s)
		var on, in int64
		for _, a := range s.Accounts {
			c, _ := money.Parse(a.Balance)
			on += c
		}
		for _, p := range s.Payments {
			if p.FromAccountID == "external_worldline" {
				c, _ := money.Parse(p.Amount)
				in += c
			}
		}
		if on != in {
			cancel()
			t.Fatalf("a snapshot holds %s on the books and %s of money in", money.Format(on), money.Format(in))
		}
		if _, err := json.Marshal(s); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	wg.Wait()
	b.engine.Wait()
	within(t, 2*time.Second, "Stop", b.bank.Stop)
}
