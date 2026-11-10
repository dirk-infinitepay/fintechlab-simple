package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/webduvet/fintechlab-simple/internal/simctl"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// call drives one endpoint the way its real client does and returns the
// decoded body.
func call(t *testing.T, a *app, h http.HandlerFunc, method, path, body string, hdr map[string]string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// token runs the vendor's auth handshake and returns the bearer it issued,
// because every guarded endpoint needs one and a client that skips it is
// meant to be refused.
func token(t *testing.T, a *app, h http.HandlerFunc, path, body string) string {
	t.Helper()
	code, out := call(t, a, h, http.MethodPost, path, body, nil)
	if code != http.StatusOK {
		t.Fatalf("auth: status %d body %v", code, out)
	}
	for _, field := range []string{"token", "access_token"} {
		if v, ok := out[field].(string); ok && v != "" {
			return v
		}
	}
	t.Fatalf("auth response carried no token: %v", out)
	return ""
}

func bearerHdr(tok string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + tok}
}
func keyHdr() map[string]string { return map[string]string{"x-api-key": "sim-key"} }

// TestEveryCheckPassesByDefault is the property the whole service exists
// for: point the platform at it and nothing blocks. Each assertion reads
// the field that vendor's own client reads.
func TestEveryCheckPassesByDefault(t *testing.T) {
	a := newApp()

	// Creditsafe: POST /authenticate, then the UK bank check.
	cs := token(t, a, a.creditsafeAuthenticate, "/creditsafe/authenticate",
		`{"username":"sim","password":"sim"}`)
	code, out := call(t, a, a.bearer(a.creditsafeBankVerification),
		http.MethodPost, "/creditsafe/localSolutions/GB/bankVerification/search",
		`{"accountNumber":"12345678","sortCode":"00-00-00","name":"Southwind Coffee Ltd"}`,
		bearerHdr(cs))
	if code != http.StatusOK {
		t.Fatalf("creditsafe bank verification: %d %v", code, out)
	}
	sup, _ := out["supplierResponse"].(map[string]any)
	if sup["result"] != true {
		t.Errorf("creditsafe result = %v, want true", sup["result"])
	}
	if sup["nameMatchResult"] != "MATCH" {
		t.Errorf("creditsafe nameMatchResult = %v, want MATCH", sup["nameMatchResult"])
	}

	// iban.com: the base URL is the endpoint, keyed by header.
	code, out = call(t, a, a.ibanVerify, http.MethodPost, "/iban/verify",
		`{"IBAN":"GB00SIMMERCH0000001","name":"Southwind Coffee Ltd"}`, keyHdr())
	if code != http.StatusOK {
		t.Fatalf("iban verify: %d %v", code, out)
	}
	res, _ := out["result"].(map[string]any)
	if res["valid"] != true || res["name_match"] != "yes" {
		t.Errorf("iban result = %v, want valid + yes", res)
	}

	// KYC6: clear means zero matches, because that is what clear is.
	code, out = call(t, a, a.apiKey(a.kyc6Screen), http.MethodPost, "/kyc6/businesses",
		`{"name":"Southwind Coffee Ltd"}`, keyHdr())
	if code != http.StatusOK {
		t.Fatalf("kyc6: %d %v", code, out)
	}
	results, _ := out["results"].(map[string]any)
	if n, _ := results["matchCount"].(float64); n != 0 {
		t.Errorf("kyc6 matchCount = %v, want 0", results["matchCount"])
	}

	// LexisNexis: OAuth, then identity. The client normalises the result,
	// so PASS is the value it maps to "Pass".
	ln := token(t, a, a.lexisToken, "/lexisnexis/oauth/token",
		`{"client_id":"sim","client_secret":"sim"}`)
	code, out = call(t, a, a.bearer(a.lexisIDU), http.MethodPost,
		"/lexisnexis/idu/v1/reports", `{"reference":"ref-1"}`, bearerHdr(ln))
	if code != http.StatusOK {
		t.Fatalf("lexisnexis idu: %d %v", code, out)
	}
	assessment := nested(out, "data", "assessment")
	if assessment["result"] != "PASS" {
		t.Errorf("idu result = %v, want PASS", assessment["result"])
	}
	// A pass whose score sits below its own threshold is a contradiction a
	// careful client would be right to reject.
	ctx := nested(out, "data", "context")
	score, _ := assessment["score"].(float64)
	threshold, _ := ctx["pass_threshold"].(float64)
	if score < threshold {
		t.Errorf("a PASS scored %v against a threshold of %v", score, threshold)
	}
}

