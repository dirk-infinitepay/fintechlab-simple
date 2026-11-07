package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/webduvet/fintechlab-simple/internal/simctl"
)

// testSurfaces builds worldline's two HTTP surfaces as main does, with
// nothing in the environment moving or narrowing the control port.
func testSurfaces(t *testing.T, w *sim) (*http.ServeMux, *simctl.Control) {
	t.Helper()
	t.Setenv("SIM_CONTROL_LISTEN", "")
	t.Setenv("SIM_CONTROL_ALLOW", "")
	mux, ctl, err := w.surfaces(":8084", ":2222")
	if err != nil {
		t.Fatal(err)
	}
	return mux, ctl
}

// controlRoutes is every route worldline's control port serves.
var controlRoutes = []string{
	"POST /sim/transactions",
	"GET /sim/transactions",
	"POST /sim/settlement-cycle/run",
	"GET /sim/settlement-cycle/runs",
	"GET /sim/enrolment",
	"POST /sim/enrolment/process",
	"POST /sim/enrolment/reject",
	"GET /sim/activity",
	"GET /sim/config",
	"PUT /sim/config",
	"GET /sim/info",
	"GET /sim/logs",
	"POST /sim/reset",
}

func serve(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// pattern is the route mux would hand a request to, without calling it.
func pattern(mux *http.ServeMux, route string) string {
	method, path, _ := strings.Cut(route, " ")
	_, p := mux.Handler(httptest.NewRequest(method, path, nil))
	return p
}

// sameAnswer fails unless both calls answered the same status and body.
func sameAnswer(t *testing.T, what string, old, now *httptest.ResponseRecorder) {
	t.Helper()
	if old.Code != now.Code || old.Body.String() != now.Body.String() {
		t.Errorf("%s: %d %s\nagainst %d %s", what, old.Code, old.Body, now.Code, now.Body)
	}
}

// Every /sim route is the control port's, on :9084; worldline's own port
// passes the same paths through to it, and keeps nothing of the lab's.
func TestTheControlPortServesEverySimRoute(t *testing.T) {
	w := newTestSim(t, "")
	mux, ctl := testSurfaces(t, w)
	if ctl.Addr() != ":9084" {
		t.Errorf("control port = %q, want :9084", ctl.Addr())
	}
	for _, route := range controlRoutes {
		if got := pattern(ctl.Mux, route); got != route {
			t.Errorf("control port: %s is handled by %q", route, got)
		}
		if got := pattern(mux, route); got != "/sim/" {
			t.Errorf("worldline port: %s is handled by %q, want the /sim/ alias", route, got)
		}
	}
	// The control port is not a second copy of the vendor's API.
	for _, route := range []string{"GET /config", "PUT /config", "GET /inbound/onboarding/m", "GET /outbound/m/daily/f.csv", "GET /archive/2026/09"} {
		if got := pattern(ctl.Mux, route); got != "" {
			t.Errorf("control port serves %s (as %q)", route, got)
		}
	}

	var info struct {
		Vendor  []string `json:"vendor_listeners"`
		Control string   `json:"control_listener"`
	}
	r := serve(t, ctl.Handler(), "GET", "/sim/info", "")
	if err := json.Unmarshal(r.Body.Bytes(), &info); err != nil || r.Code != 200 {
		t.Fatalf("GET /sim/info: %d %s", r.Code, r.Body)
	}
	if strings.Join(info.Vendor, ",") != ":8084,:2222" || info.Control != ":9084" {
		t.Errorf("GET /sim/info names vendor %v, control %q", info.Vendor, info.Control)
	}
	if r := serve(t, ctl.Handler(), "GET", "/health", ""); r.Code != 200 || !strings.Contains(r.Body.String(), `"surface":"control"`) {
		t.Errorf("control GET /health: %d %s", r.Code, r.Body)
	}
}

// The hooks work on the control port, and the old paths on worldline's own
// port answer exactly as the control port does: same status, same body.
func TestTheOldSimPathsStillAnswerOnTheWorldlinePort(t *testing.T) {
	bc, wires := bankingCircleFake(t, nil)
	w := newTestSim(t, bc.URL)
	mux, ctl := testSurfaces(t, w)
	h := ctl.Handler()

	// A run, driven over the control port as the lab API will drive it.
	if r := serve(t, h, "POST", "/sim/transactions", `{"mid":"`+testMerchant+`","currency":"EUR","amount_cents":15000,"date":"2026-09-24"}`); r.Code != 201 {
		t.Fatalf("POST /sim/transactions: %d %s", r.Code, r.Body)
	}
	if r := serve(t, h, "POST", "/sim/settlement-cycle/run?slot=morning&date=2026-09-24", ""); r.Code != 200 || wires.Load() != 1 {
		t.Fatalf("POST /sim/settlement-cycle/run: %d %s (%d wires)", r.Code, r.Body, wires.Load())
	}
	if r := serve(t, h, "POST", "/sim/enrolment/reject", `{"pf_account_id":"pf-1002","reason_code":"01"}`); r.Code != 200 {
		t.Fatalf("POST /sim/enrolment/reject: %d %s", r.Code, r.Body)
	}
	// And one through the old path, which lands in the same state.
	if r := serve(t, mux, "POST", "/sim/transactions", `[{"mid":"`+testMerchant+`","currency":"GBP","amount_cents":900,"date":"2026-09-24"}]`); r.Code != 201 {
		t.Fatalf("POST /sim/transactions on the worldline port: %d %s", r.Code, r.Body)
	}
	if n := len(w.acq.store.All()); n != 2 {
		t.Fatalf("%d transaction(s) held, want 2", n)
	}

	for _, path := range []string{
		"/sim/transactions",
		"/sim/transactions?mid=" + testMerchant + "&from=2026-09-24",
		"/sim/settlement-cycle/runs",
		"/sim/enrolment",
		"/sim/activity",
		"/sim/config",
	} {
		sameAnswer(t, "GET "+path+" on the worldline port and the control port", serve(t, mux, "GET", path, ""), serve(t, h, "GET", path, ""))
	}
	// Refusals too.
	sameAnswer(t, "POST /sim/settlement-cycle/run?slot=noon",
		serve(t, mux, "POST", "/sim/settlement-cycle/run?slot=noon", ""), serve(t, h, "POST", "/sim/settlement-cycle/run?slot=noon", ""))
	sameAnswer(t, "POST /sim/enrolment/reject {}",
		serve(t, mux, "POST", "/sim/enrolment/reject", `{}`), serve(t, h, "POST", "/sim/enrolment/reject", `{}`))

	// A reset through the old path is the control port's reset.
	if r := serve(t, mux, "POST", "/sim/reset", `{"scope":"flow"}`); r.Code != 200 || !strings.Contains(r.Body.String(), `"transactions":2`) {
		t.Fatalf("POST /sim/reset on the worldline port: %d %s", r.Code, r.Body)
	}
	if len(w.acq.store.All()) != 0 || len(w.acq.history()) != 0 {
		t.Error("the reset on the worldline port did not reach the acquirer")
	}
}

// GET and PUT /sim/config answer as GET and PUT /config do for the same
// state, so the lab API reads and writes them with the code it has.
func TestTheConfigRoutesOnTheControlPortAnswerAsTheVendorsOwn(t *testing.T) {
	w := newTestSim(t, "")
	mux, ctl := testSurfaces(t, w)
	h := ctl.Handler()
	configFile := filepath.Join(w.ch.BaseDir, "config", "merchant-onboarding.json")

	get := func(what string) {
		t.Helper()
		old, now := serve(t, mux, "GET", "/config", ""), serve(t, h, "GET", "/sim/config", "")
		if old.Code != 200 {
			t.Fatalf("%s: GET /config %d %s", what, old.Code, old.Body)
		}
		sameAnswer(t, what+": GET /config and GET /sim/config", old, now)
	}
	get("seeded")

	// The same change either way answers the same and stores the same.
	set := `{"merchant_id":"` + testMerchant + `","sftp_directory":"/sftp/outbound/` + testMerchant + `","report_types":["WX"],` +
		`"report_formats":["CSV"],"cut_off_time":"18:00 CET","remittance_frequency":"daily","sftp_directory_count":2}`
	old := serve(t, mux, "PUT", "/config", set)
	stored, _ := os.ReadFile(configFile)
	now := serve(t, h, "PUT", "/sim/config", set)
	if old.Code != 200 || !strings.Contains(old.Body.String(), `"cut_off_time":"18:00 CET"`) {
		t.Fatalf("PUT /config: %d %s", old.Code, old.Body)
	}
	sameAnswer(t, "PUT /config and PUT /sim/config", old, now)
	if again, _ := os.ReadFile(configFile); string(again) != string(stored) {
		t.Errorf("PUT /sim/config stored\n%s\nPUT /config\n%s", again, stored)
	}
	get("after a PUT")

	// A change through the control port is what the vendor's own GET reads.
	if r := serve(t, h, "PUT", "/sim/config", strings.Replace(set, "18:00 CET", "17:00 CET", 1)); r.Code != 200 {
		t.Fatalf("PUT /sim/config: %d %s", r.Code, r.Body)
	}
	if r := serve(t, mux, "GET", "/config", ""); !strings.Contains(r.Body.String(), `"cut_off_time":"17:00 CET"`) {
		t.Errorf("GET /config after PUT /sim/config: %s", r.Body)
	}

	// Refusals are byte for byte the same.
	for _, body := range []string{`{}`, `{"merchant_id":"` + testMerchant + `","colour":"red"}`, `not json`} {
		old, now := serve(t, mux, "PUT", "/config", body), serve(t, h, "PUT", "/sim/config", body)
		if old.Code != 400 {
			t.Errorf("PUT /config %s: %d %s", body, old.Code, old.Body)
		}
		sameAnswer(t, "PUT "+body, old, now)
	}

	// And so is a missing config.
	if err := os.Remove(configFile); err != nil {
		t.Fatal(err)
	}
	old, now = serve(t, mux, "GET", "/config", ""), serve(t, h, "GET", "/sim/config", "")
	if old.Code != 404 {
		t.Errorf("GET /config with no config: %d %s", old.Code, old.Body)
	}
	sameAnswer(t, "GET with no config", old, now)
}
