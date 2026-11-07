package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/wlsftp"
	"github.com/webduvet/fintechlab-simple/internal/worldline"
)

// settlementApp is mock-Worldline's acquiring side: the transactions it has
// acquired, the daily cycle that turns them into settlement files, and the
// lump sum it wires to the platform's safeguarding account when it does.
//
// This is the half that used to live on the platform. Worldline is the one
// that knows what was acquired, so Worldline is the one that cuts the file
// and moves the money; the platform's only way to learn either is to
// download the file and watch its account.
type settlementApp struct {
	store *worldline.Store
	cfg   worldline.CycleConfig
	root  string
	keys  *wlsftp.PGPKeys

	// sgaURL is Banking Circle's internal listener. Worldline does not
	// talk to Banking Circle in real life -- the lump sum simply appears
	// in the platform's safeguarding account, wired bank-to-bank. This
	// lab has no interbank rail, so the acquirer credits the account
	// directly: it is the arrival of the money that is being simulated,
	// not the mechanism that delivers it.
	sgaURL     string
	sgaAccount map[string]string // currency -> Banking Circle account id
	httpClient *http.Client

	// guard drops a run that a reset lands in the middle of (reset.go).
	guard *runGuard

	mu   sync.Mutex
	runs []CycleRun
}

// CycleRun records one execution of the settlement cycle, so an operator
// (or the harness) can see what was cut without reading the SFTP root.
type CycleRun struct {
	At        time.Time           `json:"at"`
	Slot      string              `json:"slot"`
	FromDate  string              `json:"from_date"`
	ToDate    string              `json:"to_date"`
	Files     []worldline.CutFile `json:"files"`
	LumpSums  []LumpSum           `json:"lump_sums"`
	Published []string            `json:"published"`
	Errors    []string            `json:"errors,omitempty"`
}

// LumpSum is one credit Worldline pushed to the platform's safeguarding
// account, matching the settlement total of the files in the same run.
type LumpSum struct {
	Currency    string `json:"currency"`
	AmountCents int64  `json:"amount_cents"`
	Reference   string `json:"reference"`
	AccountID   string `json:"account_id"`
	Status      string `json:"status"`
}

// run executes one settlement slot: cut the files, publish them
// PGP-encrypted onto the SFTP server, and -- for the morning slot only --
// wire the matching lump sum.
//
// `at` is the delivery time, which becomes the filename's timestamp; the
// coverage dates are separate. They have to be: a caller asking to settle
// a specific past day still delivers the file *now*, and deriving the
// timestamp from the coverage date instead produced byte-identical
// filenames on every run, so a consumer that had already taken delivery
// once ignored every later file as one it had seen.
//
// Only the morning slot moves money. The afternoon file is a confirmation
// of the same settlement, not a second one; paying twice because a
// confirmation arrived is exactly the bug a simulation should make
// impossible to write against.
func (s *settlementApp) run(slot string, at time.Time, fromDate, toDate string) CycleRun {
	// A reset that lands while this runs drops what is left of it: no file
	// is published, no wire sent and no run recorded after the reset.
	started := s.guard.start()
	run := CycleRun{At: at.UTC(), Slot: slot, FromDate: fromDate, ToDate: toDate}

	files, err := worldline.Cut(s.store, s.cfg, slot, fromDate, toDate, at)
	if err != nil {
		run.Errors = append(run.Errors, err.Error())
		if !s.guard.apply(started, func() { s.record(run) }) {
			return dropped(run)
		}
		return run
	}
	run.Files = files

	totals := map[string]int64{}
	for _, f := range files {
		var name string
		var err error
		if !s.guard.apply(started, func() { name, err = wlsftp.Publish(s.root, f.Filename, f.Bytes, s.keys) }) {
			return dropped(run)
		}
		if err != nil {
			run.Errors = append(run.Errors, err.Error())
			continue
		}
		run.Published = append(run.Published, name)
		totals[f.Currency] += f.AmountCents
	}

	if slot == worldline.SlotMorning {
		for ccy, amount := range totals {
			if amount <= 0 || ccy == "" {
				continue
			}
			var ls LumpSum
			if !s.guard.apply(started, func() { ls = s.wireLumpSum(ccy, amount, toDate) }) {
				return dropped(run)
			}
			run.LumpSums = append(run.LumpSums, ls)
		}
	}

	if !s.guard.apply(started, func() { s.record(run) }) {
		return dropped(run)
	}
	log.Printf("worldline: settlement cycle slot=%s coverage=%s..%s files=%d published=%d lump_sums=%d errors=%d",
		slot, fromDate, toDate, len(files), len(run.Published), len(run.LumpSums), len(run.Errors))
	return run
}

