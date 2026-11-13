// Package vendorsim is what the platform's verification vendors' sims share:
// Creditsafe, iban.com, KYC6 and LexisNexis are each a sim of their own, and
// each answers the platform's real client with that vendor's real contract.
// What they have in common is the lab's side of them: the environments each
// serves, the outcome switches, the tokens they issue, the http.request traps
// (docs/lab-api/traps.md §9.7), the request journal and the reset.
//
// The contract is real: the paths, the auth handshakes, the request bodies
// and the response shapes are what the platform's clients send and parse. The
// judgement is stubbed: every check passes until an outcome is flipped at
// POST /sim/outcome, or a trap answers instead.
//
// Each environment the lab created is a customer (internal/tenant), known by
// its verification login: the username is the Creditsafe username and the
// LexisNexis client id; the password is the Creditsafe password, the
// LexisNexis client secret and the KYC6, iban.com and IVI key. A call made
// with a customer's login is that environment's, and so is every token it
// was issued; any other login is the default environment's.
package vendorsim

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/logring"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
	"github.com/webduvet/fintechlab-simple/internal/trap"
)

// Check is one outcome switch: its name and the answers it can give, the
// first being the default.
type Check struct {
	Name     string
	Outcomes []string
}

// Config is one vendor's sim.
type Config struct {
	// Service is the sim's id in the lab: "creditsafe".
	Service string
	// Simulates is the vendor, as GET /sim/info names it.
	Simulates string
	// Checks are the outcome switches the vendor's answers follow.
	Checks []Check
	// Kept is what a flow reset keeps besides the tokens, for the reset's
	// answer: a vendor's own state the platform holds on to.
	Kept []string
	// Reset clears the vendor's own state in scope; all is a first start.
	// It returns what it cleared, by name.
	Reset func(scope string, all bool) map[string]int
	// Forget drops a deleted customer's own state.
	Forget func(env string)
	// Summarize is the journal's line for a call, when the default (the
	// method and path, and a 4xx's error) misses a refusal of the vendor's
	// own shape.
	Summarize activity.Summarizer
}

// Sim is one vendor's sim: its two surfaces and the lab state they share.
type Sim struct {
	cfg       Config
	customers *tenant.Customers
	traps     *trap.Store
	requests  *activity.Log
	ctl       *simctl.Control
	mux       *http.ServeMux

	mu       sync.Mutex
	outcomes map[string]map[string]string

	tokens sync.Map
}

type issued struct {
	env string
	at  time.Time
}

// New builds the sim for cfg listening on addr: the vendor surface, with
// GET /health and the old /sim paths, and the control surface on its own
// port (internal/simctl) with the outcome switches, the journal and the
// control contract.
func New(cfg Config, addr string) (*Sim, error) {
	s := &Sim{
		cfg:       cfg,
		customers: tenant.NewCustomers(),
		traps:     trap.NewStore(cfg.Service, trap.HTTPRequestHook()),
		requests: activity.New("requests", "Vendor requests",
			"Every request the platform made to "+cfg.Simulates+", and the status it was answered with."),
		outcomes: map[string]map[string]string{},
	}
	s.customers.OnDelete = s.forget
	ctl, err := simctl.NewControl(simctl.Config{
		Service: cfg.Service, Kind: "vendor", Simulates: cfg.Simulates,
		Listeners: []string{addr},
		Scopes:    []string{simctl.ScopeFlow, simctl.ScopeAll},
		Reset:     s.reset,
		Customers: s.customers,
		Traps:     s.traps,
	}, simctl.ControlAddr(addr))
	if err != nil {
		return nil, err
	}
	s.ctl = ctl
	ctl.Mux.HandleFunc("GET /sim/outcome", s.readOutcomes)
	ctl.Mux.HandleFunc("POST /sim/outcome", s.setOutcome)
	ctl.Mux.HandleFunc("GET /sim/activity", s.readActivity(activity.Handler(s.requests, s.traps.Log())))

	s.mux = http.NewServeMux()
	s.mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, map[string]any{"status": "ok", "service": cfg.Service})
	})
	s.mux.Handle("/sim/", ctl.Mux)
	return s, nil
}

// Vendor serves pattern on the vendor surface.
func (s *Sim) Vendor(pattern string, h http.HandlerFunc) { s.mux.HandleFunc(pattern, h) }

// Control serves pattern on the control surface: a vendor's own lab seam.
func (s *Sim) Control(pattern string, h http.HandlerFunc) { s.ctl.Mux.HandleFunc(pattern, h) }

