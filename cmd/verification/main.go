// Verification simulates the four external checks the platform runs before
// it will move a merchant's money: bank-account verification, sanctions and
// adverse-media screening, and identity verification.
//
// # Why one process for four vendors
//
// Each is configured by its own base URL, so they could be four services.
// They are one because nothing here is vendor-specific except the shape of
// the answer: no state is shared, no vendor knows another exists, and each
// lives under its own path prefix. Four containers would cost four
// Dockerfiles to say the same thing.
//
// # What is faked and what is not
//
// The *contract* is real: the paths, the auth handshakes, the request
// bodies and the response shapes are what the platform's clients actually
// send and parse. The *judgement* is stubbed — every check passes.
//
// That is the honest version of a stub and it is the pattern this lab
// already uses for `verify`: a real trigger-then-read-decision contract
// with a decision that is declared fake. A mode that returned a bare 200
// and did nothing would be the failure "connected, not decorative" exists
// to prevent, because a client could pass against it while being wired
// wrongly.
//
// Every outcome is flippable at POST /sim/outcome on the control port, so
// the unhappy paths are reachable when somebody wants them. Defaulting to
// "pass" is a convenience, not a limitation.
//
// # Environments
//
// Each environment the lab created is a customer here (internal/tenant),
// known by its verification login: a username and password, where the
// username is the Creditsafe username and the LexisNexis client id, and the
// password is the Creditsafe password, the LexisNexis client secret, the
// KYC6 and iban.com API key and the LexisNexis IVI key. A call made with a
// customer's login is that environment's, and so is every token it was
// issued; any other login is the default environment's, as every login was
// before. Each environment has outcomes of its own, so one platform's flip
// never moves another's answers.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/logring"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// outcomes holds the answer each vendor currently gives. One lock, because
// an operator flipping a switch while a check is in flight should see the
// flip take effect on the next call rather than corrupt this one.
type outcomes struct {
	mu sync.RWMutex
	m  map[string]string
}

// The four checks, and the answers each can give. Names are the lab's, not
// any vendor's — the vendor-shaped values live in the handlers.
const (
	checkBankAccount = "bank-account" // pass | mismatch | invalid
	checkScreening   = "screening"    // clear | hit
	checkIdentity    = "identity"     // pass | refer | fail
	checkCompany     = "company"      // active | dissolved
)

func newOutcomes() *outcomes {
	return &outcomes{m: map[string]string{
		checkBankAccount: "pass",
		checkScreening:   "clear",
		checkIdentity:    "pass",
		checkCompany:     "active",
	}}
}

func (o *outcomes) get(check string) string {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.m[check]
}

func (o *outcomes) set(check, value string) error {
	allowed := map[string][]string{
		checkBankAccount: {"pass", "mismatch", "invalid"},
		checkScreening:   {"clear", "hit"},
		checkIdentity:    {"pass", "refer", "fail"},
		checkCompany:     {"active", "dissolved"},
	}
	values, known := allowed[check]
	if !known {
		return fmt.Errorf("unknown check %q (have: %s)", check, strings.Join(keys(allowed), ", "))
	}
	for _, v := range values {
		if v == value {
			o.mu.Lock()
			o.m[check] = value
			o.mu.Unlock()
			return nil
		}
	}
	return fmt.Errorf("check %q cannot be %q (want: %s)", check, value, strings.Join(values, ", "))
}

// reset puts every check back to its default and says how many had been
// flipped.
func (o *outcomes) reset() int {
	defaults := newOutcomes().m
	o.mu.Lock()
	defer o.mu.Unlock()
	flipped := 0
	for check, v := range defaults {
		if o.m[check] != v {
			flipped++
		}
	}
	o.m = defaults
	return flipped
}

func (o *outcomes) snapshot() map[string]string {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make(map[string]string, len(o.m))
	for k, v := range o.m {
		out[k] = v
	}
	return out
}

type app struct {
	// customers are the environments besides the default, each known by
	// its verification login.
	customers *tenant.Customers

	mu   sync.Mutex
	envs map[string]*envState

	// requests is the journal of every vendor request, for GET
	// /sim/activity: what the platform asked and what it was answered.
	requests *activity.Log

	// tokens are the bearer tokens this service has issued, each to the
	// environment whose login asked for it (an issued). Each vendor
	// authenticates differently and they are checked, because a client
	// that forgets its token should find out here rather than in
	// production.
	tokens sync.Map
}

