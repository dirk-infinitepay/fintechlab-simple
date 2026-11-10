package b4b

import (
	"github.com/webduvet/fintechlab-simple/internal/simctl"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// Boarding is what a flow reset keeps: the chain a platform walked once
// before it could pay anybody, and would otherwise have to walk again. A
// beneficiary keeps its screening and active/disabled status with it.
var Boarding = []string{"companies", "addresses", "people", "extended_profiles", "documents", "vibans", "beneficiaries"}

// Reset clears b4b's stores for scope, in every environment:
// ResetIn(scope, tenant.All, …).
func Reset(scope string, e *Engine, d *Directory, s *BeneficiaryStore) simctl.ResetResult {
	return ResetIn(scope, tenant.All, e, d, s)
}

// ResetIn clears env's records for scope (docs/lab-api/sim-control.md §6):
// one environment's, or every one's for tenant.All.
//
//   - flow drops the payments and keeps the boarding.
//   - all drops the boarding too: as on first start, for that environment.
//
// Call it inside e.Hold(env), with whatever else the reset clears, so no
// work begun before it outlives it. Nothing fires OnChange: the caller
// writes the state file once, after.
func ResetIn(scope, env string, e *Engine, d *Directory, s *BeneficiaryStore) simctl.ResetResult {
	payments, inFlight := e.ClearIn(env)
	res := simctl.ResetResult{Cleared: map[string]int{"payments": payments, "payments_in_flight": inFlight}}
	if scope != simctl.ScopeAll {
		res.Kept = append([]string(nil), Boarding...)
		return res
	}
	for kind, n := range d.ClearIn(env) {
		res.Cleared[kind] = n
	}
	res.Cleared["beneficiaries"] = s.ClearIn(env)
	return res
}

// Clear empties the directory, every environment's: ClearIn(tenant.All).
func (d *Directory) Clear() map[string]int { return d.ClearIn(tenant.All) }

// ClearIn drops what scope names -- one environment's companies, with
// everything hanging off them, and its uploads, or every environment's --
// and says how many of each kind went.
func (d *Directory) ClearIn(scope string) map[string]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	cleared := map[string]int{"companies": 0, "addresses": 0, "people": 0, "extended_profiles": 0, "documents": 0, "vibans": 0}
	// Each company's owner is read before any company goes: what hangs off
	// one belongs to its environment (a company the store lost is the
	// default's, as everything from before environments is).
	in := map[string]bool{}
	inScope := func(cid string) bool {
		v, ok := in[cid]
		if !ok {
			v = tenant.InScope(scope, d.ownerLocked(cid))
			in[cid] = v
		}
		return v
	}
	for cid, list := range d.addresses {
		if inScope(cid) {
			cleared["addresses"] += len(list)
			delete(d.addresses, cid)
		}
	}
	for cid, list := range d.vibans {
		if inScope(cid) {
			cleared["vibans"] += len(list)
			delete(d.vibans, cid)
		}
	}
	for cid := range d.extended {
		if inScope(cid) {
			cleared["extended_profiles"]++
			delete(d.extended, cid)
		}
	}
	for id, p := range d.people {
		if inScope(p.CompanyID) {
			cleared["people"]++
			delete(d.people, id)
		}
	}
	for cid := range d.peopleByCompany {
		if inScope(cid) {
			delete(d.peopleByCompany, cid)
		}
	}
	for id, doc := range d.documents {
		if tenant.InScope(scope, doc.Env) {
			cleared["documents"]++
			delete(d.documents, id)
		}
	}
	for id := range d.companies {
		if inScope(id) {
			cleared["companies"]++
			delete(d.companies, id)
		}
	}
	for key, c := range d.companiesByRef {
		if tenant.InScope(scope, c.Env) {
			delete(d.companiesByRef, key)
		}
	}
	return cleared
}

// Clear forgets every registered beneficiary, in every environment:
// ClearIn(tenant.All).
func (s *BeneficiaryStore) Clear() int { return s.ClearIn(tenant.All) }

// ClearIn forgets scope's registered beneficiaries, with their screening
// and status, and says how many there were. An id named afterwards
// auto-vivifies again, as on first start.
func (s *BeneficiaryStore) ClearIn(scope string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for key, b := range s.byID {
		if tenant.InScope(scope, b.Env) {
			n++
			delete(s.byID, key)
		}
	}
	for key, b := range s.byRef {
		if tenant.InScope(scope, b.Env) {
			delete(s.byRef, key)
		}
	}
	return n
}
