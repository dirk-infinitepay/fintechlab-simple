package b4b

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The whole point of the state file: a merchant boarded before a restart is
// still boarded after one, with the rules that depend on it intact.
func TestStateRoundTripKeepsBoarding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	dir := NewDirectory(nil)
	bens := NewBeneficiaryStore(nil)
	engine := NewEngine(0, nil)

	params := gbCompany()
	params.CompanyDocumentsWaived = true
	params.PersonDocumentsWaived = true
	c, err := dir.CreateCompany(params)
	if err != nil {
		t.Fatalf("create company: %v", err)
	}
	person, err := dir.AddPerson(c.ID, PersonParams{FirstName: "Ada", LastName: "Lovelace", DateOfBirth: "1815-12-10"})
	if err != nil {
		t.Fatalf("add person: %v", err)
	}
	prof, err := dir.CreateExtended(c.ID, ExtendedParams{RegisteredCompanyNo: "12345678"})
	if err != nil {
		t.Fatalf("create extended: %v", err)
	}
	if _, err := dir.AddViban(c.ID, "EUR"); err != nil {
		t.Fatalf("add viban: %v", err)
	}
	doc := dir.AddDocument(DocumentParams{FileName: "extract.pdf", Size: 42})
	ben, err := bens.Register(RegisterParams{
		CompanyID: c.ID, ExternalRef: "MID001", AccountName: "Harness Merchant Ltd",
		AccountNumber: "GB00SIM0000000000042", FinancialInstitution: "SC112233",
	})
	if err != nil {
		t.Fatalf("register beneficiary: %v", err)
	}

	var out State
	dir.Snapshot(&out)
	bens.Snapshot(&out)
	engine.Snapshot(&out)
	if err := SaveState(path, out); err != nil {
		t.Fatalf("save: %v", err)
	}

	// A fresh process.
	loaded, err := LoadState(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	dir2, bens2 := NewDirectory(nil), NewBeneficiaryStore(nil)
	dir2.Restore(loaded)
	bens2.Restore(loaded)

	if got, err := dir2.Company(c.ID); err != nil || got.LegalName != c.LegalName {
		t.Fatalf("company after restart = %+v, %v", got, err)
	}
	// By external_ref too: the index is rebuilt, not just the map.
	if got, err := dir2.Company("mer_0001"); err != nil || got.ID != c.ID {
		t.Fatalf("company by external_ref after restart = %+v, %v", got, err)
	}
	if got, err := dir2.Person(person.ID); err != nil || got.LastName != "Lovelace" {
		t.Fatalf("person after restart = %+v, %v", got, err)
	}
	if people, err := dir2.People(c.ID); err != nil || len(people) != 1 {
		t.Fatalf("people after restart = %d, %v; want 1", len(people), err)
	}
	if addrs, err := dir2.Addresses(c.ID); err != nil || len(addrs) != 1 {
		t.Fatalf("addresses after restart = %d, %v; want 1", len(addrs), err)
	}
	if vibans, err := dir2.Vibans(c.ID); err != nil || len(vibans) != 1 {
		t.Fatalf("vibans after restart = %d, %v; want 1", len(vibans), err)
	}
	if _, err := dir2.Document(doc.ID); err != nil {
		t.Fatalf("document after restart: %v", err)
	}
	if got := bens2.Get(ben.ID); got.CompanyID != c.ID || got.AccountNumber != ben.AccountNumber {
		t.Fatalf("beneficiary after restart = %+v", got)
	}

	// The rule that most needs the memory: the profile is still there, so
	// a re-posted one is still refused.
	if got, err := dir2.Extended(c.ID); err != nil || got.ID != prof.ID {
		t.Fatalf("extended after restart = %+v, %v", got, err)
	}
	if _, err := dir2.CreateExtended(c.ID, ExtendedParams{RegisteredCompanyNo: "12345678"}); err == nil {
		t.Fatal("a restart made a create-once endpoint creatable twice")
	}
}

