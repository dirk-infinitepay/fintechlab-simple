package bankingcircle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/money"
)

// NotificationType is Banking Circle's real, closed webhook event-type enum
// (docs/ARCHITECTURE-vendor-corrections.md, section 3 — all 13 real wire
// values). This lab's Engine only ever drives the outgoing-payment subset:
// OutgoingPaymentBooked -> OutgoingPaymentProcessed | OutgoingPaymentRejected
// | MissingFunding, plus the manual Reversed test hook. The remaining
// constants exist purely for wire-shape completeness — no lifecycle in this
// lab ever fires them.
type NotificationType string

const (
	NotificationIncomingPaymentProcessed             NotificationType = "IncomingPaymentProcessed"
	NotificationIncomingPaymentBooked                NotificationType = "IncomingPaymentBooked"
	NotificationOutgoingPaymentProcessed             NotificationType = "OutgoingPaymentProcessed"
	NotificationOutgoingPaymentBooked                NotificationType = "OutgoingPaymentBooked"
	NotificationOutgoingPaymentRejected              NotificationType = "OutgoingPaymentRejected"
	NotificationMissingFunding                       NotificationType = "MissingFunding"
	NotificationReversed                             NotificationType = "Reversed"
	NotificationPaymentRouting                       NotificationType = "PaymentRouting"
	NotificationPaymentStatus                        NotificationType = "PaymentStatus"
	NotificationOutgoingDirectDebitPendingProcessing NotificationType = "OutgoingDirectDebitPendingProcessing"
	NotificationAccountHolderVerification            NotificationType = "AccountHolderVerification"
	NotificationCaseEvents                           NotificationType = "CaseEvents"
	NotificationAgencyBankingWhitelistResult         NotificationType = "AgencyBankingWhitelistResult"
)

// Payment is a single payment, outgoing or incoming, from creation through
// its terminal notification. State holds the last NotificationType the
// engine fired for it — real Banking Circle has no payment "status" field
// of its own, only the notification stream, so this lab tracks the same
// thing its webhook reports instead of inventing a parallel vocabulary.
// Both flows share this one struct and Engine deliberately (Create for
// outgoing, CreditIncoming for incoming): forking a parallel type per
// direction would duplicate the notify/onTransition plumbing for no
// benefit — real Banking Circle has no such split either.
type Payment struct {
	ID            string `json:"id"`
	SettlementID  string `json:"settlementId,omitempty"`
	FromAccountID string `json:"fromAccountId"`
	ToAccountID   string `json:"toAccountId,omitempty"`
	ToIBAN        string `json:"toIban,omitempty"`
	// ToHolder is the beneficiary's name as the sender knows it. Carried so
	// the receiving bank can put a name on the account it opens — an
	// account identified only by its IBAN is reconcilable but not readable.
	ToHolder string `json:"toHolder,omitempty"`
	// ToAccountNumber and ToFinancialInstitution name the beneficiary's
	// account when it has no IBAN — a UK account number and sort code, as
	// most of a payfac's merchants are paid. The receiving bank is credited
	// by whichever the payout carried.
	ToAccountNumber        string `json:"toAccountNumber,omitempty"`
	ToFinancialInstitution string `json:"toFinancialInstitution,omitempty"`
	Amount                 string `json:"amount"`
	Currency               string `json:"currency"`
	Reference              string `json:"reference,omitempty"`
	// ReferenceNumber is Banking Circle's own reference for the payment,
	// the reconciliation report's paymentReferenceNumber. The bank assigns
	// it; it is never anything the client sent.
	ReferenceNumber string `json:"paymentReferenceNumber,omitempty"`
	// ProcessedAt, ReversedAt and ReversalReason keep what a later state
	// would otherwise overwrite: a reversed payment still reports the
	// booking it was processed with, beside the reversal.
	ProcessedAt    string `json:"processedAt,omitempty"`
	ReversedAt     string `json:"reversedAt,omitempty"`
	ReversalReason string `json:"reversalReason,omitempty"`
	// RejectionReasonCode and RejectionReasonDescription are why a payout
	// was rejected, when a trap's step named a reason (docs/lab-api/traps.md
	// §9.1). The rejection report's statusReason carries them; without
	// them it carries the bank's fixed text.
	RejectionReasonCode        string `json:"rejectionReasonCode,omitempty"`
	RejectionReasonDescription string `json:"rejectionReasonDescription,omitempty"`

	// A returned payout. The original keeps its state (a return is not a
	// status in Banking Circle) and only records ReturnedBy; the money comes
	// back as a new incoming payment carrying the rest.
	ReturnedBy string `json:"returnedBy,omitempty"`
	// Return is the wire `return` flag: true on the incoming return payment.
	Return bool `json:"return,omitempty"`
	// ReturnOf is the payout this payment returns. Lab bookkeeping: Banking
	// Circle links the two only through ReturnedReference.
	ReturnOf string `json:"returnOf,omitempty"`
	// ReturnedReference is the returned payout's ReferenceNumber, which the
	// return's remittance information quotes back.
	ReturnedReference       string `json:"returnedReference,omitempty"`
	ReturnReasonCode        string `json:"returnReasonCode,omitempty"`
	ReturnReasonDescription string `json:"returnReasonDescription,omitempty"`
	// Remittance is remittance information lines 1-4, as sent.
	Remittance []string `json:"remittanceInformation,omitempty"`

	State     NotificationType `json:"state"`
	CreatedAt string           `json:"createdAt"`
	UpdatedAt string           `json:"updatedAt"`

	// epoch is the run the payment was made in (Run.Current). A reset ends
	// that run: the payment is cleared, and every step still to come for it
	// — its processing, its notifications, its credit at the beneficiary's
	// bank — is dropped.
	epoch uint64
	// trap is the lab's trap that made this state, on the copy handed to
	// onTransition for a trap's step and on a return payment a trap's step
	// made. Never on the wire: see Trap.
	trap string
}