// dropped closes a run a reset landed in: what it did before the reset is
// on it, the rest never happens, and it is not kept in the history the
// reset just cleared.
func dropped(run CycleRun) CycleRun {
	log.Printf("worldline: settlement cycle slot=%s coverage=%s..%s dropped by a reset after publishing %d file(s) and sending %d lump sum(s)",
		run.Slot, run.FromDate, run.ToDate, len(run.Published), len(run.LumpSums))
	run.Errors = append(run.Errors, "worldline was reset while this run was in flight; the rest of it was dropped")
	return run
}

// wireLumpSum credits the platform's safeguarding account with the run's
// settlement total. Failure is recorded on the run and never fatal: a
// simulator whose file delivery stops because a downstream account service
// is down is less useful than one that delivers the file and tells you the
// money did not move.
func (s *settlementApp) wireLumpSum(currency string, amountCents int64, valueDate string) LumpSum {
	ls := LumpSum{
		Currency:    currency,
		AmountCents: amountCents,
		Reference:   fmt.Sprintf("WORLDLINE SETTLEMENT %s %s", currency, valueDate),
		AccountID:   s.sgaAccount[currency],
		Status:      "skipped",
	}
	if s.sgaURL == "" || ls.AccountID == "" {
		ls.Status = "not configured"
		return ls
	}
	// Banking Circle derives the safeguarding account from the currency
	// itself, so the request carries no account id -- sending one is
	// rejected as an unknown field. ls.AccountID is kept for the run
	// report, where it says which account the money is expected to land
	// in.
	body, err := json.Marshal(map[string]any{
		"currency":  currency,
		"amount":    formatMinor(amountCents),
		"reference": ls.Reference,
	})
	if err != nil {
		ls.Status = "error: " + err.Error()
		return ls
	}
	resp, err := s.httpClient.Post(s.sgaURL+"/internal/incoming-payments", "application/json", bytes.NewReader(body))
	if err != nil {
		ls.Status = "error: " + err.Error()
		log.Printf("worldline: lump sum %s %s to %s failed: %v", currency, ls.Reference, ls.AccountID, err)
		return ls
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		ls.Status = fmt.Sprintf("error: safeguarding account returned %s", resp.Status)
		log.Printf("worldline: lump sum %s rejected: %s", ls.Reference, resp.Status)
		return ls
	}
	ls.Status = "credited"
	log.Printf("worldline: lump sum credited %s %d to %s (%s)", currency, amountCents, ls.AccountID, ls.Reference)
	return ls
}

// formatMinor renders minor units as the major-unit decimal string the
// account service expects ("15000" -> "150.00").
func formatMinor(cents int64) string {
	neg := ""
	if cents < 0 {
		neg, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d", neg, cents/100, cents%100)
}

func (s *settlementApp) record(run CycleRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs = append(s.runs, run)
	// Keep the history bounded: this is an observation window, not a
	// journal, and an unbounded slice in a long-running simulator is a
	// slow leak nobody notices until a demo runs overnight.
	if len(s.runs) > 100 {
		s.runs = s.runs[len(s.runs)-100:]
	}
}

// clearRuns empties the run history, for a reset. It returns how many runs
// and lump sums the history held, and the name of every file those runs
// published.
func (s *settlementApp) clearRuns() (runs, lumpSums int, published map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	published = map[string]bool{}
	for _, r := range s.runs {
		lumpSums += len(r.LumpSums)
		for _, name := range r.Published {
			published[name] = true
		}
	}
	runs = len(s.runs)
	s.runs = nil
	return runs, lumpSums, published
}

func (s *settlementApp) history() []CycleRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CycleRun, len(s.runs))
	copy(out, s.runs)
	return out
}

