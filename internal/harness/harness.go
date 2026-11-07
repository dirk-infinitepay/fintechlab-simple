// Package harness runs black-box scenario tests against a running
// fintechlab-simple stack: payment-api, bank, notifier, receiver, and
// (once running) settlement, worldline, banking-circle.
//
// A scenario is judged the same way this whole repo judges itself: a
// created record must produce a real, observable downstream effect. A
// health ping is not a scenario.
package harness

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Env holds every peer service's base URL plus a client pre-configured to
// trust the receiver's TLS certificate. Defaults point at the host-published
// ports from `make up` (127.0.0.1:*) so `go run ./cmd/harness` works right
// after `make up` with no environment variables set, exactly like
// scripts/demo-payment.sh. Running containerized (docker/Dockerfile.harness)
// overrides every URL to the in-network compose service name — see
// `make harness-docker`.
type Env struct {
	// BankingCircleControlURL is Banking Circle's control port.
	BankingCircleControlURL string
	PaymentAPIURL           string
	BankURL                 string
	NotifierURL             string
	ReceiverURL             string
	// ReceiverDeliveryURL is the base the Banking Circle twin POSTs
	// webhooks to. Distinct from ReceiverURL when the harness runs on the
	// host (127.0.0.1) but the twin runs in compose (receiver:8443).
	ReceiverDeliveryURL string
	SettlementURL       string
	WorldlineURL        string
	BankingCircleURL    string
	// Lab seams for funding / handoff. On standalone this is plain
	// INTERNAL_LISTEN (:8095), separate from the mTLS Connect surface on
	// :8085 — the lab seams are deliberately not behind client certs
	// BankingCircleURL so the harness presents mTLS like real Connect.
	BankingCircleInternalURL string
	ACIURL                   string
	// B4B's Oversight API and the keypair needed to sign its RS512 bearer
	// token. Shared read-only from the same volume the b4b service
	// generates into, so the harness signs with the key b4b itself
	// verifies against.
	B4BURL               string
	B4BJWTPrivateKeyPath string
	B4BJWTKeyID          string
	// Client trusts CA_FILE and, when CLIENT_CERT/CLIENT_KEY are present,
	// presents that client certificate — Banking Circle's real-shaped API
	// requires mTLS (docs/ARCHITECTURE-vendor-corrections.md section 3);
	// every other https peer (receiver) simply never asks for one.
	Client *http.Client
	// Worldline's real SFTP+PGP channel (docs/ARCHITECTURE-vendor-
	// corrections.md section 2): host/port for the SSH/SFTP dial, the
	// configured lab password, and the PGP keypair directory worldline
	// already generated into (shared read-only via a bind-mounted volume)
	// so the harness can decrypt what it downloads without generating its
	// own key.
	WorldlineSFTPHost          string
	WorldlineSFTPPort          string
	WorldlineSFTPPassword      string
	WorldlinePGPPublicKeyPath  string
	WorldlinePGPPrivateKeyPath string
}

