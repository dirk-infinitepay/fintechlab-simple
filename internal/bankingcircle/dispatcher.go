package bankingcircle

import (
	"context"
	"sync"
	"time"
)

// Dispatcher batches notifications per subscription and delivers them on
// the configured retry schedule, deactivating a subscription that never
// acknowledges.
//
// One queue per subscription, not one global queue: batch size is a
// per-subscription setting, and a slow subscriber must not hold up a fast
// one. Delivery itself is injected (Send) so the whole thing is testable
// without HTTP, encryption or a network.
type Dispatcher struct {
	cfg  DeliveryConfig
	subs *SubscriptionStore
	mail *MailBox
	logf func(format string, args ...any)
	// run is the epoch a reset moves. Its own until NewBank shares the
	// engine's, so a reset ends both at the same instant.
	run *Run

	// Send delivers one encrypted batch to a subscription and reports
	// whether it was acknowledged with a 2xx. Anything else -- a 500, a
	// refused connection, a timeout -- is a failure that starts the retry
	// schedule. ctx is cancelled when a reset begins; a send still on the
	// wire then is abandoned with the run it belonged to.
	Send func(ctx context.Context, sub *Subscription, env Envelope) error

	// OnQueued reports notifications parked behind a pause. Parking
	// happens inside Enqueue and inside delivery, neither of which the
	// caller can see, and a notification that silently stops existing
	// until someone presses a button is the one thing a pause must never
	// look like. May be nil.
	OnQueued func(sub *Subscription, env Envelope)

	mu     sync.Mutex
	queues map[string]*queue
	// pending holds notifications for subscriptions that were deactivated
	// before they could be delivered. They are retained and redelivered on
	// reactivation, which is what the API documents; events that occur
	// while deactivated are not queued here at all.
	pending map[string][]Notification
	// paused is the set of subscriptions an operator has stopped delivery
	// to from the console, and queued is what has piled up behind each
	// pause, oldest first.
	//
	// This is a lab control, not vendor behaviour: the real API has no
	// such switch. It exists because "the bank has not called you yet" is
	// a state a client has to survive and cannot otherwise be held still
	// long enough to look at -- with a real endpoint answering 200 in
	// milliseconds, the window between the event and the webhook is not a
	// window anyone can debug in.
	paused map[string]bool
	queued map[string][]Notification
	// inflight is every batch on its way out: being sent, or waiting out
	// the retry schedule. Kept so a reset can say how many notifications it
	// dropped mid-delivery.
	inflight map[*flight]struct{}
	wg       sync.WaitGroup
}

type queue struct {
	items []Notification
	timer *time.Timer
}

// flight is one batch's delivery, counted by its notifications.
type flight struct{ n int }

// NewDispatcher builds a Dispatcher. logf may be nil.
func NewDispatcher(cfg DeliveryConfig, subs *SubscriptionStore, mail *MailBox, logf func(string, ...any)) *Dispatcher {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if mail == nil {
		mail = &MailBox{}
	}
	return &Dispatcher{
		cfg:      cfg,
		subs:     subs,
		mail:     mail,
		logf:     logf,
		run:      NewRun(),
		queues:   map[string]*queue{},
		pending:  map[string][]Notification{},
		paused:   map[string]bool{},
		queued:   map[string][]Notification{},
		inflight: map[*flight]struct{}{},
	}
}

// Enqueue adds n to sub's batch, flushing immediately if the batch is now
// full and otherwise arming the flush timer. n belongs to the run now; for
// a notification about earlier work, use EnqueueFrom.
func (d *Dispatcher) Enqueue(sub *Subscription, n Notification) {
	d.EnqueueFrom(d.run.Current(), sub, n)
}

