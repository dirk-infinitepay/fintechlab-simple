package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/simctl"
)

// surfaces builds payment-api's two surfaces with the control port on its
// default and open to every caller, whatever the environment says.
func surfaces(t *testing.T, s *store, upstream string, client *http.Client) (svc *http.ServeMux, ctl *simctl.Control) {
	t.Helper()
	t.Setenv("SIM_CONTROL_LISTEN", "")
	t.Setenv("SIM_CONTROL_ALLOW", "")
	svc, ctl, err := routes(s, client, upstream, upstream, ":8080")
	if err != nil {
		t.Fatal(err)
	}
	return svc, ctl
}

// stub answers the bank's POST /transfers and the notifier's POST /enqueue
// with a 2xx, after hold returns.
func stub(t *testing.T, hold func()) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hold != nil && r.URL.Path == "/transfers" {
			hold()
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const pay = `{"debtorAccountId":"acc_alice","creditorIban":"GB00SIM0000000000003","amount":"25.00"}`

// TestResetForgetsPaymentsAndTheirKeys: after a reset there are no
// payments, the activity log is empty, and a key seen before creates a new
// payment rather than replaying the old one.
func TestResetForgetsPaymentsAndTheirKeys(t *testing.T) {
	up := stub(t, nil)
	s := newStore()
	mux, ctl := surfaces(t, s, up.URL, up.Client())

	key := map[string]string{"Idempotency-Key": "k-1"}
	first := do(t, mux, "POST", "/payments", pay, key)
	if first.Code != 201 {
		t.Fatalf("payment: %d %s", first.Code, first.Body)
	}
	if w := do(t, mux, "POST", "/payments", pay, nil); w.Code != 201 {
		t.Fatalf("payment: %d %s", w.Code, w.Body)
	}

	w := do(t, ctl.Handler(), "POST", "/sim/reset", "", nil)
	var got struct {
		Cleared map[string]int `json:"cleared"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
	if got.Cleared["payments"] != 2 || got.Cleared["idempotency_keys"] != 1 || got.Cleared["activity"] != 2 {
		t.Errorf("cleared = %v", got.Cleared)
	}
	if body := do(t, mux, "GET", "/payments", "", nil).Body.String(); !strings.Contains(body, `"payments":[]`) {
		t.Errorf("payments after reset = %s", body)
	}
	if body := do(t, ctl.Handler(), "GET", "/sim/activity", "", nil).Body.String(); !strings.Contains(body, `"total":0`) {
		t.Errorf("activity after reset = %s", body)
	}
	again := do(t, mux, "POST", "/payments", pay, key)
	if again.Code != 201 || again.Body.String() == first.Body.String() {
		t.Errorf("the key after a reset: %d %s, want a new payment", again.Code, again.Body)
	}
}

// TestAPaymentInFlightAcrossAResetIsNotKept: a POST /payments still
// waiting on the bank when the reset lands does not store its payment after
// the reset.
func TestAPaymentInFlightAcrossAResetIsNotKept(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	up := stub(t, func() { close(arrived); <-release })
	s := newStore()
	mux, ctl := surfaces(t, s, up.URL, up.Client())

	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- do(t, mux, "POST", "/payments", pay, map[string]string{"Idempotency-Key": "k-2"}) }()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the payment never reached the bank")
	}
	if w := do(t, ctl.Handler(), "POST", "/sim/reset", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"payments":0`) {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
	close(release)
	if w := <-done; w.Code != 201 {
		t.Fatalf("payment: %d %s", w.Code, w.Body)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.byID) != 0 || len(s.byKey) != 0 {
		t.Errorf("a payment from before the reset was stored after it: %v", s.byID)
	}
}

// TestTheControlPortServesEverySimRoute: the activity log and the control
// contract are on the control port and the payment routes are not; the
// service port still answers the old /sim paths the same way until phase 4.
func TestTheControlPortServesEverySimRoute(t *testing.T) {
	up := stub(t, nil)
	svc, ctl := surfaces(t, newStore(), up.URL, up.Client())
	if ctl.Addr() != ":9080" {
		t.Errorf("control address = %q, want :9080", ctl.Addr())
	}

	if w := do(t, ctl.Handler(), "GET", "/health", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"surface":"control"`) {
		t.Errorf("control GET /health = %d %s", w.Code, w.Body)
	}
	if w := do(t, svc, "POST", "/payments", pay, nil); w.Code != 201 {
		t.Fatalf("payment: %d %s", w.Code, w.Body)
	}
	// The payment on the service port is in the log the control port reads.
	if w := do(t, ctl.Handler(), "GET", "/sim/activity", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"total":1`) {
		t.Errorf("control GET /sim/activity = %d %s", w.Code, w.Body)
	}
	for _, r := range []struct{ method, path, body string }{
		{"GET", "/sim/activity", ""},
		{"GET", "/sim/info", ""},
		{"GET", "/sim/logs", ""},
		{"POST", "/sim/reset", `{"scope":"flow"}`},
	} {
		on := do(t, ctl.Handler(), r.method, r.path, r.body, nil)
		if on.Code == 404 || on.Code == 405 {
			t.Errorf("control %s %s = %d %s", r.method, r.path, on.Code, on.Body)
		}
		if alias := do(t, svc, r.method, r.path, r.body, nil); alias.Code != on.Code {
			t.Errorf("service %s %s = %d, want %d as on the control port", r.method, r.path, alias.Code, on.Code)
		}
	}
	for _, r := range []struct{ method, path string }{
		{"POST", "/payments"},
		{"GET", "/payments"},
	} {
		if w := do(t, ctl.Handler(), r.method, r.path, pay, nil); w.Code != 404 {
			t.Errorf("control %s %s = %d, want 404", r.method, r.path, w.Code)
		}
	}
}
