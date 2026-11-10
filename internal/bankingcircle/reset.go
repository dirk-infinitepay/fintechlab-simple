package bankingcircle

import (
	"sync"

	"github.com/webduvet/fintechlab-simple/internal/money"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// Bank is Banking Circle's state as one thing, for a reset to put back
// (docs/lab-api/sim-control.md §6): the ledger, the payments and their
// lifecycle, the notification pipe and the emails it has sent, and the
// subscriptions.
//
// A reset is one instant for all of them. It waits for every effect in
// flight it ends (Run.Do), then holds every lock at once while it moves the
// run on and clears, so a request racing it lands wholly before it (and is
// cleared) or wholly after (and is kept), and work from before it finds the
// run moved at its next step and drops what it was about to do.
//
// A reset names an environment (internal/tenant), or every one: it clears
// that environment's records only, and ends only that environment's work in
// flight. Every record says whose it is (Account.Env, Payment.Env,
// Subscription.Env, Email.Env).
type Bank struct {
	mu     sync.Mutex // one reset at a time
	engine *Engine
	ledger *Ledger
	disp   *Dispatcher
	mail   *MailBox
	subs   *SubscriptionStore
	// seed is the subscriptions as start-up left them: what a reset of
	// scope all puts back. They are the default environment's.
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

// Reset is ResetIn of every environment: the bank back to a known state.
func (b *Bank) Reset(all bool, also func(cleared map[string]int)) map[string]int {
	return b.ResetIn(tenant.All, all, also)
}

// ResetIn puts what scope names back to a known state -- one environment's
// records, or every environment's (tenant.All) -- and counts what it
// cleared, by kind.
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
// seeded at start-up (the default environment's; another environment had
// none), and the ledger with only its seeded accounts, the safeguarding
// accounts. Reference numbers start again at 1, as on first start, once no
// payment is left at all.
//
// also, when given, runs inside the reset, after the bank is cleared and
// before anything can run again: the caller's own run state (its activity
// logs) is cleared at the same instant. It must not call into the bank.
func (b *Bank) ResetIn(scope string, all bool, also func(cleared map[string]int)) map[string]int {
	return b.reset(tenant.Normalize(scope), all, false, also)
}

// Forget clears everything env has, as a reset of scope all does, and its
// safeguarding accounts with it: the customer is gone (DELETE
// /sim/customers/{env}). It never forgets the default environment, which
// is the bank as it always was.
func (b *Bank) Forget(env string, also func(cleared map[string]int)) map[string]int {
	env = tenant.Normalize(env)
	if env == tenant.Default || env == tenant.All {
		return b.ResetIn(env, true, also)
	}
	return b.reset(env, true, true, also)
}

func (b *Bank) reset(scope string, all, forget bool, also func(cleared map[string]int)) map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	run := b.engine.run

	// Wake everything of scope's sleeping out a delay or a retry wait and
	// abort any send of its on the wire, then wait for its effects in
	// flight to finish. Past this point none is running and none can start.
	run.stop(scope)
	release := run.hold(scope)
	defer release()

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

	run.next(scope)

	cleared := map[string]int{}
	cleared["payments"] = b.engine.resetLocked(scope, all)
	var accounts int
	cleared["balances"], accounts = b.ledger.resetLocked(scope, all, forget)
	cleared["notifications"], cleared["pauses"] = b.disp.resetLocked(scope, b.subs.ownerLocked)
	cleared["emails"] = b.mail.clearLocked(scope)
	if all {
		cleared["accounts"] = accounts
		cleared["subscriptions"] = b.subs.restoreLocked(scope, b.seed)
	}
	if also != nil {
		also(cleared)
	}
	return cleared
}

// resetLocked forgets scope's payments. Caller holds e.mu. A trap's steps
// still to come are the trap store's to cancel (simctl resets it first), and
// a step already under way lands before this or not at all (Booking.Step).
func (e *Engine) resetLocked(scope string, all bool) (payments int) {
	for id, p := range e.payments {
		if tenant.InScope(scope, p.Env) {
			delete(e.payments, id)
			payments++
		}
	}
	if all && len(e.payments) == 0 {
		e.refSeq = 0
	}
	return payments
}

// resetLocked puts scope's balances back to their seed -- zero, unless a
// test seeded otherwise -- and with all drops the accounts payouts opened;
// with forget, the seeded ones too. balances counts the accounts that were
// not at their seed; accounts, the ones dropped. Caller holds l.mu.
func (l *Ledger) resetLocked(scope string, all, forget bool) (balances, accounts int) {
	for id, a := range l.accts {
		if !tenant.InScope(scope, a.Env) {
			continue
		}
		seed, seeded := l.seeded[id]
		if a.cents != seed {
			balances++
		}
		if forget || (all && !seeded) {
			delete(l.accts, id)
			delete(l.byVIBAN, a.VIBAN)
			delete(l.seeded, id)
			accounts++
			continue
		}
		a.cents, a.Balance = seed, money.Format(seed)
		a.openDay, a.openCents = "", 0
	}
	return balances, accounts
}

// ownerLocked is the environment of the subscription id names, for the
// dispatcher's reset: the store's when it still has it, else "" (the
// dispatcher remembers its own). Caller holds s.mu.
func (s *SubscriptionStore) ownerLocked(id string) (string, bool) {
	if sub, ok := s.subs[id]; ok {
		return sub.Env, true
	}
	return "", false
}

// resetLocked drops scope's notifications not yet delivered -- batching,
// parked, retained or on their way out -- stops their flush timers and
// lifts their pauses. The deliveries in flight find the run moved at their
// next step. owner is the subscription store's view of whose a subscription
// is. Caller holds d.mu.
func (d *Dispatcher) resetLocked(scope string, owner func(id string) (string, bool)) (notifications, pauses int) {
	whose := func(id string) string {
		if env, ok := owner(id); ok {
			return env
		}
		return d.owners[id]
	}
	for id, q := range d.queues {
		if !tenant.InScope(scope, q.env) {
			continue
		}
		notifications += len(q.items)
		if q.timer != nil {
			q.timer.Stop()
		}
		delete(d.queues, id)
	}
	for id, held := range d.pending {
		if tenant.InScope(scope, whose(id)) {
			notifications += len(held)
			delete(d.pending, id)
		}
	}
	for id, held := range d.queued {
		if tenant.InScope(scope, whose(id)) {
			notifications += len(held)
			delete(d.queued, id)
		}
	}
	for f := range d.inflight {
		if tenant.InScope(scope, f.env) {
			notifications += f.n
			delete(d.inflight, f)
		}
	}
	for id := range d.paused {
		if tenant.InScope(scope, whose(id)) {
			pauses++
			delete(d.paused, id)
		}
	}
	for id, env := range d.owners {
		if tenant.InScope(scope, env) {
			delete(d.owners, id)
		}
	}
	return notifications, pauses
}
