// Payment API is a generic B4B-style facade: create a payment with
// Idempotency-Key, move the fake ledger, then enqueue a webhook.
// A created payment must produce a webhook — not a disconnected ping.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/logring"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
)

type payment struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	DebtorAccountID string `json:"debtorAccountId"`
	CreditorIBAN    string `json:"creditorIban"`
	Amount          string `json:"amount"`
	Currency        string `json:"currency"`
	Reference       string `json:"reference,omitempty"`
	IdempotencyKey  string `json:"idempotencyKey,omitempty"`
	Notification    string `json:"notification"`
	CreatedAt       string `json:"createdAt"`
}

type store struct {
	mu    sync.Mutex
	byID  map[string]*payment
	byKey map[string]*payment
	// epoch is what a reset moves: a POST /payments still talking to the
	// bank and the notifier when a reset lands does not store its payment
	// after it.
	epoch simctl.Epoch
}

func newStore() *store {
	return &store{byID: map[string]*payment{}, byKey: map[string]*payment{}}
}

func main() {
	logring.Install()
	addr := env("LISTEN", ":8080")
	bankURL := strings.TrimRight(env("BANK_URL", "http://bank:8081"), "/")
	notifierURL := strings.TrimRight(env("NOTIFIER_URL", "http://notifier:8082"), "/")
	client := &http.Client{Timeout: 10 * time.Second}
	mux := routes(newStore(), client, bankURL, notifierURL, addr)
	log.Printf("payment-api listening on %s bank=%s notifier=%s", addr, bankURL, notifierURL)
	log.Fatal(http.ListenAndServe(addr, logReq(mux)))
}

// routes is payment-api's HTTP surface and its sim control contract.
func routes(s *store, client *http.Client, bankURL, notifierURL, addr string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, map[string]string{"status": "ok", "service": "payment-api"})
	})
	// What reached it, for the console: nothing in the settle path calls
	// this service, so an empty log is the normal state and a row here is
	// make demo-payment, the harness or somebody's curl.
	payLog := activity.New("payments", "Payments received",
		"Every POST /payments: the ledger move at the business bank and whether the webhook was enqueued at the notifier. Only make demo-payment and the harness call this.")
	mux.HandleFunc("POST /payments", payLog.Watch("payment.create", summarizePayment, func(w http.ResponseWriter, r *http.Request) {
		s.create(w, r, client, bankURL, notifierURL)
	}))
	mux.HandleFunc("GET /payments/{id}", s.get)
	mux.HandleFunc("GET /payments", s.list)
	mux.HandleFunc("GET /sim/activity", activity.Handler(payLog))

	// Both scopes do the same: a payment and its idempotency key are what a
	// run made, and nothing here is set up once.
	simctl.Mount(mux, simctl.Config{
		Service: "payment-api", Kind: "stand-in", Simulates: "a payment API of the platform's own (demo)",
		Listeners: []string{addr},
		Scopes:    []string{simctl.ScopeFlow, simctl.ScopeAll},
		Reset: func(context.Context, simctl.ResetRequest) (simctl.ResetResult, error) {
			cleared := s.reset()
			cleared["activity"] = int(payLog.Clear())
			return simctl.ResetResult{Cleared: cleared}, nil
		},
	})
	return mux
}

// reset forgets every payment and every Idempotency-Key, so a key seen
// before the reset creates a new payment after it.
func (s *store) reset() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.epoch.Bump()
	cleared := map[string]int{"payments": len(s.byID), "idempotency_keys": len(s.byKey)}
	s.byID, s.byKey = map[string]*payment{}, map[string]*payment{}
	return cleared
}

type createReq struct {
	DebtorAccountID string `json:"debtorAccountId"`
	CreditorIBAN    string `json:"creditorIban"`
	Amount          string `json:"amount"`
	Currency        string `json:"currency"`
	Reference       string `json:"reference"`
}

