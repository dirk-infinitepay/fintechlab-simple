// ACI is the mock for the online card-payment gateway's webhook contract,
// docs/ARCHITECTURE-phase3-corrections.md section 2 (buddy:
// apps/settle-aci-webhook/src/webhook/{aci-webhook.controller,
// aci-webhook.service,aci-decryption.service}.ts). Auth here is payload
// encryption, not a header signature or mTLS: POST /internal/simulate-payment
// ("a card payment just happened", this lab's own trigger shape) builds
// ACI's real camelCase notification payload, AES-256-GCM-encrypts it exactly
// as ACI does (internal/aci), and fire-and-retry-delivers it to
// ACI_WEBHOOK_TARGET_URL with the IV/tag carried in headers. ACI's
// merchant-onboarding REST client and its separate SFTP+PGP file-exchange
// channel are explicitly out of scope (section 2) -- this binary is the
// webhook only.
//
// The lab steers it on a control port of its own, LISTEN's port + 1000
// (:9087, SIM_CONTROL_LISTEN; internal/simctl): POST /sim/payments,
// GET /sim/sent[/{id}], the delivery switch and the control contract. The
// /internal/* trigger and reads and the old /sim paths still answer on
// LISTEN until phase 4 of docs/lab-api/plan.md retires them.
//
// Each environment the lab created is a customer here (internal/tenant),
// with a webhook target of its own (aci.webhook_url). ACI only sends, so
// nothing a platform sends names the environment: the lab's trigger does,
// in ?env= or Lab-Environment, and the notification goes to that
// environment's target. The default environment's is
// ACI_WEBHOOK_TARGET_URL, behind the connected switch; another
// environment's is its own platform listening, and the switch, which is
// about the lab's receiver stand-in, does not govern it.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/aci"
	"github.com/webduvet/fintechlab-simple/internal/allowlist"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/labswitch"
	"github.com/webduvet/fintechlab-simple/internal/logring"
	"github.com/webduvet/fintechlab-simple/internal/retry"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

type app struct {
	secret    []byte
	client    *http.Client
	list      *allowlist.List
	backs     []time.Duration
	maxTry    int
	targetURL string
	bodyMode  string
	sent      *aci.SentLog
	// wiring is the delivery to targetURL, the default environment's. Off,
	// a simulated payment is still recorded — held, saying why — and
	// nothing is sent: the lab's receiver stand-in is disconnected once a
	// platform listens itself.
	wiring *labswitch.Switch
	// customers are the environments besides the default, each with its
	// own target. control makes them when they are not given.
	customers *tenant.Customers
}