// TestOutcomesAreFlippable: the unhappy paths have to be reachable, or this
// is a bypass rather than a stub. Each flip must move the field its own
// client reads.
func TestOutcomesAreFlippable(t *testing.T) {
	a := newApp()
	cs := token(t, a, a.creditsafeAuthenticate, "/creditsafe/authenticate", `{"username":"u","password":"p"}`)
	ln := token(t, a, a.lexisToken, "/lexisnexis/oauth/token", `{"client_id":"c","client_secret":"s"}`)

	if err := a.outcomes(tenant.Default).set(checkBankAccount, "mismatch"); err != nil {
		t.Fatal(err)
	}
	_, out := call(t, a, a.ibanVerify, http.MethodPost, "/iban/verify",
		`{"IBAN":"GB00X","name":"Someone Else"}`, keyHdr())
	res, _ := out["result"].(map[string]any)
	if res["valid"] != true || res["name_match"] != "no" {
		t.Errorf("a mismatch is a valid account with the wrong name: %v", res)
	}

	if err := a.outcomes(tenant.Default).set(checkBankAccount, "invalid"); err != nil {
		t.Fatal(err)
	}
	_, out = call(t, a, a.bearer(a.creditsafeBankVerification), http.MethodPost, "/x",
		`{"accountNumber":"1"}`, bearerHdr(cs))
	sup, _ := out["supplierResponse"].(map[string]any)
	if sup["result"] != false {
		t.Errorf("an invalid account should not verify: %v", sup)
	}

	if err := a.outcomes(tenant.Default).set(checkScreening, "hit"); err != nil {
		t.Fatal(err)
	}
	_, out = call(t, a, a.apiKey(a.kyc6Screen), http.MethodPost, "/kyc6/individuals",
		`{"name":"Sanctioned Person"}`, keyHdr())
	results, _ := out["results"].(map[string]any)
	if n, _ := results["matchCount"].(float64); n != 1 {
		t.Errorf("a screening hit should produce a match: %v", results)
	}

	if err := a.outcomes(tenant.Default).set(checkIdentity, "fail"); err != nil {
		t.Fatal(err)
	}
	_, out = call(t, a, a.bearer(a.lexisIDU), http.MethodPost, "/x", `{"reference":"r"}`, bearerHdr(ln))
	if nested(out, "data", "assessment")["result"] != "FAIL" {
		t.Errorf("identity should fail: %v", out)
	}
}

// TestUnknownOutcomeIsRefused: a typo that silently did nothing would leave
// an operator believing they had armed a failure that never fires.
func TestUnknownOutcomeIsRefused(t *testing.T) {
	a := newApp()
	for _, bad := range []string{
		`{"check":"bank-account","outcome":"maybe"}`,
		`{"check":"nonsense","outcome":"pass"}`,
	} {
		if code, _ := call(t, a, a.setOutcome, http.MethodPost, "/sim/outcome", bad, nil); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", bad, code)
		}
	}
	if a.outcomes(tenant.Default).get(checkBankAccount) != "pass" {
		t.Error("a refused flip must not change the answer")
	}
}

