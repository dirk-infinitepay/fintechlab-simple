package bankingcircle

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// within fails the test unless f returns inside d. The in-flight tests
// sleep for an hour: if a reset did not wake them, Wait would hang, and
// this turns the hang into a failure that says so.
func within(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s: work from before the reset is still running", what, d)
	}
}

// endRun is a reset of the run alone, as Bank.Reset does it.
func endRun(r *Run) {
	r.stop()
	r.gate.Lock()
	r.next()
	r.gate.Unlock()
}

func TestRunDropsWorkFromBeforeAReset(t *testing.T) {
	r := NewRun()
	started := r.Current()
	if !r.Do(started, func(context.Context) {}) {
		t.Fatal("Do refused work from the current run")
	}

	// An effect in flight holds the reset off, and sees its context
	// cancelled the moment the reset begins.
	inEffect, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go r.Do(started, func(ctx context.Context) {
		close(inEffect)
		<-ctx.Done()
		close(cancelled)
		<-release
	})
	<-inEffect
	resetDone := make(chan struct{})
	go func() { endRun(r); close(resetDone) }()
	<-cancelled
	select {
	case <-resetDone:
		t.Fatal("the reset finished while an effect was still running")
	case <-time.After(20 * time.Millisecond):
	}
	if r.Moved(started) {
		t.Fatal("the epoch moved while an effect from the old run was still running")
	}
	close(release)
	<-resetDone

	if !r.Moved(started) {
		t.Fatal("the reset did not move the epoch")
	}
	ran := false
	if r.Do(started, func(context.Context) { ran = true }) || ran {
		t.Fatal("Do ran work from before the reset")
	}
	if r.Sleep(started, time.Hour) {
		t.Fatal("Sleep says a run that has ended is still going")
	}
	if !r.Do(r.Current(), func(context.Context) {}) {
		t.Fatal("Do refused work from the new run")
	}

	// A sleeper is woken by the reset rather than sleeping it out.
	started = r.Current()
	woke := make(chan bool)
	go func() { woke <- r.Sleep(started, time.Hour) }()
	time.Sleep(5 * time.Millisecond)
	endRun(r)
	select {
	case going := <-woke:
		if going {
			t.Fatal("a sleeper woken by a reset was told its run is still going")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a reset did not wake a sleeper")
	}
}

// testBank is the package wired as cmd/bankingcircle wires it, without
// HTTP: every transition goes to every matching subscription through
// EnqueueFrom, and Send records instead of posting.
type testBank struct {
	ledger *Ledger
	engine *Engine
	disp   *Dispatcher
	subs   *SubscriptionStore
	mail   *MailBox
	rec    *recorder
	bank   *Bank
	seed   *Subscription

	mu          sync.Mutex
	transitions []Payment
}

func newTestBank(t *testing.T, delay time.Duration, cfg DeliveryConfig) *testBank {
	t.Helper()
	b := &testBank{ledger: NewLedger(), subs: NewSubscriptionStore(nil), mail: &MailBox{}, rec: &recorder{}}
	b.disp = NewDispatcher(cfg, b.subs, b.mail, nil)
	b.disp.Send = b.rec.send
	b.engine = NewEngine(b.ledger, delay, func(p *Payment) {
		b.mu.Lock()
		b.transitions = append(b.transitions, *p)
		b.mu.Unlock()
		for _, rec := range b.subs.Recipients(string(p.State), p.ToAccountID, p.FromAccountID) {
			b.disp.EnqueueFrom(p.Epoch(), rec.Subscription, Notification{EventID: p.ID, NotificationType: string(p.State)})
		}
	})
	b.seed = subscribeEverything(t, b.subs, "https://example.test/seed", 5)
	b.bank = NewBank(b.engine, b.disp)
	return b
}

func subscribeEverything(t *testing.T, store *SubscriptionStore, endpoint string, batch int) *Subscription {
	t.Helper()
	sub, err := store.Create(CreateParams{
		Endpoint:                   endpoint,
		EncryptionKey:              "0123456789abcdef0123456789abcdef",
		Status:                     StatusActive,
		Email:                      "alerts@example.com",
		MaxNotificationsPerMessage: batch,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, et := range []NotificationType{
		NotificationOutgoingPaymentBooked, NotificationOutgoingPaymentProcessed,
		NotificationOutgoingPaymentRejected, NotificationMissingFunding, NotificationReversed,
		NotificationIncomingPaymentBooked, NotificationIncomingPaymentProcessed,
	} {
		if _, err := store.AddEvent(sub.ID, string(et), TargetCompany, nil); err != nil {
			t.Fatal(err)
		}
	}
	return sub
}

func (b *testBank) states() []NotificationType {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]NotificationType, len(b.transitions))
	for i, p := range b.transitions {
		out[i] = p.State
	}
	return out
}

// quiet is a config under which nothing leaves on its own during a test:
// partial batches wait an hour, and a failed delivery waits an hour before
// its one retry.
func quiet() DeliveryConfig {
	cfg := fastConfig(1)
	cfg.Schedule[0].After = Duration(time.Hour)
	cfg.BatchFlushInterval = Duration(time.Hour)
	return cfg
}

func balance(t *testing.T, l *Ledger, id string) string {
	t.Helper()
	a, err := l.Get(id)
	if err != nil {
		t.Fatalf("account %s: %v", id, err)
	}
	return a.Balance
}

func TestResetFlowClearsWhatARunMadeAndKeepsTheSetup(t *testing.T) {
	b := newTestBank(t, time.Millisecond, quiet())
	creditor := AccountIDFor("merchant")
	if _, err := b.ledger.GetOrCreate(creditor, "EUR"); err != nil {
		t.Fatal(err)
	}
	// What a platform sets up once: its own subscription, after start-up.
	platform := subscribeEverything(t, b.subs, "https://example.test/platform", 5)
	platformVersion := platform.RowVersion
	// An operator's pause, so the platform's notifications park.
	b.disp.Pause(platform.ID)

	// A run: money in, a payout, and a forced outcome still waiting.
	if _, err := b.engine.CreditIncoming(SGAAccountEUR, "EUR", "100.00", "lump sum"); err != nil {
		t.Fatal(err)
	}
	b.engine.Create(&Payment{ID: "bcp_out", FromAccountID: SGAAccountEUR, ToAccountID: creditor, Amount: "10.00", Currency: "EUR"})
	b.engine.Wait()
	if got := balance(t, b.ledger, creditor); got != "10.00" {
		t.Fatalf("creditor balance before the reset = %s, want 10.00", got)
	}
	lastRef := mustGet(t, b.engine, "bcp_out").ReferenceNumber
	if _, err := b.engine.ForceNext([]Outcome{OutcomeRejected, OutcomePending}); err != nil {
		t.Fatal(err)
	}
	// Notifications in every place one can wait: batching (four
	// transitions per subscription, under a batch of five, with an hour's
	// flush interval), parked behind the pause, and retained after a
	// deactivation. And an email.
	b.disp.retain(b.seed.ID, []Notification{{EventID: "held1"}, {EventID: "held2"}})
	b.mail.Send(Email{To: "alerts@example.com", Kind: "warning", SubscriptionID: b.seed.ID})
	if got := b.disp.Queued(platform.ID); got != 4 {
		t.Fatalf("%d parked behind the pause, want the platform's 4 transitions", got)
	}

	cleared := b.bank.Reset(false, nil)

	want := map[string]int{
		"payments":      2,
		"outcomes":      2,
		"balances":      2,         // the safeguarding account and the creditor
		"notifications": 4 + 4 + 2, // batching for the seed, parked for the platform, retained
		"pauses":        1,
		"emails":        1,
	}
	for k, v := range want {
		if cleared[k] != v {
			t.Errorf("cleared[%q] = %d, want %d (all: %v)", k, cleared[k], v, cleared)
		}
	}
	for _, k := range []string{"subscriptions", "accounts", "tokens"} {
		if _, ok := cleared[k]; ok {
			t.Errorf("flow reports %q cleared: %v", k, cleared)
		}
	}

	if got := b.engine.List(); len(got) != 0 {
		t.Fatalf("%d payments after the reset, want none", len(got))
	}
	if got := b.engine.ForcedOutcomes(); len(got) != 0 {
		t.Fatalf("forced outcomes after the reset = %v, want none", got)
	}
	for _, id := range []string{SGAAccountEUR, SGAAccountGBP, creditor} {
		if got := balance(t, b.ledger, id); got != "0.00" {
			t.Errorf("%s balance after the reset = %s, want 0.00", id, got)
		}
	}
	if a, _ := b.ledger.Get(creditor); a != nil {
		if open, intraday := a.DayAmounts(FinancialDay(time.Now())); open != 0 || intraday != 0 {
			t.Errorf("creditor day amounts after the reset = %d, %d; want 0, 0", open, intraday)
		}
	}
	if b.disp.Paused(platform.ID) || b.disp.Queued(platform.ID) != 0 || b.disp.Pending(b.seed.ID) != 0 {
		t.Fatalf("after the reset: paused=%v queued=%d pending=%d, want nothing held",
			b.disp.Paused(platform.ID), b.disp.Queued(platform.ID), b.disp.Pending(b.seed.ID))
	}
	if got := len(b.mail.All()); got != 0 {
		t.Fatalf("%d emails after the reset, want none", got)
	}
	// The batch that was waiting on its timer is gone: flushing it sends
	// nothing, and resuming the platform releases nothing.
	b.disp.Flush(b.seed.ID)
	if n := b.disp.Resume(platform); n != 0 {
		t.Fatalf("resume released %d after the reset, want 0", n)
	}
	b.disp.Wait()
	if got := len(b.rec.envelopes()); got != 0 {
		t.Fatalf("%d batches sent after the reset, want none", got)
	}

	// Kept: both subscriptions, untouched, and the account the payout opened.
	for _, sub := range []*Subscription{b.seed, platform} {
		got, err := b.subs.Get(sub.ID)
		if err != nil {
			t.Fatalf("subscription %s gone after a flow reset: %v", sub.ID, err)
		}
		if !got.IsActive || len(got.Events) != 7 {
			t.Fatalf("subscription %s after the reset: active=%v events=%d", sub.ID, got.IsActive, len(got.Events))
		}
	}
	if got, _ := b.subs.Get(platform.ID); got.RowVersion != platformVersion {
		t.Fatalf("a flow reset wrote to the platform's subscription (rowVersion %s → %s)", platformVersion, got.RowVersion)
	}

	// Reference numbers keep counting, so a platform that kept its books
	// never sees one handed out twice.
	if _, err := b.engine.CreditIncoming(SGAAccountEUR, "EUR", "1.00", "after"); err != nil {
		t.Fatal(err)
	}
	b.engine.Wait()
	for _, p := range b.engine.List() {
		if p.ReferenceNumber <= lastRef {
			t.Fatalf("reference %s after the reset, want one after %s", p.ReferenceNumber, lastRef)
		}
	}
}

func mustGet(t *testing.T, e *Engine, id string) *Payment {
	t.Helper()
	p, err := e.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResetAllPutsBackTheSeed(t *testing.T) {
	b := newTestBank(t, time.Millisecond, quiet())
	seedVersion := b.seed.RowVersion
	seedEvent := b.seed.Events[0].ID

	// The run, and the setup a flow reset would keep.
	creditor := AccountIDFor("merchant")
	if _, err := b.ledger.GetOrCreate(creditor, "EUR"); err != nil {
		t.Fatal(err)
	}
	platform := subscribeEverything(t, b.subs, "https://example.test/platform", 5)
	if _, err := b.subs.Deactivate(b.seed.ID, "Subscription deactivated after the final unsuccessful delivery retry"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.engine.CreditIncoming(SGAAccountEUR, "EUR", "5.00", "lump sum"); err != nil {
		t.Fatal(err)
	}
	b.engine.Wait()

	for round := 1; round <= 2; round++ {
		cleared := b.bank.Reset(true, nil)
		if round == 1 {
			if cleared["subscriptions"] != 2 || cleared["accounts"] != 1 || cleared["payments"] != 1 {
				t.Fatalf("cleared = %v, want subscriptions 2 (the platform's, and the seed put back), accounts 1, payments 1", cleared)
			}
		}

		subs, total := b.subs.List(1, 50)
		if total != 1 || subs[0].ID != b.seed.ID {
			t.Fatalf("round %d: %d subscriptions after an all reset, want only the seed %s", round, total, b.seed.ID)
		}
		got := subs[0]
		if !got.IsActive || got.Status != StatusActive || got.RowVersion != seedVersion || len(got.Events) != 7 {
			t.Fatalf("round %d: seed after the reset: active=%v status=%d rowVersion=%s events=%d; want it as seeded (rowVersion %s)",
				round, got.IsActive, got.Status, got.RowVersion, len(got.Events), seedVersion)
		}
		if ev, err := b.subs.GetEvent(seedEvent); err != nil || ev != got.Events[0] {
			t.Fatalf("round %d: the seed's event is not indexed as the subscription's own: %v", round, err)
		}
		if _, err := b.subs.Get(platform.ID); err == nil {
			t.Fatalf("round %d: the platform's subscription survived an all reset", round)
		}
		if _, err := b.ledger.Get(creditor); err == nil {
			t.Fatalf("round %d: the account a payout opened survived an all reset", round)
		}
		for _, id := range []string{SGAAccountEUR, SGAAccountGBP} {
			if got := balance(t, b.ledger, id); got != "0.00" {
				t.Fatalf("round %d: %s = %s after an all reset, want 0.00", round, id, got)
			}
		}

		// As on first start: the first payment is reference 1 again.
		p, err := b.engine.CreditIncoming(SGAAccountEUR, "EUR", "1.00", "first")
		if err != nil {
			t.Fatal(err)
		}
		if p.ReferenceNumber != "010F100000000001" {
			t.Fatalf("round %d: first reference after an all reset = %s, want 010F100000000001", round, p.ReferenceNumber)
		}
		b.engine.Wait()

		// Write to the restored seed, so round 2 proves the snapshot was
		// copied, not handed over.
		if _, err := b.subs.AddEvent(b.seed.ID, string(NotificationPaymentStatus), TargetCompany, nil); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAPaymentInFlightWhenTheResetLandsIsDropped is the rule that matters
// most: a payout still sleeping out PROCESSING_DELAY when the reset lands
// never moves money, never changes state and is never notified, and the
// money that landed for it is gone.
func TestAPaymentInFlightWhenTheResetLandsIsDropped(t *testing.T) {
	// An hour's delay: without the reset waking them, Wait would hang.
	b := newTestBank(t, time.Hour, quiet())
	creditor := AccountIDFor("merchant")
	if _, err := b.ledger.GetOrCreate(creditor, "EUR"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.engine.CreditIncoming(SGAAccountEUR, "EUR", "100.00", "lump sum"); err != nil {
		t.Fatal(err)
	}
	b.engine.Create(&Payment{ID: "bcp_inflight", FromAccountID: SGAAccountEUR, ToAccountID: creditor, Amount: "10.00", Currency: "EUR"})
	b.engine.Create(&Payment{ID: "bcp_inflight2", FromAccountID: SGAAccountEUR, ToAccountID: creditor, Amount: "10.00", Currency: "EUR"})
	before := b.states()

	cleared := b.bank.Reset(false, nil)
	if cleared["payments"] != 3 || cleared["notifications"] != 3 {
		t.Fatalf("cleared = %v, want the 3 payments and their 3 booked notifications", cleared)
	}
	within(t, 2*time.Second, "the engine's in-flight payments", b.engine.Wait)
	within(t, 2*time.Second, "the dispatcher", b.disp.Wait)

	if got := b.states(); len(got) != len(before) {
		t.Fatalf("transitions after the reset: %v, want none beyond %v", got[len(before):], before)
	}
	if got := b.engine.List(); len(got) != 0 {
		t.Fatalf("%d payments after the reset, want none", len(got))
	}
	for _, id := range []string{SGAAccountEUR, creditor} {
		if got := balance(t, b.ledger, id); got != "0.00" {
			t.Fatalf("%s = %s after the reset, want 0.00: a dropped payout moved money", id, got)
		}
	}
	if got := b.rec.attemptCount(); got != 0 {
		t.Fatalf("%d deliveries after the reset, want none", got)
	}
}

// TestADeliveryMidRetryWhenTheResetLandsIsDropped: a batch waiting out the
// retry schedule when the reset lands is never sent again, mails nothing
// and deactivates nothing.
func TestADeliveryMidRetryWhenTheResetLandsIsDropped(t *testing.T) {
	cfg := fastConfig(1)
	cfg.Schedule[0].After = Duration(time.Hour) // the one retry, with its deactivation email, an hour out
	b := newTestBank(t, time.Millisecond, cfg)
	b.rec.failFor = 1 << 30 // the subscriber never answers 2xx

	for i := range 5 {
		b.disp.Enqueue(b.seed, Notification{EventID: string(rune('a' + i))})
	}
	// The full batch went out straight away and failed; it is now sleeping
	// out its hour.
	for b.rec.attemptCount() == 0 {
		time.Sleep(time.Millisecond)
	}

	cleared := b.bank.Reset(false, nil)
	if cleared["notifications"] != 5 {
		t.Fatalf("cleared = %v, want the 5 notifications mid-retry", cleared)
	}
	within(t, 2*time.Second, "the delivery mid-retry", b.disp.Wait)

	if got := b.rec.attemptCount(); got != 1 {
		t.Fatalf("%d delivery attempts, want only the 1 before the reset", got)
	}
	if got := len(b.mail.All()); got != 0 {
		t.Fatalf("%d emails after the reset, want none", got)
	}
	sub, _ := b.subs.Get(b.seed.ID)
	if !sub.IsActive || b.disp.Pending(sub.ID) != 0 {
		t.Fatalf("after the reset: active=%v pending=%d; a dropped delivery must not deactivate or retain", sub.IsActive, b.disp.Pending(sub.ID))
	}
}

// TestANotificationFromBeforeAResetIsNotQueuedAfterIt: the gap between the
// engine's check and the dispatcher's queue, closed. A notification about a
// payment from before the reset, arriving after it, is dropped.
func TestANotificationFromBeforeAResetIsNotQueuedAfterIt(t *testing.T) {
	b := newTestBank(t, time.Millisecond, quiet())
	old := b.engine.Run().Current()
	b.bank.Reset(false, nil)

	b.disp.EnqueueFrom(old, b.seed, Notification{EventID: "stray", NotificationType: string(NotificationOutgoingPaymentProcessed)})
	b.disp.Pause(b.seed.ID)
	b.disp.EnqueueFrom(old, b.seed, Notification{EventID: "stray2"})
	if got := b.disp.Queued(b.seed.ID); got != 0 {
		t.Fatalf("%d parked after the reset, want the stray dropped", got)
	}
	b.disp.Resume(b.seed)
	b.disp.Flush(b.seed.ID)
	b.disp.Wait()
	if got := b.rec.attemptCount(); got != 0 {
		t.Fatalf("%d deliveries, want none: a notification about a cleared payment went out", got)
	}

	// The new run's own notifications still flow.
	b.disp.Enqueue(b.seed, Notification{EventID: "fresh"})
	b.disp.Flush(b.seed.ID)
	b.disp.Wait()
	if got := b.rec.attemptCount(); got != 1 {
		t.Fatalf("%d deliveries in the new run, want 1", got)
	}
}

// TestResetRacingPaymentsLeavesNothingBehind lands resets at arbitrary
// points in payments' lives -- before the delay is up, mid-move, between a
// transition and its notification, with batches on their flush timers --
// and holds every interleaving to the same rule: once the reset is done,
// nothing from before it is sent, moved or recorded.
func TestResetRacingPaymentsLeavesNothingBehind(t *testing.T) {
	cfg := fastConfig(1)
	cfg.BatchFlushInterval = Duration(time.Millisecond)
	for round := range 25 {
		b := newTestBank(t, time.Duration(round%4)*time.Millisecond, cfg)
		var after atomic.Bool
		var late atomic.Int64
		b.disp.Send = func(ctx context.Context, sub *Subscription, env Envelope) error {
			if after.Load() {
				late.Add(1)
			}
			return b.rec.send(ctx, sub, env)
		}
		creditor := AccountIDFor("merchant")
		if _, err := b.ledger.GetOrCreate(creditor, "EUR"); err != nil {
			t.Fatal(err)
		}
		if _, err := b.engine.CreditIncoming(SGAAccountEUR, "EUR", "1000.00", "lump sum"); err != nil {
			t.Fatal(err)
		}
		for i := range 12 {
			b.engine.Create(&Payment{ID: "bcp_" + string(rune('a'+i)), FromAccountID: SGAAccountEUR, ToAccountID: creditor, Amount: "1.00", Currency: "EUR"})
		}
		time.Sleep(time.Duration(round%5) * 700 * time.Microsecond)

		b.bank.Reset(false, func(map[string]int) { after.Store(true) })
		movedAt := len(b.states())
		within(t, 2*time.Second, "the engine", b.engine.Wait)
		within(t, 2*time.Second, "the dispatcher", b.disp.Wait)

		if n := late.Load(); n != 0 {
			t.Fatalf("round %d: %d batches sent after the reset", round, n)
		}
		if got := b.states(); len(got) != movedAt {
			t.Fatalf("round %d: transitions after the reset: %v", round, got[movedAt:])
		}
		if got := b.engine.List(); len(got) != 0 {
			t.Fatalf("round %d: %d payments after the reset", round, len(got))
		}
		for _, id := range []string{SGAAccountEUR, creditor} {
			if got := balance(t, b.ledger, id); got != "0.00" {
				t.Fatalf("round %d: %s = %s after the reset", round, id, got)
			}
		}
	}
}
