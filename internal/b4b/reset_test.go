package b4b

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/simctl"
)

// boarded is a directory and beneficiary store holding one of everything a
// platform boards: a company (with its address), a person, an extended
// profile, a document, a vIBAN and a beneficiary.
func boarded(t *testing.T) (*Directory, *BeneficiaryStore) {
	t.Helper()
	dir, bens := NewDirectory(nil), NewBeneficiaryStore(nil)
	params := gbCompany()
	params.CompanyDocumentsWaived = true
	params.PersonDocumentsWaived = true
	c, err := dir.CreateCompany(params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dir.AddPerson(c.ID, PersonParams{FirstName: "Ada", LastName: "Lovelace", DateOfBirth: "1815-12-10"}); err != nil {
		t.Fatal(err)
	}
	if _, err := dir.CreateExtended(c.ID, ExtendedParams{RegisteredCompanyNo: "12345678"}); err != nil {
		t.Fatal(err)
	}
	if _, err := dir.AddViban(c.ID, "EUR"); err != nil {
		t.Fatal(err)
	}
	dir.AddDocument(DocumentParams{FileName: "extract.pdf", Size: 42})
	if _, err := bens.Register(RegisterParams{CompanyID: c.ID, ExternalRef: "MID001", AccountName: "Harness Merchant Ltd",
		AccountNumber: "GB00SIM0000000000042", FinancialInstitution: "SC112233"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := bens.SetSanctions("MID001", SanctionsFail); err != nil {
		t.Fatal(err)
	}
	return dir, bens
}

func finishedPayments() State {
	return State{Payments: []*Payment{
		{ID: "b4bp_done", BeneficiaryID: "MID001", State: StateTMApproved},
		{ID: "b4bp_failed", BeneficiaryID: "MID001", State: StateFailed},
	}}
}

func reset(scope string, e *Engine, d *Directory, s *BeneficiaryStore) simctl.ResetResult {
	var res simctl.ResetResult
	e.Gate().Hold(func(int) { res = Reset(scope, e, d, s) })
	return res
}

// waitIdle waits for every payment the engine is walking to stop: the
// positive signal that work a reset dropped has seen the reset.
func waitIdle(t *testing.T, e *Engine) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for e.Walking() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d payment(s) still walking their phases", e.Walking())
		}
		time.Sleep(time.Millisecond)
	}
}

// A flow reset is the one between two runs: the payments go, and the
// boarding a platform did once -- with each beneficiary's screening -- is
// still there for the next run.
func TestFlowResetDropsPaymentsAndKeepsBoarding(t *testing.T) {
	dir, bens := boarded(t)
	engine := NewEngine(0, nil)
	engine.Restore(finishedPayments())

	res := reset(simctl.ScopeFlow, engine, dir, bens)

	if want := map[string]int{"payments": 2, "payments_in_flight": 0}; !reflect.DeepEqual(res.Cleared, want) {
		t.Errorf("cleared = %v, want %v", res.Cleared, want)
	}
	if !reflect.DeepEqual(res.Kept, Boarding) {
		t.Errorf("kept = %v, want %v", res.Kept, Boarding)
	}
	if len(engine.List()) != 0 {
		t.Errorf("payments after a flow reset: %d", len(engine.List()))
	}
	var s State
	dir.Snapshot(&s)
	bens.Snapshot(&s)
	engine.Snapshot(&s)
	if len(s.Payments) != 0 {
		t.Errorf("the state file would still hold %d payment(s)", len(s.Payments))
	}
	if len(s.Companies) != 1 || len(s.Addresses) != 1 || len(s.People) != 1 || len(s.Extended) != 1 ||
		len(s.Documents) != 1 || len(s.Vibans) != 1 || len(s.Beneficiaries) != 1 {
		t.Errorf("boarding after a flow reset: %d companies, %d addresses, %d people, %d extended, %d documents, %d vibans, %d beneficiaries",
			len(s.Companies), len(s.Addresses), len(s.People), len(s.Extended), len(s.Documents), len(s.Vibans), len(s.Beneficiaries))
	}
	if got := bens.Get("MID001"); got.SanctionsStatus != SanctionsFail {
		t.Errorf("a beneficiary's screening did not survive a flow reset: %s", got.SanctionsStatus)
	}
	if _, err := dir.Company("mer_0001"); err != nil {
		t.Errorf("company by external_ref after a flow reset: %v", err)
	}
}

// An all reset is a first start: the state file holds nothing, and the
// counts say what went.
func TestAllResetEmptiesTheStateFile(t *testing.T) {
	dir, bens := boarded(t)
	engine := NewEngine(0, nil)
	engine.Restore(finishedPayments())

	res := reset(simctl.ScopeAll, engine, dir, bens)

	want := map[string]int{
		"payments": 2, "payments_in_flight": 0,
		"companies": 1, "addresses": 1, "people": 1, "extended_profiles": 1, "documents": 1, "vibans": 1,
		"beneficiaries": 1,
	}
	if !reflect.DeepEqual(res.Cleared, want) {
		t.Errorf("cleared = %v, want %v", res.Cleared, want)
	}
	if len(res.Kept) != 0 {
		t.Errorf("kept = %v, want nothing", res.Kept)
	}

	path := filepath.Join(t.TempDir(), "state.json")
	var s State
	dir.Snapshot(&s)
	bens.Snapshot(&s)
	engine.Snapshot(&s)
	if err := SaveState(path, s); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, State{}) {
		t.Errorf("state after an all reset = %+v, want empty", loaded)
	}
	// As on first start: the external_ref index is gone too, and the
	// create-once profile can be created again.
	if _, err := dir.Company("mer_0001"); err == nil {
		t.Error("a company is still found by external_ref after an all reset")
	}
	if got := bens.Get("MID001"); got.SanctionsStatus != SanctionsPass {
		t.Errorf("an unknown beneficiary after an all reset auto-vivifies as %s, want pass", got.SanctionsStatus)
	}
}