// Epoch is the run the payment was made in, for work done on its behalf
// outside the engine (Run.Do, Dispatcher.EnqueueFrom).
func (p *Payment) Epoch() uint64 { return p.epoch }

// Trap is the id of the trap whose step made this state, or "" for a state
// the bank came to on its own (docs/lab-api/traps.md §7). It is for the
// lab's own logs; nothing a platform reads carries it.
func (p *Payment) Trap() string { return p.trap }

// ErrPaymentNotFound is returned by Get and Reverse for an unknown payment ID.
var ErrPaymentNotFound = errors.New("bankingcircle: payment not found")

// ErrInvalidState is returned by Reverse and Return when the payment is not
// in the OutgoingPaymentProcessed state.
var ErrInvalidState = errors.New("bankingcircle: payment is not in OutgoingPaymentProcessed state")

// ErrAlreadyReturned is returned by Reverse and Return for a payout that
// has already come back.
var ErrAlreadyReturned = errors.New("bankingcircle: payment has already been returned")

// Engine owns the payment notification lifecycle, outgoing and incoming
// alike. It is HTTP-free and fully unit-testable: onTransition (nil in
// tests) is how the caller wires in webhook delivery without the engine
// knowing anything about HTTP.
type Engine struct {
	mu           sync.Mutex
	ledger       *Ledger
	payments     map[string]*Payment
	delay        time.Duration // PROCESSING_DELAY, simulates real bank latency
	onTransition func(*Payment)
	refSeq       int64 // last ReferenceNumber handed out

	// run is the epoch a reset moves; the engine owns it because the
	// payments are where in-flight work starts. NewBank shares it with the
	// dispatcher.
	run *Run
	wg  sync.WaitGroup // payments still walking their phases
}

// NewEngine builds an Engine backed by ledger. onTransition fires (with a
// value copy, safe to read without further locking) on every notification
// the engine fires, including the initial OutgoingPaymentBooked one; pass
// nil to disable.
func NewEngine(ledger *Ledger, delay time.Duration, onTransition func(*Payment)) *Engine {
	return &Engine{
		ledger:       ledger,
		payments:     map[string]*Payment{},
		delay:        delay,
		onTransition: onTransition,
		run:          NewRun(),
	}
}

