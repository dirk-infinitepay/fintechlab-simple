package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/bankingcircle"
	"github.com/webduvet/fintechlab-simple/internal/trap"
)

// trapRig is Banking Circle wired as main wires it, with every surface a
// trap test touches: the control port (traps, reset, activity), the bridge
// (payouts, money in), the vendor API (status, reports), a subscriber
// that decrypts what it is sent, and a beneficiary bank that counts
// credits. PROCESSING_DELAY and the batch flush are 5 ms.
type trapRig struct {
	t       *testing.T
	a       *app
	ctl     string
	bridge  string
	vendor  string
	tok     string
	notes   chan note
	credits *atomic.Int64
}

// note is one notification as the subscriber decrypted it.
type note struct {
	typ, payment string
	ret          bool
	plain        string // the whole envelope it came in
}

func newTrapRig(t *testing.T) *trapRig {
	t.Helper()
	a := newControlTestApp(t)
	r := &trapRig{t: t, a: a, notes: make(chan note, 256), credits: &atomic.Int64{}}
	bank := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.credits.Add(1)
	}))
	t.Cleanup(bank.Close)
	a.bankCreditURL = bank.URL
	sub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		nonce, _ := base64.StdEncoding.DecodeString(req.Header.Get("Nonce"))
		tag, _ := base64.StdEncoding.DecodeString(req.Header.Get("AuthenticationTag"))
		plain, err := decryptNotification(body, nonce, tag, a.notifKey)
		if err != nil {
			t.Errorf("decrypt: %v", err)
			return
		}
		var env bankingcircle.Envelope
		if err := json.Unmarshal(plain, &env); err != nil {
			t.Errorf("envelope: %v", err)
			return
		}
		for _, n := range env.Notifications {
			p, _ := n.Payment.(map[string]any)
			id, _ := p["paymentId"].(string)
			ret, _ := p["return"].(bool)
			r.notes <- note{typ: n.NotificationType, payment: id, ret: ret, plain: string(plain)}
		}
	}))
	t.Cleanup(sub.Close)
	subscribeAll(t, a, sub.URL)
	r.ctl = controlServer(t, a).URL
	bridge := httptest.NewServer(a.internalMux())
	t.Cleanup(bridge.Close)
	r.bridge = bridge.URL
	vendor := httptest.NewServer(vendorMux(t, a))
	t.Cleanup(vendor.Close)
	r.vendor = vendor.URL
	r.tok = a.tokens.issue(time.Hour)
	return r
}

// arm arms a trap over POST /sim/traps on the control port.
func (r *trapRig) arm(body string) trap.Trap {
	r.t.Helper()
	code, b := raw(r.t, "POST", r.ctl+"/sim/traps", "", nil, body)
	if code != 201 {
		r.t.Fatalf("arm %s = %d %s", body, code, b)
	}
	var tr trap.Trap
	if err := json.Unmarshal(b, &tr); err != nil {
		r.t.Fatal(err)
	}
	return tr
}

func (r *trapRig) trap(id string) trap.Trap {
	r.t.Helper()
	code, b := raw(r.t, "GET", r.ctl+"/sim/traps/"+id, "", nil, "")
	if code != 200 {
		r.t.Fatalf("GET /sim/traps/%s = %d %s", id, code, b)
	}
	var tr trap.Trap
	_ = json.Unmarshal(b, &tr)
	return tr
}

