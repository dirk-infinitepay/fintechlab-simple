// Creditsafe simulates the Creditsafe Connect API the platform calls for UK
// bank-account verification, company search and reports, and monitoring
// portfolios. Every check passes until an outcome is flipped: bank-account
// (pass, mismatch, invalid) and company (active, dissolved). A test registers
// a company at POST /sim/creditsafe/companies so its report carries what the
// platform compares (docs/api/services/creditsafe.md).
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
	"github.com/webduvet/fintechlab-simple/internal/vendorsim"
)

const (
	checkBankAccount = "bank-account"
	checkCompany     = "company"
)

// company is what a test registers about a company, served in its report so
// the platform's comparisons have something to compare.
type company struct {
	Country                       string   `json:"country"`
	RegNo                         string   `json:"regNo"`
	BusinessName                  string   `json:"businessName"`
	RegistrationDate              string   `json:"registrationDate"`
	Address                       string   `json:"address"`
	Directors                     []person `json:"directors"`
	Shareholders                  []person `json:"shareholders"`
	PersonsWithSignificantControl []person `json:"personsWithSignificantControl"`
}

// person is one director, shareholder or person with significant control,
// served as registered: a name and any of Creditsafe's own fields.
type person map[string]any

func (p person) named() bool { return vendorsim.Str(p["name"]) != "" }

func companyID(country, regNo string) string { return country + "-0-" + regNo }

// envState is one environment's Creditsafe: its registered companies and its
// monitoring portfolio counter.
type envState struct {
	companies  map[string]company
	portfolios int64
}

type app struct {
	sim *vendorsim.Sim

	mu   sync.Mutex
	envs map[string]*envState
}

func newApp(addr string) (*app, error) {
	a := &app{envs: map[string]*envState{}}
	sim, err := vendorsim.New(vendorsim.Config{
		Service: "creditsafe", Simulates: "Creditsafe",
		Checks: []vendorsim.Check{
			{Name: checkBankAccount, Outcomes: []string{"pass", "mismatch", "invalid"}},
			{Name: checkCompany, Outcomes: []string{"active", "dissolved"}},
		},
		Kept:   []string{"portfolios"},
		Reset:  a.reset,
		Forget: a.forget,
	}, addr)
	if err != nil {
		return nil, err
	}
	a.sim = sim
	sim.Control("POST /sim/creditsafe/companies", a.registerCompany)
	sim.Vendor("POST /creditsafe/authenticate", a.authenticate)
	sim.Vendor("POST /creditsafe/localSolutions/GB/bankVerification/search", sim.Bearer(a.bankVerification))
	sim.Vendor("GET /creditsafe/companies", sim.Bearer(a.companySearch))
	sim.Vendor("GET /creditsafe/companies/{id}", sim.Bearer(a.companyReport))
	sim.Vendor("GET /creditsafe/monitoring/portfolios", sim.Bearer(a.portfolios))
	sim.Vendor("POST /creditsafe/monitoring/portfolios", sim.Bearer(a.createPortfolio))
	sim.Vendor("POST /creditsafe/monitoring/portfolios/{portfolioId}/companies", sim.Bearer(a.addPortfolioCompany))
	sim.Vendor("GET /creditsafe/monitoring/portfolios/{portfolioId}/companies", sim.Bearer(a.portfolioCompanies))
	sim.Vendor("PUT /creditsafe/monitoring/portfolios/{portfolioId}/eventRules/{countryCode}", sim.Bearer(a.eventRules))
	sim.Vendor("GET /creditsafe/monitoring/notificationEvents", sim.Bearer(a.events))
	return a, nil
}

func main() {
	addr := vendorsim.Listen(":8091")
	a, err := newApp(addr)
	if err != nil {
		log.Fatalf("creditsafe: %v", err)
	}
	a.sim.Run(addr)
}

func (a *app) in(env string) *envState {
	env = tenant.Normalize(env)
	s := a.envs[env]
	if s == nil {
		s = &envState{companies: map[string]company{}}
		a.envs[env] = s
	}
	return s
}

func (a *app) reset(scope string, all bool) map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	companies, portfolios := 0, 0
	for env, s := range a.envs {
		if !tenant.InScope(scope, env) {
			continue
		}
		companies += len(s.companies)
		s.companies = map[string]company{}
		if all {
			portfolios += int(s.portfolios)
			s.portfolios = 0
		}
	}
	cleared := map[string]int{"companies": companies}
	if all {
		cleared["portfolios"] = portfolios
	}
	return cleared
}

func (a *app) forget(env string) {
	a.mu.Lock()
	delete(a.envs, env)
	a.mu.Unlock()
}

// registerCompany is POST /sim/creditsafe/companies: the named environment's
// reports for this company carry these details.
func (a *app) registerCompany(w http.ResponseWriter, r *http.Request) {
	env := tenant.FromRequest(r)
	switch {
	case env == tenant.All:
		httputilx.Error(w, 400, "a company is registered in one environment, not in all of them")
		return
	case !a.sim.Known(env):
		httputilx.Error(w, 404, "no environment "+env+" at creditsafe")
		return
	}
	var c company
	if err := httputilx.ReadJSON(r, &c); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	if c.Country == "" || c.RegNo == "" {
		httputilx.Error(w, 400, "a company needs its country and regNo, as the platform searches by them")
		return
	}
	for _, people := range [][]person{c.Directors, c.Shareholders, c.PersonsWithSignificantControl} {
		for _, p := range people {
			if !p.named() {
				httputilx.Error(w, 400, "every director, shareholder and person with significant control needs a name")
				return
			}
		}
	}
	id := companyID(c.Country, c.RegNo)
	a.mu.Lock()
	a.in(env).companies[id] = c
	a.mu.Unlock()
	log.Printf("creditsafe: company %s registered in environment %s", id, env)
	httputilx.WriteJSON(w, 200, map[string]any{"env": env, "id": id, "company": c})
}

