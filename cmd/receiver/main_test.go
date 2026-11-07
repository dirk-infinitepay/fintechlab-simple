package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/hmacx"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
)

var secret = []byte("test-secret")

// surfaces builds the receiver's two surfaces with the control port on its
// default and open to every caller, whatever the environment says.
func surfaces(t *testing.T, s *store) (svc *http.ServeMux, ctl *simctl.Control) {
	t.Helper()
	t.Setenv("SIM_CONTROL_LISTEN", "")
	t.Setenv("SIM_CONTROL_ALLOW", "")
	svc, ctl, err := s.routes(secret, ":8443")
	if err != nil {
		t.Fatal(err)
	}
	return svc, ctl
}

func do(t *testing.T, h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func signed(body string) map[string]string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	return map[string]string{
		hmacx.HeaderTimestamp: ts,
		hmacx.HeaderSignature: hmacx.Sign(secret, []byte(ts), []byte(body)),
	}
}

// TestResetEmptiesBothStores: every stored event and raw capture goes, the
// counts say how many, and both reads answer null as on first start.
func TestResetEmptiesBothStores(t *testing.T) {
	s := &store{}
	mux, ctl := surfaces(t, s)

	for _, body := range []string{`{"id":"evt_1","type":"payment.accepted"}`, `{"id":"evt_2"}`} {
		if w := do(t, mux, "POST", "/webhooks", body, signed(body)); w.Code != 200 {
			t.Fatalf("webhook: %d %s", w.Code, w.Body)
		}
	}
	for i := 0; i < 3; i++ {
		if w := do(t, mux, "POST", "/raw-events?sub=x", "opaque", nil); w.Code != 200 {
			t.Fatalf("raw: %d %s", w.Code, w.Body)
		}
	}

	w := do(t, ctl.Handler(), "POST", "/sim/reset", `{"scope":"flow"}`, nil)
	var got struct {
		Cleared map[string]int `json:"cleared"`
		Kept    []string       `json:"kept"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
	if got.Cleared["events"] != 2 || got.Cleared["raw_events"] != 3 || len(got.Kept) != 0 {
		t.Errorf("reset = %s", w.Body)
	}
	if body := do(t, mux, "GET", "/events", "", nil).Body.String(); !strings.Contains(body, `"events":null`) {
		t.Errorf("events after reset = %s", body)
	}
	if body := do(t, mux, "GET", "/raw-events", "", nil).Body.String(); !strings.Contains(body, `"raw_events":null`) {
		t.Errorf("raw events after reset = %s", body)
	}

	// all does the same; there is nothing left to clear.
	if w := do(t, ctl.Handler(), "POST", "/sim/reset", `{"scope":"all"}`, nil); w.Code != 200 ||
		!strings.Contains(w.Body.String(), `"cleared":{"events":0,"raw_events":0}`) {
		t.Errorf("second reset = %d %s", w.Code, w.Body)
	}
}

// TestTheControlPortServesEverySimRoute: the control contract is on the
// control port and nothing a sender posts to is; the HTTPS port still
// answers the old /sim paths the same way until phase 4. What the stores
// hold stays on the HTTPS port, where the e2e suite reads it.
func TestTheControlPortServesEverySimRoute(t *testing.T) {
	s := &store{}
	svc, ctl := surfaces(t, s)
	if ctl.Addr() != ":9443" {
		t.Errorf("control address = %q, want :9443", ctl.Addr())
	}

	if w := do(t, ctl.Handler(), "GET", "/health", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"surface":"control"`) {
		t.Errorf("control GET /health = %d %s", w.Code, w.Body)
	}
	for _, r := range []struct{ method, path, body string }{
		{"GET", "/sim/info", ""},
		{"GET", "/sim/logs", ""},
		{"POST", "/sim/reset", `{"scope":"flow"}`},
	} {
		on := do(t, ctl.Handler(), r.method, r.path, r.body, nil)
		if on.Code == 404 || on.Code == 405 {
			t.Errorf("control %s %s = %d %s", r.method, r.path, on.Code, on.Body)
		}
		if alias := do(t, svc, r.method, r.path, r.body, nil); alias.Code != on.Code {
			t.Errorf("HTTPS port %s %s = %d, want %d as on the control port", r.method, r.path, alias.Code, on.Code)
		}
	}
	for _, r := range []struct{ method, path string }{
		{"POST", "/webhooks"},
		{"POST", "/raw-events"},
		{"GET", "/events"},
		{"GET", "/raw-events"},
	} {
		if w := do(t, ctl.Handler(), r.method, r.path, "{}", nil); w.Code != 404 {
			t.Errorf("control %s %s = %d, want 404", r.method, r.path, w.Code)
		}
	}
	if len(s.events) != 0 || len(s.rawEvents) != 0 {
		t.Errorf("the control port stored something: %d events, %d raw", len(s.events), len(s.rawEvents))
	}
}
