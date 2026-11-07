package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/logring"
)

// vendorMux is LISTEN's mux wired as main wires it, with a control surface
// of its own behind the /sim alias. Tests never start the control listener,
// so its address is only what /sim/info reports.
func vendorMux(t *testing.T, a *app) *http.ServeMux {
	t.Helper()
	ctl, err := a.control(":9085")
	if err != nil {
		t.Fatal(err)
	}
	return a.mux(ctl.Mux)
}

// controlServer serves a's control surface as the control port does.
func controlServer(t *testing.T, a *app) *httptest.Server {
	t.Helper()
	ctl, err := a.control(":9085")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(ctl.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// sinkSubscriber answers 200 to every delivery and keeps nothing.
func sinkSubscriber(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// raw makes one request and returns the status and the body as sent.
func raw(t *testing.T, method, url, tok string, hdr http.Header, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// newControlTestApp is a test app whose dispatcher logs to the process log:
// a clienttest or a flush timer can deliver after the test has returned, and
// t.Logf then would panic.
func newControlTestApp(t *testing.T) *app {
	t.Helper()
	a := newTestAppLogging(t, log.Printf)
	a.payLog = activity.New("payments", "Payments received", "")
	return a
}

// contractRoutes are the routes simctl mounts on every control surface.
var contractRoutes = []string{"GET /sim/info", "GET /sim/logs", "POST /sim/reset"}

type labCall struct {
	pattern, path, body string
	want                int
}

// labCalls is one call for every route on the control surface, the sim
// control contract's included, in pattern order. It fails the test for a
// route it has no call for, so a route added later is covered or noticed.
func labCalls(t *testing.T, a *app, subID string) []labCall {
	t.Helper()
	sub := "/sim/subscription/" + subID
	byPattern := map[string]labCall{
		"GET /sim/info":                                         {path: "/sim/info", want: 200},
		"GET /sim/logs":                                         {path: "/sim/logs", want: 200},
		"POST /sim/reset":                                       {path: "/sim/reset", body: `{"scope":"flow"}`, want: 200},
		"GET /sim/activity":                                     {path: "/sim/activity", want: 200},
		"GET /sim/emails":                                       {path: "/sim/emails", want: 200},
		"GET /sim/delivery-config":                              {path: "/sim/delivery-config", want: 200},
		"GET /sim/subscriptions":                                {path: "/sim/subscriptions", want: 200},
		"POST /sim/subscription/{subscriptionId}/test":          {path: sub + "/test", want: 200},
		"POST /sim/subscription/{subscriptionId}/activate":      {path: sub + "/activate", want: 200},
		"POST /sim/subscription/{subscriptionId}/deactivate":    {path: sub + "/deactivate", want: 200},
		"POST /sim/subscription/{subscriptionId}/notifications": {path: sub + "/notifications?count=1", want: 202},
		"GET /sim/subscription/{subscriptionId}/pending":        {path: sub + "/pending", want: 200},
		"POST /sim/subscription/{subscriptionId}/pause":         {path: sub + "/pause", want: 200},
		"POST /sim/subscription/{subscriptionId}/resume":        {path: sub + "/resume", want: 200},
		"GET /sim/accounts":                                     {path: "/sim/accounts", want: 200},
		"GET /sim/accounts/{accountId}/balances":                {path: "/sim/accounts/" + sgaEUR + "/balances", want: 200},
		"GET /sim/payments":                                     {path: "/sim/payments", want: 200},
		"POST /sim/incoming-payments":                           {path: "/sim/incoming-payments", body: `{"currency":"EUR","amount":"1.00","reference":"ctl"}`, want: 202},
		"GET /sim/payments/outcomes":                            {path: "/sim/payments/outcomes", want: 200},
		"POST /sim/payments/outcomes":                           {path: "/sim/payments/outcomes", body: `{"outcomes":[]}`, want: 200},
		"POST /sim/payments/{id}/return":                        {path: "/sim/payments/bcp_nope/return", want: 404},
		"POST /sim/payments/{id}/reverse":                       {path: "/sim/payments/bcp_nope/reverse", want: 404},
	}
	patterns := append([]string{}, contractRoutes...)
	for p := range a.simRoutes() {
		patterns = append(patterns, p)
	}
	sort.Strings(patterns)
	out := make([]labCall, 0, len(patterns))
	for _, p := range patterns {
		c, ok := byPattern[p]
		if !ok {
			t.Fatalf("no call for %s: add one, so the route is checked on both listeners", p)
		}
		c.pattern = p
		out = append(out, c)
	}
	if len(out) != len(byPattern) {
		t.Fatalf("%d calls for %d routes: a call names a route the control surface does not register", len(byPattern), len(out))
	}
	return out
}

func (c labCall) method() string { m, _, _ := strings.Cut(c.pattern, " "); return m }

func TestTheControlSurfaceServesEveryLabRouteWithoutAToken(t *testing.T) {
	if logring.Default == nil {
		logring.Install() // so /sim/logs has a ring to read
	}
	a := newControlTestApp(t)
	sub := subscribeAll(t, a, sinkSubscriber(t))
	ctl := controlServer(t, a)

	for _, c := range labCalls(t, a, sub.ID) {
		if code, body := raw(t, c.method(), ctl.URL+c.path, "", nil, c.body); code != c.want {
			t.Errorf("%s %s on the control port = %d %s, want %d", c.method(), c.path, code, body, c.want)
		}
	}
}

func TestTheOldSimPathsAnswerOnLISTENBehindTheBearer(t *testing.T) {
	if logring.Default == nil {
		logring.Install()
	}
	a := newControlTestApp(t)
	sub := subscribeAll(t, a, sinkSubscriber(t))
	srv := httptest.NewServer(vendorMux(t, a))
	defer srv.Close()

	tok := a.tokens.issue(time.Hour)
	for _, c := range labCalls(t, a, sub.ID) {
		for _, bad := range []string{"", "bcat_not_issued"} {
			req, _ := http.NewRequest(c.method(), srv.URL+c.path, strings.NewReader(c.body))
			if bad != "" {
				req.Header.Set("Authorization", "Bearer "+bad)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 401 || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer") {
				t.Errorf("%s %s on :8085 with token %q = %d (WWW-Authenticate %q), want 401 Bearer",
					c.method(), c.path, bad, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
			}
		}
		if code, body := raw(t, c.method(), srv.URL+c.path, tok, nil, c.body); code != c.want {
			t.Errorf("%s %s on :8085 with the bearer = %d %s, want %d", c.method(), c.path, code, body, c.want)
		}
	}
	// The control port's own /health is not aliased: :8085 keeps its own.
	if health := mustCall(t, 200, "GET", srv.URL+"/health", "", ""); health["surface"] != nil {
		t.Fatalf(":8085 /health = %v, want the vendor's own", health)
	}
}

// decode reads a JSON body for comparison, sorting the named list by id
// where the source hands it out unordered.
func decode(t *testing.T, b []byte, unordered string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	if m, ok := v.(map[string]any); ok && unordered != "" {
		list, _ := m[unordered].([]any)
		sort.Slice(list, func(i, j int) bool {
			return list[i].(map[string]any)["id"].(string) < list[j].(map[string]any)["id"].(string)
		})
	}
	return v
}

func keysOf(t *testing.T, b []byte) []string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestEachControlReadAnswersAsItsCounterpart: every route the control
// surface adds for the lab API answers what the vendor or bridge route it
// stands in for answers, for the same state, so the lab API parses it with
// the code it already has.
func TestEachControlReadAnswersAsItsCounterpart(t *testing.T) {
	a := newControlTestApp(t)
	sub := subscribeAll(t, a, sinkSubscriber(t))
	subscribeAll(t, a, sinkSubscriber(t)+"/second")
	srv := httptest.NewServer(vendorMux(t, a))
	defer srv.Close()
	bridge := httptest.NewServer(a.internalMux())
	defer bridge.Close()
	ctl := controlServer(t, a)
	tok := a.tokens.issue(time.Hour)

	// Some state: money in, a payout opening a merchant account, settled.
	mustCall(t, 202, "POST", bridge.URL+"/internal/incoming-payments", "", `{"currency":"EUR","amount":"100.00","reference":"lump"}`)
	mustCall(t, 202, "POST", bridge.URL+"/internal/payments", "",
		`{"paymentId":"bcp_ctl1","accountId":"`+merchantAccount+`","amount":"10.00","currency":"EUR"}`)
	a.engine.Wait()

	same := func(what string, wantCode, gotCode int, want, got []byte, unordered string) {
		t.Helper()
		if gotCode != wantCode {
			t.Errorf("%s: %d %s, its counterpart %d %s", what, gotCode, got, wantCode, want)
			return
		}
		if !reflect.DeepEqual(decode(t, want, unordered), decode(t, got, unordered)) {
			t.Errorf("%s answered\n  %s\nits counterpart\n  %s", what, got, want)
		}
	}

	t.Run("accounts and payments", func(t *testing.T) {
		wc, w := raw(t, "GET", srv.URL+"/accounts", tok, nil, "")
		gc, g := raw(t, "GET", ctl.URL+"/sim/accounts", "", nil, "")
		same("GET /sim/accounts", wc, gc, w, g, "accounts")
		wc, w = raw(t, "GET", srv.URL+"/payments", tok, nil, "")
		gc, g = raw(t, "GET", ctl.URL+"/sim/payments", "", nil, "")
		same("GET /sim/payments", wc, gc, w, g, "payments")
	})

	t.Run("balances", func(t *testing.T) {
		for _, id := range []string{sgaEUR, merchantAccount, someoneAccount, "not-a-uuid"} {
			wc, w := raw(t, "GET", bridge.URL+"/internal/accounts/"+id+"/balances", "", nil, "")
			gc, g := raw(t, "GET", ctl.URL+"/sim/accounts/"+id+"/balances", "", nil, "")
			// lastTransactionTimestamp is the time of the read, to the
			// second; the two reads may straddle one.
			stamp := func(b []byte) []byte {
				var m map[string]any
				if json.Unmarshal(b, &m) != nil {
					return b
				}
				for _, e := range func() []any { l, _ := m["balances"].([]any); return l }() {
					e.(map[string]any)["lastTransactionTimestamp"] = "…"
				}
				out, _ := json.Marshal(m)
				return out
			}
			same("GET /sim/accounts/"+id+"/balances", wc, gc, stamp(w), stamp(g), "")
		}
	})

	t.Run("subscriptions", func(t *testing.T) {
		wc, w := raw(t, "GET", srv.URL+"/api/v1/notificationselfservice/subscription?PageSize=1000", tok, nil, "")
		gc, g := raw(t, "GET", ctl.URL+"/sim/subscriptions", "", nil, "")
		if wc != gc || !bytes.Equal(w, g) {
			t.Errorf("GET /sim/subscriptions = %d %s, the vendor's list with PageSize=1000 %d %s", gc, g, wc, w)
		}
		if n := len(decode(t, g, "").(map[string]any)["result"].([]any)); n != 2 {
			t.Errorf("GET /sim/subscriptions listed %d, want both", n)
		}
		wc, w = raw(t, "GET", srv.URL+"/api/v1/notificationselfservice/subscription?PageNumber=2&PageSize=1", tok, nil, "")
		gc, g = raw(t, "GET", ctl.URL+"/sim/subscriptions?PageNumber=2&PageSize=1", "", nil, "")
		if wc != gc || !bytes.Equal(w, g) {
			t.Errorf("a page of /sim/subscriptions = %d %s, the vendor's %d %s", gc, g, wc, w)
		}
	})

	t.Run("clienttest", func(t *testing.T) {
		for _, id := range []string{sub.ID, "sub_nope"} {
			wc, w := raw(t, "POST", srv.URL+"/api/v1/notificationselfservice/clienttest/"+id, tok, nil, "")
			gc, g := raw(t, "POST", ctl.URL+"/sim/subscription/"+id+"/test", "", nil, "")
			if wc != gc || !bytes.Equal(w, g) {
				t.Errorf("POST /sim/subscription/%s/test = %d %s, clienttest %d %s", id, gc, g, wc, w)
			}
		}
	})

	t.Run("activate and deactivate", func(t *testing.T) {
		vendorPath := srv.URL + "/api/v1/notificationselfservice/subscription/" + sub.ID
		read := func() []byte {
			_, b := raw(t, "GET", vendorPath, tok, nil, "")
			return b
		}
		rowVersion := func() string {
			var s struct {
				RowVersion string `json:"rowVersion"`
			}
			_ = json.Unmarshal(read(), &s)
			return s.RowVersion
		}
		ifMatch := func() http.Header { return http.Header{"If-Match": {rowVersion()}} }

		// The vendor's switch wants If-Match; the lab's does not.
		if code, _ := raw(t, "PUT", vendorPath+"/deactivate", tok, nil, ""); code != 412 {
			t.Fatalf("vendor deactivate without If-Match = %d, want 412", code)
		}
		stale := rowVersion()
		code, g := raw(t, "POST", ctl.URL+"/sim/subscription/"+sub.ID+"/deactivate", "", nil, "")
		same("POST /sim/subscription/{id}/deactivate", 200, code, read(), g, "")
		if code, _ := raw(t, "PUT", vendorPath+"/activate", tok, http.Header{"If-Match": {stale}}, ""); code != 412 {
			t.Fatalf("the rowVersion from before the lab's deactivate = %d, want 412", code)
		}
		code, g = raw(t, "POST", ctl.URL+"/sim/subscription/"+sub.ID+"/activate", "", nil, "")
		var act map[string]json.RawMessage
		_ = json.Unmarshal(g, &act)
		same("POST /sim/subscription/{id}/activate's subscription", 200, code, read(), act["subscription"], "")

		// The vendor's answers, for the shape.
		wc, w := raw(t, "PUT", vendorPath+"/deactivate", tok, ifMatch(), "")
		same("PUT …/deactivate", 200, wc, read(), w, "")
		_, gDeact := raw(t, "POST", ctl.URL+"/sim/subscription/"+sub.ID+"/deactivate", "", nil, "")
		if !slices.Equal(keysOf(t, w), keysOf(t, gDeact)) {
			t.Errorf("deactivate: the lab's keys %v, the vendor's %v", keysOf(t, gDeact), keysOf(t, w))
		}
		wc, w = raw(t, "PUT", vendorPath+"/activate", tok, ifMatch(), "")
		if wc != 200 || !slices.Equal(keysOf(t, w), keysOf(t, g)) {
			t.Errorf("activate: the vendor's %d %s, the lab's %s", wc, w, g)
		}

		for _, verb := range []string{"activate", "deactivate"} {
			wc, w := raw(t, "PUT", srv.URL+"/api/v1/notificationselfservice/subscription/sub_nope/"+verb, tok, http.Header{"If-Match": {"x"}}, "")
			gc, g := raw(t, "POST", ctl.URL+"/sim/subscription/sub_nope/"+verb, "", nil, "")
			if wc != gc || !bytes.Equal(w, g) {
				t.Errorf("%s of an unknown subscription = %d %s, the vendor's %d %s", verb, gc, g, wc, w)
			}
		}
	})

	t.Run("incoming payments", func(t *testing.T) {
		body := `{"currency":"GBP","amount":"5.00","reference":"lump"}`
		wc, w := raw(t, "POST", bridge.URL+"/internal/incoming-payments", "", nil, body)
		gc, g := raw(t, "POST", ctl.URL+"/sim/incoming-payments", "", nil, body)
		if wc != 202 || gc != 202 || !slices.Equal(keysOf(t, w), keysOf(t, g)) {
			t.Fatalf("POST /sim/incoming-payments = %d %s, the bridge's %d %s", gc, g, wc, w)
		}
		var bp, sp map[string]any
		_ = json.Unmarshal(w, &bp)
		_ = json.Unmarshal(g, &sp)
		for _, k := range []string{"fromAccountId", "toAccountId", "amount", "currency", "reference", "state", "remittanceInformation"} {
			if !reflect.DeepEqual(bp[k], sp[k]) {
				t.Errorf("%s: the lab's %v, the bridge's %v", k, sp[k], bp[k])
			}
		}
		for _, bad := range []string{`{"currency":"USD","amount":"1.00"}`, `{"currency":"EUR","amount":"-1"}`, `{"currency":"EUR","amount":"1.00","account":"x"}`} {
			wc, w := raw(t, "POST", bridge.URL+"/internal/incoming-payments", "", nil, bad)
			gc, g := raw(t, "POST", ctl.URL+"/sim/incoming-payments", "", nil, bad)
			if wc != 400 || wc != gc || !bytes.Equal(w, g) {
				t.Errorf("%s: the lab's %d %s, the bridge's %d %s", bad, gc, g, wc, w)
			}
		}
		// Both are money landing, in the payments log alike.
		landed := 0
		for _, e := range a.payLog.Recent(0) {
			if e.Op == "payment.incoming" && e.Status == activity.StatusOK && e.Detail["status"] == "Accepted" {
				landed++
			}
		}
		if landed != 3 { // the lump sum above, then one each here
			t.Errorf("%d payment.incoming events landed, want 3", landed)
		}
	})

	// Last: a forced outcome would end the next payout above.
	t.Run("forced outcomes", func(t *testing.T) {
		check := func() {
			t.Helper()
			wc, w := raw(t, "GET", bridge.URL+"/internal/payments/outcomes", "", nil, "")
			gc, g := raw(t, "GET", ctl.URL+"/sim/payments/outcomes", "", nil, "")
			if wc != gc || !bytes.Equal(w, g) {
				t.Errorf("GET /sim/payments/outcomes = %d %s, the bridge's %d %s", gc, g, wc, w)
			}
		}
		check()
		if out := mustCall(t, 200, "POST", ctl.URL+"/sim/payments/outcomes", "", `{"outcomes":["Rejected"]}`); !reflect.DeepEqual(out["queued"], []any{"Rejected"}) {
			t.Fatalf("POST /sim/payments/outcomes = %v", out)
		}
		if out := mustCall(t, 200, "POST", bridge.URL+"/internal/payments/outcomes", "", `{"outcomes":["Pending"]}`); !reflect.DeepEqual(out["queued"], []any{"Rejected", "Pending"}) {
			t.Fatalf("the bridge's POST after the lab's = %v, want both queued", out)
		}
		check()
		bad := `{"outcomes":["Maybe"]}`
		wc, w := raw(t, "POST", bridge.URL+"/internal/payments/outcomes", "", nil, bad)
		gc, g := raw(t, "POST", ctl.URL+"/sim/payments/outcomes", "", nil, bad)
		if wc != 400 || wc != gc || !bytes.Equal(w, g) {
			t.Errorf("an unknown outcome: the lab's %d %s, the bridge's %d %s", gc, g, wc, w)
		}
	})
}

// TestAResetKeepsTheTokens: the platform's Banking Circle client is never
// logged out by a lab reset, of either scope.
func TestAResetKeepsTheTokens(t *testing.T) {
	a := newControlTestApp(t)
	srv := httptest.NewServer(vendorMux(t, a))
	defer srv.Close()
	ctl := controlServer(t, a)
	tok := a.tokens.issue(time.Hour)

	for _, scope := range []string{"flow", "all"} {
		out := mustCall(t, 200, "POST", ctl.URL+"/sim/reset", "", `{"scope":"`+scope+`"}`)
		if kept, _ := out["kept"].([]any); !slices.Contains(kept, any("tokens")) {
			t.Fatalf("%s reset kept %v, want tokens among them", scope, out["kept"])
		}
		if cleared, _ := out["cleared"].(map[string]any); cleared["tokens"] != nil {
			t.Fatalf("%s reset cleared tokens: %v", scope, out)
		}
		mustCall(t, 200, "GET", srv.URL+"/api/v1/accounts/"+sgaEUR+"/balances", tok, "")
	}
}
