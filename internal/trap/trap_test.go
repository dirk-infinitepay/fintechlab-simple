package trap

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A Banking Circle-shaped hook: every step a direct successor.
func payoutHook() Hook {
	return Hook{
		Name: "payment.outgoing", About: "a payout", Subject: "payment_id", Effect: EffectPath,
		Match: []Field{{Name: "creditor.iban", Type: TypeIBAN}, {Name: "amount", Type: TypeDecimal}, {Name: "currency", Type: TypeString}},
		Graph: &Graph{
			Start: "Booked",
			Next:  map[string][]string{"Booked": {"Processed", "Rejected", "MissingFunding"}, "Processed": {"Reversed", "Returned"}},
			Params: map[string][]string{"Rejected": {"reason_code", "reason_description"}, "Reversed": {"reason"},
				"Returned": {"reason_code", "reason_description"}},
		},
		FirstAfter: 10 * time.Millisecond,
	}
}

// A B4B-shaped hook: a step may name a state further along.
func paymentHook() Hook {
	return Hook{
		Name: "payment.create", Subject: "payment_id", Effect: EffectPath,
		Match: []Field{{Name: "beneficiary_id", Type: TypeString}},
		Graph: &Graph{
			Start: "Accepted", Walks: true,
			Next: map[string][]string{"Accepted": {"SanctionsPending"}, "SanctionsPending": {"SanctionsApproved", "Failed"},
				"SanctionsApproved": {"TMPending", "Failed"}, "TMPending": {"TMApproved", "Failed"}},
			Params: map[string][]string{"Failed": {"reason"}},
		},
	}
}

func newStore() *Store { return NewStore("test-sim", payoutHook(), paymentHook()) }

func payout(iban, amount string) Event {
	return Event{Hook: "payment.outgoing", Subject: "bcp_" + iban, Fields: map[string]string{"creditor.iban": iban, "amount": amount, "currency": "EUR"}}
}

func TestArmingRefusesWhatCouldNotWork(t *testing.T) {
	s := newStore()
	for name, spec := range map[string]Spec{
		"unknown hook":           {Hook: "nope", Match: map[string]any{}, Path: []Step{}},
		"unknown field":          {Hook: "payment.outgoing", Match: map[string]any{"iban": "X"}, Path: []Step{}},
		"empty exact value":      {Hook: "payment.outgoing", Match: map[string]any{"currency": ""}, Path: []Step{}},
		"bad object":             {Hook: "payment.outgoing", Match: map[string]any{"currency": map[string]any{"like": "E"}}, Path: []Step{}},
		"range on a string":      {Hook: "payment.outgoing", Match: map[string]any{"currency": Between("1", "")}, Path: []Step{}},
		"no path":                {Hook: "payment.outgoing", Match: map[string]any{}},
		"an edge that is not":    {Hook: "payment.outgoing", Match: map[string]any{}, Path: []Step{{To: "Rejected"}, {To: "Reversed"}}},
		"a skip without walking": {Hook: "payment.outgoing", Match: map[string]any{}, Path: []Step{{To: "Reversed"}}},
		"a param it does not take": {Hook: "payment.outgoing", Match: map[string]any{},
			Path: []Step{{To: "Processed", Reason: "x"}}},
		"both waits": {Hook: "payment.outgoing", Match: map[string]any{}, Path: []Step{{To: "Processed", After: "1s", AfterLab: "1h"}}},
		"bad wait":   {Hook: "payment.outgoing", Match: map[string]any{}, Path: []Step{{To: "Processed", After: "soon"}}},
	} {
		if _, err := s.Arm(spec); err == nil || !IsInvalid(err) {
			t.Errorf("%s: armed (%v)", name, err)
		}
	}
	_, err := s.Arm(Spec{Hook: "payment.outgoing", Match: map[string]any{}, Path: []Step{{To: "Rejected"}, {To: "Reversed"}}})
	if !strings.Contains(err.Error(), "Reversed cannot follow Rejected") {
		t.Errorf("the refusal should name the edge: %v", err)
	}
}

