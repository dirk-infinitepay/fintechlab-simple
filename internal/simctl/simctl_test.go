package simctl

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/webduvet/fintechlab-simple/internal/logring"
)

func mounted(t *testing.T, reset ResetFunc, scopes ...string) (http.Handler, *logring.Ring) {
	t.Helper()
	ring := logring.New(50)
	prev := log.Writer()
	log.SetOutput(ring)
	t.Cleanup(func() { log.SetOutput(prev) })
	mux := http.NewServeMux()
	Mount(mux, Config{Service: "test-sim", Kind: "vendor", Simulates: "a test", Scopes: scopes, Reset: reset, Logs: ring})
	return mux, ring
}

func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestResetDefaultsToFlowAndSaysWhatItClearedAndKept(t *testing.T) {
	var got ResetRequest
	h, ring := mounted(t, func(_ context.Context, req ResetRequest) (ResetResult, error) {
		got = req
		return ResetResult{Cleared: map[string]int{"payments": 3}, Kept: []string{"subscriptions"}}, nil
	})
	w := do(h, "POST", "/sim/reset", "")
	if w.Code != 200 || got.Scope != ScopeFlow {
		t.Fatalf("%d %s, scope %q", w.Code, w.Body, got.Scope)
	}
	var body struct {
		Service string         `json:"service"`
		Scope   string         `json:"scope"`
		Cleared map[string]int `json:"cleared"`
		Kept    []string       `json:"kept"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Service != "test-sim" || body.Scope != "flow" || body.Cleared["payments"] != 3 || len(body.Kept) != 1 {
		t.Errorf("answer = %+v", body)
	}
	// The log keeps a line marking the reset.
	lines := ring.Read(0, false, 10, "reset (flow)")
	if len(lines.Lines) != 1 || !strings.Contains(lines.Lines[0].Text, "payments=3") {
		t.Errorf("log lines = %+v", lines.Lines)
	}
}

func TestAScopeTheSimDoesNotHonourAndAMisspeltFieldAreRefused(t *testing.T) {
	called := false
	h, _ := mounted(t, func(context.Context, ResetRequest) (ResetResult, error) {
		called = true
		return ResetResult{}, nil
	}, ScopeFlow)
	for _, body := range []string{`{"scope":"all"}`, `{"scope":"everything"}`, `{"scop":"all"}`, `not json`} {
		if w := do(h, "POST", "/sim/reset", body); w.Code != 400 {
			t.Errorf("%s = %d, want 400", body, w.Code)
		}
	}
	if called {
		t.Error("a refused body reached the reset")
	}
}

func TestARefusalIs409AndForceReachesTheReset(t *testing.T) {
	h, _ := mounted(t, func(_ context.Context, req ResetRequest) (ResetResult, error) {
		if !req.Force {
			return ResetResult{}, Refusal{Reason: "the clock is held by runner"}
		}
		return ResetResult{}, nil
	})
	w := do(h, "POST", "/sim/reset", `{"scope":"flow"}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "held by runner") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := do(h, "POST", "/sim/reset", `{"scope":"flow","force":true}`); w.Code != 200 {
		t.Errorf("forced = %d", w.Code)
	}
}

func TestAFailedResetIs500WithTheReason(t *testing.T) {
	h, _ := mounted(t, func(context.Context, ResetRequest) (ResetResult, error) {
		return ResetResult{}, fmt.Errorf("remove %s: permission denied", "/sftp/x")
	})
	if w := do(h, "POST", "/sim/reset", ""); w.Code != 500 || !strings.Contains(w.Body.String(), "permission denied") {
		t.Errorf("%d %s", w.Code, w.Body)
	}
}

func TestInfoAndLogsAnswer(t *testing.T) {
	h, _ := mounted(t, func(context.Context, ResetRequest) (ResetResult, error) { return ResetResult{}, nil })
	log.Print("test-sim: hello")
	var info struct {
		Service string   `json:"service"`
		Scopes  []string `json:"reset_scopes"`
		Logs    struct {
			Held int `json:"held"`
		} `json:"logs"`
	}
	w := do(h, "GET", "/sim/info", "")
	_ = json.Unmarshal(w.Body.Bytes(), &info)
	if w.Code != 200 || info.Service != "test-sim" || len(info.Scopes) != 2 || info.Logs.Held != 1 {
		t.Errorf("info = %d %s", w.Code, w.Body)
	}
	if w := do(h, "GET", "/sim/logs?q=hello", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "test-sim: hello") {
		t.Errorf("logs = %d %s", w.Code, w.Body)
	}
}

func TestEpochSaysWhenAResetHappenedSinceWorkStarted(t *testing.T) {
	var e Epoch
	started := e.Current()
	if e.Moved(started) {
		t.Fatal("moved before any reset")
	}
	e.Bump()
	if !e.Moved(started) {
		t.Fatal("a reset was not noticed")
	}
}
