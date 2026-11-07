package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/sftpgateway"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
	"github.com/webduvet/fintechlab-simple/internal/wlsftp"
	"github.com/webduvet/fintechlab-simple/internal/worldline"
)

const testMerchant = "GB00SIM0000000000003"

// newTestSim builds worldline on two temp folders: the SFTP root and the
// SFT channel's base. The PGP key is small, so the test stays fast.
func newTestSim(t *testing.T, bankingCircle string) *sim {
	t.Helper()
	root, base := t.TempDir(), t.TempDir()
	if err := wlsftp.EnsureLayout(root); err != nil {
		t.Fatal(err)
	}
	if err := ensureLayout(base); err != nil {
		t.Fatal(err)
	}
	ch := &sftpgateway.Channel{BaseDir: base}
	if err := seedDefaultConfig(ch, testMerchant); err != nil {
		t.Fatal(err)
	}
	own, err := openpgp.NewEntity("mock-Worldline", "test", "worldline@example.test", &packet.Config{RSABits: 1024})
	if err != nil {
		t.Fatal(err)
	}
	keys := &wlsftp.PGPKeys{Own: own}
	guard := &runGuard{}
	acq := &settlementApp{
		store:      worldline.NewStore(),
		cfg:        worldline.CycleConfig{Identifier: "Worldline_Settlement", Source: worldline.FileSourceGenerate},
		root:       root,
		keys:       keys,
		sgaURL:     bankingCircle,
		sgaAccount: map[string]string{"EUR": "sga-eur", "GBP": "sga-gbp"},
		httpClient: &http.Client{Timeout: 5 * time.Second},
		guard:      guard,
	}
	desk := newEnrolmentDesk(root, keys, 0, time.Second)
	desk.guard = guard
	return &sim{guard: guard, acq: acq, desk: desk, sftpLog: activity.New("sftp", "File exchange", ""), ch: ch, merchantID: testMerchant}
}

func writeFile(t *testing.T, p string, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// gate holds the first request it sees until released.
type gate struct {
	entered, release chan struct{}
	once             sync.Once
}

func newGate() *gate { return &gate{entered: make(chan struct{}), release: make(chan struct{})} }

func (g *gate) hold() {
	first := false
	g.once.Do(func() { first = true; close(g.entered) })
	if first {
		<-g.release
	}
}

// bankingCircleFake credits every lump sum and counts them; hold, when set,
// runs on each one first.
func bankingCircleFake(t *testing.T, hold func()) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var wires atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wires.Add(1)
		if hold != nil {
			hold()
		}
		w.WriteHeader(201)
	}))
	t.Cleanup(srv.Close)
	return srv, &wires
}

// boardOne puts a platform's enrolment batch in to_WLNORDIC/ and has the
// desk answer it, returning the batch and its receipt.
func boardOne(t *testing.T, w *sim) (batch, receipt string) {
	t.Helper()
	batch = writeFile(t, filepath.Join(w.acq.root, wlsftp.DirToWLNordic, "INFINITEENROLL_01.20260925.pgp"),
		`<enrollment><submerchant><pf_account_id>pf-1001</pf_account_id><legal_name>Acme</legal_name></submerchant></enrollment>`)
	if n, err := w.desk.process(true); err != nil || n != 1 {
		t.Fatalf("desk answered %d batch(es), %v; want 1", n, err)
	}
	receipt = filepath.Join(w.acq.root, wlsftp.DirFromWLNordic, "INFINITEENROLL_01.20260925.ack.pgp")
	if !exists(receipt) {
		t.Fatal("no receipt was written")
	}
	return batch, receipt
}

func resetScope(t *testing.T, w *sim, scope string) simctl.ResetResult {
	t.Helper()
	res, err := w.reset(context.Background(), simctl.ResetRequest{Scope: scope})
	if err != nil {
		t.Fatalf("reset (%s): %v", scope, err)
	}
	return res
}

