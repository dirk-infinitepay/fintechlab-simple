// Package simctl is the part of the sim control contract every simulation
// serves the same way (docs/lab-api/sim-control.md): who it is
// (GET /sim/info), its own log lines (GET /sim/logs) and putting it back to
// a known state (POST /sim/reset). What a reset clears is each vendor's own
// business and stays in its package; this package is the plumbing around
// it — scopes, the refusal, the line in the log, the answer's shape, and
// the epoch that keeps in-flight work from landing after a reset.
package simctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/logring"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
	"github.com/webduvet/fintechlab-simple/internal/trap"
)

// The reset scopes (sim-control.md §6).
const (
	// ScopeFlow clears what a run makes and keeps what a platform set up
	// once: subscriptions, boarding, enrolment.
	ScopeFlow = "flow"
	// ScopeAll puts the sim back as it was on first start.
	ScopeAll = "all"
)

// Config describes one simulation.
type Config struct {
	Service   string // "banking-circle"
	Kind      string // "vendor", "lab" or "stand-in"
	Simulates string // "Banking Circle Connect"
	// Listeners are the vendor-facing addresses, for GET /sim/info.
	Listeners []string
	// ControlListener is the control surface's own address, set by
	// NewControl.
	ControlListener string
	// Scopes are the reset scopes the sim honours. Empty means both.
	Scopes []string
	// Reset puts the sim back to a known state. Required.
	Reset ResetFunc
	// Logs is the ring GET /sim/logs reads. Nil means logring.Default, and
	// a sim with neither answers 501.
	Logs *logring.Ring
	// Traps is the sim's trap store, when it has hooks: Mount serves its
	// /sim/traps routes, GET /sim/info lists its hooks, and a reset
	// disarms every trap before the sim's own (internal/trap).
	Traps *trap.Store
	// Customers, when set, are the environments the sim serves besides
	// the default (internal/tenant): Mount serves /sim/customers, GET
	// /sim/info says "environments": true, and a reset may name one
	// environment. A sim without them is the default environment's alone,
	// and refuses a reset of any other.
	Customers *tenant.Customers
}

// ResetFunc does a reset. It returns what it cleared (a count per kind of
// thing) and the names of what it kept on purpose. A Refusal is a reset
// that cannot happen safely now, answered 409; req.Force asks for it anyway.
type ResetFunc func(ctx context.Context, req ResetRequest) (ResetResult, error)

// ResetRequest is the body of POST /sim/reset.
type ResetRequest struct {
	Scope string `json:"scope"`
	Force bool   `json:"force"`
	// Env is the environment to reset (internal/tenant): one environment's
	// records only, or every environment's for "*". Empty is the
	// default's; ?env= or the Lab-Environment header say the same.
	Env string `json:"env,omitempty"`
}

// ResetResult is what a reset reports.
type ResetResult struct {
	Cleared map[string]int `json:"cleared"`
	Kept    []string       `json:"kept"`
}

// Refusal is a reset the sim will not do right now, with the reason in
// words an operator can act on.
type Refusal struct{ Reason string }

func (r Refusal) Error() string { return r.Reason }

// Epoch is the counter a reset bumps. Work that outlives a request — a
// payment walking its phases on a delay, a webhook retrying — takes the
// epoch when it starts and checks it before each step it takes; if a reset
// moved it in between, the work is dropped. A reset is then never followed
// by a stray callback for a payment that no longer exists (sim-control.md
// §6).
//
// A check followed by an effect still races a reset that lands between the
// two. Where that matters — a state change, a POST to another service — the
// check and the effect go under a read lock that the reset takes for
// writing, so an effect lands wholly before a reset or not at all. b4b's
// Gate, aci's SentLog and worldline's and settlement's runGuard each do
// exactly that; one shared guard here, replacing the four, is still to do.
type Epoch struct{ n atomic.Uint64 }

// Current is the epoch now, to remember when work starts.
func (e *Epoch) Current() uint64 { return e.n.Load() }

// Bump moves the epoch, from the reset.
func (e *Epoch) Bump() uint64 { return e.n.Add(1) }

// Moved reports whether a reset happened since the work took started.
func (e *Epoch) Moved(started uint64) bool { return e.n.Load() != started }

var startedAt = time.Now().UTC()

// Mount registers GET /sim/info, GET /sim/logs and POST /sim/reset for c.
func Mount(mux *http.ServeMux, c Config) {
	if c.Reset == nil {
		panic("simctl: " + c.Service + " has no Reset")
	}
	if len(c.Scopes) == 0 {
		c.Scopes = []string{ScopeFlow, ScopeAll}
	}
	mux.HandleFunc("GET /sim/info", c.info)
	mux.HandleFunc("GET /sim/logs", c.logs)
	if c.Traps != nil {
		c.Traps.Routes(mux)
	}
	if c.Customers != nil {
		c.Customers.Routes(mux)
	}
	var mu sync.Mutex // one reset at a time
	mux.HandleFunc("POST /sim/reset", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		c.reset(w, r)
	})
}