func (a *app) authenticate(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	username, password := vendorsim.Str(body["username"]), vendorsim.Str(body["password"])
	if username == "" || password == "" {
		httputilx.Error(w, 401, "username and password required")
		return
	}
	env, ok := a.sim.ByLogin(username, password)
	if !ok {
		httputilx.Error(w, 401, "invalid username or password")
		return
	}
	a.sim.As(r, env)
	httputilx.WriteJSON(w, 200, map[string]any{"token": a.sim.Issue("cs", env)})
}

// bankVerification answers the UK bank-account check. The client reads
// supplierResponse.result, nameMatchResult and accountTypeResult; the rest is
// echoed back.
func (a *app) bankVerification(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	result, nameMatch, accountType := true, "Full", true
	switch a.sim.Answer(r, checkBankAccount) {
	case "mismatch":
		nameMatch = "No Match"
	case "invalid":
		result, nameMatch, accountType = false, "No Match", false
	}
	httputilx.WriteJSON(w, 200, map[string]any{
		"supplierResponse": map[string]any{
			"result":            result,
			"nameMatchResult":   nameMatch,
			"accountTypeResult": accountType,
			"accountNumber":     body["accountNumber"],
			"sortCode":          body["sortCode"],
		},
		"request": body,
	})
}

func (a *app) companySearch(w http.ResponseWriter, r *http.Request) {
	country := r.URL.Query().Get("countries")
	regNo := r.URL.Query().Get("regNo")
	httputilx.WriteJSON(w, 200, map[string]any{
		"totalSize": 1,
		"companies": []map[string]any{{
			"id":      companyID(country, regNo),
			"country": country,
			"regNo":   regNo,
		}},
	})
}

func (a *app) companyReport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	companyStatus := map[string]any{"status": "Active"}
	if a.sim.Answer(r, checkCompany) == "dissolved" {
		companyStatus = map[string]any{"status": "NonActive", "providerStatus": "Dissolved"}
	}
	summary := map[string]any{
		"companyNumber": id,
		"companyStatus": companyStatus,
		"creditRating":  map[string]any{"commonValue": "A", "commonDescription": "Very Low Risk"},
	}
	report := map[string]any{
		"companySummary": summary,
		"creditScore": map[string]any{
			"currentCreditRating": map[string]any{"commonValue": "A", "commonDescription": "Very Low Risk", "pod": 0.5},
		},
		"negativeInformation": map[string]any{
			"ccjSummary": map[string]any{"numberOfExact": 0, "numberOfPossible": 0, "numberOfSatisfied": 0},
		},
	}
	a.mu.Lock()
	c, registered := a.in(tenant.Of(r.Context())).companies[id]
	a.mu.Unlock()
	if registered {
		summary["businessName"] = c.BusinessName
		summary["companyRegistrationNumber"] = c.RegNo
		summary["country"] = c.Country
		report["companyIdentification"] = map[string]any{
			"basicInformation": map[string]any{
				"registeredCompanyName":   c.BusinessName,
				"companyRegistrationDate": c.RegistrationDate,
				"contactAddress":          map[string]any{"simpleValue": c.Address},
			},
		}
		report["directors"] = map[string]any{"currentDirectors": orEmpty(c.Directors)}
		report["shareCapitalStructure"] = map[string]any{"shareHolders": orEmpty(c.Shareholders)}
		report["additionalInformation"] = map[string]any{
			"personsWithSignificantControl": map[string]any{"activePSC": orEmpty(c.PersonsWithSignificantControl)},
		}
	}
	httputilx.WriteJSON(w, 200, map[string]any{"companyId": id, "report": report})
}

func orEmpty(people []person) []person {
	if people == nil {
		return []person{}
	}
	return people
}

func (a *app) portfolios(w http.ResponseWriter, r *http.Request) {
	httputilx.WriteJSON(w, 200, map[string]any{
		"portfolios": []map[string]any{{"id": "sim-portfolio-1", "name": "Simulated portfolio"}},
	})
}

func (a *app) createPortfolio(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	isDefault, _ := body["isDefault"].(bool)
	a.mu.Lock()
	s := a.in(tenant.Of(r.Context()))
	s.portfolios++
	id := s.portfolios
	a.mu.Unlock()
	httputilx.WriteJSON(w, 200, map[string]any{
		"portfolioId": id,
		"name":        vendorsim.Str(body["name"]),
		"isDefault":   isDefault,
	})
}

func (a *app) addPortfolioCompany(w http.ResponseWriter, r *http.Request) {
	httputilx.WriteJSON(w, 201, map[string]any{"message": "Company added to portfolio"})
}

func (a *app) portfolioCompanies(w http.ResponseWriter, r *http.Request) {
	httputilx.WriteJSON(w, 200, map[string]any{"data": []any{}, "totalCount": 0})
}

func (a *app) eventRules(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// events answers the monitoring poll with nothing to report: an event this
// lab invented would send the platform chasing a company that did not change.
func (a *app) events(w http.ResponseWriter, r *http.Request) {
	httputilx.WriteJSON(w, 200, map[string]any{"data": []any{}, "totalCount": 0})
}
