package bankingcircle

import (
	"context"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// Run is one run of the simulation: from start-up, or from a reset, to the
// next reset. It is what keeps work that outlives a request — a payment
// sleeping out PROCESSING_DELAY, a batch waiting on its flush timer, a
// delivery working through the eleven-step retry schedule, the credit to
// the beneficiary's bank — from landing in the run after the one it started
// in.
//
// Such work takes Current when it starts and does every effect through Do,
// naming the environment it belongs to (internal/tenant): the payment's, the
// subscription's. Do runs the effect only while the run it started in is
// still that environment's run, and holds off a reset of that environment
// until the effect is finished, so an effect is either wholly before a
// reset (and the reset clears what it did) or not done at all. A check
// followed by an effect outside Do is a race: the reset can land between
// the two.
//
// A reset names an environment, or every one (tenant.All). It first cancels
// that environment's context, which wakes everything of its sleeping in
// Sleep and aborts a webhook or credit of its already on the wire, then
// waits for its effects in flight, then moves the run on for it. Another
// environment's work carries on untouched: one platform's reset never drops
// another's payout.
//
// Current is one counter for the whole bank; every reset moves it. An
// environment's work is from before its last reset when it started before
// the counter moved for that reset, or for the last reset of every
// environment. An environment the bank has never seen needs no entry to be
// reset with all the others.
type Run struct {
	// all is held shared by every effect, and exclusively by a reset of
	// every environment. An effect never calls Do again while inside one: a
	// reset waiting on a gate would block the inner call, and the outer
	// would never end.
	all sync.RWMutex

	mu    sync.Mutex // seq, floor and envs
	seq   uint64     // the run now; every reset moves it
	floor uint64     // where the last reset of every environment moved it to
	envs  map[string]*envRun
}

// envRun is one environment's part of the run.
type envRun struct {
	// gate is held shared by the environment's effects, and exclusively by
	// a reset of it alone.
	gate   sync.RWMutex
	since  uint64 // where its last reset moved the run to
	ctx    context.Context
	cancel context.CancelFunc
}

// NewRun starts the first run.
func NewRun() *Run { return &Run{envs: map[string]*envRun{}} }

// envLocked is env's part of the run, made on first use. Caller holds r.mu.
func (r *Run) envLocked(env string) *envRun {
	env = tenant.Normalize(env)
	e := r.envs[env]
	if e == nil {
		e = &envRun{}
		e.ctx, e.cancel = context.WithCancel(context.Background())
		r.envs[env] = e
	}
	return e
}

// ended is the context of a run that is over.
var ended = func() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}()

// Current is the run now, for work to remember when it starts.
func (r *Run) Current() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seq
}

// Moved reports whether a reset has ended, for env, the run started began
// in.
func (r *Run) Moved(env string, started uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.movedLocked(env, started)
}

func (r *Run) movedLocked(env string, started uint64) bool {
	since := r.floor
	if e := r.envs[tenant.Normalize(env)]; e != nil && e.since > since {
		since = e.since
	}
	return started < since
}

// context is env's context in the run started began in: live while that
// run is, cancelled from the moment a reset of it begins.
func (r *Run) context(env string, started uint64) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.movedLocked(env, started) {
		return ended
	}
	return r.envLocked(env).ctx
}

// gate is env's gate.
func (r *Run) gate(env string) *sync.RWMutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	return &r.envLocked(env).gate
}

// Sleep waits d, or less if a reset of env ends the run first, and reports
// whether the run started began in is still env's. Work that gets false
// drops what it was about to do.
func (r *Run) Sleep(env string, started uint64, d time.Duration) bool {
	ctx := r.context(env, started)
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

// Do runs effect if no reset of env has happened since started, and keeps
// such a reset from happening until it returns. effect gets env's context,
// cancelled as soon as a reset of env begins: anything it sends over the
// network goes with it, so a reset never waits on a slow subscriber. Do
// reports whether the effect ran.
func (r *Run) Do(env string, started uint64, effect func(ctx context.Context)) bool {
	r.all.RLock()
	defer r.all.RUnlock()
	g := r.gate(env)
	g.RLock()
	defer g.RUnlock()
	if r.Moved(env, started) {
		return false
	}
	effect(r.context(env, started))
	return true
}

// stop is the first part of a reset of scope: the contexts of the
// environments it names are cancelled, so their sleepers wake and their
// sends in flight abort. Then the caller holds the gates (hold).
func (r *Run) stop(scope string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if tenant.Normalize(scope) == tenant.All {
		for _, e := range r.envs {
			e.cancel()
		}
		return
	}
	r.envLocked(scope).cancel()
}

// hold waits for the effects in flight that a reset of scope ends, and keeps
// new ones from starting, until release.
func (r *Run) hold(scope string) (release func()) {
	if tenant.Normalize(scope) == tenant.All {
		r.all.Lock()
		return r.all.Unlock
	}
	g := r.gate(scope)
	g.Lock()
	return g.Unlock
}

// next moves the run on for scope and starts its next contexts. The caller
// holds the gates (hold) and the locks of everything the reset clears, so
// the new run begins at one instant for all of them.
func (r *Run) next(scope string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	renew := func(e *envRun) {
		e.cancel()
		e.ctx, e.cancel = context.WithCancel(context.Background())
	}
	if tenant.Normalize(scope) == tenant.All {
		r.floor = r.seq
		for _, e := range r.envs {
			renew(e)
		}
		return
	}
	e := r.envLocked(scope)
	e.since = r.seq
	renew(e)
}