func main() {
	logring.Install()
	addr := env("LISTEN", ":8087")
	labclock.FollowEnv(context.Background(), "aci")
	// Default is a fake-obvious *valid* 64-hex-char value -- hex-decodes to
	// exactly 32 bytes (AES-256 key), clearly not ACI's real sample vector
	// (internal/aci/crypto_test.go's known-answer key), so it can never be
	// confused with a credential.
	secretHex := env("ACI_WEBHOOK_SECRET", "aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666aaaa7777bbbb8888")
	secret, err := hex.DecodeString(secretHex)
	if err != nil {
		log.Fatalf("ACI_WEBHOOK_SECRET must be hex: %v", err)
	}
	if len(secret) != 32 {
		log.Fatalf("ACI_WEBHOOK_SECRET must hex-decode to exactly 32 bytes (AES-256 key), got %d", len(secret))
	}
	targetURL := env("ACI_WEBHOOK_TARGET_URL", "http://localhost:3013/api/v1/webhook")
	bodyMode := env("ACI_WEBHOOK_BODY_MODE", "raw")
	switch bodyMode {
	case "raw", "json":
	default:
		log.Fatalf("ACI_WEBHOOK_BODY_MODE must be raw or json, got %q", bodyMode)
	}
	spec := env("WEBHOOK_ALLOWLIST", "localhost,127.0.0.1")
	list, err := allowlist.Parse(spec)
	if err != nil {
		log.Fatalf("allowlist: %v", err)
	}
	backs, err := retry.ParseBackoffs(env("RETRY_BACKOFF", "1s,2s,4s"))
	if err != nil {
		log.Fatalf("backoff: %v", err)
	}
	// The default delivery target (compose.yml) is receiver's HTTPS raw
	// capture sink, signed by this lab's CA -- a bare http.Client would
	// reject that handshake outright. CA_FILE is optional (empty target
	// URLs, or a plain-http real settle-aci-webhook target, need no
	// trust at all).
	client, err := httpClient(env("CA_FILE", "keys/certs/ca.pem"))
	if err != nil {
		log.Fatalf("aci: build http client: %v", err)
	}

	a := &app{
		secret:    secret,
		client:    client,
		list:      list,
		backs:     backs,
		maxTry:    envInt("MAX_ATTEMPTS", 4),
		targetURL: targetURL,
		bodyMode:  bodyMode,
		sent:      aci.NewSentLog(),
		wiring: labswitch.FromEnv("aci", "delivers webhooks to ACI_WEBHOOK_TARGET_URL",
			env("ACI_WEBHOOK_CONNECTED", "true")).Target(targetURL),
		customers: tenant.NewCustomers(),
	}

	ctl, err := a.control(addr)
	if err != nil {
		log.Fatalf("aci: %v", err)
	}
	ctl.Start()

	log.Printf("aci listening on %s target=%s body_mode=%s allowlist=%s", addr, targetURL, bodyMode, spec)
	log.Fatal(http.ListenAndServe(addr, logReq(a.routes(ctl.Mux))))
}

// control is ACI's control surface (internal/simctl), on a port of its own:
// the control contract and simRoutes. addr is the vendor listener, for
// GET /sim/info and the control port's default (its port + 1000).
func (a *app) control(addr string) (*simctl.Control, error) {
	if a.customers == nil {
		a.customers = tenant.NewCustomers()
	}
	// A customer removed takes its sent log and deliveries with it.
	a.customers.OnDelete = func(env string) error {
		a.sent.ResetIn(env)
		return nil
	}
	ctl, err := simctl.NewControl(simctl.Config{
		Service: "aci", Kind: "vendor", Simulates: "ACI payment notification webhooks",
		Listeners: []string{addr},
		// Both scopes, and they do the same: ACI holds nothing a platform
		// set up once. Both are offered so a lab-wide all reset reaches it.
		Scopes: []string{simctl.ScopeFlow, simctl.ScopeAll},
		Reset: func(_ context.Context, req simctl.ResetRequest) (simctl.ResetResult, error) {
			return a.reset(req.Env), nil
		},
		Customers: a.customers,
	}, simctl.ControlAddr(addr))
	if err != nil {
		return nil, err
	}
	a.simRoutes(ctl.Mux)
	return ctl, nil
}

// simRoutes are ACI's own lab hooks, on the control port's mux: make a card
// payment happen, see what was sent, and the delivery switch. The first
// three are POST /internal/simulate-payment and GET /internal/sent[/{id}]
// under their control-port names (docs/lab-api/sim-control.md §7).
func (a *app) simRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /sim/payments", a.simulatePayment)
	mux.HandleFunc("GET /sim/sent", a.listSent)
	mux.HandleFunc("GET /sim/sent/{id}", a.getSent)
	a.wiring.Routes(mux, "/sim/connected")
}

// routes is ACI's own listener. ctl is the control surface's mux: the /sim
// paths here delegate to it, and the /internal routes stay, until phase 4
// of docs/lab-api/plan.md retires them.
func (a *app) routes(ctl http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, map[string]string{"status": "ok", "service": "aci"})
	})
	mux.HandleFunc("POST /internal/simulate-payment", a.simulatePayment)
	mux.HandleFunc("GET /internal/sent", a.listSent)
	mux.HandleFunc("GET /internal/sent/{id}", a.getSent)
	mux.Handle("/sim/", ctl)
	return mux
}