func checkCleared(t *testing.T, got, want map[string]int) {
	t.Helper()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("cleared[%s] = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
}

// flow clears what a run made and keeps what the platform set up once. It
// deletes only the files it can prove it wrote; the SFTP root and ./sftp
// are shared with the platform and the host.
func TestFlowResetClearsARunAndKeepsEnrolment(t *testing.T) {
	bc, wires := bankingCircleFake(t, nil)
	w := newTestSim(t, bc.URL)
	root, base := w.acq.root, w.ch.BaseDir

	if _, err := w.acq.store.Add(worldline.Transaction{MID: testMerchant, Currency: "EUR", AmountCents: 15000, Date: "2026-09-24"}); err != nil {
		t.Fatal(err)
	}
	run := w.acq.run(worldline.SlotMorning, time.Date(2026, 9, 25, 9, 0, 0, 0, time.UTC), "2026-09-24", "2026-09-24")
	if len(run.Published) != 1 || len(run.LumpSums) != 1 || len(run.Errors) != 0 || wires.Load() != 1 {
		t.Fatalf("the run did not publish and wire: %+v", run)
	}
	published := run.Published[0]
	batch, receipt := boardOne(t, w)

	w.desk.rejects["pf-1002"] = "09"
	w.sftpLog.Record(activity.Event{Op: "sftp.session", Summary: "infinitepay connected over SFTP"})
	archived := writeFile(t, filepath.Join(base, "archive", "2026", "09", testMerchant+"_2026-09-24_EUR_WX.csv"), "x")
	foreign := []string{
		writeFile(t, filepath.Join(root, wlsftp.DirDownload, "notes.txt"), "x"),
		writeFile(t, filepath.Join(root, wlsftp.DirDownload, "20260925090000_Another_Identifier_ER_EUR.csv.pgp"), "x"),
		writeFile(t, filepath.Join(base, "archive", "2026", "09", "financial-report.pdf"), "x"),
		writeFile(t, filepath.Join(base, "outbound", testMerchant, "daily", testMerchant+"_2026-09-24_EUR_WX.csv"), "x"), // the settlement stand-in's
	}
	upload, err := w.ch.DropInbound(sftpgateway.CategoryOnboarding, testMerchant, "kyc.pdf", []byte("%PDF"))
	if err != nil {
		t.Fatal(err)
	}
	set := sftpgateway.DefaultConfig(base, testMerchant)
	set.CutOffTime = "18:00 CET"
	if err := w.ch.UpdateConfig(set); err != nil {
		t.Fatal(err)
	}

	res := resetScope(t, w, simctl.ScopeFlow)
	checkCleared(t, res.Cleared, map[string]int{
		"transactions": 1, "cycle_runs": 1, "lump_sums": 1, "settlement_files": 2,
		"archived_reports": 1, "activity": 1, "enrolment_rejections": 1,
	})
	for _, k := range []string{"enrolment_receipts", "answered_batches", "channel_config"} {
		if _, ok := res.Cleared[k]; ok {
			t.Errorf("flow cleared %s, which only all clears", k)
		}
	}
	if want := []string{"enrolment_receipts", "channel_config", "inbound_uploads"}; !slices.Equal(res.Kept, want) {
		t.Errorf("kept = %v, want %v", res.Kept, want)
	}

	for _, gone := range []string{
		filepath.Join(root, wlsftp.DirDownload, published),
		filepath.Join(root, wlsftp.DirToWLNordic, published),
		archived,
	} {
		if exists(gone) {
			t.Errorf("%s survived", gone)
		}
	}
	if len(w.acq.store.All()) != 0 || len(w.acq.history()) != 0 || len(w.sftpLog.Recent(10)) != 0 || len(w.desk.rejects) != 0 {
		t.Error("in-memory state survived the reset")
	}

	for _, kept := range append(foreign, batch, receipt, upload) {
		if !exists(kept) {
			t.Errorf("%s was removed", kept)
		}
	}
	if cfg, err := w.ch.GetConfig(); err != nil || cfg.CutOffTime != "18:00 CET" {
		t.Errorf("flow changed the channel config: %+v, %v", cfg, err)
	}
	if n, _ := w.desk.process(true); n != 0 {
		t.Error("flow forgot an answered batch: the desk answered it again")
	}
}

// all also forgets enrolment and puts the SFT channel back to its seed. The
// platform's own files stay, so a batch still waiting is answered again —
// with the same MIDs.
func TestAllResetAlsoForgetsEnrolmentAndRestoresTheChannel(t *testing.T) {
	w := newTestSim(t, "")
	root, base := w.acq.root, w.ch.BaseDir
	batch, receipt := boardOne(t, w)
	partial := writeFile(t, filepath.Join(root, wlsftp.DirFromWLNordic, "INFINITEENROLL_02.20260925.ack.pgp.partial"), "x")
	notOurs := writeFile(t, filepath.Join(root, wlsftp.DirFromWLNordic, "README"), "x")
	upload, err := w.ch.DropInbound(sftpgateway.CategoryCorrections, testMerchant, "fix.csv", []byte("a,b"))
	if err != nil {
		t.Fatal(err)
	}
	set := sftpgateway.DefaultConfig(base, testMerchant)
	set.RemittanceFrequency = "weekly"
	if err := w.ch.UpdateConfig(set); err != nil {
		t.Fatal(err)
	}

	res := resetScope(t, w, simctl.ScopeAll)
	checkCleared(t, res.Cleared, map[string]int{"answered_batches": 1, "enrolment_receipts": 2, "channel_config": 1})
	if want := []string{"enrolment_batches", "inbound_uploads", "keys"}; !slices.Equal(res.Kept, want) {
		t.Errorf("kept = %v, want %v", res.Kept, want)
	}
	if exists(receipt) || exists(partial) {
		t.Error("a receipt survived")
	}
	for _, kept := range []string{batch, upload, notOurs} {
		if !exists(kept) {
			t.Errorf("%s was removed", kept)
		}
	}
	if cfg, err := w.ch.GetConfig(); err != nil || !reflect.DeepEqual(cfg, sftpgateway.DefaultConfig(base, testMerchant)) {
		t.Errorf("the channel config is not back to its seed: %+v, %v", cfg, err)
	}

	if n, err := w.desk.process(true); err != nil || n != 1 || !exists(receipt) {
		t.Fatalf("the batch still waiting was not answered again: %d, %v", n, err)
	}
	if res := resetScope(t, w, simctl.ScopeAll); res.Cleared["channel_config"] != 0 {
		t.Errorf("a seed already in place was rewritten: %v", res.Cleared)
	}
}

// waitForWriter returns once a reset is waiting on the guard: a reader can
// no longer get in.
func waitForWriter(t *testing.T, g *runGuard) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for g.mu.TryRLock() {
		g.mu.RUnlock()
		if time.Now().After(deadline) {
			t.Fatal("the reset never started waiting")
		}
		runtime.Gosched()
	}
}

