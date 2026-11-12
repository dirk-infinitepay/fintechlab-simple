// Package trap is the generic half of traps (docs/lab-api/traps.md): an
// instruction armed in advance in one simulation, deciding something about
// a future event at one of its hooks — which outcome it takes, and when.
//
// A sim declares its hooks (what can be caught, the fields a trap matches
// on, and for a lifecycle the graph of states that may follow each other),
// offers each event to the store as it happens, and applies the steps of a
// caught event through its own engine. Everything else is here: arming and
// validating, matching and precedence, the step schedule in real or lab
// time, cancelling on disarm and on reset, the "traps" activity log, and
// the /sim/traps routes on the sim's control port.
//
// A trap only ever puts the vendor in a state the real vendor can be in,
// and it is invisible on the vendor surface: what a trap caused carries
// detail.trap in the sim's own logs, nowhere a platform looks.
package trap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// Field types a hook's match fields have.
const (
	TypeString  = "string"
	TypeIBAN    = "iban"    // compared upper case, spaces removed
	TypeDecimal = "decimal" // compared as numbers: "10" equals "10.00"
)

// Trap states.
const (
	StateArmed    = "armed"
	StateSpent    = "spent"
	StateExpired  = "expired"
	StateDisarmed = "disarmed"
)

// EffectPath is a lifecycle's next states.
const EffectPath = "path"

// Field is one field a hook's traps can match on.
type Field struct {
	Name  string `json:"field"`
	Type  string `json:"type"`
	About string `json:"about,omitempty"`
}

// Graph is a lifecycle: which states may follow which.
type Graph struct {
	Start string              `json:"start"`
	Next  map[string][]string `json:"next"`
	// Params are the parameters a step to each state may carry: reason,
	// reason_code, reason_description.
	Params map[string][]string `json:"params,omitempty"`
	// Walks is true when a step may name a state further along than the
	// next: the sim walks the states in between at its own pace (B4B).
	// False, every step names a direct successor (Banking Circle).
	Walks bool `json:"walks,omitempty"`
}

// Hook is one point in a sim where traps catch events.
type Hook struct {
	Name    string  `json:"hook"`
	About   string  `json:"about"`
	Subject string  `json:"subject"`
	Match   []Field `json:"match"`
	Effect  string  `json:"effect"`
	Graph   *Graph  `json:"graph,omitempty"`
	// FirstAfter is how long after the event the first step applies when
	// it names no after of its own: the vendor's normal delay.
	FirstAfter time.Duration `json:"-"`
}

// Step is one state a caught event is taken to.
type Step struct {
	To                string `json:"to"`
	After             string `json:"after,omitempty"`
	AfterLab          string `json:"after_lab,omitempty"`
	Reason            string `json:"reason,omitempty"`
	ReasonCode        string `json:"reason_code,omitempty"`
	ReasonDescription string `json:"reason_description,omitempty"`
}

