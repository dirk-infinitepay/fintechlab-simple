package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/b4b"
	"github.com/webduvet/fintechlab-simple/internal/trap"
)

// trapLab is b4b on both listeners with two active payees, MID001 and
// MID002, and a processing delay short enough for a test.
type trapLab struct {
	a      *app
	l      listeners
	cb, bc *peer
	paid   int
}

var payees = map[string]string{"MID001": "GB00SIM0000000000041", "MID002": "GB00SIM0000000000042"}

func newTrapLab(t *testing.T, delay time.Duration, forceFail ...string) *trapLab {
	t.Helper()
	cb, bc := newPeer(t), newPeer(t)
	a := testApp(t, cb, bc, "", delay)
	var err error
	if a.traps, err = newTraps(delay, forceFail); err != nil {
		t.Fatal(err)
	}
	for mid, acct := range payees {
		if _, err := a.beneficiaries.Register(b4b.RegisterParams{ExternalRef: mid, AccountName: mid + " Ltd",
			AccountNumber: acct, FinancialInstitution: "SC112233"}); err != nil {
			t.Fatal(err)
		}
	}
	return &trapLab{a: a, l: serve(t, a, signingKey(t)), cb: cb, bc: bc}
}

// pay sends a payout to mid the way a platform does, and returns its id.
func (lab *trapLab) pay(t *testing.T, mid string) string {
	t.Helper()
	lab.paid++
	body := fmt.Sprintf(`{"external_ref": "stl_%s_%d", "beneficiary_id": %q, "amount": {"amount": 12.34, "currency": "EUR"},
		"currencyOfTransfer": "EUR", "debtorViban": {"account": "GB00SIM0000000000001"},
		"creditorAccount": {"account": %q, "financialInstitution": "SC112233"}, "chargeBearer": "SHA"}`, mid, lab.paid, mid, payees[mid])
	st, b := send(t, lab.l.vendor, "POST", "/oversight/v1/payments", body, lab.l.token)
	if st != 202 {
		t.Fatalf("POST /oversight/v1/payments = %d %s", st, b)
	}
	return object(t, b)["id"].(string)
}

// arm arms a trap on the control port and returns its id.
func (lab *trapLab) arm(t *testing.T, body string) string {
	t.Helper()
	st, b := send(t, lab.l.control, "POST", "/sim/traps", body, "")
	if st != 201 {
		t.Fatalf("POST /sim/traps = %d %s", st, b)
	}
	return object(t, b)["id"].(string)
}

func (lab *trapLab) trap(t *testing.T, id string) trap.Trap {
	t.Helper()
	st, b := send(t, lab.l.control, "GET", "/sim/traps/"+id, "", "")
	var tr trap.Trap
	if st != 200 || json.Unmarshal(b, &tr) != nil {
		t.Fatalf("GET /sim/traps/%s = %d %s", id, st, b)
	}
	return tr
}

func (lab *trapLab) traps(t *testing.T) []trap.Trap {
	t.Helper()
	st, b := send(t, lab.l.control, "GET", "/sim/traps", "", "")
	var out struct{ Traps []trap.Trap }
	if st != 200 || json.Unmarshal(b, &out) != nil {
		t.Fatalf("GET /sim/traps = %d %s", st, b)
	}
	return out.Traps
}

// status is the payment's state as a platform reads it.
func (lab *trapLab) status(t *testing.T, id string) string {
	t.Helper()
	st, b := send(t, lab.l.vendor, "GET", "/oversight/v1/payments/"+id, "", lab.l.token)
	if st != 200 {
		t.Fatalf("GET /oversight/v1/payments/%s = %d %s", id, st, b)
	}
	return object(t, b)["status"].(string)
}

// callbacks are the statuses the client was called back with for one
// payment, sorted: each is delivered from a goroutine of its own.
func (lab *trapLab) callbacks(id string) []string {
	var out []string
	for _, body := range lab.cb.received() {
		var cb struct{ ID, Status string }
		if json.Unmarshal([]byte(body), &cb) == nil && cb.ID == id {
			out = append(out, cb.Status)
		}
	}
	slices.Sort(out)
	return out
}

