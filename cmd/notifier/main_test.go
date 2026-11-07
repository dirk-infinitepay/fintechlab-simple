package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/allowlist"
)

// listener stands in for the receiver: it counts deliveries and answers
// each with status, after hold returns.
type listener struct {
	*httptest.Server
	got atomic.Int32
}

func newListener(t *testing.T, status int, hold func()) *listener {
	t.Helper()
	l := &listener{}
	l.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.got.Add(1)
		if hold != nil {
			hold()
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(l.Close)
	return l
}

func newTestApp(t *testing.T, dest string, backs []time.Duration, maxTry int) *app {
	t.Helper()
	list, err := allowlist.Parse("127.0.0.1,receiver,receiver:8443")
	if err != nil {
		t.Fatal(err)
	}
	return newApp(dest, list, []byte("secret"), &http.Client{Timeout: 5 * time.Second}, backs, maxTry)
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

type resetAnswer struct {
	Cleared map[string]int `json:"cleared"`
	Kept    []string       `json:"kept"`
}

func reset(t *testing.T, h http.Handler, scope string) resetAnswer {
	t.Helper()
	w := do(t, h, "POST", "/sim/reset", `{"scope":"`+scope+`"}`)
	var got resetAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
	return got
}

// waitFor polls cond briefly: the delivery goroutine records its attempt
// a moment after the listener answers.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (a *app) state() (deliveries []*delivery, sub subscription, events int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*delivery(nil), a.deliver...), *a.sub, len(a.log.Recent(0))
}

// TestResetStopsADeliveryWaitingToRetry: a delivery that failed once and
// is sleeping out its backoff when the reset lands makes no further
// attempt, records nothing more, and does not mark the subscription failed
// when it would have run out of retries.
func TestResetStopsADeliveryWaitingToRetry(t *testing.T) {
	l := newListener(t, http.StatusInternalServerError, nil)
	a := newTestApp(t, l.URL+"/webhooks", []time.Duration{time.Hour}, 2)
	mux := a.routes(":8082")

	if w := do(t, mux, "POST", "/enqueue", `{"paymentId":"pay_1"}`); w.Code != 202 {
		t.Fatalf("enqueue: %d %s", w.Code, w.Body)
	}
	waitFor(t, "the first attempt", func() bool { _, _, events := a.state(); return events == 1 })

	got := reset(t, mux, "flow")
	if got.Cleared["deliveries"] != 1 || got.Cleared["in_flight"] != 1 || got.Cleared["activity"] != 1 {
		t.Errorf("cleared = %v", got.Cleared)
	}
	a.running.Wait() // returns at once: the reset woke it

	if n := l.got.Load(); n != 1 {
		t.Errorf("the listener got %d deliveries, want 1: a retry went out after the reset", n)
	}
	deliveries, sub, events := a.state()
	if len(deliveries) != 0 || events != 0 {
		t.Errorf("after the reset: %d deliveries, %d events", len(deliveries), events)
	}
	if sub.Status != "active" {
		t.Errorf("subscription = %+v: a dropped delivery marked it failed", sub)
	}
}

// TestResetDropsTheOutcomeOfADeliveryOnTheWire: a delivery whose request
// is still open when the reset lands records nothing when it comes back —
// not on the delivery, not in the log, not on the subscription.
func TestResetDropsTheOutcomeOfADeliveryOnTheWire(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	l := newListener(t, http.StatusInternalServerError, func() { close(arrived); <-release })
	a := newTestApp(t, l.URL+"/webhooks", []time.Duration{time.Millisecond}, 1)
	mux := a.routes(":8082")

	do(t, mux, "POST", "/enqueue", `{"paymentId":"pay_2"}`)
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the delivery never reached the listener")
	}
	if got := reset(t, mux, "flow"); got.Cleared["in_flight"] != 1 || got.Cleared["activity"] != 0 {
		t.Errorf("cleared = %v", got.Cleared)
	}
	close(release)
	a.running.Wait()

	deliveries, sub, events := a.state()
	if len(deliveries) != 0 || events != 0 || sub.Status != "active" {
		t.Errorf("after the reset: deliveries %v, %d events, subscription %+v", deliveries, events, sub)
	}
}

// TestFlowResetKeepsTheSubscription: the subscription is what a platform
// set up once and survives a flow reset with its id and URL; a failed
// status, which only a run's exhausted delivery sets, is active again.
func TestFlowResetKeepsTheSubscription(t *testing.T) {
	a := newTestApp(t, "https://receiver:8443/webhooks", nil, 1)
	mux := a.routes(":8082")
	w := do(t, mux, "POST", "/subscriptions", `{"url":"https://receiver:8443/other"}`)
	var set subscription
	if err := json.Unmarshal(w.Body.Bytes(), &set); err != nil || w.Code != 201 {
		t.Fatalf("subscribe: %d %s", w.Code, w.Body)
	}
	a.mu.Lock()
	a.sub.Status = "failed"
	a.mu.Unlock()

	got := reset(t, mux, "flow")
	if got.Cleared["failed_subscription"] != 1 || len(got.Kept) != 1 || got.Kept[0] != "subscription" {
		t.Errorf("reset = %+v", got)
	}
	if _, sub, _ := a.state(); sub.ID != set.ID || sub.URL != set.URL || sub.Status != "active" {
		t.Errorf("subscription after a flow reset = %+v, want %+v active", sub, set)
	}
	if w := do(t, mux, "POST", "/enqueue", `{}`); w.Code != 202 {
		t.Errorf("enqueue after a flow reset: %d %s", w.Code, w.Body)
	}
}

// TestAllResetGoesBackToWebhookURL: all is first start, so a subscription
// set since is replaced by the WEBHOOK_URL one.
func TestAllResetGoesBackToWebhookURL(t *testing.T) {
	a := newTestApp(t, "https://receiver:8443/webhooks", nil, 1)
	mux := a.routes(":8082")
	do(t, mux, "POST", "/subscriptions", `{"url":"https://receiver:8443/other"}`)

	got := reset(t, mux, "all")
	if got.Cleared["subscription"] != 1 || got.Cleared["failed_subscription"] != 0 || len(got.Kept) != 0 {
		t.Errorf("reset = %+v", got)
	}
	if _, sub, _ := a.state(); sub != *defaultSub("https://receiver:8443/webhooks") {
		t.Errorf("subscription after an all reset = %+v", sub)
	}
}
