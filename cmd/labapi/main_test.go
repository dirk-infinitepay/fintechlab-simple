package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/console"
)

func testApp(t *testing.T, peers *httptest.Server) *app {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	base := ""
	if peers != nil {
		base = peers.URL
		client = peers.Client()
	}
	cat := &console.Catalogue{Services: []console.Service{
		{ID: "worldline", Name: "Worldline", Kind: console.KindVendor, BaseURL: base, HealthPath: "/health", Ports: []string{"8084/http"}},
		{ID: "settlement", Name: "Settlement", Kind: console.KindPlatform, BaseURL: base, HealthPath: "/health", Ports: []string{"8083/http"}},
		{ID: "bank", Name: "Core ledger", Kind: console.KindPlatform, BaseURL: base, HealthPath: "/health", Ports: []string{"8081/http"}},
	}}
	reg, err := console.NewRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	a := &app{
		cat: cat, reg: reg, client: client, secure: client,
		bcCfg: "../../config/banking-circle.json",
		prov:  &console.Provisioner{Client: client, Registry: reg, B4BURL: base, WorldlineURL: base},
	}
	a.mon = console.NewMonitor(cat, func(console.Service) *http.Client { return client }, time.Hour, time.Second)
	a.bc = &console.BankingCircleBank{BaseURL: base, Client: client}
	a.banks = console.NewBanks(
		&console.CoreLedgerBank{BaseURL: base, Client: client},
		a.bc,
	)
	return a
}

func call(t *testing.T, a *app, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, req)
	return w
}

