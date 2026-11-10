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
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
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

// lifecycle is which states may follow which. The default walk is its
// spine, B4BAccepted to B4BTMApproved; B4BFailed can cut in from any
// review. It is declared once because everything that needs to know where
// a payment can go reads it: the default walk, a trap's hops (Hop), Restore
// deciding what is unfinished, and the payment.create trap hook's graph.
var lifecycle = map[PaymentState][]PaymentState{
	StateAccepted:          {StateSanctionsPending},
	StateSanctionsPending:  {StateSanctionsApproved, StateFailed},
	StateSanctionsApproved: {StateTMPending, StateFailed},
	StateTMPending:         {StateTMApproved, StateFailed},
}

// Lifecycle is a copy of which states may follow which: the graph of the
// payment.create trap hook (docs/lab-api/traps.md §9.2).
func Lifecycle() map[PaymentState][]PaymentState {
	out := make(map[PaymentState][]PaymentState, len(lifecycle))
	for from, next := range lifecycle {
		out[from] = append([]PaymentState(nil), next...)
	}
	return out
}

// finished reports whether nothing can follow s: B4BTMApproved, B4BFailed,
// or a state this engine does not know.
func finished(s PaymentState) bool { return len(lifecycle[s]) == 0 }

// Hops is the walk from one state to another: the states in between, then
// the one named, by the shortest way. It is empty when from is to, and nil
// when to cannot follow from at all. B4BAccepted to B4BFailed is
// B4BSanctionsPending, B4BFailed: a payment fails out of a review, never
// straight out of acceptance.
func Hops(from, to PaymentState) []PaymentState {
	if from == to {
		return []PaymentState{}
	}
	prev := map[PaymentState]PaymentState{}
	queue := []PaymentState{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, n := range lifecycle[cur] {
			if _, seen := prev[n]; seen || n == from {
				continue
			}
			prev[n] = cur
			if n == to {
				var out []PaymentState
				for s := to; s != from; s = prev[s] {
					out = append([]PaymentState{s}, out...)
				}
				return out
			}
			queue = append(queue, n)
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
	ID string `json:"id"`
	// Env is the environment that sent it (env.go): empty for the default.
	// Its callbacks go to that environment, the bridge call says it is
	// theirs, and only that environment reads it back.
	Env                    string       `json:"env,omitempty"`
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

	// Trap is the trap that caught this payment when it was accepted
	// (docs/lab-api/traps.md §9.2), if one did. Such a payment never takes
	// the default walk: the trap's path takes it, one Hop at a time.
	// FailureReason is the reason that path's B4BFailed step gave. Both are
	// lab-side: the state file and the activity carry them, the wire never
	// does -- a callback or a status read looks exactly as it would
	// without a trap.
	Trap          string `json:"trap,omitempty"`
	FailureReason string `json:"failure_reason,omitempty"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ErrPaymentNotFound is returned by Get for an unknown payment ID.
var ErrPaymentNotFound = errors.New("b4b: payment not found")

// ErrDropped is a hop refused because a reset happened since the payment's
// work began: the payment no longer exists.
var ErrDropped = errors.New("b4b: the payment was dropped by a reset")

// Engine owns the B4B payout lifecycle state machine. It is HTTP-free and
// fully unit-testable: onTransition (nil in tests) is how the caller wires
// in webhook delivery (and the Banking Circle bridge call, at
// StateTMApproved) without the engine knowing anything about HTTP.
type Engine struct {
	mu           sync.Mutex
	payments     map[string]*Payment
	delay        time.Duration // per-hop delay, simulates real bank latency
	onTransition func(p *Payment, started uint64)
	walking      atomic.Int64 // payments still walking their phases

	// gates are the epochs each environment's work runs under: resetting
	// one environment drops its work and nobody else's. One is made the
	// first time an environment needs it and is never dropped, so work
	// holding an old epoch can never meet a fresh gate that reads as
	// unmoved. gatesMu is held through a reset of every environment, so no
	// gate is made while one is under way.
	gatesMu sync.Mutex
	gates   map[string]*Gate
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
		gates:        map[string]*Gate{},
	}
}

// Gate is the default environment's gate: GateFor(tenant.Default).
func (e *Engine) Gate() *Gate { return e.GateFor(tenant.Default) }

// GateFor is the epoch env's payments, and everything done about them or
// about env's boarding, run under. A reset of env holds it. An effect run
// under a gate must not itself ask for one.
func (e *Engine) GateFor(env string) *Gate {
	env = tenant.Normalize(env)
	e.gatesMu.Lock()
	defer e.gatesMu.Unlock()
	g, ok := e.gates[env]
	if !ok {
		g = &Gate{}
		e.gates[env] = g
	}
	return g
}

// Hold is a reset of scope: one environment's gate held (Gate.Hold), or for
// tenant.All every gate at once, with clear run inside them all and told
// how much work the reset drops between them.
func (e *Engine) Hold(scope string, clear func(running int)) {
	if scope = tenant.Normalize(scope); scope != tenant.All {
		e.GateFor(scope).Hold(clear)
		return
	}
	e.gatesMu.Lock()
	defer e.gatesMu.Unlock()
	envs := make([]string, 0, len(e.gates))
	for env := range e.gates {
		envs = append(envs, env)
	}
	sort.Strings(envs)
	var running int
	var hold func(i int)
	hold = func(i int) {
		if i == len(envs) {
			clear(running)
			return
		}
		e.gates[envs[i]].Hold(func(n int) {
			running += n
			hold(i + 1)
		})
	}
	hold(0)
}

// Running is how much work started with a gate's Go is still running, in
// every environment.
func (e *Engine) Running() int {
	e.gatesMu.Lock()
	defer e.gatesMu.Unlock()
	n := 0
	for _, g := range e.gates {
		n += g.Running()
	}
	return n
}

// Accept records p as B4BAccepted, fires that transition, and returns the
// epoch the payment's work runs under: its environment's (p.Env). It does
// not walk p on: Submit does, the default way, and a payment a trap caught
// is taken along the trap's path with Hop instead.
func (e *Engine) Accept(p *Payment) uint64 {
	ts := labclock.Now().Format(time.RFC3339)
	p.Env = owner(p.Env)
	var cp Payment
	started := e.GateFor(p.Env).Begin(func() {
		e.mu.Lock()
		p.State = StateAccepted
		p.CreatedAt = ts
		p.UpdatedAt = ts
		e.payments[p.ID] = p
		cp = *p
		e.mu.Unlock()
	})
	e.notify(cp, started)
	return started
}

// Submit accepts p (firing immediately) and, after `delay` per hop, walks it
// in a goroutine through
// B4BSanctionsPending -> B4BSanctionsApproved -> B4BTMPending -> B4BTMApproved
// -- the default walk; the caller is never blocked waiting on lifecycle
// progression.
func (e *Engine) Submit(p *Payment) {
	e.walking.Add(1)
	started := e.Accept(p)
	go e.process(p, started)
}

// process walks p the default way, to B4BTMApproved, from whatever state it
// has reached. It reads the starting point from p rather than assuming
// B4BAccepted, so a payment restored from the state file resumes instead of
// replaying. Each step is taken only if no reset happened since the
// payment's work started; after one, the payment no longer exists and the
// walk stops where it is.
func (e *Engine) process(p *Payment, started uint64) {
	defer e.walking.Add(-1)
	e.mu.Lock()
	hops := Hops(p.State, StateTMApproved)
	e.mu.Unlock()

	for _, st := range hops {
		e.sleep()
		if !e.advance(p, started, st) {
			return
		}
	}
}

// advance moves p to `to` and fires onTransition, unless a reset moved the
// epoch since started, in which case nothing happens.
func (e *Engine) advance(p *Payment, started uint64, to PaymentState) bool {
	var cp Payment
	ok := e.GateFor(p.Env).Do(started, func() {
		e.mu.Lock()
		p.State = to
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

// Hop moves env's payment id one hop, to `to`, on behalf of trap: the hop a
// trap's path asks for (cmd/b4b paces them). `to` must directly follow the
// state the payment is in. Like every hop of the default walk it goes
// through env's gate under started, so a reset since refuses it with
// ErrDropped, and it fires onTransition -- the same callback, the same
// bridge call at B4BTMApproved. reason is kept on a payment it fails.
func (e *Engine) Hop(env, id string, started uint64, to PaymentState, trap, reason string) error {
	var cp Payment
	var err error
	own := owner(env)
	ran := e.GateFor(env).Do(started, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		p, ok := e.payments[id]
		if !ok || p.Env != own {
			err = ErrPaymentNotFound
			return
		}
		if h := Hops(p.State, to); len(h) != 1 {
			err = fmt.Errorf("b4b: payment %s is %s; %s does not follow it", id, p.State, to)
			return
		}
		p.State = to
		p.Trap = trap
		if to == StateFailed {
			p.FailureReason = reason
		}
		p.UpdatedAt = labclock.Now().Format(time.RFC3339)
		cp = *p
	})
	if !ran {
		return ErrDropped
	}
	if err != nil {
		return err
	}
	e.notify(cp, started)
	return nil
}

// Delay is the wait before each hop: B4B_PROCESSING_DELAY.
func (e *Engine) Delay() time.Duration { return e.delay }

func (e *Engine) sleep() {
	if e.delay > 0 {
		time.Sleep(e.delay)
	}
}

// Walking is how many payment walks are still running, including any a
// reset dropped that have not yet woken to find out.
func (e *Engine) Walking() int { return int(e.walking.Load()) }

// Clear drops every payment, in every environment: ClearIn(tenant.All).
func (e *Engine) Clear() (payments, inFlight int) { return e.ClearIn(tenant.All) }

// ClearIn drops scope's payments and says how many there were, and how many
// of them had not reached a terminal state. Call it inside Hold(scope): the
// walkers it leaves behind are then refused their next step.
func (e *Engine) ClearIn(scope string) (payments, inFlight int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, p := range e.payments {
		if !tenant.InScope(scope, p.Env) {
			continue
		}
		payments++
		if !finished(p.State) {
			inFlight++
		}
		delete(e.payments, id)
	}
	return payments, inFlight
}

// Get returns a copy of the payment by ID, whichever environment's it is.
func (e *Engine) Get(id string) (*Payment, error) { return e.GetIn(tenant.All, id) }

// GetIn returns a copy of one of scope's payments by ID: another
// environment's reads as unknown.
func (e *Engine) GetIn(scope, id string) (*Payment, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.payments[id]
	if !ok || !tenant.InScope(scope, p.Env) {
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
