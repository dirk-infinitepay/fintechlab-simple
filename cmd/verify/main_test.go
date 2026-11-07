package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/verify"
)

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
	mux := a.routes(":8088")

	if w := do(t, mux, "POST", "/api/v1/verification/verify/aml-decision", `{"merchantApplicationId": 7}`); w.Code != 202 {
		t.Fatalf("trigger: %d %s", w.Code, w.Body)
	}
	w := do(t, mux, "POST", "/sim/reset", `{"scope": "all"}`)
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
	w := do(t, a.routes(":8088"), "GET", "/sim/info", "")
	var got struct {
		Service, Kind string
		Scopes        []string `json:"reset_scopes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Service != "verify" || got.Kind != "vendor" || len(got.Scopes) != 2 {
		t.Errorf("info = %d %s", w.Code, w.Body)
	}
}