// EnqueueFrom is Enqueue for a notification about work that started in run
// started — a payment's (Payment.Epoch). If a reset has ended that run the
// notification is dropped: it is about a payment that no longer exists.
// The check is under the lock a reset clears the queues under, so the
// notification is either queued before the reset (and cleared by it) or
// dropped, never queued after it.
func (d *Dispatcher) EnqueueFrom(started uint64, sub *Subscription, n Notification) {
	// A paused subscription does not batch: the notification is parked
	// whole, so what comes out on resume is what went in, in order, rather
	// than a batch half-assembled before the pause and half after.
	if parked, dropped := d.park(sub, []Notification{n}, started, nil); parked || dropped {
		return
	}

	size := d.batchSize(sub)

	d.mu.Lock()
	if d.run.Moved(started) {
		d.mu.Unlock()
		return
	}
	q, ok := d.queues[sub.ID]
	if !ok {
		q = &queue{}
		d.queues[sub.ID] = q
	}
	q.items = append(q.items, n)

	if len(q.items) >= size {
		batch := q.items
		q.items = nil
		if q.timer != nil {
			q.timer.Stop()
			q.timer = nil
		}
		f := d.launchLocked(len(batch))
		d.mu.Unlock()
		d.deliver(sub, batch, started, f)
		return
	}

	if q.timer == nil {
		subID := sub.ID
		q.timer = time.AfterFunc(time.Duration(d.cfg.BatchFlushInterval), func() {
			d.flush(subID)
		})
	}
	d.mu.Unlock()
}

// batchSize resolves how many notifications go in one message: the
// subscription's own setting, the configured default, then the documented
// one.
func (d *Dispatcher) batchSize(sub *Subscription) int {
	size := sub.MaxNotificationsPerMessage
	if size <= 0 {
		size = d.cfg.DefaultMaxNotificationsPerMessage
	}
	if size <= 0 {
		size = DefaultNotificationsPerMessage
	}
	return size
}

// Flush sends whatever is queued for a subscription right now, without
// waiting for the timer. Tests and the clienttest endpoint want this;
// nothing in the production path needs it.
func (d *Dispatcher) Flush(subscriptionID string) { d.flush(subscriptionID) }

func (d *Dispatcher) flush(subscriptionID string) {
	d.mu.Lock()
	q, ok := d.queues[subscriptionID]
	if !ok || len(q.items) == 0 {
		if ok && q.timer != nil {
			q.timer.Stop()
			q.timer = nil
		}
		d.mu.Unlock()
		return
	}
	batch := q.items
	q.items = nil
	if q.timer != nil {
		q.timer.Stop()
		q.timer = nil
	}
	// Read under d.mu, where a reset clears the queues: whatever was still
	// queued belongs to the run now.
	started := d.run.Current()
	f := d.launchLocked(len(batch))
	d.mu.Unlock()

	sub, err := d.subs.Get(subscriptionID)
	if err != nil {
		d.land(f)
		d.logf("banking-circle: dropping %d notification(s) for deleted subscription %s", len(batch), subscriptionID)
		return
	}
	d.deliver(sub, batch, started, f)
}

// launchLocked counts a batch of n as on its way out. Caller holds d.mu.
func (d *Dispatcher) launchLocked(n int) *flight {
	f := &flight{n: n}
	d.inflight[f] = struct{}{}
	return f
}

// land is a batch's delivery over: delivered, retained or abandoned. It
// lands inside the same Run.Do as the effect that ended it, so a reset
// counts the batch as in flight or as done, never both. A delivery a reset
// dropped does not land; its flight stays for the reset to count, and goes
// with the reset's clear.
func (d *Dispatcher) land(f *flight) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.inflight, f)
}

// deliver runs one batch through the retry schedule in its own goroutine,
// so a subscriber that takes two simulated days to fail never blocks
// payment processing.
func (d *Dispatcher) deliver(sub *Subscription, batch []Notification, started uint64, f *flight) {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.deliverSync(sub, batch, started, f)
	}()
}