// Run is the engine's run: what a reset ends, and what work on a payment's
// behalf checks before each step.
func (e *Engine) Run() *Run { return e.run }

// Wait blocks until every payment still walking its phases has finished or
// been dropped. For tests.
func (e *Engine) Wait() { e.wg.Wait() }

// Create books p (Book) and ends it the bank's own way (Booking.Process):
// OutgoingPaymentBooked at once, then, after `delay` and in a goroutine so
// the caller is never blocked on ledger settlement, exactly one of three
// notifications: OutgoingPaymentProcessed (the ledger move succeeded),
// MissingFunding (it failed for insufficient funds on FromAccountID), or
// OutgoingPaymentRejected (it failed for any other reason, e.g. an unknown
// account or the same account).
func (e *Engine) Create(p *Payment) { e.Book(p).Process() }

// Booking is a payout the bank has just booked, waiting to learn what
// happens to it next. Process ends it the bank's own way; a trap's path
// (docs/lab-api/traps.md §9.1) takes it along with Step instead. A booking
// that is given neither stays OutgoingPaymentBooked for good.
type Booking struct {
	e      *Engine
	p      *Payment
	epoch  uint64
	booked Payment
}

// Book records p and fires OutgoingPaymentBooked at once: the bank books a
// payout the moment it arrives, whatever becomes of it next.
func (e *Engine) Book(p *Payment) *Booking {
	now := labclock.Now().Format(time.RFC3339)
	e.mu.Lock()
	p.epoch = e.run.Current()
	p.State = NotificationOutgoingPaymentBooked
	p.CreatedAt = now
	p.UpdatedAt = now
	p.ReferenceNumber = e.nextReferenceNumber()
	e.payments[p.ID] = p
	cp := *p
	e.mu.Unlock()
	e.notify(cp)
	return &Booking{e: e, p: p, epoch: cp.epoch, booked: cp}
}

// Booked is the payout as it was booked, whatever has happened to it since.
func (b *Booking) Booked() Payment { return b.booked }

// Process ends the booked payout the bank's own way, after the engine's
// delay, in the background (see Create).
func (b *Booking) Process() {
	b.e.wg.Add(1)
	go b.e.process(b.p)
}

// process ends an outgoing payment after the delay. A reset during the
// delay, or before the move, drops it: no money moves, no state changes and
// nothing is notified for a payment the reset cleared.
func (e *Engine) process(p *Payment) {
	defer e.wg.Done()
	started := p.epoch
	if !e.run.Sleep(started, e.delay) {
		return
	}
	e.run.Do(started, func(context.Context) {
		// The move and the state change are one step under e.mu, the lock a
		// reset clears the payments under: a reset sees both or neither.
		e.mu.Lock()
		e.settleLocked(p)
		cp := *p
		e.mu.Unlock()
		e.notify(cp)
	})
}

// settleLocked is the bank processing a booked payout: the ledger move, and
// the state the payout ends in by the move's outcome. It returns the move's
// error, nil when the money moved. Caller holds e.mu.
func (e *Engine) settleLocked(p *Payment) error {
	cents, err := money.Parse(p.Amount)
	if err == nil {
		_, _, err = e.ledger.Move(p.FromAccountID, p.ToAccountID, cents)
	}
	now := labclock.Now().Format(time.RFC3339)
	switch {
	case err == nil:
		p.State = NotificationOutgoingPaymentProcessed
		p.ProcessedAt = now
	case errors.Is(err, ErrInsufficientFunds):
		p.State = NotificationMissingFunding
	default:
		p.State = NotificationOutgoingPaymentRejected
	}
	p.UpdatedAt = now
	return err
}

