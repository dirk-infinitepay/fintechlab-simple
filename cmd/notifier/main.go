// Notifier delivers HMAC-SHA256 signed HTTPS webhooks with retries and
// a hard destination allowlist.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/allowlist"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/logring"
	"github.com/webduvet/fintechlab-simple/internal/retry"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
	"github.com/webduvet/fintechlab-simple/internal/waitfor"
	"github.com/webduvet/fintechlab-simple/internal/webhook"
)

type subscription struct {
	ID     string `json:"id"`
	URL    string `json:"url"`
	Status string `json:"status"`
}

type delivery struct {
	ID             string `json:"id"`
	SubscriptionID string `json:"subscriptionId"`
	Type           string `json:"type"`
	PaymentID      string `json:"paymentId,omitempty"`
	Attempts       int    `json:"attempts"`
	LastStatus     int    `json:"lastStatus"`
	LastError      string `json:"lastError,omitempty"`
	State          string `json:"state"`
	CreatedAt      string `json:"createdAt"`
	UpdatedAt      string `json:"updatedAt"`
}

type enqueueReq struct {
	Type      string          `json:"type"`
	PaymentID string          `json:"paymentId"`
	Data      json.RawMessage `json:"data"`
}

type app struct {
	mu  sync.Mutex
	sub *subscription
	// dest is WEBHOOK_URL: the subscription the service starts with, and
	// the one a reset with scope all goes back to.
	dest    string
	list    *allowlist.List
	secret  []byte
	client  *http.Client
	backs   []time.Duration
	maxTry  int
	deliver []*delivery
	// log is every delivery attempt's outcome, for the console.
	log *activity.Log

	// epoch is what a reset moves. A delivery takes it when it is
	// enqueued and checks it before each step — an attempt, recording an
	// attempt's outcome, marking the subscription failed — so a reset is
	// never followed by a stray webhook or a failed subscription for a
	// delivery that no longer exists. Cancelling ctx wakes the deliveries
	// asleep between attempts, so they see it at once.
	epoch  simctl.Epoch
	ctx    context.Context
	cancel context.CancelFunc
	// running is every delivery's goroutine, for the tests to wait on.
	running sync.WaitGroup
}

func newApp(dest string, list *allowlist.List, secret []byte, client *http.Client, backs []time.Duration, maxTry int) *app {
	a := &app{
		sub:    defaultSub(dest),
		dest:   dest,
		list:   list,
		secret: secret,
		client: client,
		backs:  backs,
		maxTry: maxTry,
		log: activity.New("deliveries", "Webhooks delivered",
			"Each signed webhook payment-api enqueued, and how its delivery to "+dest+" went — attempts, the status it got, retries. Only make demo-payment and the harness enqueue anything."),
	}
	a.ctx, a.cancel = context.WithCancel(context.Background())
	return a
}

func defaultSub(dest string) *subscription {
	return &subscription{ID: "sub_default", URL: dest, Status: "active"}
}

func main() {
	logring.Install()
	addr := env("LISTEN", ":8082")
	dest := env("WEBHOOK_URL", "https://receiver:8443/webhooks")
	spec := env("WEBHOOK_ALLOWLIST", "receiver,receiver:8443,localhost,127.0.0.1")
	list, err := allowlist.Parse(spec)
	if err != nil {
		log.Fatalf("allowlist: %v", err)
	}
	if err := list.Allowed(dest); err != nil {
		log.Fatalf("default WEBHOOK_URL rejected by allowlist: %v", err)
	}
	backs, err := retry.ParseBackoffs(env("RETRY_BACKOFF", "1s,2s,4s"))
	if err != nil {
		log.Fatalf("backoff: %v", err)
	}
	secret := []byte(env("HMAC_SECRET", "sim-hmac-dev-only"))
	caFile := env("CA_FILE", "/keys/certs/ca.pem")
	// receiver's cert is signed by this CA; the ca and receiver services
	// start concurrently with us under compose, so wait rather than trust
	// orchestrator ordering (podman-compose does not block on it — see
	// docs/security/ca-and-tls.md).
	if err := waitfor.Files(30*time.Second, caFile); err != nil {
		log.Printf("notifier: %v; continuing, HTTPS to receiver will fail until certs exist", err)
	}
	client, err := httpClient(caFile)
	if err != nil {
		log.Fatalf("tls client: %v", err)
	}
	a := newApp(dest, list, secret, client, backs, envInt("MAX_ATTEMPTS", 4))
	log.Printf("notifier listening on %s dest=%s allowlist=%s", addr, dest, spec)
	log.Fatal(http.ListenAndServe(addr, logReq(a.routes(addr))))
}

