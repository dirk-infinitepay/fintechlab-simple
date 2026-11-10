package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/bankingcircle"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
	"github.com/webduvet/fintechlab-simple/internal/trap"
)

// Traps on Banking Circle (docs/lab-api/traps.md §9.1): one hook so far,
// payment.outgoing, a payout arriving over the bridge. A trap that catches
// one takes it along its path through the engine's own transitions
// (bankingcircle.Booking.Step); a payout no trap catches is processed the
// bank's own way, as before.

// hookPayout is the payment.outgoing hook's name.
const hookPayout = "payment.outgoing"

// payoutHook is the payment.outgoing hook. delay is PROCESSING_DELAY: a
// path's first step waits it when it names no wait of its own, as an
// untouched payout does.
func payoutHook(delay time.Duration) trap.Hook {
	processed := string(bankingcircle.NotificationOutgoingPaymentProcessed)
	rejected := string(bankingcircle.NotificationOutgoingPaymentRejected)
	missing := string(bankingcircle.NotificationMissingFunding)
	reversed := string(bankingcircle.NotificationReversed)
	return trap.Hook{
		Name:    hookPayout,
		About:   "A payout arriving over the bridge from B4B, once B4B approved it.",
		Subject: "payment_id",
		Effect:  trap.EffectPath,
		Match: []trap.Field{
			{Name: "payment_id", Type: trap.TypeString, About: "bcp_…, B4B mints it"},
			{Name: "external_ref", Type: trap.TypeString, About: "what B4B forwarded as externalRef"},
			{Name: "creditor.iban", Type: trap.TypeIBAN, About: "the receiving account's IBAN; a vIBAN is an IBAN"},
			{Name: "creditor.account_number", Type: trap.TypeString, About: "a UK account number, when the payout has no IBAN"},
			{Name: "creditor.financial_institution", Type: trap.TypeString, About: "the sort code that goes with the account number"},
			{Name: "creditor.holder", Type: trap.TypeString, About: "the beneficiary's name as sent"},
			{Name: "amount", Type: trap.TypeDecimal},
			{Name: "currency", Type: trap.TypeString},
		},
		Graph: &trap.Graph{
			Start: string(bankingcircle.NotificationOutgoingPaymentBooked),
			Next: map[string][]string{
				string(bankingcircle.NotificationOutgoingPaymentBooked): {processed, rejected, missing},
				processed: {reversed, bankingcircle.StepReturned},
			},
			Params: map[string][]string{
				rejected:                   {"reason_code", "reason_description"},
				reversed:                   {"reason"},
				bankingcircle.StepReturned: {"reason_code", "reason_description"},
			},
		},
		FirstAfter: delay,
	}
}

// newTraps is Banking Circle's trap store.
func newTraps(delay time.Duration) *trap.Store {
	return trap.NewStore("banking-circle", payoutHook(delay))
}

// payoutFields are what a payment.outgoing trap matches on. A field the
// payout does not carry is left out, so a trap naming it does not match.
func payoutFields(p *bankingcircle.Payment) map[string]string {
	return nonEmpty(map[string]string{
		"payment_id":                     p.ID,
		"external_ref":                   p.SettlementID,
		"creditor.iban":                  p.ToIBAN,
		"creditor.account_number":        p.ToAccountNumber,
		"creditor.financial_institution": p.ToFinancialInstitution,
		"creditor.holder":                p.ToHolder,
		"amount":                         p.Amount,
		"currency":                       p.Currency,
	})
}

// book books an arriving payout, as the bank does the moment one arrives,
// and offers it to the traps of the payer's environment. The first armed
// trap that matches takes it along its path; otherwise the bank processes
// it its own way. It returns the payout as booked.
func (a *app) book(p *bankingcircle.Payment) bankingcircle.Payment {
	fields := payoutFields(p)
	b := a.engine.Book(p)
	if a.traps != nil {
		if caught, ok := a.traps.Offer(trap.Event{Env: p.Env, Hook: hookPayout, Subject: p.ID, Fields: fields}); ok {
			caught.Run(a.stepper(b, caught.TrapID))
			return b.Booked()
		}
	}
	b.Process()
	return b.Booked()
}

// stepper is the apply a caught payout's path runs each step through: the
// engine's own transition, in the payout's run (a step due after a reset
// does nothing), recorded in the payments log with the trap that caused
// it. A step the books cannot follow comes back as trap.ErrDiverted, and
// the trap drops the rest of its path.
func (a *app) stepper(b *bankingcircle.Booking, trapID string) func(trap.Step) error {
	return func(st trap.Step) error {
		err := b.Step(bankingcircle.Step{
			To:                st.To,
			Reason:            st.Reason,
			ReasonCode:        st.ReasonCode,
			ReasonDescription: st.ReasonDescription,
			Trap:              trapID,
		}, func(p bankingcircle.Payment) { a.recordStep(st.To, p, trapID) })
		var d *bankingcircle.Diverted
		if errors.As(err, &d) {
			return fmt.Errorf("%w: %s", trap.ErrDiverted, d.Reason)
		}
		return err
	}
}

