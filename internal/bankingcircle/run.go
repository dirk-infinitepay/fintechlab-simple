package bankingcircle

import (
	"context"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/simctl"
)

// Run is one run of the simulation: from start-up, or from a reset, to the
// next reset. It owns the epoch a reset moves (simctl.Epoch), and is what
// keeps work that outlives a request — a payment sleeping out
// PROCESSING_DELAY, a batch waiting on its flush timer, a delivery working
// through the eleven-step retry schedule, the credit to the beneficiary's
// bank — from landing in the run after the one it started in.
//
// Such work takes Current when it starts and does every effect through Do.
// Do runs the effect only while the run it started in is still the run,
// and holds off a reset until the effect is finished, so an effect is
// either wholly before a reset (and the reset clears what it did) or not
// done at all. A check followed by an effect outside Do is a race: the
// reset can land between the two.
//
// A reset first cancels the run's context, which wakes everything sleeping
// in Sleep and aborts a webhook or credit already on the wire, then waits
// for the effects in flight, then moves the epoch.
type Run struct {
	epoch simctl.Epoch
	// gate is held shared by an effect in Do and exclusively by a reset.
	// An effect never calls Do again while inside one: a reset waiting on
	// the gate would block the inner call, and the outer would never end.
	gate sync.RWMutex

	mu     sync.Mutex // ctx, cancel, and the epoch moving with them
	ctx    context.Context
	cancel context.CancelFunc
}

// NewRun starts the first run.
func NewRun() *Run {
	r := &Run{}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	return r
}

// ended is the context of a run that is over.
var ended = func() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}()

// Current is the run now, for work to remember when it starts.
func (r *Run) Current() uint64 { return r.epoch.Current() }

// Moved reports whether a reset has ended the run started began in.
func (r *Run) Moved(started uint64) bool { return r.epoch.Moved(started) }

// context is the context of the run started began in: live while that run
// is, cancelled from the moment a reset of it begins.
func (r *Run) context(started uint64) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.epoch.Moved(started) {
		return ended
	}
	return r.ctx
}

// Sleep waits d, or less if a reset ends the run first, and reports whether
// the run started began in is still going. Work that gets false drops what
// it was about to do.
func (r *Run) Sleep(started uint64, d time.Duration) bool {
	ctx := r.context(started)
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return false
		}
	}
	return ctx.Err() == nil
}

// Do runs effect if no reset has happened since started, and keeps a reset
// from happening until it returns. effect gets the run's context, cancelled
// as soon as a reset begins: anything it sends over the network goes with
// it, so a reset never waits on a slow subscriber. Do reports whether the
// effect ran.
func (r *Run) Do(started uint64, effect func(ctx context.Context)) bool {
	r.gate.RLock()
	defer r.gate.RUnlock()
	if r.epoch.Moved(started) {
		return false
	}
	effect(r.context(started))
	return true
}

// stop is the first half of a reset: the run's context is cancelled, so
// sleepers wake and sends in flight abort. Then the caller takes the gate.
func (r *Run) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancel()
}

// next moves the epoch and starts the next run's context. The caller holds
// the gate exclusively, and the locks of everything the reset clears, so
// the new run begins at one instant for all of them.
func (r *Run) next() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancel()
	r.epoch.Bump()
	r.ctx, r.cancel = context.WithCancel(context.Background())
}