// envState is one environment's part of the service: the answers its
// checks give, and its Creditsafe portfolio ids.
type envState struct {
	out          *outcomes
	portfolioIDs atomic.Int64
	companies    sync.Map
}

// creditsafeCompany is what a test registers about a company, served in its
// Creditsafe report so the platform's comparisons have something to compare.
type creditsafeCompany struct {
	Country                       string             `json:"country"`
	RegNo                         string             `json:"regNo"`
	BusinessName                  string             `json:"businessName"`
	RegistrationDate              string             `json:"registrationDate"`
	Address                       string             `json:"address"`
	Directors                     []creditsafePerson `json:"directors"`
	Shareholders                  []creditsafePerson `json:"shareholders"`
	PersonsWithSignificantControl []creditsafePerson `json:"personsWithSignificantControl"`
}

// creditsafePerson is one director, shareholder or person with significant
// control, served as registered: a name and any of Creditsafe's own fields,
// such as percentSharesHeld, shareholderType or countryOfResidence.
type creditsafePerson map[string]any

func (p creditsafePerson) named() bool {
	name, _ := p["name"].(string)
	return name != ""
}

func creditsafeCompanyID(country, regNo string) string { return country + "-0-" + regNo }

func (s *envState) forgetCompanies() int {
	n := 0
	s.companies.Range(func(k, _ any) bool {
		s.companies.Delete(k)
		n++
		return true
	})
	return n
}

// issued is a bearer token's record: whose it is, and when it was issued.
type issued struct {
	env string
	at  time.Time
}

func newApp() *app {
	a := &app{customers: tenant.NewCustomers(), envs: map[string]*envState{},
		requests: activity.New("requests", "Vendor requests",
			"Every request the platform made to Creditsafe, iban.com, KYC6 and LexisNexis, and the status it was answered with.")}
	a.customers.OnDelete = a.forget
	return a
}

// in is env's state, made on first use: every check at its default.
func (a *app) in(env string) *envState {
	env = tenant.Normalize(env)
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.envs[env]
	if s == nil {
		s = &envState{out: newOutcomes()}
		a.envs[env] = s
	}
	return s
}

// outcomes is what env's checks answer.
func (a *app) outcomes(env string) *outcomes { return a.in(env).out }

// answer is what check answers the caller: the environment its login
// named.
func (a *app) answer(r *http.Request, check string) string {
	return a.outcomes(tenant.Of(r.Context())).get(check)
}

// statesIn is every environment's state in scope that exists yet: one
// that never had any is at its defaults, with nothing to clear.
func (a *app) statesIn(scope string) []*envState {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*envState
	for env, s := range a.envs {
		if tenant.InScope(scope, env) {
			out = append(out, s)
		}
	}
	return out
}

// dropTokens forgets every token issued in scope and says how many.
func (a *app) dropTokens(scope string) int {
	n := 0
	a.tokens.Range(func(k, v any) bool {
		if tenant.InScope(scope, v.(issued).env) {
			a.tokens.Delete(k)
			n++
		}
		return true
	})
	return n
}

// forget is a customer's removal: its outcomes and its tokens go with it.
func (a *app) forget(env string) error {
	a.dropTokens(env)
	a.mu.Lock()
	delete(a.envs, env)
	a.mu.Unlock()
	return nil
}

// known reports whether env is one this service serves: the default, every
// environment ("*"), or a customer's.
func (a *app) known(env string) bool {
	if env == tenant.Default || env == tenant.All {
		return true
	}
	_, ok := a.customers.Get(env)
	return ok
}

func main() {
	logring.Install()
	addr := env("LISTEN", ":8089")
	a := newApp()
	mux, ctl, err := a.routes(addr)
	if err != nil {
		log.Fatalf("verification: %v", err)
	}
	ctl.Start()
	log.Printf("verification listening on %s — every check passes by default; POST /sim/outcome on %s to change one", addr, ctl.Addr())
	log.Fatal(http.ListenAndServe(addr, logReq(mux)))
}

