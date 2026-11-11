package main

// POST /sim/reset for worldline (docs/lab-api/sim-control.md §6;
// docs/api/services/worldline.md, Lab control).
//
// flow clears what a run makes: acquired transactions, the cycle-run
// history and the lump sums in it, the settlement files the cycle and the
// publish hook (files.go) published, the reports the SFT channel archived
// on download, the armed enrolment rejections (traps by another name) and
// the activity log. It keeps what a platform set up once: enrolment
// receipts and the desk's record of what it answered (the MIDs it issued),
// and the SFT channel's configuration. all also forgets enrolment and puts
// the channel back to its seed.
//
// Every file a reset deletes is one this service wrote, recognised by its
// name in the directory it writes it to. The SFTP root and ./sftp are
// shared with the platform and the host; anything else in them stays.
//
// A reset names an environment (sim-control.md §8, rule 6): it clears that
// environment's records and files only, in its own jail, and "*" clears
// every environment's. The SFT channel over HTTP is the default's, so only
// a reset that reaches the default touches the archive and the channel
// configuration.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/sftpgateway"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
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
	// retired is an environment that was deleted: nothing of it applies
	// again, whenever it started.
	retired bool
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
	if g.retired || g.epoch.Moved(started) {
		return false
	}
	effect()
	return true
}

// resetting moves the epoch and runs clear with every effect held off.
func (g *runGuard) resetting(clear func() error) error {
	return resettingAll([]*runGuard{g}, clear)
}

// resettingAll is resetting over several guards at once: every
// environment a reset reaches. Only a reset takes more than one, resets
// run one at a time, and no effect waits on another guard, so the order
// cannot deadlock.
func resettingAll(gs []*runGuard, clear func() error) error {
	for _, g := range gs {
		g.mu.Lock()
		g.epoch.Bump()
	}
	defer func() {
		for _, g := range gs {
			g.mu.Unlock()
		}
	}()
	return clear()
}

// retire is a last reset: clear runs with every effect held off, and no
// effect applies after it.
func (g *runGuard) retire(clear func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.epoch.Bump()
	g.retired = true
	clear()
}

// sim is everything a reset of worldline reaches. guard, acq and desk are
// the default environment's; every other environment's are in tenants.
type sim struct {
	guard      *runGuard
	acq        *settlementApp
	desk       *enrolmentDesk
	sftpLog    *activity.Log
	ch         *sftpgateway.Channel
	merchantID string
	tenants    environments
}

// reset is worldline's simctl.ResetFunc. A file it cannot delete is
// returned as an error, answered 500, after everything else is done; a
// second reset finishes the job.
func (s *sim) reset(_ context.Context, req simctl.ResetRequest) (simctl.ResetResult, error) {
	scope := tenant.Normalize(req.Env)
	envs := s.in(scope)
	guards := make([]*runGuard, len(envs))
	for i, e := range envs {
		guards[i] = e.guard
	}
	cleared := map[string]int{}
	err := resettingAll(guards, func() error {
		var errs []error
		for _, e := range envs {
			errs = append(errs, s.clearEnv(e, req.Scope, cleared))
		}
		// An environment's activity is cleared with it; "*" takes every
		// ring the log has.
		cleared["activity"] = int(s.sftpLog.ClearIn(scope))
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

// clearEnv clears one environment's part of a reset, adding what it cleared
// to cleared. The caller holds the environment's guard.
func (s *sim) clearEnv(e *envSim, scope string, cleared map[string]int) error {
	var errs []error
	note := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	add := func(k string, n int) { cleared[k] += n }
	def := e.env == tenant.Default

	runs, lumpSums, published := e.acq.clearRuns()
	add("cycle_runs", runs)
	add("lump_sums", lumpSums)
	add("transactions", e.acq.store.Clear())

	n, err := removeSettlementFiles(e.acq, published)
	add("settlement_files", n)
	note(err)
	n = 0
	if def {
		// The SFT channel over HTTP is the default's alone.
		n, err = s.ch.ClearArchive()
		note(err)
	}
	add("archived_reports", n)
	add("enrolment_rejections", e.desk.clearRejections())

	if scope == simctl.ScopeAll {
		add("answered_batches", e.desk.forgetAnswered())
		n, err = wlsftp.RemoveFiles(e.acq.root, wlsftp.DirFromWLNordic, worldline.IsReceiptFilename)
		add("enrolment_receipts", n)
		note(err)
		n = 0
		if def {
			n, err = s.restoreChannel()
		} else {
			err = wlsftp.EnsureLayout(e.acq.root)
		}
		add("channel_config", n)
		note(err)
	}
	return errors.Join(errs...)
}

// removeSettlementFiles deletes the settlement files the cycle published,
// from both download/ and to_WLNORDIC/: a name the cycle gives a file
// (generated, or a fixture's own), or one the run history or the publish
// hook's record says was published -- also with the .pgp a lab before plain
// files appended to it. The platform's enrolment batches beside them in
// to_WLNORDIC/ never match.
func removeSettlementFiles(acq *settlementApp, published map[string]bool) (int, error) {
	cut, err := acq.config().CutNames()
	if err != nil {
		cut = func(string) bool { return false }
	}
	ours := func(name string) bool {
		if published[name] {
			return true
		}
		return cut(strings.TrimSuffix(name, ".pgp"))
	}
	var total int
	errs := []error{err}
	for _, dir := range []string{wlsftp.DirDownload, wlsftp.DirToWLNordic} {
		n, err := wlsftp.RemoveFiles(acq.root, dir, ours)
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