func TestOverviewReportsEveryServiceEvenBeforeItHasBeenProbed(t *testing.T) {
	a := testApp(t, nil)
	w := call(t, a, "GET", "/v1/lab", "")
	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	var got struct {
		Services []struct {
			ID     string `json:"id"`
			Status struct {
				State string `json:"state"`
			} `json:"status"`
		} `json:"services"`
		Counts map[string]int `json:"counts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Services) != 3 {
		t.Fatalf("services = %d", len(got.Services))
	}
	// "unknown", not "down": the console has not looked yet, and claiming
	// a service is broken on that basis is how a status page loses trust.
	for _, s := range got.Services {
		if s.Status.State != console.StateUnknown {
			t.Errorf("%s state = %q before any probe", s.ID, s.Status.State)
		}
	}
	if got.Counts["unknown"] != 3 {
		t.Fatalf("counts = %v", got.Counts)
	}
}

func TestProbeOfAnUnknownServiceIs404(t *testing.T) {
	a := testApp(t, nil)
	if w := call(t, a, "POST", "/v1/services/nope/probe", ""); w.Code != 404 {
		t.Fatalf("code = %d, want 404", w.Code)
	}
}

func TestOpeningAnAccountWhereTheVendorHasNoEndpointIs501(t *testing.T) {
	a := testApp(t, nil)
	w := call(t, a, "POST", "/v1/banks/banking-circle/accounts", `{"holder":"X","currency":"EUR","opening_balance":""}`)
	// Not 500: a UI that cannot tell "this vendor has no such endpoint"
	// from "this call failed" will offer a retry button forever.
	if w.Code != 501 {
		t.Fatalf("code = %d body=%s, want 501", w.Code, w.Body)
	}
	if w := call(t, a, "POST", "/v1/banks/nope/accounts", `{"holder":"X"}`); w.Code != 404 {
		t.Fatalf("unknown bank code = %d, want 404", w.Code)
	}
}

func TestMerchantValidationAndLookupStatuses(t *testing.T) {
	a := testApp(t, nil)
	if w := call(t, a, "POST", "/v1/merchants", `{"legal_name":""}`); w.Code != 400 {
		t.Fatalf("nameless merchant = %d, want 400", w.Code)
	}
	if w := call(t, a, "POST", "/v1/merchants/mer_nope/outlets", `{"name":""}`); w.Code != 404 {
		t.Fatalf("unknown merchant = %d, want 404", w.Code)
	}
	w := call(t, a, "POST", "/v1/merchants", `{"legal_name":"Handled Ltd","outlets":2}`)
	if w.Code != 201 {
		t.Fatalf("create = %d body=%s", w.Code, w.Body)
	}
	var m console.Merchant
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Outlets) != 2 || m.Outlets[0].MID == "" {
		t.Fatalf("created merchant = %+v", m)
	}
	if w := call(t, a, "DELETE", "/v1/merchants/"+m.ID, ""); w.Code != 200 {
		t.Fatalf("delete = %d", w.Code)
	}
	if w := call(t, a, "DELETE", "/v1/merchants/"+m.ID, ""); w.Code != 404 {
		t.Fatalf("second delete = %d, want 404", w.Code)
	}
}

func TestCallerFixableErrorsAre400NotBadGateway(t *testing.T) {
	a := testApp(t, nil)
	w := call(t, a, "POST", "/v1/merchants", `{"legal_name":"Bounded Ltd"}`)
	var m console.Merchant
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	// 502 "bad gateway" for a number the operator typed would send them
	// reading container logs for a form-validation problem.
	if w := call(t, a, "POST", "/v1/merchants/"+m.ID+"/trading", `{"count":5000}`); w.Code != 400 {
		t.Fatalf("over-cap trading count = %d, want 400", w.Code)
	}
	if w := call(t, a, "POST", "/v1/merchants/"+m.ID+"/status", `{"status":"frozen"}`); w.Code != 400 {
		t.Fatalf("invented status = %d, want 400", w.Code)
	}
}

func TestActionsPassThroughThePeersOwnStatusAndBody(t *testing.T) {
	peers := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sim/settlement-cycle/run":
			// 207: some files published, some did not. The console must
			// not round that up to 200 -- the operator needs to see it.
			w.WriteHeader(207)
			_ = json.NewEncoder(w).Encode(map[string]any{"slot": r.URL.Query().Get("slot"), "errors": []string{"boom"}})
		case "/worldline/pull":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []any{}})
		default:
			http.Error(w, "no", 404)
		}
	}))
	defer peers.Close()
	a := testApp(t, peers)

	w := call(t, a, "POST", "/v1/actions/worldline-cycle?slot=afternoon", "")
	if w.Code != 207 {
		t.Fatalf("code = %d, want the peer's 207", w.Code)
	}
	if !strings.Contains(w.Body.String(), "afternoon") {
		t.Fatalf("slot was not forwarded: %s", w.Body)
	}
	if w := call(t, a, "POST", "/v1/actions/settlement-pull", ""); w.Code != 200 {
		t.Fatalf("pull = %d", w.Code)
	}
}

func TestActionsReportAnUnreachablePeerRatherThanHanging(t *testing.T) {
	a := testApp(t, nil)
	a.cat.Services[0].BaseURL = "http://127.0.0.1:1"
	w := call(t, a, "POST", "/v1/actions/worldline-cycle?slot=morning", "")
	if w.Code != 502 {
		t.Fatalf("code = %d, want 502", w.Code)
	}
}

func TestConfigViewAlwaysRendersSomething(t *testing.T) {
	a := testApp(t, nil)
	w := call(t, a, "GET", "/v1/config", "")
	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// The shipped delivery config is the answer to "how do I not wait two
	// days for a subscription to deactivate", so it has to be readable.
	// Banking Circle is unreachable here, so this exercises the fallback
	// to the file -- which must say that it is a fallback, because
	// BC_TIME_SCALE overrides what the file claims.
	if _, ok := got["banking_circle"]; !ok {
		t.Fatalf("banking_circle missing: %v", got["banking_circle_error"])
	}
	src, _ := got["banking_circle_source"].(string)
	if !strings.Contains(src, "BC_TIME_SCALE") {
		t.Fatalf("banking_circle_source = %q; a file-sourced schedule must warn that it can be overridden", src)
	}
	if len(got["groups"].([]any)) == 0 {
		t.Fatal("no setting groups")
	}
	// Worldline is unreachable in this test; the view must still render
	// and say why that panel is empty.
	if _, ok := got["worldline_channel_error"]; !ok {
		t.Fatal("an unreachable worldline was not reported")
	}
}

func TestMonitorRunStopsWithItsContext(t *testing.T) {
	a := testApp(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.mon.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Monitor.Run ignored its context")
	}
}

// --- activity ---------------------------------------------------------

// TestActivityIsProxiedFromTheServiceItBelongsTo. The console holds the
// only credentials that reach some of these vendors, so the panel reads
// through here rather than from the browser.
func TestActivityIsProxiedFromTheServiceItBelongsTo(t *testing.T) {
	var gotPath string
	peers := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"logs":[{"name":"sftp","title":"File exchange","total":2,"events":[]}]}`))
	}))
	defer peers.Close()

	a := testApp(t, peers)
	a.cat.Services[0].Activity = "/sim/activity"

	w := call(t, a, http.MethodGet, "/v1/services/worldline/activity?limit=25", "")
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/sim/activity?limit=25" {
		t.Errorf("asked the service for %q — the limit must be carried through", gotPath)
	}
	var out struct {
		Logs []struct {
			Name  string `json:"name"`
			Total int    `json:"total"`
		} `json:"logs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Logs) != 1 || out.Logs[0].Name != "sftp" || out.Logs[0].Total != 2 {
		t.Errorf("the service's own answer was not passed through: %s", w.Body.String())
	}
}

// TestActivityOfAServiceThatKeepsNoLogIs501. A UI that cannot tell "this
// vendor does not keep one" from "the call failed" will offer a retry
// button forever.
func TestActivityOfAServiceThatKeepsNoLogIs501(t *testing.T) {
	a := testApp(t, nil)
	if w := call(t, a, http.MethodGet, "/v1/services/bank/activity", ""); w.Code != 501 {
		t.Errorf("status %d, want 501: %s", w.Code, w.Body.String())
	}
	if w := call(t, a, http.MethodGet, "/v1/services/nope/activity", ""); w.Code != 404 {
		t.Errorf("unknown service: status %d, want 404", w.Code)
	}
}

// TestActivityReportsAnUnreachableServiceRatherThanAnEmptyList, because
// "nothing has happened yet" and "this service is not answering" look
// identical once the error is swallowed, and they send an operator to
// opposite ends of the stack.
func TestActivityReportsAnUnreachableServiceRatherThanAnEmptyList(t *testing.T) {
	a := testApp(t, nil)
	a.cat.Services[0].Activity = "/sim/activity"
	a.cat.Services[0].BaseURL = "http://127.0.0.1:1" // nothing listens here

	w := call(t, a, http.MethodGet, "/v1/services/worldline/activity", "")
	if w.Code != 502 {
		t.Fatalf("status %d, want 502: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "127.0.0.1:1") {
		t.Errorf("the failure should name what could not be reached: %s", w.Body.String())
	}
}

// --- system in test ---------------------------------------------------

// flowPeers stands in for the four services the diagram reads, each under
// its own path prefix so one server can be all of them.
func flowPeers(t *testing.T) *httptest.Server {
	t.Helper()
	logs := map[string]string{
		"/worldline/sim/activity": `{"logs":[{"name":"sftp","total":9,"events":[
			{"seq":9,"at":"2026-09-18T21:00:03Z","op":"sftp.download","summary":"infinitepay collected file.csv.pgp","status":"ok"},
			{"seq":8,"at":"2026-09-18T21:00:02Z","op":"sftp.list","summary":"listed /download","status":"ok"},
			{"seq":7,"at":"2026-09-18T21:00:01Z","op":"sftp.session","summary":"infinitepay connected over SFTP","status":"ok"}]}]}`,
		"/b4b/sim/activity": `{"logs":[
			{"name":"payments","total":6,"events":[
				{"seq":6,"at":"2026-09-18T21:01:00Z","op":"payment.create","summary":"payout 951.98 EUR","status":"ok"},
				{"seq":5,"at":"2026-09-18T21:01:00Z","op":"payment.create","summary":"payout 12.00 EUR","status":"warn"}]},
			{"name":"callbacks","total":30,"events":[
				{"seq":30,"at":"2026-09-18T21:02:00Z","op":"callback","summary":"delivered","status":"ok"},
				{"seq":29,"at":"2026-09-18T21:02:00Z","op":"callback","summary":"refused by the client","status":"bad"}]}]}`,
		"/bc/sim/activity": `{"logs":[
			{"name":"payments","total":3,"events":[
				{"seq":3,"at":"2026-09-18T21:03:00Z","op":"payment.create","summary":"payout at the bank","status":"ok"},
				{"seq":2,"at":"2026-09-18T21:02:00Z","op":"payment.incoming","summary":"funds in","status":"ok"}]},
			{"name":"notifications","total":3,"events":[
				{"seq":3,"at":"2026-09-18T21:04:01Z","op":"notification","summary":"batch of 2 to the platform","status":"ok",
				 "detail":{"endpoint":"http://host.containers.internal:3114/api/v1/banking-circle/webhook"}},
				{"seq":2,"at":"2026-09-18T21:04:00Z","op":"notification","summary":"batch of 5","status":"ok",
				 "detail":{"endpoint":"https://receiver:8443/raw-events"}}]},
			{"name":"reports","total":4,"events":[
				{"seq":4,"at":"2026-09-18T22:05:02Z","op":"payment.status","summary":"status of bcp_1 → Processed","status":"ok"},
				{"seq":3,"at":"2026-09-18T22:05:01Z","op":"report.intraday","summary":"intraday report 2026-09-18, page 1 → 6 row(s)","status":"ok"},
				{"seq":2,"at":"2026-09-18T22:05:00Z","op":"report.intraday","summary":"intraday report refused — PageSize","status":"warn"},
				{"seq":1,"at":"2026-09-18T22:04:00Z","op":"report.rejection","summary":"rejection report 2026-09-18 → 0 row(s)","status":"ok"}]}]}`,
		"/bank/sim/activity": `{"logs":[{"name":"credits","total":2,"events":[
			{"seq":2,"at":"2026-09-18T21:03:03Z","op":"credit","summary":"credited 951.98 EUR to GB00SIMM1","status":"ok"},
			{"seq":1,"at":"2026-09-18T21:03:02Z","op":"credit","summary":"credited 12.00 EUR to GB00SIMM2 — account opened","status":"ok","detail":{"opened":"true"}}]},
			{"name":"ledger","total":0,"events":[]}]}`,
		"/runner/sim/activity": `{"logs":[{"name":"runs","total":4,"events":[]}]}`,
		"/runner/status": `{"status":"ok","running_now":null,"last_run":{"id":"run-1","root":"r1",
			"payouts":{"count":6,"total":"27698.78","statuses":{"SUCCESS":2,"IN_PROGRESS":4}},
			"stages":[{"stage":"SETTLEMENT_FILE_INGESTION","status":"COMPLETED"},
			          {"stage":"DAILY_MOVEMENT_PROCESSING","status":"COMPLETED"},
			          {"stage":"DAILY_SETTLEMENT_REPORT","status":"FAILED"}]}}`,
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/authorizations/authorize") {
			_, _ = w.Write([]byte(`{"access_token":"t","expires_in":300}`))
			return
		}
		body, ok := logs[r.URL.Path]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func flowApp(t *testing.T, peers *httptest.Server) *app {
	t.Helper()
	a := testApp(t, peers)
	a.cat.Services = []console.Service{
		{ID: "worldline", Name: "Worldline", Kind: console.KindVendor, BaseURL: peers.URL + "/worldline", Activity: "/sim/activity", HealthPath: "/health"},
		{ID: "b4b", Name: "B4B Payments", Kind: console.KindVendor, BaseURL: peers.URL + "/b4b", Activity: "/sim/activity", HealthPath: "/health"},
		{ID: "banking-circle", Name: "Banking Circle", Kind: console.KindVendor, BaseURL: peers.URL + "/bc", Activity: "/sim/activity", HealthPath: "/health"},
		{ID: "bank", Name: "Business bank", Kind: console.KindVendor, BaseURL: peers.URL + "/bank", Activity: "/sim/activity", HealthPath: "/health"},
		{ID: "receiver", Name: "Webhook receiver", Kind: console.KindPlatform, BaseURL: peers.URL, HealthPath: "/health"},
	}
	a.bc.BaseURL = peers.URL + "/bc"
	register(t, a, testPlugin(peers.URL+"/runner"))
	return a
}

// testPlugin is a settling platform as a plugin declares itself — the
// shape buddy's runner registers, with its stage names.
func testPlugin(base string) console.Plugin {
	return console.Plugin{
		ID: "test-platform", Name: "Test platform", Summary: "a platform under test",
		BaseURL: base, HealthPath: "/status", ActivityPath: "/sim/activity",
		Clock: console.ClockFollows,
		Actions: []console.PluginAction{
			{ID: "sweep", Label: "Run reconciliation sweep", Path: "/sim/sweep", Primary: true},
		},
		Settlement: &console.PluginSettlement{
			FilesPath: "/sim/files", RunPath: "/sim/run", StatusPath: "/status", RunsLog: "runs",
			Stages: map[string][]string{
				"ingest": {"SETTLEMENT_FILE_INGESTION", "DAILY_MOVEMENT_PROCESSING"},
				"reports": {"DAILY_MERCHANT_HELD_REPORT", "MERCHANT_DEFICIT_REPORT",
					"VERIFY_DAILY_SETTLEMENT_STATISTICS", "DAILY_SETTLEMENT_REPORT",
					"DAILY_REJECTED_SETTLEMENT_REPORT"},
			},
		},
	}
}

// register posts p to the lab API the way a plugin does, and waits for the
// probe registration starts in the background: it reads the catalogue, and
// a test that changes the catalogue next would race it.
func register(t *testing.T, a *app, p console.Plugin) {
	t.Helper()
	body, _ := json.Marshal(p)
	if w := call(t, a, http.MethodPost, "/v1/plugins", string(body)); w.Code != 200 {
		t.Fatalf("register %s = %d %s", p.ID, w.Code, w.Body.String())
	}
	if p.HealthPath == "" {
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, probed := a.mon.Get(p.ID); probed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("register %s: its first probe never finished", p.ID)
		}
		time.Sleep(time.Millisecond)
	}
}

func flowOf(t *testing.T, a *app) map[string]flowStep {
	t.Helper()
	w := call(t, a, http.MethodGet, "/v1/flow", "")
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got flowResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	steps := map[string]flowStep{}
	for _, s := range got.Steps {
		steps[s.ID] = s
	}
	return steps
}

// TestFlowCountsWhatEachHopActuallyCarried. The diagram's whole vocabulary
// is these numbers: the browser diffs them to decide what just lit up, so a
// hop counted against the wrong log would animate the wrong arrow.
func TestFlowCountsWhatEachHopActuallyCarried(t *testing.T) {
	peers := flowPeers(t)
	defer peers.Close()
	steps := flowOf(t, flowApp(t, peers))

	for _, tc := range []struct {
		id    string
		count int
		from  string
		to    string
	}{
		{"pull", 2, "platform", "worldline"},    // session + list
		{"collect", 1, "worldline", "platform"}, // the download
		{"payouts", 2, "platform", "b4b"},
		{"bridge", 1, "b4b", "banking-circle"},    // payment.create only
		{"fund", 1, "platform", "banking-circle"}, // payment.incoming only
		{"callbacks", 2, "b4b", "platform"},
		{"notify", 1, "banking-circle", "receiver"},
		{"land", 2, "banking-circle", "bank"},      // the credits at the merchants' bank
		{"recon", 2, "banking-circle", "platform"}, // intraday report reads only, refused one included
	} {
		got := steps[tc.id]
		if got.Count != tc.count {
			t.Errorf("%s carried %d, want %d", tc.id, got.Count, tc.count)
		}
		if got.From != tc.from || got.To != tc.to {
			t.Errorf("%s goes %s->%s, want %s->%s", tc.id, got.From, got.To, tc.from, tc.to)
		}
	}
}

// TestFlowSeparatesTheLabsOwnStubFromThePlatformsSubscriber is the reason
// the confirmations arrow exists. Both hops are Banking Circle delivering
// the same encrypted batches; only one of them is evidence that the system
// under test heard anything. Counting them together is what let a payout
// sit at IN_PROGRESS underneath a green "notifications" hop.
func TestFlowSeparatesTheLabsOwnStubFromThePlatformsSubscriber(t *testing.T) {
	peers := flowPeers(t)
	defer peers.Close()
	steps := flowOf(t, flowApp(t, peers))

	notify, confirm := steps["notify"], steps["confirm"]
	if notify.To != "receiver" || notify.Count != 1 {
		t.Errorf("the lab's own stub took one batch: %+v", notify)
	}
	if confirm.From != "banking-circle" || confirm.To != "platform" {
		t.Errorf("confirmations go %s->%s, want banking-circle->platform", confirm.From, confirm.To)
	}
	if confirm.Count != 1 {
		t.Errorf("the platform's subscriber took one batch: %+v", confirm)
	}
	if !confirm.Multi {
		t.Error("a batch hop carries many events, so it must be able to report partial success")
	}
}

// TestAnUnattributableBatchIsNotClaimedAsThePlatforms. The confirmations
// arrow asserts "your own listener was called". A batch whose endpoint
// cannot be read is not evidence of that, so it stays where it has always
// been counted rather than turning the arrow green on a guess.
func TestAnUnattributableBatchIsNotClaimedAsThePlatforms(t *testing.T) {
	isLab := labDelivery(map[string]bool{"receiver": true})
	lab, platform := splitEvents([]activity.Event{
		{Op: "notification", Detail: map[string]string{"endpoint": "https://receiver:8443/raw-events"}},
		{Op: "notification", Detail: map[string]string{"endpoint": "http://host.containers.internal:3114/hook"}},
		{Op: "notification"}, // no detail at all
		{Op: "notification", Detail: map[string]string{"events": "5"}}, // detail, but no endpoint
		{Op: "notification", Detail: map[string]string{"endpoint": "://not a url"}},
	}, isLab)

	if len(platform) != 1 {
		t.Errorf("only the batch with a foreign host is the platform's: %+v", platform)
	}
	if len(lab) != 4 {
		t.Errorf("everything unattributable stays on the lab's side, got %d", len(lab))
	}
}

// TestTheReportSaysHowManyPayoutsTheBankConfirmed — "0 of 6" is the line
// somebody is looking for, so it is reported even when it is zero.
func TestTheReportSaysHowManyPayoutsTheBankConfirmed(t *testing.T) {
	peers := flowPeers(t)
	defer peers.Close()
	w := call(t, flowApp(t, peers), http.MethodGet, "/v1/flow", "")
	var got flowResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	stats := map[string]string{}
	for _, st := range got.Report {
		stats[st.Label] = st.Value
	}
	if stats["payouts confirmed"] != "2 of 6" {
		t.Errorf("payouts confirmed = %q, want \"2 of 6\"", stats["payouts confirmed"])
	}
	if stats["confirmations to the platform"] != "1" {
		t.Errorf("confirmations to the platform = %q, want 1", stats["confirmations to the platform"])
	}
}

// TestFlowSeparatesAFailureFromARefusal: red and amber mean different
// things on this diagram — one is the vendor breaking, the other is the
// vendor refusing, and an operator reacts to them differently.
func TestFlowSeparatesAFailureFromARefusal(t *testing.T) {
	peers := flowPeers(t)
	defer peers.Close()
	steps := flowOf(t, flowApp(t, peers))

	if steps["callbacks"].Failed != 1 {
		t.Errorf("a refused callback should count as failed, got %+v", steps["callbacks"])
	}
	if steps["payouts"].Refused != 1 {
		t.Errorf("a 4xx payout should count as refused, not failed: %+v", steps["payouts"])
	}
	if steps["payouts"].Failed != 0 {
		t.Errorf("a refusal must not be reported as a failure: %+v", steps["payouts"])
	}
}

// TestFlowReadsStagesForTheHopsThatNeverLeaveThePlatform.
func TestFlowReadsStagesForTheHopsThatNeverLeaveThePlatform(t *testing.T) {
	peers := flowPeers(t)
	defer peers.Close()
	steps := flowOf(t, flowApp(t, peers))

	if steps["ingest"].Count != 2 {
		t.Errorf("ingest should count its two stages, got %d", steps["ingest"].Count)
	}
	if steps["reports"].Count != 1 || steps["reports"].Failed != 1 {
		t.Errorf("the failed report stage should show as failed: %+v", steps["reports"])
	}
}

// TestAHopListsExactlyWhatItsArrowCounted. The drawer beside the diagram
// shows the traffic behind one arrow. If it re-derived that list it could
// disagree with the number on the arrow; it reads the same step instead.
func TestAHopListsExactlyWhatItsArrowCounted(t *testing.T) {
	peers := flowPeers(t)
	defer peers.Close()
	a := flowApp(t, peers)
	steps := flowOf(t, a)

	hop := func(id string) flowHopResponse {
		t.Helper()
		w := call(t, a, http.MethodGet, "/v1/flow/hops/"+id, "")
		if w.Code != 200 {
			t.Fatalf("hop %s = %d %s", id, w.Code, w.Body.String())
		}
		var got flowHopResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	for _, id := range []string{"pull", "payouts", "confirm", "notify", "recon"} {
		got := hop(id)
		if len(got.Events) != steps[id].Count {
			t.Errorf("%s: drawer lists %d, arrow says %d", id, len(got.Events), steps[id].Count)
		}
	}
	// The platform's subscriber, not the lab's stub: the one batch with a
	// foreign endpoint.
	if c := hop("confirm"); len(c.Events) != 1 || !strings.Contains(c.Events[0].Summary, "to the platform") {
		t.Errorf("confirm lists %+v", c.Events)
	}
	p := hop("payouts")
	if p.Step.Source != "b4b" || p.Step.SourceLog != "payments" {
		t.Errorf("payouts should point at b4b's payments log, got %q/%q", p.Step.Source, p.Step.SourceLog)
	}

	// A hop the platform makes to itself lists its stages, in the order the
	// plugin declared them, including the ones the run has not reached.
	r := hop("reports")
	if len(r.Stages) != 5 || r.Step.Source != "test-platform" {
		t.Fatalf("reports stages = %+v, source %q", r.Stages, r.Step.Source)
	}
	status := map[string]string{}
	for _, st := range r.Stages {
		status[st.Stage] = st.Status
	}
	if status["DAILY_SETTLEMENT_REPORT"] != "FAILED" || status["MERCHANT_DEFICIT_REPORT"] != "" {
		t.Errorf("stage statuses = %+v", status)
	}

	if w := call(t, a, http.MethodGet, "/v1/flow/hops/nope", ""); w.Code != 404 {
		t.Errorf("unknown hop = %d, want 404", w.Code)
	}
}

// TestFlowSurvivesAVendorBeingDown. The view is the first thing an operator
// opens; it must render with one participant unreachable and say which,
// rather than 500 and leave them with nothing.
func TestFlowSurvivesAVendorBeingDown(t *testing.T) {
	peers := flowPeers(t)
	defer peers.Close()
	a := flowApp(t, peers)
	for i := range a.cat.Services {
		if a.cat.Services[i].ID == "b4b" {
			a.cat.Services[i].BaseURL = "http://127.0.0.1:1"
		}
	}

	w := call(t, a, http.MethodGet, "/v1/flow", "")
	if w.Code != 200 {
		t.Fatalf("status %d, want the page to still render: %s", w.Code, w.Body.String())
	}
	var got flowResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Errors["b4b"] == "" {
		t.Error("the unreachable vendor should be named in errors")
	}
	if len(got.Participants) != 6 || len(got.Steps) != 12 {
		t.Errorf("the diagram should still be whole: %d participants, %d steps",
			len(got.Participants), len(got.Steps))
	}
}

// --- banking circle subscriptions -------------------------------------

// bcFake is Banking Circle's notification surface, enough of it to drive
// the card: the token exchange, the subscription list, the queue depth, the
// clienttest, and the notification ring the result is read back from.
type bcFake struct {
	sent          int
	deliveredWith string // the status the ring reports for the delivery
	paused        bool   // what the vendor says about this subscription's delivery
	calls         []string
}

func (f *bcFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/authorizations/authorize"):
			_, _ = w.Write([]byte(`{"access_token":"t","expires_in":300}`))
		case strings.HasSuffix(r.URL.Path, "/notificationselfservice/subscription"):
			_, _ = w.Write([]byte(`{"result":[{
				"id":"sub_1","endpoint":"https://receiver:8443/raw-events","isActive":true,
				"status":2,"statusMessage":"Subscription successfully created","version":1,
				"mtlsEnabled":false,"maxNotificationsPerMessage":5,
				"subscriptionEvents":[
					{"eventType":"OutgoingPaymentBooked","isActive":true,"subscriptionEventTargetDetails":[]},
					{"eventType":"MissingFunding","isActive":false,"subscriptionEventTargetDetails":[{"id":"t1"}]}]}]}`))
		case strings.Contains(r.URL.Path, "/sim/subscription/") && strings.HasSuffix(r.URL.Path, "/pending"):
			if f.paused {
				_, _ = w.Write([]byte(`{"pending":3,"paused":true,"queued":7}`))
				return
			}
			_, _ = w.Write([]byte(`{"pending":3,"paused":false,"queued":0}`))
		case strings.HasSuffix(r.URL.Path, "/pause"), strings.HasSuffix(r.URL.Path, "/resume"):
			f.calls = append(f.calls, r.Method+" "+r.URL.Path)
			f.paused = strings.HasSuffix(r.URL.Path, "/pause")
			if f.paused {
				_, _ = w.Write([]byte(`{"subscriptionId":"sub_1","endpoint":"https://receiver:8443/raw-events","paused":true,"queued":0}`))
				return
			}
			_, _ = w.Write([]byte(`{"subscriptionId":"sub_1","endpoint":"https://receiver:8443/raw-events","paused":false,"released":7,"queued":0}`))
		case strings.Contains(r.URL.Path, "/clienttest/"):
			f.sent++
			_, _ = w.Write([]byte(`{"status":"sent"}`))
		case strings.HasPrefix(r.URL.Path, "/sim/activity"):
			if f.sent == 0 {
				_, _ = w.Write([]byte(`{"logs":[{"name":"notifications","total":0,"events":[]}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"logs":[{"name":"notifications","total":1,"events":[
				{"seq":1,"at":"2026-09-19T10:00:00Z","op":"notification","status":"` + f.deliveredWith + `",
				 "summary":"batch of 1 to https://receiver:8443/raw-events",
				 "detail":{"http":"200"}}]}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
}

// TestSubscriptionsAreNamedNotNumbered. The vendor sends status 2; an
// operator needs the word. A card that printed the number would be a lookup
// table, and the point of the card is not having to hold one.
func TestSubscriptionsAreNamedNotNumbered(t *testing.T) {
	fake := &bcFake{}
	srv := fake.server(t)
	defer srv.Close()
	a := testApp(t, srv)
	a.bc.BaseURL = srv.URL

	w := call(t, a, http.MethodGet, "/v1/banking-circle/subscriptions", "")
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Subscriptions []subscriptionView `json:"subscriptions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Subscriptions) != 1 {
		t.Fatalf("got %d subscriptions", len(got.Subscriptions))
	}
	s := got.Subscriptions[0]
	if s.Status != "active" {
		t.Errorf("status = %q, want the word", s.Status)
	}
	if s.Endpoint != "https://receiver:8443/raw-events" {
		t.Errorf("endpoint = %q", s.Endpoint)
	}
	if len(s.Events) != 2 || s.Events[0].Type != "OutgoingPaymentBooked" {
		t.Errorf("events = %+v — the card is about which events go where", s.Events)
	}
	if s.Events[1].Active {
		t.Error("an inactive event must not be shown as active")
	}
	if s.Events[1].Targets != 1 {
		t.Errorf("targets = %d, want the count that makes an event narrower", s.Events[1].Targets)
	}
	if s.Pending != 3 {
		t.Errorf("pending = %d — a queue that is not draining is the first thing to check", s.Pending)
	}
}

// TestProbeReportsWhatTheEndpointDidNotThatItWasSent. The vendor's
// clienttest answers 200 "sent" whether or not anything took it, and "sent"
// is not the question being asked.
func TestProbeReportsWhatTheEndpointDidNotThatItWasSent(t *testing.T) {
	for _, tc := range []struct {
		ring      string
		delivered bool
	}{
		{"ok", true},
		{"bad", false},
	} {
		fake := &bcFake{deliveredWith: tc.ring}
		srv := fake.server(t)
		a := testApp(t, srv)
		a.bc.BaseURL = srv.URL

		w := call(t, a, http.MethodPost, "/v1/banking-circle/subscriptions/sub_1/test", "")
		if w.Code != 200 {
			srv.Close()
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		var got struct {
			Delivered bool   `json:"delivered"`
			Result    string `json:"result"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			srv.Close()
			t.Fatal(err)
		}
		if got.Delivered != tc.delivered {
			t.Errorf("ring said %q, console reported delivered=%v", tc.ring, got.Delivered)
		}
		if !strings.Contains(got.Result, "batch of 1") {
			t.Errorf("the vendor's own line should be the answer, got %q", got.Result)
		}
		if fake.sent != 1 {
			t.Errorf("clienttest fired %d times, want 1", fake.sent)
		}
		srv.Close()
	}
}

// TestSubscriptionListIsAnArrayWhenEmpty, because the card decides between
// "nothing is subscribed" and rows by counting, and null renders neither.
func TestSubscriptionListIsAnArrayWhenEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/authorizations/authorize") {
			_, _ = w.Write([]byte(`{"access_token":"t","expires_in":300}`))
			return
		}
		_, _ = w.Write([]byte(`{"result":[]}`))
	}))
	defer srv.Close()
	a := testApp(t, srv)
	a.bc.BaseURL = srv.URL

	w := call(t, a, http.MethodGet, "/v1/banking-circle/subscriptions", "")
	if !strings.Contains(w.Body.String(), `"subscriptions":[]`) {
		t.Errorf("empty list marshalled as %s", w.Body.String())
	}
}