// deliverSync is deliver's blocking form. Every step — each attempt, each
// email, the deactivation — goes through the run, so a reset that lands
// anywhere in a two-day schedule stops it there: nothing further is sent,
// mailed or deactivated for a batch from before it.
func (d *Dispatcher) deliverSync(sub *Subscription, batch []Notification, started uint64, f *flight) {
	// The pause may have arrived after this batch was assembled -- a full
	// batch flushes straight out of Enqueue, and the timer fires on its
	// own schedule. Checking again here is what makes the button mean
	// "nothing further leaves" rather than "nothing further is queued".
	if parked, dropped := d.park(sub, batch, started, f); parked || dropped {
		return
	}

	env := Envelope{Notifications: batch}

	var err error
	if !d.run.Do(started, func(ctx context.Context) {
		if err = d.Send(ctx, sub, env); err == nil {
			d.land(f)
		}
	}) {
		return
	}
	if err == nil {
		d.logf("banking-circle: delivered %d notification(s) to %s on the first attempt", len(batch), sub.Endpoint)
		return
	}
	d.logf("banking-circle: initial delivery to %s failed: %v", sub.Endpoint, err)

	for retry := 1; ; retry++ {
		wait, ok := d.cfg.Wait(retry)
		if !ok {
			break
		}
		if !d.run.Sleep(started, wait) {
			return
		}

		gone := false
		if !d.run.Do(started, func(ctx context.Context) {
			// A subscription deleted mid-schedule has nothing left to
			// retry against.
			if _, gerr := d.subs.Get(sub.ID); gerr != nil {
				gone = true
				d.land(f)
				return
			}
			if kind := d.cfg.EmailFor(retry); kind != "" {
				d.sendEmail(sub, kind, retry)
			}
			if err = d.Send(ctx, sub, env); err == nil {
				d.land(f)
			}
		}) {
			return
		}
		if gone {
			d.logf("banking-circle: subscription %s disappeared mid-retry, abandoning %d notification(s)", sub.ID, len(batch))
			return
		}
		if err == nil {
			d.logf("banking-circle: delivered %d notification(s) to %s on retry %d", len(batch), sub.Endpoint, retry)
			return
		}
		d.logf("banking-circle: retry %d to %s failed: %v", retry, sub.Endpoint, err)
	}

	// Schedule exhausted. Deactivate, and hold on to the notifications:
	// they are delivered when the subscription is reactivated. Discarding
	// them here is how a client loses a day of payments and never learns
	// which ones.
	d.run.Do(started, func(context.Context) {
		d.retain(sub.ID, batch)
		d.land(f)
		if _, derr := d.subs.Deactivate(sub.ID,
			"Subscription deactivated after the final unsuccessful delivery retry"); derr != nil {
			d.logf("banking-circle: deactivate %s: %v", sub.ID, derr)
		}
		d.logf("banking-circle: subscription %s (%s) deactivated after %d failed retries; %d notification(s) retained for redelivery",
			sub.ID, sub.Endpoint, d.cfg.DeactivateAfterRetries, len(batch))
	})
}

func (d *Dispatcher) sendEmail(sub *Subscription, kind string, attempt int) {
	subject := "Webhook delivery is failing"
	if kind == "deactivation" {
		subject = "Webhook subscription deactivated"
	}
	to := sub.Email
	if to == "" {
		to = "(no email configured)"
	}
	d.mail.Send(Email{
		To:             to,
		Kind:           kind,
		SubscriptionID: sub.ID,
		Endpoint:       sub.Endpoint,
		Subject:        subject,
		Attempt:        attempt,
	})
	d.logf("banking-circle: %s email for subscription %s to %s (retry %d)", kind, sub.ID, to, attempt)
}

func (d *Dispatcher) retain(subscriptionID string, batch []Notification) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pending[subscriptionID] = append(d.pending[subscriptionID], batch...)
}

// Pending reports how many notifications are held for redelivery.
func (d *Dispatcher) Pending(subscriptionID string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.pending[subscriptionID])
}

// Redeliver re-queues everything retained for a subscription. Call it on
// reactivation.
func (d *Dispatcher) Redeliver(sub *Subscription) int {
	d.mu.Lock()
	held := d.pending[sub.ID]
	delete(d.pending, sub.ID)
	started := d.run.Current() // the run they were held in
	d.mu.Unlock()
	if len(held) == 0 {
		return 0
	}
	d.logf("banking-circle: redelivering %d retained notification(s) to %s", len(held), sub.Endpoint)
	for _, n := range held {
		d.EnqueueFrom(started, sub, n)
	}
	return len(held)
}

