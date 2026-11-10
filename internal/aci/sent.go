package aci

import (
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// Record is one simulated notification: the plaintext payload (echoed back
// for introspection -- the lab already holds it, no need to decrypt its own
// ciphertext), the encrypted wire material, and how delivery went.
type Record struct {
	ID string `json:"id"`
	// Env is the environment the notification is for (internal/tenant),
	// delivered to that environment's target. Empty is the default's.
	Env            string        `json:"env,omitempty"`
	Notification   *Notification `json:"notification"`
	CiphertextHex  string        `json:"ciphertext_hex"`
	IVHex          string        `json:"iv_hex"`
	TagHex         string        `json:"tag_hex"`
	TargetURL      string        `json:"target_url"`
	BodyMode       string        `json:"body_mode"`
	CreatedAt      time.Time     `json:"created_at"`
	DeliveryStatus string        `json:"delivery_status"`
	HTTPStatus     int           `json:"http_status,omitempty"`
	DeliveryError  string        `json:"delivery_error,omitempty"`
	DeliveredAt    *time.Time    `json:"delivered_at,omitempty"`
}

// SentLog is every notification ACI built (GET /sim/sent), and the
// epochs their deliveries run under, so a reset cancels a delivery rather
// than racing it (docs/lab-api/sim-control.md §6).
//
// A delivery takes its epoch from Put, with its record, and runs each
// attempt through Do. A reset waits for an attempt already on the wire,
// clears the log and moves the epoch: every later attempt of a delivery
// put before it is refused. So no webhook is sent for a record a reset
// removed, and none is still in flight when the reset answers -- which is
// what lets the lab reset ACI before the receiver and know nothing lands
// behind it.
//
// Each environment has an epoch of its own, so a reset of one never drops
// another's deliveries. No two epochs are ever the same number, whichever
// environment they are, so the epoch alone says whose a delivery is and
// whether it is still current.
type SentLog struct {
	mu      sync.Mutex
	records map[string]*Record
	order   []string
	epochs  map[string]uint64 // each environment's current epoch
	current map[uint64]bool   // the same, as a set
	last    uint64            // the last epoch handed out

	gate sync.RWMutex // read: a delivery attempt; write: a reset

	countMu sync.Mutex
	running map[uint64]int // deliveries still running, by the epoch their record was put in
	total   int
}

// NewSentLog returns an empty log.
func NewSentLog() *SentLog {
	return &SentLog{records: map[string]*Record{}, epochs: map[string]uint64{},
		current: map[uint64]bool{}, running: map[uint64]int{}}
}

// epochLocked is env's current epoch, a fresh one if it has none. s.mu is
// held.
func (s *SentLog) epochLocked(env string) uint64 {
	e, ok := s.epochs[env]
	if !ok {
		e = s.bumpLocked(env)
	}
	return e
}

// bumpLocked gives env a new epoch, ending its last. s.mu is held.
func (s *SentLog) bumpLocked(env string) uint64 {
	delete(s.current, s.epochs[env])
	s.last++
	s.epochs[env] = s.last
	s.current[s.last] = true
	return s.last
}

// moved reports whether a reset ended the epoch a delivery started in.
func (s *SentLog) moved(started uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.current[started]
}

// Put adds r and returns the epoch it was added in, its environment's, for
// its delivery's Do calls. A reset either clears r and refuses its
// delivery, or comes after and keeps both.
func (s *SentLog) Put(r *Record) (started uint64) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[r.ID] = r
	s.order = append(s.order, r.ID)
	return s.epochLocked(tenant.Normalize(r.Env))
}

// Go runs delivery in the background, counted as in flight until it
// returns, under the epoch its record was put in. A delivery whose record
// a reset already removed is not started at all.
func (s *SentLog) Go(started uint64, delivery func(started uint64)) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	if s.moved(started) {
		return
	}
	s.count(started, 1)
	go func() {
		defer s.count(started, -1)
		delivery(started)
	}()
}

func (s *SentLog) count(epoch uint64, delta int) {
	s.countMu.Lock()
	defer s.countMu.Unlock()
	s.running[epoch] += delta
	if s.running[epoch] == 0 {
		delete(s.running, epoch)
	}
	s.total += delta
}

// InFlight is how many deliveries are still running, including any a reset
// dropped that have not yet woken to find out.
func (s *SentLog) InFlight() int {
	s.countMu.Lock()
	defer s.countMu.Unlock()
	return s.total
}

// Do runs effect -- an attempt, or the record of how it went -- unless a
// reset happened since started, and reports whether it ran. effect must not
// call Put, Go, Do or Reset.
func (s *SentLog) Do(started uint64, effect func()) bool {
	s.gate.RLock()
	defer s.gate.RUnlock()
	if s.moved(started) {
		return false
	}
	effect()
	return true
}

// Update mutates the record in place under lock -- the delivering
// goroutine's record and the log's entry are the same pointer, so this is
// the only place delivery status is ever written after Put. An id the log
// no longer holds is left alone.
func (s *SentLog) Update(id string, fn func(*Record)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.records[id]; ok {
		fn(r)
	}
}

// Get returns a copy of one record, whichever environment's.
func (s *SentLog) Get(id string) (*Record, bool) { return s.GetIn(tenant.All, id) }

// GetIn returns a copy of one of scope's records.
func (s *SentLog) GetIn(scope, id string) (*Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if !ok || !tenant.InScope(scope, r.Env) {
		return nil, false
	}
	cp := *r
	return &cp, true
}

// List returns a copy of every record, oldest first.
func (s *SentLog) List() []*Record { return s.ListIn(tenant.All) }

// ListIn returns a copy of scope's records, oldest first: one
// environment's, or every one's for "*".
func (s *SentLog) ListIn(scope string) []*Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Record, 0, len(s.order))
	for _, id := range s.order {
		if r := s.records[id]; tenant.InScope(scope, r.Env) {
			cp := *r
			out = append(out, &cp)
		}
	}
	return out
}

// Reset is ResetIn for every environment.
func (s *SentLog) Reset() (sent, deliveries int) { return s.ResetIn(tenant.All) }

// ResetIn empties scope's part of the log and drops its deliveries in
// flight -- first attempts and retries alike -- and says how many of each
// there were; a delivery an earlier reset dropped is not counted again.
// Another environment's records and deliveries are left alone. It waits
// for an attempt already on the wire, so when it returns nothing it
// dropped is still being sent.
func (s *SentLog) ResetIn(scope string) (sent, deliveries int) {
	s.gate.Lock()
	defer s.gate.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	order := s.order[:0:0]
	for _, id := range s.order {
		if tenant.InScope(scope, s.records[id].Env) {
			delete(s.records, id)
			sent++
		} else {
			order = append(order, id)
		}
	}
	s.order = order
	var ended []uint64
	for env, e := range s.epochs {
		if tenant.InScope(scope, env) {
			ended = append(ended, e)
			s.bumpLocked(env)
		}
	}
	s.countMu.Lock()
	for _, e := range ended {
		deliveries += s.running[e]
	}
	s.countMu.Unlock()
	return sent, deliveries
}