// TestPauseAndResumeAreTheVendorsOwnSwitch. The console holds no state of
// its own here: it calls the vendor and re-reads the vendor. A pause the
// console remembered locally would be invisible to anything not going
// through the console — including Banking Circle's own notification log,
// which is where the queued notifications have to show up.
func TestPauseAndResumeAreTheVendorsOwnSwitch(t *testing.T) {
	fake := &bcFake{}
	srv := fake.server(t)
	defer srv.Close()
	a := testApp(t, srv)
	a.bc.BaseURL = srv.URL

	subs := func() subscriptionView {
		t.Helper()
		w := call(t, a, http.MethodGet, "/v1/banking-circle/subscriptions", "")
		var got struct {
			Subscriptions []subscriptionView `json:"subscriptions"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Subscriptions) != 1 {
			t.Fatalf("got %d subscriptions", len(got.Subscriptions))
		}
		return got.Subscriptions[0]
	}

	if v := subs(); v.Paused || v.Queued != 0 {
		t.Fatalf("a running subscription reported paused=%v queued=%d", v.Paused, v.Queued)
	}

	if w := call(t, a, http.MethodPost, "/v1/banking-circle/subscriptions/sub_1/pause", ""); w.Code != 200 {
		t.Fatalf("pause = %d: %s", w.Code, w.Body.String())
	}
	v := subs()
	if !v.Paused || v.Queued != 7 {
		t.Errorf("after pause: paused=%v queued=%d, want the vendor's own answer", v.Paused, v.Queued)
	}
	// Retained and queued are different stalls and stay different numbers:
	// one clears by reactivating the subscription, the other by pressing
	// release.
	if v.Pending != 3 {
		t.Errorf("pending = %d, want 3 — a pause must not swallow what the vendor retained", v.Pending)
	}

	w := call(t, a, http.MethodPost, "/v1/banking-circle/subscriptions/sub_1/resume", "")
	if w.Code != 200 {
		t.Fatalf("resume = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"released":7`) {
		t.Errorf("the count released is what the toast reports, got %s", w.Body.String())
	}
	if v := subs(); v.Paused {
		t.Error("still paused after resume")
	}

	want := []string{"POST /sim/subscription/sub_1/pause", "POST /sim/subscription/sub_1/resume"}
	if strings.Join(fake.calls, ",") != strings.Join(want, ",") {
		t.Errorf("vendor saw %v, want %v — each button is exactly one call", fake.calls, want)
	}
}

// TestPauseSurfacesTheVendorsRefusal. The button is rendered from a list
// the console polled; the subscription may be gone by the time anyone
// clicks it. The vendor's own status has to come through rather than
// become a console-shaped success.
func TestPauseSurfacesTheVendorsRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/authorizations/authorize") {
			_, _ = w.Write([]byte(`{"access_token":"t","expires_in":300}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"bankingcircle: subscription not found"}`))
	}))
	defer srv.Close()
	a := testApp(t, srv)
	a.bc.BaseURL = srv.URL

	w := call(t, a, http.MethodPost, "/v1/banking-circle/subscriptions/sub_gone/pause", "")
	if w.Code != 502 {
		t.Fatalf("status %d, want 502", w.Code)
	}
	if !strings.Contains(w.Body.String(), "subscription not found") {
		t.Errorf("the vendor's own words are the answer, got %s", w.Body.String())
	}
}