// routes is verification's two surfaces: the four vendors on addr, and the
// lab's control surface on a port of its own (internal/simctl) — the
// outcome switches, the sim control contract and its own GET /health, with
// no vendor auth. The vendor port still answers the /sim paths until
// phase 4 of docs/lab-api/plan.md retires them.
func (a *app) routes(addr string) (*http.ServeMux, *simctl.Control, error) {
	ctl, err := simctl.NewControl(simctl.Config{
		Service: "verification", Kind: "vendor", Simulates: "Creditsafe, iban.com, KYC6 and LexisNexis",
		Listeners: []string{addr},
		Scopes:    []string{simctl.ScopeFlow, simctl.ScopeAll},
		Reset:     a.reset,
		Customers: a.customers,
	}, simctl.ControlAddr(addr))
	if err != nil {
		return nil, nil, err
	}
	// --- the lab seam -----------------------------------------------
	ctl.Mux.HandleFunc("GET /sim/outcome", a.readOutcomes)
	ctl.Mux.HandleFunc("POST /sim/outcome", a.setOutcome)
	ctl.Mux.HandleFunc("POST /sim/creditsafe/companies", a.registerCreditsafeCompany)
	ctl.Mux.HandleFunc("GET /sim/activity", activity.Handler(a.requests))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, map[string]any{"status": "ok", "service": "verification"})
	})
	// The old /sim paths on the vendor port, until phase 4.
	mux.Handle("/sim/", ctl.Mux)

	// vendor routes a vendor endpoint through the request journal.
	vendor := func(pattern string, h http.HandlerFunc) {
		path := pattern[strings.Index(pattern, " ")+1:]
		mux.HandleFunc(pattern, a.requests.Watch(vendorOf(path), nil, h))
	}

	// --- Creditsafe: company data and UK bank verification ----------
	vendor("POST /creditsafe/authenticate", a.creditsafeAuthenticate)
	vendor("POST /creditsafe/localSolutions/GB/bankVerification/search", a.bearer(a.creditsafeBankVerification))
	vendor("GET /creditsafe/companies", a.bearer(a.creditsafeCompanySearch))
	vendor("GET /creditsafe/companies/{id}", a.bearer(a.creditsafeCompany))
	vendor("GET /creditsafe/monitoring/portfolios", a.bearer(a.creditsafePortfolios))
	vendor("POST /creditsafe/monitoring/portfolios", a.bearer(a.creditsafeCreatePortfolio))
	vendor("POST /creditsafe/monitoring/portfolios/{portfolioId}/companies", a.bearer(a.creditsafeAddPortfolioCompany))
	vendor("GET /creditsafe/monitoring/portfolios/{portfolioId}/companies", a.bearer(a.creditsafePortfolioCompanies))
	vendor("PUT /creditsafe/monitoring/portfolios/{portfolioId}/eventRules/{countryCode}", a.bearer(a.creditsafeEventRules))
	vendor("GET /creditsafe/monitoring/notificationEvents", a.bearer(a.creditsafeEvents))

	// --- iban.com: the base URL is the endpoint ---------------------
	vendor("POST /iban/verify", a.ibanVerify)

	// --- KYC6: sanctions, PEP and adverse media ---------------------
	vendor("POST /kyc6/individuals", a.apiKey(a.kyc6Screen))
	vendor("POST /kyc6/businesses", a.apiKey(a.kyc6Screen))
	vendor("GET /kyc6/individuals/{resourceId}", a.apiKey(a.kyc6Profile))
	vendor("GET /kyc6/businesses/{resourceId}", a.apiKey(a.kyc6Profile))
	vendor("PUT /kyc6/worklists/{worklistId}", a.apiKey(a.kyc6CreateWorklist))
	vendor("GET /kyc6/lifecycle/ongoing-monitoring/{alertId}", a.apiKey(a.kyc6Monitoring))

	// --- LexisNexis: OAuth, then identity and verification ----------
	vendor("POST /lexisnexis/oauth/token", a.lexisToken)
	vendor("POST /lexisnexis/idu/v1/reports", a.bearer(a.lexisIDU))
	vendor("POST /lexisnexis/ivi/api/attribute-query", a.lexisIVI)
	return mux, ctl, nil
}

