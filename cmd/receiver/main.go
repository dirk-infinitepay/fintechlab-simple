// Receiver accepts TLS webhooks from the local CA, verifies HMAC, stores events.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/hmacx"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/logring"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
	"github.com/webduvet/fintechlab-simple/internal/waitfor"
)

type event struct {
	ID         string            `json:"id"`
	ReceivedAt string            `json:"receivedAt"`
	Type       string            `json:"type,omitempty"`
	PaymentID  string            `json:"paymentId,omitempty"`
	Headers    map[string]string `json:"headers"`
	Body       json.RawMessage   `json:"body"`
}

// rawEvent captures a webhook delivery this lab cannot authenticate itself
// (Banking Circle/ACI use their own encryption/signature schemes receiver
// holds no keys for — see docs/ARCHITECTURE-phase3-corrections.md section 6).
// It proves delivery reached the destination with these headers and this
// many bytes, and never decrypts or verifies anything -- it holds no keys.
//
// The body is retained, base64-encoded, up to maxRawBodyBytes. It used to
// be discarded, which meant nothing could check *what* was delivered: a
// caller holding the encryption key (the harness does) can now decrypt the
// capture and assert on the payload, which is the difference between
// proving a message arrived and proving the right message arrived.
type rawEvent struct {
	ID         string              `json:"id"`
	ReceivedAt string              `json:"receivedAt"`
	Path       string              `json:"path"`
	Headers    map[string][]string `json:"headers"`
	BodySize   int                 `json:"bodySize"`
	// Body is base64 of the raw bytes, empty if the body exceeded
	// maxRawBodyBytes.
	Body string `json:"body,omitempty"`
}

// maxRawBodyBytes bounds what a capture retains. This is a sink for
// anything at all, so an unbounded retained body is an unbounded memory
// leak the first time something posts a large file at it.
const maxRawBodyBytes = 1 << 20

type store struct {
	mu        sync.Mutex
	events    []event
	rawEvents []rawEvent
}

func main() {
	logring.Install()
	addr := env("LISTEN", ":8443")
	cert := env("TLS_CERT", "/keys/certs/receiver.pem")
	key := env("TLS_KEY", "/keys/certs/receiver-key.pem")
	secret := []byte(env("HMAC_SECRET", "sim-hmac-dev-only"))
	s := &store{}

	// The ca service and this one start concurrently under compose; not
	// every compose implementation blocks on "depends_on" the same way
	// (podman-compose in particular does not), so wait for our own files
	// instead of trusting orchestrator ordering.
	if err := waitfor.Files(30*time.Second, cert, key); err != nil {
		log.Fatalf("receiver: %v", err)
	}
	log.Printf("receiver listening https %s", addr)
	log.Fatal(http.ListenAndServeTLS(addr, cert, key, logReq(s.routes(secret, addr))))
}

// routes is the receiver's HTTP surface: the HMAC-checked listener, the
// raw capture sink, what each stored, and the sim control contract.
func (s *store) routes(secret []byte, addr string) *http.ServeMux {
	const skew = 10 * time.Minute
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, map[string]string{"status": "ok", "service": "receiver"})
	})
	mux.HandleFunc("POST /webhooks", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			httputilx.Error(w, 400, "read body")
			return
		}
		sig := r.Header.Get(hmacx.HeaderSignature)
		ts := r.Header.Get(hmacx.HeaderTimestamp)
		if err := hmacx.Verify(secret, ts, sig, body, time.Now(), skew); err != nil {
			log.Printf("hmac reject: %v", err)
			httputilx.Error(w, 401, "hmac verification failed")
			return
		}
		var preview struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			PaymentID string `json:"paymentId"`
		}
		_ = json.Unmarshal(body, &preview)
		id := preview.ID
		if id == "" {
			id = r.Header.Get(hmacx.HeaderEventID)
		}
		if id == "" {
			id = "recv_" + shortID()
		}
		ev := event{
			ID:         id,
			ReceivedAt: time.Now().UTC().Format(time.RFC3339),
			Type:       preview.Type,
			PaymentID:  preview.PaymentID,
			Headers: map[string]string{
				hmacx.HeaderSignature: sig,
				hmacx.HeaderTimestamp: ts,
				hmacx.HeaderEventID:   r.Header.Get(hmacx.HeaderEventID),
			},
			Body: json.RawMessage(append([]byte(nil), body...)),
		}
		s.mu.Lock()
		s.events = append(s.events, ev)
		s.mu.Unlock()
		log.Printf("stored event %s type=%s payment=%s", id, preview.Type, preview.PaymentID)
		httputilx.WriteJSON(w, 200, map[string]string{"status": "stored", "id": id})
	})
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		httputilx.WriteJSON(w, 200, map[string]any{"events": s.events})
	})

	// POST /raw-events: unvalidated capture for any webhook wire format this
	// lab has no keys for (no signature/HMAC/crypto check whatsoever, by
	// design — see the rawEvent doc comment above). Accepts any
	// content-type and any body.
	mux.HandleFunc("POST /raw-events", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxRawBodyBytes+1))
		if err != nil {
			httputilx.Error(w, 400, "read body")
			return
		}
		headers := make(map[string][]string, len(r.Header))
		for k, v := range r.Header {
			headers[k] = append([]string(nil), v...)
		}
		ev := rawEvent{
			ID:         "raw_" + shortID(),
			ReceivedAt: time.Now().UTC().Format(time.RFC3339),
			Path:       r.URL.RequestURI(),
			Headers:    headers,
			BodySize:   len(body),
		}
		if len(body) <= maxRawBodyBytes {
			ev.Body = base64.StdEncoding.EncodeToString(body)
		}
		s.mu.Lock()
		s.rawEvents = append(s.rawEvents, ev)
		s.mu.Unlock()
		log.Printf("stored raw event %s path=%s size=%d", ev.ID, ev.Path, ev.BodySize)
		httputilx.WriteJSON(w, 200, map[string]string{"status": "received"})
	})
	mux.HandleFunc("GET /raw-events", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		httputilx.WriteJSON(w, 200, map[string]any{"raw_events": s.rawEvents})
	})

	// Both scopes do the same: everything here is what a run delivered.
	simctl.Mount(mux, simctl.Config{
		Service: "receiver", Kind: "stand-in", Simulates: "the platform's webhook listener",
		Listeners: []string{addr},
		Scopes:    []string{simctl.ScopeFlow, simctl.ScopeAll},
		Reset: func(context.Context, simctl.ResetRequest) (simctl.ResetResult, error) {
			events, raw := s.reset()
			return simctl.ResetResult{Cleared: map[string]int{"events": events, "raw_events": raw}}, nil
		},
	})
	return mux
}

// reset empties both stores and says how many each held. GET /events and
// GET /raw-events answer null again, as on first start.
func (s *store) reset() (events, raw int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	events, raw = len(s.events), len(s.rawEvents)
	s.events, s.rawEvents = nil, nil
	return events, raw
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

func logReq(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
