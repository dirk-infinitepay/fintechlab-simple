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

// as sends one request with the given x-internal-api-key.
func as(t *testing.T, h http.Handler, key, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("x-internal-api-key", key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// decided polls key's decision for id until it is readable.
func decided(t *testing.T, h http.Handler, key string, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		w := as(t, h, key, "GET", "/api/v1/verification/verify/data/verify-decision/"+id, "")
		if w.Code == 200 {
			var out map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &out)
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("decision %s with key %s never readable: %d %s", id, key, w.Code, w.Body)
		}
		time.Sleep(time.Millisecond)
	}
}

// withSbx is verify with sbx pushed as a customer, its key "sbx-key", and
// merchantApplicationId 7 force-declined (VERIFY_FORCE_DECLINE_IDS, the
// default's).
func withSbx(t *testing.T) (svc, ctl http.Handler) {
	t.Helper()
	a := &app{engine: verify.NewEngine(0, map[int]bool{7: true}), apiKey: "k"}
	mux, c := surfaces(t, a)
	if w := as(t, c.Handler(), "", "PUT", "/sim/customers/sbx", `{"env":"sbx","verify":{"key":"sbx-key"}}`); w.Code != 200 {
		t.Fatalf("PUT /sim/customers/sbx = %d %s", w.Code, w.Body)
	}
	return mux, c.Handler()
}

// Two environments deciding the same merchantApplicationId each read
// their own decision; the force-decline list is the default's alone.
func TestEachEnvironmentReadsItsOwnDecisions(t *testing.T) {
	svc, ctl := withSbx(t)
	for _, key := range []string{"k", "sbx-key"} {
		if w := as(t, svc, key, "POST", "/api/v1/verification/verify/aml-decision", `{"merchantApplicationId": 7}`); w.Code != 202 {
			t.Fatalf("trigger with %s: %d %s", key, w.Code, w.Body)
		}
	}
	if got := decided(t, svc, "k", "7")["overallDecision"]; got != "DECLINE" {
		t.Errorf("the default's 7 = %v, want DECLINE (VERIFY_FORCE_DECLINE_IDS)", got)
	}
	if got := decided(t, svc, "sbx-key", "7")["overallDecision"]; got != "APPROVED" {
		t.Errorf("sbx's 7 = %v, want APPROVED: the force-decline list is the default's", got)
	}

	// A decision triggered in one environment is not the other's.
	as(t, svc, "sbx-key", "POST", "/api/v1/verification/verify/aml-decision", `{"merchantApplicationId": 8}`)
	decided(t, svc, "sbx-key", "8")
	if w := as(t, svc, "k", "GET", "/api/v1/verification/verify/data/verify-decision/8", ""); w.Code != 404 {
		t.Errorf("the default reading sbx's decision 8 = %d, want 404", w.Code)
	}

	// The customer was seen by its calls.
	w := as(t, ctl, "", "GET", "/sim/customers", "")
	if !strings.Contains(w.Body.String(), `"env":"sbx"`) || strings.Contains(w.Body.String(), `"last_seen":null`) {
		t.Errorf("GET /sim/customers = %s, want both seen", w.Body)
	}
}

// A key nobody has is refused, as before: verify checks its key.
func TestAnUnknownKeyIsStillRefused(t *testing.T) {
	svc, _ := withSbx(t)
	for _, key := range []string{"", "nobody's"} {
		if w := as(t, svc, key, "POST", "/api/v1/verification/verify/aml-decision", `{"merchantApplicationId": 1}`); w.Code != 401 {
			t.Errorf("trigger with key %q = %d, want 401", key, w.Code)
		}
	}
}

// A reset of one environment forgets its decisions only; deleting a
// customer forgets them too, and its key opens nothing after.
func TestAResetOrDeleteOfOneEnvironmentKeepsTheOthersDecisions(t *testing.T) {
	svc, ctl := withSbx(t)
	for _, key := range []string{"k", "sbx-key"} {
		as(t, svc, key, "POST", "/api/v1/verification/verify/aml-decision", `{"merchantApplicationId": 9}`)
		decided(t, svc, key, "9")
	}
	w := as(t, ctl, "", "POST", "/sim/reset", `{"env": "sbx"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"decisions":1`) || !strings.Contains(w.Body.String(), `"env":"sbx"`) {
		t.Fatalf("reset sbx = %d %s", w.Code, w.Body)
	}
	if w := as(t, svc, "sbx-key", "GET", "/api/v1/verification/verify/data/verify-decision/9", ""); w.Code != 404 {
		t.Errorf("sbx's decision after its reset = %d, want 404", w.Code)
	}
	decided(t, svc, "k", "9") // the default's is still there

	as(t, svc, "sbx-key", "POST", "/api/v1/verification/verify/aml-decision", `{"merchantApplicationId": 9}`)
	decided(t, svc, "sbx-key", "9")
	if w := as(t, ctl, "", "DELETE", "/sim/customers/sbx", ""); w.Code != 200 {
		t.Fatalf("DELETE /sim/customers/sbx = %d %s", w.Code, w.Body)
	}
	if w := as(t, svc, "sbx-key", "GET", "/api/v1/verification/verify/data/verify-decision/9", ""); w.Code != 401 {
		t.Errorf("sbx's key after it was deleted = %d, want 401", w.Code)
	}
	decided(t, svc, "k", "9")

	// "*" resets every environment.
	if w := as(t, ctl, "", "POST", "/sim/reset", `{"env": "*"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"decisions":1`) {
		t.Errorf("reset * = %d %s", w.Code, w.Body)
	}
}