// events are the callbacks activity log's events about one payment, by the
// status each reported.
func (lab *trapLab) events(id string) map[string]activity.Event {
	out := map[string]activity.Event{}
	for _, e := range lab.a.cbLog.Recent(0) {
		if strings.HasPrefix(e.Summary, "payment "+id+" ") {
			status := strings.Fields(strings.TrimPrefix(e.Summary, "payment "+id+" status="))[0]
			out[status] = e
		}
	}
	return out
}

// created is the payouts log's payment.create event for one payment.
func (lab *trapLab) created(t *testing.T, id string) activity.Event {
	t.Helper()
	for _, e := range lab.a.payLog.Recent(0) {
		if e.Detail["payment_id"] == id {
			return e
		}
	}
	t.Fatalf("no payment.create event for %s", id)
	return activity.Event{}
}

func states(s ...b4b.PaymentState) []string {
	out := make([]string, len(s))
	for i, st := range s {
		out[i] = string(st)
	}
	slices.Sort(out)
	return out
}

func applied(c trap.Catch) []string {
	var out []string
	for _, a := range c.Applied {
		out = append(out, a.To)
	}
	return out
}

// The catalogue is the documentation an agent reads before arming
// anything: one hook, payment.create, whose graph is the engine's own and
// walks.
func TestTheHookCatalogueListsPaymentCreate(t *testing.T) {
	lab := newTrapLab(t, 10*time.Millisecond)
	st, b := send(t, lab.l.control, "GET", "/sim/traps/hooks", "", "")
	var cat struct {
		Service string
		Hooks   []struct {
			trap.Hook
			FirstAfter string `json:"first_step_default_after"`
		}
	}
	if st != 200 || json.Unmarshal(b, &cat) != nil || cat.Service != "b4b" || len(cat.Hooks) != 1 {
		t.Fatalf("GET /sim/traps/hooks = %d %s", st, b)
	}
	h := cat.Hooks[0]
	var fields []string
	for _, f := range h.Match {
		fields = append(fields, f.Name)
	}
	want := []string{"beneficiary_id", "beneficiary.external_ref", "company_id", "debtor_viban", "creditor.account", "amount", "currency", "external_ref"}
	if h.Name != "payment.create" || h.Subject != "payment_id" || h.Effect != "path" || !slices.Equal(fields, want) {
		t.Errorf("hook = %+v", h)
	}
	g := h.Graph
	if g == nil || !g.Walks || g.Start != "B4BAccepted" || !slices.Equal(g.Params["B4BFailed"], []string{"reason"}) ||
		!slices.Equal(g.Next["B4BTMPending"], []string{"B4BTMApproved", "B4BFailed"}) || len(g.Next) != 4 {
		t.Errorf("graph = %+v", g)
	}
	if h.FirstAfter != "10ms" {
		t.Errorf("first_step_default_after = %q, want the processing delay", h.FirstAfter)
	}
	if !strings.Contains(string(b), `"walks":true`) {
		t.Errorf("the catalogue does not say the graph walks: %s", b)
	}

	st, b = send(t, lab.l.control, "GET", "/sim/info", "", "")
	if info := object(t, b); st != 200 || fmt.Sprint(info["hooks"]) != "[payment.create]" {
		t.Errorf("GET /sim/info = %d %s", st, b)
	}

	for body, want := range map[string]string{
		`{"hook": "payment.create", "match": {}, "effect": {"path": [{"to": "B4BTMApproved"}, {"to": "B4BFailed"}]}}`:           "B4BFailed cannot follow B4BTMApproved",
		`{"hook": "payment.create", "match": {}, "effect": {"path": [{"to": "B4BTMPending", "reason": "x"}]}}`:                  "takes no parameters",
		`{"hook": "payment.create", "match": {"mid": "MID001"}, "effect": {"path": []}}`:                                        "beneficiary.external_ref",
		`{"hook": "payment.create", "match": {"amount": {"gte": "ten"}}, "effect": {"path": []}}`:                               "decimal",
		`{"hook": "payment.create", "match": {}, "effect": {"path": [{"to": "B4BSanctionsPending"}, {"to": "B4BTMApproved"}]}}`: "",
	} {
		st, b := send(t, lab.l.control, "POST", "/sim/traps", body, "")
		if want == "" && st != 201 || want != "" && (st != 400 || !strings.Contains(string(b), want)) {
			t.Errorf("%s = %d %s", body, st, b)
		}
	}
}

