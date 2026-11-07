package aci

import (
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/simctl"
)

// Record is one simulated notification: the plaintext payload (echoed back
// for introspection -- the lab already holds it, no need to decrypt its own
// ciphertext), the encrypted wire material, and how delivery went.
type Record struct {
	ID             string        `json:"id"`
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
// epoch their deliveries run under, so a reset cancels a delivery rather
// than racing it (docs/lab-api/sim-control.md §6).
//
// A delivery takes its epoch from Put, with its record, and runs each
// attempt through Do. Reset waits for an attempt already on the wire,
// clears the log and moves the epoch: every later attempt of a delivery
// put before it is refused. So no webhook is sent for a record a reset
// removed, and none is still in flight when the reset answers -- which is
// what lets the lab reset ACI before the receiver and know nothing lands
// behind it.
type SentLog struct {
	mu      sync.Mutex
	records map[string]*Record
	order   []string

	gate  sync.RWMutex // read: a delivery attempt; write: a reset
	epoch simctl.Epoch

	countMu sync.Mutex
	running map[uint64]int // deliveries still running, by the epoch their record was put in
	total   int
}

// NewSentLog returns an empty log.
func NewSentLog() *SentLog {
	return &SentLog{records: map[string]*Record{}, running: map[uint64]int{}}
}

// Put adds r and returns the epoch it was added in, for its delivery's Do
// calls. A reset either clears r and refuses its delivery, or comes after
// and keeps both.
func (s *SentLog) Put(r *Record) (started uint64) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[r.ID] = r
	s.order = append(s.order, r.ID)
	return s.epoch.Current()
}

// Go runs delivery in the background, counted as in flight until it
// returns, under the epoch its record was put in. A delivery whose record
// a reset already removed is not started at all.
func (s *SentLog) Go(started uint64, delivery func(started uint64)) {
	s.gate.RLock()
	defer s.gate.RUnlock()
	if s.epoch.Moved(started) {
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
	if s.epoch.Moved(started) {
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

// Get returns a copy of one record.
func (s *SentLog) Get(id string) (*Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if !ok {
		return nil, false
	}
	cp := *r
	return &cp, true
}

// List returns a copy of every record, oldest first.
func (s *SentLog) List() []*Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Record, 0, len(s.order))
	for _, id := range s.order {
		cp := *s.records[id]
		out = append(out, &cp)
	}
	return out
}

// Reset empties the log and drops every delivery in flight -- first
// attempts and retries alike -- and says how many of each there were; a
// delivery an earlier reset dropped is not counted again. It waits for an
// attempt already on the wire, so when it returns nothing it dropped is
// still being sent.
func (s *SentLog) Reset() (sent, deliveries int) {
	s.gate.Lock()
	defer s.gate.Unlock()
	s.mu.Lock()
	sent = len(s.records)
	s.records = map[string]*Record{}
	s.order = nil
	s.mu.Unlock()
	s.countMu.Lock()
	deliveries = s.running[s.epoch.Current()]
	s.countMu.Unlock()
	s.epoch.Bump()
	return sent, deliveries
}
