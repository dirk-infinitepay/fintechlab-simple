package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/labswitch"
	"github.com/webduvet/fintechlab-simple/internal/settlement"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
	"github.com/webduvet/fintechlab-simple/internal/wlsftp"
	"github.com/webduvet/fintechlab-simple/internal/worldline"
)

func writeFile(t *testing.T, p string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func resetFlow(t *testing.T, a *app, p *worldlinePuller) simctl.ResetResult {
	t.Helper()
	res, err := a.reset(p)(context.Background(), simctl.ResetRequest{Scope: simctl.ScopeFlow})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	return res
}

// A reset clears what a run made — records, the state file, the report
// files, the archive of Worldline's files and the pull history — and keeps
// the switch. It deletes only files it can prove it wrote: the folders are
// shared with Worldline and the host.
func TestResetClearsWhatTheStandInMadeAndKeepsTheRest(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)
	a.store.Path = filepath.Join(dir, "data", "settlements.json")
	a.wiring = labswitch.New("settlement", "test", true)
	a.wiring.Set(false, "a platform is plugged in")

	for _, id := range []string{"set_a", "set_b"} {
		if err := a.store.Create(&settlement.SettlementRecord{ID: id, MerchantID: "GB00SIM0000000000003", TransactionsDate: "2026-09-03"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.stageReport(sampleTestReport()); err != nil { // csv, xml and json, in out/ and outbound/
		t.Fatal(err)
	}

	archive := filepath.Join(dir, "data", "worldline")
	generated := writeFile(t, filepath.Join(archive, "20260925083412_Worldline_Settlement_ER_EUR.csv.pgp"))
	fixture := writeFile(t, filepath.Join(archive, "bank_export_ER_1.csv.pgp")) // only the history proves it
	p := &worldlinePuller{app: a, archiveDir: archive, seen: map[string]bool{}}
	p.pulled = []pulledFile{
		{Name: filepath.Base(generated), ArchivedAt: generated},
		{Name: filepath.Base(fixture), ArchivedAt: fixture},
	}
	p.seen[filepath.Base(generated)], p.seen[filepath.Base(fixture)] = true, true

	foreign := []string{
		writeFile(t, filepath.Join(dir, "out", "GB00SIM0000000000003", "notes.txt")),
		writeFile(t, filepath.Join(dir, "outbound", "GB00SIM0000000000003", "daily", "GB00SIM0000000000004_2026-09-03_EUR_WX.csv")),
		writeFile(t, filepath.Join(archive, "dropped-by-hand.csv")),
	}

	// Through the contract, as the lab API calls it.
	mux := http.NewServeMux()
	simctl.Mount(mux, simctl.Config{Service: "settlement", Kind: "stand-in", Reset: a.reset(p)})
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/sim/reset", strings.NewReader(`{"scope": "all"}`)))
	if rr.Code != 200 {
		t.Fatalf("POST /sim/reset: %d %s", rr.Code, rr.Body)
	}
	var got struct {
		Cleared map[string]int `json:"cleared"`
		Kept    []string       `json:"kept"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"settlements": 2, "report_files": 6, "archived_files": 2, "pulled_files": 2}
	for k, v := range want {
		if got.Cleared[k] != v {
			t.Errorf("cleared[%s] = %d, want %d (all: %v)", k, got.Cleared[k], v, got.Cleared)
		}
	}
	if !slices.Equal(got.Kept, []string{"connected"}) {
		t.Errorf("kept = %v, want [connected]", got.Kept)
	}

	if n := len(a.store.List()); n != 0 {
		t.Errorf("%d record(s) survived", n)
	}
	reloaded := settlement.NewStore()
	if err := reloaded.Load(a.store.Path); err != nil || len(reloaded.List()) != 0 {
		t.Errorf("the state file still holds records (%v)", err)
	}
	if files, _ := a.stager.ListOut(); len(files) != 1 { // notes.txt
		t.Errorf("SFTP_OUT_DIR holds %d file(s), want only the one the stand-in did not write", len(files))
	}
	for _, f := range []string{"csv", "xml", "json"} {
		report := filepath.Join(dir, "outbound", "GB00SIM0000000000003", "daily", "GB00SIM0000000000003_2026-09-03_EUR_WX."+f)
		if _, err := os.Stat(report); !os.IsNotExist(err) {
			t.Errorf("outbound report %s survived", filepath.Base(report))
		}
	}
	for _, f := range []string{generated, fixture} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("archived %s survived", filepath.Base(f))
		}
	}
	for _, f := range foreign {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s, which the stand-in did not write, was removed: %v", f, err)
		}
	}
	if len(p.history()) != 0 || len(p.seen) != 0 {
		t.Errorf("the puller still remembers %d file(s) and has seen %d", len(p.history()), len(p.seen))
	}
	if a.wiring.Connected() {
		t.Error("the reset reconnected the stand-in: the switch is live wiring and stays")
	}
}

// The archive was what said a file had been taken. With it gone, a file
// still on Worldline's server from before the reset would be taken again and
// paid twice; it is not. One Worldline publishes after the reset is.
func TestFilesFromBeforeAResetAreNotTakenAgain(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	p := &worldlinePuller{app: a, archiveDir: t.TempDir(), seen: map[string]bool{}}
	before := wlsftp.RemoteFile{Name: "20260925083412_Worldline_Settlement_ER_EUR.csv.pgp", ModTime: time.Now().Add(-time.Hour)}
	if p.predatesReset(before) {
		t.Fatal("with no reset yet, a file was held back")
	}

	now := time.Now()
	if _, _, err := p.reset("Worldline_Settlement", now); err != nil {
		t.Fatal(err)
	}
	if !p.predatesReset(before) || p.claim(before.Name) {
		t.Fatal("a file from before the reset would be taken again")
	}
	after := wlsftp.RemoteFile{Name: "20260925093000_Worldline_Settlement_ER_EUR.csv.pgp", ModTime: now.Truncate(time.Second)}
	if p.predatesReset(after) || !p.claim(after.Name) {
		t.Fatal("a file published in the second of the reset was not taken")
	}
}

// gate is an HTTP fake that holds its first request until released.
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

// b4bCounter is a B4B fake that answers everything and counts the calls.
func b4bCounter(t *testing.T, payment func()) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "M", "account_name": "M", "account_number": "1", "sanctions_status": "pass"})
			return
		}
		if payment != nil {
			payment()
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "b4bp_1", "status": "B4BAccepted"})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// A batch waiting on the bank's ledger when the reset lands makes no record
// and writes no file once the ledger answers.
func TestABatchInFlightWhenTheResetLandsMakesNothing(t *testing.T) {
	g := newGate()
	bank := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/accounts":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": []map[string]string{{"id": "acc", "iban": "GB00SIM0000000000003", "currency": "EUR"}}})
		case "/ledger":
			g.hold()
			_ = json.NewEncoder(w).Encode(map[string]any{"entries": []map[string]string{{"accountId": "acc", "amount": "100.00", "currency": "EUR", "at": "2026-09-03T10:00:00Z"}}})
		}
	}))
	t.Cleanup(bank.Close)
	dir := t.TempDir()
	a := newTestApp(t, dir)
	a.bankURL = bank.URL

	type result struct {
		ids []string
		err error
	}
	done := make(chan result, 1)
	go func() {
		ids, err := a.runBatch("2026-09-03", "2026-09-03", "")
		done <- result{ids, err}
	}()
	<-g.entered
	resetFlow(t, a, nil)
	close(g.release)
	r := <-done

	if r.err != nil || len(r.ids) != 0 {
		t.Fatalf("runBatch after the reset: ids %v, err %v; want none", r.ids, r.err)
	}
	if n := len(a.store.List()); n != 0 {
		t.Fatalf("%d record(s) made after the reset", n)
	}
	if files, _ := a.stager.ListOut(); len(files) != 0 {
		t.Fatalf("%d report file(s) written after the reset", len(files))
	}
	if _, err := os.Stat(filepath.Join(dir, "outbound", "GB00SIM0000000000003")); !os.IsNotExist(err) {
		t.Fatalf("an outbound report was written after the reset: %v", err)
	}
}

// A payout still in its gates when the reset lands is never sent: B4B hears
// nothing about a record the reset cleared.
func TestAPayoutInFlightWhenTheResetLandsIsNeverSent(t *testing.T) {
	g := newGate()
	bc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.hold()
		_ = json.NewEncoder(w).Encode(map[string]any{"balances": []map[string]string{{"intraDayAmount": "1000000.00"}}})
	}))
	t.Cleanup(bc.Close)
	b4b, calls := b4bCounter(t, nil)
	a := newTestApp(t, t.TempDir())
	a.bcInternalURL, a.b4bURL = bc.URL, b4b.URL
	rec := &settlement.SettlementRecord{ID: "set_inflight", MerchantID: "GB00SIM0000000000003", Currency: "EUR", Report: sampleTestReport()}
	if err := a.store.Create(rec); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		a.submitPayout(rec, a.guard.start())
		close(done)
	}()
	<-g.entered
	res := resetFlow(t, a, nil)
	close(g.release)
	<-done

	if res.Cleared["settlements"] != 1 {
		t.Fatalf("cleared %v, want the one record", res.Cleared)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("B4B was called %d time(s) for a payout the reset dropped", n)
	}
	if n := len(a.store.List()); n != 0 {
		t.Fatalf("%d record(s) after the reset", n)
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

// A payment already on its way to B4B when the reset lands is waited for:
// it lands wholly before the reset, and the reset then clears its record.
func TestAResetWaitsForAPaymentAlreadyOnItsWay(t *testing.T) {
	g := newGate()
	b4b, calls := b4bCounter(t, g.hold)
	a := newTestApp(t, t.TempDir())
	a.b4bURL = b4b.URL
	rec := &settlement.SettlementRecord{ID: "set_onitsway", MerchantID: "GB00SIM0000000000003", Currency: "EUR", Report: sampleTestReport()}
	if err := a.store.Create(rec); err != nil {
		t.Fatal(err)
	}

	paid := make(chan struct{})
	go func() {
		a.submitPayout(rec, a.guard.start())
		close(paid)
	}()
	<-g.entered
	type outcome struct {
		res simctl.ResetResult
		err error
	}
	reset := make(chan outcome, 1)
	go func() {
		res, err := a.reset(nil)(context.Background(), simctl.ResetRequest{Scope: simctl.ScopeFlow})
		reset <- outcome{res, err}
	}()
	waitForWriter(t, &a.guard)
	select {
	case <-reset:
		t.Fatal("the reset finished while a payment was still on its way")
	default:
	}
	close(g.release)
	<-paid
	o := <-reset
	if o.err != nil {
		t.Fatal(o.err)
	}
	res := o.res

	if n := calls.Load(); n != 2 { // the beneficiary, then the payment
		t.Fatalf("B4B was called %d time(s), want 2", n)
	}
	if res.Cleared["settlements"] != 1 || len(a.store.List()) != 0 {
		t.Fatalf("the reset did not clear the paid record: cleared %v, %d left", res.Cleared, len(a.store.List()))
	}
}

// A morning file being processed when the reset lands stops at the next
// MID: no record is made for it and nothing is paid.
func TestAFileInFlightWhenTheResetLandsStopsAtTheNextMID(t *testing.T) {
	var buf strings.Builder
	file := worldline.GenerateSettlementFile([]worldline.Transaction{
		{ID: "t1", MID: "MID-A", Currency: "EUR", AmountCents: 10000, Date: "2026-09-24"},
		{ID: "t2", MID: "MID-B", Currency: "EUR", AmountCents: 5000, Date: "2026-09-24"},
	}, "EUR", "2026-09-24", "2026-09-24", worldline.BamboraOptions{})
	if err := file.WriteCSV(&buf); err != nil {
		t.Fatal(err)
	}

	g := newGate()
	bc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.hold()
		_ = json.NewEncoder(w).Encode(map[string]any{"balances": []map[string]string{{"intraDayAmount": "1000000.00"}}})
	}))
	t.Cleanup(bc.Close)
	b4b, calls := b4bCounter(t, nil)
	a := newTestApp(t, t.TempDir())
	a.bcInternalURL, a.b4bURL = bc.URL, b4b.URL
	p := &worldlinePuller{app: a, archiveDir: t.TempDir(), seen: map[string]bool{}}

	type result struct {
		ids []string
		err error
	}
	done := make(chan result, 1)
	go func() {
		ids, _, err := p.process("20260925083412_Worldline_Settlement_ER_EUR.csv.pgp", []byte(buf.String()), a.guard.start())
		done <- result{ids, err}
	}()
	<-g.entered // MID-A is settled and asking about its funds
	res := resetFlow(t, a, p)
	close(g.release)
	r := <-done

	if !errors.Is(r.err, errReset) {
		t.Fatalf("process after the reset: %v, want errReset", r.err)
	}
	if len(r.ids) != 1 || res.Cleared["settlements"] != 1 {
		t.Fatalf("ids %v, cleared %v: want the one record made before the reset, and cleared by it", r.ids, res.Cleared)
	}
	if n := len(a.store.List()); n != 0 {
		t.Fatalf("%d record(s) made after the reset", n)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("B4B was called %d time(s) after the reset", n)
	}
}
