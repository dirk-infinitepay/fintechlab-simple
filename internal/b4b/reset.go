package b4b

import "github.com/webduvet/fintechlab-simple/internal/simctl"

// Boarding is what a flow reset keeps: the chain a platform walked once
// before it could pay anybody, and would otherwise have to walk again. A
// beneficiary keeps its screening and active/disabled status with it.
var Boarding = []string{"companies", "addresses", "people", "extended_profiles", "documents", "vibans", "beneficiaries"}

// Reset clears b4b's stores for scope (docs/lab-api/sim-control.md §6).
//
//   - flow drops every payment and keeps the boarding.
//   - all drops the boarding too: the state file as on first start.
//
// Call it inside e.Gate().Hold, with whatever else the reset clears, so no
// work begun before it outlives it. Nothing fires OnChange: the caller
// writes the state file once, after.
func Reset(scope string, e *Engine, d *Directory, s *BeneficiaryStore) simctl.ResetResult {
	payments, inFlight := e.Clear()
	res := simctl.ResetResult{Cleared: map[string]int{"payments": payments, "payments_in_flight": inFlight}}
	if scope != simctl.ScopeAll {
		res.Kept = append([]string(nil), Boarding...)
		return res
	}
	for kind, n := range d.Clear() {
		res.Cleared[kind] = n
	}
	res.Cleared["beneficiaries"] = s.Clear()
	return res
}

// Clear empties the directory and says how many of each kind it held.
func (d *Directory) Clear() map[string]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	cleared := map[string]int{
		"companies":         len(d.companies),
		"addresses":         0,
		"people":            len(d.people),
		"extended_profiles": len(d.extended),
		"documents":         len(d.documents),
		"vibans":            0,
	}
	for _, list := range d.addresses {
		cleared["addresses"] += len(list)
	}
	for _, list := range d.vibans {
		cleared["vibans"] += len(list)
	}
	d.companies = map[string]*Company{}
	d.companiesByRef = map[string]*Company{}
	d.addresses = map[string][]*CompanyAddress{}
	d.people = map[string]*Person{}
	d.peopleByCompany = map[string][]string{}
	d.extended = map[string]*ExtendedProfile{}
	d.documents = map[string]*Document{}
	d.vibans = map[string][]*Viban{}
	return cleared
}

// Clear forgets every registered beneficiary, with its screening and
// status, and says how many there were. An id named afterwards
// auto-vivifies again, as on first start.
func (s *BeneficiaryStore) Clear() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.byID)
	s.byID = map[string]*Beneficiary{}
	s.byRef = map[string]*Beneficiary{}
	return n
}
