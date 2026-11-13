package vendorsim

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const sbxCustomer = `{"env": "sbx", "name": "Sandbox", "verification": {"username": "sbx-user", "password": "sbx-secret"}}`

// testSim is a vendor with one check, a login, a bearer route and a key
// route: what every verification vendor's sim is made of.
func testSim(t *testing.T) (svc, ctl http.Handler, s *Sim) {
	t.Helper()
	t.Setenv("SIM_CONTROL_LISTEN", "")
	t.Setenv("SIM_CONTROL_ALLOW", "")
	s, err := New(Config{
		Service: "acme", Simulates: "Acme",
		Checks: []Check{{Name: "verdict", Outcomes: []string{"pass", "fail"}}},
	}, ":8099")
	if err != nil {
		t.Fatal(err)
	}
	answer := func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"verdict": s.Answer(r, "verdict")})
	}
	s.Vendor("POST /acme/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		env, ok := s.ByLogin(Str(body["username"]), Str(body["password"]))
		if !ok {
			http.Error(w, `{"error":"invalid login"}`, http.StatusUnauthorized)
			return
		}
		s.As(r, env)
		writeJSON(w, map[string]any{"token": s.Issue("acme", env)})
	})
	s.Vendor("POST /acme/bearer", s.Bearer(answer))
	s.Vendor("POST /acme/key", s.APIKey(answer))
	vendor, control := s.Surfaces()
	return vendor, control.Handler(), s
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

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

func login(t *testing.T, svc http.Handler, user, pass string) string {
	t.Helper()
	code, out := do(t, svc, "POST", "/acme/login", `{"username":"`+user+`","password":"`+pass+`"}`, nil)
	tok, _ := out["token"].(string)
	if code != 200 || tok == "" {
		t.Fatalf("login %s: %d %v", user, code, out)
	}
	return tok
}

func bearer(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }
func key(k string) map[string]string      { return map[string]string{"x-api-key": k} }

func TestEveryCheckStartsAtItsFirstOutcomeAndCanBeFlipped(t *testing.T) {
	svc, ctl, _ := testSim(t)
	if _, out := do(t, svc, "POST", "/acme/key", `{}`, key("k")); out["verdict"] != "pass" {
		t.Fatalf("default verdict = %v, want pass", out)
	}
	if code, out := do(t, ctl, "POST", "/sim/outcome", `{"check":"verdict","outcome":"fail"}`, nil); code != 200 {
		t.Fatalf("flip: %d %v", code, out)
	}
	if _, out := do(t, svc, "POST", "/acme/key", `{}`, key("k")); out["verdict"] != "fail" {
		t.Errorf("verdict after the flip = %v, want fail", out)
	}
	for body, want := range map[string]string{
		`{"check":"verdict","outcome":"maybe"}`:      "cannot be",
		`{"check":"screening","outcome":"hit"}`:      "unknown check",
		`{"check":"verdict","outcome":"fail","x":1}`: "unknown field",
	} {
		if code, out := do(t, ctl, "POST", "/sim/outcome", body, nil); code != 400 || !strings.Contains(out["error"].(string), want) {
			t.Errorf("%s: %d %v, want 400 about %q", body, code, out, want)
		}
	}
}

func TestAuthIsReal(t *testing.T) {
	svc, _, _ := testSim(t)
	if code, _ := do(t, svc, "POST", "/acme/bearer", `{}`, nil); code != 401 {
		t.Errorf("no bearer: %d, want 401", code)
	}
	if code, _ := do(t, svc, "POST", "/acme/bearer", `{}`, bearer("made-up")); code != 401 {
		t.Errorf("unknown bearer: %d, want 401", code)
	}
	if code, _ := do(t, svc, "POST", "/acme/key", `{}`, nil); code != 401 {
		t.Errorf("no key: %d, want 401", code)
	}
	tok := login(t, svc, "anyone", "anything")
	if code, out := do(t, svc, "POST", "/acme/bearer", `{}`, bearer(tok)); code != 200 || out["verdict"] != "pass" {
		t.Errorf("issued bearer: %d %v", code, out)
	}
}