// Hang at TM review: the payment walks the states in between at the normal
// pace, calling back on each, and stays at B4BTMPending for good. Nothing
// reaches Banking Circle. Each transition the trap made says so.
func TestATrapHangsAPaymentAtTMReview(t *testing.T) {
	lab := newTrapLab(t, 10*time.Millisecond)
	trapID := lab.arm(t, `{"label": "MID001 hangs", "hook": "payment.create", "match": {"beneficiary.external_ref": "MID001"},
		"effect": {"path": [{"to": "B4BTMPending"}]}}`)
	id := lab.pay(t, "MID001")

	waitFor(t, "the walk to TM review", func() bool { return len(lab.events(id)) == 4 })
	time.Sleep(80 * time.Millisecond) // eight hops' worth: nothing more comes
	if got, want := lab.callbacks(id), states(b4b.StateAccepted, b4b.StateSanctionsPending, b4b.StateSanctionsApproved, b4b.StateTMPending); !slices.Equal(got, want) {
		t.Fatalf("callbacks = %v, want %v", got, want)
	}
	if st := lab.status(t, id); st != string(b4b.StateTMPending) {
		t.Fatalf("status = %s, want it hung at TM review", st)
	}
	if lab.bc.count() != 0 {
		t.Fatalf("%d bridge call(s) for a payment hung at TM review", lab.bc.count())
	}

	tr := lab.trap(t, trapID)
	if tr.State != trap.StateSpent || len(tr.Caught) != 1 || tr.Caught[0].Subject != id || len(tr.PendingSteps) != 0 ||
		!slices.Equal(applied(tr.Caught[0]), []string{"B4BSanctionsPending", "B4BSanctionsApproved", "B4BTMPending"}) {
		t.Errorf("the trap after its path: %+v", tr)
	}
	for status, e := range lab.events(id) {
		want := trapID
		if status == string(b4b.StateAccepted) {
			want = "" // the engine's own, trap or no trap
		}
		if e.Detail["trap"] != want {
			t.Errorf("the %s callback's activity: detail.trap = %q, want %q", status, e.Detail["trap"], want)
		}
	}
	if e := lab.created(t, id); e.Detail["trap"] != trapID {
		t.Errorf("payment.create detail = %v, want the trap that caught it", e.Detail)
	}
	if got, _ := lab.a.engine.Get(id); got.Trap != trapID {
		t.Errorf("the payment record names trap %q", got.Trap)
	}
}