// StepReturned is the step a returned payout takes. A return is not a
// state in Banking Circle (the payout stays OutgoingPaymentProcessed and the
// money comes back as a payment of its own, Engine.Return), so it is not a
// NotificationType either.
const StepReturned = "Returned"

// Step is one state a trap's path takes a booked payout to
// (docs/lab-api/traps.md §9.1): OutgoingPaymentProcessed,
// OutgoingPaymentRejected or MissingFunding from booked, then Reversed or
// StepReturned from processed, with the parameters that state takes.
type Step struct {
	To string
	// Reason is a reversal's reason.
	Reason string
	// ReasonCode and ReasonDescription are a rejection's or a return's.
	ReasonCode        string
	ReasonDescription string
	// Trap is the trap that asked for the step. Every notification the
	// step fires carries it (Payment.Trap); the wire never does.
	Trap string
}

// ErrReset is a step for a payout booked in a run a reset has since ended:
// dropped, like every other piece of work from that run.
var ErrReset = errors.New("bankingcircle: the run this payout was booked in has been reset")

// Diverted is a step the books could not follow (docs/lab-api/traps.md
// §4.1): processing with too little on the safeguarding account, or
// reversing a payout that is no longer processed. The bank did what its own
// rules say instead, if anything, and Reason says what and why.
type Diverted struct {
	To     string
	Reason string
}

func (d *Diverted) Error() string {
	return "bankingcircle: " + d.To + " diverted: " + d.Reason
}

// Step takes the booked payout to s.To through the bank's own code: the
// same ledger move, notifications, report rows and status read as a
// transition no trap touched. applied, when not nil, is called with the
// payment as the step left it (for a return, the new incoming payment).
//
// The step goes through the run like every other piece of work on a
// payment's behalf (Run.Do): if a reset has ended the run the payout was
// booked in it does nothing and answers ErrReset, and a reset cannot land
// halfway through it. applied runs inside that guard, so what it records
// is cleared by a reset or never written.
//
// The books come first: a step they cannot follow answers a *Diverted,
// after doing what the bank does instead (processing with the safeguarding
// account short ends the payout MissingFunding, as it would untouched).
func (b *Booking) Step(s Step, applied func(Payment)) error {
	var err error
	if !b.e.run.Do(b.epoch, func(context.Context) { err = b.e.step(b.p, s, applied) }) {
		return ErrReset
	}
	return err
}

func (e *Engine) step(p *Payment, s Step, applied func(Payment)) error {
	e.mu.Lock()
	var (
		notify   []Payment // in the order they fire
		result   Payment
		incoming *Payment // a return payment, to process after its booking
		diverted error
	)
	switch s.To {
	case string(NotificationOutgoingPaymentProcessed), string(NotificationOutgoingPaymentRejected), string(NotificationMissingFunding):
		if p.State != NotificationOutgoingPaymentBooked {
			reason := fmt.Sprintf("the payout is %s, no longer booked", p.State)
			e.mu.Unlock()
			return &Diverted{To: s.To, Reason: reason}
		}
		switch NotificationType(s.To) {
		case NotificationOutgoingPaymentProcessed:
			short := e.shortfallLocked(p)
			if err := e.settleLocked(p); err != nil {
				reason := fmt.Sprintf("the ledger refused the move (%v)", err)
				if errors.Is(err, ErrInsufficientFunds) && short != "" {
					reason = short
				}
				diverted = &Diverted{To: s.To, Reason: reason + "; " + string(p.State) + " instead"}
			}
		case NotificationOutgoingPaymentRejected:
			e.endLocked(p, NotificationOutgoingPaymentRejected)
			p.RejectionReasonCode, p.RejectionReasonDescription = s.ReasonCode, s.ReasonDescription
		default:
			e.endLocked(p, NotificationMissingFunding)
		}
		result = *p
		result.trap = s.Trap
		notify = []Payment{result}
	case string(NotificationReversed):
		cp, err := e.reverseLocked(p, s.Reason)
		if err != nil {
			reason := refusal(p, err)
			e.mu.Unlock()
			return &Diverted{To: s.To, Reason: reason}
		}
		cp.trap = s.Trap
		booking := cp
		booking.State = NotificationOutgoingPaymentBooked
		result, notify = cp, []Payment{booking, cp}
	case StepReturned:
		ret, err := e.returnLocked(p, s.ReasonCode, s.ReasonDescription, s.Trap)
		if err != nil {
			reason := refusal(p, err)
			e.mu.Unlock()
			return &Diverted{To: s.To, Reason: reason}
		}
		result, incoming = *ret, ret
		notify = []Payment{result}
	default:
		e.mu.Unlock()
		return fmt.Errorf("bankingcircle: a payout has no step to %q", s.To)
	}
	e.mu.Unlock()
	for _, n := range notify {
		e.notify(n)
	}
	if incoming != nil {
		e.wg.Add(1)
		go e.processIncoming(incoming)
	}
	if applied != nil {
		applied(result)
	}
	return diverted
}