// reset is POST /sim/reset (docs/lab-api/sim-control.md §6) of scope, one
// environment or every one ("*"): its sent log emptied and every delivery
// of its in flight dropped, first attempts and retries alike. The delivery
// switch stays as it was: it is live wiring -- whether a platform listens
// itself -- not something a run made.
func (a *app) reset(scope string) simctl.ResetResult {
	sent, deliveries := a.sent.ResetIn(scope)
	return simctl.ResetResult{
		Cleared: map[string]int{"sent": sent, "deliveries_in_flight": deliveries},
		Kept:    []string{"connected_switch"},
	}
}

// simulateReq is this lab's own trigger shape (snake_case) -- not ACI's
// wire shape, which is camelCase and only ever travels encrypted.
type simulateReq struct {
	MerchantTransactionID string `json:"merchant_transaction_id"`
	PaymentType           string `json:"payment_type"`
	PaymentBrand          string `json:"payment_brand"`
	PresentationAmount    string `json:"presentation_amount"`
	PresentationCurrency  string `json:"presentation_currency"`
	PluginType            string `json:"plugin_type"`
	Source                string `json:"source"`
	ResultCode            string `json:"result_code"`
	ResultDescription     string `json:"result_description"`
}

type simulateResp struct {
	ID string `json:"id"`
}

// simulatePayment implements POST /sim/payments and its alias
// POST /internal/simulate-payment: builds ACI's real notification payload
// from this lab's trigger shape, encrypts it, and queues fire-and-retry
// delivery. It never blocks on delivery -- the response is 202 with the
// generated event id as soon as encryption succeeds, matching every other
// sender in this lab.
func (a *app) simulatePayment(w http.ResponseWriter, r *http.Request) {
	var req simulateReq
	if err := httputilx.ReadJSON(r, &req); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	if req.MerchantTransactionID == "" {
		httputilx.Error(w, 400, "merchant_transaction_id required")
		return
	}
	// The environment the payment happened in, and so whose platform
	// hears of it.
	env := tenant.FromRequest(r)
	targetURL, status, why := a.target(env)
	if status != 0 {
		httputilx.Error(w, status, why)
		return
	}

	now := labclock.Now()
	notif := &aci.Notification{
		Type: "PAYMENT",
		Payload: &aci.Payload{
			ID:                    randomHex(16),
			MerchantTransactionID: req.MerchantTransactionID,
			PaymentType:           req.PaymentType,
			PaymentBrand:          req.PaymentBrand,
			PresentationAmount:    req.PresentationAmount,
			PresentationCurrency:  req.PresentationCurrency,
			PluginType:            req.PluginType,
			Source:                req.Source,
			ShortID:               shortID(),
			Timestamp:             now.Format("2006-01-02 15:04:05"),
		},
	}
	if req.ResultCode != "" || req.ResultDescription != "" {
		notif.Payload.Result = &aci.Result{Code: req.ResultCode, Description: req.ResultDescription}
	}

	plaintext, err := json.Marshal(notif)
	if err != nil {
		httputilx.Error(w, 500, "marshal notification: "+err.Error())
		return
	}
	ciphertextHex, ivHex, tagHex, err := aci.Encrypt(plaintext, a.secret)
	if err != nil {
		httputilx.Error(w, 500, "encrypt notification: "+err.Error())
		return
	}

	rec := &aci.Record{
		ID:             "aci_evt_" + shortID(),
		Notification:   notif,
		CiphertextHex:  ciphertextHex,
		IVHex:          ivHex,
		TagHex:         tagHex,
		TargetURL:      targetURL,
		BodyMode:       a.bodyMode,
		CreatedAt:      now,
		DeliveryStatus: "pending",
	}
	if env != tenant.Default {
		rec.Env = env
	}
	if env == tenant.Default && !a.wiring.Connected() {
		// Said where the silence lands: the record is there, held.
		rec.DeliveryStatus = "held"
		rec.DeliveryError = "not delivered: the webhook target is disconnected in the lab (GET /sim/connected)"
		a.sent.Put(rec)
		httputilx.WriteJSON(w, 202, simulateResp{ID: rec.ID})
		return
	}
	a.sent.Go(a.sent.Put(rec), func(started uint64) { a.deliver(rec, started) })

	httputilx.WriteJSON(w, 202, simulateResp{ID: rec.ID})
}