// schedule sleeps until each slot's next fire time and runs it, forever.
// Times are on the lab clock (labclock): when the clock is moved past a
// slot, that slot's file is delivered, as it would have been once the
// business day reached it.
// The morning slot's fire time is jittered across its delivery window so a
// consumer cannot come to depend on an exact second -- real files do not
// land at 08:00:00.
func (s *settlementApp) schedule() {
	for {
		at, slot := s.cfg.NextFire(labclock.Now().In(time.Local))
		log.Printf("worldline: next settlement slot=%s at=%s (in %s)", slot, at.Format(time.RFC3339), at.Sub(labclock.Now()).Truncate(time.Second))
		// Wait may re-plan to an earlier slot if the clock is moved back, so
		// remember which slot each planned time belongs to.
		slots := map[int64]string{at.UnixNano(): slot}
		fired := labclock.Wait(func(now time.Time) time.Time {
			next, name := s.cfg.NextFire(now.In(time.Local))
			slots[next.UnixNano()] = name
			return next
		})
		slot = slots[fired.UnixNano()]
		now := labclock.Now().In(time.Local)
		fromDate, toDate := s.cfg.CoverageFor(now)
		s.run(slot, now, fromDate, toDate)
	}
}

// --- HTTP: the simulator's own control surface -----------------------
//
// Everything under /sim is this lab's, not Worldline's. It is how a test
// says "a card payment happened" or "it is tomorrow morning now" without
// waiting for a real clock. It is served on the control port, apart from
// the vendor's own (main.go, surfaces), so the real-shaped surface stays
// recognizable as the part you can point at a real host.

func (s *settlementApp) routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /sim/transactions", s.addTransactions)
	mux.HandleFunc("GET /sim/transactions", s.listTransactions)
	mux.HandleFunc("POST /sim/settlement-cycle/run", s.runCycle)
	mux.HandleFunc("GET /sim/settlement-cycle/runs", s.listRuns)
}

// addTransactions accepts either one transaction or an array of them, so
// seeding a scenario is one call rather than a loop.
func (s *settlementApp) addTransactions(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxUploadBytes))
	if err != nil {
		httputilx.Error(w, 400, "read body: "+err.Error())
		return
	}
	var batch []worldline.Transaction
	if err := json.Unmarshal(raw, &batch); err != nil {
		var one worldline.Transaction
		if err2 := json.Unmarshal(raw, &one); err2 != nil {
			httputilx.Error(w, 400, "body must be a transaction or an array of transactions: "+err.Error())
			return
		}
		batch = []worldline.Transaction{one}
	}
	stored := make([]worldline.Transaction, 0, len(batch))
	for _, t := range batch {
		saved, err := s.store.Add(t)
		if err != nil {
			httputilx.Error(w, 400, err.Error())
			return
		}
		stored = append(stored, saved)
	}
	httputilx.WriteJSON(w, 201, map[string]any{"accepted": len(stored), "transactions": stored})
}

func (s *settlementApp) listTransactions(w http.ResponseWriter, r *http.Request) {
	all := s.store.All()
	if mid := r.URL.Query().Get("mid"); mid != "" {
		from := r.URL.Query().Get("from")
		to := r.URL.Query().Get("to")
		if from == "" {
			from = "0000-00-00"
		}
		if to == "" {
			to = "9999-99-99"
		}
		all = s.store.Range(mid, from, to)
	}
	httputilx.WriteJSON(w, 200, map[string]any{"count": len(all), "transactions": all})
}

// runCycle fires a slot immediately. ?slot=morning|afternoon, and
// ?date=YYYY-MM-DD to settle a specific day rather than yesterday --
// without which a test would have to backdate its own transactions to
// match whatever "yesterday" happens to be when it runs.
func (s *settlementApp) runCycle(w http.ResponseWriter, r *http.Request) {
	slot, err := worldline.SlotName(r.URL.Query().Get("slot"))
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	// The file is delivered now regardless; ?date only chooses which day's
	// transactions it settles, so a test does not have to backdate its
	// seed data to whatever "yesterday" happens to be when it runs.
	at := labclock.Now().In(time.Local)
	fromDate, toDate := s.cfg.CoverageFor(at)
	if d := r.URL.Query().Get("date"); d != "" {
		if _, err := time.Parse("2006-01-02", d); err != nil {
			httputilx.Error(w, 400, fmt.Sprintf("date %q is not YYYY-MM-DD", d))
			return
		}
		fromDate, toDate = d, d
	}
	run := s.run(slot, at, fromDate, toDate)
	status := 200
	if len(run.Errors) > 0 {
		status = 207 // some files published, some did not: say so rather than pretending
	}
	httputilx.WriteJSON(w, status, run)
}

func (s *settlementApp) listRuns(w http.ResponseWriter, _ *http.Request) {
	runs := s.history()
	httputilx.WriteJSON(w, 200, map[string]any{"count": len(runs), "runs": runs})
}
