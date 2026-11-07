package console

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Health states. "unknown" means not probed yet, and is deliberately
// distinct from "down": a console that has been up for two seconds should
// not claim a service is broken when it simply has not looked yet.
const (
	StateUp      = "up"
	StateDown    = "down"
	StateUnknown = "unknown"
	// StateNotProbed is for entries with no health endpoint at all (the CA
	// is a one-shot script, not a server). Reporting those as "down"
	// forever would train an operator to ignore red.
	StateNotProbed = "not-probed"
)

// Status is the result of the most recent probe of one service.
type Status struct {
	ServiceID  string    `json:"service_id"`
	State      string    `json:"state"`
	HTTPStatus int       `json:"http_status,omitempty"`
	LatencyMS  int64     `json:"latency_ms,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	CheckedAt  time.Time `json:"checked_at"`
	// Since is when the service entered its current state -- the "up for
	// 4m" an operator actually reads, rather than the age of the last
	// probe, which is always a few seconds.
	Since time.Time `json:"since"`
	// History is the last few probe outcomes, oldest first, for the
	// sparkline. Bounded: this is a window, not a journal.
	History []bool `json:"history"`
}

const historyLen = 24

// Monitor probes the catalogue on an interval and serves the last result.
// Reads never block on the network: a page load that has to wait for ten
// health checks is a page load an operator learns to dread.
type Monitor struct {
	cat      *Catalogue
	client   func(s Service) *http.Client
	interval time.Duration
	timeout  time.Duration

	mu   sync.RWMutex
	last map[string]Status
}

// NewMonitor builds a monitor. clientFor picks the HTTP client per service
// so the mTLS-gated Banking Circle listener can be probed with a client
// certificate while everything else uses a plain one.
func NewMonitor(cat *Catalogue, clientFor func(Service) *http.Client, interval, timeout time.Duration) *Monitor {
	m := &Monitor{cat: cat, client: clientFor, interval: interval, timeout: timeout, last: map[string]Status{}}
	now := time.Now().UTC()
	for _, s := range cat.Services {
		st := Status{ServiceID: s.ID, State: StateUnknown, Since: now}
		if s.HealthPath == "" {
			st.State = StateNotProbed
			st.Detail = "no health endpoint"
		}
		m.last[s.ID] = st
	}
	return m
}

// Run probes everything immediately and then every interval until ctx ends.
func (m *Monitor) Run(ctx context.Context) {
	m.ProbeAll(ctx)
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.ProbeAll(ctx)
		}
	}
}

// ProbeAll probes every service concurrently and returns once all are done.
func (m *Monitor) ProbeAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, s := range m.cat.All() {
		// A plugin that unregistered itself has said it is gone; probing
		// it would only paint its card red for something nobody is running.
		if s.HealthPath == "" || (s.Plugin != nil && s.Plugin.State == PluginStopped) {
			continue
		}
		wg.Add(1)
		go func(s Service) {
			defer wg.Done()
			m.record(m.probe(ctx, s))
		}(s)
	}
	wg.Wait()
}

// Probe refreshes one service now and returns its new status.
func (m *Monitor) Probe(ctx context.Context, id string) (Status, bool) {
	s, ok := m.cat.Get(id)
	if !ok {
		return Status{}, false
	}
	if s.HealthPath == "" {
		return m.Get(id)
	}
	m.record(m.probe(ctx, s))
	return m.Get(id)
}

func (m *Monitor) probe(ctx context.Context, s Service) Status {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	st := Status{ServiceID: s.ID, CheckedAt: time.Now().UTC()}
	// A lab service is probed on its control port, which needs no vendor
	// credentials: the same process, so the same answer to "is it up". A
	// plugin has none, and is probed where it said.
	target := s.BaseURL + s.HealthPath
	if s.Control != "" {
		target = s.Control + "/health"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		st.State, st.Detail = StateDown, err.Error()
		return st
	}
	start := time.Now()
	resp, err := m.client(s).Do(req)
	st.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		st.State, st.Detail = StateDown, err.Error()
		return st
	}
	defer resp.Body.Close()
	st.HTTPStatus = resp.StatusCode
	// 2xx is up. Anything else is reported with its code rather than
	// collapsed into "down" without a reason -- a 401 from a credentialed
	// listener is a different problem from a refused connection, and an
	// operator needs to be able to tell them apart.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		st.State = StateUp
	} else {
		st.State, st.Detail = StateDown, fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return st
}

func (m *Monitor) record(st Status) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.last[st.ServiceID]
	st.History = append(append([]bool{}, prev.History...), st.State == StateUp)
	if len(st.History) > historyLen {
		st.History = st.History[len(st.History)-historyLen:]
	}
	st.Since = prev.Since
	if prev.State != st.State || st.Since.IsZero() {
		st.Since = st.CheckedAt
	}
	m.last[st.ServiceID] = st
}

// Get returns the last known status for a service.
func (m *Monitor) Get(id string) (Status, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st, ok := m.last[id]
	return st, ok
}

// All returns every status, ordered by service id so the UI does not
// reshuffle itself on every poll.
func (m *Monitor) All() []Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Status, 0, len(m.last))
	for _, st := range m.last {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ServiceID < out[j].ServiceID })
	return out
}