// target is where env's notifications go: the default's
// ACI_WEBHOOK_TARGET_URL, another environment's own webhook URL. A status
// other than 0 is why there is none, for the trigger to answer with.
func (a *app) target(env string) (url string, status int, why string) {
	switch env {
	case tenant.Default:
		return a.targetURL, 0, ""
	case tenant.All:
		return "", 400, "a card payment happens in one environment, not in all of them"
	}
	c, ok := a.customers.Get(env)
	if !ok {
		return "", 404, "no environment " + env + " at aci"
	}
	if c.ACI == nil || c.ACI.WebhookURL == "" {
		return "", 409, "environment " + env + " has no ACI webhook target: its customer needs aci.webhook_url"
	}
	return c.ACI.WebhookURL, 0, ""
}

// allowed reports whether rec may be delivered where it is going: a host
// WEBHOOK_ALLOWLIST names, or, for an environment's own notification, one
// its customer's webhook_hosts names.
func (a *app) allowed(rec *aci.Record) error {
	err := a.list.Allowed(rec.TargetURL)
	if err == nil || rec.Env == "" {
		return err
	}
	c, ok := a.customers.Get(rec.Env)
	if !ok || len(c.WebhookHosts) == 0 {
		return err
	}
	hosts, perr := allowlist.Parse(strings.Join(c.WebhookHosts, ","))
	if perr != nil {
		return fmt.Errorf("%v; environment %s's webhook_hosts: %v", err, rec.Env, perr)
	}
	if herr := hosts.Allowed(rec.TargetURL); herr != nil {
		return fmt.Errorf("%v, nor in environment %s's webhook_hosts", err, rec.Env)
	}
	return nil
}

// deliver retries the encrypted webhook POST with backoff until 2xx or
// MAX_ATTEMPTS is exhausted -- same non-fatal, log-and-continue shape as
// every other webhook sender in this lab (cmd/b4b/main.go's
// deliverWebhook). Runs in its own goroutine so a down or slow
// ACI_WEBHOOK_TARGET_URL never stalls the HTTP handler that queued it.
//
// started is the epoch rec was put in. Every attempt goes through the sent
// log's gate: after a reset the rest of the delivery is dropped, not raced.
func (a *app) deliver(rec *aci.Record, started uint64) {
	var last int
	var lastErr error
	for attempt := 1; attempt <= a.maxTry; attempt++ {
		if wait := retry.SleepBeforeAttempt(attempt, a.backs); wait > 0 {
			time.Sleep(wait)
		}
		var st int
		var err error
		if !a.sent.Do(started, func() {
			st, err = a.postWebhook(rec)
			if err == nil {
				log.Printf("aci: webhook event=%s attempt=%d http=%d delivered", rec.ID, attempt, st)
				deliveredAt := time.Now().UTC()
				a.sent.Update(rec.ID, func(r *aci.Record) {
					r.DeliveryStatus = "delivered"
					r.HTTPStatus = st
					r.DeliveryError = ""
					r.DeliveredAt = &deliveredAt
				})
			}
		}) {
			log.Printf("aci: webhook event=%s dropped before attempt %d: aci was reset", rec.ID, attempt)
			return
		}
		last, lastErr = st, err
		if err == nil {
			return
		}
		log.Printf("aci: webhook event=%s attempt=%d http=%d err=%v", rec.ID, attempt, st, err)
		if !retry.ShouldRetry(st, attempt, a.maxTry) {
			break
		}
	}
	a.sent.Do(started, func() {
		log.Printf("aci: webhook event=%s failed after retries: http=%d err=%v", rec.ID, last, lastErr)
		a.sent.Update(rec.ID, func(r *aci.Record) {
			r.DeliveryStatus = "failed"
			r.HTTPStatus = last
			if lastErr != nil {
				r.DeliveryError = lastErr.Error()
			}
		})
	})
}

