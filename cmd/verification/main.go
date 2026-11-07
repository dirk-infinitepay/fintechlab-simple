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
// Every outcome is flippable at POST /sim/outcome, so the unhappy paths are
// reachable when somebody wants them. Defaulting to "pass" is a
// convenience, not a limitation.
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

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/logring"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
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
	out *outcomes
	// tokens are the bearer tokens this service has issued. Each vendor
	// authenticates differently and they are checked, because a client
	// that forgets its token should find out here rather than in
	// production.
	tokens sync.Map

	portfolioIDs atomic.Int64
}

func main() {
	logring.Install()
	addr := env("LISTEN", ":8089")
	a := &app{out: newOutcomes()}

	log.Printf("verification listening on %s — every check passes by default; POST /sim/outcome to change one", addr)
	log.Fatal(http.ListenAndServe(addr, logReq(a.routes(addr))))
}

// routes is the four vendors' surface, the outcome switches, and the sim
// control contract.
func (a *app) routes(addr string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, map[string]any{"status": "ok", "service": "verification"})
	})

	// --- the lab seam -----------------------------------------------
	mux.HandleFunc("GET /sim/outcome", a.readOutcomes)
	mux.HandleFunc("POST /sim/outcome", a.setOutcome)

	// --- Creditsafe: company data and UK bank verification ----------
	mux.HandleFunc("POST /creditsafe/authenticate", a.creditsafeAuthenticate)
	mux.HandleFunc("POST /creditsafe/localSolutions/GB/bankVerification/search", a.bearer(a.creditsafeBankVerification))
	mux.HandleFunc("GET /creditsafe/companies", a.bearer(a.creditsafeCompanySearch))
	mux.HandleFunc("GET /creditsafe/companies/{id}", a.bearer(a.creditsafeCompany))
	mux.HandleFunc("GET /creditsafe/monitoring/portfolios", a.bearer(a.creditsafePortfolios))
	mux.HandleFunc("POST /creditsafe/monitoring/portfolios", a.bearer(a.creditsafeCreatePortfolio))
	mux.HandleFunc("POST /creditsafe/monitoring/portfolios/{portfolioId}/companies", a.bearer(a.creditsafeAddPortfolioCompany))
	mux.HandleFunc("GET /creditsafe/monitoring/portfolios/{portfolioId}/companies", a.bearer(a.creditsafePortfolioCompanies))
	mux.HandleFunc("PUT /creditsafe/monitoring/portfolios/{portfolioId}/eventRules/{countryCode}", a.bearer(a.creditsafeEventRules))
	mux.HandleFunc("GET /creditsafe/monitoring/notificationEvents", a.bearer(a.creditsafeEvents))

	// --- iban.com: the base URL is the endpoint ---------------------
	mux.HandleFunc("POST /iban/verify", a.ibanVerify)

	// --- KYC6: sanctions, PEP and adverse media ---------------------
	mux.HandleFunc("POST /kyc6/individuals", a.apiKey(a.kyc6Screen))
	mux.HandleFunc("POST /kyc6/businesses", a.apiKey(a.kyc6Screen))
	mux.HandleFunc("GET /kyc6/individuals/{resourceId}", a.apiKey(a.kyc6Profile))
	mux.HandleFunc("GET /kyc6/businesses/{resourceId}", a.apiKey(a.kyc6Profile))
	mux.HandleFunc("PUT /kyc6/worklists/{worklistId}", a.apiKey(a.kyc6CreateWorklist))
	mux.HandleFunc("GET /kyc6/lifecycle/ongoing-monitoring/{alertId}", a.apiKey(a.kyc6Monitoring))

	// --- LexisNexis: OAuth, then identity and verification ----------
	mux.HandleFunc("POST /lexisnexis/oauth/token", a.lexisToken)
	mux.HandleFunc("POST /lexisnexis/idu/v1/reports", a.bearer(a.lexisIDU))
	mux.HandleFunc("POST /lexisnexis/ivi/api/attribute-query", a.lexisIVI)

	simctl.Mount(mux, simctl.Config{
		Service: "verification", Kind: "vendor", Simulates: "Creditsafe, iban.com, KYC6 and LexisNexis",
		Listeners: []string{addr},
		Scopes:    []string{simctl.ScopeFlow, simctl.ScopeAll},
		Reset:     a.reset,
	})
	return mux
}