// TestPayoutsOfferOnlyWhatTheVendorWouldDo: the card lists outgoing
// payments newest first, offers Return and Reverse only on a processed
// payout that has not come back, and each button is exactly one call to the
// vendor's /sim hook — whose refusal comes back in its own words.
func TestPayoutsOfferOnlyWhatTheVendorWouldDo(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/authorizations/authorize"):
			_, _ = w.Write([]byte(`{"access_token":"t","expires_in":300}`))
		case r.Method == http.MethodGet && r.URL.Path == "/payments":
			_, _ = w.Write([]byte(`{"payments":[
				{"id":"bcp_old","state":"OutgoingPaymentProcessed","amount":"1.00","currency":"EUR","createdAt":"2026-09-23T09:00:00Z","paymentReferenceNumber":"010F100000000001","settlementId":"sttl_v1:a"},
				{"id":"bcp_new","state":"OutgoingPaymentBooked","amount":"2.00","currency":"EUR","createdAt":"2026-09-23T10:00:00Z"},
				{"id":"bcp_back","state":"OutgoingPaymentProcessed","amount":"3.00","currency":"EUR","createdAt":"2026-09-23T09:30:00Z","returnedBy":"bcp_ret"},
				{"id":"bcp_ret","state":"IncomingPaymentProcessed","amount":"3.00","currency":"EUR","createdAt":"2026-09-23T11:00:00Z","return":true}]}`))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/sim/payments/"):
			calls = append(calls, r.URL.Path)
			if strings.Contains(r.URL.Path, "bcp_back") {
				w.WriteHeader(409)
				_, _ = w.Write([]byte(`{"error":"bankingcircle: payment has already been returned"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"bcp_ret2","return":true}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	a := testApp(t, srv)
	a.bc.BaseURL = srv.URL

	w := call(t, a, http.MethodGet, "/v1/banking-circle/payouts", "")
	var got struct {
		Payouts []payoutView `json:"payouts"`
		Total   int          `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, w.Body.String())
	}
	ids := []string{}
	for _, p := range got.Payouts {
		ids = append(ids, p.ID)
	}
	if strings.Join(ids, ",") != "bcp_new,bcp_back,bcp_old" || got.Total != 3 {
		t.Fatalf("payouts = %v (total %d), want the three outgoing ones newest first", ids, got.Total)
	}
	offered := map[string]bool{}
	for _, p := range got.Payouts {
		offered[p.ID] = p.CanReturn && p.CanReverse
	}
	if !offered["bcp_old"] || offered["bcp_new"] || offered["bcp_back"] {
		t.Errorf("offered = %v, want only the processed payout that has not come back", offered)
	}

	if w := call(t, a, http.MethodPost, "/v1/banking-circle/payouts/bcp_old/return", ""); w.Code != 200 {
		t.Fatalf("return = %d: %s", w.Code, w.Body.String())
	}
	if w := call(t, a, http.MethodPost, "/v1/banking-circle/payouts/bcp_old/reverse", ""); w.Code != 200 {
		t.Fatalf("reverse = %d: %s", w.Code, w.Body.String())
	}
	w = call(t, a, http.MethodPost, "/v1/banking-circle/payouts/bcp_back/return", "")
	if w.Code != 502 || !strings.Contains(w.Body.String(), "already been returned") {
		t.Errorf("refused return = %d %s, want 502 with the vendor's own words", w.Code, w.Body.String())
	}
	want := []string{"/sim/payments/bcp_old/return", "/sim/payments/bcp_old/reverse", "/sim/payments/bcp_back/return"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Errorf("vendor saw %v, want %v — each button is exactly one call", calls, want)
	}
}

// TestFlowReconciliationArrowCountsIntradayReads: the sweep's arrow lights
// on intraday report reads only — not a status read, not the rejection
// report — and a refused read shows as refused, not as traffic that worked.
func TestFlowReconciliationArrowCountsIntradayReads(t *testing.T) {
	peers := flowPeers(t)
	defer peers.Close()
	recon := flowOf(t, flowApp(t, peers))["recon"]
	if recon.Count != 2 || recon.Refused != 1 {
		t.Errorf("recon = count %d refused %d, want 2 and 1", recon.Count, recon.Refused)
	}
	if recon.LastSummary != "intraday report 2026-09-18, page 1 → 6 row(s)" {
		t.Errorf("last summary %q, want the newest intraday read", recon.LastSummary)
	}
}

// TestPluginRunForwardsTheFilesAsAsked: picking files is the platform's
// API, not the console's — {"files": […]} reaches the plugin's declared
// run_path as it was sent, several files are still one call, and the
// platform's refusal comes back with its own status and words rather than
// as a console error.
func TestPluginRunForwardsTheFilesAsAsked(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/sim/files":
			_, _ = w.Write([]byte(`{"dir":"apps/settle-ingest/local","files":[
				{"name":"worldline-reconciliation-sample.csv","currency":"EUR","mids":["1"],"seeded":true},
				{"name":"worldline-reconciliation-sample-gbp.csv","currency":"GBP","mids":["6"],"seeded":false,"missing_mids":["6"]}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/sim/run":
			b, _ := io.ReadAll(r.Body)
			bodies = append(bodies, string(b))
			if len(bodies) > 1 {
				w.WriteHeader(409)
				_, _ = w.Write([]byte(`{"error":"a GBP run is already in flight (worldline/run-gbp-1.csv)"}`))
				return
			}
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"runs":[{"run":"worldline/run-eur-1.csv"},{"run":"worldline/run-gbp-1.csv"}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	a := testApp(t, srv)
	register(t, a, testPlugin(srv.URL))

	w := call(t, a, http.MethodGet, "/v1/plugins/test-platform/files", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "worldline-reconciliation-sample-gbp.csv") {
		t.Fatalf("files = %d %s", w.Code, w.Body.String())
	}

	both := `{"files":["worldline-reconciliation-sample.csv","worldline-reconciliation-sample-gbp.csv"]}`
	if w := call(t, a, http.MethodPost, "/v1/plugins/test-platform/run", both); w.Code != 202 {
		t.Fatalf("run both = %d %s, want the platform's 202", w.Code, w.Body.String())
	}
	w = call(t, a, http.MethodPost, "/v1/plugins/test-platform/run", `{"files":["worldline-reconciliation-sample-gbp.csv"]}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "already in flight") {
		t.Errorf("second GBP run = %d %s, want the platform's 409 and its words", w.Code, w.Body.String())
	}
	if len(bodies) != 2 || bodies[0] != both {
		t.Errorf("platform saw %q, want the body as sent, one call per click", bodies)
	}
}

// A declared action is one call to the path the plugin declared, and its
// refusal comes back as the platform said it — a second click while a sweep
// is running is the platform's 409, not a console-made error. An action it
// did not declare goes nowhere.
func TestPluginActionIsOneCallToTheDeclaredPath(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/sim/sweep" {
			w.WriteHeader(404)
			return
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls > 1 {
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"error":"a reconciliation sweep is already running"}`))
			return
		}
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"sweep":1,"note":"running"}`))
	}))
	defer srv.Close()
	a := testApp(t, srv)
	register(t, a, testPlugin(srv.URL))

	if w := call(t, a, http.MethodPost, "/v1/plugins/test-platform/actions/sweep", "{}"); w.Code != 202 {
		t.Fatalf("first sweep = %d %s, want the platform's 202", w.Code, w.Body)
	}
	w := call(t, a, http.MethodPost, "/v1/plugins/test-platform/actions/sweep", "{}")
	if w.Code != 409 || !strings.Contains(w.Body.String(), "already running") {
		t.Errorf("second sweep = %d %s, want the platform's 409 and its words", w.Code, w.Body)
	}
	if w := call(t, a, http.MethodPost, "/v1/plugins/test-platform/actions/drop-tables", "{}"); w.Code != 404 {
		t.Errorf("undeclared action = %d, want 404", w.Code)
	}
	if calls != 2 {
		t.Errorf("platform saw %d calls, want one per click", calls)
	}
}

