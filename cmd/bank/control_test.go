package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
)

// testSurfaces builds the bank's two surfaces as main does, with nothing in
// the environment moving or narrowing the control port.
func testSurfaces(t *testing.T, s *store, credits, ledger *activity.Log) (*http.ServeMux, *simctl.Control) {
	t.Helper()
	t.Setenv("SIM_CONTROL_LISTEN", "")
	t.Setenv("SIM_CONTROL_ALLOW", "")
	mux, ctl, err := surfaces(s, credits, ledger, ":8081")
	if err != nil {
		t.Fatal(err)
	}
	return mux, ctl
}

// controlRoutes is every route the bank's control port serves.
var controlRoutes = []string{
	"GET /sim/activity",
	"GET /sim/accounts",
	"POST /sim/accounts",
	"GET /sim/info",
	"GET /sim/logs",
	"POST /sim/reset",
}

// pattern is the route mux would hand req to, without calling it.
func pattern(mux *http.ServeMux, method, path string) string {
	_, p := mux.Handler(httptest.NewRequest(method, path, nil))
	return p
}

// Every /sim route is the control port's, on :9081; the bank's own port
// passes the same paths through to it, and keeps nothing of the lab's.
func TestTheControlPortServesEverySimRoute(t *testing.T) {
	s := newStore()
	credits, ledger := newLogs()
	mux, ctl := testSurfaces(t, s, credits, ledger)
	if ctl.Addr() != ":9081" {
		t.Errorf("control port = %q, want :9081", ctl.Addr())
	}
	for _, route := range controlRoutes {
		method, path, _ := strings.Cut(route, " ")
		if got := pattern(ctl.Mux, method, path); got != route {
			t.Errorf("control port: %s is handled by %q", route, got)
		}
		if got := pattern(mux, method, path); got != "/sim/" {
			t.Errorf("bank port: %s is handled by %q, want the /sim/ alias", route, got)
		}
	}
	// And the control port is not a second copy of the bank's own API.
	for _, route := range []string{"GET /accounts", "POST /accounts", "POST /transfers", "POST /internal/credit"} {
		method, path, _ := strings.Cut(route, " ")
		if got := pattern(ctl.Mux, method, path); got != "" {
			t.Errorf("control port serves %s (as %q)", route, got)
		}
	}
	w := serve(t, ctl.Handler(), "GET", "/health", "")
	if w.Code != 200 || !jsonHas(t, w.Body.Bytes(), "surface", "control") {
		t.Errorf("control GET /health: %d %s", w.Code, w.Body)
	}
}

// The old paths answer on the bank's own port exactly as on the control
// port: same status, same body.
func TestTheOldSimPathsStillAnswerOnTheBankPort(t *testing.T) {
	s := newStore()
	credits, ledger := newLogs()
	mux, ctl := testSurfaces(t, s, credits, ledger)
	if w := serve(t, mux, "POST", "/internal/credit", `{"iban":"GB00SIMMERCH0000001","amount":"1.00","currency":"EUR","reference":"SETL-1"}`); w.Code != 200 {
		t.Fatalf("credit: %d %s", w.Code, w.Body)
	}
	for _, path := range []string{"/sim/activity", "/sim/activity?limit=1"} {
		old, now := serve(t, mux, "GET", path, ""), serve(t, ctl.Handler(), "GET", path, "")
		if old.Code != 200 || old.Code != now.Code || old.Body.String() != now.Body.String() {
			t.Errorf("GET %s: bank port %d %s, control port %d %s", path, old.Code, old.Body, now.Code, now.Body)
		}
	}
	if w := serve(t, mux, "GET", "/sim/info", ""); w.Code != 200 || !jsonHas(t, w.Body.Bytes(), "control_listener", ":9081") {
		t.Errorf("GET /sim/info on the bank port: %d %s", w.Code, w.Body)
	}
	if w := serve(t, mux, "POST", "/sim/reset", ""); w.Code != 200 {
		t.Errorf("POST /sim/reset on the bank port: %d %s", w.Code, w.Body)
	}
	if len(s.entries) != 0 {
		t.Error("the reset on the bank port did not reach the ledger")
	}
}