// TestAuthIsReal: the contract is the part that is not faked. A client that
// never authenticates, or sends a token this service did not issue, should
// find out here rather than against the real vendor.
func TestAuthIsReal(t *testing.T) {
	a := newApp()

	if code, _ := call(t, a, a.bearer(a.creditsafeBankVerification), http.MethodPost, "/x", `{}`, nil); code != http.StatusUnauthorized {
		t.Errorf("no bearer: status %d, want 401", code)
	}
	if code, _ := call(t, a, a.bearer(a.lexisIDU), http.MethodPost, "/x", `{}`,
		bearerHdr("not-a-token-we-issued")); code != http.StatusUnauthorized {
		t.Errorf("forged bearer: status %d, want 401", code)
	}
	if code, _ := call(t, a, a.apiKey(a.kyc6Screen), http.MethodPost, "/x", `{}`, nil); code != http.StatusUnauthorized {
		t.Errorf("no api key: status %d, want 401", code)
	}
	if code, _ := call(t, a, a.ibanVerify, http.MethodPost, "/x", `{"IBAN":"GB00X"}`, nil); code != http.StatusUnauthorized {
		t.Errorf("iban without a key: status %d, want 401", code)
	}
	if code, _ := call(t, a, a.creditsafeAuthenticate, http.MethodPost, "/x", `{"username":""}`, nil); code != http.StatusUnauthorized {
		t.Errorf("creditsafe with no credentials: status %d, want 401", code)
	}
}

// nested walks two levels of a decoded JSON object, returning an empty map
// rather than panicking so a failure reports the shape it got.
func nested(m map[string]any, a, b string) map[string]any {
	first, _ := m[a].(map[string]any)
	if first == nil {
		return map[string]any{}
	}
	second, _ := first[b].(map[string]any)
	if second == nil {
		return map[string]any{}
	}
	return second
}

// surfaces builds verification's two surfaces with the control port on its
// default and open to every caller, whatever the environment says.
func surfaces(t *testing.T, a *app) (svc *http.ServeMux, ctl *simctl.Control) {
	t.Helper()
	t.Setenv("SIM_CONTROL_LISTEN", "")
	t.Setenv("SIM_CONTROL_ALLOW", "")
	svc, ctl, err := a.routes(":8089")
	if err != nil {
		t.Fatal(err)
	}
	return svc, ctl
}