func TestEachEnvironmentHasItsOwnOutcomesTokensAndKeys(t *testing.T) {
	svc, ctl, _ := testSim(t)
	if code, out := do(t, ctl, "PUT", "/sim/customers/sbx", sbxCustomer, nil); code != 200 {
		t.Fatalf("PUT sbx: %d %v", code, out)
	}
	if code, out := do(t, ctl, "POST", "/sim/outcome?env=sbx", `{"check":"verdict","outcome":"fail"}`, nil); code != 200 || out["env"] != "sbx" {
		t.Fatalf("flip in sbx: %d %v", code, out)
	}
	if _, out := do(t, svc, "POST", "/acme/key", `{}`, key("sbx-secret")); out["verdict"] != "fail" {
		t.Errorf("sbx's key = %v, want sbx's fail", out)
	}
	if _, out := do(t, svc, "POST", "/acme/key", `{}`, key("other")); out["verdict"] != "pass" {
		t.Errorf("another key = %v, want the default's pass", out)
	}
	sbx := login(t, svc, "sbx-user", "sbx-secret")
	if _, out := do(t, svc, "POST", "/acme/bearer", `{}`, bearer(sbx)); out["verdict"] != "fail" {
		t.Errorf("sbx's token = %v, want sbx's fail", out)
	}
	if code, _ := do(t, svc, "POST", "/acme/login", `{"username":"sbx-user","password":"wrong"}`, nil); code != 401 {
		t.Errorf("sbx's username with another password: %d, want 401", code)
	}
	if code, _ := do(t, ctl, "GET", "/sim/outcome?env=nope", "", nil); code != 404 {
		t.Errorf("an environment never given: %d, want 404", code)
	}
}

func TestAFlowResetKeepsTokensAndAnAllResetDropsThem(t *testing.T) {
	svc, ctl, _ := testSim(t)
	tok := login(t, svc, "anyone", "anything")
	do(t, ctl, "POST", "/sim/outcome", `{"check":"verdict","outcome":"fail"}`, nil)

	code, out := do(t, ctl, "POST", "/sim/reset", `{"scope":"flow"}`, nil)
	cleared, _ := out["cleared"].(map[string]any)
	if code != 200 || cleared["outcomes"] != float64(1) {
		t.Fatalf("flow reset: %d %v", code, out)
	}
	if _, out := do(t, svc, "POST", "/acme/bearer", `{}`, bearer(tok)); out["verdict"] != "pass" {
		t.Errorf("after a flow reset: %v, want the token still valid and the verdict back to pass", out)
	}
	code, out = do(t, ctl, "POST", "/sim/reset", `{"scope":"all"}`, nil)
	if cleared, _ := out["cleared"].(map[string]any); code != 200 || cleared["tokens"] != float64(1) {
		t.Fatalf("all reset: %d %v", code, out)
	}
	if code, _ := do(t, svc, "POST", "/acme/bearer", `{}`, bearer(tok)); code != 401 {
		t.Errorf("a token from before an all reset: %d, want 401", code)
	}
}