func (c Config) ring() *logring.Ring {
	if c.Logs != nil {
		return c.Logs
	}
	return logring.Default
}

func (c Config) info(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"service":          c.Service,
		"kind":             c.Kind,
		"simulates":        c.Simulates,
		"started_at":       startedAt,
		"vendor_listeners": nonNil(c.Listeners),
		"control_listener": c.ControlListener,
		"reset_scopes":     c.Scopes,
		"hooks":            []string{},
		"environments":     c.Customers != nil,
	}
	if c.Customers != nil {
		out["customers"] = len(c.Customers.List())
	}
	if c.Traps != nil {
		armed, pending := c.Traps.Counts()
		out["hooks"] = c.Traps.Hooks()
		out["traps"] = map[string]int{"armed": armed, "pending_steps": pending}
	}
	if b := build(); b != "" {
		out["build"] = b
	}
	if url := strings.TrimSpace(os.Getenv("LAB_CLOCK_URL")); url != "" {
		out["clock"] = map[string]any{"follows": url, "offset_ms": labclock.Offset().Milliseconds()}
	}
	if ring := c.ring(); ring != nil {
		held, capacity := ring.Held()
		out["logs"] = map[string]int{"held": held, "capacity": capacity}
	}
	httputilx.WriteJSON(w, 200, out)
}

func (c Config) logs(w http.ResponseWriter, r *http.Request) {
	ring := c.ring()
	if ring == nil {
		httputilx.Error(w, 501, c.Service+" keeps no log ring")
		return
	}
	ring.Handler(c.Service)(w, r)
}

func (c Config) reset(w http.ResponseWriter, r *http.Request) {
	var req ResetRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	if len(bytes.TrimSpace(body)) > 0 {
		// Strict: a misspelt field must not reset with a different scope.
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			httputilx.Error(w, 400, `want {"scope": "flow"|"all", "force": false, "env": "<environment>"}: `+err.Error())
			return
		}
	}
	if req.Scope == "" {
		req.Scope = ScopeFlow
	}
	if req.Env == "" {
		req.Env = tenant.FromRequest(r)
	}
	req.Env = tenant.Normalize(req.Env)
	switch {
	case req.Env == tenant.Default || req.Env == tenant.All:
	case c.Customers == nil:
		httputilx.Error(w, 400, c.Service+" does not support environments: it resets the default environment, or everything (\"*\")")
		return
	default:
		if _, ok := c.Customers.Get(req.Env); !ok {
			httputilx.Error(w, 404, "no environment "+req.Env+" at "+c.Service)
			return
		}
	}
	if !slices.Contains(c.Scopes, req.Scope) {
		httputilx.Error(w, 400, fmt.Sprintf("%s resets with scope %s, not %q", c.Service, strings.Join(c.Scopes, " or "), req.Scope))
		return
	}
	// Traps first: a step still waiting must not land on state the reset
	// is about to clear (traps.md §6). A sim that then refuses its reset
	// (409) has lost its traps all the same; the clock is the one sim that
	// refuses, and it has no hooks.
	var traps, steps int
	if c.Traps != nil {
		scope := req.Env
		if c.Customers == nil {
			scope = tenant.All // a sim with no environments is the default's alone
		}
		traps, steps = c.Traps.ResetIn(scope)
	}
	res, err := c.Reset(r.Context(), req)
	var refusal Refusal
	if errors.As(err, &refusal) {
		log.Printf("%s: reset (%s) of environment %s refused: %s", c.Service, req.Scope, req.Env, refusal.Reason)
		httputilx.Error(w, 409, refusal.Reason+`; {"force": true} resets anyway`)
		return
	}
	if err != nil {
		log.Printf("%s: reset (%s) failed: %v", c.Service, req.Scope, err)
		httputilx.Error(w, 500, err.Error())
		return
	}
	if res.Cleared == nil {
		res.Cleared = map[string]int{}
	}
	if c.Traps != nil {
		res.Cleared["traps"] = traps
		res.Cleared["trap_steps"] = steps
	}
	res.Kept = nonNil(res.Kept)
	// The log is kept across a reset, with this line marking it: the log is
	// where you look for what happened just before (sim-control.md §6).
	log.Printf("%s: reset (%s%s) of environment %s — cleared %s; kept %s", c.Service, req.Scope,
		map[bool]string{true: ", forced", false: ""}[req.Force], req.Env, counts(res.Cleared), orNone(res.Kept))
	httputilx.WriteJSON(w, 200, map[string]any{
		"service": c.Service,
		"scope":   req.Scope,
		"env":     req.Env,
		"cleared": res.Cleared,
		"kept":    res.Kept,
		"at":      time.Now().UTC(),
	})
}

func counts(m map[string]int) string {
	if len(m) == 0 {
		return "nothing"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "nothing"
	}
	return strings.Join(s, ", ")
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// build is the VCS revision the binary was built from, when the build
// recorded one.
func build() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if rev != "" && dirty {
		rev += "-dirty"
	}
	return rev
}
