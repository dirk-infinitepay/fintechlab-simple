package bankingcircle

import (
	"sync"

	"github.com/webduvet/fintechlab-simple/internal/money"
)

// Bank is Banking Circle's state as one thing, for a reset to put back
// (docs/lab-api/sim-control.md §6): the ledger, the payments and their
// lifecycle, the notification pipe and the emails it has sent, and the
// subscriptions.
//
// A reset is one instant for all of them. It waits for every effect in
// flight (Run.Do), then holds every lock at once while it moves the epoch
// and clears, so a request racing it lands wholly before it (and is
// cleared) or wholly after (and is kept), and work from before it finds
// the epoch moved at its next step and drops what it was about to do.
type Bank struct {
	mu     sync.Mutex // one reset at a time
	engine *Engine
	ledger *Ledger
	disp   *Dispatcher
	mail   *MailBox
	subs   *SubscriptionStore
	// seed is the subscriptions as start-up left them: what a reset of
	// scope all puts back.
	seed subscriptionState
}

// NewBank gathers the engine and the dispatcher, and the ledger, mailbox
// and subscriptions they hold, for a reset. Call it once start-up has
// seeded what it seeds and before any work starts: the subscriptions as
// they stand now are what a reset of scope all puts back, and from here on
// the dispatcher follows the engine's run, so one reset ends both.
func NewBank(e *Engine, d *Dispatcher) *Bank {
	d.run = e.run
	return &Bank{engine: e, ledger: e.ledger, disp: d, mail: d.mail, subs: d.subs, seed: d.subs.snapshot()}
}

// Reset puts the bank back to a known state and counts what it cleared, by
// kind.
//
// Both scopes clear what a run makes: every payment (outgoing, incoming,
// returns), every balance back to its seed (zero), every notification not
// yet delivered (batching, parked behind a pause, retained after a
// deactivation, or mid-retry), the pauses themselves, and the emails. The
// work in flight for any of them is dropped, never completed. Payment
// reference numbers keep counting, so a platform that kept its books never
// sees one reused.
//
// all also puts back what a platform set up: the subscriptions as they were
// seeded at start-up, and the ledger with only its seeded accounts; and
// reference numbers start again at 1, as on first start.
//
// also, when given, runs inside the reset, after the bank is cleared and
// before anything can run again: the caller's own run state (its activity
// logs) is cleared at the same instant. It must not call into the bank.
func (b *Bank) Reset(all bool, also func(cleared map[string]int)) map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	run := b.engine.run

	// Wake everything sleeping out a delay or a retry wait and abort any
	// send on the wire, then wait for the effects in flight to finish.
	// Past this point none is running and none can start.
	run.stop()
	run.gate.Lock()
	defer run.gate.Unlock()

	// Every lock, in the order the code takes them elsewhere (the engine
	// moves money under its own lock).
	b.engine.mu.Lock()
	defer b.engine.mu.Unlock()
	b.ledger.mu.Lock()
	defer b.ledger.mu.Unlock()
	b.disp.mu.Lock()
	defer b.disp.mu.Unlock()
	b.mail.mu.Lock()
	defer b.mail.mu.Unlock()
	b.subs.mu.Lock()
	defer b.subs.mu.Unlock()

	run.next()

	cleared := map[string]int{}
	cleared["payments"] = b.engine.resetLocked(all)
	var accounts int
	cleared["balances"], accounts = b.ledger.resetLocked(all)
	cleared["notifications"], cleared["pauses"] = b.disp.resetLocked()
	cleared["emails"] = b.mail.clearLocked()
	if all {
		cleared["accounts"] = accounts
		cleared["subscriptions"] = b.subs.restoreLocked(b.seed)
	}
	if also != nil {
		also(cleared)
	}
	return cleared
}

// resetLocked forgets every payment. Caller holds e.mu. A trap's steps
// still to come are the trap store's to cancel (simctl resets it first), and
// a step already under way lands before this or not at all (Booking.Step).
func (e *Engine) resetLocked(all bool) (payments int) {
	payments = len(e.payments)
	e.payments = map[string]*Payment{}
	if all {
		e.refSeq = 0
	}
	return payments
}

// resetLocked puts every balance back to its seed -- zero, unless a test
// seeded otherwise -- and with all drops the accounts payouts opened.
// balances counts the accounts that were not at their seed; accounts, the
// ones dropped. Caller holds l.mu.
func (l *Ledger) resetLocked(all bool) (balances, accounts int) {
	for id, a := range l.accts {
		seed, seeded := l.seeded[id]
		if a.cents != seed {
			balances++
		}
		if all && !seeded {
			delete(l.accts, id)
			delete(l.byVIBAN, a.VIBAN)
			accounts++
			continue
		}
		a.cents, a.Balance = seed, money.Format(seed)
		a.openDay, a.openCents = "", 0
	}
	return balances, accounts
}

// resetLocked drops every notification not yet delivered -- batching,
// parked, retained or on its way out -- stops the flush timers and lifts
// every pause. The deliveries in flight find the epoch moved at their next
// step. Caller holds d.mu.
func (d *Dispatcher) resetLocked() (notifications, pauses int) {
	for _, q := range d.queues {
		notifications += len(q.items)
		if q.timer != nil {
			q.timer.Stop()
		}
	}
	for _, held := range d.pending {
		notifications += len(held)
	}
	for _, held := range d.queued {
		notifications += len(held)
	}
	for f := range d.inflight {
		notifications += f.n
	}
	pauses = len(d.paused)
	d.queues = map[string]*queue{}
	d.pending = map[string][]Notification{}
	d.paused = map[string]bool{}
	d.queued = map[string][]Notification{}
	d.inflight = map[*flight]struct{}{}
	return notifications, pauses
}
