// B4B Payments is the mock for the vendor Infinite pays merchants through:
// buddy's real accounts-settlement -> B4B -> Banking Circle chain, per
// docs/ARCHITECTURE-vendor-corrections.md section 4 and its Addendum
// sections A/D/F, plus the company-boarding half of the Oversight API per
// docs/ARCHITECTURE-b4b-oversight.md.
//
// It verifies inbound RS512-signed bearer tokens, boards companies (people,
// addresses, extended profile, documents, vIBANs), registers beneficiaries,
// owns the B4BAccepted -> ... -> B4BTMApproved|B4BFailed lifecycle, fires
// plain JSON callbacks, and bridges an approved payment into the Banking
// Circle mock. Everything it is told is written to one state file, so a
// merchant boarded before a restart is still boarded after one.
//
// Two listeners. LISTEN (:8086) is the vendor's: the Oversight API, behind
// the RS512 bearer JWT. Its port + 1000 (:9086, SIM_CONTROL_LISTEN) is the
// control port the lab steers the simulation through: every /sim/* route,
// with no vendor auth (internal/simctl). The old /sim paths still answer on
// LISTEN, behind the JWT as they were, until phase 4 of docs/lab-api/plan.md
// retires them.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/allowlist"
	"github.com/webduvet/fintechlab-simple/internal/b4b"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/logring"
	"github.com/webduvet/fintechlab-simple/internal/retry"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
)

type app struct {
	engine        *b4b.Engine
	beneficiaries *b4b.BeneficiaryStore
	dir           *b4b.Directory
	pubKey        *rsa.PublicKey
	kid           string
	client        *http.Client
	list          *allowlist.List
	backs         []time.Duration
	maxTry        int
	bcURL         string
	forceFails    map[string]bool
	statePath     string
	// persistMu makes each write of the state file one snapshot-and-save,
	// so writes land in the order their snapshots were taken: a stale one
	// can never overwrite a reset's.
	persistMu sync.Mutex
	// callbackURL is the client-account webhook endpoint: where a callback
	// goes when the entity it is about carries no callback_url of its own.
	// The real API has exactly this -- one endpoint agreed per client, not
	// a per-record field -- so without it, boarding callbacks have nowhere
	// to go and a client waits forever for a screening result that was
	// computed and then dropped.
	callbackURL string
	// enforceCompany turns on the beneficiary-belongs-to-the-paying-company
	// check. Off by default; see b4b.Beneficiary.CheckCompany for why it is
	// a switch and not a rule.
	enforceCompany bool
	// The two conversations worth watching from the console: what the
	// platform asked of us, and what we told it afterwards. They are
	// separate logs because they fail separately -- a payout can be
	// accepted perfectly and every callback about it still be refused at
	// the client's door, which is exactly the failure that looks like
	// nothing happening at all.
	payLog *activity.Log
	cbLog  *activity.Log
}