func EnvFromOS() (*Env, error) {
	// Where the lab generated its keys (the Makefile's LAB_KEYS_DIR), so a
	// harness run against a lab keeping them elsewhere needs one variable,
	// not five.
	keys := env("LAB_KEYS_DIR", "keys")
	client, err := NewTLSClient(
		env("CA_FILE", filepath.Join(keys, "certs/ca.pem")),
		env("CLIENT_CERT", filepath.Join(keys, "certs/client.pem")),
		env("CLIENT_KEY", filepath.Join(keys, "certs/client-key.pem")),
	)
	if err != nil {
		return nil, fmt.Errorf("harness: tls client: %w", err)
	}
	return &Env{
		PaymentAPIURL:       env("PAYMENT_API_URL", "http://127.0.0.1:8080"),
		BankURL:             env("BANK_URL", "http://127.0.0.1:8081"),
		NotifierURL:         env("NOTIFIER_URL", "http://127.0.0.1:8082"),
		ReceiverURL:         env("RECEIVER_URL", "https://127.0.0.1:8443"),
		ReceiverDeliveryURL: env("RECEIVER_DELIVERY_URL", ""),
		SettlementURL:       env("SETTLEMENT_URL", "http://127.0.0.1:8083"),
		WorldlineURL:        env("WORLDLINE_URL", "http://127.0.0.1:8084"),
		// The standalone Banking Circle mock: mTLS :8085 for the Connect
		// surface, plain :8095 for the internal/lab seams.
		BankingCircleURL:         env("BANKING_CIRCLE_URL", "https://127.0.0.1:8085"),
		BankingCircleInternalURL: env("BANKING_CIRCLE_INTERNAL_URL", "http://127.0.0.1:8095"),
		// Banking Circle's control port: /sim/*, traps and reset, plain
		// HTTP with no vendor login (internal/simctl).
		BankingCircleControlURL:    env("BANKING_CIRCLE_CONTROL_URL", "http://127.0.0.1:9085"),
		ACIURL:                     env("ACI_URL", "http://127.0.0.1:8087"),
		B4BURL:                     env("B4B_URL", "http://127.0.0.1:8086"),
		B4BJWTPrivateKeyPath:       env("B4B_JWT_PRIVATE_KEY_PATH", filepath.Join(keys, "b4b-keys/private.pem")),
		B4BJWTKeyID:                env("B4B_JWT_KEY_ID", "b4b-mock-1"),
		Client:                     client,
		WorldlineSFTPHost:          env("WORLDLINE_SFTP_HOST", "127.0.0.1"),
		WorldlineSFTPPort:          env("WORLDLINE_SFTP_PORT", "2222"),
		WorldlineSFTPPassword:      env("WORLDLINE_SFTP_PASSWORD", "sim-sftp-dev-only"),
		WorldlinePGPPublicKeyPath:  env("WORLDLINE_PGP_PUBLIC_KEY_PATH", filepath.Join(keys, "wlsftp-keys/worldline_public.asc")),
		WorldlinePGPPrivateKeyPath: env("WORLDLINE_PGP_PRIVATE_KEY_PATH", filepath.Join(keys, "wlsftp-keys/worldline_private.asc")),
	}, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// State is shared, ordered mutation between scenarios — e.g. the
// settlement record ID scenario 2 creates is what scenario 3 needs to find
// its matching Banking Circle payout. Scenarios run strictly sequentially
// (RunAll never parallelizes), so a plain map needs no locking.
type State struct {
	values map[string]string
}

func newState() *State { return &State{values: map[string]string{}} }

func (s *State) Set(key, value string) { s.values[key] = value }

// Get returns the value and whether it was present — a missing key almost
// always means an earlier scenario didn't run or didn't reach the point
// that sets it, which callers should report as a failure, not a zero value.
func (s *State) Get(key string) (string, bool) {
	v, ok := s.values[key]
	return v, ok
}

// Scenario is one named, ordered check. Later scenarios may depend on state
// left behind by earlier ones (a payment must exist before it can be
// settled) — RunAll runs them in registration order and stops at the first
// failure, since there is no point settling a payment that was never
// created.
type Scenario struct {
	Name string
	Run  func(ctx context.Context, env *Env, state *State) error
}

type Result struct {
	Name     string
	Err      error
	Duration time.Duration
}

// RunAll executes scenarios in order, stopping at the first failure.
func RunAll(ctx context.Context, env *Env, scenarios []Scenario) []Result {
	state := newState()
	results := make([]Result, 0, len(scenarios))
	for _, s := range scenarios {
		start := time.Now()
		err := s.Run(ctx, env, state)
		results = append(results, Result{Name: s.Name, Err: err, Duration: time.Since(start)})
		if err != nil {
			break
		}
	}
	return results
}

// Report prints a pass/fail summary and returns a process exit code.
func Report(results []Result, total int) int {
	fail := false
	for _, r := range results {
		status := "PASS"
		if r.Err != nil {
			status = "FAIL"
			fail = true
		}
		fmt.Printf("[%s] %-32s %s\n", status, r.Name, r.Duration.Round(time.Millisecond))
		if r.Err != nil {
			fmt.Printf("       %v\n", r.Err)
		}
	}
	if skipped := total - len(results); skipped > 0 {
		fmt.Printf("[SKIP] %d scenario(s) not reached\n", skipped)
	}
	if fail {
		return 1
	}
	if len(results) < total {
		return 1
	}
	return 0
}
