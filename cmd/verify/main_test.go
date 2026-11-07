package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/simctl"
	"github.com/webduvet/fintechlab-simple/internal/verify"
)

// surfaces builds verify's two surfaces with the control port on its
// default and open to every caller, whatever the environment says.
func surfaces(t *testing.T, a *app) (svc *http.ServeMux, ctl *simctl.Control) {
	t.Helper()
	t.Setenv("SIM_CONTROL_LISTEN", "")
	t.Setenv("SIM_CONTROL_ALLOW", "")
	svc, ctl, err := a.routes(":8088")
	if err != nil {
		t.Fatal(err)
	}
	return svc, ctl
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("x-internal-api-key", "k")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// TestResetDropsADecisionStillInItsDelay: the platform triggered a
// decision, the lab reset before the delay ran out, and the decision never
// becomes readable — the reset says it dropped one pending, and keeps the
// force-decline list.
func TestResetDropsADecisionStillInItsDelay(t *testing.T) {
	a := &app{engine: verify.NewEngine(time.Hour, map[int]bool{99: true}), apiKey: "k"}
	mux, ctl := surfaces(t, a)

	if w := do(t, mux, "POST", "/api/v1/verification/verify/aml-decision", `{"merchantApplicationId": 7}`); w.Code != 202 {
		t.Fatalf("trigger: %d %s", w.Code, w.Body)
	}
	w := do(t, ctl.Handler(), "POST", "/sim/reset", `{"scope": "all"}`)
	if w.Code != 200 {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
	var got struct {
		Service string         `json:"service"`
		Cleared map[string]int `json:"cleared"`
		Kept    []string       `json:"kept"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Service != "verify" || got.Cleared["decisions"] != 0 || got.Cleared["pending"] != 1 ||
		len(got.Kept) != 1 || got.Kept[0] != "force_decline_ids" {
		t.Errorf("reset = %s", w.Body)
	}
	if w := do(t, mux, "GET", "/api/v1/verification/verify/data/verify-decision/7", ""); w.Code != 404 {
		t.Errorf("decision after reset: %d %s, want 404", w.Code, w.Body)
	}
}

// TestSimInfoSaysWhatVerifyIs: the lab reads who a service is from here.
func TestSimInfoSaysWhatVerifyIs(t *testing.T) {
	a := &app{engine: verify.NewEngine(0, nil), apiKey: "k"}
	_, ctl := surfaces(t, a)
	w := do(t, ctl.Handler(), "GET", "/sim/info", "")
	var got struct {
		Service, Kind string
		Scopes        []string `json:"reset_scopes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Service != "verify" || got.Kind != "stand-in" || len(got.Scopes) != 2 {
		t.Errorf("info = %d %s", w.Code, w.Body)
	}
}

// TestTheControlPortServesEverySimRoute: the lab steers verify on the
// control port, with no x-internal-api-key, and the platform's routes are
// not there; the service port still answers the old /sim paths the same
// way until phase 4.
func TestTheControlPortServesEverySimRoute(t *testing.T) {
	a := &app{engine: verify.NewEngine(0, nil), apiKey: "k"}
	svc, ctl := surfaces(t, a)
	if ctl.Addr() != ":9088" {
		t.Errorf("control address = %q, want :9088", ctl.Addr())
	}

	if w := do(t, ctl.Handler(), "GET", "/health", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"surface":"control"`) {
		t.Errorf("control GET /health = %d %s", w.Code, w.Body)
	}
	for _, r := range []struct{ method, path, body string }{
		{"GET", "/sim/info", ""},
		{"GET", "/sim/logs", ""},
		{"POST", "/sim/reset", `{"scope": "flow"}`},
	} {
		on := do(t, ctl.Handler(), r.method, r.path, r.body)
		if on.Code == 404 || on.Code == 405 {
			t.Errorf("control %s %s = %d %s", r.method, r.path, on.Code, on.Body)
		}
		if alias := do(t, svc, r.method, r.path, r.body); alias.Code != on.Code {
			t.Errorf("service %s %s = %d, want %d as on the control port", r.method, r.path, alias.Code, on.Code)
		}
	}
	for _, r := range []struct{ method, path string }{
		{"GET", "/api/v1/verification/health"},
		{"POST", "/api/v1/verification/verify/aml-decision"},
	} {
		if w := do(t, ctl.Handler(), r.method, r.path, `{"merchantApplicationId": 7}`); w.Code != 404 {
			t.Errorf("control %s %s = %d, want 404: the platform's routes are not on the control port", r.method, r.path, w.Code)
		}
	}
}