func main() {
	logring.Install()
	addr := env("LISTEN", ":8086")
	labclock.FollowEnv(context.Background(), "b4b")
	keysDir := env("B4B_JWT_KEYS_DIR", "/keys/b4b-keys")
	pubKeyPath := env("B4B_JWT_PUBLIC_KEY_PATH", "")
	kid := env("B4B_JWT_KEY_ID", "b4b-mock-1")
	delay := envDuration("B4B_PROCESSING_DELAY", 300*time.Millisecond)
	spec := env("WEBHOOK_ALLOWLIST", "settlement,settlement:8083,localhost,127.0.0.1")
	bcURL := env("BANKING_CIRCLE_URL", "http://banking-circle:8095")
	forceFailSpec := env("B4B_FORCE_FAILURE_BENEFICIARY_IDS", "")
	statePath := env("B4B_STATE_PATH", "/b4b-data/state.json")
	callbackURL := env("B4B_CALLBACK_URL", "")

	list, err := allowlist.Parse(spec)
	if err != nil {
		log.Fatalf("allowlist: %v", err)
	}
	backs, err := retry.ParseBackoffs(env("RETRY_BACKOFF", "1s,2s,4s"))
	if err != nil {
		log.Fatalf("backoff: %v", err)
	}
	if callbackURL != "" {
		if err := b4b.ValidateCallbackURL(list, callbackURL); err != nil {
			log.Fatalf("B4B_CALLBACK_URL: %v", err)
		}
	}

	pub, err := loadVerificationKey(keysDir, pubKeyPath, kid)
	if err != nil {
		log.Fatalf("b4b jwt: %v", err)
	}

	a := &app{
		beneficiaries:  b4b.NewBeneficiaryStore(func() string { return "ben_" + shortID() }),
		dir:            b4b.NewDirectory(nil),
		pubKey:         pub,
		kid:            kid,
		client:         &http.Client{Timeout: 8 * time.Second},
		list:           list,
		backs:          backs,
		maxTry:         envInt("MAX_ATTEMPTS", 4),
		bcURL:          bcURL,
		forceFails:     parseForceFailSet(forceFailSpec),
		statePath:      statePath,
		callbackURL:    callbackURL,
		enforceCompany: envBool("B4B_ENFORCE_BENEFICIARY_COMPANY", false),
		payLog: activity.New("payments", "Payouts received",
			"Every call the platform made to the payments API, and what this vendor answered."),
		cbLog: activity.New("callbacks", "Callbacks sent",
			"Each lifecycle callback this vendor posted back, and whether the client took it."),
	}
	a.engine = b4b.NewEngine(delay, a.onTransition)

	// Restore before anything can serve: a company that existed a second
	// before the restart has to exist a second after it, and a payment
	// caught mid-lifecycle resumes rather than hanging (b4b.Engine.Restore).
	state, err := b4b.LoadState(statePath)
	if err != nil {
		log.Fatalf("b4b state: %v", err)
	}
	a.dir.Restore(state)
	a.beneficiaries.Restore(state)
	a.engine.Restore(state)
	log.Printf("b4b: state %s restored: %d companies, %d people, %d beneficiaries, %d payments",
		statePathLabel(statePath), len(state.Companies), len(state.People), len(state.Beneficiaries), len(state.Payments))

	// Persist after every mutation. Whole-file writes at lab volumes; see
	// internal/b4b/state.go for why this is a file and not a database.
	a.dir.OnChange = a.persist
	a.beneficiaries.OnChange = a.persist

	ctl, err := a.control(addr)
	if err != nil {
		log.Fatalf("b4b: %v", err)
	}
	ctl.Start()

	log.Printf("b4b listening on %s kid=%s allowlist=%s delay=%s banking_circle_url=%s state=%s callback_url=%q enforce_beneficiary_company=%t",
		addr, kid, spec, delay, bcURL, statePathLabel(statePath), callbackURL, a.enforceCompany)
	log.Fatal(http.ListenAndServe(addr, logReq(a.routes(ctl.Mux))))
}

// control is b4b's control surface (internal/simctl): every /sim/* route,
// on a port of its own and with no bearer JWT. The JWT is for a platform
// calling B4B; the lab steering its own simulation is not a B4B client, and
// holds no B4B credential to do it. addr is the vendor listener, for
// GET /sim/info and the control port's default (its port + 1000).
func (a *app) control(addr string) (*simctl.Control, error) {
	ctl, err := simctl.NewControl(simctl.Config{
		Service: "b4b", Kind: "vendor", Simulates: "B4B Payments (Oversight API)",
		Listeners: []string{addr},
		Reset: func(_ context.Context, req simctl.ResetRequest) (simctl.ResetResult, error) {
			return a.reset(req.Scope)
		},
	}, simctl.ControlAddr(addr))
	if err != nil {
		return nil, err
	}
	mux := ctl.Mux

	// A read-only window onto a lab run: only what the platform itself sent
	// and what this service said back.
	mux.HandleFunc("GET /sim/activity", activity.Handler(a.payLog, a.cbLog))

	// Registering a payee exactly as POST /oversight/v1/beneficiaries does --
	// the same handler, so the same body, refusals, answer, callback and
	// state file -- for the lab API, which registers merchants' outlets and
	// holds no B4B credential.
	mux.HandleFunc("POST /sim/beneficiaries", a.registerBeneficiary)

	// Real screening decides these, and they can change at any time under
	// continuous screening. Exposing them lets a scenario exercise the paths
	// a client has to handle -- a payee going from pass to fail, a clean
	// payee that is nonetheless disabled, a PEP flagged separately from a
	// sanctions hit -- instead of reading that it would.
	mux.HandleFunc("PUT /sim/beneficiaries/{id}/sanctions", a.setSanctions)
	mux.HandleFunc("PUT /sim/beneficiaries/{id}/status", a.setBeneficiaryStatus)
	mux.HandleFunc("PUT /sim/companies/{id}/sanctions", a.setCompanySanctions)
	mux.HandleFunc("PUT /sim/people/{id}/sanctions", a.setPersonSanctions)
	return ctl, nil
}