// TestAPluginCardAppearsGoesGreyAndComesBack: a registered platform is in
// the overview with its buttons; once it unregisters, its card stays with
// the words on how to start it, grey rather than red, and its buttons are
// refused with a reason rather than forwarded into a closed port.
func TestAPluginCardAppearsGoesGreyAndComesBack(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer up.Close()
	a := testApp(t, nil)
	p := testPlugin(up.URL)
	p.StartHint = "start it with make platform"
	register(t, a, p)

	overview := func() map[string]any {
		t.Helper()
		var got struct {
			Services []map[string]any `json:"services"`
		}
		_ = json.Unmarshal(call(t, a, http.MethodGet, "/v1/lab", "").Body.Bytes(), &got)
		for _, s := range got.Services {
			if s["id"] == "test-platform" {
				return s
			}
		}
		t.Fatal("the registered platform is not in the overview")
		return nil
	}
	s := overview()
	if s["kind"] != "platform" || s["plugin"].(map[string]any)["state"] != "live" {
		t.Fatalf("registered card: %v", s)
	}

	// Its last probe said up — the moment before it said goodbye.
	a.mon.Probe(context.Background(), "test-platform")
	if w := call(t, a, http.MethodDelete, "/v1/plugins/test-platform", ""); w.Code != 204 {
		t.Fatalf("unregister = %d", w.Code)
	}
	s = overview()
	plugin := s["plugin"].(map[string]any)
	status := s["status"].(map[string]any)
	if plugin["state"] != "stopped" || plugin["start_hint"] != "start it with make platform" || status["state"] != "unknown" {
		t.Fatalf("stopped card: plugin %v status %v", plugin, status)
	}
	if w := call(t, a, http.MethodPost, "/v1/plugins/test-platform/actions/sweep", "{}"); w.Code != 409 {
		t.Errorf("action on a stopped plugin = %d, want 409", w.Code)
	}

	register(t, a, p)
	if overview()["plugin"].(map[string]any)["state"] != "live" {
		t.Error("re-registering did not bring the card back")
	}
}