// endLocked ends a booked payout in state without moving money: a
// rejection, or missing funding. Caller holds e.mu.
func (e *Engine) endLocked(p *Payment, state NotificationType) {
	p.State = state
	p.UpdatedAt = labclock.Now().Format(time.RFC3339)
}

// shortfallLocked says how far the debtor account is short of the payout,
// or "" when it is not (or cannot be read). Caller holds e.mu.
func (e *Engine) shortfallLocked(p *Payment) string {
	cents, err := money.Parse(p.Amount)
	if err != nil {
		return ""
	}
	acc, err := e.ledger.Get(p.FromAccountID)
	if err != nil {
		return ""
	}
	if have, _ := money.Parse(acc.Balance); have >= cents {
		return ""
	}
	return fmt.Sprintf("the safeguarding account %s holds %s %s, short of %s", acc.VIBAN, acc.Balance, acc.Currency, p.Amount)
}

// refusal is why the bank would not reverse or return p, in words. Caller
// holds e.mu.
func refusal(p *Payment, err error) string {
	switch {
	case errors.Is(err, ErrInvalidState):
		return fmt.Sprintf("the payout is %s, not OutgoingPaymentProcessed", p.State)
	case errors.Is(err, ErrAlreadyReturned):
		return "the payout has already been returned, as " + p.ReturnedBy
	}
	return fmt.Sprintf("the ledger refused to move the money back (%v)", err)
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

// Reverse manually transitions an OutgoingPaymentProcessed payment to
// Reversed, reversing the ledger credit (toAccount -> fromAccount). It
// fires two notifications: OutgoingPaymentBooked for the reversal booking —
// Banking Circle's payload examples: "In a Reversal-scenario, two
// OutgoingPaymentBooked-webhooks will be triggered", the second "when it's
// later reversed", with the same account and transaction reference — and
// then Reversed, the status change. The docs do not say which of the two
// comes first. Lab-only test hook, reachable via
// POST /internal/payments/{id}/reverse — real reversals are bank-driven and
// asynchronous; this lets the harness exercise that path deterministically,
// the same purpose the old Return() served. reason becomes the reversal
// booking's statusReasonDescription on the reconciliation report.
func (e *Engine) Reverse(id, reason string) (*Payment, error) {
	e.mu.Lock()
	p, ok := e.payments[id]
	if !ok {
		e.mu.Unlock()
		return nil, ErrPaymentNotFound
	}
	cp, err := e.reverseLocked(p, reason)
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	booking := cp
	booking.State = NotificationOutgoingPaymentBooked
	e.notify(booking)
	e.notify(cp)
	return &cp, nil
}

// reverseLocked moves a processed payout's money back and marks it
// Reversed, returning a copy for the two notifications (the reversal's
// booking, then Reversed) the caller fires once e.mu is released. Caller
// holds e.mu.
func (e *Engine) reverseLocked(p *Payment, reason string) (Payment, error) {
	if p.State != NotificationOutgoingPaymentProcessed {
		return Payment{}, ErrInvalidState
	}
	if p.ReturnedBy != "" {
		return Payment{}, ErrAlreadyReturned
	}
	cents, err := money.Parse(p.Amount)
	if err != nil {
		return Payment{}, err
	}
	if _, _, err := e.ledger.Move(p.ToAccountID, p.FromAccountID, cents); err != nil {
		return Payment{}, err
	}
	p.State = NotificationReversed
	p.ReversalReason = reason
	p.ReversedAt = labclock.Now().Format(time.RFC3339)
	p.UpdatedAt = p.ReversedAt
	return *p, nil
}

// notify hands a transition to the caller, unless a reset has ended the run
// the payment was made in. The dispatcher checks again under its own lock
// (EnqueueFrom), which is what closes the gap between this check and the
// notification being queued.
func (e *Engine) notify(p Payment) {
	if e.onTransition != nil && !e.run.Moved(p.epoch) {
		e.onTransition(&p)
	}
}

// CreditIncoming simulates Worldline's lump sum landing in the
// safeguarding account (docs/ARCHITECTURE-phase3-corrections.md section
// 1): parses amount, credits the ledger, and immediately fires
// IncomingPaymentBooked, then — after the same `delay` the outgoing engine
// uses, in a goroutine, so the caller is never blocked — flips to
// IncomingPaymentProcessed and fires again. Both notifications go through
// the same notify/onTransition plumbing Create uses; there is no separate
// callback. Unlike Create, the ledger credit happens synchronously before
// the initial notification: Credit's only failure mode is an unknown
// toAccountID, which callers must resolve (or auto-vivify) before calling
// this, so there is no MissingFunding-equivalent second outcome to report.
func (e *Engine) CreditIncoming(toAccountID, currency, amount, reference string) (*Payment, error) {
	cents, err := money.Parse(amount)
	if err != nil {
		return nil, err
	}

	// The sender's reference reaches the beneficiary as remittance
	// information, the way Banking Circle shows it.
	now := labclock.Now().Format(time.RFC3339)
	p := &Payment{
		ID:            "bcp_" + randomHex(6),
		FromAccountID: "external_worldline",
		ToAccountID:   toAccountID,
		Amount:        amount,
		Currency:      currency,
		Reference:     reference,
		State:         NotificationIncomingPaymentBooked,
		Remittance:    nonEmpty(reference),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	// The credit and the record are one step under e.mu, so a reset cannot
	// land between them and leave money on the account with no payment
	// that put it there.
	e.mu.Lock()
	if _, err := e.ledger.Credit(toAccountID, cents); err != nil {
		e.mu.Unlock()
		return nil, err
	}
	p.epoch = e.run.Current()
	p.ReferenceNumber = e.nextReferenceNumber()
	e.payments[p.ID] = p
	cp := *p
	e.mu.Unlock()
	e.notify(cp)

	e.wg.Add(1)
	go e.processIncoming(p)

	return &cp, nil
}

// processIncoming moves a booked incoming payment to
// IncomingPaymentProcessed after the engine's delay, unless a reset has
// cleared it in the meantime.
func (e *Engine) processIncoming(p *Payment) {
	defer e.wg.Done()
	started := p.epoch
	if !e.run.Sleep(started, e.delay) {
		return
	}
	e.run.Do(started, func(context.Context) {
		e.mu.Lock()
		p.State = NotificationIncomingPaymentProcessed
		p.UpdatedAt = labclock.Now().Format(time.RFC3339)
		p.ProcessedAt = p.UpdatedAt
		cp := *p
		e.mu.Unlock()
		e.notify(cp)
	})
}

// nonEmpty is lines without the empty ones, nil when none are left.
func nonEmpty(lines ...string) []string {
	var out []string
	for _, line := range lines {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// returnRemittance is the remittance information of a return payment, as
// in Banking Circle's IncomingPaymentProcessed return example: "RETURN OF
// PAYMENT", then the returned payment's reference. The docs add that the
// remittance often carries the reason too (e.g. account closed), so a
// reason, when given, is the third line.
func returnRemittance(returnedReference, reasonCode, reasonDescription string) []string {
	lines := []string{"RETURN OF PAYMENT", returnedReference}
	if reason := strings.TrimSpace(reasonCode + " " + reasonDescription); reason != "" {
		lines = append(lines, reason)
	}
	return lines
}

// Return simulates the beneficiary's bank sending a processed payout back.
// Lab-only test hook, reachable via POST /internal/payments/{id}/return:
// real returns come from the beneficiary's bank, days later.
//
// It follows Banking Circle's model of a return, which is not a status:
// the payout stays OutgoingPaymentProcessed, and the money arrives as a
// new incoming payment of its own — own paymentId, own reference,
// `return: true` — that is booked and then processed like any other
// incoming payment (IncomingPaymentBooked, IncomingPaymentProcessed). The
// money moves back from the beneficiary's account to the account the payout
// left; the beneficiary bank service the payout was forwarded to is not
// debited, since the lab has no path back from it.
func (e *Engine) Return(id, reasonCode, reasonDescription string) (*Payment, error) {
	e.mu.Lock()
	orig, ok := e.payments[id]
	if !ok {
		e.mu.Unlock()
		return nil, ErrPaymentNotFound
	}
	p, err := e.returnLocked(orig, reasonCode, reasonDescription, "")
	if err != nil {
		e.mu.Unlock()
		return nil, err
	}
	cp := *p
	e.mu.Unlock()
	e.notify(cp)

	e.wg.Add(1)
	go e.processIncoming(p)

	return &cp, nil
}

// returnLocked moves a processed payout's money back as a new incoming
// return payment, booked, and records it; the caller notifies its booking
// once e.mu is released and then processes it (processIncoming). trap,
// when set, is the trap whose step made the return: the return payment is
// wholly its doing, so both its notifications carry it. Caller holds e.mu.
func (e *Engine) returnLocked(orig *Payment, reasonCode, reasonDescription, trap string) (*Payment, error) {
	if orig.State != NotificationOutgoingPaymentProcessed {
		return nil, ErrInvalidState
	}
	if orig.ReturnedBy != "" {
		return nil, ErrAlreadyReturned
	}
	cents, err := money.Parse(orig.Amount)
	if err != nil {
		return nil, err
	}
	if _, _, err := e.ledger.Move(orig.ToAccountID, orig.FromAccountID, cents); err != nil {
		return nil, err
	}
	now := labclock.Now().Format(time.RFC3339)
	p := &Payment{
		epoch:                   e.run.Current(),
		trap:                    trap,
		ID:                      "bcp_" + randomHex(6),
		FromAccountID:           orig.ToAccountID,
		ToAccountID:             orig.FromAccountID,
		Amount:                  orig.Amount,
		Currency:                orig.Currency,
		State:                   NotificationIncomingPaymentBooked,
		CreatedAt:               now,
		UpdatedAt:               now,
		ReferenceNumber:         e.nextReferenceNumber(),
		Return:                  true,
		ReturnOf:                orig.ID,
		ReturnedReference:       orig.ReferenceNumber,
		ReturnReasonCode:        reasonCode,
		ReturnReasonDescription: reasonDescription,
		Remittance:              returnRemittance(orig.ReferenceNumber, reasonCode, reasonDescription),
	}
	orig.ReturnedBy = p.ID
	e.payments[p.ID] = p
	return p, nil
}

// nextReferenceNumber hands out Banking Circle's reference for a new
// payment. The shape follows the reference docs' examples (010F10xxxx001007,
// 010F101010110001): sixteen characters under a fixed 010F10 prefix. The
// bank's actual numbering scheme is not documented, so a client must treat
// it as opaque. Callers hold e.mu.
func (e *Engine) nextReferenceNumber() string {
	e.refSeq++
	return fmt.Sprintf("010F10%010d", e.refSeq)
}

// randomHex generates a random hex id, local to this package — same tiny
// per-binary/per-package helper shape as cmd/bankingcircle's shortID, not
// shared across packages by convention in this repo.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