// Surfaces are the vendor surface and the control port. Every vendor request
// goes through the request journal, then the traps: a request a trap
// answers is journalled with that answer.
func (s *Sim) Surfaces() (http.Handler, *simctl.Control) {
	trapped := s.faults(s.mux)
	journalled := s.requests.Watch(s.cfg.Service, s.cfg.Summarize, trapped.ServeHTTP)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if labSeam(r) {
			trapped.ServeHTTP(w, r)
			return
		}
		journalled(w, r)
	}), s.ctl
}

func labSeam(r *http.Request) bool {
	return r.URL.Path == "/health" || strings.HasPrefix(r.URL.Path, "/sim/")
}

// Run serves the sim until it fails.
func (s *Sim) Run(addr string) {
	logring.Install()
	s.ctl.Start()
	log.Printf("%s listening on %s — every check passes by default; POST /sim/outcome on %s to change one", s.cfg.Service, addr, s.ctl.Addr())
	vendor, _ := s.Surfaces()
	log.Fatal(http.ListenAndServe(addr, logReq(vendor)))
}

// Listen is LISTEN, or def.
func Listen(def string) string {
	if v := os.Getenv("LISTEN"); v != "" {
		return v
	}
	return def
}

// --- outcomes -----------------------------------------------------------

func (s *Sim) defaults() map[string]string {
	out := map[string]string{}
	for _, c := range s.cfg.Checks {
		out[c.Name] = c.Outcomes[0]
	}
	return out
}

func (s *Sim) outcomesOf(env string) map[string]string {
	env = tenant.Normalize(env)
	o := s.outcomes[env]
	if o == nil {
		o = s.defaults()
		s.outcomes[env] = o
	}
	return o
}

// Answer is what check answers the caller: the environment its login named.
func (s *Sim) Answer(r *http.Request, check string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outcomesOf(tenant.Of(r.Context()))[check]
}

// SetOutcome flips check in env.
func (s *Sim) SetOutcome(env, check, outcome string) error {
	var values []string
	var names []string
	for _, c := range s.cfg.Checks {
		names = append(names, c.Name)
		if c.Name == check {
			values = c.Outcomes
		}
	}
	if values == nil {
		return fmt.Errorf("unknown check %q (have: %s)", check, strings.Join(names, ", "))
	}
	for _, v := range values {
		if v == outcome {
			s.mu.Lock()
			s.outcomesOf(env)[check] = outcome
			s.mu.Unlock()
			return nil
		}
	}
	return fmt.Errorf("check %q cannot be %q (want: %s)", check, outcome, strings.Join(values, ", "))
}

func (s *Sim) snapshot(env string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for k, v := range s.outcomesOf(env) {
		out[k] = v
	}
	return out
}

// Known reports whether env is one this sim serves: the default, every
// environment ("*"), or a customer's.
func (s *Sim) Known(env string) bool {
	if env == tenant.Default || env == tenant.All {
		return true
	}
	_, ok := s.customers.Get(env)
	return ok
}

func (s *Sim) readOutcomes(w http.ResponseWriter, r *http.Request) {
	env := tenant.FromRequest(r)
	if !s.Known(env) {
		httputilx.Error(w, 404, "no environment "+env+" at "+s.cfg.Service)
		return
	}
	if env != tenant.All {
		httputilx.WriteJSON(w, 200, map[string]any{"env": env, "outcomes": s.snapshot(env)})
		return
	}
	all := map[string]map[string]string{}
	for _, e := range s.customers.Envs() {
		all[e] = s.snapshot(e)
	}
	httputilx.WriteJSON(w, 200, map[string]any{"env": env, "environments": all})
}

// readActivity is GET /sim/activity for an environment this sim knows: a
// typo is a 404, as at GET /sim/outcome, not an empty journal.
func (s *Sim) readActivity(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if env := tenant.FromRequest(r); !s.Known(env) {
			httputilx.Error(w, 404, "no environment "+env+" at "+s.cfg.Service)
			return
		}
		next(w, r)
	}
}

type outcomeReq struct {
	Check   string `json:"check"`
	Outcome string `json:"outcome"`
}

func (s *Sim) setOutcome(w http.ResponseWriter, r *http.Request) {
	env := tenant.FromRequest(r)
	switch {
	case env == tenant.All:
		httputilx.Error(w, 400, "an outcome is set in one environment, not in all of them")
		return
	case !s.Known(env):
		httputilx.Error(w, 404, "no environment "+env+" at "+s.cfg.Service)
		return
	}
	var req outcomeReq
	if err := httputilx.ReadJSON(r, &req); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	if err := s.SetOutcome(env, req.Check, req.Outcome); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	log.Printf("%s: %s now answers %q in environment %s", s.cfg.Service, req.Check, req.Outcome, env)
	httputilx.WriteJSON(w, 200, map[string]any{"env": env, "outcomes": s.snapshot(env)})
}

// --- reset and environments ---------------------------------------------