// routes is b4b's vendor surface, the Oversight API a platform calls with
// its bearer JWT. ctl is the control surface's mux: the old /sim paths here
// delegate to it until phase 4 of docs/lab-api/plan.md retires them.
func (a *app) routes(ctl http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, map[string]string{"status": "ok", "service": "b4b"})
	})

	// Boarding: the chain a platform walks before it can pay anybody.
	mux.HandleFunc("POST /oversight/v1/companies", a.requireAuth(a.createCompany))
	mux.HandleFunc("GET /oversight/v1/companies", a.requireAuth(a.listCompanies))
	mux.HandleFunc("GET /oversight/v1/companies/{id}", a.requireAuth(a.getCompany))
	mux.HandleFunc("POST /oversight/v1/companies/{id}/addresses", a.requireAuth(a.createAddress))
	mux.HandleFunc("GET /oversight/v1/companies/{id}/addresses", a.requireAuth(a.listAddresses))
	mux.HandleFunc("POST /oversight/v1/companies/{id}/people", a.requireAuth(a.createPerson))
	mux.HandleFunc("GET /oversight/v1/companies/{id}/people", a.requireAuth(a.listPeople))
	mux.HandleFunc("GET /oversight/v1/companies/{id}/people/{personID}", a.requireAuth(a.getPerson))
	mux.HandleFunc("POST /oversight/v1/companies/{id}/extended", a.requireAuth(a.createExtended))
	// Both spellings of the read. The one with an id is what the vendor
	// documents; the one without is what a client actually has at the point
	// it needs to ask "did I already create this?", because it does not
	// know the id until it has read it.
	mux.HandleFunc("GET /oversight/v1/companies/{id}/extended", a.requireAuth(a.getExtended))
	mux.HandleFunc("GET /oversight/v1/companies/{id}/extended/{extendedID}", a.requireAuth(a.getExtended))
	mux.HandleFunc("POST /oversight/v1/companies/{id}/vibans", a.requireAuth(a.createViban))
	mux.HandleFunc("GET /oversight/v1/companies/{id}/vibans", a.requireAuth(a.listVibans))
	mux.HandleFunc("POST /oversight/v1/uploads", a.requireAuth(a.uploadDocument))
	mux.HandleFunc("GET /oversight/v1/uploads/{id}", a.requireAuth(a.getDocument))

	// Payout.
	mux.HandleFunc("POST /oversight/v1/beneficiaries", a.requireAuth(a.registerBeneficiary))
	mux.HandleFunc("GET /oversight/v1/beneficiaries/{id}", a.requireAuth(a.getBeneficiary))
	mux.HandleFunc("POST /oversight/v1/payments", a.requireAuth(a.payLog.Watch("payment.create", summarizePayment, a.createPayment)))
	// The documented recovery path for a missed callback: callbacks are
	// unsigned, unordered and may repeat, so a client that lost one reads
	// the current state instead of trying to replay it.
	mux.HandleFunc("GET /oversight/v1/payments/{id}", a.requireAuth(a.getPayment))

	// The old /sim paths, answering as they did: behind the bearer JWT, but
	// for GET /sim/activity, which never asked for one (the console held no
	// client credential to read it with).
	mux.Handle("GET /sim/activity", ctl)
	mux.HandleFunc("/sim/", a.requireAuth(ctl.ServeHTTP))
	return mux
}

