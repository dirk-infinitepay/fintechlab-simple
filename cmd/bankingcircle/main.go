// Banking Circle models the real Banking Circle contract as buddy consumes
// it: auth (mTLS + OAuth-shaped bearer), a balance read, webhook
// subscription CRUD, and the outgoing-payment notification lifecycle it
// drives once told a payment exists by the B4B mock. See
// docs/ARCHITECTURE-vendor-corrections.md section 3 and its Addendum
// (sections B/C), which supersede docs/ARCHITECTURE-banking-circle.md's
// lifecycle/auth/HTTP-surface model — that doc's ledger/VIBAN seed shape is
// the only part of it still in effect.
//
// Three listeners run in one process. mTLS applies to an entire
// http.Server, not per-route, so the real-shaped, credentialed API and the
// lab's plain-HTTP surfaces cannot share a listener (Addendum section C).
//   - LISTEN (default :8085, mTLS + bearer): every real-shaped endpoint,
//     plus a few read-only debug endpoints (accounts/payments/reconciliation
//     listings) this lab keeps for harness introspection — not part of the
//     real contract, but still behind the same credentials as everything
//     else on this listener. The old /sim paths answer here too, behind the
//     bearer, until phase 4 of docs/lab-api/plan.md retires them.
//   - INTERNAL_LISTEN (default :8095, plain HTTP, no auth): the B4B bridge
//     (POST /internal/payments), the manual reversal and return test hooks
//     (POST /internal/payments/{id}/reverse, .../return), the "Worldline
//     lump sum landed" trigger (POST /internal/incoming-payments), and a
//     no-auth balance proxy (GET /internal/accounts/{accountId}/balances) —
//     lab-only, same-network trust. b4b, worldline and settlement call it.
//   - The control port (SIM_CONTROL_LISTEN, default LISTEN's port + 1000,
//     :9085; plain HTTP, no vendor auth): every /sim route, the sim control
//     contract (internal/simctl: /sim/info, /sim/logs, /sim/reset), the
//     traps (/sim/traps, internal/trap; the payment.outgoing hook is in
//     traps.go), and the lab's reads of what the vendor and bridge routes
//     answer. The lab API calls only this, and holds no Banking Circle
//     credentials.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/allowlist"
	"github.com/webduvet/fintechlab-simple/internal/bankingcircle"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/logring"
	"github.com/webduvet/fintechlab-simple/internal/money"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
	"github.com/webduvet/fintechlab-simple/internal/trap"
	"github.com/webduvet/fintechlab-simple/internal/waitfor"
)

type app struct {
	ledger   *bankingcircle.Ledger
	engine   *bankingcircle.Engine
	client   *http.Client
	list     *allowlist.List
	notifKey []byte
	tokens   *tokenStore
	subs     *bankingcircle.SubscriptionStore
	dispatch *bankingcircle.Dispatcher
	mail     *bankingcircle.MailBox
	tokenTTL time.Duration
	// bankCreditURL is the beneficiary bank's rail endpoint. Empty
	// disables the hop entirely, so a stack without one is unchanged.
	bankCreditURL string
	// delivery is the *effective* config after BC_TIME_SCALE is applied,
	// kept so it can be reported. A reader of the config file alone sees
	// the file's scale, not the one this process is running.
	delivery bankingcircle.DeliveryConfig
	// The two halves of this vendor's conversation, kept apart because
	// they answer different questions. "Did the payout reach the bank?"
	// is the payments log; "did the bank tell us it had?" is the
	// notifications log, and a run where the first is full and the second
	// empty is a specific, common and otherwise invisible failure.
	payLog   *activity.Log
	notifLog *activity.Log
	// reportLog is every reconciliation read: the two reports and a
	// payment's status. See recordRead.
	reportLog *activity.Log
	// bank is the ledger, engine, dispatcher, mailbox and subscriptions
	// together, for POST /sim/reset.
	bank *bankingcircle.Bank
	// traps is the payment.outgoing hook's trap store (traps.go), served on
	// the control port and reset with it; outcomes is the outcome queue's
	// traps, the shortcut that arms them.
	traps    *trap.Store
	outcomes outcomeQueue
	// listeners are the vendor-facing addresses, for GET /sim/info.
	listeners []string
}