// recordStep logs one transition a trap's step made, as payment.state in
// the payments log, with detail.trap: the lab's own mark, so a reversal the
// lab made reads apart from one the bank came to (traps.md §7). asked is the
// state the step named; p is the payment as the step left it, and for a
// return the new incoming payment.
func (a *app) recordStep(asked string, p bankingcircle.Payment, trapID string) {
	payout, state, reason := p.ID, string(p.State), ""
	detail := map[string]string{"trap": trapID}
	switch {
	case p.Return:
		payout, state = p.ReturnOf, bankingcircle.StepReturned
		detail["return_payment_id"] = p.ID
		reason = strings.TrimSpace(p.ReturnReasonCode + " " + p.ReturnReasonDescription)
	case p.State == bankingcircle.NotificationOutgoingPaymentRejected:
		reason = strings.TrimSpace(p.RejectionReasonCode + " " + p.RejectionReasonDescription)
	case p.State == bankingcircle.NotificationReversed:
		reason = p.ReversalReason
	}
	detail["payment_id"], detail["state"], detail["reason"] = payout, state, reason
	summary := fmt.Sprintf("payout %s → %s", payout, state)
	if state != asked {
		detail["asked"] = asked
		summary += ", not " + asked + ": the books could not follow"
	}
	if reason != "" {
		summary += " (" + reason + ")"
	}
	status := activity.StatusWarn
	if state == string(bankingcircle.NotificationOutgoingPaymentProcessed) && state == asked {
		status = activity.StatusOK
	}
	a.payLog.Record(activity.Event{Env: p.Env, Op: "payment.state", Summary: summary + " — trap " + trapID,
		Status: status, Detail: nonEmpty(detail)})
}

// trapIDs are the distinct traps behind a batch's notifications, for a
// notifications log event's detail.trap; "" when none was a trap's doing.
func trapIDs(env bankingcircle.Envelope) string {
	var ids []string
	seen := map[string]bool{}
	for _, n := range env.Notifications {
		if n.Trap != "" && !seen[n.Trap] {
			seen[n.Trap] = true
			ids = append(ids, n.Trap)
		}
	}
	return strings.Join(ids, ",")
}

// --- the outcome queue, a shortcut for traps -----------------------------

// The outcome queue decides how the next payouts end, one each, in order:
// "Rejected" or "Pending". It is a shortcut (traps.md §9.1): each outcome
// is one match-all payment.outgoing trap, armed in order, so it is taken
// by the next payout no trap armed before it catches.
const (
	outcomeRejected = "Rejected"
	outcomePending  = "Pending"
	outcomeLabel    = "outcome queue: "
)

var errUnknownOutcome = errors.New("bankingcircle: unknown outcome (want Rejected or Pending)")

// outcomePath is the trap path an outcome stands for.
func outcomePath(outcome string) ([]trap.Step, bool) {
	switch outcome {
	case outcomeRejected:
		return []trap.Step{{To: string(bankingcircle.NotificationOutgoingPaymentRejected)}}, true
	case outcomePending:
		return []trap.Step{}, true
	}
	return nil, false
}

// outcomeOf names the outcome a queue trap's path stands for.
func outcomeOf(path []trap.Step) string {
	if len(path) == 0 {
		return outcomePending
	}
	return outcomeRejected
}

// outcomeQueue is the traps the shortcut armed, in order, every
// environment's. Which of them are still queued is the trap store's
// answer: armed, and not yet spent, disarmed or forgotten by a reset.
type outcomeQueue struct {
	mu  sync.Mutex
	ids []string
}

// queuedLocked is scope's outcomes still waiting, in order, forgetting the
// traps that no longer are. Caller holds q.mu.
func (q *outcomeQueue) queuedLocked(store *trap.Store, scope string) []string {
	out := []string{}
	var keep []string
	for _, id := range q.ids {
		t, ok := store.Get(id)
		if !ok || t.State != trap.StateArmed {
			continue
		}
		keep = append(keep, id)
		if tenant.InScope(scope, t.Env) {
			out = append(out, outcomeOf(t.Effect.Path))
		}
	}
	q.ids = keep
	return out
}

func (q *outcomeQueue) clear() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.ids = nil
}

type forceOutcomesReq struct {
	Outcomes []string `json:"outcomes"`
}

// forceOutcomes decides how the next outgoing payments end: each outcome
// ("Rejected" or "Pending") arms one trap that takes the next payout, in
// order, after anything already queued. It answers the whole queue.
//
// The traps are armed in the environment the call names, so they take that
// environment's next payouts, never another's (a trap's match is empty).
func (a *app) forceOutcomes(w http.ResponseWriter, r *http.Request) {
	env, ok := a.oneEnv(w, r, "an outcome")
	if !ok {
		return
	}
	var req forceOutcomesReq
	if err := httputilx.ReadJSON(r, &req); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	paths := make([][]trap.Step, len(req.Outcomes))
	for i, o := range req.Outcomes {
		path, ok := outcomePath(o)
		if !ok {
			httputilx.Error(w, 400, errUnknownOutcome.Error())
			return
		}
		paths[i] = path
	}
	a.outcomes.mu.Lock()
	defer a.outcomes.mu.Unlock()
	for i, o := range req.Outcomes {
		t, err := a.traps.Arm(trap.Spec{Env: env, Label: outcomeLabel + o, Hook: hookPayout, Match: map[string]any{}, Path: paths[i]})
		if err != nil {
			httputilx.Error(w, 500, err.Error())
			return
		}
		a.outcomes.ids = append(a.outcomes.ids, t.ID)
	}
	queued := a.outcomes.queuedLocked(a.traps, env)
	if len(req.Outcomes) > 0 {
		log.Printf("banking-circle: next %d outgoing payment(s) of environment %s will end %v", len(queued), env, queued)
	}
	httputilx.WriteJSON(w, 200, map[string]any{"queued": queued})
}

// forcedOutcomes answers the outcomes still waiting for a payout of the
// environment the call names ("*": every one's, in the order armed).
func (a *app) forcedOutcomes(w http.ResponseWriter, r *http.Request) {
	a.outcomes.mu.Lock()
	defer a.outcomes.mu.Unlock()
	httputilx.WriteJSON(w, 200, map[string]any{"queued": a.outcomes.queuedLocked(a.traps, scopeOf(r))})
}