// loadVerificationKey decides what this service verifies inbound tokens
// against.
//
// B4B_JWT_PUBLIC_KEY_PATH is the real-shaped option and takes precedence: a
// real vendor holds your *public* key and nothing else. The keys-directory
// path is the lab default, where this service generates the keypair and
// this lab's own settlement stand-in reads the private half back to sign
// with -- convenient, and inverted from how it works with a real vendor,
// which is why the other option exists. Point a third-party client at this
// mock by exporting its public key here rather than handing the mock its
// private one.
func loadVerificationKey(keysDir, pubKeyPath, kid string) (*rsa.PublicKey, error) {
	if pubKeyPath != "" {
		pub, err := b4b.LoadPublicKey(pubKeyPath)
		if err != nil {
			return nil, err
		}
		log.Printf("b4b: verifying inbound JWTs against %s (kid=%s)", pubKeyPath, kid)
		return pub, nil
	}
	key, err := b4b.LoadOrGenerateKeyPair(keysDir)
	if err != nil {
		return nil, err
	}
	pubPEM, err := b4b.PublicKeyPEM(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	// Printed to stdout (not the log package, which defaults to stderr) so
	// an operator/harness reading this service's stdout can pick it up.
	fmt.Printf("b4b: JWT public key (kid=%s):\n%s\n", kid, pubPEM)
	return &key.PublicKey, nil
}

// persist writes the whole store out. Fired by the directory and the
// beneficiary store after every mutation, and by onTransition after every
// payment state change.
//
// A failure here is logged and not fatal: losing the ability to remember
// is bad, but taking the service down mid-payment because a disk is full
// is worse, and the in-memory state is still correct.
func (a *app) persist() {
	if err := a.save(); err != nil {
		log.Printf("b4b: persist state to %s: %v", a.statePath, err)
	}
}

func (a *app) save() error {
	if a.statePath == "" {
		return nil
	}
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	var s b4b.State
	a.dir.Snapshot(&s)
	a.beneficiaries.Snapshot(&s)
	a.engine.Snapshot(&s)
	return b4b.SaveState(a.statePath, s)
}

// reset is POST /sim/reset (docs/lab-api/sim-control.md §6). flow drops the
// payments, the callbacks in flight and both activity logs, and keeps the
// boarding; all drops the boarding too, back to the empty state file of a
// first start. Either way the state file is written before it answers, so
// a restart cannot bring back -- or resume -- a payment it dropped. The
// signing keys and B4B_FORCE_FAILURE_BENEFICIARY_IDS are configuration and
// stay.
func (a *app) reset(scope string) (simctl.ResetResult, error) {
	var res simctl.ResetResult
	var err error
	a.engine.Gate().Hold(func(deliveries int) {
		res = b4b.Reset(scope, a.engine, a.dir, a.beneficiaries)
		res.Cleared["callbacks_in_flight"] = deliveries
		res.Cleared["activity_events"] = int(a.payLog.Clear() + a.cbLog.Clear())
		res.Kept = append(res.Kept, "jwt_keys", "force_failure_beneficiary_ids")
		err = a.save()
	})
	if err != nil {
		return res, fmt.Errorf("reset in memory, but the state file was not written, so a restart would bring it back: %w", err)
	}
	return res, nil
}

// later delivers in the background, counted as in flight until it is done,
// under the epoch the work started in: a reset in between drops it.
func (a *app) later(started uint64, deliver func(started uint64)) {
	a.engine.Gate().Go(started, deliver)
}

// requireAuth verifies the inbound Authorization: Bearer <RS512 JWT> header
// against this service's configured public key before delegating to next.
// Any parse/verify failure is rejected 401 with a clear log message -- B4B
// is the server here, so it is the one doing the verifying (docs/
// ARCHITECTURE-vendor-corrections.md section 4).
func (a *app) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		authz := r.Header.Get("Authorization")
		if !strings.HasPrefix(authz, prefix) {
			log.Printf("b4b: rejected %s %s: missing Bearer authorization header", r.Method, r.URL.Path)
			httputilx.Error(w, 401, "missing bearer token")
			return
		}
		token := strings.TrimPrefix(authz, prefix)
		if err := b4b.VerifyBearerToken(token, a.pubKey, a.kid); err != nil {
			log.Printf("b4b: rejected %s %s: %v", r.Method, r.URL.Path, err)
			httputilx.Error(w, 401, "invalid bearer token")
			return
		}
		next(w, r)
	}
}

// writeStoreError maps a domain error to the status code the real API
// answers with. The distinction that matters is 400 vs 422: a missing
// field is something the caller fixes and resends, while "you already
// created this" is something it has to go and read instead -- and a client
// that cannot tell them apart retries the one it should reconcile.
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, b4b.ErrNotFound):
		httputilx.Error(w, 404, err.Error())
	case errors.Is(err, b4b.ErrDuplicate), errors.Is(err, b4b.ErrRequirementUnmet):
		httputilx.Error(w, 422, err.Error())
	default:
		httputilx.Error(w, 400, err.Error())
	}
}

// callbackTarget picks where a callback for this entity goes: its own
// callback_url if the caller supplied one (a lab affordance), otherwise the
// client-account endpoint from B4B_CALLBACK_URL. Empty means nowhere, and
// the caller logs that rather than silently doing nothing.
func (a *app) callbackTarget(override string) string {
	if override != "" {
		return override
	}
	return a.callbackURL
}