// reset is POST /sim/reset, of the environment it names, or of every one
// for "*". Every scope puts the outcomes back to their defaults: a flipped
// check is a run's trap. flow keeps what a platform obtained once and holds
// on to — the bearer tokens it was issued, the monitoring portfolios it
// created — because taking them away mid-session fails its next call with a
// 401 that no run caused. all is first start: no token is valid and
// portfolio ids count from 1 again.
func (a *app) reset(_ context.Context, req simctl.ResetRequest) (simctl.ResetResult, error) {
	scope := tenant.Normalize(req.Env)
	states := a.statesIn(scope)
	flipped, companies := 0, 0
	for _, s := range states {
		flipped += s.out.reset()
		companies += s.forgetCompanies()
	}
	cleared := map[string]int{"outcomes": flipped, "companies": companies, "requests": int(a.requests.ClearIn(scope))}
	if req.Scope != simctl.ScopeAll {
		return simctl.ResetResult{Cleared: cleared, Kept: []string{"tokens", "portfolios"}}, nil
	}
	cleared["tokens"] = a.dropTokens(scope)
	portfolios := 0
	for _, s := range states {
		portfolios += int(s.portfolioIDs.Swap(0))
	}
	cleared["portfolios"] = portfolios
	return simctl.ResetResult{Cleared: cleared}, nil
}

// --- the lab seam -------------------------------------------------------

// readOutcomes is GET /sim/outcome: what the named environment's checks
// answer, or, for "*", every environment's.
func (a *app) readOutcomes(w http.ResponseWriter, r *http.Request) {
	env := tenant.FromRequest(r)
	if !a.known(env) {
		httputilx.Error(w, 404, "no environment "+env+" at verification")
		return
	}
	if env != tenant.All {
		httputilx.WriteJSON(w, 200, map[string]any{"env": env, "outcomes": a.outcomes(env).snapshot()})
		return
	}
	all := map[string]map[string]string{}
	for _, e := range a.customers.Envs() {
		all[e] = a.outcomes(e).snapshot()
	}
	httputilx.WriteJSON(w, 200, map[string]any{"env": env, "environments": all})
}

type outcomeReq struct {
	Check   string `json:"check"`
	Outcome string `json:"outcome"`
}

// setOutcome is POST /sim/outcome: flips one check in the named
// environment, for that environment's later calls only.
func (a *app) setOutcome(w http.ResponseWriter, r *http.Request) {
	env := tenant.FromRequest(r)
	switch {
	case env == tenant.All:
		httputilx.Error(w, 400, "an outcome is set in one environment, not in all of them")
		return
	case !a.known(env):
		httputilx.Error(w, 404, "no environment "+env+" at verification")
		return
	}
	var req outcomeReq
	if err := httputilx.ReadJSON(r, &req); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	out := a.outcomes(env)
	if err := out.set(req.Check, req.Outcome); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	log.Printf("verification: %s now answers %q in environment %s", req.Check, req.Outcome, env)
	httputilx.WriteJSON(w, 200, map[string]any{"env": env, "outcomes": out.snapshot()})
}

// registerCreditsafeCompany is POST /sim/creditsafe/companies: the named
// environment's Creditsafe reports for this company carry these details.
func (a *app) registerCreditsafeCompany(w http.ResponseWriter, r *http.Request) {
	env := tenant.FromRequest(r)
	switch {
	case env == tenant.All:
		httputilx.Error(w, 400, "a company is registered in one environment, not in all of them")
		return
	case !a.known(env):
		httputilx.Error(w, 404, "no environment "+env+" at verification")
		return
	}
	var company creditsafeCompany
	if err := httputilx.ReadJSON(r, &company); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	if company.Country == "" || company.RegNo == "" {
		httputilx.Error(w, 400, "a company needs its country and regNo, as the platform searches by them")
		return
	}
	for _, people := range [][]creditsafePerson{company.Directors, company.Shareholders, company.PersonsWithSignificantControl} {
		for _, person := range people {
			if !person.named() {
				httputilx.Error(w, 400, "every director, shareholder and person with significant control needs a name")
				return
			}
		}
	}
	id := creditsafeCompanyID(company.Country, company.RegNo)
	a.in(env).companies.Store(id, company)
	log.Printf("verification: Creditsafe company %s registered in environment %s", id, env)
	httputilx.WriteJSON(w, 200, map[string]any{"env": env, "id": id, "company": company})
}