func (s *store) create(w http.ResponseWriter, r *http.Request, client *http.Client, bankURL, notifierURL string) {
	started := s.epoch.Current()
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	var req createReq
	if err := httputilx.ReadJSON(r, &req); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	if req.DebtorAccountID == "" || req.CreditorIBAN == "" || req.Amount == "" {
		httputilx.Error(w, 400, "debtorAccountId, creditorIban, amount required")
		return
	}
	if req.Currency == "" {
		req.Currency = "EUR"
	}
	if key != "" {
		s.mu.Lock()
		if existing, ok := s.byKey[key]; ok {
			s.mu.Unlock()
			httputilx.WriteJSON(w, 200, existing)
			return
		}
		s.mu.Unlock()
	}

	p := &payment{
		ID:              "pay_" + shortID(),
		Status:          "accepted",
		DebtorAccountID: req.DebtorAccountID,
		CreditorIBAN:    req.CreditorIBAN,
		Amount:          req.Amount,
		Currency:        req.Currency,
		Reference:       req.Reference,
		IdempotencyKey:  key,
		Notification:    "pending",
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
	}

	xfer := map[string]string{
		"fromAccountId": req.DebtorAccountID,
		"toIban":        req.CreditorIBAN,
		"amount":        req.Amount,
		"currency":      req.Currency,
		"reference":     req.Reference,
		"paymentId":     p.ID,
	}
	if err := postJSON(client, bankURL+"/transfers", xfer, nil); err != nil {
		p.Status = "rejected"
		p.Notification = "not_enqueued"
		s.put(p, key, started)
		httputilx.WriteJSON(w, 409, map[string]any{"error": "ledger rejected transfer: " + err.Error(), "payment": p})
		return
	}

	// Connected path: enqueue is part of accept. A ping that never sees
	// this payload is not a working lab.
	evt := map[string]any{
		"type":      "payment.accepted",
		"paymentId": p.ID,
		"data":      p,
	}
	if err := postJSON(client, notifierURL+"/enqueue", evt, nil); err != nil {
		p.Notification = "enqueue_failed: " + err.Error()
		s.put(p, key, started)
		httputilx.WriteJSON(w, 201, p)
		return
	}
	p.Notification = "enqueued"
	s.put(p, key, started)
	httputilx.WriteJSON(w, 201, p)
}

// put stores p under its id and key, unless a reset happened since the
// request that made it started: that payment belongs to the run the reset
// cleared.
func (s *store) put(p *payment, key string, started uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.epoch.Moved(started) {
		log.Printf("payment-api: %s finished after a reset; not kept", p.ID)
		return
	}
	s.byID[p.ID] = p
	if key != "" {
		s.byKey[key] = p
	}
}

func (s *store) get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byID[id]
	if !ok {
		httputilx.Error(w, 404, "payment not found")
		return
	}
	httputilx.WriteJSON(w, 200, p)
}

func (s *store) list(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*payment, 0, len(s.byID))
	for _, p := range s.byID {
		out = append(out, p)
	}
	httputilx.WriteJSON(w, 200, map[string]any{"payments": out})
}

func postJSON(client *http.Client, url string, body any, dest any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := client.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return errStatus(resp.StatusCode, string(raw))
	}
	if dest != nil && len(raw) > 0 {
		return json.Unmarshal(raw, dest)
	}
	return nil
}

type statusErr struct {
	code int
	msg  string
}

func errStatus(code int, msg string) error { return &statusErr{code, msg} }
func (e *statusErr) Error() string         { return e.msg }

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

func logReq(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

// summarizePayment: "payment 25.00 EUR acc_alice → GB00SIM… — accepted, webhook enqueued".
func summarizePayment(c *activity.Call) (string, map[string]string) {
	detail := map[string]string{
		"payment":         c.RespField("id"),
		"idempotency_key": c.Request.Header.Get("Idempotency-Key"),
	}
	sum := fmt.Sprintf("payment %s %s %s → %s", c.JSONField("amount"), c.JSONField("currency"), c.JSONField("debtorAccountId"), c.JSONField("creditorIban"))
	switch {
	case c.Status == 200:
		sum += " — replayed: this Idempotency-Key was seen before"
	case c.Status == 201:
		sum += " — " + c.RespField("status") + ", webhook " + c.RespField("notification")
	}
	return sum, detail
}