func main() {
	logring.Install()
	addr := env("LISTEN", ":8085")
	internalAddr := env("INTERNAL_LISTEN", ":8095")
	delay := envDuration("PROCESSING_DELAY", 2*time.Second)
	dest := env("WEBHOOK_URL", "https://receiver:8443/webhooks")
	spec := env("WEBHOOK_ALLOWLIST", "receiver,receiver:8443,localhost,127.0.0.1")
	bankCreditURL := strings.TrimSpace(env("BANK_CREDIT_URL", ""))
	list, err := allowlist.Parse(spec)
	if err != nil {
		log.Fatalf("allowlist: %v", err)
	}
	if err := list.Allowed(dest); err != nil {
		log.Fatalf("default WEBHOOK_URL rejected by allowlist: %v", err)
	}
	// The lab clock can put the whole lab on another day. Follow it, so a
	// booking is dated on the day the lab says it is; with LAB_CLOCK_URL
	// unset this bank keeps the wall clock.
	labclock.FollowEnv(context.Background(), "banking-circle")
	// Delivery behaviour -- the retry schedule, batch sizing and the time
	// scale that makes a two-day schedule watchable -- comes from a config
	// file, because the interesting part of it is a table. A missing file
	// means the real, unscaled schedule.
	deliveryCfg, err := bankingcircle.LoadDeliveryConfig(env("BC_DELIVERY_CONFIG", "/config/banking-circle.json"))
	if err != nil {
		log.Printf("banking-circle: %v; falling back to the documented default schedule", err)
	}
	// The one knob worth having outside the file: the real schedule takes
	// two days to reach deactivation, and nobody is going to wait. The
	// file keeps the real table so it stays readable as documentation;
	// this divides every delay in it. The step count and the shape --
	// which is what a client is actually being tested against -- do not
	// change.
	if v := env("BC_TIME_SCALE", ""); v != "" {
		scale, perr := strconv.ParseFloat(v, 64)
		if perr != nil || scale <= 0 {
			log.Printf("banking-circle: BC_TIME_SCALE=%q is not a positive number, keeping %g", v, deliveryCfg.TimeScale)
		} else {
			deliveryCfg.TimeScale = scale
		}
	}
	// Real Banking Circle encrypts webhook notifications with the raw UTF-8
	// bytes of a 32-character key — NOT base64-decoded, confirmed against
	// buddy's own decrypt code even though this looks like a bug. This is a
	// fresh env var (HMAC_SECRET no longer applies: this isn't an HMAC
	// scheme). Default is a fake-obvious 32-char dev key, never a real one.
	notifKey := []byte(env("BC_NOTIFICATION_KEY", "sim-bc-notification-key-32-chars"))
	if len(notifKey) != 32 {
		log.Fatalf("BC_NOTIFICATION_KEY must be exactly 32 characters (raw UTF-8 bytes = AES-256 key), got %d", len(notifKey))
	}
	caFile := env("CA_FILE", "/keys/certs/ca.pem")
	certFile := env("TLS_CERT", "/keys/certs/banking-circle.pem")
	keyFile := env("TLS_KEY", "/keys/certs/banking-circle-key.pem")
	// receiver's and our own certs are signed by this CA; the ca service and
	// this one start concurrently under compose, so wait rather than trust
	// orchestrator ordering (podman-compose does not block on it — see
	// docs/security/ca-and-tls.md).
	if err := waitfor.Files(30*time.Second, caFile, certFile, keyFile); err != nil {
		log.Printf("banking-circle: %v; continuing, mTLS listener and HTTPS to receiver will fail until certs exist", err)
	}
	client, err := httpClient(caFile)
	if err != nil {
		log.Fatalf("tls client: %v", err)
	}

	a := &app{
		ledger:   bankingcircle.NewLedger(),
		client:   client,
		list:     list,
		notifKey: notifKey,
		tokens:   newTokenStore(),
		subs:     bankingcircle.NewSubscriptionStore(func(prefix string) string { return prefix + "_" + shortID() }),
		mail:     &bankingcircle.MailBox{},
		tokenTTL: envDuration("TOKEN_TTL", time.Hour),
		delivery: deliveryCfg,

		listeners: []string{addr, internalAddr},
		traps:     newTraps(delay),

		bankCreditURL: bankCreditURL,
		payLog: activity.New("payments", "Payments received",
			"What arrived on the lab bridge: payouts from B4B, and money landing on the safeguarding accounts."),
		notifLog: activity.New("notifications", "Notifications sent",
			"Each encrypted batch this vendor posted to a subscription's endpoint, and what that endpoint answered. "+
				"A subscription whose delivery is paused shows its notifications queued here instead, until it is resumed."),
		reportLog: activity.New("reports", "Reconciliation reads",
			"Each call to the intraday reconciliation report, the rejection report and a payment's status: when, what was asked, "+
				"and what went back. The platform's reconciliation sweep is what normally makes these; a refused one is amber, with the reason."),
	}
	a.dispatch = bankingcircle.NewDispatcher(deliveryCfg, a.subs, a.mail, log.Printf)
	a.dispatch.Send = a.sendEncrypted
	a.dispatch.OnQueued = a.recordQueued
	a.engine = bankingcircle.NewEngine(a.ledger, delay, a.onTransition)

	// Seed one default subscription, subscribed to every event type this
	// lab's engine can fire, so a demo works without an explicit subscribe
	// call first. A real client creates its own and picks its own events;
	// this is a convenience, and every field on it is one the real API
	// would have required.
	seeded, err := a.subs.Create(bankingcircle.CreateParams{
		Endpoint:                   dest,
		EncryptionKey:              string(notifKey),
		Status:                     bankingcircle.StatusActive,
		Email:                      env("BC_SEED_SUBSCRIPTION_EMAIL", "alerts@fintechlab-simple.local"),
		MaxNotificationsPerMessage: envInt("BC_SEED_BATCH_SIZE", bankingcircle.MinNotificationsPerMessage),
	})
	if err != nil {
		log.Fatalf("seed default subscription: %v", err)
	}
	for _, et := range []bankingcircle.NotificationType{
		bankingcircle.NotificationOutgoingPaymentBooked,
		bankingcircle.NotificationOutgoingPaymentProcessed,
		bankingcircle.NotificationOutgoingPaymentRejected,
		bankingcircle.NotificationMissingFunding,
		bankingcircle.NotificationReversed,
		bankingcircle.NotificationIncomingPaymentProcessed,
		bankingcircle.NotificationIncomingPaymentBooked,
		bankingcircle.NotificationPaymentStatus,
	} {
		// No targetIds: subscription-wide, which is what a seed should be.
		if _, err := a.subs.AddEvent(seeded.ID, string(et), bankingcircle.TargetCompany, nil); err != nil {
			log.Fatalf("seed subscription event %s: %v", et, err)
		}
	}
	log.Printf("banking-circle: seeded subscription %s -> %s (batch=%d, retries=%d, time_scale=%g)",
		seeded.ID, dest, seeded.MaxNotificationsPerMessage,
		deliveryCfg.DeactivateAfterRetries, deliveryCfg.TimeScale)
	// Taken after the seed, before any listener: the subscriptions as
	// they stand now are what a reset of scope all puts back.
	a.bank = bankingcircle.NewBank(a.engine, a.dispatch)

	// The control surface: the lab's /sim hooks on a port of their own,
	// apart from the vendor's login (internal/simctl).
	ctl, err := a.control(simctl.ControlAddr(addr))
	if err != nil {
		log.Fatalf("banking-circle: %v", err)
	}
	ctl.Start()

	go func() {
		log.Printf("banking-circle internal listening on %s (plain, no auth) — B4B bridge only", internalAddr)
		log.Fatal(http.ListenAndServe(internalAddr, logReq(a.internalMux())))
	}()

	// mTLS is optional. Real integrations vary -- some present a client
	// certificate, some do not -- and a simulator that forces it on makes
	// the lab unusable for the ones that do not, while a simulator that
	// cannot do it at all makes it untestable for the ones that do.
	//
	//   off      plain HTTPS, no client certificate asked for
	//   optional a client certificate is requested and verified if given
	//   require  a valid client certificate is mandatory (the old behaviour)
	mode := strings.ToLower(env("BC_MTLS", "require"))
	ln, err := tlsListener(addr, certFile, keyFile, caFile, mode)
	if err != nil {
		log.Fatalf("tls listener: %v", err)
	}
	log.Printf("banking-circle listening on %s mtls=%s dest=%s allowlist=%s delay=%s", addr, mode, dest, spec, delay)
	log.Fatal(http.Serve(ln, logReq(a.mux(ctl.Mux))))
}