// --- pause ------------------------------------------------------------
//
// An operator-controlled hold on one subscription's delivery. Nothing here
// is Banking Circle behaviour; it is the lab's own tap on the pipe, and it
// is deliberately narrow: it stops delivery and it stops nothing else.
// Payments still process, notifications are still produced, still matched
// against subscriptions and still ordered -- they simply wait. A pause
// that quietly stopped events being generated would be a different
// experiment, and would teach a client the wrong thing about what it
// missed.

// Pause stops delivery to one subscription. Returns how many
// notifications are already waiting behind it.
func (d *Dispatcher) Pause(subscriptionID string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.paused[subscriptionID] = true
	return len(d.queued[subscriptionID])
}

// Resume lifts the pause and sends everything parked behind it, oldest
// first, immediately -- someone pressed a button and is watching for the
// batch. It returns how many notifications were released.
//
// The release is cut into batches by the subscription's own
// maxNotificationsPerMessage, so six held notifications against a batch
// size of five come out as five and one: the same shape they would have
// had if nobody had touched the switch.
//
// The batches go out one after another on a single goroutine rather than
// concurrently, which is the one place a release deliberately differs from
// ordinary delivery. Ordinary batches may overtake each other and it does
// not matter; a catch-up is a replay of a queue somebody watched fill up,
// and a replay that arrives shuffled is not a replay. The cost is that a
// failing batch holds up the ones behind it -- which is also what a real
// endpoint coming back to a backlog does.
func (d *Dispatcher) Resume(sub *Subscription) int {
	size := d.batchSize(sub)
	d.mu.Lock()
	delete(d.paused, sub.ID)
	held := d.queued[sub.ID]
	delete(d.queued, sub.ID)
	started := d.run.Current() // the run they were parked in
	var batches [][]Notification
	var flights []*flight
	for start := 0; start < len(held); start += size {
		end := min(start+size, len(held))
		batches = append(batches, held[start:end])
		flights = append(flights, d.launchLocked(end-start))
	}
	d.mu.Unlock()

	if len(held) == 0 {
		d.logf("banking-circle: delivery to %s resumed; nothing was waiting", sub.Endpoint)
		return 0
	}
	d.logf("banking-circle: delivery to %s resumed; releasing %d notification(s)", sub.Endpoint, len(held))

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for i, batch := range batches {
			// A reset ends the release: the batches still to go belong to
			// the run it ended, and are counted by it.
			if d.run.Moved(started) {
				return
			}
			d.deliverSync(sub, batch, started, flights[i])
		}
	}()
	return len(held)
}

// Paused reports whether delivery to a subscription is on hold.
func (d *Dispatcher) Paused(subscriptionID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.paused[subscriptionID]
}

// Queued reports how many notifications are waiting behind a pause.
func (d *Dispatcher) Queued(subscriptionID string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.queued[subscriptionID])
}

// park puts a batch behind the pause if there is one, and reports whether
// it did; or drops it, when it is from a run a reset has ended. Either way
// there is nothing left for the caller to deliver. The paused check and
// the append are one critical section: a resume racing a delivery must
// either release the batch or find it parked, never lose it between the
// two.
//
// f is the batch's flight, when it was on its way out: parked, it is no
// longer in flight but waiting, and counted as such.
func (d *Dispatcher) park(sub *Subscription, batch []Notification, started uint64, f *flight) (parked, dropped bool) {
	d.mu.Lock()
	if d.run.Moved(started) {
		d.mu.Unlock()
		return false, true
	}
	if !d.paused[sub.ID] {
		d.mu.Unlock()
		return false, false
	}
	delete(d.inflight, f)
	d.queued[sub.ID] = append(d.queued[sub.ID], batch...)
	depth := len(d.queued[sub.ID])
	d.mu.Unlock()

	d.logf("banking-circle: delivery to %s is paused; %d notification(s) queued (%d waiting)",
		sub.Endpoint, len(batch), depth)
	if d.OnQueued != nil && !d.run.Moved(started) {
		d.OnQueued(sub, Envelope{Notifications: batch})
	}
	return true, false
}

// Wait blocks until every in-flight delivery has finished. For tests.
func (d *Dispatcher) Wait() { d.wg.Wait() }