// The rule that matters most: a payment still sleeping out its processing
// delay when the reset lands takes no further step -- no transition, so no
// callback and no bridge call is ever asked for.
func TestAResetDropsAPaymentWalkingItsPhases(t *testing.T) {
	events := make(chan Payment, 8)
	engine := NewEngine(50*time.Millisecond, func(p *Payment, _ uint64) { events <- *p })
	engine.Submit(&Payment{ID: "b4bp_walking", BeneficiaryID: "MID001"})
	if ev := <-events; ev.State != StateAccepted {
		t.Fatalf("first transition = %s, want %s", ev.State, StateAccepted)
	}

	res := reset(simctl.ScopeFlow, engine, NewDirectory(nil), NewBeneficiaryStore(nil))
	if res.Cleared["payments"] != 1 || res.Cleared["payments_in_flight"] != 1 {
		t.Fatalf("cleared = %v, want the one payment, still in flight", res.Cleared)
	}
	waitIdle(t, engine)

	select {
	case ev := <-events:
		t.Fatalf("a payment the reset dropped moved on to %s", ev.State)
	default:
	}
	if _, err := engine.Get("b4bp_walking"); err != ErrPaymentNotFound {
		t.Fatalf("Get after reset: %v, want ErrPaymentNotFound", err)
	}
}

// A payment resumed from the state file is work like any other: a reset
// drops it too.
func TestAResetDropsAResumedPayment(t *testing.T) {
	events := make(chan Payment, 8)
	engine := NewEngine(50*time.Millisecond, func(p *Payment, _ uint64) { events <- *p })
	engine.Restore(State{Payments: []*Payment{{ID: "b4bp_resumed", BeneficiaryID: "MID001", State: StateSanctionsPending}}})

	res := reset(simctl.ScopeFlow, engine, NewDirectory(nil), NewBeneficiaryStore(nil))
	if res.Cleared["payments_in_flight"] != 1 {
		t.Fatalf("cleared = %v, want the resumed payment in flight", res.Cleared)
	}
	waitIdle(t, engine)
	select {
	case ev := <-events:
		t.Fatalf("a resumed payment the reset dropped moved on to %s", ev.State)
	default:
	}
}

// A payment submitted after a reset walks as usual: the reset dropped the
// work before it, not the engine.
func TestAPaymentAfterAResetWalks(t *testing.T) {
	events := make(chan Payment, 8)
	engine := NewEngine(time.Millisecond, func(p *Payment, _ uint64) { events <- *p })
	reset(simctl.ScopeFlow, engine, NewDirectory(nil), NewBeneficiaryStore(nil))
	engine.Submit(&Payment{ID: "b4bp_after", BeneficiaryID: "MID001"})
	waitIdle(t, engine)
	if got, err := engine.Get("b4bp_after"); err != nil || got.State != StateTMApproved {
		t.Fatalf("payment after a reset = %+v, %v; want %s", got, err, StateTMApproved)
	}
}

func TestGateRefusesWorkBegunBeforeAReset(t *testing.T) {
	var g Gate
	before := g.Begin(nil)
	if !g.Do(before, func() {}) {
		t.Fatal("Do refused work with no reset since it began")
	}
	g.Hold(func(int) {})
	if g.Do(before, func() { t.Error("an effect ran for work begun before the reset") }) {
		t.Error("Do ran work begun before the reset")
	}
	after := g.Begin(nil)
	if !g.Do(after, func() {}) {
		t.Error("Do refused work begun after the reset")
	}
}

// A reset waits for an effect already on the wire, so when it answers
// nothing it dropped is still being sent.
func TestGateHoldWaitsForAnEffectUnderWay(t *testing.T) {
	var g Gate
	started := g.Begin(nil)
	inEffect, release := make(chan struct{}), make(chan struct{})
	go g.Do(started, func() {
		close(inEffect)
		<-release
	})
	<-inEffect

	held := make(chan struct{})
	go func() {
		g.Hold(func(int) {})
		close(held)
	}()
	select {
	case <-held:
		t.Fatal("the reset answered while an effect was still under way")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-held
	if g.Do(started, func() {}) {
		t.Error("work begun before the reset ran after it")
	}
}

// Hold counts the background work begun since the last reset -- what this
// reset drops -- and not work an earlier reset already dropped that is
// still waking up.
func TestGateCountsOnlyTheWorkThisResetDrops(t *testing.T) {
	var g Gate
	release := make(chan struct{})
	g.Go(g.Begin(nil), func(uint64) { <-release })

	var first, second int
	g.Hold(func(running int) { first = running })
	g.Hold(func(running int) { second = running })
	if first != 1 || second != 0 {
		t.Errorf("dropped by the first reset %d, by the second %d; want 1 and 0", first, second)
	}
	if g.Running() != 1 {
		t.Errorf("Running() = %d, want the dropped work until it returns", g.Running())
	}
	close(release)

	// Work begun before a reset is not started after it.
	stale := g.Begin(nil)
	g.Hold(func(int) {})
	g.Go(stale, func(uint64) { t.Error("work begun before a reset was started after it") })

	deadline := time.Now().Add(2 * time.Second)
	for g.Running() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("Running() = %d, want 0", g.Running())
		}
		time.Sleep(time.Millisecond)
	}
}