// vendorOf names the vendor a path belongs to, as the journal files it.
func vendorOf(path string) string {
	switch {
	case strings.HasPrefix(path, "/creditsafe/"):
		return "creditsafe"
	case strings.HasPrefix(path, "/iban/"):
		return "iban.com"
	case strings.HasPrefix(path, "/kyc6/"):
		return "kyc6"
	default:
		return "lexisnexis"
	}
}

// --- auth ---------------------------------------------------------------

// bearer guards the endpoints that need a token this service issued.
//
// It checks the token rather than waving it through: a client that never
// calls authenticate, or caches a token past its life, should learn that
// here. The whole value of a contract-shaped stub is that the parts which
// are real are actually real. The call is the environment's the token was
// issued to.
func (a *app) bearer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" || tok == r.Header.Get("Authorization") {
			httputilx.Error(w, 401, "bearer token required")
			return
		}
		v, ok := a.tokens.Load(tok)
		if !ok {
			httputilx.Error(w, 401, "unknown or expired token")
			return
		}
		next(w, a.as(r, v.(issued).env))
	}
}

// apiKey guards the vendors that authenticate with a header key. Any
// non-empty key is accepted: this lab holds no vendor credentials, and
// checking the *shape* is what catches a client that forgot to send one.
// A customer's key makes the call its environment's.
func (a *app) apiKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("x-api-key")
		if key == "" {
			httputilx.Error(w, 401, "x-api-key header required")
			return
		}
		next(w, a.as(r, a.byKey(key)))
	}
}

// as is r, the call of env: the environment goes into its context, for the
// handler's outcomes, and env is recorded as seen.
func (a *app) as(r *http.Request, env string) *http.Request {
	a.customers.Seen(env)
	return r.WithContext(tenant.With(r.Context(), env))
}

// byKey is the environment whose key key is (its login's password): the
// KYC6, iban.com and LexisNexis IVI key. Any other key is the default's.
func (a *app) byKey(key string) string {
	c, ok := a.customers.Find(func(c tenant.Customer) bool {
		return c.Verification != nil && c.Verification.Password != "" && c.Verification.Password == key
	})
	if !ok {
		return tenant.Default
	}
	return c.Env
}

// byLogin is the environment a username and password log in as: the
// Creditsafe username and password, or the LexisNexis client id and
// secret. A username no customer has is the default's, whatever the
// password, as every login was before; a customer's username with another
// password is refused, as the real vendor would refuse it.
func (a *app) byLogin(username, password string) (string, bool) {
	c, ok := a.customers.Find(func(c tenant.Customer) bool {
		return c.Verification != nil && c.Verification.Username != "" && c.Verification.Username == username
	})
	if !ok {
		return tenant.Default, true
	}
	return c.Env, c.Verification.Password == password
}

func (a *app) issue(prefix, env string) string {
	tok := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	a.tokens.Store(tok, issued{env: env, at: time.Now()})
	return tok
}

// --- Creditsafe ---------------------------------------------------------

func (a *app) creditsafeAuthenticate(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if str(body["username"]) == "" || str(body["password"]) == "" {
		httputilx.Error(w, 401, "username and password required")
		return
	}
	env, ok := a.byLogin(str(body["username"]), str(body["password"]))
	if !ok {
		httputilx.Error(w, 401, "invalid username or password")
		return
	}
	a.as(r, env)
	httputilx.WriteJSON(w, 200, map[string]any{"token": a.issue("cs", env)})
}