// TestRegistrationRefusesWhatTheConsoleWouldForwardBlindly: the console
// sends a declared path to the plugin's own base URL and nowhere else, so a
// path that names another host or climbs out is refused at the door, as is
// an id that would shadow one of the lab's own services.
func TestRegistrationRefusesWhatTheConsoleWouldForwardBlindly(t *testing.T) {
	a := testApp(t, nil)
	for name, mutate := range map[string]func(*console.Plugin){
		"absolute URL as a path": func(p *console.Plugin) { p.Actions[0].Path = "http://evil.test/x" },
		"scheme-relative path":   func(p *console.Plugin) { p.Actions[0].Path = "//evil.test/x" },
		"climbing path":          func(p *console.Plugin) { p.Settlement.RunPath = "/../../admin" },
		"lab service id":         func(p *console.Plugin) { p.ID = "worldline" },
		"no clock policy":        func(p *console.Plugin) { p.Clock = "" },
		"GET action":             func(p *console.Plugin) { p.Actions[0].Method = "GET" },
		"not a URL":              func(p *console.Plugin) { p.BaseURL = "file:///etc" },
	} {
		p := testPlugin("http://127.0.0.1:1")
		mutate(&p)
		body, _ := json.Marshal(p)
		if w := call(t, a, http.MethodPost, "/v1/plugins", string(body)); w.Code != 400 {
			t.Errorf("%s: register = %d, want 400", name, w.Code)
		}
	}
}

// TestTheClockDoesNotMoveUnderAWallClockPlatform: a platform that registered
// on the wall clock cannot follow a move, so the console refuses one — and
// passes the clock service's own refusal (a hold) through as it came.
func TestTheClockDoesNotMoveUnderAWallClockPlatform(t *testing.T) {
	moves := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		moves++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":"clock is held: runner holds the clock: settlement in flight"}`))
	}))
	defer srv.Close()
	a := testApp(t, srv)
	a.cat.Services = append(a.cat.Services, console.Service{ID: "clock", Name: "Lab clock", Kind: console.KindPlatform, BaseURL: srv.URL})

	w := call(t, a, http.MethodPost, "/v1/clock", `{"advance":"1h"}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "settlement in flight") || moves != 1 {
		t.Fatalf("held clock = %d %s after %d calls, want the service's 409", w.Code, w.Body, moves)
	}

	p := testPlugin("http://127.0.0.1:1")
	p.Clock = console.ClockWall
	register(t, a, p)
	w = call(t, a, http.MethodPost, "/v1/clock", `{"advance":"1h"}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "wall clock") || moves != 1 {
		t.Fatalf("under a wall-clock platform = %d %s after %d calls, want the console's own 409 and no call", w.Code, w.Body, moves)
	}
}
