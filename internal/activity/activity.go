// Package activity is the recent-history buffer a vendor simulation keeps
// so an operator can see what the platform just did to it.
//
// The services already log to stdout, and stdout is the wrong surface
// during a test run: it is behind `podman logs`, it interleaves twelve
// containers, and it scrolls away. What an operator actually wants to know
// is narrow — did the payout call arrive, what did we answer, did the
// webhook go out, did anyone log in over SFTP — and wants it next to the
// service it is about.
//
// So each service keeps a small ring of structured events and serves them
// as JSON. Nothing is persisted: this is a window onto a run, not a record
// of one. The books of record are the services' own stores.
package activity

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// Status is the three-way verdict the console colours an event by. It maps
// onto the design system's data colours, which is why there are three of
// them and not a numeric level: green means the peer got what it asked
// for, amber means the vendor refused for a reason the caller can act on
// (a 422, a failed gate), red means something broke.
const (
	StatusOK   = "ok"
	StatusWarn = "warn"
	StatusBad  = "bad"
)

// Event is one thing that happened at this vendor's edge.
//
// Summary is written for a human reading a list at a glance, and Detail
// carries the identifiers they would then want to copy. Keeping them apart
// is what lets the console render one line per event and still have
// something to expand into.
type Event struct {
	Seq int64     `json:"seq"`
	At  time.Time `json:"at"`
	// Env is the environment the event belongs to (internal/tenant): the
	// customer whose call it was, or whose payment it is about. Empty is
	// the default environment's.
	Env     string            `json:"env,omitempty"`
	Op      string            `json:"op"`
	Peer    string            `json:"peer,omitempty"`
	Summary string            `json:"summary"`
	Status  string            `json:"status"`
	Detail  map[string]string `json:"detail,omitempty"`
	// Steps break a long event into its parts — a settlement run into its
	// stages — each with its own status and times. The console draws them
	// as a table under the event (design-system.md, Activity panel).
	Steps []Step `json:"steps,omitempty"`
}

// Step is one part of an event. Status is an event status, or "running"
// for a step that has started and not finished. Times are RFC 3339; a step
// without FinishedAt is still going (or never finished).
type Step struct {
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Note       string     `json:"note,omitempty"`
}

// Log is a fixed-size ring of events per environment, safe for concurrent
// use.
//
// Fixed-size on purpose: a lab left running for a week must not grow a
// heap because nobody was watching. When it wraps, the oldest event goes —
// the interesting one is almost always the most recent. One ring per
// environment, so a busy environment never pushes another's history out
// (docs/lab-api/environments.md §7).
type Log struct {
	name  string
	title string
	note  string

	mu    sync.Mutex
	rings map[string]*ring
	seq   int64 // across every environment, so a merged read stays in order
}

// ring is one environment's events.
type ring struct {
	buf   []Event
	next  int // where the next event is written
	n     int // how many slots are in use
	total int64
}

// DefaultCapacity is how many events a log keeps per environment. The
// console shows 100; holding a little more means a burst during a
// settlement run does not push the thing you are looking for out from
// under you before you scroll.
const DefaultCapacity = 256

// New creates a log. The name is the stable key the console renders under;
// title and note are shown to the operator, so they are sentence case and
// say what this log is *for*.
func New(name, title, note string) *Log {
	return &Log{name: name, title: title, note: note, rings: map[string]*ring{}}
}

// Record adds an event. At and Seq are filled in here so a caller cannot
// accidentally record two events with the same sequence, or with a clock
// it read earlier.
func (l *Log) Record(e Event) {
	if l == nil {
		// A nil log is a service that was built without one. Recording into
		// it is a no-op rather than a panic, so instrumentation can be added
		// to a code path before every caller has been given a log.
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	env := tenant.Normalize(e.Env)
	if env == tenant.Default {
		e.Env = "" // the default's events read as they always did
	} else {
		e.Env = env
	}
	rg := l.rings[env]
	if rg == nil {
		rg = &ring{buf: make([]Event, DefaultCapacity)}
		l.rings[env] = rg
	}
	l.seq++
	rg.total++
	e.Seq = l.seq
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	if e.Status == "" {
		e.Status = StatusOK
	}
	rg.buf[rg.next] = e
	rg.next = (rg.next + 1) % len(rg.buf)
	if rg.n < len(rg.buf) {
		rg.n++
	}
}

// Clear empties the log, every environment's, and returns how many events
// it had recorded, so a reset can say what it cleared. Seq keeps counting:
// a reader that remembered a seq from before never mistakes a new event
// for an old one.
func (l *Log) Clear() int64 { return l.ClearIn(tenant.All) }

// ClearIn empties what scope names: one environment's ring, or every one.
func (l *Log) ClearIn(scope string) int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var cleared int64
	for env, rg := range l.rings {
		if tenant.InScope(scope, env) {
			cleared += rg.total
			delete(l.rings, env)
		}
	}
	return cleared
}