// tlsListener builds the credentialed API's TLS listener. mode selects how
// hard a client certificate is enforced: "require" (the default, and what
// the previous implementation always did), "optional", or "off".
//
// "off" still serves HTTPS -- it drops the client-certificate requirement,
// not the transport security. Bearer auth applies in every mode, so
// turning mTLS off never makes the API unauthenticated.
func tlsListener(addr, certFile, keyFile, caFile, mode string) (net.Listener, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("server cert/key: %w", err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	switch mode {
	case "off", "false", "none":
		cfg.ClientAuth = tls.NoClientCert
	case "optional", "verify-if-given":
		cfg.ClientAuth = tls.VerifyClientCertIfGiven
	case "require", "required", "true", "":
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	default:
		return nil, fmt.Errorf("BC_MTLS=%q, want off, optional or require", mode)
	}
	if cfg.ClientAuth != tls.NoClientCert {
		caPEM, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("ca file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("ca file %s had no certs", caFile)
		}
		cfg.ClientCAs = pool
	}
	return tls.Listen("tcp", addr, cfg)
}

// mux is the real-shaped, mTLS + bearer credentialed API (LISTEN). ctl is
// the control surface's mux, whose /sim routes answer here too.
func (a *app) mux(ctl http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, map[string]string{"status": "ok", "service": "banking-circle"})
	})
	mux.HandleFunc("GET /api/v1/authorizations/authorize", a.authorize)
	mux.HandleFunc("GET /api/v1/accounts/{accountId}/balances", a.requireBearer(a.balances))
	mux.HandleFunc("GET /api/v1/notificationselfservice/subscription", a.requireBearer(a.listSubscriptions))
	mux.HandleFunc("POST /api/v1/notificationselfservice/subscription", a.requireBearer(a.createSubscription))
	mux.HandleFunc("GET /api/v1/notificationselfservice/subscription/{id}", a.requireBearer(a.getSubscription))
	mux.HandleFunc("PUT /api/v1/notificationselfservice/subscription/{id}", a.requireBearer(a.updateSubscription))
	mux.HandleFunc("PUT /api/v1/notificationselfservice/subscription/{id}/activate", a.requireBearer(a.activateSubscription))
	mux.HandleFunc("PUT /api/v1/notificationselfservice/subscription/{id}/deactivate", a.requireBearer(a.deactivateSubscription))
	mux.HandleFunc("DELETE /api/v1/notificationselfservice/subscription/{id}", a.requireBearer(a.deleteSubscription))
	mux.HandleFunc("POST /api/v1/notificationselfservice/subscriptionEvent", a.requireBearer(a.createSubscriptionEvent))
	mux.HandleFunc("PUT /api/v1/notificationselfservice/subscriptionEvent/{id}/targets", a.requireBearer(a.replaceSubscriptionEventTargets))
	mux.HandleFunc("DELETE /api/v1/notificationselfservice/subscriptionEvent/{id}", a.requireBearer(a.deleteSubscriptionEvent))
	mux.HandleFunc("POST /api/v1/notificationselfservice/clienttest/{subscriptionId}", a.requireBearer(a.clientTest))
	// The old /sim paths, behind the bearer they always had, until phase 4
	// of docs/lab-api/plan.md retires them. They are the control surface's
	// own routes, so they answer exactly what it answers.
	mux.Handle("/sim/", a.requireBearer(ctl.ServeHTTP))
	// Debug/introspection endpoints kept from the old surface — not part of
	// the real Banking Circle contract, but useful for the harness and kept
	// behind the same credentials as everything else on this listener.
	mux.HandleFunc("GET /accounts", a.requireBearer(a.listAccounts))
	mux.HandleFunc("GET /accounts/{id}", a.requireBearer(a.getAccount))
	mux.HandleFunc("GET /payments", a.requireBearer(a.listPayments))
	mux.HandleFunc("GET /payments/{id}", a.requireBearer(a.getPayment))
	mux.HandleFunc("GET /reconciliation", a.requireBearer(a.reconcile))

	// The Connect reports surface. These are what a reconciliation sweep
	// reads, so they carry the vendor's own paths and body shapes rather
	// than this mock's convenience ones above.
	mux.HandleFunc("GET /api/v1/reports/intraday-reconciliation-paged-report",
		a.requireBearer(a.intradayReconciliationReport))
	mux.HandleFunc("GET /api/v1/reports/rejection-report",
		a.requireBearer(a.rejectionReport))
	mux.HandleFunc("GET /api/v1/payments/singles/{paymentId}/status",
		a.requireBearer(a.paymentStatus))
	return mux
}

func (a *app) deliveryConfig(w http.ResponseWriter, r *http.Request) {
	httputilx.WriteJSON(w, 200, a.delivery)
}