// routes is the notifier's HTTP surface and its sim control contract.
func (a *app) routes(addr string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, map[string]string{"status": "ok", "service": "notifier"})
	})
	mux.HandleFunc("GET /subscriptions", a.getSubs)
	mux.HandleFunc("POST /subscriptions", a.putSub)
	mux.HandleFunc("POST /enqueue", a.enqueue)
	mux.HandleFunc("GET /deliveries", a.listDeliveries)
	mux.HandleFunc("GET /sim/activity", activity.Handler(a.log))
	simctl.Mount(mux, simctl.Config{
		Service: "notifier", Kind: "stand-in", Simulates: "a webhook worker of the platform's own (demo)",
		Listeners: []string{addr},
		Scopes:    []string{simctl.ScopeFlow, simctl.ScopeAll},
		Reset:     a.reset,
	})
	return mux
}

// reset is POST /sim/reset. Both scopes clear the deliveries — stopping
// those still running — and the activity log, and put a subscription a run
// marked failed back to active: failed is what a delivery that ran out of
// retries made, and that delivery is gone. flow keeps the subscription
// itself (its id and URL), because it is what a platform set up once;
// all goes back to the WEBHOOK_URL one, as on first start.
func (a *app) reset(_ context.Context, req simctl.ResetRequest) (simctl.ResetResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.epoch.Bump()
	a.cancel()
	a.ctx, a.cancel = context.WithCancel(context.Background())

	inFlight := 0
	for _, d := range a.deliver {
		if d.State == "pending" {
			inFlight++
		}
	}
	cleared := map[string]int{"deliveries": len(a.deliver), "in_flight": inFlight, "failed_subscription": 0}
	a.deliver = nil
	cleared["activity"] = int(a.log.Clear())
	if a.sub.Status == "failed" {
		cleared["failed_subscription"] = 1
	}
	if req.Scope == simctl.ScopeAll {
		cleared["subscription"] = 0
		if def := defaultSub(a.dest); a.sub.ID != def.ID || a.sub.URL != def.URL {
			cleared["subscription"] = 1
		}
		a.sub = defaultSub(a.dest)
		return simctl.ResetResult{Cleared: cleared}, nil
	}
	a.sub.Status = "active"
	return simctl.ResetResult{Cleared: cleared, Kept: []string{"subscription"}}, nil
}

func (a *app) getSubs(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	httputilx.WriteJSON(w, 200, map[string]any{"subscriptions": []*subscription{a.sub}})
}

type subReq struct {
	URL string `json:"url"`
}

func (a *app) putSub(w http.ResponseWriter, r *http.Request) {
	var req subReq
	if err := httputilx.ReadJSON(r, &req); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	if err := a.list.Allowed(req.URL); err != nil {
		httputilx.Error(w, 403, err.Error())
		return
	}
	a.mu.Lock()
	a.sub = &subscription{ID: "sub_" + shortID(), URL: req.URL, Status: "active"}
	sub := *a.sub
	a.mu.Unlock()
	httputilx.WriteJSON(w, 201, sub)
}

