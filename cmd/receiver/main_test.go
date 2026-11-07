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
)

var secret = []byte("test-secret")

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
	mux := s.routes(secret, ":8443")

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

	w := do(t, mux, "POST", "/sim/reset", `{"scope":"flow"}`, nil)
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
	if w := do(t, mux, "POST", "/sim/reset", `{"scope":"all"}`, nil); w.Code != 200 ||
		!strings.Contains(w.Body.String(), `"cleared":{"events":0,"raw_events":0}`) {
		t.Errorf("second reset = %d %s", w.Code, w.Body)
	}
}