// Fail at sanctions after a delay: the step's after is the wait at
// B4BSanctionsPending, and its reason is kept where the lab looks -- the
// payment record and the callback's activity -- while the callback itself
// looks exactly as it does without a trap.
func TestATrapFailsAPaymentAtSanctionsAfterADelay(t *testing.T) {
	lab := newTrapLab(t, 10*time.Millisecond)
	trapID := lab.arm(t, `{"hook": "payment.create", "match": {"beneficiary_id": "MID001"},
		"effect": {"path": [{"to": "B4BSanctionsPending"}, {"to": "B4BFailed", "after": "120ms", "reason": "sanctions hit"}]}}`)
	start := time.Now()
	id := lab.pay(t, "MID001")

	waitFor(t, "B4BSanctionsPending", func() bool { return lab.status(t, id) == string(b4b.StateSanctionsPending) })
	time.Sleep(50 * time.Millisecond)
	if st := lab.status(t, id); st != string(b4b.StateSanctionsPending) {
		t.Fatalf("status %s before the step's after was out", st)
	}
	waitFor(t, "B4BFailed", func() bool { return lab.status(t, id) == string(b4b.StateFailed) })
	if el := time.Since(start); el < 130*time.Millisecond {
		t.Errorf("failed %v after the payment, want one hop and the step's 120ms", el)
	}
	waitFor(t, "the failure's callback", func() bool { return len(lab.events(id)) == 3 })
	if got, want := lab.callbacks(id), states(b4b.StateAccepted, b4b.StateSanctionsPending, b4b.StateFailed); !slices.Equal(got, want) {
		t.Fatalf("callbacks = %v, want %v", got, want)
	}
	if lab.bc.count() != 0 {
		t.Fatalf("%d bridge call(s) for a failed payment", lab.bc.count())
	}

	if got, _ := lab.a.engine.Get(id); got.FailureReason != "sanctions hit" {
		t.Errorf("failure reason on the record = %q", got.FailureReason)
	}
	if e := lab.events(id)[string(b4b.StateFailed)]; e.Detail["reason"] != "sanctions hit" || e.Detail["trap"] != trapID {
		t.Errorf("the B4BFailed callback's activity = %v", e.Detail)
	}
	for _, body := range lab.cb.received() {
		var cb map[string]any
		_ = json.Unmarshal([]byte(body), &cb)
		if cb["id"] != id {
			continue
		}
		keys := make([]string, 0, len(cb))
		for k := range cb {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, []string{"id", "payload", "status"}) {
			t.Errorf("a trapped payment's callback carries %v: the wire must not change", keys)
		}
	}
	if tr := lab.trap(t, trapID); !slices.Equal(applied(tr.Caught[0]), []string{"B4BSanctionsPending", "B4BFailed"}) {
		t.Errorf("applied = %v", applied(tr.Caught[0]))
	}
}

// A trap on one beneficiary takes that one's payments and no other: the
// next payee's payout walks the default way to B4BTMApproved and is bridged
// into Banking Circle, with no trap mark anywhere.
func TestATrapOnOnePayeeLeavesTheOthersToTheDefaultWalk(t *testing.T) {
	lab := newTrapLab(t, 10*time.Millisecond)
	lab.arm(t, `{"hook": "payment.create", "match": {"beneficiary_id": "MID001"}, "times": null,
		"effect": {"path": [{"to": "B4BTMPending"}]}}`)
	other := lab.pay(t, "MID002")
	trapped := lab.pay(t, "MID001")

	waitFor(t, "the default walk and the bridge", func() bool { return len(lab.events(other)) == 5 })
	if got, want := lab.callbacks(other), states(b4b.StateAccepted, b4b.StateSanctionsPending, b4b.StateSanctionsApproved, b4b.StateTMPending, b4b.StateTMApproved); !slices.Equal(got, want) {
		t.Fatalf("callbacks for the other payee = %v, want %v", got, want)
	}
	if lab.bc.count() != 1 || !strings.Contains(lab.bc.received()[0], `"externalRef":"stl_MID002_1"`) {
		t.Fatalf("bridge calls = %v, want the other payee's one", lab.bc.received())
	}
	for status, e := range lab.events(other) {
		if _, ok := e.Detail["trap"]; ok {
			t.Errorf("the untrapped payment's %s callback is marked: %v", status, e.Detail)
		}
	}
	if _, ok := lab.created(t, other).Detail["trap"]; ok {
		t.Error("the untrapped payment's payment.create is marked")
	}
	waitFor(t, "the trapped one at TM review", func() bool { return len(lab.events(trapped)) == 4 })
	time.Sleep(30 * time.Millisecond)
	if st := lab.status(t, trapped); st != string(b4b.StateTMPending) || lab.bc.count() != 1 {
		t.Fatalf("the trapped payment is %s with %d bridge call(s)", st, lab.bc.count())
	}
}

