package main

// POST /sim/reset for the settlement stand-in (docs/lab-api/sim-control.md
// §6; docs/api/services/settlement.md, Lab control).
//
// Both scopes do the same: the stand-in has nothing a platform set up once
// except its switch, which is live wiring and stays. A reset clears the
// settlement records (and rewrites the state file empty), the internal
// report files, the archive of Worldline files it took delivery of and its
// pull history. Every file it deletes is one it wrote, recognised by its
// name in the folder it writes it to; the folders are shared with Worldline
// and the host, and anything else in them stays.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/settlement"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
	"github.com/webduvet/fintechlab-simple/internal/worldline"
)

// runGuard is what a reset and the work it must not race agree on. Work
// that outlives a request — a pull and the payouts it makes, the daily
// cutoff batch, a payout retried — takes the epoch when it starts and puts
// each of its effects through apply. A reset holds every effect off while
// it clears, so an effect lands wholly before a reset or not at all: a
// payout already on its way to B4B when the reset arrives is waited for,
// and the next one is dropped. The timers themselves keep running; only
// work that started before the reset is dropped.
type runGuard struct {
	epoch simctl.Epoch
	mu    sync.RWMutex
}

// start is the epoch to remember when work begins.
func (g *runGuard) start() uint64 { return g.epoch.Current() }

// moved reports whether a reset happened since started: a cheap check
// before work that has no effect of its own yet.
func (g *runGuard) moved(started uint64) bool { return g.epoch.Moved(started) }

// apply runs effect unless a reset happened since started, and reports
// whether it ran. Effects must not nest: effect may not call apply.
func (g *runGuard) apply(started uint64, effect func()) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.epoch.Moved(started) {
		return false
	}
	effect()
	return true
}

// resetting moves the epoch and runs clear with every effect held off.
func (g *runGuard) resetting(clear func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.epoch.Bump()
	return clear()
}

// errReset is work a reset dropped.
var errReset = errors.New("the stand-in was reset while this was in flight; the rest was dropped")

// reset is the stand-in's simctl.ResetFunc. A file it cannot delete is
// returned as an error, answered 500, after everything else is done; a
// second reset finishes the job.
func (a *app) reset(puller *worldlinePuller) simctl.ResetFunc {
	return func(context.Context, simctl.ResetRequest) (simctl.ResetResult, error) {
		cleared := map[string]int{}
		err := a.guard.resetting(func() error {
			var errs []error
			n, err := a.store.Clear()
			cleared["settlements"] = n
			errs = append(errs, err)

			out, err := settlement.RemoveReports(a.stager.OutDir, "")
			errs = append(errs, err)
			outbound, err := settlement.RemoveReports(a.outboundDir, "daily")
			errs = append(errs, err)
			cleared["report_files"] = out + outbound

			if puller != nil {
				pulled, archived, err := puller.reset(a.worldlineIdentifier, time.Now())
				cleared["pulled_files"], cleared["archived_files"] = pulled, archived
				errs = append(errs, err)
			}
			return errors.Join(errs...)
		})
		if err != nil {
			return simctl.ResetResult{}, err
		}
		return simctl.ResetResult{Cleared: cleared, Kept: []string{"connected"}}, nil
	}
}

// reset forgets what the puller took delivery of — its history and the
// names it has seen — and deletes the archived copies: files in
// WORLDLINE_ARCHIVE_DIR named as Worldline names a generated settlement
// file under identifier, or recorded in the history as archived there. It
// returns how many history entries and archived files it cleared.
//
// A file still on Worldline's server from before the reset is not taken
// again (predatesReset): taking it would pay its merchants a second time.
// Reset Worldline as well to start from no files at all.
func (p *worldlinePuller) reset(identifier string, now time.Time) (pulled, archived int, err error) {
	p.mu.Lock()
	recorded := map[string]bool{}
	for _, f := range p.pulled {
		if f.ArchivedAt != "" && filepath.Dir(f.ArchivedAt) == filepath.Clean(p.archiveDir) {
			recorded[filepath.Base(f.ArchivedAt)] = true
		}
	}
	pulled = len(p.pulled)
	p.pulled = nil
	p.seen = map[string]bool{}
	// SFTP carries modification times in whole seconds.
	p.notBefore = now.Truncate(time.Second)
	p.mu.Unlock()

	archived, err = settlement.RemoveFiles(p.archiveDir, func(name string) bool {
		if recorded[name] {
			return true
		}
		// Plain, as Worldline delivers them; .pgp from a lab before that.
		return worldline.IsCutFilename(identifier, strings.TrimSuffix(name, ".pgp"))
	})
	return pulled, archived, err
}