// control builds the control surface (internal/simctl) for a listener of
// its own, at SIM_CONTROL_LISTEN or else defaultAddr: the sim control
// contract, the trap routes, GET /health, and every route in simRoutes.
//
// It carries none of Banking Circle's authentication. mTLS and the
// Basic→Bearer exchange are for the platform calling the bank's API;
// reversing a payout, reading a log or resetting is the lab steering its
// own simulation, and the lab API that does it holds no Banking Circle
// credentials. The port is trusted by where it is: compose does not
// publish it, and SIM_CONTROL_ALLOW narrows it further.
func (a *app) control(defaultAddr string) (*simctl.Control, error) {
	ctl, err := simctl.NewControl(simctl.Config{
		Service: "banking-circle", Kind: "vendor", Simulates: "Banking Circle Connect",
		Listeners: a.listeners,
		Reset:     a.reset,
		Traps:     a.traps,
	}, defaultAddr)
	if err != nil {
		return nil, err
	}
	for pattern, h := range a.simRoutes() {
		ctl.Mux.HandleFunc(pattern, h)
	}
	return ctl, nil
}

// simRoutes are the lab's own routes on the control surface, by pattern.
// None is Banking Circle's. Where one answers what a vendor or bridge
// route answers, it is that route's handler, so the two cannot drift.
func (a *app) simRoutes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		// Everything this vendor has done, as the console's activity view
		// and the lab API's flow read it, and what its traps did.
		"GET /sim/activity": activity.Handler(a.payLog, a.notifLog, a.reportLog, a.trapLog()),
		// The warning and deactivation emails the retry schedule emits, so
		// a failing subscription is observable rather than just eventually
		// silent.
		"GET /sim/emails": a.listEmails,
		// The delivery table this process is actually running, which is
		// the file's plus whatever BC_TIME_SCALE overrode. Reading the file
		// alone reports a schedule nobody is on.
		"GET /sim/delivery-config": a.deliveryConfig,

		// Subscriptions: the vendor's list, its clienttest and its
		// activate/deactivate (here without If-Match), and the lab's tap on
		// the notification pipe.
		"GET /sim/subscriptions":                                a.simListSubscriptions,
		"POST /sim/subscription/{subscriptionId}/test":          a.clientTest,
		"POST /sim/subscription/{subscriptionId}/activate":      a.simActivateSubscription,
		"POST /sim/subscription/{subscriptionId}/deactivate":    a.simDeactivateSubscription,
		"POST /sim/subscription/{subscriptionId}/notifications": a.enqueueSyntheticNotifications,
		"GET /sim/subscription/{subscriptionId}/pending":        a.pendingNotifications,
		"POST /sim/subscription/{subscriptionId}/pause":         a.pauseNotifications,
		"POST /sim/subscription/{subscriptionId}/resume":        a.resumeNotifications,

		// The ledger and the payments, as GET /accounts and GET /payments
		// on LISTEN and the bridge's balance read answer them.
		"GET /sim/accounts":                      a.listAccounts,
		"GET /sim/accounts/{accountId}/balances": a.internalAccountBalances,
		"GET /sim/payments":                      a.listPayments,
		"POST /sim/incoming-payments":            a.incomingPayments(),
		"GET /sim/payments/outcomes":             a.forcedOutcomes,
		"POST /sim/payments/outcomes":            a.forceOutcomes,
		"POST /sim/payments/{id}/return":         a.returnInternalPayment,
		"POST /sim/payments/{id}/reverse":        a.reverseInternalPayment,
	}
}

// reset is POST /sim/reset (docs/lab-api/sim-control.md §6). flow clears
// what a run makes -- payments, balances, notifications not yet delivered,
// pauses, emails, activity -- and keeps what a platform set up once: its
// subscriptions and the accounts its payouts opened. all also puts the bank
// back as it was on first start: the seeded subscription only, the
// safeguarding accounts only, references from the first. The traps, the
// outcome queue's among them, are reset by simctl before this runs.
//
// Issued bearer tokens survive both. A lab reset must never log the
// platform's Banking Circle client out, and nothing the lab does needs one.
// Work in flight is dropped, not raced (bankingcircle.Bank.Reset). Never
// refused: nothing here is unsafe to cut short.
func (a *app) reset(_ context.Context, req simctl.ResetRequest) (simctl.ResetResult, error) {
	all := req.Scope == simctl.ScopeAll
	cleared := a.bank.Reset(all, func(cleared map[string]int) {
		cleared["activity"] = int(a.payLog.Clear() + a.notifLog.Clear() + a.reportLog.Clear())
		a.outcomes.clear()
	})
	kept := []string{"subscriptions", "accounts", "tokens"}
	if all {
		kept = []string{"tokens"}
	}
	return simctl.ResetResult{Cleared: cleared, Kept: kept}, nil
}

// internalMux is the lab-only, unauthenticated B4B<->Banking-Circle bridge
// (INTERNAL_LISTEN). Nothing else is served here.
func (a *app) internalMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/payments", a.payLog.Watch("payment.create", summarizeBridgePayment, a.createInternalPayment))
	mux.HandleFunc("POST /internal/payments/{id}/reverse", a.reverseInternalPayment)
	mux.HandleFunc("POST /internal/payments/{id}/return", a.returnInternalPayment)
	mux.HandleFunc("GET /internal/payments/outcomes", a.forcedOutcomes)
	mux.HandleFunc("POST /internal/payments/outcomes", a.forceOutcomes)
	mux.HandleFunc("POST /internal/incoming-payments", a.incomingPayments())
	mux.HandleFunc("GET /internal/accounts/{accountId}/balances", a.internalAccountBalances)
	return mux
}

// trapLog is the traps' activity log, nil without a trap store.
func (a *app) trapLog() *activity.Log {
	if a.traps == nil {
		return nil
	}
	return a.traps.Log()
}

// incomingPayments is money landing on a safeguarding account, logged as
// payment.incoming: the bridge's POST /internal/incoming-payments and the
// control surface's POST /sim/incoming-payments.
func (a *app) incomingPayments() http.HandlerFunc {
	return a.payLog.Watch("payment.incoming", summarizeIncoming, a.createIncomingPayment)
}

// --- auth ---------------------------------------------------------------

type tokenStore struct {
	mu     sync.Mutex
	tokens map[string]time.Time
}