// B4B_FORCE_FAILURE_BENEFICIARY_IDS is standing traps now, with the same
// result it always had: the five callbacks, ending B4BFailed where
// B4BTMApproved would have been, and no bridge call. A reset arms them
// again.
func TestForcedFailureBeneficiariesAreStandingTraps(t *testing.T) {
	lab := newTrapLab(t, 10*time.Millisecond, "MID001")
	standing := lab.traps(t)
	if len(standing) != 1 || !standing[0].Standing || standing[0].State != trap.StateArmed || standing[0].Times != nil ||
		standing[0].Label != "B4B_FORCE_FAILURE_BENEFICIARY_IDS MID001" {
		t.Fatalf("traps at start = %+v", standing)
	}

	forced := func(when string) {
		t.Helper()
		id := lab.pay(t, "MID001")
		waitFor(t, when+": the forced failure", func() bool { return len(lab.events(id)) == 5 })
		if got, want := lab.callbacks(id), states(b4b.StateAccepted, b4b.StateSanctionsPending, b4b.StateSanctionsApproved, b4b.StateTMPending, b4b.StateFailed); !slices.Equal(got, want) {
			t.Fatalf("%s: callbacks = %v, want %v", when, got, want)
		}
		if st := lab.status(t, id); st != string(b4b.StateFailed) {
			t.Fatalf("%s: status = %s", when, st)
		}
	}
	forced("at start")
	paid := lab.pay(t, "MID002")
	waitFor(t, "the other payee's bridge call", func() bool { return lab.bc.count() == 1 && len(lab.events(paid)) == 5 })

	st, b := send(t, lab.l.control, "POST", "/sim/reset", `{"scope": "flow"}`, "")
	res := object(t, b)
	if st != 200 || !strings.Contains(fmt.Sprint(res["kept"]), "force_failure_beneficiary_ids") ||
		res["cleared"].(map[string]any)["traps"] != float64(0) {
		t.Fatalf("reset = %d %s", st, b)
	}
	after := lab.traps(t)
	if len(after) != 1 || !after[0].Standing || after[0].State != trap.StateArmed || after[0].ID == standing[0].ID || len(after[0].Caught) != 0 {
		t.Fatalf("traps after the reset = %+v, want the standing one armed afresh", after)
	}
	forced("after a reset")
	if lab.bc.count() != 1 {
		t.Fatalf("%d bridge calls, want only the other payee's", lab.bc.count())
	}
	idle(t, lab.a)
}

// A reset in the middle of a trap's walk drops the rest of it: no further
// hop, no callback, no bridge call, and nothing left in the traps log.
func TestAResetMidWalkDropsTheRestOfATrapsPath(t *testing.T) {
	lab := newTrapLab(t, 40*time.Millisecond)
	trapID := lab.arm(t, `{"hook": "payment.create", "match": {"beneficiary_id": "MID001"}, "effect": {"path": [{"to": "B4BTMApproved"}]}}`)
	id := lab.pay(t, "MID001")
	waitFor(t, "the first hop's callback, and the next hop waiting", func() bool {
		return len(lab.events(id)) == 2 && len(lab.trap(t, trapID).PendingSteps) == 1
	})

	st, b := send(t, lab.l.control, "POST", "/sim/reset", `{"scope": "flow"}`, "")
	cleared, _ := object(t, b)["cleared"].(map[string]any)
	if st != 200 || cleared["traps"] != float64(1) || cleared["trap_steps"] != float64(1) || cleared["payments_in_flight"] != float64(1) {
		t.Fatalf("reset = %d %s, want the trap, its next hop and the payment", st, b)
	}
	callbacks := lab.cb.count()
	time.Sleep(200 * time.Millisecond) // five hops' worth
	idle(t, lab.a)
	if lab.cb.count() != callbacks || lab.bc.count() != 0 {
		t.Fatalf("after the reset: %d more callback(s), %d bridge call(s); want none", lab.cb.count()-callbacks, lab.bc.count())
	}
	if n := len(lab.traps(t)); n != 0 {
		t.Errorf("%d trap(s) after the reset", n)
	}
	if ev := lab.a.traps.Log().Recent(0); len(ev) != 0 {
		t.Errorf("the traps log after the reset: %+v", ev)
	}
	if st, _ := send(t, lab.l.vendor, "GET", "/oversight/v1/payments/"+id, "", lab.l.token); st != 404 {
		t.Errorf("the payment after the reset = %d, want 404", st)
	}
}