// Recent returns up to limit events from every environment, newest first.
func (l *Log) Recent(limit int) []Event { return l.RecentIn(tenant.All, limit) }

// RecentIn returns up to limit of scope's events, newest first.
func (l *Log) RecentIn(scope string, limit int) []Event {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	events, _, _ := l.collect(scope, limit)
	return events
}

// collect is scope's newest limit events (0: all it holds), how many it
// ever recorded and how many it still holds. l.mu is held.
func (l *Log) collect(scope string, limit int) ([]Event, int64, int) {
	var out []Event
	var total int64
	var kept int
	for env, rg := range l.rings {
		if !tenant.InScope(scope, env) {
			continue
		}
		total += rg.total
		kept += rg.n
		for i := 0; i < rg.n; i++ {
			// Walk backwards from the most recently written slot.
			out = append(out, rg.buf[(rg.next-1-i+len(rg.buf)*2)%len(rg.buf)])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq > out[j].Seq })
	if limit > 0 && limit < len(out) {
		out = out[:limit]
	}
	return out, total, kept
}

// Snapshot is one log as the console consumes it: enough in the header to
// be worth reading collapsed, and the events for when it is opened.
type Snapshot struct {
	Name   string  `json:"name"`
	Title  string  `json:"title"`
	Note   string  `json:"note,omitempty"`
	Total  int64   `json:"total"`
	Kept   int     `json:"kept"`
	Last   *Event  `json:"last,omitempty"`
	Events []Event `json:"events"`
}

// Snapshot renders the log, every environment's. Total counts everything
// ever recorded, Kept only what is still in the ring — an operator who sees
// 4000 total and 256 kept knows the list is a window, not the whole run.
func (l *Log) Snapshot(limit int) Snapshot { return l.SnapshotIn(tenant.All, limit) }

// SnapshotIn renders what scope names: one environment's events, or all.
func (l *Log) SnapshotIn(scope string, limit int) Snapshot {
	l.mu.Lock()
	events, total, kept := l.collect(scope, limit)
	snap := Snapshot{Name: l.name, Title: l.title, Note: l.note, Total: total, Kept: kept}
	l.mu.Unlock()
	snap.Events = events
	if len(events) > 0 {
		last := events[0]
		snap.Last = &last
	}
	if snap.Events == nil {
		// An empty array, never null: the console renders an empty state
		// from a list it can count, and null would make it render "loading"
		// forever on a service nothing has touched yet.
		snap.Events = []Event{}
	}
	return snap
}

// DefaultLimit is how many events a request returns when it does not ask.
const DefaultLimit = 100

// Handler serves the given logs, in the order passed, as
// {"logs": [snapshot, ...]}. A service with two distinct conversations
// worth watching — inbound calls and outbound webhooks, say — passes two
// logs and the console draws two panels. It serves the environment the
// call names (?env=, internal/tenant): the default's when it names none,
// every environment's for "*".
func Handler(logs ...*Log) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := DefaultLimit
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		scope := tenant.FromRequest(r)
		out := make([]Snapshot, 0, len(logs))
		for _, l := range logs {
			if l == nil {
				continue
			}
			out = append(out, l.SnapshotIn(scope, limit))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"logs": out})
	}
}

// StatusFor maps an HTTP status onto an event status.
//
// A 4xx is amber, not red: a vendor refusing a malformed payout or an
// unknown beneficiary is the simulation working. Red is reserved for the
// vendor itself failing, which is the line an operator scanning the list
// actually cares about.
func StatusFor(code int) string {
	switch {
	case code >= 500:
		return StatusBad
	case code >= 400:
		return StatusWarn
	default:
		return StatusOK
	}
}
