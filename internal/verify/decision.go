// Package verify models the merchant-verification mock: buddy's real
// verification-service, keyed on merchantApplicationId, gates a merchant's
// progression to UNDERWRITING_APPROVED (and can later PAUSE_SETTLEMENTS).
// This lab mocks it as an always-positive stub with an optional
// force-decline override, per
// docs/ARCHITECTURE-phase3-corrections.md section 3.
package verify

import (
	"context"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
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
	mu           sync.Mutex
	decisions    map[int]*Decision
	delay        time.Duration
	forceDecline map[int]bool

	// pending counts triggers still sleeping out the delay. epoch is what
	// Reset moves, and cancel wakes the sleepers so they see it: a
	// decision triggered before a reset is dropped, never recorded after.
	pending int
	epoch   simctl.Epoch
	ctx     context.Context
	cancel  context.CancelFunc
	// running is every trigger's goroutine, for the tests to wait on.
	running sync.WaitGroup
}

// NewEngine builds an Engine. forceDecline is the set of
// merchantApplicationIds (VERIFY_FORCE_DECLINE_IDS) that resolve to a
// decline decision instead of the default approval.
func NewEngine(delay time.Duration, forceDecline map[int]bool) *Engine {
	e := &Engine{
		decisions:    map[int]*Decision{},
		delay:        delay,
		forceDecline: forceDecline,
	}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	return e
}

// Trigger starts deciding id and returns immediately with the decision that
// will be recorded -- the caller (the HTTP layer) uses this to build its
// synchronous 202 response body, even though Get is what a poller actually
// consults. The decision itself is recorded, with CompletedAt set, only
// after `delay` elapses in a background goroutine; before then Get reports
// not-found, matching the real service's "404 until ready" shape. A Reset
// in between drops it.
func (e *Engine) Trigger(id int) *Decision {
	var d Decision
	if e.forceDecline[id] {
		d = declineDecision(id)
	} else {
		d = positiveDecision(id)
	}

	// The caller's copy, taken before the goroutine stamps CompletedAt.
	result := d

	e.mu.Lock()
	started, ctx := e.epoch.Current(), e.ctx
	e.pending++
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
		if e.epoch.Moved(started) {
			return // Reset zeroed pending and cleared the decisions
		}
		e.pending--
		e.decisions[id] = &d
	}()

	return &result
}

// Get returns the recorded decision for id, or (nil, false) if it was never
// triggered or the trigger's delay has not yet elapsed.
func (e *Engine) Get(id int) (*Decision, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, ok := e.decisions[id]
	if !ok {
		return nil, false
	}
	cp := *d
	return &cp, true
}

// Reset forgets every recorded decision and drops every trigger still
// sleeping out its delay, so nothing decided before the reset can become
// readable after it. It returns how many of each it cleared. The
// force-decline set is configuration and stays.
func (e *Engine) Reset() (decisions, pending int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	decisions, pending = len(e.decisions), e.pending
	e.epoch.Bump()
	e.cancel()
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.decisions = map[int]*Decision{}
	e.pending = 0
	return decisions, pending
}