func TestMatchingComparesLikeTheVendorWould(t *testing.T) {
	cases := []struct {
		match any
		value string
		want  bool
	}{
		{"GB00 SIM0 0000 0000 0777", "gb00sim0000000000777", true}, // IBANs normalised
		{In("GB01", "GB02"), "GB02", true},
		{In("GB01", "GB02"), "GB03", false},
		{Prefix("GB00SIM00000000007"), "GB00SIM0000000000777", true},
	}
	for i, c := range cases {
		s := newStore()
		if _, err := s.Arm(Spec{Hook: "payment.outgoing", Match: map[string]any{"creditor.iban": c.match}, Path: []Step{}}); err != nil {
			t.Fatal(err)
		}
		if _, got := s.Offer(payout(c.value, "1")); got != c.want {
			t.Errorf("case %d: %v against %q = %v", i, c.match, c.value, got)
		}
	}
	for _, c := range []struct {
		match any
		value string
		want  bool
	}{{"10", "10.00", true}, {Between("100.00", "500.00"), "250", true}, {Between("100.00", "500.00"), "99.99", false}, {Between("", "5"), "5.00", true}} {
		s := newStore()
		if _, err := s.Arm(Spec{Hook: "payment.outgoing", Match: map[string]any{"amount": c.match}, Path: []Step{}}); err != nil {
			t.Fatal(err)
		}
		if _, got := s.Offer(payout("X", c.value)); got != c.want {
			t.Errorf("amount %v against %q = %v", c.match, c.value, got)
		}
	}
}

func TestTheFirstArmedTakesTheEventAndOnlyThatOne(t *testing.T) {
	s := newStore()
	a, _ := s.Arm(Spec{Label: "first", Hook: "payment.outgoing", Match: map[string]any{}, Path: []Step{{To: "Rejected"}}})
	b, _ := s.Arm(Spec{Label: "second", Hook: "payment.outgoing", Match: map[string]any{}, Path: []Step{}})
	c1, _ := s.Offer(payout("A", "1"))
	c2, _ := s.Offer(payout("B", "1"))
	_, third := s.Offer(payout("C", "1"))
	if c1.TrapID != a.ID || c2.TrapID != b.ID || third {
		t.Fatalf("caught by %s, %s, then %v: the next two payouts are two traps armed one after the other", c1.TrapID, c2.TrapID, third)
	}
	if got, _ := s.Get(a.ID); got.State != StateSpent || len(got.Caught) != 1 || got.Caught[0].Subject != "bcp_A" {
		t.Errorf("first trap after one catch: %+v", got)
	}
}

func TestTimesNullTakesEveryEventAndExpiryStopsIt(t *testing.T) {
	s := newStore()
	forever, _ := s.Arm(Spec{Hook: "payment.outgoing", Match: map[string]any{}, Forever: true, Path: []Step{}})
	for i := 0; i < 5; i++ {
		if c, ok := s.Offer(payout(fmt.Sprint(i), "1")); !ok || c.TrapID != forever.ID {
			t.Fatalf("event %d not taken", i)
		}
	}
	if got, _ := s.Get(forever.ID); got.State != StateArmed || got.Times != nil {
		t.Errorf("a forever trap = %+v", got)
	}
	s2 := newStore()
	now := time.Now()
	s2.Wall = func() time.Time { return now }
	past := now.Add(-time.Second)
	s2.Arm(Spec{Hook: "payment.outgoing", Match: map[string]any{}, ExpiresAt: &past, Path: []Step{}})
	if _, ok := s2.Offer(payout("A", "1")); ok {
		t.Error("an expired trap took an event")
	}
	if l := s2.List(StateExpired); len(l) != 1 {
		t.Errorf("expired traps listed: %d", len(l))
	}
}

// applied records the steps a sim's engine was asked to take.
type applied struct {
	mu    sync.Mutex
	steps []string
	done  chan string
}

func newApplied() *applied { return &applied{done: make(chan string, 16)} }

func (a *applied) apply(st Step) error {
	a.mu.Lock()
	a.steps = append(a.steps, st.To)
	a.mu.Unlock()
	a.done <- st.To
	return nil
}

func waitFor(t *testing.T, ch chan string, want string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("applied %s, want %s", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("%s was never applied", want)
	}
}

func TestAPathIsTakenStepByStepAfterItsWaits(t *testing.T) {
	s := newStore()
	tr, _ := s.Arm(Spec{Hook: "payment.outgoing", Match: map[string]any{}, Path: []Step{{To: "Processed"}, {To: "Reversed", After: "30ms", Reason: "scheme"}}})
	c, _ := s.Offer(payout("A", "1"))
	a := newApplied()
	start := time.Now()
	c.Run(a.apply)
	waitFor(t, a.done, "Processed")
	if el := time.Since(start); el < 10*time.Millisecond {
		t.Errorf("first step after %v: it waits the hook's normal delay when it names none", el)
	}
	waitFor(t, a.done, "Reversed")
	if el := time.Since(start); el < 40*time.Millisecond {
		t.Errorf("second step after %v, want its 30ms after the first", el)
	}
	got, _ := s.Get(tr.ID)
	if len(got.Caught[0].Applied) != 2 || len(got.PendingSteps) != 0 {
		t.Errorf("after the path: %+v", got)
	}
}