// Params are the step's parameters that are set.
func (s Step) Params() map[string]string {
	out := map[string]string{}
	for k, v := range map[string]string{"reason": s.Reason, "reason_code": s.ReasonCode, "reason_description": s.ReasonDescription} {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// Effect is what happens to a caught event: a path, or for a respond hook
// one of Respond, Delay or Close.
type Effect struct {
	Path    []Step    `json:"path"`
	Respond *Response `json:"respond,omitempty"`
	Delay   string    `json:"delay,omitempty"`
	Close   bool      `json:"close,omitempty"`
}

// MarshalJSON leaves the path out of a respond effect.
func (e Effect) MarshalJSON() ([]byte, error) {
	if e.Respond != nil || e.Delay != "" || e.Close {
		type respond struct {
			Respond *Response `json:"respond,omitempty"`
			Delay   string    `json:"delay,omitempty"`
			Close   bool      `json:"close,omitempty"`
		}
		return json.Marshal(respond{Respond: e.Respond, Delay: e.Delay, Close: e.Close})
	}
	type path struct {
		Path []Step `json:"path"`
	}
	return json.Marshal(path{Path: e.Path})
}

// Spec arms a trap. Match values are a string (exact), or In, Prefix or
// Between; Times is how many events it takes (0 means 1), Forever takes
// every one until disarmed.
type Spec struct {
	// Env is the environment the trap is armed in (internal/tenant): it
	// takes only that environment's events. Empty is the default's.
	Env       string
	Label     string
	Hook      string
	Match     map[string]any
	Times     int
	Forever   bool
	ExpiresAt *time.Time
	Path      []Step
	Respond   *Response
	Delay     string
	Close     bool
}

// In matches any of the values.
func In(values ...string) map[string]any { return map[string]any{"in": values} }

// Prefix matches values starting with p.
func Prefix(p string) map[string]any { return map[string]any{"prefix": p} }

// Between matches decimals from gte to lte, either open when "".
func Between(gte, lte string) map[string]any {
	m := map[string]any{}
	if gte != "" {
		m["gte"] = gte
	}
	if lte != "" {
		m["lte"] = lte
	}
	return m
}

// Trap is one armed instruction, as the store reports it.
type Trap struct {
	ID           string         `json:"id"`
	Env          string         `json:"env,omitempty"`
	Label        string         `json:"label,omitempty"`
	Hook         string         `json:"hook"`
	Match        map[string]any `json:"match"`
	Times        *int           `json:"times"`
	ExpiresAt    *time.Time     `json:"expires_at"`
	Effect       Effect         `json:"effect"`
	State        string         `json:"state"`
	Standing     bool           `json:"standing,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	Caught       []Catch        `json:"caught"`
	PendingSteps []Pending      `json:"pending_steps"`
}

// Catch is one event a trap took.
type Catch struct {
	Subject string        `json:"subject"`
	At      time.Time     `json:"at"`
	Applied []AppliedStep `json:"applied"`
	// Diverted is a step the vendor's own rules would not take: it did
	// what it does instead, and the path ended there.
	Diverted string `json:"diverted,omitempty"`
	// Failed is a step that could not be applied for any other reason.
	Failed    string `json:"failed,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
}

// AppliedStep is a step that took effect.
type AppliedStep struct {
	To string    `json:"to"`
	At time.Time `json:"at"`
}

// Pending is a step scheduled and not yet applied.
type Pending struct {
	TrapID  string     `json:"trap_id"`
	Subject string     `json:"subject"`
	Step    Step       `json:"step"`
	Due     *time.Time `json:"due,omitempty"`
	DueLab  *time.Time `json:"due_lab,omitempty"`

	key string // the catch and the step's place in its path
}

// Event is one event at a hook, offered to the armed traps.
type Event struct {
	// Env is the environment the event is about: the payer's, the
	// boarder's. Empty is the default's.
	Env     string
	Hook    string
	Subject string
	Fields  map[string]string
}

// ErrDiverted is a step the vendor's own rules would not take — the books
// cannot follow it — so the vendor did what it does instead, and the rest
// of the path is dropped (traps.md §4.1, OPEN 7). Wrap it with the reason:
// fmt.Errorf("%w: safeguarding balance 10.00 short of 25.00", ErrDiverted).
var ErrDiverted = errors.New("diverted by the vendor's own rule")

// ErrInvalid is a trap the store refuses to arm; the message says why.
var ErrInvalid = errors.New("invalid trap")

// trap is the store's own record of one trap.
type trap struct {
	Trap
	conds  []cond
	taken  int
	ctx    context.Context
	cancel context.CancelFunc
	spec   Spec
}

// Store holds one sim's traps.
type Store struct {
	service string
	hooks   map[string]Hook
	order   []string

	mu     sync.Mutex
	traps  []*trap // in the order they were armed
	seq    int
	root   context.Context
	cancel context.CancelFunc
	log    *activity.Log

	// Wall and LabNow are the real and the lab clock; tests set them.
	Wall   func() time.Time
	LabNow func() time.Time
	// LabPoll is how often a step waiting on the lab clock looks at it: a
	// step lands at most this long after the clock is moved past it.
	LabPoll time.Duration
}

// NewStore creates the store for service, with its hooks.
func NewStore(service string, hooks ...Hook) *Store {
	s := &Store{
		service: service,
		hooks:   map[string]Hook{},
		log: activity.New("traps", "Traps",
			"Every trap armed on this simulation, every event one caught, each step it applied, and each one disarmed. Amber is a step the vendor's own rules would not take."),
		Wall:    time.Now,
		LabNow:  labclock.Now,
		LabPoll: 250 * time.Millisecond,
	}
	for _, h := range hooks {
		s.hooks[h.Name] = h
		s.order = append(s.order, h.Name)
	}
	s.root, s.cancel = context.WithCancel(context.Background())
	return s
}

// Log is the store's "traps" activity log, for the sim's GET /sim/activity.
func (s *Store) Log() *activity.Log { return s.log }

// Hooks are the hooks' names, in the order the sim declared them.
func (s *Store) Hooks() []string { return append([]string(nil), s.order...) }

// Arm validates spec and arms it. The error wraps ErrInvalid with a reason
// an operator can act on.
func (s *Store) Arm(spec Spec) (Trap, error) { return s.arm(spec, false) }

// ArmStanding arms a trap the sim sets up from its own configuration at
// start (B4B_FORCE_FAILURE_BENEFICIARY_IDS): a reset puts it back, armed
// afresh, where it disarms every other trap.
func (s *Store) ArmStanding(spec Spec) (Trap, error) { return s.arm(spec, true) }

func (s *Store) arm(spec Spec, standing bool) (Trap, error) {
	h, ok := s.hooks[spec.Hook]
	if !ok {
		return Trap{}, fmt.Errorf("%w: %s has no hook %q; it has %s", ErrInvalid, s.service, spec.Hook, strings.Join(s.order, ", "))
	}
	conds, err := compile(h, spec.Match)
	if err != nil {
		return Trap{}, err
	}
	if spec.Times < 0 {
		return Trap{}, fmt.Errorf("%w: times is at least 1, or null for every event until disarmed", ErrInvalid)
	}
	if h.Effect == EffectPath {
		if spec.Path == nil {
			return Trap{}, fmt.Errorf("%w: %s's effect is a path: {\"path\": [{\"to\": …}, …]}; [] leaves the subject where it starts", ErrInvalid, h.Name)
		}
		if err := validatePath(h, spec.Path); err != nil {
			return Trap{}, err
		}
	}
	if h.Effect == EffectRespond {
		if err := validateRespond(h, spec); err != nil {
			return Trap{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.armLocked(spec, conds, standing), nil
}

func (s *Store) armLocked(spec Spec, conds []cond, standing bool) Trap {
	s.seq++
	t := &trap{conds: conds, spec: spec}
	t.ID = fmt.Sprintf("trp_%04d", s.seq)
	t.spec.Env = tenant.Normalize(spec.Env)
	if t.spec.Env != tenant.Default {
		t.Env = t.spec.Env
	}
	t.Label, t.Hook, t.ExpiresAt, t.Standing = spec.Label, spec.Hook, spec.ExpiresAt, standing
	t.Match = map[string]any{}
	for k, v := range spec.Match {
		t.Match[k] = v
	}
	if !spec.Forever {
		n := spec.Times
		if n == 0 {
			n = 1
		}
		t.Times = &n
	}
	t.Effect = Effect{Respond: spec.Respond, Delay: spec.Delay, Close: spec.Close}
	if spec.Path != nil || s.hooks[spec.Hook].Effect == EffectPath {
		t.Effect.Path = append([]Step{}, spec.Path...)
	}
	t.State = StateArmed
	t.CreatedAt = s.Wall().UTC()
	t.ctx, t.cancel = context.WithCancel(s.root)
	s.traps = append(s.traps, t)
	s.log.Record(activity.Event{Env: t.Env, Op: "trap.armed", Summary: fmt.Sprintf("%s armed on %s — %s", t.ID, t.Hook, describe(t.Trap)),
		Detail: map[string]string{"trap": t.ID, "hook": t.Hook}})
	log.Printf("%s: trap %s armed on %s — %s", s.service, t.ID, t.Hook, describe(t.Trap))
	return s.viewLocked(t)
}

func describe(t Trap) string {
	var parts []string
	if t.Label != "" {
		parts = append(parts, t.Label)
	}
	if len(t.Match) == 0 {
		parts = append(parts, "any event")
	} else {
		b, _ := json.Marshal(t.Match)
		parts = append(parts, "match "+string(b))
	}
	if t.Effect.Respond != nil || t.Effect.Delay != "" || t.Effect.Close {
		b, _ := json.Marshal(t.Effect)
		parts = append(parts, "→ "+string(b))
	} else if len(t.Effect.Path) > 0 {
		var tos []string
		for _, st := range t.Effect.Path {
			tos = append(tos, st.To)
		}
		parts = append(parts, "→ "+strings.Join(tos, " → "))
	} else {
		parts = append(parts, "→ stays where it starts")
	}
	return strings.Join(parts, "; ")
}

// Caught is a trap's hold on one event: the path to take it along. The sim
// runs it with Run, applying each step through its own engine.
type Caught struct {
	TrapID  string
	Label   string
	Subject string
	Path    []Step

	store *Store
	t     *trap
	idx   int // into t.Caught
	hook  Hook
}

// Offer gives an event to the armed traps on its hook, in the order they
// were armed. The first that matches takes it, and only that one.
func (s *Store) Offer(ev Event) (*Caught, bool) {
	h, ok := s.hooks[ev.Hook]
	if !ok {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Wall()
	for _, t := range s.traps {
		if t.Hook != ev.Hook || t.State != StateArmed || t.spec.Env != tenant.Normalize(ev.Env) {
			continue
		}
		if t.ExpiresAt != nil && now.After(*t.ExpiresAt) {
			t.State = StateExpired
			continue
		}
		if !matches(t.conds, ev.Fields) {
			continue
		}
		t.taken++
		if t.Times != nil && t.taken >= *t.Times {
			t.State = StateSpent
		}
		t.Caught = append(t.Caught, Catch{Subject: ev.Subject, At: now.UTC(), Applied: []AppliedStep{}})
		label := t.Label
		if label == "" {
			label = describe(t.Trap)
		}
		s.log.Record(activity.Event{Env: t.Env, Op: "trap.caught", Summary: fmt.Sprintf("%s caught %s — %s", t.ID, ev.Subject, label),
			Detail: map[string]string{"trap": t.ID, "subject": ev.Subject}})
		log.Printf("%s: trap %s caught %s at %s", s.service, t.ID, ev.Subject, ev.Hook)
		return &Caught{TrapID: t.ID, Label: t.Label, Subject: ev.Subject, Path: append([]Step{}, t.Effect.Path...),
			store: s, t: t, idx: len(t.Caught) - 1, hook: h}, true
	}
	return nil, false
}

// Run takes the caught event along its path in the background: each step
// waits its after (real time) or after_lab (lab clock), then apply takes
// it through the sim's engine. A step apply refuses with ErrDiverted ends
// the path as the vendor's own rule left it. Disarming the trap or
// resetting the sim cancels the steps not yet applied.
func (c *Caught) Run(apply func(Step) error) {
	go c.run(apply)
}

func (c *Caught) run(apply func(Step) error) {
	s := c.store
	prevReal, prevLab := s.Wall(), s.LabNow()
	for i, step := range c.Path {
		var due, dueLab *time.Time
		if step.AfterLab != "" {
			d, _ := parseDuration(step.AfterLab)
			at := prevLab.Add(d)
			dueLab = &at
		} else {
			d := time.Duration(0)
			if step.After != "" {
				d, _ = parseDuration(step.After)
			} else if i == 0 {
				d = c.hook.FirstAfter
			}
			at := prevReal.Add(d)
			due = &at
		}
		pending := Pending{TrapID: c.TrapID, Subject: c.Subject, Step: step, Due: due, DueLab: dueLab,
			key: fmt.Sprintf("%d/%d", c.idx, i)}
		s.mu.Lock()
		c.t.PendingSteps = append(c.t.PendingSteps, pending)
		s.mu.Unlock()

		ok := c.wait(due, dueLab)
		s.mu.Lock()
		c.t.PendingSteps = removePending(c.t.PendingSteps, pending.key)
		if !ok {
			c.t.Caught[c.idx].Cancelled = true
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()

		err := apply(step)
		s.mu.Lock()
		if c.t.ctx.Err() != nil {
			// Disarmed or reset while the step was being applied: the
			// sim's own guard decided whether it landed, and the trap it
			// belonged to is gone from the record — nothing to write.
			c.t.Caught[c.idx].Cancelled = true
			s.mu.Unlock()
			return
		}
		switch {
		case err == nil:
			c.t.Caught[c.idx].Applied = append(c.t.Caught[c.idx].Applied, AppliedStep{To: step.To, At: s.Wall().UTC()})
			s.log.Record(activity.Event{Env: c.t.Env, Op: "trap.step", Summary: fmt.Sprintf("%s took %s to %s", c.TrapID, c.Subject, step.To),
				Detail: map[string]string{"trap": c.TrapID, "subject": c.Subject, "to": step.To}})
			log.Printf("%s: trap %s took %s to %s", s.service, c.TrapID, c.Subject, step.To)
		case errors.Is(err, ErrDiverted):
			c.t.Caught[c.idx].Diverted = err.Error()
			s.log.Record(activity.Event{Env: c.t.Env, Op: "trap.diverted", Status: activity.StatusWarn,
				Summary: fmt.Sprintf("%s could not take %s to %s — %v", c.TrapID, c.Subject, step.To, err),
				Detail:  map[string]string{"trap": c.TrapID, "subject": c.Subject, "to": step.To}})
			log.Printf("%s: trap %s could not take %s to %s: %v", s.service, c.TrapID, c.Subject, step.To, err)
			s.mu.Unlock()
			return
		default:
			c.t.Caught[c.idx].Failed = err.Error()
			s.log.Record(activity.Event{Env: c.t.Env, Op: "trap.step", Status: activity.StatusBad,
				Summary: fmt.Sprintf("%s: %s to %s failed — %v", c.TrapID, c.Subject, step.To, err),
				Detail:  map[string]string{"trap": c.TrapID, "subject": c.Subject, "to": step.To}})
			log.Printf("%s: trap %s: %s to %s failed: %v", s.service, c.TrapID, c.Subject, step.To, err)
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		prevReal, prevLab = s.Wall(), s.LabNow()
	}
}

// wait sleeps until the step is due, and reports false when the trap was
// disarmed or the sim reset in the meantime.
func (c *Caught) wait(due, dueLab *time.Time) bool {
	ctx := c.t.ctx
	s := c.store
	if due != nil {
		d := due.Sub(s.Wall())
		if d <= 0 {
			return ctx.Err() == nil
		}
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}
	for s.LabNow().Before(*dueLab) {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(s.LabPoll):
		}
	}
	return ctx.Err() == nil
}

func removePending(ps []Pending, key string) []Pending {
	for i, p := range ps {
		if p.key == key {
			return append(ps[:i:i], ps[i+1:]...)
		}
	}
	return ps
}

// List is every environment's traps, newest first; state filters when
// not "".
func (s *Store) List(state string) []Trap { return s.ListIn(tenant.All, state) }

// ListIn is scope's traps, newest first; state filters when not "".
func (s *Store) ListIn(scope, state string) []Trap {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	out := []Trap{}
	for i := len(s.traps) - 1; i >= 0; i-- {
		t := s.traps[i]
		if tenant.InScope(scope, t.spec.Env) && (state == "" || t.State == state) {
			out = append(out, s.viewLocked(t))
		}
	}
	return out
}

// Get is one trap, whichever environment armed it.
func (s *Store) Get(id string) (Trap, bool) { return s.GetIn(tenant.All, id) }

// GetIn is one of scope's traps.
func (s *Store) GetIn(scope, id string) (Trap, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	for _, t := range s.traps {
		if t.ID == id && tenant.InScope(scope, t.spec.Env) {
			return s.viewLocked(t), true
		}
	}
	return Trap{}, false
}

// Disarm disarms one trap and cancels its pending steps, which it returns.
func (s *Store) Disarm(id string) (Trap, []Pending, bool) { return s.DisarmIn(tenant.All, id) }

// DisarmIn disarms one of scope's traps.
func (s *Store) DisarmIn(scope, id string) (Trap, []Pending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.traps {
		if t.ID == id && tenant.InScope(scope, t.spec.Env) {
			cancelled := s.disarmLocked(t)
			return s.viewLocked(t), cancelled, true
		}
	}
	return Trap{}, nil, false
}

// DisarmAll disarms every trap still armed or with steps pending.
func (s *Store) DisarmAll() (traps int, cancelled []Pending) { return s.DisarmAllIn(tenant.All) }

// DisarmAllIn disarms scope's traps still armed or with steps pending.
func (s *Store) DisarmAllIn(scope string) (traps int, cancelled []Pending) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cancelled = []Pending{}
	for _, t := range s.traps {
		if !tenant.InScope(scope, t.spec.Env) {
			continue
		}
		if t.State == StateArmed || len(t.PendingSteps) > 0 {
			traps++
			cancelled = append(cancelled, s.disarmLocked(t)...)
		}
	}
	return traps, cancelled
}

func (s *Store) disarmLocked(t *trap) []Pending {
	cancelled := append([]Pending{}, t.PendingSteps...)
	t.cancel()
	if t.State == StateArmed || len(cancelled) > 0 {
		t.State = StateDisarmed
		s.log.Record(activity.Event{Env: t.Env, Op: "trap.disarmed", Summary: fmt.Sprintf("%s disarmed; %d pending step(s) cancelled", t.ID, len(cancelled)),
			Detail: map[string]string{"trap": t.ID}})
		log.Printf("%s: trap %s disarmed, %d pending step(s) cancelled", s.service, t.ID, len(cancelled))
	}
	return cancelled
}

// Reset is the sim's reset: every trap disarmed and forgotten, every
// pending step cancelled, the traps log emptied — and the standing traps
// armed again, afresh. It returns how many traps and steps it dropped.
func (s *Store) Reset() (traps, pending int) { return s.ResetIn(tenant.All) }

// ResetIn is the reset of what scope names. One environment's reset drops
// only that environment's traps, their steps and their log; a standing
// trap (the default's, from the sim's configuration) is armed again
// afresh when its environment is reset.
func (s *Store) ResetIn(scope string) (traps, pending int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tenant.Normalize(scope) == tenant.All {
		s.cancel()
		s.root, s.cancel = context.WithCancel(context.Background())
	}
	var standing []*trap
	kept := s.traps[:0:0]
	for _, t := range s.traps {
		if !tenant.InScope(scope, t.spec.Env) {
			kept = append(kept, t)
			continue
		}
		t.cancel()
		pending += len(t.PendingSteps)
		if t.Standing {
			standing = append(standing, t)
		} else {
			traps++
		}
	}
	s.traps = kept
	s.log.ClearIn(scope)
	for _, t := range standing {
		s.armLocked(t.spec, t.conds, true)
	}
	return traps, pending
}

// Counts are how many traps are armed and how many steps are pending, for
// GET /sim/info.
func (s *Store) Counts() (armed, pending int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	for _, t := range s.traps {
		if t.State == StateArmed {
			armed++
		}
		pending += len(t.PendingSteps)
	}
	return armed, pending
}

func (s *Store) expireLocked() {
	now := s.Wall()
	for _, t := range s.traps {
		if t.State == StateArmed && t.ExpiresAt != nil && now.After(*t.ExpiresAt) {
			t.State = StateExpired
		}
	}
}

func (s *Store) viewLocked(t *trap) Trap {
	v := t.Trap
	v.Caught = append([]Catch{}, t.Caught...)
	for i := range v.Caught {
		v.Caught[i].Applied = append([]AppliedStep{}, v.Caught[i].Applied...)
	}
	v.PendingSteps = append([]Pending{}, t.PendingSteps...)
	sort.SliceStable(v.PendingSteps, func(i, j int) bool { return v.PendingSteps[i].Subject < v.PendingSteps[j].Subject })
	return v
}

// --- validation and matching -------------------------------------------

type cond struct {
	field  string
	typ    string
	equals string
	in     []string
	prefix string
	gte    *big.Rat
	lte    *big.Rat
}

func compile(h Hook, match map[string]any) ([]cond, error) {
	types := map[string]string{}
	var names []string
	for _, f := range h.Match {
		types[f.Name] = f.Type
		names = append(names, f.Name)
	}
	var out []cond
	keys := make([]string, 0, len(match))
	for k := range match {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, field := range keys {
		typ, ok := types[field]
		if !ok {
			return nil, fmt.Errorf("%w: %s matches on %s, not %q", ErrInvalid, h.Name, strings.Join(names, ", "), field)
		}
		c := cond{field: field, typ: typ}
		switch v := match[field].(type) {
		case string:
			if v == "" {
				return nil, fmt.Errorf("%w: %s: an exact value is a non-empty string", ErrInvalid, field)
			}
			c.equals = normalise(typ, v)
		case map[string]any:
			if err := compileObject(&c, v); err != nil {
				return nil, fmt.Errorf("%w: %s: %v", ErrInvalid, field, err)
			}
		default:
			return nil, fmt.Errorf("%w: %s: a value is a string, {\"in\": [...]}, {\"prefix\": \"...\"} or {\"gte\": \"...\", \"lte\": \"...\"}", ErrInvalid, field)
		}
		if typ == TypeDecimal && c.equals != "" {
			if _, ok := new(big.Rat).SetString(c.equals); !ok {
				return nil, fmt.Errorf("%w: %s: %q is not a decimal", ErrInvalid, field, c.equals)
			}
		}
		out = append(out, c)
	}
	return out, nil
}

func compileObject(c *cond, v map[string]any) error {
	for k := range v {
		switch k {
		case "in", "prefix", "gte", "lte":
		default:
			return fmt.Errorf("%q is not one of in, prefix, gte, lte", k)
		}
	}
	if raw, ok := v["in"]; ok {
		list, err := stringList(raw)
		if err != nil || len(list) == 0 {
			return fmt.Errorf("in is a non-empty list of strings")
		}
		for _, s := range list {
			c.in = append(c.in, normalise(c.typ, s))
		}
	}
	if raw, ok := v["prefix"]; ok {
		p, ok := raw.(string)
		if !ok || p == "" {
			return fmt.Errorf("prefix is a non-empty string")
		}
		c.prefix = normalise(c.typ, p)
	}
	for _, bound := range []string{"gte", "lte"} {
		raw, ok := v[bound]
		if !ok {
			continue
		}
		if c.typ != TypeDecimal {
			return fmt.Errorf("%s is for decimal fields", bound)
		}
		str, ok := raw.(string)
		r, ok2 := new(big.Rat).SetString(str)
		if !ok || !ok2 {
			return fmt.Errorf("%s is a decimal string, like \"100.00\"", bound)
		}
		if bound == "gte" {
			c.gte = r
		} else {
			c.lte = r
		}
	}
	if len(c.in) == 0 && c.prefix == "" && c.gte == nil && c.lte == nil {
		return fmt.Errorf("an object value needs in, prefix, gte or lte")
	}
	return nil
}

func stringList(raw any) ([]string, error) {
	switch v := raw.(type) {
	case []string:
		return v, nil
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			s, ok := x.(string)
			if !ok {
				return nil, errors.New("not a string")
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, errors.New("not a list")
}

func normalise(typ, v string) string {
	if typ == TypeIBAN {
		return strings.ToUpper(strings.ReplaceAll(v, " ", ""))
	}
	return v
}

func matches(conds []cond, fields map[string]string) bool {
	for _, c := range conds {
		got, ok := fields[c.field]
		if !ok {
			return false
		}
		got = normalise(c.typ, got)
		switch {
		case c.equals != "":
			if c.typ == TypeDecimal {
				a, ok1 := new(big.Rat).SetString(got)
				b, _ := new(big.Rat).SetString(c.equals)
				if !ok1 || a.Cmp(b) != 0 {
					return false
				}
			} else if c.typ == TypePath {
				if !pathMatches(c.equals, got) {
					return false
				}
			} else if got != c.equals {
				return false
			}
		case len(c.in) > 0:
			found := false
			for _, v := range c.in {
				if v == got {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		if c.prefix != "" && !strings.HasPrefix(got, c.prefix) {
			return false
		}
		if c.gte != nil || c.lte != nil {
			r, ok := new(big.Rat).SetString(got)
			if !ok || (c.gte != nil && r.Cmp(c.gte) < 0) || (c.lte != nil && r.Cmp(c.lte) > 0) {
				return false
			}
		}
	}
	return true
}

func validatePath(h Hook, path []Step) error {
	g := h.Graph
	if g == nil {
		return fmt.Errorf("%w: %s has no graph", ErrInvalid, h.Name)
	}
	prev := g.Start
	for i, st := range path {
		if st.To == "" {
			return fmt.Errorf("%w: step %d has no \"to\"", ErrInvalid, i+1)
		}
		if !reachable(g, prev, st.To) {
			return fmt.Errorf("%w: %s cannot follow %s%s", ErrInvalid, st.To, prev, nexts(g, prev))
		}
		if st.After != "" && st.AfterLab != "" {
			return fmt.Errorf("%w: step %d has both after and after_lab; one is the wait", ErrInvalid, i+1)
		}
		for _, d := range []string{st.After, st.AfterLab} {
			if d == "" {
				continue
			}
			if v, err := parseDuration(d); err != nil || v < 0 {
				return fmt.Errorf("%w: step %d: %q is not a duration like \"10s\", \"2h\" or \"1d\"", ErrInvalid, i+1, d)
			}
		}
		allowed := g.Params[st.To]
		for p := range st.Params() {
			if !contains(allowed, p) {
				if len(allowed) == 0 {
					return fmt.Errorf("%w: %s takes no parameters, not %s", ErrInvalid, st.To, p)
				}
				return fmt.Errorf("%w: %s takes %s, not %s", ErrInvalid, st.To, strings.Join(allowed, ", "), p)
			}
		}
		prev = st.To
	}
	return nil
}

func nexts(g *Graph, from string) string {
	n := g.Next[from]
	if len(n) == 0 {
		return " (" + from + " is final)"
	}
	return " (it is followed by " + strings.Join(n, ", ") + ")"
}

func reachable(g *Graph, from, to string) bool {
	if !g.Walks {
		return contains(g.Next[from], to)
	}
	seen := map[string]bool{from: true}
	queue := []string{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, n := range g.Next[cur] {
			if n == to {
				return true
			}
			if !seen[n] {
				seen[n] = true
				queue = append(queue, n)
			}
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// parseDuration reads Go durations ("10s", "2h") and days ("1d", "1d12h").
func parseDuration(s string) (time.Duration, error) {
	var days time.Duration
	if i := strings.Index(s, "d"); i > 0 {
		var n int
		if _, err := fmt.Sscanf(s[:i], "%d", &n); err != nil {
			return 0, err
		}
		days = time.Duration(n) * 24 * time.Hour
		s = s[i+1:]
		if s == "" {
			return days, nil
		}
	}
	d, err := time.ParseDuration(s)
	return days + d, err
}