// reset is POST /sim/reset. Every scope puts the outcomes back to their
// defaults: a flipped check is a run's trap. flow keeps what a platform
// obtained once and holds on to — the bearer tokens it was issued, the
// monitoring portfolios it created — because taking them away mid-session
// fails its next call with a 401 that no run caused. all is first start:
// no token is valid and portfolio ids count from 1 again.
func (a *app) reset(_ context.Context, req simctl.ResetRequest) (simctl.ResetResult, error) {
	cleared := map[string]int{"outcomes": a.out.reset()}
	if req.Scope != simctl.ScopeAll {
		return simctl.ResetResult{Cleared: cleared, Kept: []string{"tokens", "portfolios"}}, nil
	}
	tokens := 0
	a.tokens.Range(func(k, _ any) bool {
		a.tokens.Delete(k)
		tokens++
		return true
	})
	cleared["tokens"] = tokens
	cleared["portfolios"] = int(a.portfolioIDs.Swap(0))
	return simctl.ResetResult{Cleared: cleared}, nil
}

// --- the lab seam -------------------------------------------------------

func (a *app) readOutcomes(w http.ResponseWriter, r *http.Request) {
	httputilx.WriteJSON(w, 200, map[string]any{"outcomes": a.out.snapshot()})
}

type outcomeReq struct {
	Check   string `json:"check"`
	Outcome string `json:"outcome"`
}

func (a *app) setOutcome(w http.ResponseWriter, r *http.Request) {
	var req outcomeReq
	if err := httputilx.ReadJSON(r, &req); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	if err := a.out.set(req.Check, req.Outcome); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	log.Printf("verification: %s now answers %q", req.Check, req.Outcome)
	httputilx.WriteJSON(w, 200, map[string]any{"outcomes": a.out.snapshot()})
}

// --- auth ---------------------------------------------------------------

// bearer guards the endpoints that need a token this service issued.
//
// It checks the token rather than waving it through: a client that never
// calls authenticate, or caches a token past its life, should learn that
// here. The whole value of a contract-shaped stub is that the parts which
// are real are actually real.
func (a *app) bearer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" || tok == r.Header.Get("Authorization") {
			httputilx.Error(w, 401, "bearer token required")
			return
		}
		if _, ok := a.tokens.Load(tok); !ok {
			httputilx.Error(w, 401, "unknown or expired token")
			return
		}
		next(w, r)
	}
}

// apiKey guards the vendors that authenticate with a header key. Any
// non-empty key is accepted: this lab holds no vendor credentials, and
// checking the *shape* is what catches a client that forgot to send one.
func (a *app) apiKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" {
			httputilx.Error(w, 401, "x-api-key header required")
			return
		}
		next(w, r)
	}
}

func (a *app) issue(prefix string) string {
	tok := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	a.tokens.Store(tok, time.Now())
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
	httputilx.WriteJSON(w, 200, map[string]any{"token": a.issue("cs")})
}

// creditsafeBankVerification answers the UK bank-account check.
//
// The client reads `supplierResponse.result` and `supplierResponse.
// nameMatchResult`, so those are what vary. Everything else is echoed back
// so a caller logging the raw response sees something shaped like one.
func (a *app) creditsafeBankVerification(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	result, nameMatch := true, "MATCH"
	switch a.out.get(checkBankAccount) {
	case "mismatch":
		nameMatch = "NOMATCH"
	case "invalid":
		result, nameMatch = false, "NOMATCH"
	}
	httputilx.WriteJSON(w, 200, map[string]any{
		"supplierResponse": map[string]any{
			"result":          result,
			"nameMatchResult": nameMatch,
			"accountNumber":   body["accountNumber"],
			"sortCode":        body["sortCode"],
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
			"id":      country + "-0-" + regNo,
			"country": country,
			"regNo":   regNo,
		}},
	})
}

func (a *app) creditsafeCompany(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	status := "Active"
	if a.out.get(checkCompany) == "dissolved" {
		status = "Dissolved"
	}
	httputilx.WriteJSON(w, 200, map[string]any{
		"companyId": id,
		"report": map[string]any{
			"companySummary": map[string]any{
				"companyNumber": id,
				"companyStatus": map[string]any{"status": status},
			},
		},
	})
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
		"portfolioId": a.portfolioIDs.Add(1),
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
	if r.Header.Get("x-api-key") == "" {
		httputilx.Error(w, 401, "x-api-key header required")
		return
	}
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
	switch a.out.get(checkBankAccount) {
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
	if a.out.get(checkScreening) == "hit" {
		matches = append(matches, map[string]any{
			"resourceId": "sim-match-1",
			"name":       str(body["name"]),
			"score":      0.92,
			"datasets":   []string{"SANCTIONS"},
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
		"datasets":   []string{"SANCTIONS"},
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
	httputilx.WriteJSON(w, 200, map[string]any{
		"access_token": a.issue("ln"),
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
	switch a.out.get(checkIdentity) {
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
			"attributes": map[string]any{"dob_count": 1},
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

	status, score := "PASS", 850
	switch a.out.get(checkIdentity) {
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
