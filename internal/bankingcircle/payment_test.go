package bankingcircle

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/labclock"
)

func TestEngineCreateHappyPath(t *testing.T) {
	ledger := NewLedger()
	ledger.mustSeed("bc_acc_test_funded", "VBTESTFUNDED0000001", "Test Funded", "EUR", 50_000_000)
	ledger.mustSeed("bc_acc_test_merchant", "VBTESTMERCHANT00001", "Test Merchant", "EUR", 0)
	events := make(chan Payment, 8)
	engine := NewEngine(ledger, 10*time.Millisecond, func(p *Payment) { events <- *p })

	p := &Payment{
		ID:            "bcp_test1",
		FromAccountID: "bc_acc_test_funded",
		ToAccountID:   "bc_acc_test_merchant",
		Amount:        "100.00",
		Currency:      "EUR",
	}
	engine.Create(p)

	for _, want := range []NotificationType{NotificationOutgoingPaymentBooked, NotificationOutgoingPaymentProcessed} {
		select {
		case got := <-events:
			if got.State != want {
				t.Fatalf("notification = %s, want %s", got.State, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s", want)
		}
	}

	got, err := engine.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != NotificationOutgoingPaymentProcessed {
		t.Fatalf("final state = %s", got.State)
	}
	merchant, _ := ledger.Get("bc_acc_test_merchant")
	if merchant.Balance != "100.00" {
		t.Fatalf("merchant balance = %s", merchant.Balance)
	}
}

func TestEngineCreateMissingFunding(t *testing.T) {
	ledger := NewLedger()
	events := make(chan Payment, 8)
	engine := NewEngine(ledger, time.Millisecond, func(p *Payment) { events <- *p })

	// The EUR safeguarding account opens at zero -- the whole point of the
	// safeguarding-account model -- so paying out of it before any
	// Worldline lump sum has landed is an insufficient-funds ledger error
	// -> MissingFunding, not a generic rejection.
	p := &Payment{
		ID:            "bcp_test2",
		FromAccountID: SGAAccountEUR,
		ToAccountID:   SGAAccountGBP,
		Amount:        "50.00",
		Currency:      "EUR",
	}
	engine.Create(p)

	for _, want := range []NotificationType{NotificationOutgoingPaymentBooked, NotificationMissingFunding} {
		select {
		case got := <-events:
			if got.State != want {
				t.Fatalf("notification = %s, want %s", got.State, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s", want)
		}
	}

	got, err := engine.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != NotificationMissingFunding {
		t.Fatalf("final state = %s", got.State)
	}
	sga, _ := ledger.Get(SGAAccountEUR)
	if sga.Balance != "0.00" {
		t.Fatalf("balance mutated on missing-funding payment: %s", sga.Balance)
	}
}

func TestEngineCreateRejectedOnUnknownAccount(t *testing.T) {
	ledger := NewLedger()
	events := make(chan Payment, 8)
	engine := NewEngine(ledger, time.Millisecond, func(p *Payment) { events <- *p })

	p := &Payment{
		ID:            "bcp_test3",
		FromAccountID: SGAAccountEUR,
		ToAccountID:   "bc_acc_does_not_exist",
		Amount:        "10.00",
		Currency:      "EUR",
	}
	engine.Create(p)

	for _, want := range []NotificationType{NotificationOutgoingPaymentBooked, NotificationOutgoingPaymentRejected} {
		select {
		case got := <-events:
			if got.State != want {
				t.Fatalf("notification = %s, want %s", got.State, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s", want)
		}
	}

	got, err := engine.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != NotificationOutgoingPaymentRejected {
		t.Fatalf("final state = %s", got.State)
	}
}

func TestEngineCreateRejectedOnSameAccount(t *testing.T) {
	ledger := NewLedger()
	events := make(chan Payment, 8)
	engine := NewEngine(ledger, time.Millisecond, func(p *Payment) { events <- *p })

	p := &Payment{
		ID:            "bcp_test4",
		FromAccountID: SGAAccountEUR,
		ToAccountID:   SGAAccountEUR,
		Amount:        "10.00",
		Currency:      "EUR",
	}
	engine.Create(p)

	for _, want := range []NotificationType{NotificationOutgoingPaymentBooked, NotificationOutgoingPaymentRejected} {
		select {
		case got := <-events:
			if got.State != want {
				t.Fatalf("notification = %s, want %s", got.State, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s", want)
		}
	}
}

func TestEngineReverseOnlyFromProcessed(t *testing.T) {
	ledger := NewLedger()
	ledger.mustSeed("bc_acc_test_funded", "VBTESTFUNDED0000002", "Test Funded", "EUR", 50_000_000)
	ledger.mustSeed("bc_acc_test_merchant", "VBTESTMERCHANT00002", "Test Merchant", "EUR", 0)
	events := make(chan Payment, 8)
	engine := NewEngine(ledger, time.Millisecond, func(p *Payment) { events <- *p })

	p := &Payment{
		ID:            "bcp_test5",
		FromAccountID: "bc_acc_test_funded",
		ToAccountID:   "bc_acc_test_merchant",
		Amount:        "75.00",
		Currency:      "EUR",
	}

	// Reverse before Create even runs must fail: unknown payment ID.
	if _, err := engine.Reverse(p.ID, ""); err != ErrPaymentNotFound {
		t.Fatalf("err = %v, want ErrPaymentNotFound", err)
	}

	engine.Create(p)
	// Drain the Booked notification; the payment is not yet Processed, so
	// reversing now must fail with ErrInvalidState.
	<-events
	if _, err := engine.Reverse(p.ID, ""); err != ErrInvalidState {
		t.Fatalf("err = %v, want ErrInvalidState", err)
	}

	// Wait for settlement to land on Processed.
	select {
	case got := <-events:
		if got.State != NotificationOutgoingPaymentProcessed {
			t.Fatalf("state = %s, want Processed", got.State)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Processed")
	}

	reversed, err := engine.Reverse(p.ID, "test hook")
	if err != nil {
		t.Fatalf("Reverse: %v", err)
	}
	if reversed.State != NotificationReversed {
		t.Fatalf("state after Reverse = %s", reversed.State)
	}
	if reversed.ReversalReason != "test hook" {
		t.Fatalf("reversal reason = %q, want the reason recorded", reversed.ReversalReason)
	}
	if reversed.Reference != "" {
		t.Fatalf("reference = %q, want it untouched by the reversal", reversed.Reference)
	}
	if reversed.ProcessedAt == "" || reversed.ReversedAt == "" {
		t.Fatalf("processedAt = %q, reversedAt = %q; want both kept", reversed.ProcessedAt, reversed.ReversedAt)
	}

	// The reversal is booked (a second OutgoingPaymentBooked, same payment
	// and reference) and then reported as Reversed.
	for _, want := range []NotificationType{NotificationOutgoingPaymentBooked, NotificationReversed} {
		select {
		case got := <-events:
			if got.State != want || got.ID != p.ID || got.ReferenceNumber != reversed.ReferenceNumber {
				t.Fatalf("notification = %s for %s (%s), want %s for %s (%s)",
					got.State, got.ID, got.ReferenceNumber, want, p.ID, reversed.ReferenceNumber)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s notification", want)
		}
	}

	merchant, _ := ledger.Get("bc_acc_test_merchant")
	if merchant.Balance != "0.00" {
		t.Fatalf("merchant balance after reversal = %s, want 0.00", merchant.Balance)
	}

	// Reversing twice must fail: no longer Processed.
	if _, err := engine.Reverse(p.ID, ""); err != ErrInvalidState {
		t.Fatalf("second Reverse err = %v, want ErrInvalidState", err)
	}
}

func TestEngineGetUnknownPayment(t *testing.T) {
	engine := NewEngine(NewLedger(), time.Millisecond, nil)
	if _, err := engine.Get("nope"); err != ErrPaymentNotFound {
		t.Fatalf("err = %v, want ErrPaymentNotFound", err)
	}
}

func TestEngineCreditIncomingBookedThenProcessed(t *testing.T) {
	ledger := NewLedger()
	events := make(chan Payment, 8)
	engine := NewEngine(ledger, 10*time.Millisecond, func(p *Payment) { events <- *p })

	p, err := engine.CreditIncoming(SGAAccountEUR, "EUR", "12500.00", "worldline-lump-sum-1")
	if err != nil {
		t.Fatalf("CreditIncoming: %v", err)
	}
	if p.State != NotificationIncomingPaymentBooked {
		t.Fatalf("initial state = %s, want Booked", p.State)
	}
	if p.FromAccountID != "external_worldline" || p.ToAccountID != SGAAccountEUR {
		t.Fatalf("payment parties = %+v", p)
	}
	if p.Reference != "worldline-lump-sum-1" {
		t.Fatalf("reference = %q", p.Reference)
	}
	if !strings.HasPrefix(p.ID, "bcp_") {
		t.Fatalf("id = %s, want bcp_ prefix", p.ID)
	}

	// the ledger credit is synchronous -- it has already happened by the
	// time the Booked payment is returned.
	acc, err := ledger.Get(SGAAccountEUR)
	if err != nil {
		t.Fatal(err)
	}
	if acc.Balance != "12500.00" {
		t.Fatalf("sga balance = %s, want 12500.00", acc.Balance)
	}

	for _, want := range []NotificationType{NotificationIncomingPaymentBooked, NotificationIncomingPaymentProcessed} {
		select {
		case got := <-events:
			if got.State != want {
				t.Fatalf("notification = %s, want %s", got.State, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s", want)
		}
	}

	got, err := engine.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != NotificationIncomingPaymentProcessed {
		t.Fatalf("final state = %s", got.State)
	}
	// the Processed transition is notification-only -- the balance does not
	// move again.
	acc, _ = ledger.Get(SGAAccountEUR)
	if acc.Balance != "12500.00" {
		t.Fatalf("sga balance after Processed = %s, want unchanged 12500.00", acc.Balance)
	}
}

func TestEngineCreditIncomingUnknownAccount(t *testing.T) {
	engine := NewEngine(NewLedger(), time.Millisecond, nil)
	if _, err := engine.CreditIncoming("bc_acc_nope", "EUR", "10.00", "ref"); err != ErrAccountNotFound {
		t.Fatalf("err = %v, want ErrAccountNotFound", err)
	}
}

func TestEngineCreditIncomingInvalidAmount(t *testing.T) {
	engine := NewEngine(NewLedger(), time.Millisecond, nil)
	if _, err := engine.CreditIncoming(SGAAccountEUR, "EUR", "not-a-number", "ref"); err == nil {
		t.Fatal("expected error for invalid amount")
	}
}

// stepRecorder keeps every transition the engine hands onTransition.
type stepRecorder struct {
	mu   sync.Mutex
	seen []Payment
}

func (r *stepRecorder) on(p *Payment) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, *p)
}

// of is what was notified about payment id, as "State" or "State@trap".
func (r *stepRecorder) of(id string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, p := range r.seen {
		if p.ID == id {
			out = append(out, string(p.State)+map[bool]string{true: "@" + p.Trap(), false: ""}[p.Trap() != ""])
		}
	}
	return out
}

func stepTestEngine(t *testing.T, funded int64) (*Engine, *Ledger, *stepRecorder) {
	t.Helper()
	ledger := NewLedger()
	ledger.mustSeed("bc_acc_test_funded", "VBTESTFUNDED0000001", "Test Funded", "EUR", funded)
	ledger.mustSeed("bc_acc_test_merchant", "VBTESTMERCHANT00001", "Test Merchant", "EUR", 0)
	rec := &stepRecorder{}
	return NewEngine(ledger, time.Millisecond, rec.on), ledger, rec
}

func payout(id string) *Payment {
	return &Payment{ID: id, FromAccountID: "bc_acc_test_funded", ToAccountID: "bc_acc_test_merchant", Amount: "10.00", Currency: "EUR"}
}

// TestABookingTakesATrapsStepsThroughTheEngine: a booking nobody processes
// stays booked; each step a trap names is the engine's own transition, with
// its money, its notifications (marked with the trap) and its reason.
func TestABookingTakesATrapsStepsThroughTheEngine(t *testing.T) {
	engine, ledger, rec := stepTestEngine(t, 100_00)
	step := func(b *Booking, s Step) Payment {
		t.Helper()
		var got Payment
		if err := b.Step(s, func(p Payment) { got = p }); err != nil {
			t.Fatalf("%s: %v", s.To, err)
		}
		return got
	}

	// Left alone: booked for good.
	engine.Book(payout("bcp_held"))
	time.Sleep(10 * time.Millisecond)
	if got := mustGet(t, engine, "bcp_held").State; got != NotificationOutgoingPaymentBooked {
		t.Fatalf("a booking nobody processes = %s, want it booked", got)
	}

	// Rejected, with the trap's reason on the payment and on the report.
	got := step(engine.Book(payout("bcp_rej")), Step{To: string(NotificationOutgoingPaymentRejected),
		ReasonCode: "AC01", ReasonDescription: "Incorrect account number", Trap: "trp_0001"})
	if got.State != NotificationOutgoingPaymentRejected || got.RejectionReasonCode != "AC01" || got.Trap() != "trp_0001" {
		t.Fatalf("rejected = %+v", got)
	}
	rows := Rejections(engine.List(), RejectionQuery{TransactionDate: BusinessDate(got.CreatedAt), IncludeReceived: true, IncludeMissingFunds: true})
	reasons := map[string]string{}
	for _, r := range rows {
		reasons[*r.PaymentReferenceNumber] = *r.StatusReason
	}
	if reasons[got.ReferenceNumber] != "AC01 Incorrect account number" {
		t.Errorf("the rejection report's reason = %q", reasons[got.ReferenceNumber])
	}
	if got := mustGet(t, engine, "bcp_rej"); got.Trap() != "" {
		t.Errorf("the stored payment carries the trap %q: only what a step notifies does", got.Trap())
	}

	// Processed, then reversed: the money goes and comes back.
	b := engine.Book(payout("bcp_rev"))
	step(b, Step{To: string(NotificationOutgoingPaymentProcessed), Trap: "trp_0002"})
	if got := balance(t, ledger, "bc_acc_test_merchant"); got != "10.00" {
		t.Fatalf("merchant after processed = %s", got)
	}
	step(b, Step{To: string(NotificationReversed), Reason: "scheme", Trap: "trp_0002"})
	if got := balance(t, ledger, "bc_acc_test_merchant"); got != "0.00" {
		t.Fatalf("merchant after reversed = %s", got)
	}
	if got := mustGet(t, engine, "bcp_rev"); got.ReversalReason != "scheme" {
		t.Errorf("reversal reason = %q", got.ReversalReason)
	}
	want := []string{"OutgoingPaymentBooked", "OutgoingPaymentProcessed@trp_0002", "OutgoingPaymentBooked@trp_0002", "Reversed@trp_0002"}
	if got := rec.of("bcp_rev"); !slices.Equal(got, want) {
		t.Errorf("notified %v, want %v", got, want)
	}

	// Processed, then returned: a return payment, wholly the trap's.
	b = engine.Book(payout("bcp_ret"))
	step(b, Step{To: string(NotificationOutgoingPaymentProcessed), Trap: "trp_0003"})
	ret := step(b, Step{To: StepReturned, ReasonCode: "AC04", ReasonDescription: "Closed account number", Trap: "trp_0003"})
	if !ret.Return || ret.ReturnOf != "bcp_ret" || ret.ReturnReasonCode != "AC04" {
		t.Fatalf("return = %+v", ret)
	}
	engine.Wait()
	want = []string{"IncomingPaymentBooked@trp_0003", "IncomingPaymentProcessed@trp_0003"}
	if got := rec.of(ret.ID); !slices.Equal(got, want) {
		t.Errorf("the return notified %v, want %v", got, want)
	}
	if got := mustGet(t, engine, "bcp_ret").State; got != NotificationOutgoingPaymentProcessed {
		t.Errorf("a returned payout = %s, want it still processed", got)
	}

	// A step the books cannot follow: reversing what is no longer processed.
	err := b.Step(Step{To: string(NotificationReversed)}, nil)
	var d *Diverted
	if !errors.As(err, &d) || !strings.Contains(d.Reason, "already been returned") {
		t.Errorf("reversing a returned payout = %v, want it diverted", err)
	}
}

// TestTheBooksComeFirst: processing with the safeguarding account short
// ends the payout MissingFunding, as it would untouched, and says so.
func TestTheBooksComeFirst(t *testing.T) {
	engine, ledger, rec := stepTestEngine(t, 5_00)
	err := engine.Book(payout("bcp_short")).Step(Step{To: string(NotificationOutgoingPaymentProcessed), Trap: "trp_0001"}, nil)
	var d *Diverted
	if !errors.As(err, &d) || !strings.Contains(d.Reason, "holds 5.00 EUR, short of 10.00") || !strings.Contains(d.Reason, "MissingFunding instead") {
		t.Fatalf("processing short = %v", err)
	}
	if got := mustGet(t, engine, "bcp_short").State; got != NotificationMissingFunding {
		t.Errorf("state = %s, want MissingFunding", got)
	}
	if got := balance(t, ledger, "bc_acc_test_funded"); got != "5.00" {
		t.Errorf("funded = %s, want untouched", got)
	}
	if got := rec.of("bcp_short"); !slices.Equal(got, []string{"OutgoingPaymentBooked", "MissingFunding@trp_0001"}) {
		t.Errorf("notified %v", got)
	}
}

// TestAStepAfterAResetDoesNothing: a step due after a reset ended the run
// its payout was booked in moves no money and notifies nothing.
func TestAStepAfterAResetDoesNothing(t *testing.T) {
	engine, ledger, rec := stepTestEngine(t, 100_00)
	b := engine.Book(payout("bcp_gone"))
	endRun(engine.run)
	called := false
	if err := b.Step(Step{To: string(NotificationOutgoingPaymentProcessed)}, func(Payment) { called = true }); !errors.Is(err, ErrReset) || called {
		t.Fatalf("a step after the reset = %v (applied %v), want ErrReset", err, called)
	}
	if got := balance(t, ledger, "bc_acc_test_merchant"); got != "0.00" {
		t.Errorf("merchant = %s, want nothing moved", got)
	}
	if got := rec.of("bcp_gone"); len(got) != 1 {
		t.Errorf("notified %v, want the booking alone", got)
	}
}

// TestEngineAssignsBankReferenceNumbers: every payment the bank accepts,
// outgoing or incoming, gets its own 010F10-prefixed reference.
func TestEngineAssignsBankReferenceNumbers(t *testing.T) {
	ledger := NewLedger()
	ledger.mustSeed("bc_acc_test_funded", "VBTESTFUNDED0000009", "Test Funded", "EUR", 50_000_000)
	ledger.mustSeed("bc_acc_test_merchant", "VBTESTMERCHANT00009", "Test Merchant", "EUR", 0)
	engine := NewEngine(ledger, time.Millisecond, nil)

	out := &Payment{ID: "bcp_ref1", FromAccountID: "bc_acc_test_funded",
		ToAccountID: "bc_acc_test_merchant", Amount: "1.00", Currency: "EUR"}
	engine.Create(out)
	in, err := engine.CreditIncoming("bc_acc_test_merchant", "EUR", "2.00", "lump")
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for _, p := range []*Payment{out, in} {
		ref := p.ReferenceNumber
		if len(ref) != 16 || ref[:6] != "010F10" {
			t.Errorf("%s: reference %q, want 16 characters under 010F10", p.ID, ref)
		}
		if seen[ref] {
			t.Errorf("%s: reference %q handed out twice", p.ID, ref)
		}
		seen[ref] = true
	}
}

// TestEngineReturnIsANewIncomingPayment: a return is not a status. The
// payout stays Processed, and the money comes back as a payment of its own
// — own id, own reference, `return` set — that is booked and then
// processed like any incoming payment. A payout comes back at most once,
// and a returned payout can no longer be reversed.
func TestEngineReturnIsANewIncomingPayment(t *testing.T) {
	ledger := NewLedger()
	ledger.mustSeed("bc_acc_test_funded", "VBTESTFUNDED0000010", "Test Funded", "EUR", 50_000_000)
	ledger.mustSeed("bc_acc_test_merchant", "VBTESTMERCHANT00010", "Test Merchant", "EUR", 0)
	events := make(chan Payment, 8)
	engine := NewEngine(ledger, time.Millisecond, func(p *Payment) { events <- *p })
	next := func(want NotificationType) Payment {
		t.Helper()
		select {
		case got := <-events:
			if got.State != want {
				t.Fatalf("notification = %s, want %s", got.State, want)
			}
			return got
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s", want)
		}
		return Payment{}
	}

	if _, err := engine.Return("bcp_nope", "", ""); err != ErrPaymentNotFound {
		t.Fatalf("unknown payment: err = %v, want ErrPaymentNotFound", err)
	}
	payout := &Payment{ID: "bcp_ret1", FromAccountID: "bc_acc_test_funded",
		ToAccountID: "bc_acc_test_merchant", Amount: "75.00", Currency: "EUR"}
	engine.Create(payout)
	next(NotificationOutgoingPaymentBooked)
	if _, err := engine.Return(payout.ID, "", ""); err != ErrInvalidState {
		t.Fatalf("booked payout: err = %v, want ErrInvalidState", err)
	}
	next(NotificationOutgoingPaymentProcessed)

	ret, err := engine.Return(payout.ID, "AC04", "Closed account number")
	if err != nil {
		t.Fatalf("Return: %v", err)
	}
	if ret.ID == payout.ID || !ret.Return || ret.ReturnOf != payout.ID {
		t.Fatalf("return payment = %+v, want a new payment flagged as the payout's return", ret)
	}
	if ret.ReferenceNumber == "" || ret.ReferenceNumber == payout.ReferenceNumber {
		t.Errorf("return reference = %q, want its own (payout's is %q)", ret.ReferenceNumber, payout.ReferenceNumber)
	}
	if ret.FromAccountID != "bc_acc_test_merchant" || ret.ToAccountID != "bc_acc_test_funded" {
		t.Errorf("return moves %s -> %s, want merchant back to the funded account", ret.FromAccountID, ret.ToAccountID)
	}
	wantLines := []string{"RETURN OF PAYMENT", payout.ReferenceNumber, "AC04 Closed account number"}
	if strings.Join(ret.Remittance, "|") != strings.Join(wantLines, "|") {
		t.Errorf("remittance = %q, want %q", ret.Remittance, wantLines)
	}
	next(NotificationIncomingPaymentBooked)
	if got := next(NotificationIncomingPaymentProcessed); got.ID != ret.ID || !got.Return {
		t.Errorf("processed notification for %s (return %v), want the return payment", got.ID, got.Return)
	}

	original, _ := engine.Get(payout.ID)
	if original.State != NotificationOutgoingPaymentProcessed || original.ReturnedBy != ret.ID {
		t.Errorf("payout state = %s, returnedBy = %q; want still Processed, returned by %s",
			original.State, original.ReturnedBy, ret.ID)
	}
	if merchant, _ := ledger.Get("bc_acc_test_merchant"); merchant.Balance != "0.00" {
		t.Errorf("merchant balance after return = %s, want 0.00", merchant.Balance)
	}
	if _, err := engine.Return(payout.ID, "", ""); err != ErrAlreadyReturned {
		t.Errorf("second return: err = %v, want ErrAlreadyReturned", err)
	}
	if _, err := engine.Reverse(payout.ID, ""); err != ErrAlreadyReturned {
		t.Errorf("reverse after return: err = %v, want ErrAlreadyReturned", err)
	}
}

// TestBookingsFollowTheRunnersClock: with the platform's clock moved, a
// payment is stamped — and so dated on the reports — on the platform's
// day, not the wall clock's.
func TestBookingsFollowTheRunnersClock(t *testing.T) {
	labclock.Set(72 * time.Hour)
	defer labclock.Set(0)

	ledger := NewLedger()
	ledger.mustSeed("bc_acc_test_funded", "VBTESTFUNDED0000011", "Test Funded", "EUR", 50_000_000)
	ledger.mustSeed("bc_acc_test_merchant", "VBTESTMERCHANT00011", "Test Merchant", "EUR", 0)
	engine := NewEngine(ledger, 0, nil)
	p := &Payment{ID: "bcp_clock1", FromAccountID: "bc_acc_test_funded",
		ToAccountID: "bc_acc_test_merchant", Amount: "1.00", Currency: "EUR"}
	engine.Create(p)

	created, err := time.Parse(time.RFC3339, p.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if d := created.Sub(time.Now()); d < 72*time.Hour-2*time.Second || d > 72*time.Hour+2*time.Second {
		t.Fatalf("createdAt %s is %s from the wall clock, want the runner's +72h", p.CreatedAt, d)
	}
	if got, want := BusinessDate(p.CreatedAt), BusinessDate(labclock.Now().Format(time.RFC3339)); got != want {
		t.Errorf("business date %s, want the runner's %s", got, want)
	}
}
