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
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/aci"
	"github.com/webduvet/fintechlab-simple/internal/allowlist"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/labswitch"
	"github.com/webduvet/fintechlab-simple/internal/retry"
)

type app struct {
	secret    []byte
	client    *http.Client
	list      *allowlist.List
	backs     []time.Duration
	maxTry    int
	targetURL string
	bodyMode  string
	sent      *sentStore
	// wiring is the delivery to targetURL. Off, a simulated payment is
	// still recorded — held, saying why — and nothing is sent: the lab's
	// receiver stand-in is disconnected once a platform listens itself.
	wiring *labswitch.Switch
}

func main() {
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
		sent:      newSentStore(),
		wiring: labswitch.FromEnv("aci", "delivers webhooks to ACI_WEBHOOK_TARGET_URL",
			env("ACI_WEBHOOK_CONNECTED", "true")).Target(targetURL),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, map[string]string{"status": "ok", "service": "aci"})
	})
	mux.HandleFunc("POST /internal/simulate-payment", a.simulatePayment)
	mux.HandleFunc("GET /internal/sent", a.listSent)
	mux.HandleFunc("GET /internal/sent/{id}", a.getSent)
	a.wiring.Routes(mux, "/sim/connected")

	log.Printf("aci listening on %s target=%s body_mode=%s allowlist=%s", addr, targetURL, bodyMode, spec)
	log.Fatal(http.ListenAndServe(addr, logReq(mux)))
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

// simulatePayment implements POST /internal/simulate-payment: builds ACI's
// real notification payload from this lab's trigger shape, encrypts it, and
// queues fire-and-retry delivery. It never blocks on delivery -- the
// response is 202 with the generated event id as soon as encryption
// succeeds, matching every other sender in this lab.
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

	rec := &sentRecord{
		ID:             "aci_evt_" + shortID(),
		Notification:   notif,
		CiphertextHex:  ciphertextHex,
		IVHex:          ivHex,
		TagHex:         tagHex,
		TargetURL:      a.targetURL,
		BodyMode:       a.bodyMode,
		CreatedAt:      now,
		DeliveryStatus: "pending",
	}
	if !a.wiring.Connected() {
		// Said where the silence lands: the record is there, held.
		rec.DeliveryStatus = "held"
		rec.DeliveryError = "not delivered: the webhook target is disconnected in the lab (GET /sim/connected)"
		a.sent.put(rec)
		httputilx.WriteJSON(w, 202, simulateResp{ID: rec.ID})
		return
	}
	a.sent.put(rec)

	go a.deliver(rec)

	httputilx.WriteJSON(w, 202, simulateResp{ID: rec.ID})
}

// deliver retries the encrypted webhook POST with backoff until 2xx or
// MAX_ATTEMPTS is exhausted -- same non-fatal, log-and-continue shape as
// every other webhook sender in this lab (cmd/b4b/main.go's
// deliverWebhook). Runs in its own goroutine so a down or slow
// ACI_WEBHOOK_TARGET_URL never stalls the HTTP handler that queued it.
func (a *app) deliver(rec *sentRecord) {
	var last int
	var lastErr error
	for attempt := 1; attempt <= a.maxTry; attempt++ {
		if wait := retry.SleepBeforeAttempt(attempt, a.backs); wait > 0 {
			time.Sleep(wait)
		}
		st, err := a.postWebhook(rec)
		last, lastErr = st, err
		if err == nil {
			log.Printf("aci: webhook event=%s attempt=%d http=%d delivered", rec.ID, attempt, st)
			deliveredAt := time.Now().UTC()
			a.sent.update(rec.ID, func(r *sentRecord) {
				r.DeliveryStatus = "delivered"
				r.HTTPStatus = st
				r.DeliveryError = ""
				r.DeliveredAt = &deliveredAt
			})
			return
		}
		log.Printf("aci: webhook event=%s attempt=%d http=%d err=%v", rec.ID, attempt, st, err)
		if !retry.ShouldRetry(st, attempt, a.maxTry) {
			break
		}
	}
	log.Printf("aci: webhook event=%s failed after retries: http=%d err=%v", rec.ID, last, lastErr)
	a.sent.update(rec.ID, func(r *sentRecord) {
		r.DeliveryStatus = "failed"
		r.HTTPStatus = last
		if lastErr != nil {
			r.DeliveryError = lastErr.Error()
		}
	})
}

// postWebhook does the actual POST: the raw hex ciphertext as text/plain
// (BodyMode "raw", the default), or {"encryptedBody":"<hex>"} as
// application/json (BodyMode "json") -- the real controller sniffs a
// leading '{' to tell them apart (aci-webhook.controller.ts:70), so both
// must be real options, not just the default. IV and auth tag always travel
// as hex in headers, never in the body.
func (a *app) postWebhook(rec *sentRecord) (int, error) {
	if err := a.list.Allowed(rec.TargetURL); err != nil {
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

// --- sent-notification introspection (GET /internal/sent[/{id}]) -------

// sentRecord is one simulated notification: the plaintext payload (echoed
// back for introspection -- this lab already holds it, no need to decrypt
// its own ciphertext), the encrypted wire material, and delivery outcome.
type sentRecord struct {
	ID             string            `json:"id"`
	Notification   *aci.Notification `json:"notification"`
	CiphertextHex  string            `json:"ciphertext_hex"`
	IVHex          string            `json:"iv_hex"`
	TagHex         string            `json:"tag_hex"`
	TargetURL      string            `json:"target_url"`
	BodyMode       string            `json:"body_mode"`
	CreatedAt      time.Time         `json:"created_at"`
	DeliveryStatus string            `json:"delivery_status"`
	HTTPStatus     int               `json:"http_status,omitempty"`
	DeliveryError  string            `json:"delivery_error,omitempty"`
	DeliveredAt    *time.Time        `json:"delivered_at,omitempty"`
}

type sentStore struct {
	mu      sync.Mutex
	records map[string]*sentRecord
	order   []string
}

func newSentStore() *sentStore {
	return &sentStore{records: map[string]*sentRecord{}}
}

func (s *sentStore) put(r *sentRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[r.ID] = r
	s.order = append(s.order, r.ID)
}

// update mutates the record in place under lock -- rec, held by the
// delivering goroutine, and the store's map entry are the same pointer, so
// this is the only place delivery status is ever written after put.
func (s *sentStore) update(id string, fn func(*sentRecord)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.records[id]; ok {
		fn(r)
	}
}

func (s *sentStore) get(id string) (*sentRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if !ok {
		return nil, false
	}
	cp := *r
	return &cp, true
}

func (s *sentStore) list() []*sentRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*sentRecord, 0, len(s.order))
	for _, id := range s.order {
		cp := *s.records[id]
		out = append(out, &cp)
	}
	return out
}

func (a *app) listSent(w http.ResponseWriter, r *http.Request) {
	httputilx.WriteJSON(w, 200, map[string]any{"result": a.sent.list()})
}

func (a *app) getSent(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.sent.get(r.PathValue("id"))
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

func logReq(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
