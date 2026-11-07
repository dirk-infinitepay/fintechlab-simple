package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/bankingcircle"
	"github.com/webduvet/fintechlab-simple/internal/logring"
)

// call makes one request with an optional bearer and JSON body and decodes
// the answer.
func call(t *testing.T, method, url, tok, body string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
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
	out := map[string]any{}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func mustCall(t *testing.T, want int, method, url, tok, body string) map[string]any {
	t.Helper()
	code, out := call(t, method, url, tok, body)
	if code != want {
		t.Fatalf("%s %s = %d %v, want %d", method, url, code, out, want)
	}
	return out
}

// within fails the test unless f returns inside d.
func within(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s", what, d)
	}
}

func TestSimControlIsOnTheControlPortWithoutAToken(t *testing.T) {
	a := newTestApp(t)
	ctl := controlServer(t, a)

	info := mustCall(t, 200, "GET", ctl.URL+"/sim/info", "", "")
	if info["service"] != "banking-circle" || info["kind"] != "vendor" || info["simulates"] != "Banking Circle Connect" {
		t.Fatalf("info = %v", info)
	}
	if scopes, _ := info["reset_scopes"].([]any); len(scopes) != 2 {
		t.Fatalf("reset_scopes = %v, want flow and all", info["reset_scopes"])
	}
	if code, out := call(t, "POST", ctl.URL+"/sim/reset", "", `{"scope":"some"}`); code != 400 {
		t.Fatalf("an unknown scope = %d %v, want 400", code, out)
	}
	if health := mustCall(t, 200, "GET", ctl.URL+"/health", "", ""); health["surface"] != "control" {
		t.Fatalf("control /health = %v", health)
	}
}