// eventually polls cond until it holds, or fails the test after 3 s.
func (r *trapRig) eventually(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			r.t.Fatalf("never: %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// fund lands money on the EUR safeguarding account, and waits for its
// notifications so they do not mix with what a test watches for.
func (r *trapRig) fund(amount string) {
	r.t.Helper()
	out := mustCall(r.t, 202, "POST", r.bridge+"/internal/incoming-payments", "", `{"currency":"EUR","amount":"`+amount+`","reference":"lump"}`)
	r.await(out["id"].(string), bankingcircle.NotificationIncomingPaymentProcessed)
}

// payout is B4B's hand-off of one payout to iban.
func (r *trapRig) payout(id, iban, amount, externalRef string) map[string]any {
	r.t.Helper()
	return mustCall(r.t, 202, "POST", r.bridge+"/internal/payments", "",
		`{"paymentId":"`+id+`","accountId":"`+merchantAccount+`","amount":"`+amount+`","currency":"EUR","iban":"`+iban+
			`","holder":"Outlet Seven","externalRef":"`+externalRef+`"}`)
}

// await reads notifications until typ arrives for payment, and returns
// every one read on the way, that one included.
func (r *trapRig) await(payment string, typ bankingcircle.NotificationType) []note {
	r.t.Helper()
	var seen []note
	for {
		select {
		case n := <-r.notes:
			seen = append(seen, n)
			if n.payment == payment && n.typ == string(typ) {
				return seen
			}
		case <-time.After(3 * time.Second):
			r.t.Fatalf("no %s for %s; saw %v", typ, payment, seen)
		}
	}
}

// quiet is what arrives in d.
func (r *trapRig) quiet(d time.Duration) []note {
	var seen []note
	end := time.After(d)
	for {
		select {
		case n := <-r.notes:
			seen = append(seen, n)
		case <-end:
			return seen
		}
	}
}

func (r *trapRig) status(id string) string {
	r.t.Helper()
	return mustCall(r.t, 200, "GET", r.vendor+"/api/v1/payments/singles/"+id+"/status", r.tok, "")["status"].(string)
}

func (r *trapRig) balance(id string) string {
	r.t.Helper()
	return mustCall(r.t, 200, "GET", r.vendor+"/accounts/"+id, r.tok, "")["balance"].(string)
}

func (r *trapRig) payment(id string) map[string]any {
	r.t.Helper()
	return mustCall(r.t, 200, "GET", r.vendor+"/payments/"+id, r.tok, "")
}

func types(notes []note, payment string) []string {
	var out []string
	for _, n := range notes {
		if n.payment == payment {
			out = append(out, n.typ)
		}
	}
	return out
}

// events is one activity log's events, oldest first.
func events(l *activity.Log) []activity.Event {
	ev := l.Recent(0)
	slices.Reverse(ev)
	return ev
}

const merchant7 = "GB00SIM0000000000777"

// TestTrapRejectsAPayoutWithItsReason is the owner's first example: a payout
// to vIBAN X ends OutgoingPaymentRejected, with the trap's reason on the
// payment and the rejection report, and nothing moves.
func TestTrapRejectsAPayoutWithItsReason(t *testing.T) {
	r := newTrapRig(t)
	r.fund("100.00")
	tr := r.arm(`{"label": "merchant 7 rejected",
	 "hook": "payment.outgoing",
	 "match": {"creditor.iban": "GB00 SIM0 0000 0000 0777"},
	 "effect": {"path": [{"to": "OutgoingPaymentRejected", "reason_code": "AC01",
	                      "reason_description": "Incorrect account number"}]}}`)

	booked := r.payout("bcp_rej7", merchant7, "10.00", "sttl_7")
	if booked["state"] != "OutgoingPaymentBooked" {
		t.Fatalf("the bridge answered %v, want the payout booked", booked)
	}
	seen := r.await("bcp_rej7", bankingcircle.NotificationOutgoingPaymentRejected)
	if got := types(seen, "bcp_rej7"); !slices.Equal(got, []string{"OutgoingPaymentBooked", "OutgoingPaymentRejected"}) {
		t.Errorf("notified %v", got)
	}
	if got := r.status("bcp_rej7"); got != "Rejected" {
		t.Errorf("status = %s", got)
	}
	p := r.payment("bcp_rej7")
	if p["rejectionReasonCode"] != "AC01" || p["rejectionReasonDescription"] != "Incorrect account number" {
		t.Errorf("payment = %v, want the trap's reason on it", p)
	}
	day := bankingcircle.BusinessDate(p["createdAt"].(string))
	rows, _ := mustCall(t, 200, "GET", r.vendor+"/api/v1/reports/rejection-report?TransactionDate="+day, r.tok, "")["rejections"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["statusReason"] != "AC01 Incorrect account number" {
		t.Errorf("rejection report = %v", rows)
	}
	if got := r.balance(merchantAccount); got != "0.00" {
		t.Errorf("merchant = %s, nothing moves on a rejection", got)
	}
	got := r.trap(tr.ID)
	if got.State != trap.StateSpent || len(got.Caught) != 1 || got.Caught[0].Subject != "bcp_rej7" ||
		len(got.Caught[0].Applied) != 1 || got.Caught[0].Applied[0].To != "OutgoingPaymentRejected" {
		t.Errorf("trap after = %+v", got)
	}
	// Nothing on the vendor surface says a trap did it.
	for _, n := range seen {
		if strings.Contains(n.plain, tr.ID) || strings.Contains(strings.ToLower(n.plain), "trap") {
			t.Errorf("a notification names the trap: %s", n.plain)
		}
	}
	if _, ok := p["trap"]; ok {
		t.Errorf("the payment names the trap: %v", p)
	}
}

// TestTrapProcessesThenReversesAfterADelay is the owner's second example,
// with a short wait: processed through the engine (money off the
// safeguarding account, credited at the beneficiary's bank), then reversed
// that long after.
func TestTrapProcessesThenReversesAfterADelay(t *testing.T) {
	r := newTrapRig(t)
	r.fund("100.00")
	tr := r.arm(`{"label": "merchant 7 processed, then reversed",
	 "hook": "payment.outgoing",
	 "match": {"creditor.iban": "` + merchant7 + `"},
	 "effect": {"path": [{"to": "OutgoingPaymentProcessed"},
	                     {"to": "Reversed", "after": "40ms", "reason": "scheme"}]}}`)

	r.payout("bcp_rev7", merchant7, "10.00", "sttl_7")
	seen := r.await("bcp_rev7", bankingcircle.NotificationReversed)
	want := []string{"OutgoingPaymentBooked", "OutgoingPaymentProcessed", "OutgoingPaymentBooked", "Reversed"}
	if got := types(seen, "bcp_rev7"); !slices.Equal(got, want) {
		t.Errorf("notified %v, want %v", got, want)
	}
	r.eventually("the beneficiary bank credited", func() bool { return r.credits.Load() == 1 })
	if r.balance(sgaEUR) != "100.00" || r.balance(merchantAccount) != "0.00" {
		t.Errorf("after the reversal: safeguarding %s, merchant %s; want the money back", r.balance(sgaEUR), r.balance(merchantAccount))
	}
	if got := r.status("bcp_rev7"); got != "Reversed" {
		t.Errorf("status = %s", got)
	}
	if p := r.payment("bcp_rev7"); p["reversalReason"] != "scheme" {
		t.Errorf("reversal reason = %v", p["reversalReason"])
	}
	got := r.trap(tr.ID)
	applied := got.Caught[0].Applied
	if len(applied) != 2 || applied[1].To != "Reversed" {
		t.Fatalf("applied = %+v", applied)
	}
	if gap := applied[1].At.Sub(applied[0].At); gap < 40*time.Millisecond {
		t.Errorf("reversed %v after processed, want its 40ms", gap)
	}
	if len(got.PendingSteps) != 0 {
		t.Errorf("pending after the path = %+v", got.PendingSteps)
	}
}

// TestTrapReturnsAPayoutByTheLabClock: "returned two days later", without
// waiting two days. The step waits for the lab clock, and lands when it is
// moved.
func TestTrapReturnsAPayoutByTheLabClock(t *testing.T) {
	r := newTrapRig(t)
	var mu sync.Mutex
	lab := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	r.a.traps.LabNow = func() time.Time { mu.Lock(); defer mu.Unlock(); return lab }
	r.a.traps.LabPoll = 2 * time.Millisecond
	r.fund("100.00")
	tr := r.arm(`{"hook": "payment.outgoing", "match": {"external_ref": "sttl_v1:4c1e:7"},
	 "effect": {"path": [{"to": "OutgoingPaymentProcessed"},
	                     {"to": "Returned", "after_lab": "48h", "reason_code": "AC04",
	                      "reason_description": "Closed account number"}]}}`)

	r.payout("bcp_ret7", merchant7, "10.00", "sttl_v1:4c1e:7")
	r.await("bcp_ret7", bankingcircle.NotificationOutgoingPaymentProcessed)
	r.eventually("the Returned step pending", func() bool {
		ps := r.trap(tr.ID).PendingSteps
		return len(ps) == 1 && ps[0].Step.To == "Returned" && ps[0].DueLab != nil
	})
	if n := r.quiet(40 * time.Millisecond); len(n) != 0 {
		t.Fatalf("%v arrived with the lab clock still", n)
	}
	mu.Lock()
	lab = lab.Add(49 * time.Hour)
	mu.Unlock()

	// The return is a payment of its own: booked, then processed with
	// return: true.
	var seen []note
	var ret note
	for ret.payment == "" {
		select {
		case n := <-r.notes:
			seen = append(seen, n)
			if n.ret && n.typ == "IncomingPaymentProcessed" {
				ret = n
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("no return arrived once the lab clock moved; saw %v", seen)
		}
	}
	if got := types(seen, ret.payment); !slices.Equal(got, []string{"IncomingPaymentBooked", "IncomingPaymentProcessed"}) {
		t.Errorf("the return notified %v", got)
	}
	if r.balance(sgaEUR) != "100.00" {
		t.Errorf("safeguarding after the return = %s", r.balance(sgaEUR))
	}
	if got := r.status("bcp_ret7"); got != "Processed" {
		t.Errorf("a returned payout's status = %s, want Processed", got)
	}
	rp := r.payment(ret.payment)
	if rp["returnOf"] != "bcp_ret7" || rp["returnReasonCode"] != "AC04" || rp["returnReasonDescription"] != "Closed account number" {
		t.Errorf("return payment = %v", rp)
	}
}

// TestAnEmptyPathLeavesThePayoutBooked: [] is today's Pending.
func TestAnEmptyPathLeavesThePayoutBooked(t *testing.T) {
	r := newTrapRig(t)
	r.fund("100.00")
	tr := r.arm(`{"hook": "payment.outgoing", "match": {"payment_id": "bcp_held"}, "effect": {"path": []}}`)
	r.payout("bcp_held", merchant7, "10.00", "")
	r.await("bcp_held", bankingcircle.NotificationOutgoingPaymentBooked)
	if n := r.quiet(60 * time.Millisecond); len(n) != 0 {
		t.Fatalf("%v after the booking, want nothing", n)
	}
	if got := r.status("bcp_held"); got != "PendingProcessing" {
		t.Errorf("status = %s", got)
	}
	if got := r.trap(tr.ID); len(got.Caught) != 1 || len(got.Caught[0].Applied) != 0 {
		t.Errorf("trap = %+v", got)
	}
	if r.balance(merchantAccount) != "0.00" {
		t.Error("money moved for a payout left booked")
	}
}

// TestATrapOnOneIBANLeavesTheRestToTheBank: a trap takes the payouts it
// matches and no other.
func TestATrapOnOneIBANLeavesTheRestToTheBank(t *testing.T) {
	r := newTrapRig(t)
	r.fund("100.00")
	tr := r.arm(`{"hook": "payment.outgoing", "match": {"creditor.iban": "` + merchant7 + `"}, "times": null,
	 "effect": {"path": [{"to": "OutgoingPaymentRejected"}]}}`)

	r.payout("bcp_other", "GB00SIM0000000000003", "10.00", "")
	r.await("bcp_other", bankingcircle.NotificationOutgoingPaymentProcessed)
	r.payout("bcp_seven", merchant7, "10.00", "")
	r.await("bcp_seven", bankingcircle.NotificationOutgoingPaymentRejected)
	r.payout("bcp_other2", "GB00SIM0000000000004", "5.00", "")
	r.await("bcp_other2", bankingcircle.NotificationOutgoingPaymentProcessed)

	got := r.trap(tr.ID)
	if got.State != trap.StateArmed || len(got.Caught) != 1 || got.Caught[0].Subject != "bcp_seven" {
		t.Errorf("trap = %+v, want it to have caught bcp_seven alone", got)
	}
	if r.balance(merchantAccount) != "15.00" {
		t.Errorf("merchant = %s, want the two untrapped payouts", r.balance(merchantAccount))
	}
	// A rejection the bank came to without a trap keeps the fixed reason.
	p := r.payment("bcp_seven")
	if _, ok := p["rejectionReasonCode"]; ok {
		t.Errorf("a reasonless rejection = %v", p)
	}
	day := bankingcircle.BusinessDate(p["createdAt"].(string))
	rows, _ := mustCall(t, 200, "GET", r.vendor+"/api/v1/reports/rejection-report?TransactionDate="+day, r.tok, "")["rejections"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["statusReason"] != "Payment rejected by the beneficiary bank (simulated)" {
		t.Errorf("rejection report = %v", rows)
	}
}

// TestTheOutcomeQueueIsAShortcutForTraps: POST …/outcomes answers as it
// always did, and what it queues is one match-all trap per outcome, in
// order, which the next payouts take.
func TestTheOutcomeQueueIsAShortcutForTraps(t *testing.T) {
	r := newTrapRig(t)
	r.fund("100.00")
	queued := func(base string) any {
		return mustCall(t, 200, "GET", base, "", "")["queued"]
	}
	if got := queued(r.ctl + "/sim/payments/outcomes"); !reflect.DeepEqual(got, []any{}) {
		t.Fatalf("an empty queue = %#v, want []", got)
	}
	out := mustCall(t, 200, "POST", r.ctl+"/sim/payments/outcomes", "", `{"outcomes":["Rejected","Pending"]}`)
	if !reflect.DeepEqual(out, map[string]any{"queued": []any{"Rejected", "Pending"}}) {
		t.Fatalf("POST = %v", out)
	}
	if got := queued(r.bridge + "/internal/payments/outcomes"); !reflect.DeepEqual(got, []any{"Rejected", "Pending"}) {
		t.Fatalf("the bridge's GET = %v", got)
	}
	code, b := raw(t, "POST", r.bridge+"/internal/payments/outcomes", "", nil, `{"outcomes":["Lost"]}`)
	if code != 400 || !strings.Contains(string(b), "bankingcircle: unknown outcome (want Rejected or Pending)") {
		t.Fatalf("an unknown outcome = %d %s", code, b)
	}

	var listed struct{ Traps []trap.Trap }
	_, b = raw(t, "GET", r.ctl+"/sim/traps?state=armed", "", nil, "")
	_ = json.Unmarshal(b, &listed)
	if len(listed.Traps) != 2 {
		t.Fatalf("armed traps = %s", b)
	}
	// Newest first.
	pending, rejected := listed.Traps[0], listed.Traps[1]
	if rejected.Label != "outcome queue: Rejected" || len(rejected.Match) != 0 || len(rejected.Effect.Path) != 1 ||
		rejected.Effect.Path[0].To != "OutgoingPaymentRejected" || rejected.Hook != "payment.outgoing" {
		t.Errorf("the Rejected trap = %+v", rejected)
	}
	if pending.Label != "outcome queue: Pending" || len(pending.Effect.Path) != 0 {
		t.Errorf("the Pending trap = %+v", pending)
	}

	r.payout("bcp_q1", merchant7, "1.00", "")
	r.await("bcp_q1", bankingcircle.NotificationOutgoingPaymentRejected)
	if got := queued(r.ctl + "/sim/payments/outcomes"); !reflect.DeepEqual(got, []any{"Pending"}) {
		t.Fatalf("after one payout the queue = %v, want [Pending]", got)
	}
	r.payout("bcp_q2", merchant7, "1.00", "")
	r.payout("bcp_q3", merchant7, "1.00", "")
	r.await("bcp_q3", bankingcircle.NotificationOutgoingPaymentProcessed)
	if got := r.status("bcp_q2"); got != "PendingProcessing" {
		t.Errorf("the Pending payout = %s", got)
	}
	if got := queued(r.ctl + "/sim/payments/outcomes"); !reflect.DeepEqual(got, []any{}) {
		t.Fatalf("the queue once taken = %v", got)
	}

	// Disarming a queued outcome's trap takes it off the queue.
	mustCall(t, 200, "POST", r.ctl+"/sim/payments/outcomes", "", `{"outcomes":["Pending","Rejected"]}`)
	_, b = raw(t, "GET", r.ctl+"/sim/traps?state=armed", "", nil, "")
	_ = json.Unmarshal(b, &listed)
	mustCall(t, 200, "DELETE", r.ctl+"/sim/traps/"+listed.Traps[1].ID, "", "")
	if got := queued(r.ctl + "/sim/payments/outcomes"); !reflect.DeepEqual(got, []any{"Rejected"}) {
		t.Fatalf("after disarming the Pending trap the queue = %v", got)
	}
}

// TestTheBooksComeFirst: a step the books cannot follow (processing with
// the safeguarding account short) ends the payout the bank's own way,
// MissingFunding, and the rest of the path is dropped.
func TestTheBooksComeFirst(t *testing.T) {
	r := newTrapRig(t)
	r.fund("0.50")
	tr := r.arm(`{"hook": "payment.outgoing", "match": {},
	 "effect": {"path": [{"to": "OutgoingPaymentProcessed"}, {"to": "Reversed", "after": "10ms"}]}}`)
	r.payout("bcp_short", merchant7, "1.00", "")
	r.await("bcp_short", bankingcircle.NotificationMissingFunding)
	r.eventually("the catch diverted", func() bool {
		c := r.trap(tr.ID).Caught
		return len(c) == 1 && c[0].Diverted != ""
	})
	got := r.trap(tr.ID).Caught[0]
	if !strings.Contains(got.Diverted, "holds 0.50 EUR, short of 1.00") || len(got.Applied) != 0 {
		t.Errorf("catch = %+v", got)
	}
	if n := r.quiet(50 * time.Millisecond); len(n) != 0 {
		t.Errorf("%v after the divert, want the path dropped", n)
	}
	if got := r.status("bcp_short"); got != "MissingFunding" {
		t.Errorf("status = %s", got)
	}
	last := events(r.a.traps.Log())
	if e := last[len(last)-1]; e.Op != "trap.diverted" || e.Status != activity.StatusWarn {
		t.Errorf("last traps event = %+v", e)
	}
	var step *activity.Event
	for _, e := range events(r.a.payLog) {
		if e.Op == "payment.state" {
			step = &e
		}
	}
	if step == nil || step.Detail["state"] != "MissingFunding" || step.Detail["asked"] != "OutgoingPaymentProcessed" || step.Detail["trap"] != tr.ID {
		t.Errorf("payments log = %+v", step)
	}
}

// TestAResetMidPathDropsTheRest: the steps still to come when the lab is
// reset never land: no notification, no money moves.
func TestAResetMidPathDropsTheRest(t *testing.T) {
	r := newTrapRig(t)
	r.fund("100.00")
	r.arm(`{"hook": "payment.outgoing", "match": {},
	 "effect": {"path": [{"to": "OutgoingPaymentProcessed"}, {"to": "Reversed", "after": "150ms"}]}}`)
	r.payout("bcp_mid", merchant7, "10.00", "")
	r.await("bcp_mid", bankingcircle.NotificationOutgoingPaymentProcessed)
	r.eventually("credited", func() bool { return r.credits.Load() == 1 })

	out := mustCall(t, 200, "POST", r.ctl+"/sim/reset", "", `{"scope":"flow"}`)
	cleared := out["cleared"].(map[string]any)
	if cleared["traps"] != float64(1) || cleared["trap_steps"] != float64(1) {
		t.Fatalf("reset = %v, want the trap and its Reversed step cleared", out)
	}
	if n := r.quiet(250 * time.Millisecond); len(n) != 0 {
		t.Fatalf("%v after the reset", n)
	}
	for _, id := range []string{sgaEUR, merchantAccount} {
		if got := r.balance(id); got != "0.00" {
			t.Errorf("%s = %s after the reset, want nothing moved back onto it", id, got)
		}
	}
	if got := r.credits.Load(); got != 1 {
		t.Errorf("credits = %d", got)
	}
	if ps, _ := mustCall(t, 200, "GET", r.ctl+"/sim/payments", "", "")["payments"].([]any); len(ps) != 0 {
		t.Errorf("payments = %v", ps)
	}
	if l := r.a.traps.List(""); len(l) != 0 {
		t.Errorf("traps after the reset = %+v", l)
	}
	for _, e := range events(r.a.traps.Log()) {
		t.Errorf("traps log after the reset: %+v", e)
	}
}

// TestAStepDueAfterTheBankResetDoesNotLand: the engine's own guard, alone.
// A step the trap store still runs (the bank was reset under it) finds the
// payout's run over and does nothing.
func TestAStepDueAfterTheBankResetDoesNotLand(t *testing.T) {
	r := newTrapRig(t)
	r.fund("100.00")
	tr := r.arm(`{"hook": "payment.outgoing", "match": {}, "effect": {"path": [{"to": "OutgoingPaymentProcessed", "after": "60ms"}]}}`)
	r.payout("bcp_late", merchant7, "10.00", "")
	r.await("bcp_late", bankingcircle.NotificationOutgoingPaymentBooked)
	r.a.bank.Reset(false, nil)
	// Not a divert: the bank's books did not refuse the step, its reset
	// did, so the catch records it as failed.
	r.eventually("the step tried", func() bool {
		c := r.trap(tr.ID).Caught
		return len(c) == 1 && c[0].Failed != ""
	})
	if got := r.trap(tr.ID).Caught[0]; !strings.Contains(got.Failed, "has been reset") || got.Diverted != "" || len(got.Applied) != 0 {
		t.Errorf("catch = %+v", got)
	}
	if n := r.quiet(30 * time.Millisecond); len(n) != 0 {
		t.Errorf("%v after the reset", n)
	}
	if r.balance(merchantAccount) != "0.00" || r.credits.Load() != 0 {
		t.Errorf("merchant %s, credits %d: the step landed", r.balance(merchantAccount), r.credits.Load())
	}
}

// TestWhatATrapCausedCarriesDetailTrap: the payments log's step events and
// the notifications log's batches carry detail.trap; what the bank did on
// its own does not.
func TestWhatATrapCausedCarriesDetailTrap(t *testing.T) {
	r := newTrapRig(t)
	r.fund("100.00")
	r.payout("bcp_plain", "GB00SIM0000000000003", "10.00", "")
	r.await("bcp_plain", bankingcircle.NotificationOutgoingPaymentProcessed)
	logged := func(kind string) func() bool {
		return func() bool {
			for _, e := range r.a.notifLog.Recent(0) {
				if strings.Contains(e.Summary, kind) {
					return true
				}
			}
			return false
		}
	}
	r.eventually("the money in logged", logged("IncomingPaymentProcessed"))
	r.eventually("the plain payout logged", logged("OutgoingPaymentProcessed"))
	before := len(r.a.notifLog.Recent(0))

	tr := r.arm(`{"hook": "payment.outgoing", "match": {"creditor.iban": "` + merchant7 + `"},
	 "effect": {"path": [{"to": "OutgoingPaymentProcessed"}, {"to": "Reversed", "after": "10ms", "reason": "scheme"}]}}`)
	r.payout("bcp_marked", merchant7, "10.00", "")
	r.await("bcp_marked", bankingcircle.NotificationReversed)
	r.eventually("the reversal's batch logged", logged("Reversed"))

	notifs := events(r.a.notifLog)
	for _, e := range notifs[:before] {
		if _, ok := e.Detail["trap"]; ok {
			t.Errorf("a batch from before any trap carries one: %+v", e)
		}
	}
	marked := 0
	for _, e := range notifs[before:] {
		if e.Detail["trap"] == tr.ID {
			marked++
		} else if strings.Contains(e.Summary, "Reversed") || strings.Contains(e.Summary, "Processed") {
			t.Errorf("a batch with the trap's transitions is not marked: %+v", e)
		}
	}
	if marked == 0 {
		t.Error("no notifications batch carries detail.trap")
	}

	var states []string
	for _, e := range events(r.a.payLog) {
		if e.Op != "payment.state" {
			continue
		}
		if e.Detail["trap"] != tr.ID || e.Detail["payment_id"] != "bcp_marked" {
			t.Errorf("payment.state = %+v", e)
		}
		states = append(states, e.Detail["state"])
	}
	if !slices.Equal(states, []string{"OutgoingPaymentProcessed", "Reversed"}) {
		t.Errorf("payment.state events %v", states)
	}

	var ops []string
	for _, e := range events(r.a.traps.Log()) {
		if e.Detail["trap"] != tr.ID {
			t.Errorf("traps log event without the trap: %+v", e)
		}
		ops = append(ops, e.Op)
	}
	if !slices.Equal(ops, []string{"trap.armed", "trap.caught", "trap.step", "trap.step"}) {
		t.Errorf("traps log = %v", ops)
	}

	// The traps log is on GET /sim/activity, last.
	logs, _ := mustCall(t, 200, "GET", r.ctl+"/sim/activity", "", "")["logs"].([]any)
	var names []string
	for _, l := range logs {
		names = append(names, l.(map[string]any)["name"].(string))
	}
	if !slices.Equal(names, []string{"payments", "notifications", "reports", "traps"}) {
		t.Errorf("activity logs = %v", names)
	}
}

// TestTheHookIsInTheCatalogue: GET /sim/traps/hooks on the control port
// lists payment.outgoing with its fields and graph, and /sim/info names it.
func TestTheHookIsInTheCatalogue(t *testing.T) {
	r := newTrapRig(t)
	code, b := raw(t, "GET", r.ctl+"/sim/traps/hooks", "", nil, "")
	if code != 200 {
		t.Fatalf("hooks = %d %s", code, b)
	}
	var cat struct {
		Service string `json:"service"`
		Hooks   []struct {
			trap.Hook
			FirstAfter string `json:"first_step_default_after"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &cat); err != nil {
		t.Fatal(err)
	}
	if cat.Service != "banking-circle" || len(cat.Hooks) != 1 {
		t.Fatalf("catalogue = %s", b)
	}
	h := cat.Hooks[0]
	if h.Name != "payment.outgoing" || h.Subject != "payment_id" || h.Effect != "path" || h.FirstAfter != "5ms" {
		t.Errorf("hook = %+v", h)
	}
	var fields []string
	for _, f := range h.Match {
		fields = append(fields, f.Name+":"+f.Type)
	}
	want := []string{"payment_id:string", "external_ref:string", "creditor.iban:iban", "creditor.account_number:string",
		"creditor.financial_institution:string", "creditor.holder:string", "amount:decimal", "currency:string"}
	if !slices.Equal(fields, want) {
		t.Errorf("fields = %v", fields)
	}
	g := h.Graph
	if g == nil || g.Start != "OutgoingPaymentBooked" || g.Walks ||
		!slices.Equal(g.Next["OutgoingPaymentBooked"], []string{"OutgoingPaymentProcessed", "OutgoingPaymentRejected", "MissingFunding"}) ||
		!slices.Equal(g.Next["OutgoingPaymentProcessed"], []string{"Reversed", "Returned"}) ||
		!slices.Equal(g.Params["Reversed"], []string{"reason"}) ||
		!slices.Equal(g.Params["OutgoingPaymentRejected"], []string{"reason_code", "reason_description"}) ||
		!slices.Equal(g.Params["Returned"], []string{"reason_code", "reason_description"}) {
		t.Errorf("graph = %+v", g)
	}

	info := mustCall(t, 200, "GET", r.ctl+"/sim/info", "", "")
	if !reflect.DeepEqual(info["hooks"], []any{"payment.outgoing"}) || info["traps"] == nil {
		t.Errorf("info = %v", info)
	}
	// An edge the graph does not have is refused when armed.
	code, b = raw(t, "POST", r.ctl+"/sim/traps", "", nil,
		`{"hook":"payment.outgoing","match":{},"effect":{"path":[{"to":"OutgoingPaymentRejected"},{"to":"Reversed"}]}}`)
	if code != 400 || !bytes.Contains(b, []byte("Reversed cannot follow OutgoingPaymentRejected")) {
		t.Errorf("an impossible path = %d %s", code, b)
	}
}