// creditsafeBankVerification answers the UK bank-account check.
//
// The client reads `supplierResponse.result` and `supplierResponse.
// nameMatchResult`, so those are what vary. Everything else is echoed back
// so a caller logging the raw response sees something shaped like one.
func (a *app) creditsafeBankVerification(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	result, nameMatch, accountType := true, "Full", true
	switch a.answer(r, checkBankAccount) {
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

func (a *app) creditsafeCompanySearch(w http.ResponseWriter, r *http.Request) {
	country := r.URL.Query().Get("countries")
	regNo := r.URL.Query().Get("regNo")
	httputilx.WriteJSON(w, 200, map[string]any{
		"totalSize": 1,
		"companies": []map[string]any{{
			"id":      creditsafeCompanyID(country, regNo),
			"country": country,
			"regNo":   regNo,
		}},
	})
}

func (a *app) creditsafeCompany(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	companyStatus := map[string]any{"status": "Active"}
	if a.answer(r, checkCompany) == "dissolved" {
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
	if stored, ok := a.in(tenant.Of(r.Context())).companies.Load(id); ok {
		company := stored.(creditsafeCompany)
		summary["businessName"] = company.BusinessName
		summary["companyRegistrationNumber"] = company.RegNo
		summary["country"] = company.Country
		report["companyIdentification"] = map[string]any{
			"basicInformation": map[string]any{
				"registeredCompanyName":   company.BusinessName,
				"companyRegistrationDate": company.RegistrationDate,
				"contactAddress":          map[string]any{"simpleValue": company.Address},
			},
		}
		report["directors"] = map[string]any{"currentDirectors": orEmpty(company.Directors)}
		report["shareCapitalStructure"] = map[string]any{"shareHolders": orEmpty(company.Shareholders)}
		report["additionalInformation"] = map[string]any{
			"personsWithSignificantControl": map[string]any{"activePSC": orEmpty(company.PersonsWithSignificantControl)},
		}
	}
	httputilx.WriteJSON(w, 200, map[string]any{"companyId": id, "report": report})
}

func orEmpty(people []creditsafePerson) []creditsafePerson {
	if people == nil {
		return []creditsafePerson{}
	}
	return people
}

func (a *app) creditsafePortfolios(w http.ResponseWriter, r *http.Request) {
	httputilx.WriteJSON(w, 200, map[string]any{
		"portfolios": []map[string]any{{"id": "sim-portfolio-1", "name": "Simulated portfolio"}},
	})
}

func (a *app) creditsafeCreatePortfolio(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	isDefault, _ := body["isDefault"].(bool)
	httputilx.WriteJSON(w, 200, map[string]any{
		"portfolioId": a.in(tenant.Of(r.Context())).portfolioIDs.Add(1),
		"name":        str(body["name"]),
		"isDefault":   isDefault,
	})
}

func (a *app) creditsafeAddPortfolioCompany(w http.ResponseWriter, r *http.Request) {
	httputilx.WriteJSON(w, 201, map[string]any{"message": "Company added to portfolio"})
}

func (a *app) creditsafePortfolioCompanies(w http.ResponseWriter, r *http.Request) {
	httputilx.WriteJSON(w, 200, map[string]any{"data": []any{}, "totalCount": 0})
}

func (a *app) creditsafeEventRules(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// creditsafeEvents answers the monitoring poll with nothing to report,
// which is the only honest default: an event this lab invented would send
// the platform chasing a company that did not change.
func (a *app) creditsafeEvents(w http.ResponseWriter, r *http.Request) {
	httputilx.WriteJSON(w, 200, map[string]any{
		"data": []any{}, "totalCount": 0,
	})
}

// --- iban.com -----------------------------------------------------------

// ibanVerify answers the account/name check.
//
// The base URL *is* the endpoint for this vendor, so a recipe points
// IBAN_COM_BASE_URL straight at this path. The client returns the body
// unchanged to its own caller, so the shape here is the shape the platform
// sees.
func (a *app) ibanVerify(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("x-api-key")
	if key == "" {
		httputilx.Error(w, 401, "x-api-key header required")
		return
	}
	r = a.as(r, a.byKey(key))
	var body struct {
		IBAN string `json:"IBAN"`
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.IBAN == "" {
		httputilx.WriteJSON(w, 200, map[string]any{"error": "IBAN is required"})
		return
	}

	valid, nameMatch := true, "yes"
	switch a.answer(r, checkBankAccount) {
	case "mismatch":
		nameMatch = "no"
	case "invalid":
		valid, nameMatch = false, "no"
	}
	httputilx.WriteJSON(w, 200, map[string]any{
		"query": map[string]any{"IBAN": body.IBAN, "name": body.Name, "success": true},
		"result": map[string]any{
			"valid": valid, "name_match": nameMatch,
			// Obviously fake, and stable for a given IBAN so a screenshot
			// and a rerun agree.
			"bic": "SIMBGB2L",
		},
	})
}

// --- KYC6 ---------------------------------------------------------------

// kyc6Screen answers a sanctions/PEP/adverse-media screen.
//
// Clear means zero matches, because that is what clear *is* for this
// vendor: the client reads matchCount and the matches array, and an empty
// list is the statement that nothing was found.
func (a *app) kyc6Screen(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	matches := []map[string]any{}
	if a.answer(r, checkScreening) == "hit" {
		matches = append(matches, map[string]any{
			"resourceId": "sim-match-1",
			"name":       str(body["name"]),
			"score":      0.92,
			"datasets":   []string{"SAN-CURRENT"},
			"matchTypes": []string{"name"},
		})
	}
	httputilx.WriteJSON(w, 200, map[string]any{
		"results": map[string]any{
			"matchCount": len(matches),
			"matches":    matches,
		},
	})
}

func (a *app) kyc6Profile(w http.ResponseWriter, r *http.Request) {
	httputilx.WriteJSON(w, 200, map[string]any{
		"resourceId": r.PathValue("resourceId"),
		"datasets":   []string{"SAN-CURRENT"},
	})
}

func (a *app) kyc6CreateWorklist(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	httputilx.WriteJSON(w, 200, map[string]any{
		"worklistId": r.PathValue("worklistId"),
		"name":       str(body["name"]),
	})
}

func (a *app) kyc6Monitoring(w http.ResponseWriter, r *http.Request) {
	httputilx.WriteJSON(w, 200, map[string]any{
		"alertId": r.PathValue("alertId"),
		"status":  "closed",
		"matches": []any{},
	})
}

// --- LexisNexis ---------------------------------------------------------

func (a *app) lexisToken(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if str(body["client_id"]) == "" || str(body["client_secret"]) == "" {
		httputilx.Error(w, 401, "client_id and client_secret required")
		return
	}
	env, ok := a.byLogin(str(body["client_id"]), str(body["client_secret"]))
	if !ok {
		httputilx.Error(w, 401, "invalid client_id or client_secret")
		return
	}
	a.as(r, env)
	httputilx.WriteJSON(w, 200, map[string]any{
		"access_token": a.issue("ln", env),
		"expires_in":   3600,
		"token_type":   "Bearer",
	})
}

// lexisIDU answers the identity check.
//
// The client normalises `assessment.result` to Pass / Refer / Fail and
// treats anything it does not recognise as Refer, so the values here are
// the three it knows. The score is reported against the threshold it also
// reads, because a pass whose score is below its own threshold would be a
// contradiction a careful client should catch.
func (a *app) lexisIDU(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	result, score := "PASS", 850
	switch a.answer(r, checkIdentity) {
	case "refer":
		result, score = "REFER", 500
	case "fail":
		result, score = "FAIL", 100
	}
	httputilx.WriteJSON(w, 200, map[string]any{
		"data": map[string]any{
			"id":         "sim-idu-" + str(body["reference"]),
			"status":     result,
			"assessment": map[string]any{"result": result, "score": score},
			"context":    map[string]any{"pass_threshold": 700},
			"attributes": map[string]any{
				"dob_count":        1,
				"address_verified": true,
				"sanction":         false,
				"pep":              false,
				"insolvency":       false,
				"ccj":              false,
				"death_ddri":       false,
				"death_gro":        false,
				"death_halo":       false,
			},
		},
	})
}

func (a *app) lexisIVI(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if str(body["org_id"]) == "" || str(body["api_key"]) == "" || str(body["policy"]) == "" {
		httputilx.Error(w, 401, "org_id, api_key and policy required")
		return
	}
	r = a.as(r, a.byKey(str(body["api_key"])))

	status, score := "PASS", 850
	switch a.answer(r, checkIdentity) {
	case "refer":
		status, score = "REFER", 500
	case "fail":
		status, score = "FAIL", 100
	}
	httputilx.WriteJSON(w, 200, map[string]any{
		"request_id": "sim-ivi-" + str(body["reference"]),
		"request_result": map[string]any{
			"review_status": status,
			"policy_score":  score,
		},
		"integration_hub_results": []map[string]any{
			{"Products": []map[string]any{{"ProductStatus": status}}},
		},
	})
}

// --- helpers ------------------------------------------------------------

func str(v any) string {
	s, _ := v.(string)
	return s
}

func keys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func logReq(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