// deliverCallback POSTs a plain-JSON callback, retrying with backoff until
// 2xx or MAX_ATTEMPTS. B4B's Oversight callbacks sign nothing -- no HMAC,
// no AES-GCM -- and are retried by the real vendor for about a day, which
// is why every callback here goes through one retrying path rather than
// some of them being best-effort single shots.
//
// started is the epoch the work began in. Every attempt, and the activity
// event it leaves, goes through the gate: after a reset the rest of the
// delivery is dropped, not raced.
func (a *app) deliverCallback(started uint64, what, dest string, body any) {
	gate := a.engine.Gate()
	if dest == "" {
		gate.Do(started, func() {
			log.Printf("b4b: %s callback not delivered: no callback_url and no B4B_CALLBACK_URL configured", what)
			a.cbLog.Record(activity.Event{
				Op: "callback", Summary: what + " — nowhere to send it",
				Status: activity.StatusWarn,
				Detail: map[string]string{"reason": "no callback_url on the record and no B4B_CALLBACK_URL configured"},
			})
		})
		return
	}
	payload, err := json.Marshal(body)
	if err != nil {
		log.Printf("b4b: marshal %s callback: %v", what, err)
		return
	}
	var last int
	var lastErr error
	for attempt := 1; attempt <= a.maxTry; attempt++ {
		if wait := retry.SleepBeforeAttempt(attempt, a.backs); wait > 0 {
			time.Sleep(wait)
		}
		var st int
		var postErr error
		if !gate.Do(started, func() {
			st, postErr = a.postWebhook(dest, payload)
			if postErr == nil {
				log.Printf("b4b: %s callback attempt=%d http=%d delivered to %s", what, attempt, st, dest)
				a.cbLog.Record(activity.Event{
					Op: "callback", Peer: dest, Summary: what + " delivered",
					Status: activity.StatusOK,
					Detail: map[string]string{"attempt": strconv.Itoa(attempt), "http": strconv.Itoa(st)},
				})
			}
		}) {
			log.Printf("b4b: %s callback dropped before attempt %d: b4b was reset", what, attempt)
			return
		}
		last, lastErr = st, postErr
		if postErr == nil {
			return
		}
		log.Printf("b4b: %s callback attempt=%d http=%d err=%v", what, attempt, st, postErr)
		if !retry.ShouldRetry(st, attempt, a.maxTry) {
			break
		}
	}
	// Recorded once, at the end, rather than per attempt: the retries are
	// one event from the operator's point of view, and a panel that shows
	// four rows for one refused callback buries the other three payouts.
	gate.Do(started, func() {
		log.Printf("b4b: %s callback failed after retries: http=%d err=%v", what, last, lastErr)
		detail := map[string]string{"attempts": strconv.Itoa(a.maxTry), "http": strconv.Itoa(last)}
		if lastErr != nil {
			detail["error"] = lastErr.Error()
		}
		a.cbLog.Record(activity.Event{
			Op: "callback", Peer: dest, Summary: what + " refused by the client after " + strconv.Itoa(a.maxTry) + " attempts",
			Status: activity.StatusBad, Detail: detail,
		})
	})
}

// postWebhook does the plain JSON POST. The allowlist is enforced here as
// well as at creation time, so a delivery can never bypass policy even if
// the destination was stored before the policy changed.
func (a *app) postWebhook(dest string, body []byte) (int, error) {
	if err := a.list.Allowed(dest); err != nil {
		return 0, err
	}
	req, err := http.NewRequest(http.MethodPost, dest, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("b4b: webhook destination returned %s", resp.Status)
	}
	return resp.StatusCode, nil
}

func parseForceFailSet(spec string) map[string]bool {
	set := map[string]bool{}
	for _, raw := range strings.Split(spec, ",") {
		raw = strings.TrimSpace(raw)
		if raw != "" {
			set[raw] = true
		}
	}
	return set
}

func statePathLabel(p string) string {
	if p == "" {
		return "(disabled, in-memory only)"
	}
	return p
}

func shortID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// paymentID is a UUIDv4, because that is what a client validates it as: the
// platform's own payment-callback schema declares the id a uuid and answers
// 422 to anything else, so a prettier "b4bp_<hex>" is not a cosmetic choice —
// it makes every callback for that payment undeliverable.
func paymentID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
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

func envBool(k string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(k))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
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