// A run in flight when the reset lands: the wire already on its way is
// waited for and lands before the reset; the other currency's wire never
// leaves, the run is not recorded, and its files are gone with the reset.
func TestARunInFlightWhenTheResetLandsHasNoLaterEffect(t *testing.T) {
	g := newGate()
	bc, wires := bankingCircleFake(t, g.hold)
	w := newTestSim(t, bc.URL)
	for _, ccy := range []string{"EUR", "GBP"} {
		if _, err := w.acq.store.Add(worldline.Transaction{MID: testMerchant, Currency: ccy, AmountCents: 10000, Date: "2026-09-24"}); err != nil {
			t.Fatal(err)
		}
	}

	ran := make(chan CycleRun, 1)
	go func() {
		ran <- w.acq.run(worldline.SlotMorning, time.Date(2026, 9, 25, 9, 0, 0, 0, time.UTC), "2026-09-24", "2026-09-24")
	}()
	<-g.entered // both files are published; the first wire is on its way

	type outcome struct {
		res simctl.ResetResult
		err error
	}
	reset := make(chan outcome, 1)
	go func() {
		res, err := w.reset(context.Background(), simctl.ResetRequest{Scope: simctl.ScopeFlow})
		reset <- outcome{res, err}
	}()
	waitForWriter(t, w.guard)
	select {
	case <-reset:
		t.Fatal("the reset finished while a wire was still on its way")
	default:
	}
	close(g.release)
	run := <-ran
	o := <-reset
	if o.err != nil {
		t.Fatal(o.err)
	}

	if n := wires.Load(); n != 1 {
		t.Fatalf("%d lump sum(s) wired, want only the one already on its way", n)
	}
	if len(run.Errors) == 0 || !strings.Contains(run.Errors[len(run.Errors)-1], "dropped") {
		t.Fatalf("the run does not say it was dropped: %v", run.Errors)
	}
	if len(w.acq.history()) != 0 {
		t.Fatal("a run dropped by the reset was recorded after it")
	}
	checkCleared(t, o.res.Cleared, map[string]int{"cycle_runs": 0, "settlement_files": 4, "transactions": 2})
	for _, name := range run.Published {
		if exists(filepath.Join(w.acq.root, wlsftp.DirDownload, name)) {
			t.Errorf("%s survived the reset", name)
		}
	}
}

// A batch the desk started answering before the reset gets no receipt from
// that answer; the desk's next sweep, after the reset, answers it.
func TestAnAnswerInFlightWhenTheResetLandsIsDropped(t *testing.T) {
	w := newTestSim(t, "")
	name := "INFINITEENROLL_03.20260925.pgp"
	path := writeFile(t, filepath.Join(w.acq.root, wlsftp.DirToWLNordic, name),
		`<enrollment><submerchant><pf_account_id>pf-3</pf_account_id></submerchant></enrollment>`)
	receipt := filepath.Join(w.acq.root, wlsftp.DirFromWLNordic, worldline.ReceiptFilename(3, "20260925"))

	started := w.guard.start()
	resetScope(t, w, simctl.ScopeFlow)
	if err := w.desk.answer(name, path, started); !errors.Is(err, errReset) {
		t.Fatalf("answer after the reset: %v, want errReset", err)
	}
	if exists(receipt) || len(w.desk.seen) != 0 {
		t.Fatal("an answer dropped by the reset left a receipt or a record")
	}
	if n, err := w.desk.process(true); err != nil || n != 1 || !exists(receipt) {
		t.Fatalf("the sweep after the reset did not answer: %d, %v", n, err)
	}
}
