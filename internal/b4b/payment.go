// Package b4b models the B4B Payments mock: the payout rail Infinite pays
// merchants through (buddy's real accounts-settlement -> B4B -> Banking
// Circle chain), corrected per docs/ARCHITECTURE-vendor-corrections.md
// section 4 and its Addendum sections A/D/F, and extended with the
// company-boarding half of the Oversight API per
// docs/ARCHITECTURE-b4b-oversight.md.
//
// See payment.go for the payout lifecycle engine, beneficiary.go for
// beneficiaries and their gates, company.go/directory.go for boarding
// (companies, addresses, people, extended profiles, documents, vIBANs),
// state.go for persistence, jwt.go for inbound RS512 verification, and
// request.go for the one piece of creation-time validation shared with
// cmd/b4b.
package b4b

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/labclock"
)

// PaymentState is the B4B payout lifecycle state, exact wire strings per
// docs/ARCHITECTURE-vendor-corrections.md section 4.
type PaymentState string

const (
	StateAccepted          PaymentState = "B4BAccepted"
	StateSanctionsPending  PaymentState = "B4BSanctionsPending"
	StateSanctionsApproved PaymentState = "B4BSanctionsApproved"
	StateTMPending         PaymentState = "B4BTMPending"
	StateTMApproved        PaymentState = "B4BTMApproved"
	StateFailed            PaymentState = "B4BFailed"
)

// stateOrder is the walk a payment makes before its terminal state. It is
// declared once, rather than inline in process(), because Restore needs to
// know where in it a payment had got to.
var stateOrder = []PaymentState{StateAccepted, StateSanctionsPending, StateSanctionsApproved, StateTMPending}

// remainingStates returns the states still to walk after `from`, or nil if
// `from` is terminal or unknown. A zero-length (but non-nil) result means
// "only the terminal hop is left" -- the distinction Restore uses to decide
// whether a payment is resumable.
func remainingStates(from PaymentState) []PaymentState {
	for i, s := range stateOrder {
		if s == from {
			return stateOrder[i+1:]
		}
	}
	return nil
}

// Amount is a decimal amount + ISO currency, matching B4B's own
// {amount,currency} wire shape.
type Amount struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// AccountRef is B4B's {account,financialInstitution?,country?} wire shape,
// used for debtorViban/debtorAccount/creditorAccount.
type AccountRef struct {
	Account              string `json:"account"`
	FinancialInstitution string `json:"financialInstitution,omitempty"`
	Country              string `json:"country,omitempty"`
}

