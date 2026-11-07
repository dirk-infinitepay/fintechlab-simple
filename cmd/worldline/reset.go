package main

// POST /sim/reset for worldline (docs/lab-api/sim-control.md §6;
// docs/api/services/worldline.md, Lab control).
//
// flow clears what a run makes: acquired transactions, the cycle-run
// history and the lump sums in it, the settlement files the cycle
// published, the reports the SFT channel archived on download, the armed
// enrolment rejections (traps by another name) and the activity log. It
// keeps what a platform set up once: enrolment receipts and the desk's
// record of what it answered (the MIDs it issued), and the SFT channel's
// configuration. all also forgets enrolment and puts the channel back to
// its seed.
//
// Every file a reset deletes is one this service wrote, recognised by its
// name in the directory it writes it to. The SFTP root and ./sftp are
// shared with the platform and the host; anything else in them stays.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/sftpgateway"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
	"github.com/webduvet/fintechlab-simple/internal/wlsftp"
	"github.com/webduvet/fintechlab-simple/internal/worldline"
)

// runGuard is what a reset and the work it must not race agree on. Work
// that outlives a request — a slot cutting and publishing its files, the
// lump-sum wire, the desk answering a batch — takes the epoch when it
// starts and puts each of its effects through apply. A reset holds every
// effect off while it clears, so an effect lands wholly before a reset or
// not at all: a wire already on its way when the reset arrives is waited
// for, and the run's next step is dropped. The schedules themselves keep
// running; only work that started before the reset is dropped.
//
// A nil guard applies everything, so a part built without one still works.
type runGuard struct {
	epoch simctl.Epoch
	mu    sync.RWMutex
}

// start is the epoch to remember when work begins.
func (g *runGuard) start() uint64 {
	if g == nil {
		return 0
	}
	return g.epoch.Current()
}

// moved reports whether a reset happened since started: a cheap check
// before work that has no effect of its own yet.
func (g *runGuard) moved(started uint64) bool {
	return g != nil && g.epoch.Moved(started)
}

// apply runs effect unless a reset happened since started, and reports
// whether it ran. Effects must not nest: effect may not call apply.
func (g *runGuard) apply(started uint64, effect func()) bool {
	if g == nil {
		effect()
		return true
	}
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

// sim is everything a reset of worldline reaches.
type sim struct {
	guard      *runGuard
	acq        *settlementApp
	desk       *enrolmentDesk
	sftpLog    *activity.Log
	ch         *sftpgateway.Channel
	merchantID string
}

// reset is worldline's simctl.ResetFunc. A file it cannot delete is
// returned as an error, answered 500, after everything else is done; a
// second reset finishes the job.
func (s *sim) reset(_ context.Context, req simctl.ResetRequest) (simctl.ResetResult, error) {
	cleared := map[string]int{}
	err := s.guard.resetting(func() error {
		var errs []error
		note := func(err error) {
			if err != nil {
				errs = append(errs, err)
			}
		}

		runs, lumpSums, published := s.acq.clearRuns()
		cleared["cycle_runs"], cleared["lump_sums"] = runs, lumpSums
		cleared["transactions"] = s.acq.store.Clear()

		n, err := s.removeSettlementFiles(published)
		cleared["settlement_files"] = n
		note(err)
		n, err = s.ch.ClearArchive()
		cleared["archived_reports"] = n
		note(err)

		cleared["activity"] = int(s.sftpLog.Clear())
		cleared["enrolment_rejections"] = s.desk.clearRejections()

		if req.Scope == simctl.ScopeAll {
			cleared["answered_batches"] = s.desk.forgetAnswered()
			n, err = wlsftp.RemoveFiles(s.acq.root, wlsftp.DirFromWLNordic, worldline.IsReceiptFilename)
			cleared["enrolment_receipts"] = n
			note(err)
			n, err = s.restoreChannel()
			cleared["channel_config"] = n
			note(err)
		}
		return errors.Join(errs...)
	})
	if err != nil {
		return simctl.ResetResult{}, err
	}
	kept := []string{"enrolment_receipts", "channel_config", "inbound_uploads"}
	if req.Scope == simctl.ScopeAll {
		// The platform's own files: its enrolment batches and the documents
		// it dropped. Nothing proves this service wrote them, so they stay.
		kept = []string{"enrolment_batches", "inbound_uploads", "keys"}
	}
	return simctl.ResetResult{Cleared: cleared, Kept: kept}, nil
}

// removeSettlementFiles deletes the settlement files the cycle published,
// from both download/ and to_WLNORDIC/: a name the cycle gives a file
// (generated, or a fixture's own) with .pgp appended, or one the run
// history says it published. The platform's enrolment batches beside them
// in to_WLNORDIC/ never match.
func (s *sim) removeSettlementFiles(published map[string]bool) (int, error) {
	cut, err := s.acq.cfg.CutNames()
	if err != nil {
		cut = func(string) bool { return false }
	}
	ours := func(name string) bool {
		if published[name] {
			return true
		}
		base, ok := strings.CutSuffix(name, ".pgp")
		return ok && cut(base)
	}
	var total int
	errs := []error{err}
	for _, dir := range []string{wlsftp.DirDownload, wlsftp.DirToWLNordic} {
		n, err := wlsftp.RemoveFiles(s.acq.root, dir, ours)
		total += n
		errs = append(errs, err)
	}
	return total, errors.Join(errs...)
}

// restoreChannel puts the SFT channel back to its seed: its folders, and the
// seeded configuration if it was changed. It returns 1 when it rewrote the
// configuration. The platform's inbound uploads are not touched.
func (s *sim) restoreChannel() (int, error) {
	if err := ensureLayout(s.ch.BaseDir); err != nil {
		return 0, err
	}
	if err := wlsftp.EnsureLayout(s.acq.root); err != nil {
		return 0, err
	}
	seed := sftpgateway.DefaultConfig(s.ch.BaseDir, s.merchantID)
	if cur, err := s.ch.GetConfig(); err == nil && reflect.DeepEqual(cur, seed) {
		return 0, nil
	}
	if err := s.ch.UpdateConfig(seed); err != nil {
		return 0, err
	}
	return 1, nil
}