func TestTheJournalFilesEveryRequestUnderItsEnvironment(t *testing.T) {
	svc, ctl, _ := testSim(t)
	do(t, ctl, "PUT", "/sim/customers/sbx", sbxCustomer, nil)
	login(t, svc, "sbx-user", "sbx-secret")
	do(t, svc, "POST", "/acme/key", `{}`, key("k"))

	count := func(env string) int {
		var got struct {
			Logs []struct {
				Events []struct {
					Op string `json:"op"`
				} `json:"events"`
			} `json:"logs"`
		}
		r := httptest.NewRequest("GET", "/sim/activity?env="+env, nil)
		w := httptest.NewRecorder()
		ctl.ServeHTTP(w, r)
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		for _, e := range got.Logs[0].Events {
			if e.Op != "acme" {
				t.Errorf("event filed under %q, want acme", e.Op)
			}
		}
		return len(got.Logs[0].Events)
	}
	if sbx, def := count("sbx"), count("default"); sbx != 1 || def != 1 {
		t.Errorf("journal: sbx %d, default %d, want one each", sbx, def)
	}
	_, out := do(t, ctl, "POST", "/sim/reset", `{"scope":"flow","env":"sbx"}`, nil)
	if cleared, _ := out["cleared"].(map[string]any); cleared["activity"] != float64(1) {
		t.Errorf("sbx's reset cleared %v, want its one journal event as activity", out["cleared"])
	}
	if sbx, def := count("sbx"), count("default"); sbx != 0 || def != 1 {
		t.Errorf("after sbx's reset: sbx %d, default %d", sbx, def)
	}

	login(t, svc, "sbx-user", "sbx-secret")
	do(t, ctl, "DELETE", "/sim/customers/sbx", "", nil)
	do(t, ctl, "PUT", "/sim/customers/sbx", sbxCustomer, nil)
	if sbx := count("sbx"); sbx != 0 {
		t.Errorf("a recreated sbx's journal: %d, want none of the old sbx's requests", sbx)
	}
	if code, _ := do(t, ctl, "GET", "/sim/activity?env=sbz", "", nil); code != 404 {
		t.Errorf("GET /sim/activity for an environment never given: %d, want 404", code)
	}
}

func TestADroppedConnectionIsJournalled(t *testing.T) {
	svc, ctl, _ := testSim(t)
	srv := httptest.NewServer(svc)
	t.Cleanup(srv.Close)
	if code, out := do(t, ctl, "POST", "/sim/traps", `{"hook":"http.request","match":{"path":"/acme/key"},"effect":{"close":true}}`, nil); code != 201 {
		t.Fatalf("arm: %d %v", code, out)
	}
	r, _ := http.NewRequest("POST", srv.URL+"/acme/key", strings.NewReader(`{}`))
	r.Header.Set("x-api-key", "k")
	if res, err := http.DefaultClient.Do(r); err == nil {
		t.Fatalf("a dropped connection answered %d", res.StatusCode)
	}

	_, out := do(t, ctl, "GET", "/sim/activity", "", nil)
	logs, _ := out["logs"].([]any)
	journal, _ := logs[0].(map[string]any)
	events, _ := journal["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("journal = %v, want the dropped request", journal)
	}
	event, _ := events[0].(map[string]any)
	if event["status"] != "bad" || !strings.Contains(event["summary"].(string), "connection dropped") {
		t.Errorf("dropped request = %v, want it bad, saying the connection was dropped", event)
	}
}

func TestATrapAnswersInsteadOfTheVendorAndNeverLocksTheLabOut(t *testing.T) {
	svc, ctl, _ := testSim(t)
	srv := httptest.NewServer(svc)
	t.Cleanup(srv.Close)
	arm := func(body string) {
		t.Helper()
		r := httptest.NewRequest("POST", "/sim/traps", strings.NewReader(body))
		w := httptest.NewRecorder()
		ctl.ServeHTTP(w, r)
		if w.Code != 201 {
			t.Fatalf("arm: %d %s", w.Code, w.Body)
		}
	}
	post := func(path string) (*http.Response, error) {
		r, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(`{}`))
		r.Header.Set("x-api-key", "k")
		return http.DefaultClient.Do(r)
	}

	arm(`{"hook":"http.request","match":{"path":"/acme/key"},"effect":{"respond":{"status":200,"body":"<html>oops</html>"}}}`)
	res, err := post("/acme/key")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != "<html>oops</html>" {
		t.Errorf("trapped answer = %q", body)
	}

	arm(`{"hook":"http.request","match":{"path":"/acme/*"},"effect":{"close":true}}`)
	if res, err := post("/acme/key"); err == nil {
		t.Errorf("a dropped connection answered %d", res.StatusCode)
	}

	arm(`{"hook":"http.request","match":{},"effect":{"respond":{"status":503}},"times":null}`)
	for _, path := range []string{"/health", "/sim/outcome"} {
		res, err := http.Get(srv.URL + path)
		if err != nil || res.StatusCode != 200 {
			t.Errorf("GET %s with a trap on everything: %v %v", path, res, err)
		}
	}
}