// Payment is a single B4B payout, from creation through its terminal state.
// It carries every field B4B needs to echo back in its create response and
// webhooks, plus external_ref/callback_url retained from the request for
// later use (external_ref forwarded to Banking Circle at approval,
// callback_url as this payment's own webhook destination).
//
// The JSON tags are for the state file (state.go), not for any wire shape:
// the handlers in cmd/b4b build the wire bodies explicitly, because what
// B4B echoes back is a subset of this and shaped differently.
type Payment struct {
	ID                     string       `json:"id"`
	ExternalRef            string       `json:"external_ref,omitempty"`
	BeneficiaryID          string       `json:"beneficiary_id"`
	CompanyID              string       `json:"company_id,omitempty"`
	CallbackURL            string       `json:"callback_url,omitempty"`
	SCAApplied             bool         `json:"sca_applied"`
	Amount                 Amount       `json:"amount"`
	CurrencyOfTransfer     string       `json:"currency_of_transfer"`
	DebtorViban            AccountRef   `json:"debtor_viban"`
	DebtorAccount          *AccountRef  `json:"debtor_account,omitempty"`
	CreditorAccount        AccountRef   `json:"creditor_account"`
	CreditorName           string       `json:"creditor_name"`
	ChargeBearer           string       `json:"charge_bearer"`
	RequestedExecutionDate string       `json:"requested_execution_date,omitempty"`
	RemittanceInformation  string       `json:"remittance_information,omitempty"`
	DebtorReference        string       `json:"debtor_reference,omitempty"`
	State                  PaymentState `json:"state"`

	// ForceFail is decided once, at creation time, from
	// B4B_FORCE_FAILURE_BENEFICIARY_IDS -- the lab's manual failure-injection
	// control. When true, the payment diverts to StateFailed at the point
	// StateTMApproved would otherwise be reached, deterministically.
	ForceFail bool `json:"force_fail,omitempty"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ErrPaymentNotFound is returned by Get for an unknown payment ID.
var ErrPaymentNotFound = errors.New("b4b: payment not found")

// Engine owns the B4B payout lifecycle state machine. It is HTTP-free and
// fully unit-testable: onTransition (nil in tests) is how the caller wires
// in webhook delivery (and the Banking Circle bridge call, at
// StateTMApproved) without the engine knowing anything about HTTP.
type Engine struct {
	mu           sync.Mutex
	payments     map[string]*Payment
	delay        time.Duration // per-hop delay, simulates real bank latency
	onTransition func(p *Payment, started uint64)
	gate         Gate
	walking      atomic.Int64 // payments still walking their phases
}

// NewEngine builds an Engine. onTransition fires (with a value copy, safe to
// read without further locking) on every state transition, including the
// initial B4BAccepted one, with the epoch the payment's work started under:
// whatever the caller does about the transition later -- a callback, the
// bridge call -- goes through Gate().Do with it, so a reset in between drops
// it. Pass nil to disable.
func NewEngine(delay time.Duration, onTransition func(p *Payment, started uint64)) *Engine {
	return &Engine{
		payments:     map[string]*Payment{},
		delay:        delay,
		onTransition: onTransition,
	}
}

// Gate is the epoch this engine's payments, and everything done about them,
// run under. A reset holds it.
func (e *Engine) Gate() *Gate { return &e.gate }

// Submit records p as B4BAccepted (firing immediately) and, after `delay`
// per hop, progresses it in a goroutine through
// B4BSanctionsPending -> B4BSanctionsApproved -> B4BTMPending -> B4BTMApproved
// (or B4BFailed, if p.ForceFail) -- the caller is never blocked waiting on
// lifecycle progression.
func (e *Engine) Submit(p *Payment) {
	ts := labclock.Now().Format(time.RFC3339)
	var cp Payment
	started := e.gate.Begin(func() {
		e.mu.Lock()
		p.State = StateAccepted
		p.CreatedAt = ts
		p.UpdatedAt = ts
		e.payments[p.ID] = p
		cp = *p
		e.mu.Unlock()
		e.walking.Add(1)
	})
	e.notify(cp, started)
	go e.process(p, started)
}

// process walks p through whatever states it has not reached yet. It reads
// the starting point from p rather than assuming B4BAccepted, so a payment
// restored from the state file resumes instead of replaying. Each step is
// taken only if no reset happened since the payment's work started; after
// one, the payment no longer exists and the walk stops where it is.
func (e *Engine) process(p *Payment, started uint64) {
	defer e.walking.Add(-1)
	e.mu.Lock()
	rest := remainingStates(p.State)
	e.mu.Unlock()

	for _, st := range rest {
		e.sleep()
		if !e.advance(p, started, func() PaymentState { return st }) {
			return
		}
	}

	e.sleep()
	e.advance(p, started, func() PaymentState {
		if p.ForceFail {
			return StateFailed
		}
		return StateTMApproved
	})
}

// advance moves p to the state next names and fires onTransition, unless a
// reset moved the epoch since started, in which case nothing happens.
func (e *Engine) advance(p *Payment, started uint64, next func() PaymentState) bool {
	var cp Payment
	ok := e.gate.Do(started, func() {
		e.mu.Lock()
		p.State = next()
		p.UpdatedAt = labclock.Now().Format(time.RFC3339)
		cp = *p
		e.mu.Unlock()
	})
	if !ok {
		return false
	}
	e.notify(cp, started)
	return true
}

func (e *Engine) sleep() {
	if e.delay > 0 {
		time.Sleep(e.delay)
	}
}

// Walking is how many payment walks are still running, including any a
// reset dropped that have not yet woken to find out.
func (e *Engine) Walking() int { return int(e.walking.Load()) }

// Clear drops every payment and says how many there were, and how many of
// them had not reached a terminal state. Call it inside Gate().Hold: the
// walkers it leaves behind are then refused their next step.
func (e *Engine) Clear() (payments, inFlight int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range e.payments {
		if remainingStates(p.State) != nil {
			inFlight++
		}
	}
	payments = len(e.payments)
	e.payments = map[string]*Payment{}
	return payments, inFlight
}

// Get returns a copy of the payment by ID.
func (e *Engine) Get(id string) (*Payment, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.payments[id]
	if !ok {
		return nil, ErrPaymentNotFound
	}
	cp := *p
	return &cp, nil
}

// List returns a copy of every payment.
func (e *Engine) List() []*Payment {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*Payment, 0, len(e.payments))
	for _, p := range e.payments {
		cp := *p
		out = append(out, &cp)
	}
	return out
}

func (e *Engine) notify(p Payment, started uint64) {
	if e.onTransition != nil {
		e.onTransition(&p, started)
	}
}