func (s *Sim) reset(_ context.Context, req simctl.ResetRequest) (simctl.ResetResult, error) {
	scope := tenant.Normalize(req.Env)
	flipped := 0
	s.mu.Lock()
	defaults := s.defaults()
	for env, o := range s.outcomes {
		if !tenant.InScope(scope, env) {
			continue
		}
		for check, v := range defaults {
			if o[check] != v {
				flipped++
			}
		}
		s.outcomes[env] = s.defaults()
	}
	s.mu.Unlock()
	all := req.Scope == simctl.ScopeAll
	cleared := map[string]int{"outcomes": flipped, "activity": int(s.requests.ClearIn(scope))}
	if s.cfg.Reset != nil {
		for k, v := range s.cfg.Reset(scope, all) {
			cleared[k] = v
		}
	}
	if !all {
		kept := append([]string{"tokens"}, s.cfg.Kept...)
		sort.Strings(kept[1:])
		return simctl.ResetResult{Cleared: cleared, Kept: kept}, nil
	}
	cleared["tokens"] = s.dropTokens(scope)
	return simctl.ResetResult{Cleared: cleared}, nil
}

func (s *Sim) dropTokens(scope string) int {
	n := 0
	s.tokens.Range(func(k, v any) bool {
		if tenant.InScope(scope, v.(issued).env) {
			s.tokens.Delete(k)
			n++
		}
		return true
	})
	return n
}

func (s *Sim) forget(env string) error {
	s.dropTokens(env)
	s.traps.ResetIn(env)
	s.requests.ClearIn(env)
	s.mu.Lock()
	delete(s.outcomes, env)
	s.mu.Unlock()
	if s.cfg.Forget != nil {
		s.cfg.Forget(env)
	}
	return nil
}

// --- auth ---------------------------------------------------------------

// Bearer guards an endpoint that needs a token this sim issued. The call is
// the environment's the token was issued to.
func (s *Sim) Bearer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" || tok == r.Header.Get("Authorization") {
			httputilx.Error(w, 401, "bearer token required")
			return
		}
		v, ok := s.tokens.Load(tok)
		if !ok {
			httputilx.Error(w, 401, "unknown or expired token")
			return
		}
		next(w, s.As(r, v.(issued).env))
	}
}

// APIKey guards an endpoint that takes a header key. Any non-empty key is
// accepted; a customer's key makes the call its environment's.
func (s *Sim) APIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("x-api-key")
		if key == "" {
			httputilx.Error(w, 401, "x-api-key header required")
			return
		}
		next(w, s.As(r, s.ByKey(key)))
	}
}

// As is r, the call of env: the environment goes into its context, for the
// handler's outcomes and the journal, and env is recorded as seen.
func (s *Sim) As(r *http.Request, env string) *http.Request {
	s.customers.Seen(env)
	return r.WithContext(tenant.With(r.Context(), env))
}

// ByKey is the environment whose verification password key is; any other
// key is the default's.
func (s *Sim) ByKey(key string) string {
	c, ok := s.customers.Find(func(c tenant.Customer) bool {
		return c.Verification != nil && c.Verification.Password != "" && c.Verification.Password == key
	})
	if !ok {
		return tenant.Default
	}
	return c.Env
}

// ByLogin is the environment a username and password log in as. A username
// no customer has is the default's, whatever the password; a customer's
// username with another password is refused, as the real vendor would.
func (s *Sim) ByLogin(username, password string) (string, bool) {
	c, ok := s.customers.Find(func(c tenant.Customer) bool {
		return c.Verification != nil && c.Verification.Username != "" && c.Verification.Username == username
	})
	if !ok {
		return tenant.Default, true
	}
	return c.Env, c.Verification.Password == password
}

// Issue is a new bearer token for env.
func (s *Sim) Issue(prefix, env string) string {
	tok := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	s.tokens.Store(tok, issued{env: env, at: time.Now()})
	return tok
}

// faults lets an http.request trap answer a vendor request first, in the
// environment the request's credentials name.
func (s *Sim) faults(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if labSeam(r) {
			next.ServeHTTP(w, r)
			return
		}
		env := s.envOfCredentials(r)
		tenant.Set(r.Context(), env)
		if s.traps.Intercept(w, r, env) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Sim) envOfCredentials(r *http.Request) string {
	if tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); tok != r.Header.Get("Authorization") {
		if v, ok := s.tokens.Load(tok); ok {
			return v.(issued).env
		}
	}
	if key := r.Header.Get("x-api-key"); key != "" {
		return s.ByKey(key)
	}
	return tenant.Default
}

// Str is v as a string, or "".
func Str(v any) string {
	str, _ := v.(string)
	return str
}

func logReq(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