func TestATrappedRequestIsJournalledWithTheTrapsAnswer(t *testing.T) {
	svc, ctl, _ := testSim(t)
	r := httptest.NewRequest("POST", "/sim/traps", strings.NewReader(`{"hook":"http.request","match":{"path":"/acme/key"},"effect":{"respond":{"status":503,"body":{"error":"down"}}}}`))
	w := httptest.NewRecorder()
	ctl.ServeHTTP(w, r)
	if code, _ := do(t, svc, "POST", "/acme/key", `{}`, key("k")); code != 503 {
		t.Fatalf("trapped request: %d, want 503", code)
	}
	var got struct {
		Logs []struct {
			Name   string `json:"name"`
			Events []struct {
				Op      string `json:"op"`
				Summary string `json:"summary"`
				Status  string `json:"status"`
			} `json:"events"`
		} `json:"logs"`
	}
	w = httptest.NewRecorder()
	ctl.ServeHTTP(w, httptest.NewRequest("GET", "/sim/activity", nil))
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.Logs) != 2 {
		t.Fatalf("activity = %s, want the journal and the traps log", w.Body)
	}
	requests, traps := got.Logs[0], got.Logs[1]
	if requests.Name != "requests" || len(requests.Events) != 1 || !strings.Contains(requests.Events[0].Summary, "down") {
		t.Errorf("journal = %+v, want the trapped request with the trap's error", requests)
	}
	if traps.Name != "traps" || len(traps.Events) == 0 {
		t.Errorf("traps log = %+v", traps)
	}
}

func TestDeletingAnEnvironmentDisarmsItsTraps(t *testing.T) {
	svc, ctl, _ := testSim(t)
	do(t, ctl, "PUT", "/sim/customers/sbx", sbxCustomer, nil)
	if code, out := do(t, ctl, "POST", "/sim/traps?env=sbx", `{"hook":"http.request","match":{},"effect":{"respond":{"status":503}},"times":null}`, nil); code != 201 {
		t.Fatalf("arm in sbx: %d %v", code, out)
	}
	do(t, ctl, "DELETE", "/sim/customers/sbx", "", nil)
	do(t, ctl, "PUT", "/sim/customers/sbx", sbxCustomer, nil)
	if code, out := do(t, svc, "POST", "/acme/key", `{}`, key("sbx-secret")); code != 200 {
		t.Errorf("a recreated sbx: %d %v, want the vendor's 200, not the old trap", code, out)
	}
}

func TestDisarmingLetsADelayedRequestThrough(t *testing.T) {
	svc, ctl, _ := testSim(t)
	srv := httptest.NewServer(svc)
	t.Cleanup(srv.Close)
	r := httptest.NewRequest("POST", "/sim/traps", strings.NewReader(`{"hook":"http.request","match":{},"effect":{"delay":"1h"}}`))
	w := httptest.NewRecorder()
	ctl.ServeHTTP(w, r)

	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest("POST", srv.URL+"/acme/key", strings.NewReader(`{}`))
		req.Header.Set("x-api-key", "k")
		if res, err := http.DefaultClient.Do(req); err == nil {
			done <- res.StatusCode
		}
	}()
	time.Sleep(100 * time.Millisecond)
	w = httptest.NewRecorder()
	ctl.ServeHTTP(w, httptest.NewRequest("DELETE", "/sim/traps", nil))
	select {
	case code := <-done:
		if code != 200 {
			t.Errorf("the delayed request: %d, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the delayed request was still held after the traps were disarmed")
	}
}

func TestTheVendorPortStillAnswersTheSimPaths(t *testing.T) {
	svc, _, _ := testSim(t)
	if code, out := do(t, svc, "GET", "/sim/outcome", "", nil); code != 200 || out["outcomes"] == nil {
		t.Errorf("GET /sim/outcome on the vendor port: %d %v", code, out)
	}
	if code, out := do(t, svc, "GET", "/sim/info", "", nil); code != 200 || out["service"] != "acme" {
		t.Errorf("GET /sim/info: %d %v", code, out)
	}
}
