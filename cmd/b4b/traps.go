package main

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/b4b"
	"github.com/webduvet/fintechlab-simple/internal/trap"
)

// Traps on b4b (docs/lab-api/traps.md §9.2): one hook, payment.create, on
// a payout the gates let through. A trap that catches one decides its walk
// instead of the default: where it stops, whether it fails, and when.

const hookPaymentCreate = "payment.create"

// paymentHook is the payment.create hook. Its graph is the engine's own
// lifecycle, and it walks: a step may name a state further along, and b4b
// takes the payment through the states in between at its normal pace.
func paymentHook(delay time.Duration) trap.Hook {
	next := map[string][]string{}
	for from, tos := range b4b.Lifecycle() {
		for _, to := range tos {
			next[string(from)] = append(next[string(from)], string(to))
		}
	}
	return trap.Hook{
		Name: hookPaymentCreate,
		About: "A payout accepted by POST /oversight/v1/payments, once it cleared the gates. " +
			"A step may name a state further along: b4b walks the states in between, one hop per B4B_PROCESSING_DELAY, " +
			"and calls back on each. A step's after or after_lab is the wait before its first hop. " +
			"Reaching B4BTMApproved bridges it into Banking Circle; a path that stops short of a final state leaves the payment there for good.",
		Subject: "payment_id",
		Match: []trap.Field{
			{Name: "beneficiary_id", Type: trap.TypeString, About: "as sent on the payment: a B4B id or the payee's external_ref"},
			{Name: "beneficiary.external_ref", Type: trap.TypeString, About: "the payee's external_ref, the MID the lab registered it with; absent for a payee never registered"},
			{Name: "company_id", Type: trap.TypeString, About: "as sent on the payment"},
			{Name: "debtor_viban", Type: trap.TypeIBAN, About: "debtorViban.account"},
			{Name: "creditor.account", Type: trap.TypeIBAN, About: "creditorAccount.account, an IBAN or an account number; compared upper case, spaces removed"},
			{Name: "amount", Type: trap.TypeDecimal, About: "amount.amount"},
			{Name: "currency", Type: trap.TypeString, About: "currencyOfTransfer, the currency that moves"},
			{Name: "external_ref", Type: trap.TypeString, About: "the client's reference for the payout"},
		},
		Effect: trap.EffectPath,
		Graph: &trap.Graph{
			Start:  string(b4b.StateAccepted),
			Next:   next,
			Params: map[string][]string{string(b4b.StateFailed): {"reason"}},
			Walks:  true,
		},
		// A first step that names no after takes its first hop this long
		// after the payment was accepted, as the default walk does.
		FirstAfter: delay,
	}
}

// newTraps is b4b's trap store, with a standing trap for each id in
// B4B_FORCE_FAILURE_BENEFICIARY_IDS: every payment to that beneficiary_id
// walks to B4BTMPending and fails there, which is what the variable always
// did. Standing, a reset arms them again.
func newTraps(delay time.Duration, forceFail []string) (*trap.Store, error) {
	s := trap.NewStore("b4b", paymentHook(delay))
	for _, id := range forceFail {
		if _, err := s.ArmStanding(trap.Spec{
			Label:   "B4B_FORCE_FAILURE_BENEFICIARY_IDS " + id,
			Hook:    hookPaymentCreate,
			Match:   map[string]any{"beneficiary_id": id},
			Forever: true,
			Path:    []trap.Step{{To: string(b4b.StateTMPending)}, {To: string(b4b.StateFailed)}},
		}); err != nil {
			return nil, fmt.Errorf("B4B_FORCE_FAILURE_BENEFICIARY_IDS %s: %w", id, err)
		}
	}
	return s, nil
}

