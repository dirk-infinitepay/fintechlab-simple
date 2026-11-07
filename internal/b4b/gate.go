package b4b

import (
	"sync"

	"github.com/webduvet/fintechlab-simple/internal/simctl"
)

// Gate is the epoch b4b's background work runs under, and what makes a
// reset cancel that work rather than race it (docs/lab-api/sim-control.md
// §6).
//
// Work that outlives a request -- a payment walking its phases on
// B4B_PROCESSING_DELAY, a callback and its retries, the bridge call into
// Banking Circle -- takes its epoch from Begin when it starts and runs each
// effect through Do. Hold, for a reset, waits for an effect already under
// way, clears, and moves the epoch: every later Do for work begun before it
// is refused. So nothing a reset removed is called back about afterwards,
// and nothing it waited for is still on the wire when it answers -- which
// is what lets the lab reset b4b before Banking Circle and know no bridge
// call lands behind it.
type Gate struct {
	mu    sync.RWMutex
	epoch simctl.Epoch

	countMu sync.Mutex
	running map[uint64]int // background work still running, by the epoch it began in
	total   int
}

// Begin runs register -- the work's record going into a store, or nil --
// and returns the epoch it happened in, for the work's later Do calls. The
// two are one step as far as a reset is concerned: a reset either clears
// what register stored and refuses the work, or comes after and keeps
// both.
func (g *Gate) Begin(register func()) uint64 {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if register != nil {
		register()
	}
	return g.epoch.Current()
}

// Do runs effect unless a reset happened since started, and reports
// whether it ran. A reset waits for an effect already running, so effect
// must not itself call Do, Begin or Go.
func (g *Gate) Do(started uint64, effect func()) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.epoch.Moved(started) {
		return false
	}
	effect()
	return true
}

// Go runs work in the background, counted as running until it returns. Work
// begun before a reset that has already happened is not started at all.
func (g *Gate) Go(started uint64, work func(started uint64)) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.epoch.Moved(started) {
		return
	}
	g.count(started, 1)
	go func() {
		defer g.count(started, -1)
		work(started)
	}()
}

func (g *Gate) count(epoch uint64, delta int) {
	g.countMu.Lock()
	defer g.countMu.Unlock()
	if g.running == nil {
		g.running = map[uint64]int{}
	}
	g.running[epoch] += delta
	if g.running[epoch] == 0 {
		delete(g.running, epoch)
	}
	g.total += delta
}

// Running is how much work started with Go is still running, including
// work a reset dropped that has not yet woken to find out.
func (g *Gate) Running() int {
	g.countMu.Lock()
	defer g.countMu.Unlock()
	return g.total
}

// Hold is a reset: it waits for the effects under way, runs clear with how
// much work started with Go since the last reset is still running -- what
// this reset drops -- and moves the epoch, with no effect running in
// between.
func (g *Gate) Hold(clear func(running int)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.countMu.Lock()
	running := g.running[g.epoch.Current()]
	g.countMu.Unlock()
	clear(running)
	g.epoch.Bump()
}