// After a restart the traps are gone, and a payment one of them was walking
// is not walked the default way. A standing trap, armed again at start,
// takes its payment on from where it stopped -- the forced failure still
// fails -- and any other stays where it stopped.
func TestARestartHandsTrappedPaymentsOnlyToTheStandingTraps(t *testing.T) {
	lab := newTrapLab(t, 10*time.Millisecond, "MID001")
	url := lab.cb.srv.URL
	lab.a.resumeHeld(lab.a.engine.Restore(b4b.State{Payments: []*b4b.Payment{
		{ID: "b4bp_forced", BeneficiaryID: "MID001", CallbackURL: url, State: b4b.StateSanctionsApproved, Trap: "trp_0007"},
		{ID: "b4bp_hung", BeneficiaryID: "MID002", CallbackURL: url, State: b4b.StateTMPending, Trap: "trp_0003"},
	}}))

	waitFor(t, "the forced failure, resumed", func() bool { return len(lab.events("b4bp_forced")) == 2 })
	if got, want := lab.callbacks("b4bp_forced"), states(b4b.StateTMPending, b4b.StateFailed); !slices.Equal(got, want) {
		t.Fatalf("callbacks = %v, want only the transitions not yet made: %v", got, want)
	}
	standing := lab.traps(t)[0]
	if len(standing.Caught) != 1 || standing.Caught[0].Subject != "b4bp_forced" {
		t.Fatalf("standing trap = %+v", standing)
	}
	if e := lab.events("b4bp_forced")[string(b4b.StateFailed)]; e.Detail["trap"] != standing.ID {
		t.Errorf("the resumed failure's activity names trap %q, want %s", e.Detail["trap"], standing.ID)
	}
	time.Sleep(50 * time.Millisecond)
	if got := lab.callbacks("b4bp_hung"); len(got) != 0 {
		t.Fatalf("a payment whose trap went with the process moved: %v", got)
	}
	if p, _ := lab.a.engine.Get("b4bp_hung"); p.State != b4b.StateTMPending || lab.bc.count() != 0 {
		t.Fatalf("held payment = %s, %d bridge call(s)", p.State, lab.bc.count())
	}
}

// walkPath turns a path into the hops b4b takes, the step's own wait on
// its first hop and the processing delay on every other.
func TestWalkPathIsOneStepPerHop(t *testing.T) {
	hops, err := walkPath([]trap.Step{
		{To: "B4BSanctionsApproved"},
		{To: "B4BFailed", After: "5s", Reason: "tm alert"},
	}, b4b.StateAccepted, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	want := []trap.Step{
		{To: "B4BSanctionsPending", After: "300ms"},
		{To: "B4BSanctionsApproved", After: "300ms"},
		{To: "B4BFailed", After: "5s", Reason: "tm alert"},
	}
	if !slices.Equal(hops, want) {
		t.Errorf("hops = %+v\nwant %+v", hops, want)
	}
	lab, _ := walkPath([]trap.Step{{To: "B4BTMApproved", AfterLab: "2h"}}, b4b.StateSanctionsApproved, time.Second)
	if want := []trap.Step{{To: "B4BTMPending", AfterLab: "2h"}, {To: "B4BTMApproved", After: "1s"}}; !slices.Equal(lab, want) {
		t.Errorf("after_lab: %+v", lab)
	}
	if same, err := walkPath([]trap.Step{{To: "B4BTMPending"}, {To: "B4BFailed"}}, b4b.StateTMPending, time.Second); err != nil ||
		!slices.Equal(same, []trap.Step{{To: "B4BFailed", After: "1s"}}) {
		t.Errorf("from where the first step goes: %+v, %v", same, err)
	}
	if _, err := walkPath([]trap.Step{{To: "B4BSanctionsPending"}}, b4b.StateTMPending, time.Second); err == nil {
		t.Error("a step behind the payment was walked")
	}
}