// postWebhook does the actual POST: the raw hex ciphertext as text/plain
// (BodyMode "raw", the default), or {"encryptedBody":"<hex>"} as
// application/json (BodyMode "json") -- the real controller sniffs a
// leading '{' to tell them apart (aci-webhook.controller.ts:70), so both
// must be real options, not just the default. IV and auth tag always travel
// as hex in headers, never in the body.
func (a *app) postWebhook(rec *aci.Record) (int, error) {
	if err := a.allowed(rec); err != nil {
		return 0, err
	}
	var body []byte
	var contentType string
	switch rec.BodyMode {
	case "json":
		wrapped, err := json.Marshal(map[string]string{"encryptedBody": rec.CiphertextHex})
		if err != nil {
			return 0, err
		}
		body = wrapped
		contentType = "application/json"
	default:
		body = []byte(rec.CiphertextHex)
		contentType = "text/plain"
	}
	req, err := http.NewRequest(http.MethodPost, rec.TargetURL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Initialization-Vector", rec.IVHex)
	req.Header.Set("X-Authentication-Tag", rec.TagHex)
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("aci: webhook destination returned %s", resp.Status)
	}
	return resp.StatusCode, nil
}

// --- sent-notification introspection (GET /sim/sent[/{id}], and
// GET /internal/sent[/{id}] on the vendor port), of the environment the
// call names, or every one's for "*" ---

// scope is the environment a read names, when ACI serves it.
func (a *app) scope(w http.ResponseWriter, r *http.Request) (string, bool) {
	env := tenant.FromRequest(r)
	if env == tenant.Default || env == tenant.All {
		return env, true
	}
	if _, ok := a.customers.Get(env); !ok {
		httputilx.Error(w, 404, "no environment "+env+" at aci")
		return "", false
	}
	return env, true
}

func (a *app) listSent(w http.ResponseWriter, r *http.Request) {
	scope, ok := a.scope(w, r)
	if !ok {
		return
	}
	httputilx.WriteJSON(w, 200, map[string]any{"result": a.sent.ListIn(scope)})
}

func (a *app) getSent(w http.ResponseWriter, r *http.Request) {
	scope, ok := a.scope(w, r)
	if !ok {
		return
	}
	rec, ok := a.sent.GetIn(scope, r.PathValue("id"))
	if !ok {
		httputilx.Error(w, 404, "notification not found")
		return
	}
	httputilx.WriteJSON(w, 200, rec)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func shortID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// httpClient builds a client that trusts caFile in addition to the system
// roots -- the default delivery target (compose.yml) is receiver's HTTPS
// raw capture sink, signed by this lab's CA, which a bare http.Client
// would reject. An unreadable/empty caFile is not fatal (same graceful-
// degradation every other sender in this lab uses); a target that then
// fails TLS verification reports a clear connection error, not a silent
// pass, exactly like Banking Circle's own identical helper.
func httpClient(caFile string) (*http.Client, error) {
	t := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if caFile != "" {
		if pem, err := os.ReadFile(caFile); err == nil {
			if pool, ok := httputilx.RootsWithLabCA(pem); ok {
				t.TLSClientConfig.RootCAs = pool
			} else {
				log.Printf("CA_FILE %s had no certs, using system roots", caFile)
			}
		} else {
			log.Printf("CA_FILE %s not readable (%v); HTTPS delivery targets will fail until certs exist", caFile, err)
		}
	}
	return &http.Client{Timeout: 8 * time.Second, Transport: t}, nil
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

// logReq logs every request but the reads of the log itself: a reader
// polling GET /sim/logs would otherwise fill the ring with its own polls.
func logReq(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/sim/logs" {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}