func newTokenStore() *tokenStore {
	return &tokenStore{tokens: map[string]time.Time{}}
}

func (s *tokenStore) issue(ttl time.Duration) string {
	tok := "bcat_" + randomHex(24)
	s.mu.Lock()
	s.tokens[tok] = time.Now().Add(ttl)
	s.mu.Unlock()
	return tok
}

func (s *tokenStore) valid(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.tokens[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.tokens, tok)
		return false
	}
	return true
}

// authorize issues a bearer token for any non-empty Basic credential — this
// lab has no real credential store, matching its existing "obviously fake,
// deliberately simple" auth posture elsewhere (e.g. bank's PINs).
func (a *app) authorize(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	if !ok || user == "" || pass == "" {
		w.Header().Set("WWW-Authenticate", `Basic realm="banking-circle"`)
		httputilx.Error(w, 401, "basic auth required")
		return
	}
	token := a.tokens.issue(a.tokenTTL)
	httputilx.WriteJSON(w, 200, map[string]any{
		"access_token": token,
		"expires_in":   int(a.tokenTTL.Seconds()),
		"token_type":   "bearer",
	})
}

// requireBearer guards every real-shaped endpoint except authorize itself.
func (a *app) requireBearer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(authz, prefix) || !a.tokens.valid(strings.TrimPrefix(authz, prefix)) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="banking-circle"`)
			httputilx.Error(w, 401, "bearer token required")
			return
		}
		next(w, r)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// --- balances -------------------------------------------------------------

type balanceEntry struct {
	Type                     string `json:"type"`
	Currency                 string `json:"currency"`
	BeginOfDayAmount         string `json:"beginOfDayAmount"`
	FinancialDate            string `json:"financialDate"`
	IntraDayAmount           string `json:"intraDayAmount"`
	LastTransactionTimestamp string `json:"lastTransactionTimestamp"`
}

type pageInfo struct {
	CurrentPage int `json:"currentPage"`
	PageSize    int `json:"pageSize"`
	RowCount    int `json:"rowCount,omitempty"`
}

// balanceEntries builds the balance-entry list shared by both the
// mTLS-credentialed balances endpoint and the no-auth internal balance
// proxy below — identical shape for both, since they're reading the exact
// same underlying account (docs/ARCHITECTURE-phase3-corrections.md section
// 1).
func (a *app) balanceEntries(acct *bankingcircle.Account) []balanceEntry {
	now := labclock.Now()
	day := bankingcircle.FinancialDay(now)
	open, intraday := acct.DayAmounts(day)
	return []balanceEntry{{
		Type:                     "CurrentBalance",
		Currency:                 acct.Currency,
		BeginOfDayAmount:         money.Format(open),
		FinancialDate:            day,
		IntraDayAmount:           money.Format(intraday),
		LastTransactionTimestamp: now.Format(time.RFC3339),
	}}
}

// balances implements GET /api/v1/accounts/{accountId}/balances.
// beginOfDayAmount is the balance the account opened today with and
// intraDayAmount what has moved since; their sum is the balance, which is
// how a platform reads them. (Both used to carry the whole balance, so a
// platform summing them saw twice the money there was.)
func (a *app) balances(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("accountId")
	// A malformed id is a 400, not a 404. The distinction matters to the
	// caller: "no such account" sends someone looking for missing data,
	// "that is not an account id" sends them to their own configuration,
	// which is where the problem actually is.
	if !bankingcircle.ValidAccountID(id) {
		httputilx.Error(w, 400, "accountId must be a UUID")
		return
	}
	acc, err := a.ledger.Get(id)
	if err != nil {
		httputilx.Error(w, 404, "account not found")
		return
	}
	page := queryInt(r, "pageNumber", 1)
	size := queryInt(r, "pageSize", 20)
	entries := a.balanceEntries(acc)
	httputilx.WriteJSON(w, 200, map[string]any{
		"result":   entries,
		"pageInfo": pageInfo{CurrentPage: page, PageSize: size, RowCount: len(entries)},
	})
}

func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func (a *app) listAccounts(w http.ResponseWriter, r *http.Request) {
	httputilx.WriteJSON(w, 200, map[string]any{"accounts": a.ledger.List()})
}

func (a *app) getAccount(w http.ResponseWriter, r *http.Request) {
	acc, err := a.ledger.Get(r.PathValue("id"))
	if err != nil {
		httputilx.Error(w, 404, "account not found")
		return
	}
	httputilx.WriteJSON(w, 200, acc)
}

func (a *app) listPayments(w http.ResponseWriter, r *http.Request) {
	httputilx.WriteJSON(w, 200, map[string]any{"payments": a.engine.List()})
}

func (a *app) getPayment(w http.ResponseWriter, r *http.Request) {
	p, err := a.engine.Get(r.PathValue("id"))
	if err != nil {
		httputilx.Error(w, 404, "payment not found")
		return
	}
	httputilx.WriteJSON(w, 200, p)
}

func (a *app) reconcile(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	if date == "" {
		httputilx.Error(w, 400, "date query parameter required (YYYY-MM-DD)")
		return
	}
	payments := bankingcircle.Reconcile(a.engine.List(), date, r.URL.Query().Get("account_id"))
	httputilx.WriteJSON(w, 200, map[string]any{"payments": payments})
}

// intradayReconciliationReport implements
// GET /api/v1/reports/intraday-reconciliation-paged-report.
//
// A row's existence is the booking signal — the report carries no status —
// so this answers from the same payment records the notifications are built
// from. If the two ever disagreed, a client would be reconciling against a
// story rather than against the money.
func (a *app) intradayReconciliationReport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query, problems := reconciliationQuery(q)
	detail := map[string]string{
		"transaction_date": q.Get("FromTransactionDate") + ".." + q.Get("ToTransactionDate"),
		"created_at":       q.Get("FromCreatedAt") + ".." + q.Get("ToCreatedAt"),
		"page":             q.Get("PageNumber") + " of size " + q.Get("PageSize"),
		"properties":       firstNonEmptyString(q.Get("PropertiesIncluded"), "default"),
	}
	if len(problems) > 0 {
		a.recordRead(r, "report.intraday", "intraday report refused — "+problemNames(problems), detail, activity.StatusWarn)
		writeValidationProblem(w, problems)
		return
	}
	query.IBAN = a.accountIBAN
	rows := bankingcircle.IntradayReconciliation(a.engine.List(), query)
	detail["rows"] = strconv.Itoa(len(rows))
	a.recordRead(r, "report.intraday", fmt.Sprintf("intraday report %s, page %d → %d row(s)",
		dateRange(query.FromTransactionDate, query.ToTransactionDate), query.PageNumber, len(rows)), detail, activity.StatusOK)
	httputilx.WriteJSON(w, 200, map[string]any{
		"reconciliations": propertySelection(q).Project(rows),
	})
}

// reconciliationQuery reads the report's filter the way the reference
// defines it: FromTransactionDate, ToTransactionDate, FromCreatedAt,
// ToCreatedAt, PageNumber and PageSize are all required, the four dates are
// date-times (a bare YYYY-MM-DD included), PageNumber is 1 to n and PageSize
// any positive value. Anything else is a 400 from the real bank, so it is
// one here: a lab that fills in a default for a missing parameter passes a
// client the bank would refuse.
func reconciliationQuery(q url.Values) (bankingcircle.ReconciliationQuery, map[string][]string) {
	problems := map[string][]string{}
	for _, name := range []string{"FromTransactionDate", "ToTransactionDate", "FromCreatedAt", "ToCreatedAt"} {
		if v := q.Get(name); v != "" && !isReportDateTime(v) {
			problems[name] = append(problems[name], invalidParameter(name, v))
		}
	}
	positive := func(name string) int {
		v := q.Get(name)
		if v == "" {
			return 0
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			problems[name] = append(problems[name], invalidParameter(name, v))
		}
		return n
	}
	query := bankingcircle.ReconciliationQuery{
		FromTransactionDate: q.Get("FromTransactionDate"),
		ToTransactionDate:   q.Get("ToTransactionDate"),
		FromCreatedAt:       q.Get("FromCreatedAt"),
		ToCreatedAt:         q.Get("ToCreatedAt"),
		PageNumber:          positive("PageNumber"),
		PageSize:            positive("PageSize"),
	}
	for _, name := range []string{"FromTransactionDate", "ToTransactionDate", "FromCreatedAt", "ToCreatedAt", "PageNumber", "PageSize"} {
		if q.Get(name) == "" {
			problems[name] = append(problems[name], missingParameter(name))
		}
	}
	if ids := q.Get("AccountId"); ids != "" {
		query.AccountIDs = strings.Split(ids, ",")
	}
	return query, problems
}

// accountIBAN is how both reports name an account: its IBAN from the
// ledger, empty (reported as null) for an account the ledger does not hold.
func (a *app) accountIBAN(accountID string) string {
	acc, err := a.ledger.Get(accountID)
	if err != nil {
		return ""
	}
	return acc.VIBAN
}

// reportDateTimeLayouts are the forms the reference shows for the report's
// date parameters: YYYY-MM-DD, and date-times with or without a zone and
// with any number of fractional digits (its example is
// 2025-11-01T12:34:00.0000000). Go accepts a fraction on parse even when a
// layout has none.
var reportDateTimeLayouts = []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"}

func isReportDateTime(v string) bool {
	for _, layout := range reportDateTimeLayouts {
		if _, err := time.Parse(layout, v); err == nil {
			return true
		}
	}
	return false
}

// propertySelection reads PropertiesIncluded / PropertiesExcluded, falling
// back to their deprecated array forms (IncludeProperties /
// ExcludeProperties, repeated query keys) only when the new one is absent,
// as the reference says: "When both are used all IncludeProperties are
// ignored."
func propertySelection(q url.Values) bankingcircle.PropertySelection {
	var sel bankingcircle.PropertySelection
	if v, ok := q["PropertiesIncluded"]; ok {
		sel.Included = bankingcircle.SplitProperties(strings.Join(v, ","))
	} else {
		sel.Included = bankingcircle.SplitProperties(strings.Join(q["IncludeProperties"], ","))
	}
	if v, ok := q["PropertiesExcluded"]; ok {
		sel.Excluded, sel.ExcludedSent = bankingcircle.SplitProperties(strings.Join(v, ",")), true
	} else if v, ok := q["ExcludeProperties"]; ok {
		sel.Excluded, sel.ExcludedSent = bankingcircle.SplitProperties(strings.Join(v, ",")), true
	}
	return sel
}

// writeValidationProblem answers 400 with the body Banking Circle's
// reports send, copied from the rejection report reference's own example:
//
//	{"type": "https://tools.ietf.org/html/rfc7231#section-6.5.1",
//	 "title": "One or more validation errors occurred.", "status": 400,
//	 "extensions": {"traceId": "..."},
//	 "errors": {"transactionDate": ["A value for the 'TransactionDate'
//	            parameter or property was not provided."]}}
//
// problems is keyed by query parameter name; the errors object keys it
// with a lower-case first letter, as the example does.
func writeValidationProblem(w http.ResponseWriter, problems map[string][]string) {
	errs := make(map[string][]string, len(problems))
	for name, messages := range problems {
		errs[strings.ToLower(name[:1])+name[1:]] = messages
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(400)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":       "https://tools.ietf.org/html/rfc7231#section-6.5.1",
		"title":      "One or more validation errors occurred.",
		"status":     400,
		"extensions": map[string]string{"traceId": traceID()},
		"errors":     errs,
	})
}

// missingParameter is the reference example's wording for a required
// parameter that was not sent.
func missingParameter(name string) string {
	return fmt.Sprintf("A value for the '%s' parameter or property was not provided.", name)
}

// invalidParameter is ASP.NET Core's default wording for a value that does
// not parse, the framework the example's shape comes from; the reference
// has no example of this one, so a client must not match on it.
func invalidParameter(name, value string) string {
	return fmt.Sprintf("The value '%s' is not valid for %s.", value, name)
}

// traceID is a random id in the example's 8-4-4-4-12 shape.
func traceID() string {
	h := randomHex(16)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// rejectionReport implements GET /api/v1/reports/rejection-report: the
// payments that could not be processed on a transaction date, with a
// status and reason. TransactionDate is required (400 without it, as in the
// reference's own error example); the booleans are optional, and an
// unparseable one is a 400 too. IncludeReversals is accepted and has
// nothing to act on: it covers direct-debit reversals only.
func (a *app) rejectionReport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	problems := map[string][]string{}
	date := q.Get("TransactionDate")
	switch {
	case date == "":
		problems["TransactionDate"] = []string{missingParameter("TransactionDate")}
	case !isReportDateTime(date):
		problems["TransactionDate"] = []string{invalidParameter("TransactionDate", date)}
	}
	flag := func(name string, def bool) bool {
		v := q.Get(name)
		if v == "" {
			return def
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			problems[name] = []string{invalidParameter(name, v)}
		}
		return b
	}
	query := bankingcircle.RejectionQuery{
		TransactionDate:     datePrefix(date),
		IncludeReceived:     flag("IncludeReceived", true),
		IncludeMissingFunds: flag("IncludeMissingFunds", true),
		ExcludeBooked:       flag("ExcludeBooked", false),
		ReportDate:          labclock.Now().Format("2006-01-02"),
		IBAN:                a.accountIBAN,
	}
	flag("IncludeReversals", false)
	if len(problems) > 0 {
		a.recordRead(r, "report.rejection", "rejection report refused — "+problemNames(problems),
			map[string]string{"transaction_date": date}, activity.StatusWarn)
		writeValidationProblem(w, problems)
		return
	}
	rows := bankingcircle.Rejections(a.engine.List(), query)
	a.recordRead(r, "report.rejection", fmt.Sprintf("rejection report %s → %d row(s)", query.TransactionDate, len(rows)),
		map[string]string{
			"transaction_date": query.TransactionDate,
			"rows":             strconv.Itoa(len(rows)),
			"filters": fmt.Sprintf("received=%t missing_funds=%t exclude_booked=%t",
				query.IncludeReceived, query.IncludeMissingFunds, query.ExcludeBooked),
		}, activity.StatusOK)
	httputilx.WriteJSON(w, 200, map[string]any{"rejections": rows})
}

func datePrefix(v string) string {
	if len(v) >= 10 {
		return v[:10]
	}
	return v
}

// paymentStatus implements GET /api/v1/payments/singles/{payment-id}/status:
// the vendor's one-payment status read, `{"status": "..."}`, 404 for an id
// it has never seen.
func (a *app) paymentStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("paymentId")
	p, err := a.engine.Get(id)
	if err != nil {
		a.recordRead(r, "payment.status", "status of "+id+" → not found (404)", map[string]string{"payment_id": id}, activity.StatusWarn)
		httputilx.Error(w, 404, "payment not found")
		return
	}
	status := bankingcircle.PaymentStatus(p.State)
	a.recordRead(r, "payment.status", "status of "+id+" → "+status, map[string]string{"payment_id": id, "status": status}, activity.StatusOK)
	httputilx.WriteJSON(w, 200, map[string]string{"status": status})
}

// --- the B4B<->Banking-Circle bridge (INTERNAL_LISTEN, no auth) --------

// sgaAccountID maps a currency to the safeguarding account (SGA) this mock
// debits outgoing payouts from and credits incoming Worldline lump sums
// into — resolved by currency, not a fixed constant, now that there is one
// SGA per currency instead of a single pre-funded funding account
// (docs/ARCHITECTURE-phase3-corrections.md section 1).
func sgaAccountID(currency string) (string, error) {
	switch currency {
	case "EUR":
		return bankingcircle.SGAAccountEUR, nil
	case "GBP":
		return bankingcircle.SGAAccountGBP, nil
	default:
		return "", fmt.Errorf("no safeguarding account configured for currency %q", currency)
	}
}

type internalPaymentReq struct {
	PaymentID string `json:"paymentId"`
	AccountID string `json:"accountId"`
	// IBAN is the beneficiary's account at their own bank. Optional, and
	// carried rather than derived: the settlement bank knows where it is
	// sending money, and without it the receiving bank has nothing to open
	// an account against.
	IBAN   string `json:"iban"`
	Holder string `json:"holder"`
	// AccountNumber and FinancialInstitution are the beneficiary's account
	// when it is not an IBAN (account number and sort code).
	AccountNumber        string `json:"accountNumber"`
	FinancialInstitution string `json:"financialInstitution"`
	Amount               string `json:"amount"`
	Currency             string `json:"currency"`
	ExternalRef          string `json:"externalRef"`
}

// createInternalPayment implements POST /internal/payments — the seam the
// B4B mock calls, once its own payment reaches B4BTMApproved, carrying the
// real BC paymentId it's about to reference in its own webhook. paymentId
// is used as-is (generated by B4B, not by us); accountId is the creditor,
// auto-vivified via Ledger.GetOrCreate if this is the first payment to it
// (docs/ARCHITECTURE-phase3-corrections.md section 1 — B4B derives a
// distinct creditor account per beneficiary, not a single fixed known one,
// so this can no longer 404); the debtor is the safeguarding account
// matching the payment's currency; externalRef lands on the existing
// Payment.SettlementID field verbatim (Addendum section B). The payout is
// booked at once and offered to the payment.outgoing traps (book).
func (a *app) createInternalPayment(w http.ResponseWriter, r *http.Request) {
	var req internalPaymentReq
	if err := httputilx.ReadJSON(r, &req); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	if req.PaymentID == "" {
		httputilx.Error(w, 400, "paymentId required")
		return
	}
	if req.AccountID == "" {
		httputilx.Error(w, 400, "accountId required")
		return
	}
	cents, err := money.Parse(req.Amount)
	if err != nil || cents <= 0 {
		httputilx.Error(w, 400, "amount must be a positive decimal")
		return
	}
	ccy := req.Currency
	if ccy == "" {
		ccy = "EUR"
	}
	from, err := sgaAccountID(ccy)
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	// A malformed creditor account id is the caller's mistake and is
	// answered as one. Auto-vivifying it would open an account that looks
	// exactly like a real one, take the money, and balance -- and the
	// payout would be "successful" to an account no reconciliation could
	// ever match.
	to, err := a.ledger.GetOrCreate(req.AccountID, ccy)
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	now := labclock.Now().Format(time.RFC3339)
	p := &bankingcircle.Payment{
		ID:            req.PaymentID,
		SettlementID:  req.ExternalRef,
		FromAccountID: from,
		ToAccountID:   to.ID,
		ToIBAN:        req.IBAN,
		ToHolder:      req.Holder,

		ToAccountNumber:        req.AccountNumber,
		ToFinancialInstitution: req.FinancialInstitution,
		Amount:                 money.Format(cents),
		Currency:               ccy,
		CreatedAt:              now,
		UpdatedAt:              now,
	}
	// Answered as booked: a trap's first step may already have landed.
	httputilx.WriteJSON(w, 202, a.book(p))
}

type incomingPaymentReq struct {
	Currency  string `json:"currency"`
	Amount    string `json:"amount"`
	Reference string `json:"reference"`
}

// createIncomingPayment implements POST /internal/incoming-payments — the
// harness's "Worldline lump sum landed" trigger
// (docs/ARCHITECTURE-phase3-corrections.md section 1), same lab-only trust
// boundary as the B4B bridge above. Real Banking Circle has no equivalent
// caller-facing endpoint; this is the harness's stand-in for the acquirer
// actually wiring funds into the SGA.
func (a *app) createIncomingPayment(w http.ResponseWriter, r *http.Request) {
	var req incomingPaymentReq
	if err := httputilx.ReadJSON(r, &req); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	to, err := sgaAccountID(req.Currency)
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	cents, err := money.Parse(req.Amount)
	if err != nil || cents <= 0 {
		httputilx.Error(w, 400, "amount must be a positive decimal")
		return
	}
	p, err := a.engine.CreditIncoming(to, req.Currency, money.Format(cents), req.Reference)
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	httputilx.WriteJSON(w, 202, p)
}

// internalAccountBalances implements GET
// /internal/accounts/{accountId}/balances, mirroring buddy's own
// InternalAccountBalanceController: lets other Infinite services/Lambdas
// check SGA sufficiency without holding Banking Circle's mTLS+bearer
// credentials themselves (docs/ARCHITECTURE-phase3-corrections.md section
// 1). Read-only: unlike createInternalPayment, an unknown account id is a
// real 404, not an auto-vivify trigger.
func (a *app) internalAccountBalances(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("accountId")
	// Same rule as the credentialed route. This one mirrors buddy's
	// InternalAccountBalanceController, which validates the path segment as
	// a UUID before it calls anything -- so a lab that accepted a
	// non-UUID here would answer requests the real chain rejects two hops
	// earlier, and a client would only discover that in production.
	if !bankingcircle.ValidAccountID(id) {
		httputilx.Error(w, 400, "accountId must be a UUID")
		return
	}
	acc, err := a.ledger.Get(id)
	if err != nil {
		httputilx.Error(w, 404, "account not found")
		return
	}
	httputilx.WriteJSON(w, 200, map[string]any{
		"accountId": acc.ID,
		"balances":  a.balanceEntries(acc),
	})
}

type reverseReq struct {
	Reason string `json:"reason"`
}

// reverseInternalPayment implements POST /internal/payments/{id}/reverse —
// the manual test hook replacing the old POST /payments/{id}/return, same
// semantics: only valid from an OutgoingPaymentProcessed payment.
func (a *app) reverseInternalPayment(w http.ResponseWriter, r *http.Request) {
	var req reverseReq
	if err := httputilx.ReadJSON(r, &req); err != nil && err != io.EOF {
		httputilx.Error(w, 400, err.Error())
		return
	}
	p, err := a.engine.Reverse(r.PathValue("id"), req.Reason)
	if err != nil {
		switch {
		case errors.Is(err, bankingcircle.ErrPaymentNotFound):
			httputilx.Error(w, 404, "payment not found")
		default:
			httputilx.Error(w, 409, err.Error())
		}
		return
	}
	httputilx.WriteJSON(w, 200, p)
}

type returnReq struct {
	// ReasonCode is the return reason (e.g. AC04, see Banking Circle's
	// "Returned payments codes"); ReasonDescription its text.
	ReasonCode        string `json:"reasonCode"`
	ReasonDescription string `json:"reasonDescription"`
}

// returnInternalPayment implements POST /internal/payments/{id}/return —
// the beneficiary's bank sending a processed payout back. Lab-only test
// hook, like reverse above: only valid from an OutgoingPaymentProcessed
// payment that has not already come back. The payout keeps its state; the
// answer is the new incoming return payment (`return: true`), which is
// booked and processed like any other incoming payment.
func (a *app) returnInternalPayment(w http.ResponseWriter, r *http.Request) {
	var req returnReq
	if err := httputilx.ReadJSON(r, &req); err != nil && err != io.EOF {
		httputilx.Error(w, 400, err.Error())
		return
	}
	p, err := a.engine.Return(r.PathValue("id"), req.ReasonCode, req.ReasonDescription)
	if err != nil {
		switch {
		case errors.Is(err, bankingcircle.ErrPaymentNotFound):
			httputilx.Error(w, 404, "payment not found")
		default:
			httputilx.Error(w, 409, err.Error())
		}
		return
	}
	httputilx.WriteJSON(w, 200, p)
}

// --- notification delivery ---------------------------------------------

// notification and notificationBatch are Banking Circle's real webhook
// payload shape verbatim (docs/ARCHITECTURE-vendor-corrections.md section 3).
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

func envDuration(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func logReq(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
