// Package verify models the merchant-verification mock: buddy's real
// verification-service, keyed on merchantApplicationId, gates a merchant's
// progression to UNDERWRITING_APPROVED (and can later PAUSE_SETTLEMENTS).
// This lab mocks it as an always-positive stub with an optional
// force-decline override, per
// docs/ARCHITECTURE-phase3-corrections.md section 3.
//
// Each environment (internal/tenant) has decisions of its own: two
// platforms deciding the same merchantApplicationId never read each
// other's, and a reset of one leaves the other's alone.
package verify

import (
	"context"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// Decision is a single merchant's verification outcome, matching
// VerifyDecisionSnapshotResponseDto's field names verbatim (data.dto.ts,
// prisma/schema.prisma:629-644 for the enum values themselves).
// ManualOverrideDecision is never set by this lab -- it stays the zero
// value so its `omitempty` JSON tag (applied at the wire boundary, not
// here) omits it, matching the real always-null-until-a-human-intervenes
// shape.
type Decision struct {
	MerchantApplicationID  int
	OverallDecision        string
	ManualOverrideDecision string
	AmlDecision            string
	AmlCompanyDecision     string
	RiskDecision           string
	RiskCompanyDecision    string
	RiskScore              int
	RiskClassification     string
	CompletedAt            string
}

// positiveDecision returns the real "approved" defaults, verbatim from
// prisma/schema.prisma:629-638,640-644.
func positiveDecision(id int) Decision {
	return Decision{
		MerchantApplicationID: id,
		OverallDecision:       "APPROVED",
		AmlDecision:           "APPROVED",
		AmlCompanyDecision:    "APPROVED",
		RiskDecision:          "LOW",
		RiskCompanyDecision:   "LOW",
		RiskScore:             5,
		RiskClassification:    "LOW",
	}
}

// declineDecision is what a configured VERIFY_FORCE_DECLINE_IDS entry
// produces instead.
func declineDecision(id int) Decision {
	return Decision{
		MerchantApplicationID: id,
		OverallDecision:       "DECLINE",
		AmlDecision:           "DECLINED",
		AmlCompanyDecision:    "DECLINED",
		RiskDecision:          "HIGH",
		RiskCompanyDecision:   "HIGH",
		RiskScore:             95,
		RiskClassification:    "HIGH",
	}
}

// Engine owns the verification-decision lifecycle: a merchant's decision is
// not readable via Get until `delay` has elapsed after Trigger, mirroring
// the real service's asynchronous decisioning step (and internal/b4b's
// Engine.Submit/process shape: a goroutine sleeps then records, Trigger
// never blocks the caller).
type Engine struct {
	mu    sync.Mutex
	books map[string]*book // by environment
	delay time.Duration
	// forceDecline is the default environment's: VERIFY_FORCE_DECLINE_IDS
	// is the lab's configuration, which is the default's.
	forceDecline map[int]bool

	// running is every trigger's goroutine, for the tests to wait on.
	running sync.WaitGroup
}

// book is one environment's decisions.
type book struct {
	decisions map[int]*Decision
	// pending counts triggers still sleeping out the delay. epoch is what
	// a reset moves, and cancel wakes the sleepers so they see it: a
	// decision triggered before a reset is dropped, never recorded after.
	pending int
	epoch   simctl.Epoch
	ctx     context.Context
	cancel  context.CancelFunc
}

func newBook() *book {
	b := &book{decisions: map[int]*Decision{}}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	return b
}

// NewEngine builds an Engine. forceDecline is the set of
// merchantApplicationIds (VERIFY_FORCE_DECLINE_IDS) that resolve to a
// decline decision instead of the default approval, in the default
// environment.
func NewEngine(delay time.Duration, forceDecline map[int]bool) *Engine {
	return &Engine{
		books:        map[string]*book{},
		delay:        delay,
		forceDecline: forceDecline,
	}
}

// bookLocked is env's book, made on first use. e.mu is held.
func (e *Engine) bookLocked(env string) *book {
	env = tenant.Normalize(env)
	b := e.books[env]
	if b == nil {
		b = newBook()
		e.books[env] = b
	}
	return b
}

// Trigger is TriggerIn for the default environment.
func (e *Engine) Trigger(id int) *Decision { return e.TriggerIn(tenant.Default, id) }

// TriggerIn starts deciding id in env and returns immediately with the
// decision that will be recorded -- the caller (the HTTP layer) uses this
// to build its synchronous 202 response body, even though Get is what a
// poller actually consults. The decision itself is recorded, with
// CompletedAt set, only after `delay` elapses in a background goroutine;
// before then Get reports not-found, matching the real service's "404 until
// ready" shape. A reset of env in between drops it.
func (e *Engine) TriggerIn(env string, id int) *Decision {
	env = tenant.Normalize(env)
	var d Decision
	if env == tenant.Default && e.forceDecline[id] {
		d = declineDecision(id)
	} else {
		d = positiveDecision(id)
	}

	// The caller's copy, taken before the goroutine stamps CompletedAt.
	result := d

	e.mu.Lock()
	b := e.bookLocked(env)
	started, ctx := b.epoch.Current(), b.ctx
	b.pending++
	e.mu.Unlock()

	e.running.Add(1)
	go func() {
		defer e.running.Done()
		if e.delay > 0 {
			t := time.NewTimer(e.delay)
			select {
			case <-t.C:
			case <-ctx.Done(): // a reset: the check below drops it
				t.Stop()
			}
		}
		d.CompletedAt = labclock.Now().Format(time.RFC3339)
		e.mu.Lock()
		defer e.mu.Unlock()
		if b.epoch.Moved(started) {
			return // a reset zeroed pending and cleared the decisions
		}
		b.pending--
		b.decisions[id] = &d
	}()

	return &result
}

// Get is GetIn for the default environment.
func (e *Engine) Get(id int) (*Decision, bool) { return e.GetIn(tenant.Default, id) }

// GetIn returns env's recorded decision for id, or (nil, false) if env
// never triggered it or the trigger's delay has not yet elapsed.
func (e *Engine) GetIn(env string, id int) (*Decision, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b := e.books[tenant.Normalize(env)]
	if b == nil {
		return nil, false
	}
	d, ok := b.decisions[id]
	if !ok {
		return nil, false
	}
	cp := *d
	return &cp, true
}

// Reset is ResetIn for every environment.
func (e *Engine) Reset() (decisions, pending int) { return e.ResetIn(tenant.All) }

// ResetIn forgets every decision recorded in scope (one environment, or
// every one for "*") and drops every trigger of theirs still sleeping out
// its delay, so nothing decided before the reset can become readable after
// it. It returns how many of each it cleared. The force-decline set is
// configuration and stays.
func (e *Engine) ResetIn(scope string) (decisions, pending int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for env, b := range e.books {
		if !tenant.InScope(scope, env) {
			continue
		}
		decisions += len(b.decisions)
		pending += b.pending
		b.epoch.Bump()
		b.cancel()
		b.ctx, b.cancel = context.WithCancel(context.Background())
		b.decisions = map[int]*Decision{}
		b.pending = 0
	}
	return decisions, pending
}

// Forget is env's removal: its decisions and its triggers in flight go,
// as a reset would take them, and its book with them.
func (e *Engine) Forget(env string) (decisions, pending int) {
	decisions, pending = e.ResetIn(env)
	e.mu.Lock()
	delete(e.books, env)
	e.mu.Unlock()
	return decisions, pending
}