// serve sends one request to h and returns the recorder.
func serve(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

// resetVia posts a reset to the control port, as the lab API does, and
// decodes the answer.
func resetVia(t *testing.T, a *app, scope string) (cleared map[string]int, kept []string) {
	t.Helper()
	_, ctl := surfaces(t, a)
	w := serve(ctl.Handler(), http.MethodPost, "/sim/reset", `{"scope":"`+scope+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("reset %s: %d %s", scope, w.Code, w.Body)
	}
	var got struct {
		Cleared map[string]int `json:"cleared"`
		Kept    []string       `json:"kept"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got.Cleared, got.Kept
}

// TestFlowResetPutsOutcomesBackAndKeepsTokens: a flipped check is a run's
// trap and goes; a token the platform was issued is what it set up once,
// and still opens the bearer routes after the reset.
func TestFlowResetPutsOutcomesBackAndKeepsTokens(t *testing.T) {
	a := newApp()
	cs := token(t, a, a.creditsafeAuthenticate, "/creditsafe/authenticate", `{"username":"u","password":"p"}`)
	call(t, a, a.creditsafeCreatePortfolio, http.MethodPost, "/x", `{"name":"p"}`, bearerHdr(cs))
	for check, outcome := range map[string]string{checkScreening: "hit", checkIdentity: "fail"} {
		if err := a.outcomes(tenant.Default).set(check, outcome); err != nil {
			t.Fatal(err)
		}
	}

	cleared, kept := resetVia(t, a, "flow")
	if len(cleared) != 1 || cleared["outcomes"] != 2 {
		t.Errorf("cleared = %v, want two outcomes back to their defaults", cleared)
	}
	if strings.Join(kept, ",") != "tokens,portfolios" {
		t.Errorf("kept = %v", kept)
	}
	for check, v := range newOutcomes().m {
		if got := a.outcomes(tenant.Default).get(check); got != v {
			t.Errorf("%s = %q after a reset, want its default %q", check, got, v)
		}
	}
	if code, out := call(t, a, a.bearer(a.lexisIDU), http.MethodPost, "/x", `{}`, bearerHdr(cs)); code != http.StatusOK {
		t.Errorf("a token issued before a flow reset: %d %v", code, out)
	}
	if _, out := call(t, a, a.creditsafeCreatePortfolio, http.MethodPost, "/x", `{"name":"q"}`, bearerHdr(cs)); out["portfolioId"] != float64(2) {
		t.Errorf("portfolio ids after a flow reset: %v, want 2", out["portfolioId"])
	}
}

// TestAllResetIsFirstStart: no token issued before it is valid, and
// portfolio ids count from 1 again.
func TestAllResetIsFirstStart(t *testing.T) {
	a := newApp()
	cs := token(t, a, a.creditsafeAuthenticate, "/creditsafe/authenticate", `{"username":"u","password":"p"}`)
	token(t, a, a.lexisToken, "/lexisnexis/oauth/token", `{"client_id":"c","client_secret":"s"}`)
	call(t, a, a.creditsafeCreatePortfolio, http.MethodPost, "/x", `{"name":"p"}`, bearerHdr(cs))
	if err := a.outcomes(tenant.Default).set(checkCompany, "dissolved"); err != nil {
		t.Fatal(err)
	}

	cleared, kept := resetVia(t, a, "all")
	if cleared["outcomes"] != 1 || cleared["tokens"] != 2 || cleared["portfolios"] != 1 || len(kept) != 0 {
		t.Errorf("cleared = %v, kept = %v", cleared, kept)
	}
	if a.outcomes(tenant.Default).get(checkCompany) != "active" {
		t.Error("company should be back to active")
	}
	if code, _ := call(t, a, a.bearer(a.lexisIDU), http.MethodPost, "/x", `{}`, bearerHdr(cs)); code != http.StatusUnauthorized {
		t.Errorf("a token issued before an all reset: %d, want 401", code)
	}
	cs = token(t, a, a.creditsafeAuthenticate, "/creditsafe/authenticate", `{"username":"u","password":"p"}`)
	if _, out := call(t, a, a.creditsafeCreatePortfolio, http.MethodPost, "/x", `{"name":"q"}`, bearerHdr(cs)); out["portfolioId"] != float64(1) {
		t.Errorf("portfolio ids after an all reset: %v, want 1", out["portfolioId"])
	}
}

// TestTheControlPortServesEverySimRoute: the outcome switches and the
// control contract are on the control port, with no vendor auth, and no
// vendor route is; the vendor port still answers the old /sim paths the
// same way until phase 4.
func TestTheControlPortServesEverySimRoute(t *testing.T) {
	a := newApp()
	svc, ctl := surfaces(t, a)
	if ctl.Addr() != ":9089" {
		t.Errorf("control address = %q, want :9089", ctl.Addr())
	}

	if w := serve(ctl.Handler(), "GET", "/health", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"surface":"control"`) {
		t.Errorf("control GET /health = %d %s", w.Code, w.Body)
	}
	for _, r := range []struct{ method, path, body string }{
		{"GET", "/sim/outcome", ""},
		{"POST", "/sim/outcome", `{"check":"screening","outcome":"hit"}`},
		{"GET", "/sim/info", ""},
		{"GET", "/sim/logs", ""},
		{"POST", "/sim/reset", `{"scope":"flow"}`},
	} {
		on := serve(ctl.Handler(), r.method, r.path, r.body)
		if on.Code == 404 || on.Code == 405 {
			t.Errorf("control %s %s = %d %s", r.method, r.path, on.Code, on.Body)
		}
		if alias := serve(svc, r.method, r.path, r.body); alias.Code != on.Code {
			t.Errorf("vendor port %s %s = %d, want %d as on the control port", r.method, r.path, alias.Code, on.Code)
		}
	}

	// A flip on the control port is the vendor port's answer.
	serve(ctl.Handler(), "POST", "/sim/outcome", `{"check":"identity","outcome":"refer"}`)
	if got := a.outcomes(tenant.Default).get(checkIdentity); got != "refer" {
		t.Errorf("identity after a flip on the control port = %q", got)
	}

	for _, r := range []struct{ method, path string }{
		{"POST", "/creditsafe/authenticate"},
		{"POST", "/iban/verify"},
		{"POST", "/lexisnexis/oauth/token"},
	} {
		if w := serve(ctl.Handler(), r.method, r.path, `{}`); w.Code != 404 {
			t.Errorf("control %s %s = %d, want 404: vendor routes are not on the control port", r.method, r.path, w.Code)
		}
	}
}