func TestALabTimeStepWaitsForTheLabClock(t *testing.T) {
	s := newStore()
	var mu sync.Mutex
	lab := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	s.LabNow = func() time.Time { mu.Lock(); defer mu.Unlock(); return lab }
	s.LabPoll = 2 * time.Millisecond
	s.Arm(Spec{Hook: "payment.outgoing", Match: map[string]any{}, Path: []Step{{To: "Processed", After: "0s"}, {To: "Returned", AfterLab: "48h"}}})
	c, _ := s.Offer(payout("A", "1"))
	a := newApplied()
	c.Run(a.apply)
	waitFor(t, a.done, "Processed")
	select {
	case got := <-a.done:
		t.Fatalf("%s applied with the lab clock still", got)
	case <-time.After(30 * time.Millisecond):
	}
	if _, pending := s.Counts(); pending != 1 {
		t.Errorf("pending = %d, want the Returned step", pending)
	}
	mu.Lock()
	lab = lab.Add(49 * time.Hour) // the clock is moved, not waited out
	mu.Unlock()
	waitFor(t, a.done, "Returned")
}

func TestDisarmingCancelsWhatIsStillToCome(t *testing.T) {
	s := newStore()
	tr, _ := s.Arm(Spec{Hook: "payment.outgoing", Match: map[string]any{}, Path: []Step{{To: "Processed", After: "0s"}, {To: "Reversed", After: "1h"}}})
	c, _ := s.Offer(payout("A", "1"))
	a := newApplied()
	c.Run(a.apply)
	waitFor(t, a.done, "Processed")
	time.Sleep(5 * time.Millisecond) // the Reversed step is now pending
	got, cancelled, ok := s.Disarm(tr.ID)
	if !ok || len(cancelled) != 1 || cancelled[0].Step.To != "Reversed" || got.State != StateDisarmed {
		t.Fatalf("disarm = %+v, cancelled %+v", got, cancelled)
	}
	select {
	case to := <-a.done:
		t.Fatalf("%s applied after the trap was disarmed", to)
	case <-time.After(20 * time.Millisecond):
	}
	if got, _ := s.Get(tr.ID); !got.Caught[0].Cancelled {
		t.Errorf("the catch should say its path was cancelled: %+v", got.Caught[0])
	}
}

func TestADivertedStepEndsThePath(t *testing.T) {
	s := newStore()
	tr, _ := s.Arm(Spec{Hook: "payment.outgoing", Match: map[string]any{}, Path: []Step{{To: "Processed", After: "0s"}, {To: "Reversed"}}})
	c, _ := s.Offer(payout("A", "1"))
	calls := make(chan string, 4)
	c.Run(func(st Step) error {
		calls <- st.To
		return fmt.Errorf("%w: safeguarding balance 0.00 short of 1.00", ErrDiverted)
	})
	waitFor(t, calls, "Processed")
	select {
	case to := <-calls:
		t.Fatalf("%s tried after the path was diverted", to)
	case <-time.After(20 * time.Millisecond):
	}
	got, _ := s.Get(tr.ID)
	if !strings.Contains(got.Caught[0].Diverted, "short of") {
		t.Errorf("catch = %+v", got.Caught[0])
	}
	if e := s.Log().Recent(1); len(e) != 1 || e[0].Op != "trap.diverted" || e[0].Status != "warn" {
		t.Errorf("last traps event = %+v", e)
	}
}

