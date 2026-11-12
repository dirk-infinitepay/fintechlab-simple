package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sbxCustomer is the customer the lab pushes for environment sbx. Its
// verification username is the Creditsafe username and LexisNexis client
// id; its password is every other credential.
const sbxCustomer = `{"env": "sbx", "name": "Sandbox", "verification": {"username": "sbx-user", "password": "sbx-secret"}}`

// do sends one request with headers and decodes the answer.
func do(t *testing.T, h http.Handler, method, path, body string, hdr map[string]string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// withSbx is verification's two surfaces with sbx pushed as a customer, as
// the lab API pushes it.
func withSbx(t *testing.T) (a *app, svc, ctl http.Handler) {
	t.Helper()
	a = newApp()
	mux, c := surfaces(t, a)
	if code, out := do(t, c.Handler(), "PUT", "/sim/customers/sbx", sbxCustomer, nil); code != 200 || out["created"] != true {
		t.Fatalf("PUT /sim/customers/sbx = %d %v", code, out)
	}
	return a, mux, c.Handler()
}

func screen(t *testing.T, svc http.Handler, key string) float64 {
	t.Helper()
	code, out := do(t, svc, "POST", "/kyc6/individuals", `{"name":"Ada"}`, map[string]string{"x-api-key": key})
	if code != 200 {
		t.Fatalf("kyc6 with key %q: %d %v", key, code, out)
	}
	res, _ := out["results"].(map[string]any)
	n, _ := res["matchCount"].(float64)
	return n
}

func bearerTok(t *testing.T, svc http.Handler, path, body string) string {
	t.Helper()
	code, out := do(t, svc, "POST", path, body, nil)
	if code != 200 {
		t.Fatalf("POST %s: %d %v", path, code, out)
	}
	for _, f := range []string{"token", "access_token"} {
		if v, _ := out[f].(string); v != "" {
			return v
		}
	}
	t.Fatalf("POST %s carried no token: %v", path, out)
	return ""
}

func identity(t *testing.T, svc http.Handler, tok string) any {
	t.Helper()
	code, out := do(t, svc, "POST", "/lexisnexis/idu/v1/reports", `{"reference":"r"}`, bearerHdr(tok))
	if code != 200 {
		t.Fatalf("idu: %d %v", code, out)
	}
	return nested(out, "data", "assessment")["result"]
}

// Two environments side by side: a flip in one moves only its own
// platform's answers, whichever vendor and whichever credential the call
// came with.
func TestEachEnvironmentHasItsOwnOutcomes(t *testing.T) {
	_, svc, ctl := withSbx(t)
	if code, out := do(t, ctl, "POST", "/sim/outcome?env=sbx", `{"check":"screening","outcome":"hit"}`, nil); code != 200 || out["env"] != "sbx" {
		t.Fatalf("flip in sbx = %d %v", code, out)
	}
	if code, out := do(t, ctl, "POST", "/sim/outcome", `{"check":"identity","outcome":"fail"}`, map[string]string{"Lab-Environment": "default"}); code != 200 || out["env"] != "default" {
		t.Fatalf("flip in default = %d %v", code, out)
	}

	// KYC6: sbx's key sees sbx's hit; any other key is the default's.
	if n := screen(t, svc, "sbx-secret"); n != 1 {
		t.Errorf("sbx's screen: %v matches, want the hit", n)
	}
	if n := screen(t, svc, "sim-key"); n != 0 {
		t.Errorf("default's screen: %v matches, want clear", n)
	}

	// LexisNexis: a token is its login's environment's.
	sbx := bearerTok(t, svc, "/lexisnexis/oauth/token", `{"client_id":"sbx-user","client_secret":"sbx-secret"}`)
	def := bearerTok(t, svc, "/lexisnexis/oauth/token", `{"client_id":"anyone","client_secret":"anything"}`)
	if got := identity(t, svc, sbx); got != "PASS" {
		t.Errorf("sbx's identity = %v, want PASS: the default's fail is not its", got)
	}
	if got := identity(t, svc, def); got != "FAIL" {
		t.Errorf("default's identity = %v, want FAIL", got)
	}
	// IVI carries its key in the body.
	for key, want := range map[string]string{"sbx-secret": "PASS", "k": "FAIL"} {
		_, out := do(t, svc, "POST", "/lexisnexis/ivi/api/attribute-query", `{"org_id":"o","api_key":"`+key+`","policy":"p"}`, nil)
		if res, _ := out["request_result"].(map[string]any); res["review_status"] != want {
			t.Errorf("IVI with key %s = %v, want %s", key, out, want)
		}
	}

	// The reads say the same.
	_, sbxOut := do(t, ctl, "GET", "/sim/outcome?env=sbx", "", nil)
	_, defOut := do(t, ctl, "GET", "/sim/outcome", "", nil)
	if sbxOut["outcomes"].(map[string]any)["screening"] != "hit" || sbxOut["outcomes"].(map[string]any)["identity"] != "pass" {
		t.Errorf("GET /sim/outcome?env=sbx = %v", sbxOut)
	}
	if defOut["outcomes"].(map[string]any)["screening"] != "clear" || defOut["outcomes"].(map[string]any)["identity"] != "fail" {
		t.Errorf("GET /sim/outcome = %v", defOut)
	}
	_, all := do(t, ctl, "GET", "/sim/outcome?env=*", "", nil)
	envs, _ := all["environments"].(map[string]any)
	if len(envs) != 2 || envs["sbx"].(map[string]any)["screening"] != "hit" || envs["default"].(map[string]any)["identity"] != "fail" {
		t.Errorf("GET /sim/outcome?env=* = %v", all)
	}
}

// Creditsafe's login and iban.com's key name the environment too, and the
// customer is seen by its vendor calls.
func TestCreditsafeAndIbanComFollowTheLogin(t *testing.T) {
	_, svc, ctl := withSbx(t)
	do(t, ctl, "POST", "/sim/outcome?env=sbx", `{"check":"bank-account","outcome":"invalid"}`, nil)

	sbx := bearerTok(t, svc, "/creditsafe/authenticate", `{"username":"sbx-user","password":"sbx-secret"}`)
	def := bearerTok(t, svc, "/creditsafe/authenticate", `{"username":"sim","password":"sim"}`)
	for tok, want := range map[string]bool{sbx: false, def: true} {
		_, out := do(t, svc, "POST", "/creditsafe/localSolutions/GB/bankVerification/search", `{"accountNumber":"1"}`, bearerHdr(tok))
		if got := out["supplierResponse"].(map[string]any)["result"]; got != want {
			t.Errorf("bank verification with %s = %v, want %v", tok, got, want)
		}
	}
	for key, want := range map[string]bool{"sbx-secret": false, "other": true} {
		_, out := do(t, svc, "POST", "/iban/verify", `{"IBAN":"GB00X"}`, map[string]string{"x-api-key": key})
		if got := out["result"].(map[string]any)["valid"]; got != want {
			t.Errorf("iban.com with %s = %v, want %v", key, got, want)
		}
	}

	// Portfolio ids count per environment.
	for _, tok := range []string{sbx, def} {
		if _, out := do(t, svc, "POST", "/creditsafe/monitoring/portfolios", `{"name":"p"}`, bearerHdr(tok)); out["portfolioId"] != float64(1) {
			t.Errorf("first portfolio with %s = %v, want 1", tok, out["portfolioId"])
		}
	}

	_, st := do(t, ctl, "GET", "/sim/customers", "", nil)
	list, _ := st["customers"].([]any)
	if len(list) != 2 {
		t.Fatalf("GET /sim/customers = %v", st)
	}
	for _, c := range list {
		if c.(map[string]any)["last_seen"] == nil {
			t.Errorf("%v was never seen, but called the vendor", c)
		}
	}
}

// A login no customer has is the default's, as every login was before; a
// customer's username with the wrong password is refused, as the vendor
// would.
func TestAnUnknownLoginIsTheDefaults(t *testing.T) {
	_, svc, ctl := withSbx(t)
	do(t, ctl, "POST", "/sim/outcome", `{"check":"identity","outcome":"refer"}`, nil)
	tok := bearerTok(t, svc, "/lexisnexis/oauth/token", `{"client_id":"stranger","client_secret":"x"}`)
	if got := identity(t, svc, tok); got != "REFER" {
		t.Errorf("an unknown login's identity = %v, want the default's REFER", got)
	}
	for path, body := range map[string]string{
		"/creditsafe/authenticate": `{"username":"sbx-user","password":"wrong"}`,
		"/lexisnexis/oauth/token":  `{"client_id":"sbx-user","client_secret":"wrong"}`,
	} {
		if code, out := do(t, svc, "POST", path, body, nil); code != 401 {
			t.Errorf("POST %s with sbx's username and a wrong password = %d %v, want 401", path, code, out)
		}
	}
}

// The outcome hooks name one environment the sim knows.
func TestTheOutcomeHooksNameAnEnvironment(t *testing.T) {
	a, _, ctl := withSbx(t)
	if code, _ := do(t, ctl, "POST", "/sim/outcome?env=*", `{"check":"screening","outcome":"hit"}`, nil); code != 400 {
		t.Errorf("a flip in every environment = %d, want 400", code)
	}
	for _, m := range []string{"GET", "POST"} {
		if code, _ := do(t, ctl, m, "/sim/outcome?env=nope", `{"check":"screening","outcome":"hit"}`, nil); code != 404 {
			t.Errorf("%s /sim/outcome in an unknown environment = %d, want 404", m, code)
		}
	}
	for _, env := range []string{"default", "sbx"} {
		if got := a.outcomes(env).get(checkScreening); got != "clear" {
			t.Errorf("%s's screening = %q after refused flips", env, got)
		}
	}
}

// A reset of one environment puts back its outcomes and, with all, drops
// its tokens; the other's flips and tokens stay.
func TestAResetOfOneEnvironmentKeepsTheOther(t *testing.T) {
	a, svc, ctl := withSbx(t)
	sbx := bearerTok(t, svc, "/lexisnexis/oauth/token", `{"client_id":"sbx-user","client_secret":"sbx-secret"}`)
	def := bearerTok(t, svc, "/lexisnexis/oauth/token", `{"client_id":"c","client_secret":"s"}`)
	do(t, ctl, "POST", "/sim/outcome?env=sbx", `{"check":"screening","outcome":"hit"}`, nil)
	do(t, ctl, "POST", "/sim/outcome", `{"check":"company","outcome":"dissolved"}`, nil)

	code, out := do(t, ctl, "POST", "/sim/reset", `{"scope":"all","env":"sbx"}`, nil)
	if code != 200 || out["env"] != "sbx" {
		t.Fatalf("reset sbx = %d %v", code, out)
	}
	if c := out["cleared"].(map[string]any); c["outcomes"] != 1.0 || c["tokens"] != 1.0 {
		t.Errorf("cleared = %v, want sbx's one flip and one token", c)
	}
	if a.outcomes("sbx").get(checkScreening) != "clear" {
		t.Error("sbx's flip survived its reset")
	}
	if a.outcomes("default").get(checkCompany) != "dissolved" {
		t.Error("the default's flip went with sbx's reset")
	}
	if code, _ := do(t, svc, "POST", "/lexisnexis/idu/v1/reports", `{}`, bearerHdr(sbx)); code != 401 {
		t.Errorf("sbx's token after its all reset = %d, want 401", code)
	}
	if code, _ := do(t, svc, "POST", "/lexisnexis/idu/v1/reports", `{}`, bearerHdr(def)); code != 200 {
		t.Errorf("the default's token after sbx's reset = %d, want 200", code)
	}

	// "*" resets both.
	do(t, ctl, "POST", "/sim/outcome?env=sbx", `{"check":"screening","outcome":"hit"}`, nil)
	if _, out := do(t, ctl, "POST", "/sim/reset", `{"env":"*"}`, nil); out["cleared"].(map[string]any)["outcomes"] != 2.0 {
		t.Errorf("reset * = %v, want both flips", out)
	}
}

// Deleting a customer takes its outcomes and tokens with it: its login is
// then a stranger's, the default's.
func TestDeletingACustomerClearsWhatItHad(t *testing.T) {
	a, svc, ctl := withSbx(t)
	tok := bearerTok(t, svc, "/creditsafe/authenticate", `{"username":"sbx-user","password":"sbx-secret"}`)
	do(t, ctl, "POST", "/sim/outcome?env=sbx", `{"check":"screening","outcome":"hit"}`, nil)
	if code, out := do(t, ctl, "POST", "/sim/traps?env=sbx", `{"hook":"http.request","match":{},"effect":{"respond":{"status":503}},"times":null}`, nil); code != 201 {
		t.Fatalf("arm in sbx = %d %v", code, out)
	}

	if code, out := do(t, ctl, "DELETE", "/sim/customers/sbx", "", nil); code != 200 {
		t.Fatalf("DELETE /sim/customers/sbx = %d %v", code, out)
	}
	if code, out := do(t, ctl, "PUT", "/sim/customers/sbx", sbxCustomer, nil); code != 200 {
		t.Fatalf("PUT sbx again = %d %v", code, out)
	}
	srv := httptest.NewServer(a.faults(svc))
	t.Cleanup(srv.Close)
	r, _ := http.NewRequest("POST", srv.URL+"/kyc6/individuals", strings.NewReader(`{"name":"Ada"}`))
	r.Header.Set("x-api-key", "sbx-secret")
	if res, err := http.DefaultClient.Do(r); err != nil || res.StatusCode != 200 {
		t.Errorf("a recreated sbx's request = %v %v, want the vendor's 200, not the old trap", res, err)
	}
	if code, out := do(t, ctl, "DELETE", "/sim/customers/sbx", "", nil); code != 200 {
		t.Fatalf("DELETE /sim/customers/sbx = %d %v", code, out)
	}
	if code, _ := do(t, svc, "GET", "/creditsafe/companies/1", "", bearerHdr(tok)); code != 401 {
		t.Errorf("sbx's token after it was deleted = %d, want 401", code)
	}
	a.mu.Lock()
	_, held := a.envs["sbx"]
	a.mu.Unlock()
	if held {
		t.Error("sbx's outcomes outlived it")
	}
	if n := screen(t, svc, "sbx-secret"); n != 0 {
		t.Errorf("sbx's old key screens %v, want the default's clear", n)
	}
	if code, _ := do(t, ctl, "GET", "/sim/outcome?env=sbx", "", nil); code != 404 {
		t.Errorf("GET /sim/outcome?env=sbx after the delete = %d, want 404", code)
	}
}