// TestResetOverHTTPFlowThenAll walks a run, a flow reset that keeps the
// platform's setup, and an all reset that puts back the start-up seed.
func TestResetOverHTTPFlowThenAll(t *testing.T) {
	logring.Install() // so /sim/logs has a ring to read
	a := newTestApp(t)
	a.payLog = activity.New("payments", "Payments received", "")
	srv := httptest.NewServer(vendorMux(t, a))
	defer srv.Close()
	ctl := controlServer(t, a)
	internalSrv := httptest.NewServer(a.internalMux())
	defer internalSrv.Close()

	// Start-up's seed, then the platform's own subscription.
	seedSrv, _ := captureSubscriber(t)
	seed := subscribeAll(t, a, seedSrv.URL)
	a.bank = bankingcircle.NewBank(a.engine, a.dispatch)
	platformSrv, deliveries := captureSubscriber(t)
	subscribeAll(t, a, platformSrv.URL)
	stream := &notifStream{t: t, ch: deliveries, key: a.notifKey}

	// A run: money in, a payout to a merchant, all the way to processed.
	mustCall(t, 202, "POST", internalSrv.URL+"/internal/incoming-payments", "", `{"currency":"EUR","amount":"100.00","reference":"lump"}`)
	mustCall(t, 202, "POST", internalSrv.URL+"/internal/payments", "",
		`{"paymentId":"bcp_run1","accountId":"`+merchantAccount+`","amount":"10.00","currency":"EUR"}`)
	stream.expect(bankingcircle.NotificationOutgoingPaymentProcessed)
	if _, err := a.engine.ForceNext([]bankingcircle.Outcome{bankingcircle.OutcomeRejected}); err != nil {
		t.Fatal(err)
	}

	tok := a.tokens.issue(time.Hour)
	out := mustCall(t, 200, "POST", ctl.URL+"/sim/reset", "", "")
	cleared, _ := out["cleared"].(map[string]any)
	if out["service"] != "banking-circle" || out["scope"] != "flow" ||
		cleared["payments"] != float64(2) || cleared["outcomes"] != float64(1) || cleared["balances"] != float64(2) {
		t.Fatalf("flow reset = %v, want 2 payments, 1 outcome and 2 balances cleared", out)
	}
	if n, _ := cleared["activity"].(float64); n < 2 {
		t.Fatalf("flow reset cleared %v activity events, want at least the two bridge calls", cleared["activity"])
	}
	if kept, _ := out["kept"].([]any); len(kept) != 3 {
		t.Fatalf("kept = %v, want subscriptions, accounts and tokens", out["kept"])
	}

	// Cleared: payments, balances, activity. Kept: the merchant's account,
	// both subscriptions, the platform's token.
	if got, _ := mustCall(t, 200, "GET", srv.URL+"/payments", tok, "")["payments"].([]any); len(got) != 0 {
		t.Fatalf("payments after a flow reset = %v", got)
	}
	accounts, _ := mustCall(t, 200, "GET", srv.URL+"/accounts", tok, "")["accounts"].([]any)
	if len(accounts) != 3 {
		t.Fatalf("%d accounts after a flow reset, want the two safeguarding accounts and the merchant's", len(accounts))
	}
	for _, raw := range accounts {
		if acc := raw.(map[string]any); acc["balance"] != "0.00" {
			t.Fatalf("account %v after a flow reset, want 0.00", acc)
		}
	}
	if got := mustCall(t, 200, "GET", srv.URL+"/api/v1/notificationselfservice/subscription", tok, "")["totalItems"]; got != float64(2) {
		t.Fatalf("%v subscriptions after a flow reset, want 2", got)
	}
	logs, _ := mustCall(t, 200, "GET", ctl.URL+"/sim/activity", "", "")["logs"].([]any)
	for _, raw := range logs {
		if l := raw.(map[string]any); l["total"] != float64(0) {
			t.Fatalf("log %v has %v events after the reset, want none", l["name"], l["total"])
		}
	}
	lines, _ := mustCall(t, 200, "GET", ctl.URL+"/sim/logs?q=reset+(flow)", "", "")["lines"].([]any)
	if len(lines) == 0 {
		t.Fatal("no line in /sim/logs marks the reset")
	}

	// all: back to the seed. The platform's token survives it: a lab reset
	// never logs the platform's Banking Circle client out.
	out = mustCall(t, 200, "POST", ctl.URL+"/sim/reset", "", `{"scope":"all"}`)
	cleared, _ = out["cleared"].(map[string]any)
	if cleared["subscriptions"] != float64(1) || cleared["accounts"] != float64(1) {
		t.Fatalf("all reset = %v, want the platform's subscription and the merchant's account cleared", out)
	}
	if _, ok := cleared["tokens"]; ok {
		t.Fatalf("all reset cleared tokens: %v", out)
	}
	if kept, _ := out["kept"].([]any); len(kept) != 1 || kept[0] != "tokens" {
		t.Fatalf("kept = %v after an all reset, want [tokens]", out["kept"])
	}

	subs := mustCall(t, 200, "GET", srv.URL+"/api/v1/notificationselfservice/subscription", tok, "")
	if result, _ := subs["result"].([]any); len(result) != 1 || result[0].(map[string]any)["id"] != seed.ID {
		t.Fatalf("subscriptions after an all reset = %v, want only the seed %s", subs["result"], seed.ID)
	}
	if accounts, _ := mustCall(t, 200, "GET", srv.URL+"/accounts", tok, "")["accounts"].([]any); len(accounts) != 2 {
		t.Fatalf("%d accounts after an all reset, want the two safeguarding accounts", len(accounts))
	}
}