func TestLoadStateMissingFileIsAFirstStart(t *testing.T) {
	s, err := LoadState(filepath.Join(t.TempDir(), "nothing.json"))
	if err != nil {
		t.Fatalf("missing state file: %v", err)
	}
	if len(s.Companies) != 0 || len(s.Payments) != 0 {
		t.Fatalf("missing state file returned data: %+v", s)
	}
	if err := SaveState("", State{}); err != nil {
		t.Fatalf("empty path should be a no-op, got %v", err)
	}
	if _, err := LoadState(""); err != nil {
		t.Fatalf("empty path should be a no-op, got %v", err)
	}
}

func TestSaveStateCreatesItsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "state.json")
	if err := SaveState(path, State{Documents: []*Document{{ID: "doc_1"}}}); err != nil {
		t.Fatalf("save into a missing directory: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file: %v", err)
	}
}

// A payment caught mid-lifecycle by a restart has to finish. Left as it
// was, it looks to a client exactly like a webhook that never arrived --
// and it would wait for one forever.
func TestEngineResumesUnfinishedPaymentsAfterRestart(t *testing.T) {
	seen := make(chan PaymentState, 8)
	engine := NewEngine(time.Millisecond, func(p *Payment, _ uint64) { seen <- p.State })

	engine.Restore(State{Payments: []*Payment{
		{ID: "b4bp_midflight", BeneficiaryID: "ben_1", State: StateSanctionsApproved},
		{ID: "b4bp_done", BeneficiaryID: "ben_2", State: StateTMApproved},
		{ID: "b4bp_failed", BeneficiaryID: "ben_3", State: StateFailed},
	}})

	// Only the unfinished one moves, and it picks up where it stopped
	// rather than replaying states the client was already told about.
	want := []PaymentState{StateTMPending, StateTMApproved}
	for _, w := range want {
		select {
		case got := <-seen:
			if got != w {
				t.Fatalf("resumed state = %s, want %s", got, w)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s", w)
		}
	}
	select {
	case extra := <-seen:
		t.Fatalf("a terminal payment was resumed too: got another %s", extra)
	case <-time.After(50 * time.Millisecond):
	}

	for _, id := range []string{"b4bp_midflight", "b4bp_done", "b4bp_failed"} {
		if _, err := engine.Get(id); err != nil {
			t.Errorf("payment %s not readable after restore: %v", id, err)
		}
	}
}

// A payment a trap caught was walking the trap's path, and the trap went
// with the process: it is not resumed the default way, but handed back for
// cmd/b4b to decide, and stays where it stopped until something moves it.
func TestEngineHoldsTrappedPaymentsAfterRestart(t *testing.T) {
	seen := make(chan PaymentState, 8)
	engine := NewEngine(time.Millisecond, func(p *Payment, _ uint64) { seen <- p.State })
	held := engine.Restore(State{Payments: []*Payment{
		{ID: "b4bp_hung", BeneficiaryID: "ben_1", State: StateTMPending, Trap: "trp_0001"},
		{ID: "b4bp_trapped_done", BeneficiaryID: "ben_2", State: StateFailed, Trap: "trp_0002", FailureReason: "sanctions hit"},
		{ID: "b4bp_plain", BeneficiaryID: "ben_3", State: StateTMPending},
	}})
	if len(held) != 1 || held[0].ID != "b4bp_hung" {
		t.Fatalf("held = %+v, want the unfinished trapped payment only", held)
	}
	if got := <-seen; got != StateTMApproved {
		t.Fatalf("the untrapped payment resumed to %s", got)
	}
	select {
	case extra := <-seen:
		t.Fatalf("a trapped payment was resumed the default way: %s", extra)
	case <-time.After(30 * time.Millisecond):
	}
	if p, _ := engine.Get("b4bp_hung"); p.State != StateTMPending {
		t.Errorf("held payment = %s", p.State)
	}
	if p, _ := engine.Get("b4bp_trapped_done"); p.FailureReason != "sanctions hit" {
		t.Errorf("failure reason after restore = %q", p.FailureReason)
	}
}