func TestAResetForgetsTrapsButArmsTheStandingOnesAgain(t *testing.T) {
	s := newStore()
	s.ArmStanding(Spec{Label: "force failure ben_9", Hook: "payment.create", Match: map[string]any{"beneficiary_id": "ben_9"}, Forever: true,
		Path: []Step{{To: "TMPending"}, {To: "Failed"}}})
	s.Arm(Spec{Hook: "payment.outgoing", Match: map[string]any{}, Path: []Step{{To: "Processed", After: "1h"}}})
	c, _ := s.Offer(payout("A", "1"))
	ran := make(chan string, 1)
	c.Run(func(st Step) error { ran <- st.To; return nil })
	time.Sleep(5 * time.Millisecond)
	traps, pending := s.Reset()
	if traps != 1 || pending != 1 {
		t.Errorf("reset dropped %d trap(s) and %d step(s), want 1 and 1", traps, pending)
	}
	select {
	case to := <-ran:
		t.Fatalf("%s applied after a reset", to)
	case <-time.After(20 * time.Millisecond):
	}
	left := s.List("")
	if len(left) != 1 || !left[0].Standing || left[0].State != StateArmed || len(left[0].Caught) != 0 {
		t.Fatalf("after reset: %+v", left)
	}
	if _, ok := s.Offer(Event{Hook: "payment.create", Subject: "p1", Fields: map[string]string{"beneficiary_id": "ben_9"}}); !ok {
		t.Error("the standing trap does not catch after a reset")
	}
}

func TestAWalkingGraphTakesAStateFurtherAlong(t *testing.T) {
	s := newStore()
	if _, err := s.Arm(Spec{Hook: "payment.create", Match: map[string]any{}, Path: []Step{{To: "TMPending"}}}); err != nil {
		t.Errorf("hang at TM review: %v", err)
	}
	if _, err := s.Arm(Spec{Hook: "payment.create", Match: map[string]any{}, Path: []Step{{To: "SanctionsPending"}, {To: "Failed", After: "5s", Reason: "sanctions hit"}}}); err != nil {
		t.Errorf("fail at sanctions: %v", err)
	}
	if _, err := s.Arm(Spec{Hook: "payment.create", Match: map[string]any{}, Path: []Step{{To: "TMApproved"}, {To: "Failed"}}}); err == nil {
		t.Error("Failed after TMApproved was armed")
	}
}

func TestTheRoutesArmListAndDisarm(t *testing.T) {
	s := newStore()
	mux := http.NewServeMux()
	s.Routes(mux)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}
	w := do("POST", "/sim/traps", `{"label":"merchant 7 rejected","hook":"payment.outgoing",
		"match":{"creditor.iban":"GB00SIM0000000000777"},
		"effect":{"path":[{"to":"Rejected","reason_code":"AC01","reason_description":"Incorrect account number"}]}}`)
	if w.Code != 201 {
		t.Fatalf("arm = %d %s", w.Code, w.Body)
	}
	var armed Trap
	_ = json.Unmarshal(w.Body.Bytes(), &armed)
	if armed.ID == "" || armed.Times == nil || *armed.Times != 1 || armed.State != StateArmed {
		t.Errorf("armed = %+v: times defaults to 1", armed)
	}
	for body, want := range map[string]string{
		`{"hook":"payment.outgoing","match":{},"effect":{"path":[]},"times":null}`:                  "",
		`{"hook":"payment.outgoing","match":{},"effect":{"path":[]},"times":0}`:                     "times",
		`{"hook":"payment.outgoing","match":{},"effect":{"path":[]},"tims":2}`:                      "unknown field",
		`{"hook":"payment.outgoing","match":{},"effect":{"path":[{"to":"Processed","aftr":"1s"}]}}`: "unknown field",
		`{"hook":"payment.outgoing","effect":{"path":[]}}`:                                          "match is required",
	} {
		w := do("POST", "/sim/traps", body)
		if want == "" && w.Code != 201 || want != "" && (w.Code != 400 || !strings.Contains(w.Body.String(), want)) {
			t.Errorf("%s = %d %s", body, w.Code, w.Body)
		}
	}
	if w := do("GET", "/sim/traps?state=armed", ""); w.Code != 200 || strings.Count(w.Body.String(), `"id":"trp_`) != 2 {
		t.Errorf("list = %d %s", w.Code, w.Body)
	}
	w = do("GET", "/sim/traps/hooks", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"first_step_default_after":"10ms"`) || !strings.Contains(w.Body.String(), `"start":"Booked"`) {
		t.Errorf("hooks = %d %s", w.Code, w.Body)
	}
	if w := do("DELETE", "/sim/traps/"+armed.ID, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"disarmed"`) {
		t.Errorf("disarm = %d %s", w.Code, w.Body)
	}
	if w := do("DELETE", "/sim/traps", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"disarmed":1`) {
		t.Errorf("disarm all = %d %s", w.Code, w.Body)
	}
	if w := do("GET", "/sim/traps/trp_9999", ""); w.Code != 404 {
		t.Errorf("unknown trap = %d", w.Code)
	}
}
