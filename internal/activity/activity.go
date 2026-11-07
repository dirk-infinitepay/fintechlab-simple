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
	"strconv"
	"sync"
	"time"
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
	Seq     int64             `json:"seq"`
	At      time.Time         `json:"at"`
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

// Log is a fixed-size ring of events, safe for concurrent use.
//
// Fixed-size on purpose: a lab left running for a week must not grow a
// heap because nobody was watching. When it wraps, the oldest event goes —
// the interesting one is almost always the most recent.
type Log struct {
	name  string
	title string
	note  string

	mu    sync.Mutex
	ring  []Event
	next  int // where the next event is written
	n     int // how many slots are in use
	seq   int64
	total int64
}

// DefaultCapacity is how many events a log keeps. The console shows 100;
// holding a little more means a burst during a settlement run does not
// push the thing you are looking for out from under you before you scroll.
const DefaultCapacity = 256

// New creates a log. The name is the stable key the console renders under;
// title and note are shown to the operator, so they are sentence case and
// say what this log is *for*.
func New(name, title, note string) *Log {
	return &Log{name: name, title: title, note: note, ring: make([]Event, DefaultCapacity)}
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
	l.seq++
	l.total++
	e.Seq = l.seq
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	if e.Status == "" {
		e.Status = StatusOK
	}
	l.ring[l.next] = e
	l.next = (l.next + 1) % len(l.ring)
	if l.n < len(l.ring) {
		l.n++
	}
}

// Clear empties the log and returns how many events it had recorded, so a
// reset can say what it cleared. Seq keeps counting: a reader that
// remembered a seq from before never mistakes a new event for an old one.
func (l *Log) Clear() int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cleared := l.total
	l.ring = make([]Event, len(l.ring))
	l.next, l.n, l.total = 0, 0, 0
	return cleared
}

// Recent returns up to limit events, newest first.
func (l *Log) Recent(limit int) []Event {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 || limit > l.n {
		limit = l.n
	}
	out := make([]Event, 0, limit)
	for i := 0; i < limit; i++ {
		// Walk backwards from the most recently written slot.
		idx := (l.next - 1 - i + len(l.ring)*2) % len(l.ring)
		out = append(out, l.ring[idx])
	}
	return out
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

// Snapshot renders the log. Total counts everything ever recorded, Kept
// only what is still in the ring — an operator who sees 4000 total and 256
// kept knows the list is a window, not the whole run.
func (l *Log) Snapshot(limit int) Snapshot {
	events := l.Recent(limit)
	l.mu.Lock()
	snap := Snapshot{Name: l.name, Title: l.title, Note: l.note, Total: l.total, Kept: l.n}
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
// logs and the console draws two panels.
func Handler(logs ...*Log) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := DefaultLimit
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		out := make([]Snapshot, 0, len(logs))
		for _, l := range logs {
			if l == nil {
				continue
			}
			out = append(out, l.Snapshot(limit))
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