func (a *app) enqueue(w http.ResponseWriter, r *http.Request) {
	var req enqueueReq
	if err := httputilx.ReadJSON(r, &req); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	if req.Type == "" {
		req.Type = "payment.accepted"
	}
	a.mu.Lock()
	sub := *a.sub
	a.mu.Unlock()
	if sub.Status != "active" {
		httputilx.Error(w, 409, "subscription is "+sub.Status)
		return
	}
	if err := a.list.Allowed(sub.URL); err != nil {
		httputilx.Error(w, 403, err.Error())
		return
	}
	id := "evt_" + shortID()
	payload, _ := json.Marshal(map[string]any{
		"id":        id,
		"type":      req.Type,
		"paymentId": req.PaymentID,
		"createdAt": time.Now().UTC().Format(time.RFC3339),
		"data":      json.RawMessage(nonzeroJSON(req.Data)),
	})
	d := &delivery{
		ID:             id,
		SubscriptionID: sub.ID,
		Type:           req.Type,
		PaymentID:      req.PaymentID,
		State:          "pending",
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		UpdatedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	a.mu.Lock()
	a.deliver = append(a.deliver, d)
	started, ctx := a.epoch.Current(), a.ctx
	a.mu.Unlock()
	a.running.Add(1)
	go a.run(d, sub.URL, payload, started, ctx)
	httputilx.WriteJSON(w, 202, map[string]any{"eventId": id, "state": "pending"})
}

// run delivers d with retries. started and ctx are the epoch and its
// context at enqueue: once a reset moves the epoch, run drops the delivery
// at its next step and records nothing more about it.
func (a *app) run(d *delivery, dest string, payload []byte, started uint64, ctx context.Context) {
	defer a.running.Done()
	var last int
	var lastErr error
	for attempt := 1; attempt <= a.maxTry; attempt++ {
		if wait := retry.SleepBeforeAttempt(attempt, a.backs); wait > 0 {
			sleep(ctx, wait)
		}
		a.mu.Lock()
		if a.epoch.Moved(started) {
			a.mu.Unlock()
			return // reset while waiting: no further attempt
		}
		if a.sub != nil && a.sub.Status == "failed" && a.sub.ID == d.SubscriptionID {
			d.State = "aborted"
			d.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			a.mu.Unlock()
			return
		}
		a.mu.Unlock()

		st, err := webhook.Deliver(a.client, dest, a.list, a.secret, []byte(d.ID), payload, time.Now())
		last, lastErr = st, err
		a.mu.Lock()
		if a.epoch.Moved(started) {
			a.mu.Unlock()
			log.Printf("delivery %s attempt=%d finished after a reset; dropped", d.ID, attempt)
			return // reset while on the wire: its outcome belongs to a cleared run
		}
		d.Attempts = attempt
		d.LastStatus = st
		if err != nil {
			d.LastError = err.Error()
		} else {
			d.LastError = ""
		}
		d.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if err == nil {
			d.State = "delivered"
			// Recorded under a.mu, so a reset's Clear cannot fall between
			// the epoch check and the event.
			a.log.Record(activity.Event{
				Op: "delivery", Peer: dest, Status: activity.StatusOK,
				Summary: fmt.Sprintf("%s for %s delivered — HTTP %d on attempt %d", d.Type, d.PaymentID, st, attempt),
				Detail:  map[string]string{"event": d.ID, "payment": d.PaymentID, "endpoint": dest},
			})
			a.mu.Unlock()
			log.Printf("delivered %s attempt=%d status=%d", d.ID, attempt, st)
			return
		}
		a.log.Record(activity.Event{
			Op: "delivery", Peer: dest, Status: activity.StatusWarn,
			Summary: fmt.Sprintf("%s for %s, attempt %d of %d — %v", d.Type, d.PaymentID, attempt, a.maxTry, err),
			Detail:  map[string]string{"event": d.ID, "payment": d.PaymentID, "endpoint": dest},
		})
		a.mu.Unlock()
		log.Printf("delivery %s attempt=%d status=%d err=%v", d.ID, attempt, st, err)
		if !retry.ShouldRetry(st, attempt, a.maxTry) {
			break
		}
	}
	a.mu.Lock()
	if a.epoch.Moved(started) {
		a.mu.Unlock()
		return // a reset must not mark the subscription it kept failed
	}
	d.State = "failed"
	if lastErr != nil {
		d.LastError = lastErr.Error()
	}
	d.LastStatus = last
	d.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if a.sub != nil && a.sub.ID == d.SubscriptionID {
		a.sub.Status = "failed"
	}
	a.log.Record(activity.Event{
		Op: "delivery.failed", Peer: dest, Status: activity.StatusBad,
		Summary: fmt.Sprintf("%s for %s gave up after %d attempt(s); the subscription is marked failed", d.Type, d.PaymentID, d.Attempts),
		Detail:  map[string]string{"event": d.ID, "payment": d.PaymentID, "endpoint": dest},
	})
	a.mu.Unlock()
	log.Printf("subscription marked failed after %s exhausted retries", d.ID)
}

// sleep waits d, or until ctx is cancelled by a reset.
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

func (a *app) listDeliveries(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	httputilx.WriteJSON(w, 200, map[string]any{"deliveries": a.deliver})
}

func httpClient(caFile string) (*http.Client, error) {
	t := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if caFile != "" {
		if pem, err := os.ReadFile(caFile); err == nil {
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(pem) {
				t.TLSClientConfig.RootCAs = pool
			} else {
				log.Printf("CA_FILE %s had no certs, using system roots", caFile)
			}
		} else {
			log.Printf("CA_FILE %s not readable (%v); HTTPS to lab receiver will fail until certs exist", caFile, err)
		}
	}
	return &http.Client{Timeout: 8 * time.Second, Transport: t}, nil
}

func nonzeroJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}

func shortID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func logReq(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