// GET and POST /sim/accounts answer as GET and POST /accounts do for the
// same state, so the lab API reads them with the code it has.
func TestTheAccountRoutesOnTheControlPortAnswerAsTheBanksOwn(t *testing.T) {
	s := newStore()
	credits, ledger := newLogs()
	mux, ctl := testSurfaces(t, s, credits, ledger)
	if w := serve(t, mux, "POST", "/internal/credit", `{"iban":"GB00SIMMERCH0000001","holder":"Southwind","amount":"125.00","currency":"EUR","reference":"SETL-1"}`); w.Code != 200 {
		t.Fatalf("credit: %d %s", w.Code, w.Body)
	}

	list := func(h http.Handler, path string) (int, []map[string]any) {
		w := serve(t, h, "GET", path, "")
		var out struct {
			Accounts []map[string]any `json:"accounts"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("GET %s: %v: %s", path, err, w.Body)
		}
		// The bank lists in no particular order.
		sort.Slice(out.Accounts, func(i, j int) bool { return out.Accounts[i]["id"].(string) < out.Accounts[j]["id"].(string) })
		return w.Code, out.Accounts
	}
	oldCode, old := list(mux, "/accounts")
	newCode, now := list(ctl.Handler(), "/sim/accounts")
	if oldCode != 200 || newCode != oldCode || !reflect.DeepEqual(old, now) || len(old) != 6 {
		t.Errorf("GET /accounts %d %v\nGET /sim/accounts %d %v", oldCode, old, newCode, now)
	}

	// Refusals are byte for byte the same.
	for _, body := range []string{`{}`, `{"holder":"Acme","colour":"red"}`, `{"holder":"Acme","openingBalance":"lots"}`} {
		o, n := serve(t, mux, "POST", "/accounts", body), serve(t, ctl.Handler(), "POST", "/sim/accounts", body)
		if o.Code != 400 || n.Code != o.Code || n.Body.String() != o.Body.String() {
			t.Errorf("%s: POST /accounts %d %s, POST /sim/accounts %d %s", body, o.Code, o.Body, n.Code, n.Body)
		}
	}

	// An account opened either way is the same account but for its id and
	// the next IBAN, is listed by both, and is logged as account.open.
	body := `{"holder":"Acme Ltd","currency":"EUR","openingBalance":"10.00"}`
	o, n := serve(t, mux, "POST", "/accounts", body), serve(t, ctl.Handler(), "POST", "/sim/accounts", body)
	if o.Code != 201 || n.Code != o.Code {
		t.Fatalf("POST /accounts %d %s, POST /sim/accounts %d %s", o.Code, o.Body, n.Code, n.Body)
	}
	var opened, viaControl map[string]any
	_ = json.Unmarshal(o.Body.Bytes(), &opened)
	_ = json.Unmarshal(n.Body.Bytes(), &viaControl)
	if opened["iban"] == viaControl["iban"] {
		t.Errorf("both accounts have IBAN %v", opened["iban"])
	}
	for _, m := range []map[string]any{opened, viaControl} {
		delete(m, "id")
		delete(m, "iban")
	}
	if !reflect.DeepEqual(opened, viaControl) {
		t.Errorf("POST /accounts opened %v, POST /sim/accounts %v", opened, viaControl)
	}
	_, old = list(mux, "/accounts")
	_, now = list(ctl.Handler(), "/sim/accounts")
	if len(old) != 8 || !reflect.DeepEqual(old, now) {
		t.Errorf("after opening two: GET /accounts %d, GET /sim/accounts %d", len(old), len(now))
	}
	// Each call either way is one account.open in the ledger log, refusals
	// included; newest first, so the pairs are (control, bank).
	evs := ledger.Recent(20)
	if len(evs) != 8 {
		t.Fatalf("ledger log holds %d events, want 8: %+v", len(evs), evs)
	}
	for i := 0; i < len(evs); i += 2 {
		n, o := evs[i], evs[i+1]
		if n.Op != "account.open" || o.Op != n.Op || o.Status != n.Status || (i > 0 && o.Summary != n.Summary) {
			t.Errorf("logged %+v for POST /accounts and %+v for POST /sim/accounts", o, n)
		}
	}
}

func jsonHas(t *testing.T, body []byte, key, want string) bool {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return m[key] == want
}