// parseForceFailIDs reads B4B_FORCE_FAILURE_BENEFICIARY_IDS, a comma-separated
// list, in order and once each.
func parseForceFailIDs(spec string) []string {
	var out []string
	seen := map[string]bool{}
	for _, raw := range strings.Split(spec, ",") {
		id := strings.TrimSpace(raw)
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// paymentEvent is a payment as the payment.create hook's traps see it: its
// environment's, so only a trap armed there takes it. A field the payment
// does not have is left out, so it matches nothing.
func paymentEvent(p *b4b.Payment, ben *b4b.Beneficiary) trap.Event {
	fields := map[string]string{
		"beneficiary_id":   p.BeneficiaryID,
		"company_id":       p.CompanyID,
		"debtor_viban":     p.DebtorViban.Account,
		"creditor.account": p.CreditorAccount.Account,
		"amount":           p.Amount.Amount,
		"currency":         p.CurrencyOfTransfer,
		"external_ref":     p.ExternalRef,
	}
	if ben != nil {
		fields["beneficiary.external_ref"] = ben.ExternalRef
	}
	for k, v := range fields {
		if v == "" {
			delete(fields, k)
		}
	}
	return trap.Event{Env: p.Env, Hook: hookPaymentCreate, Subject: p.ID, Fields: fields}
}

// accept starts a payment that cleared the gates on its way, offering it
// to the traps first. Uncaught, the engine walks it the default way, to
// B4BTMApproved and the bridge. Caught, the default walk never runs: the
// trap's path takes it.
func (a *app) accept(p *b4b.Payment, ben *b4b.Beneficiary) {
	var caught *trap.Caught
	if a.traps != nil {
		caught, _ = a.traps.Offer(paymentEvent(p, ben))
	}
	if caught == nil {
		a.engine.Submit(p)
		return
	}
	p.Trap = caught.TrapID
	started := a.engine.Accept(p)
	if err := a.takeAlong(caught, p.Env, p.ID, b4b.StateAccepted, started); err != nil {
		log.Printf("b4b: trap %s caught payment %s and cannot take it: %v; it stays %s", caught.TrapID, p.ID, err, b4b.StateAccepted)
	}
}

// takeAlong runs env's caught payment along its trap's path, from the
// state it is in, one hop at a time (walkPath). Each hop goes through the
// engine's Hop, under the epoch the payment's work began in: the gate a
// reset holds, the transition, the callback, the bridge call at
// B4BTMApproved -- all exactly as a hop of the default walk.
func (a *app) takeAlong(c *trap.Caught, env, id string, from b4b.PaymentState, started uint64) error {
	hops, err := walkPath(c.Path, from, a.engine.Delay())
	if err != nil {
		return err
	}
	c.Path = hops
	c.Run(func(st trap.Step) error {
		return a.engine.Hop(env, id, started, b4b.PaymentState(st.To), c.TrapID, st.Reason)
	})
	return nil
}

// walkPath is a trap's path as b4b takes it: one step per hop. A step may
// name a state further along (traps.md §9.2), and b4b walks the states in
// between at its normal pace, calling back on each -- so each step becomes
// the hops to its state. The first hop waits the step's own after or
// after_lab, or B4B_PROCESSING_DELAY when it names neither; every hop after
// it waits B4B_PROCESSING_DELAY, as in the default walk. Only the hop to
// the step's own state carries its reason. A step to where the payment
// already is takes no hop.
//
// The hops are what the trap then schedules, so its pending_steps name the
// next hop and when it is due, its caught[].applied lists each state the
// payment reached, and disarming it or resetting b4b stops the walk at the
// next hop rather than at the next step.
func walkPath(path []trap.Step, from b4b.PaymentState, delay time.Duration) ([]trap.Step, error) {
	out := []trap.Step{}
	cur := from
	for _, st := range path {
		to := b4b.PaymentState(st.To)
		hops := b4b.Hops(cur, to)
		if hops == nil {
			return nil, fmt.Errorf("%s cannot follow %s", to, cur)
		}
		for i, h := range hops {
			hop := trap.Step{To: string(h), After: delay.String()}
			if i == 0 && (st.After != "" || st.AfterLab != "") {
				hop.After, hop.AfterLab = st.After, st.AfterLab
			}
			if h == to {
				hop.Reason = st.Reason
			}
			out = append(out, hop)
		}
		cur = to
	}
	return out, nil
}

// resumeHeld decides what happens to the payments the state file says a
// trap was walking when b4b stopped (b4b.Engine.Restore). Their traps went
// with the process. The standing traps are armed again at start, before
// this runs, so a payment one of them matches -- one of
// B4B_FORCE_FAILURE_BENEFICIARY_IDS -- is caught again and taken on from
// where it stopped, as the variable always did across a restart. Any other
// stays where it stopped, for good: walking it the default way would be a
// path nobody asked for, and it is still readable.
func (a *app) resumeHeld(held []b4b.Payment) {
	sort.Slice(held, func(i, j int) bool { return held[i].CreatedAt+held[i].ID < held[j].CreatedAt+held[j].ID })
	for i := range held {
		p := &held[i]
		caught, ok := a.traps.Offer(paymentEvent(p, a.beneficiaries.In(p.Env).Get(p.BeneficiaryID)))
		if !ok {
			log.Printf("b4b: payment %s was on trap %s's path when b4b stopped; traps do not outlive the process, so it stays %s", p.ID, p.Trap, p.State)
			continue
		}
		if err := a.takeAlong(caught, p.Env, p.ID, p.State, a.engine.GateFor(p.Env).Begin(nil)); err != nil {
			log.Printf("b4b: trap %s caught payment %s again after the restart and cannot take it: %v; it stays %s", caught.TrapID, p.ID, err, p.State)
			continue
		}
		log.Printf("b4b: payment %s, on trap %s's path when b4b stopped, taken on from %s by trap %s", p.ID, p.Trap, p.State, caught.TrapID)
	}
}

// trapMark is what an activity event about a payment's transition carries
// when a trap made it: the trap's id (traps.md §7), and the reason a failure
// was given. B4BAccepted is the engine's own, trap or no trap.
func trapMark(p *b4b.Payment) map[string]string {
	if p.Trap == "" || p.State == b4b.StateAccepted {
		return nil
	}
	m := map[string]string{"trap": p.Trap}
	if p.State == b4b.StateFailed && p.FailureReason != "" {
		m["reason"] = p.FailureReason
	}
	return m
}
