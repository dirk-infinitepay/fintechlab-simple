package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/aci"
	"github.com/webduvet/fintechlab-simple/internal/allowlist"
	"github.com/webduvet/fintechlab-simple/internal/labswitch"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// target stands in for the platform's webhook listener and counts the
// attempts that reach it.
type target struct {
	mu     sync.Mutex
	n      int
	status int
	srv    *httptest.Server
}

func newTarget(t *testing.T, status int) *target {
	tg := &target{status: status}
	tg.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		tg.mu.Lock()
		tg.n++
		tg.mu.Unlock()
		w.WriteHeader(tg.status)
	}))
	t.Cleanup(tg.srv.Close)
	return tg
}

func (tg *target) attempts() int {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	return tg.n
}

// testServer is ACI as main builds it, delivering to tg with a 100ms
// backoff between attempts, behind its own listener: the /internal routes
// and the old /sim paths.
func testServer(t *testing.T, tg *target) (*app, *httptest.Server) {
	t.Helper()
	a, vendor, _ := testServers(t, tg)
	return a, vendor
}

// testServers is testServer with the control port as well.
func testServers(t *testing.T, tg *target) (a *app, vendor, control *httptest.Server) {
	t.Helper()
	// Where the control port listens and who may call it come from the
	// environment; the one the tests run in must not leak in.
	t.Setenv("SIM_CONTROL_LISTEN", "")
	t.Setenv("SIM_CONTROL_ALLOW", "")
	list, err := allowlist.Parse("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	a = &app{
		secret:    []byte(strings.Repeat("k", 32)),
		client:    &http.Client{Timeout: 2 * time.Second},
		list:      list,
		backs:     []time.Duration{100 * time.Millisecond},
		maxTry:    3,
		targetURL: tg.srv.URL,
		bodyMode:  "raw",
		sent:      aci.NewSentLog(),
		wiring:    labswitch.New("aci", "delivers webhooks to ACI_WEBHOOK_TARGET_URL", true).Target(tg.srv.URL),
	}
	ctl, err := a.control(":8087")
	if err != nil {
		t.Fatal(err)
	}
	vendor = httptest.NewServer(a.routes(ctl.Mux))
	control = httptest.NewServer(ctl.Handler())
	t.Cleanup(vendor.Close)
	t.Cleanup(control.Close)
	return a, vendor, control
}

func call(t *testing.T, srv *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// The rule that matters most: a webhook waiting out its backoff when the
// reset lands is never retried, and its record is gone.
func TestARetryInFlightWhenTheResetLandsIsNeverSent(t *testing.T) {
	tg := newTarget(t, 500)
	a, srv := testServer(t, tg)
	if st, _ := call(t, srv, "POST", "/internal/simulate-payment", `{"merchant_transaction_id": "tx-1"}`); st != 202 {
		t.Fatalf("simulate-payment = %d", st)
	}
	waitFor(t, "the first attempt", func() bool { return tg.attempts() == 1 })

	st, res := call(t, srv, "POST", "/sim/reset", "")
	if st != 200 {
		t.Fatalf("POST /sim/reset = %d %v", st, res)
	}
	cleared, _ := res["cleared"].(map[string]any)
	if cleared["sent"] != 1.0 || cleared["deliveries_in_flight"] != 1.0 {
		t.Errorf("cleared = %v, want the one record and its retry", cleared)
	}
	waitFor(t, "the dropped delivery to stop", func() bool { return a.sent.InFlight() == 0 })
	if n := tg.attempts(); n != 1 {
		t.Fatalf("%d attempts reached the target, want the one before the reset", n)
	}
	if _, list := call(t, srv, "GET", "/internal/sent", ""); len(list["result"].([]any)) != 0 {
		t.Errorf("GET /internal/sent after a reset = %v", list)
	}
}

// A delivery whose record was put before a reset makes no attempt after it.
func TestADeliveryPutBeforeAResetIsNotSent(t *testing.T) {
	tg := newTarget(t, 200)
	a, _ := testServer(t, tg)
	rec := &aci.Record{ID: "aci_evt_1", TargetURL: tg.srv.URL, BodyMode: "raw", CiphertextHex: "00"}
	before := a.sent.Put(rec)
	a.reset(tenant.All)
	a.deliver(rec, before)
	if n := tg.attempts(); n != 0 {
		t.Fatalf("%d attempt(s) for a record the reset removed", n)
	}

	// The same delivery put after it goes through: the test can tell.
	a.deliver(rec, a.sent.Put(rec))
	if n := tg.attempts(); n != 1 {
		t.Fatalf("%d attempt(s) for a record put after the reset, want 1", n)
	}
}

// Both scopes do the same, and neither touches the delivery switch: it is
// live wiring, not something a run made.
func TestResetKeepsTheDeliverySwitch(t *testing.T) {
	tg := newTarget(t, 200)
	_, srv := testServer(t, tg)
	if st, _ := call(t, srv, "POST", "/sim/connected", `{"connected": false, "reason": "a platform listens itself"}`); st != 200 {
		t.Fatalf("POST /sim/connected = %d", st)
	}
	call(t, srv, "POST", "/internal/simulate-payment", `{"merchant_transaction_id": "tx-held"}`)

	for _, scope := range []string{"flow", "all"} {
		st, res := call(t, srv, "POST", "/sim/reset", `{"scope": "`+scope+`"}`)
		if st != 200 || res["scope"] != scope {
			t.Fatalf("reset %s = %d %v", scope, st, res)
		}
		if kept, _ := res["kept"].([]any); len(kept) != 1 || kept[0] != "connected_switch" {
			t.Errorf("reset %s kept %v, want the switch", scope, res["kept"])
		}
		if _, sw := call(t, srv, "GET", "/sim/connected", ""); sw["connected"] != false {
			t.Errorf("after a %s reset the switch is %v, want still off", scope, sw["connected"])
		}
	}
	if tg.attempts() != 0 {
		t.Errorf("a held record was sent: %d attempt(s)", tg.attempts())
	}
	if st, info := call(t, srv, "GET", "/sim/info", ""); st != 200 || info["service"] != "aci" {
		t.Errorf("GET /sim/info = %d %v", st, info)
	}
}