// TestAPayoutInFlightAtResetNeverReachesTheSubscriberOrTheBank: payments
// still sleeping out PROCESSING_DELAY when the reset lands are dropped. No
// processed notification follows, the beneficiary's bank is never credited,
// and no money moves.
func TestAPayoutInFlightAtResetNeverReachesTheSubscriberOrTheBank(t *testing.T) {
	a := newTestApp(t)
	// An hour's delay: if the reset did not wake them, Wait would hang.
	a.engine = bankingcircle.NewEngine(a.ledger, time.Hour, a.onTransition)
	a.bank = bankingcircle.NewBank(a.engine, a.dispatch)
	var credits atomic.Int64
	bank := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		credits.Add(1)
		w.WriteHeader(200)
	}))
	defer bank.Close()
	a.bankCreditURL = bank.URL
	srv := httptest.NewServer(vendorMux(t, a))
	defer srv.Close()
	internalSrv := httptest.NewServer(a.internalMux())
	defer internalSrv.Close()
	subSrv, deliveries := captureSubscriber(t)
	subscribeAll(t, a, subSrv.URL)

	mustCall(t, 202, "POST", internalSrv.URL+"/internal/incoming-payments", "", `{"currency":"EUR","amount":"100.00","reference":"lump"}`)
	mustCall(t, 202, "POST", internalSrv.URL+"/internal/payments", "",
		`{"paymentId":"bcp_inflight","accountId":"`+merchantAccount+`","amount":"10.00","currency":"EUR","iban":"GB00SIM0000000000003","holder":"Outlet One"}`)

	tok := a.tokens.issue(time.Hour)
	mustCall(t, 200, "POST", srv.URL+"/sim/reset", tok, `{"scope":"flow"}`)
	within(t, 2*time.Second, "the payments in flight", a.engine.Wait)
	within(t, 2*time.Second, "the dispatcher", a.dispatch.Wait)
	// Longer than the flush interval, so a stray batch would have gone.
	time.Sleep(30 * time.Millisecond)

	for {
		select {
		case r := <-deliveries:
			plain, err := decryptNotification(r.body, r.nonce, r.tag, a.notifKey)
			if err != nil {
				t.Fatal(err)
			}
			var env bankingcircle.Envelope
			_ = json.Unmarshal(plain, &env)
			for _, n := range env.Notifications {
				switch bankingcircle.NotificationType(n.NotificationType) {
				case bankingcircle.NotificationOutgoingPaymentBooked, bankingcircle.NotificationIncomingPaymentBooked:
					// Booked before the reset; fine.
				default:
					t.Fatalf("%s delivered for a payment the reset dropped", n.NotificationType)
				}
			}
			continue
		default:
		}
		break
	}
	if n := credits.Load(); n != 0 {
		t.Fatalf("the beneficiary bank was credited %d time(s) for a dropped payout", n)
	}
	if got, _ := mustCall(t, 200, "GET", srv.URL+"/payments", tok, "")["payments"].([]any); len(got) != 0 {
		t.Fatalf("payments after the reset = %v", got)
	}
	for _, id := range []string{sgaEUR, merchantAccount} {
		if acc := mustCall(t, 200, "GET", srv.URL+"/accounts/"+id, tok, ""); acc["balance"] != "0.00" {
			t.Fatalf("%s after the reset = %v, want 0.00", id, acc["balance"])
		}
	}
}

// TestAResetDoesNotWaitOnAHungPeer: a webhook or a credit already on the
// wire when the reset lands is abandoned with the run it belonged to, so a
// subscriber or bank that never answers cannot hold the reset up.
func TestAResetDoesNotWaitOnAHungPeer(t *testing.T) {
	a := newTestApp(t)
	hang := make(chan struct{})
	defer close(hang)
	arrived := make(chan string, 8)
	hung := func(name string) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case arrived <- name:
			default:
			}
			select {
			case <-hang:
			case <-r.Context().Done():
			}
		}))
		t.Cleanup(s.Close)
		return s
	}
	a.bankCreditURL = hung("bank").URL
	subscribeAll(t, a, hung("subscriber").URL)
	srv := httptest.NewServer(vendorMux(t, a))
	defer srv.Close()
	internalSrv := httptest.NewServer(a.internalMux())
	defer internalSrv.Close()

	mustCall(t, 202, "POST", internalSrv.URL+"/internal/incoming-payments", "", `{"currency":"EUR","amount":"100.00","reference":"lump"}`)
	mustCall(t, 202, "POST", internalSrv.URL+"/internal/payments", "",
		`{"paymentId":"bcp_hung","accountId":"`+merchantAccount+`","amount":"10.00","currency":"EUR","iban":"GB00SIM0000000000003"}`)
	seen := map[string]bool{}
	for !seen["bank"] || !seen["subscriber"] {
		select {
		case name := <-arrived:
			seen[name] = true
		case <-time.After(3 * time.Second):
			t.Fatalf("never reached both peers: %v", seen)
		}
	}

	tok := a.tokens.issue(time.Hour)
	within(t, 2*time.Second, "the reset", func() {
		req, _ := http.NewRequest("POST", srv.URL+"/sim/reset", bytes.NewReader(nil))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	})
	within(t, 2*time.Second, "the dispatcher", a.dispatch.Wait)
	if got := a.engine.List(); len(got) != 0 {
		t.Fatalf("%d payments after the reset", len(got))
	}
}
