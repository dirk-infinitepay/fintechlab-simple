// Settlement is this lab's stand-in for the platform side. It is
// scaffolding, not a vendor simulation and not a model of any real
// platform: it exists so the harness has something to prove the vendor
// simulations are actually connected to. See docs/catalogue.md.
//
// It pulls Worldline's settlement file over the real SFTP+PGP channel
// (worldline_pull.go), decrypts it, parses it, splits it per submerchant
// (MID), and submits a payout per outlet to B4B -- gated on the
// safeguarding-account balance, the beneficiary's sanctions status, and a
// merchant-verification check. It also keeps its own internal reports,
// reconciliation API and settlement state machine over the bank's ledger,
// which are the platform's own concern and drive no payouts.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/b4b"
	"github.com/webduvet/fintechlab-simple/internal/bankingcircle"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/labswitch"
	"github.com/webduvet/fintechlab-simple/internal/logring"
	"github.com/webduvet/fintechlab-simple/internal/money"
	"github.com/webduvet/fintechlab-simple/internal/settlement"
	"github.com/webduvet/fintechlab-simple/internal/sftp"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
	"github.com/webduvet/fintechlab-simple/internal/waitfor"
)

const maxRangeDays = 90

type app struct {
	store               *settlement.Store
	stager              *sftp.Stager
	outboundDir         string
	bankURL             string
	client              *http.Client // bank HTTP client
	worldlineIdentifier string       // WORLDLINE_SETTLEMENT_IDENTIFIER, used in Bambora filenames
	b4bURL              string
	b4bClient           *http.Client // B4B HTTP client, short timeout (best-effort)
	b4bPrivateKey       *rsa.PrivateKey
	b4bKeyID            string
	b4bCallbackURL      string
	verifyURL           string
	verifyClient        *http.Client // verify HTTP client, short timeout (best-effort)
	verifyKey           string       // x-internal-api-key sent to the verify mock
	bcInternalURL       string       // Banking Circle's plain INTERNAL_LISTEN, balance check only
	bcInternalClient    *http.Client
	sgaAccounts         map[string]string // currency -> Banking Circle SGA account id

	// wiring is what this stand-in does on its own — the Worldline pull on
	// a timer and the payouts at the daily cutoff. Off once a real platform
	// takes its place (internal/labswitch); an explicit pull or generate
	// still works.
	wiring *labswitch.Switch

	// guard drops work a reset lands in the middle of (reset.go).
	guard runGuard
}

func main() {
	logring.Install()
	addr := env("LISTEN", ":8083")
	labclock.FollowEnv(context.Background(), "settlement")
	bankURL := strings.TrimRight(env("BANK_URL", "http://bank:8081"), "/")
	sftpOutDir := env("SFTP_OUT_DIR", "/sftp/out")
	sftpStagingDir := env("SFTP_STAGING_DIR", "/sftp/staging")
	sftpOutboundDir := env("SFTP_OUTBOUND_DIR", "/sftp/outbound")
	stateFile := env("SETTLEMENT_STATE_FILE", "/data/settlements.json")
	cutoff := env("CUTOFF_TIME", "00:00")
	worldlineIdentifier := env("WORLDLINE_SETTLEMENT_IDENTIFIER", "Worldline_Settlement")
	b4bURL := strings.TrimRight(env("B4B_URL", "http://b4b:8086"), "/")
	b4bKeyID := env("B4B_JWT_KEY_ID", "b4b-mock-1")
	b4bCallbackURL := env("B4B_CALLBACK_URL", "http://settlement:8083/internal/b4b-webhook")
	b4bJWTKeyPath := env("B4B_JWT_PRIVATE_KEY_PATH", "/keys/b4b-keys/private.pem")
	verifyURL := strings.TrimRight(env("VERIFY_URL", "http://verify:8088"), "/")
	verifyKey := env("VERIFY_INTERNAL_API_KEY", "sim-verify-key-dev-only")
	// Not Banking Circle's mTLS+bearer surface -- its plain INTERNAL_LISTEN,
	// same trust boundary B4B's own bridge call already uses (docs/
	// ARCHITECTURE-phase3-corrections.md section 5: settlement never holds
	// Banking Circle credentials, matching real settle-processing).
	bcInternalURL := strings.TrimRight(env("BC_INTERNAL_URL", "http://banking-circle:8095"), "/")
	sgaAccounts := map[string]string{
		"EUR": env("BC_SAFEGUARDING_ACCOUNT_ID_EUR", bankingcircle.SGAAccountEUR),
		"GBP": env("BC_SAFEGUARDING_ACCOUNT_ID_GBP", bankingcircle.SGAAccountGBP),
	}

	// b4b generates this on its first start, concurrently with this one on
	// a fresh keys volume; wait for it rather than crash-looping until the
	// orchestrator's restart happens to land after it.
	if err := waitfor.Files(30*time.Second, b4bJWTKeyPath); err != nil {
		log.Fatalf("settlement: %v", err)
	}
	b4bPrivateKey, err := loadRSAPrivateKey(b4bJWTKeyPath)
	if err != nil {
		log.Fatalf("settlement: load B4B JWT private key %s: %v", b4bJWTKeyPath, err)
	}

	store := settlement.NewStore()
	if err := store.Load(stateFile); err != nil {
		log.Fatalf("settlement: load %s: %v", stateFile, err)
	}
	store.Path = stateFile

	a := &app{
		store:               store,
		stager:              &sftp.Stager{OutDir: sftpOutDir, StagingDir: sftpStagingDir},
		outboundDir:         sftpOutboundDir,
		bankURL:             bankURL,
		client:              &http.Client{Timeout: 10 * time.Second},
		worldlineIdentifier: worldlineIdentifier,
		b4bURL:              b4bURL,
		b4bClient:           &http.Client{Timeout: 5 * time.Second},
		b4bPrivateKey:       b4bPrivateKey,
		b4bKeyID:            b4bKeyID,
		b4bCallbackURL:      b4bCallbackURL,
		verifyURL:           verifyURL,
		verifyClient:        &http.Client{Timeout: 5 * time.Second},
		verifyKey:           verifyKey,
		bcInternalURL:       bcInternalURL,
		bcInternalClient:    &http.Client{Timeout: 5 * time.Second},
		sgaAccounts:         sgaAccounts,
		wiring: labswitch.FromEnv("settlement",
			"pulls Worldline's files on a timer and pays outlets at the daily cutoff",
			env("SETTLEMENT_CONNECTED", "true")),
	}

	go a.runCutoffLoop(cutoff)

	// Take delivery of Worldline's settlement files over the real SFTP+PGP
	// channel. This is the only route to them: nothing here reads the
	// acquirer's directories directly.
	puller := newWorldlinePuller(a)
	if puller != nil {
		go puller.run()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, map[string]string{"status": "ok", "service": "settlement"})
	})
	mux.HandleFunc("GET /reports/settlement", a.queryReports)
	mux.HandleFunc("GET /reports/settlement/{id}", a.getReport)
	mux.HandleFunc("GET /reports/settlement/{id}/payout", a.getPayout)
	mux.HandleFunc("POST /reports/settlement/generate", a.generate)
	mux.HandleFunc("POST /reports/settlement/{id}/transition", a.transition)
	mux.HandleFunc("POST /reports/settlement/{id}/retry-payout", a.retryPayout)
	mux.HandleFunc("GET /sftp/out", a.listOut)
	mux.HandleFunc("GET /sftp/out/{merchant_id}", a.listOutMerchant)
	mux.HandleFunc("GET /sftp/out/{merchant_id}/{filename}", a.readOut)
	mux.HandleFunc("GET /sftp/staging", a.listStaging)
	mux.HandleFunc("POST /internal/b4b-webhook", a.b4bWebhook)
	a.wiring.Routes(mux, "/sim/connected")
	if puller != nil {
		puller.routes(mux)
	}
	// GET /sim/info, GET /sim/logs and POST /sim/reset: the control
	// contract every sim serves (internal/simctl).
	simctl.Mount(mux, simctl.Config{
		Service: "settlement", Kind: "stand-in", Simulates: "the platform's settle path (scaffolding)",
		Listeners: []string{addr},
		Reset:     a.reset(puller),
	})
	log.Printf("settlement listening on %s bank=%s b4b=%s verify=%s banking_circle=%s sftp_out=%s sftp_outbound=%s",
		addr, bankURL, b4bURL, verifyURL, bcInternalURL, sftpOutDir, sftpOutboundDir)
	log.Fatal(http.ListenAndServe(addr, logReq(mux)))
}

// parseDateRange applies ARCHITECTURE-reconciliation-api.md's defaults:
// from_date defaults to 7 days ago, to_date defaults to today, and the
// resulting range may not exceed 90 days (matching Worldline's 3-month
// limit).
func parseDateRange(from, to string) (time.Time, time.Time, error) {
	now := labclock.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	fromDate := today.AddDate(0, 0, -7)
	toDate := today
	var err error
	if from != "" {
		fromDate, err = time.Parse("2006-01-02", from)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid from_date %q", from)
		}
	}
	if to != "" {
		toDate, err = time.Parse("2006-01-02", to)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid to_date %q", to)
		}
	}
	if toDate.Before(fromDate) {
		return time.Time{}, time.Time{}, fmt.Errorf("to_date before from_date")
	}
	if toDate.Sub(fromDate) > maxRangeDays*24*time.Hour {
		return time.Time{}, time.Time{}, fmt.Errorf("date range exceeds %d days", maxRangeDays)
	}
	return fromDate, toDate, nil
}

// recordReport builds the flattened Report view GET /reports/settlement(/{id})
// serves: rec.Report's aggregates plus the record's *live* state/settlement
// date (which may have moved on since the report snapshot was generated).
func recordReport(rec *settlement.SettlementRecord) *settlement.Report {
	var rep settlement.Report
	if rec.Report != nil {
		rep = *rec.Report
	} else {
		rep = settlement.Report{
			MerchantID:       rec.MerchantID,
			TransactionsDate: rec.TransactionsDate,
			Currency:         rec.Currency,
		}
	}
	rep.SettlementState = string(rec.State)
	rep.SettlementDate = rec.SettlementDate
	return &rep
}

func (a *app) queryReports(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	fromDate, toDate, err := parseDateRange(q.Get("from_date"), q.Get("to_date"))
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	fromStr, toStr := fromDate.Format("2006-01-02"), toDate.Format("2006-01-02")
	records := a.store.Query(fromStr, toStr, q.Get("merchant_id"))
	reports := make([]*settlement.Report, 0, len(records))
	for _, rec := range records {
		reports = append(reports, recordReport(rec))
	}
	writeReports(w, q.Get("format"), reports, map[string]any{
		"total":     len(reports),
		"from_date": fromStr,
		"to_date":   toStr,
	})
}

func (a *app) getReport(w http.ResponseWriter, r *http.Request) {
	rec, err := a.store.Get(r.PathValue("id"))
	if err != nil {
		httputilx.Error(w, 404, err.Error())
		return
	}
	writeReport(w, r.URL.Query().Get("format"), recordReport(rec))
}

// payoutResp is a lab-only introspection view of a record's best-effort
// payout submission -- not part of any real Worldline/B4B contract, purely
// so a test harness can observe PayoutID/PayoutState (which the vendor-
// shaped Report/CSV/XML output deliberately never carries) without
// re-deriving them from logs.
type payoutResp struct {
	PayoutID    string `json:"payout_id"`
	PayoutState string `json:"payout_state"`
}

func (a *app) getPayout(w http.ResponseWriter, r *http.Request) {
	rec, err := a.store.Get(r.PathValue("id"))
	if err != nil {
		httputilx.Error(w, 404, err.Error())
		return
	}
	httputilx.WriteJSON(w, 200, payoutResp{PayoutID: rec.PayoutID, PayoutState: rec.PayoutState})
}

// writeReports renders a multi-record response: JSON wraps reports in
// extra alongside "data", CSV/XML render every report's row/element under
// one header/root.
func writeReports(w http.ResponseWriter, format string, reports []*settlement.Report, extra map[string]any) {
	if format == "" {
		format = "json"
	}
	switch format {
	case "csv":
		data, err := settlement.ToCSVAll(reports)
		if err != nil {
			httputilx.Error(w, 500, err.Error())
			return
		}
		w.Header().Set("Content-Type", "text/csv")
		_, _ = w.Write(data)
	case "xml":
		data, err := settlement.ToXMLAll(reports)
		if err != nil {
			httputilx.Error(w, 500, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write(data)
	case "json":
		body := map[string]any{"data": reports}
		for k, v := range extra {
			body[k] = v
		}
		httputilx.WriteJSON(w, 200, body)
	default:
		httputilx.Error(w, 400, "unsupported format "+format)
	}
}

func writeReport(w http.ResponseWriter, format string, rep *settlement.Report) {
	if format == "" {
		format = "json"
	}
	switch format {
	case "csv":
		data, err := settlement.ToCSV(rep)
		if err != nil {
			httputilx.Error(w, 500, err.Error())
			return
		}
		w.Header().Set("Content-Type", "text/csv")
		_, _ = w.Write(data)
	case "xml":
		data, err := settlement.ToXML(rep)
		if err != nil {
			httputilx.Error(w, 500, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write(data)
	case "json":
		httputilx.WriteJSON(w, 200, rep)
	default:
		httputilx.Error(w, 400, "unsupported format "+format)
	}
}

type generateReq struct {
	FromDate   string `json:"from_date"`
	ToDate     string `json:"to_date"`
	MerchantID string `json:"merchant_id"`
}

func (a *app) generate(w http.ResponseWriter, r *http.Request) {
	var req generateReq
	if err := httputilx.ReadJSON(r, &req); err != nil && err != io.EOF {
		httputilx.Error(w, 400, err.Error())
		return
	}
	yesterday := labclock.Now().AddDate(0, 0, -1).Format("2006-01-02")
	from, to := req.FromDate, req.ToDate
	if from == "" {
		from = yesterday
	}
	if to == "" {
		to = yesterday
	}
	ids, err := a.runBatch(from, to, req.MerchantID)
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	httputilx.WriteJSON(w, 202, map[string]any{
		"message":         "generation started",
		"records_created": len(ids),
		"record_ids":      ids,
	})
}

type transitionReq struct {
	Event         string `json:"event"`
	Reason        string `json:"reason"`
	ReplacementID string `json:"replacement_id"`
}

func (a *app) transition(w http.ResponseWriter, r *http.Request) {
	started := a.guard.start()
	id := r.PathValue("id")
	var req transitionReq
	if err := httputilx.ReadJSON(r, &req); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	if req.Event == "" {
		httputilx.Error(w, 400, "event required")
		return
	}
	meta := req.Reason
	if meta == "" {
		meta = req.ReplacementID
	}
	event := settlement.TransitionEvent(req.Event)
	result, err := a.store.Transition(id, event, meta)
	if err != nil {
		status := 409
		if strings.Contains(err.Error(), "not found") {
			status = 404
		}
		httputilx.WriteJSON(w, status, result)
		return
	}
	if event == settlement.EventSettle {
		if rec, gerr := a.store.Get(id); gerr == nil {
			a.submitPayout(rec, started)
		}
	}
	httputilx.WriteJSON(w, 200, result)
}

// --- bank ledger fetch + batch generation ---

type bankAccount struct {
	ID       string `json:"id"`
	IBAN     string `json:"iban"`
	Currency string `json:"currency"`
}

type bankEntry struct {
	AccountID string `json:"accountId"`
	Amount    string `json:"amount"`
	Currency  string `json:"currency"`
	At        string `json:"at"`
}

// fetchLedger adapts bank's GET /accounts + GET /ledger responses into
// settlement's own Ledger view: one Entry per credit (money-in) leg,
// resolved to the receiving account's IBAN. See settlement.Entry's doc
// comment for why declines/chargebacks/returns have no source data here.
func (a *app) fetchLedger(fromDate, toDate string) (*settlement.Ledger, error) {
	var accResp struct {
		Accounts []bankAccount `json:"accounts"`
	}
	if err := a.getJSON(a.bankURL+"/accounts", &accResp); err != nil {
		return nil, fmt.Errorf("settlement: fetch accounts: %w", err)
	}
	ibanByAccount := make(map[string]string, len(accResp.Accounts))
	for _, acc := range accResp.Accounts {
		ibanByAccount[acc.ID] = acc.IBAN
	}

	var ledgerResp struct {
		Entries []bankEntry `json:"entries"`
	}
	if err := a.getJSON(a.bankURL+"/ledger", &ledgerResp); err != nil {
		return nil, fmt.Errorf("settlement: fetch ledger: %w", err)
	}

	out := &settlement.Ledger{}
	for _, e := range ledgerResp.Entries {
		cents, err := money.Parse(e.Amount)
		if err != nil || cents <= 0 {
			continue // debit leg or unparseable; only credits are sales
		}
		iban, ok := ibanByAccount[e.AccountID]
		if !ok {
			continue
		}
		ts, err := time.Parse(time.RFC3339, e.At)
		if err != nil {
			continue
		}
		date := ts.UTC().Format("2006-01-02")
		if date < fromDate || date > toDate {
			continue
		}
		out.Entries = append(out.Entries, settlement.Entry{
			MerchantID:  iban,
			Currency:    e.Currency,
			AmountCents: cents,
			Date:        date,
		})
	}
	return out, nil
}

func (a *app) getJSON(url string, dest any) error {
	resp, err := a.client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(raw))
	}
	return json.Unmarshal(raw, dest)
}

// runBatch fetches the ledger for [fromDate,toDate], groups entries by
// (merchant, currency), and drives each group through
// Create -> Process -> (Generate) -> Settle, staging files and submitting
// the B4B payout on every successful Settle. Returns the
// created record IDs.
//
// A reset that lands while it runs drops the rest: no record is made and no
// file written after it (reset.go).
func (a *app) runBatch(fromDate, toDate, merchantFilter string) ([]string, error) {
	started := a.guard.start()
	ledger, err := a.fetchLedger(fromDate, toDate)
	if err != nil {
		return nil, err
	}
	settlementDate := toDate
	if ts, err := time.Parse("2006-01-02", toDate); err == nil {
		settlementDate = ts.AddDate(0, 0, 1).Format("2006-01-02")
	}

	type key struct{ merchantID, currency string }
	groups := map[key][]settlement.Entry{}
	var order []key
	for _, e := range ledger.Entries {
		if merchantFilter != "" && e.MerchantID != merchantFilter {
			continue
		}
		k := key{e.MerchantID, e.Currency}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], e)
	}

	ids := make([]string, 0, len(order))
	for _, k := range order {
		report := settlement.Generate(&settlement.Ledger{Entries: groups[k]}, k.merchantID, fromDate, toDate, settlementDate)
		rec := &settlement.SettlementRecord{
			ID:               "set_" + shortID(),
			MerchantID:       k.merchantID,
			TransactionsDate: toDate,
			Currency:         k.currency,
			State:            settlement.StateScheduled,
			SettlementDate:   settlementDate,
		}
		var created bool
		if !a.guard.apply(started, func() {
			var settled bool
			created, settled = a.settleRecord(rec, report, "batch settle failed")
			if !settled {
				return
			}
			// No payout here. A payout is owed because the acquirer
			// settled, and the acquirer says so in the settlement file --
			// which worldline_pull.go processes. This endpoint generates
			// the platform's own internal report from its own ledger;
			// paying out from it as well would pay every merchant twice,
			// once per source.
			if err := a.stageReport(report); err != nil {
				log.Printf("settlement: stage %s: %v", rec.ID, err)
			}
		}) {
			log.Printf("settlement: batch %s..%s dropped by a reset after %d record(s)", fromDate, toDate, len(ids))
			break
		}
		if created {
			ids = append(ids, rec.ID)
		}
	}
	return ids, nil
}

// settleRecord creates rec and drives it Scheduled -> Processing -> Settled
// with report, or to Failed with failReason when the settle is refused. It
// reports whether the record was created and whether it settled. Callers
// run it under the guard.
func (a *app) settleRecord(rec *settlement.SettlementRecord, report *settlement.Report, failReason string) (created, settled bool) {
	if err := a.store.Create(rec); err != nil {
		log.Printf("settlement: create %s: %v", rec.ID, err)
		return false, false
	}
	if _, err := a.store.Transition(rec.ID, settlement.EventProcess); err != nil {
		log.Printf("settlement: process %s: %v", rec.ID, err)
		return true, false
	}
	if err := a.store.SetReport(rec.ID, report); err != nil {
		log.Printf("settlement: set report %s: %v", rec.ID, err)
		return true, false
	}
	if res, err := a.store.Transition(rec.ID, settlement.EventSettle); err != nil || !res.Success {
		log.Printf("settlement: settle %s failed: %v", rec.ID, err)
		if _, ferr := a.store.Transition(rec.ID, settlement.EventFail, failReason); ferr != nil {
			log.Printf("settlement: fail %s: %v", rec.ID, ferr)
		}
		return true, false
	}
	return true, true
}

// stageReport writes report as CSV, XML, and JSON to the SFTP-out
// directory (via Stager.Stage, backing this service's own /sftp/out*
// endpoints) and, separately, as a second identical copy to
// {SFTP_OUTBOUND_DIR}/{merchant_id}/daily/{filename}.
//
// These are the platform's own internal reports. It used to also generate
// the Worldline/Bambora settlement file here and hand it to the acquirer
// simulator through a shared bind mount -- the platform manufacturing the
// file it was meant to be receiving. The acquirer cuts that file now (see
// cmd/worldline), and this service's only route to it is
// worldline_pull.go's real SFTP download.
func (a *app) stageReport(report *settlement.Report) error {
	for _, format := range []string{"csv", "xml", "json"} {
		if _, err := a.stager.Stage(report, format); err != nil {
			return fmt.Errorf("stage %s: %w", format, err)
		}
		if err := a.writeOutbound(report, format); err != nil {
			return fmt.Errorf("outbound %s: %w", format, err)
		}
	}
	return nil
}

func (a *app) writeOutbound(report *settlement.Report, format string) error {
	if err := sftp.ValidatePathSegment(report.MerchantID); err != nil {
		return err
	}
	var data []byte
	var err error
	switch format {
	case "csv":
		data, err = settlement.ToCSV(report)
	case "xml":
		data, err = settlement.ToXML(report)
	case "json":
		data, err = json.MarshalIndent(report, "", "  ")
	default:
		return fmt.Errorf("unsupported format %q", format)
	}
	if err != nil {
		return err
	}
	filename := fmt.Sprintf("%s_%s_%s_WX.%s", report.MerchantID, report.TransactionsDate, report.Currency, format)
	dir := filepath.Join(a.outboundDir, report.MerchantID, "daily")
	if err := os.MkdirAll(dir, 0o777); err != nil { // world-writable: shared with worldline + host, see internal/sftp/staging.go
		return err
	}
	return os.WriteFile(filepath.Join(dir, filename), data, 0o644)
}

// --- B4B payout (best-effort, non-fatal) ---
//
// Settlement's payout call goes to B4B, not Banking Circle directly (see
// docs/ARCHITECTURE-vendor-corrections.md Addendum section A -- Banking
// Circle's payment-creation endpoint never existed in the real product;
// settlement's real analogue calls B4B, and B4B is the one that later
// bridges into Banking Circle on its own).

// worldlineFundingVIBAN is a fixed, obviously-simulated lab constant
// standing in for the real Worldline settlement funding VIBAN (same
// "obviously fake" spirit as this lab's "GB00SIM..." IBANs and the
// bc_acc_worldline/bc_acc_merchant ledger account ids elsewhere).
const worldlineFundingVIBAN = "WORLDLINE_FUNDING_VIBAN_PLACEHOLDER"

// b4bCompanyID is a fixed lab constant for B4B's required company_id
// field (the Infinite-side B4B company account initiating the payment).
// Nothing in this lab's scope configures a real one, and B4B's own mock
// does not need to validate it (same "does not need to know or validate"
// spirit as its beneficiary auto-vivification, see Addendum section F) --
// so a fixed placeholder is sufficient here.
const b4bCompanyID = "company_infinite"

// currencyCountry maps a settlement currency to the country B4B expects on
// its {account,financialInstitution?,country?} refs -- reusing buddy's own
// exact mapping (apps/accounts-settlement/.../b4b-credential.resolver.ts),
// not inventing a different one (docs/ARCHITECTURE-phase3-corrections.md
// section 4).
var currencyCountry = map[string]string{"EUR": "IE", "GBP": "GB"}

// b4bAmount.Amount is a JSON number on the wire (real B4B's
// B4bPaymentAmount = {amount: number, currency: string}) -- json.Number
// accepts any numeric literal without float precision loss and marshals
// back out unquoted, matching the real shape in both directions. Only
// this wire boundary changes; internal/money and every domain-level
// decimal stay plain strings.
type b4bAmount struct {
	Amount   json.Number `json:"amount"`
	Currency string      `json:"currency"`
}

type b4bAccountRef struct {
	Account              string `json:"account"`
	FinancialInstitution string `json:"financialInstitution,omitempty"`
	Country              string `json:"country,omitempty"`
}

// b4bPaymentReq is POST {B4B_URL}/oversight/v1/payments' request body,
// the exact shape from ARCHITECTURE-vendor-corrections.md section 4.
type b4bPaymentReq struct {
	ExternalRef            string        `json:"external_ref"`
	BeneficiaryID          string        `json:"beneficiary_id"`
	CompanyID              string        `json:"company_id"`
	CallbackURL            string        `json:"callback_url"`
	SCAApplied             bool          `json:"sca_applied"`
	Amount                 b4bAmount     `json:"amount"`
	CurrencyOfTransfer     string        `json:"currencyOfTransfer"`
	DebtorViban            b4bAccountRef `json:"debtorViban"`
	CreditorAccount        b4bAccountRef `json:"creditorAccount"`
	CreditorName           string        `json:"creditorName"`
	ChargeBearer           string        `json:"chargeBearer"`
	RequestedExecutionDate string        `json:"requestedExecutionDate"`
}

// b4bPaymentResp is B4B's 202 response body (only the fields settlement
// needs; payload is intentionally not decoded -- settlement doesn't use it).
type b4bPaymentResp struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// verifyDecisionResp is the verify mock's GET .../verify-decision/{id}
// response (docs/ARCHITECTURE-phase3-corrections.md section 3) -- only
// the field submitPayout gates on is decoded.
type verifyDecisionResp struct {
	OverallDecision string `json:"overallDecision"`
}

// bcBalanceEntry/bcInternalBalancesResp mirror Banking Circle's internal
// balance-proxy response (docs/ARCHITECTURE-phase3-corrections.md section
// 1, GET /internal/accounts/{accountId}/balances) -- only the field
// checkSgaBalance needs is decoded.
type bcBalanceEntry struct {
	BeginOfDayAmount string `json:"beginOfDayAmount"`
	IntraDayAmount   string `json:"intraDayAmount"`
}

type bcInternalBalancesResp struct {
	Balances []bcBalanceEntry `json:"balances"`
}

// fnvPositive derives a stable non-negative int from s -- real B4B/verify
// identify a merchant by an integer database key this lab's string
// merchant ids (GB00SIM...) have no equivalent for. This lab owns both the
// caller (here) and the verify mock, so the derivation only needs to be
// stable, not "real" (docs/ARCHITECTURE-phase3-corrections.md section 5).
func fnvPositive(s string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	v := int32(h.Sum32())
	if v < 0 {
		v = -v
	}
	return int(v)
}

// checkSgaBalance confirms Banking Circle's safeguarding account for
// currency currently holds at least requiredCents, via Banking Circle's
// plain, no-credential internal balance proxy -- settlement never holds
// Banking Circle's mTLS+bearer credentials itself, matching real
// settle-processing (docs/ARCHITECTURE-phase3-corrections.md section 1
// and 5). Unlike the beneficiary-lookup call below, connectivity errors
// here are NOT logged-and-continued: this gates real money movement, so
// "can't confirm funds" is treated the same as "insufficient funds" --
// fail closed, not fail open.
func (a *app) checkSgaBalance(currency string, requiredCents int64) (sufficient bool, err error) {
	sgaID, ok := a.sgaAccounts[currency]
	if !ok {
		return false, fmt.Errorf("no Banking Circle safeguarding account configured for currency %s", currency)
	}
	resp, err := a.bcInternalClient.Get(a.bcInternalURL + "/internal/accounts/" + sgaID + "/balances")
	if err != nil {
		return false, fmt.Errorf("banking circle internal balance check: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("banking circle internal balance check: status %d: %s", resp.StatusCode, string(raw))
	}
	var out bcInternalBalancesResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, fmt.Errorf("banking circle internal balance check: unparseable response: %w", err)
	}
	if len(out.Balances) == 0 {
		return false, fmt.Errorf("banking circle internal balance check: empty balances for %s", sgaID)
	}
	// The balance is the day's opening plus what has moved since.
	openCents, err := money.Parse(out.Balances[0].BeginOfDayAmount)
	if err != nil {
		return false, fmt.Errorf("banking circle internal balance check: unparseable beginOfDayAmount: %w", err)
	}
	movedCents, err := money.Parse(out.Balances[0].IntraDayAmount)
	if err != nil {
		return false, fmt.Errorf("banking circle internal balance check: unparseable intraDayAmount: %w", err)
	}
	return openCents+movedCents >= requiredCents, nil
}

// checkVerification calls the merchant-verification mock's async
// trigger-then-read pair (docs/ARCHITECTURE-phase3-corrections.md section
// 3) and reports whether the merchant is cleared to be paid. Unlike
// checkSgaBalance, connectivity errors here ARE logged-and-continued
// (approved=true): verification is explicitly a sample/stub component in
// this lab, not the thing this harness exists to prove, so its own
// unavailability must not block the payout demonstration the way
// insufficient SGA funds genuinely should. An actual DECLINE decision,
// once received, still blocks -- only *not hearing back* is forgiven.
func (a *app) checkVerification(merchantID string) (approved bool) {
	id := fnvPositive(merchantID)
	triggerBody, _ := json.Marshal(map[string]int{"merchantApplicationId": id})
	triggerReq, err := http.NewRequest(http.MethodPost, a.verifyURL+"/api/v1/verification/verify/aml-decision", bytes.NewReader(triggerBody))
	if err != nil {
		log.Printf("settlement: verify trigger for merchant %s: build request: %v (continuing)", merchantID, err)
		return true
	}
	triggerReq.Header.Set("Content-Type", "application/json")
	triggerReq.Header.Set("x-internal-api-key", a.verifyKey)
	if resp, err := a.verifyClient.Do(triggerReq); err != nil {
		log.Printf("settlement: verify trigger for merchant %s failed (continuing): %v", merchantID, err)
		return true
	} else {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
	}

	readURL := fmt.Sprintf("%s/api/v1/verification/verify/data/verify-decision/%d", a.verifyURL, id)
	for range 10 {
		time.Sleep(50 * time.Millisecond)
		readReq, err := http.NewRequest(http.MethodGet, readURL, nil)
		if err != nil {
			log.Printf("settlement: verify read for merchant %s: build request: %v (continuing)", merchantID, err)
			return true
		}
		readReq.Header.Set("x-internal-api-key", a.verifyKey)
		resp, err := a.verifyClient.Do(readReq)
		if err != nil {
			log.Printf("settlement: verify read for merchant %s failed (continuing): %v", merchantID, err)
			return true
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			continue // decision not ready yet
		}
		if resp.StatusCode != http.StatusOK {
			log.Printf("settlement: verify read for merchant %s: status %d (continuing): %s", merchantID, resp.StatusCode, string(raw))
			return true
		}
		var out verifyDecisionResp
		if err := json.Unmarshal(raw, &out); err != nil {
			log.Printf("settlement: verify read for merchant %s: unparseable response (continuing): %v", merchantID, err)
			return true
		}
		switch out.OverallDecision {
		case "APPROVE", "APPROVED", "PRE_APPROVE":
			return true
		default:
			log.Printf("settlement: verify declined merchant %s: overallDecision=%s", merchantID, out.OverallDecision)
			return false
		}
	}
	log.Printf("settlement: verify decision for merchant %s never became ready (continuing)", merchantID)
	return true
}

// submitPayout POSTs the settled record's net amount to B4B (settlement's
// real payout rail -- see the section comment above), gated by two checks
// that model the real causal chain (docs/ARCHITECTURE-phase3-corrections.md
// section 5): Banking Circle's safeguarding account must actually hold the
// funds Worldline's lump sum was supposed to deliver, and the merchant
// must clear verification. Both gates set a distinct PayoutState and
// return without calling B4B; POST .../retry-payout re-invokes this
// function once the gate is expected to pass. This is best-effort and
// must never fail settlement itself: on any B4B-side error (bad status,
// network error, timeout) it logs clearly, marks PayoutState
// "submission_failed", and returns -- the record already finished its
// Settled transition before this was called.
func (a *app) submitPayout(rec *settlement.SettlementRecord, started uint64) {
	if rec.Report == nil {
		return
	}
	// Every outcome, and the payment itself, lands only if no reset came
	// since started (reset.go): a payout for a record a reset cleared is
	// never sent, and the gates are not asked about it either.
	dropped := func() bool {
		if a.guard.moved(started) {
			log.Printf("settlement: payout for %s: %v", rec.ID, errReset)
			return true
		}
		return false
	}
	if dropped() {
		return
	}
	setPayout := func(state string) {
		a.guard.apply(started, func() { _ = a.store.SetPayout(rec.ID, "", state) })
	}

	requiredCents := rec.Report.SettlementNetAmount
	sufficient, err := a.checkSgaBalance(rec.Currency, requiredCents)
	if err != nil {
		log.Printf("settlement: payout for %s: SGA balance check failed: %v", rec.ID, err)
		setPayout("balance_check_failed")
		return
	}
	if !sufficient {
		log.Printf("settlement: payout for %s: SGA balance insufficient for currency %s, awaiting Worldline funding", rec.ID, rec.Currency)
		setPayout("awaiting_funding")
		return
	}

	if dropped() {
		return
	}
	if !a.checkVerification(rec.MerchantID) {
		setPayout("verification_declined")
		return
	}

	token, err := a.b4bJWT()
	if err != nil {
		log.Printf("settlement: payout for %s: build JWT: %v", rec.ID, err)
		setPayout("submission_failed")
		return
	}

	// The MID is the platform's own reference for this payee, and B4B
	// resolves either its own minted id or the client's external_ref. It
	// used to be "ben_"+MID -- an id nothing had ever registered, which
	// always landed on an auto-vivified record and so quietly ignored
	// whatever account details had actually been registered for the
	// outlet.
	beneficiaryID := rec.MerchantID
	if dropped() {
		return
	}
	// GET the beneficiary first, matching the real call order (Addendum
	// section A) -- this is a realism-only call per the spec: its
	// response body is not used for anything. A failure here (B4B's mock
	// auto-vivifies any id, per Addendum section F, so this should only
	// fail if B4B itself is unreachable) is logged and does not block the
	// payment attempt below -- same graceful-degradation style as the
	// rest of this lab (log and continue, never abort on a downstream
	// being unreachable).
	ben, err := a.fetchB4BBeneficiary(beneficiaryID, token)
	if err != nil {
		log.Printf("settlement: payout for %s: beneficiary lookup %s failed: %v", rec.ID, beneficiaryID, err)
		setPayout("submission_failed")
		return
	}
	// Only "pass" permits payments, and the status can move at any time
	// under continuous screening -- so this is checked per payment, not
	// once at onboarding. Submitting anyway would just earn a 422.
	if ben.SanctionsStatus != "pass" {
		log.Printf("settlement: payout for %s: beneficiary %s has sanctions_status %q, not paying out",
			rec.ID, beneficiaryID, ben.SanctionsStatus)
		setPayout("sanctions_blocked")
		return
	}

	country := currencyCountry[rec.Currency]
	// The creditor fields come from the beneficiary record, not from this
	// service's own idea of the merchant. B4B checks them against that
	// record and refuses the payment on a mismatch, which is the point:
	// they are a consistency check, not a recipient override.
	body, err := json.Marshal(b4bPaymentReq{
		ExternalRef:            rec.ID,
		BeneficiaryID:          beneficiaryID,
		CompanyID:              b4bCompanyID,
		CallbackURL:            a.b4bCallbackURL,
		SCAApplied:             false,
		Amount:                 b4bAmount{Amount: json.Number(money.Format(rec.Report.SettlementNetAmount)), Currency: rec.Currency},
		CurrencyOfTransfer:     rec.Currency,
		DebtorViban:            b4bAccountRef{Account: worldlineFundingVIBAN, Country: country},
		CreditorAccount:        b4bAccountRef{Account: ben.AccountNumber, FinancialInstitution: ben.FinancialInstitution, Country: country},
		CreditorName:           ben.AccountName,
		ChargeBearer:           "SHA",
		RequestedExecutionDate: labclock.Now().Format("2006-01-02"),
	})
	if err != nil {
		log.Printf("settlement: payout request for %s: marshal error: %v", rec.ID, err)
		setPayout("submission_failed")
		return
	}
	httpReq, err := http.NewRequest(http.MethodPost, a.b4bURL+"/oversight/v1/payments", bytes.NewReader(body))
	if err != nil {
		log.Printf("settlement: payout request for %s: build request: %v", rec.ID, err)
		setPayout("submission_failed")
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	// The payment and the record of it land together: a reset that arrives
	// while it is on its way waits for B4B's answer.
	if !a.guard.apply(started, func() { a.postPayout(rec, httpReq) }) {
		log.Printf("settlement: payout for %s not sent: %v", rec.ID, errReset)
	}
}

// postPayout sends the payment to B4B and stores the outcome on the record.
// It runs under the guard, so it stores directly.
func (a *app) postPayout(rec *settlement.SettlementRecord, httpReq *http.Request) {
	resp, err := a.b4bClient.Do(httpReq)
	if err != nil {
		log.Printf("settlement: payout submission for %s failed (b4b unreachable, continuing as Settled): %v", rec.ID, err)
		_ = a.store.SetPayout(rec.ID, "", "submission_failed")
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		log.Printf("settlement: payout submission for %s failed (status %d, continuing as Settled): %s", rec.ID, resp.StatusCode, string(raw))
		_ = a.store.SetPayout(rec.ID, "", "submission_failed")
		return
	}
	var out b4bPaymentResp
	if err := json.Unmarshal(raw, &out); err != nil {
		log.Printf("settlement: payout response for %s unparseable (continuing as Settled): %v", rec.ID, err)
		_ = a.store.SetPayout(rec.ID, "", "submission_failed")
		return
	}
	if err := a.store.SetPayout(rec.ID, out.ID, out.Status); err != nil {
		log.Printf("settlement: persist payout for %s: %v", rec.ID, err)
	}
}

// retryPayout implements POST /reports/settlement/{id}/retry-payout --
// re-invokes submitPayout for a Settled record whose payout is stuck in a
// gate this lab can re-check deterministically (docs/
// ARCHITECTURE-phase3-corrections.md section 5): simulate Worldline's lump
// sum via Banking Circle's POST /internal/incoming-payments, then retry.
// Refuses (400) a record not yet Settled, or whose payout is already in
// flight or done -- retrying an approved/in-progress B4B payment would
// double-submit it.
func (a *app) retryPayout(w http.ResponseWriter, r *http.Request) {
	started := a.guard.start()
	id := r.PathValue("id")
	rec, err := a.store.Get(id)
	if err != nil {
		httputilx.Error(w, 404, "settlement record not found")
		return
	}
	if rec.State != settlement.StateSettled {
		httputilx.Error(w, 400, "retry-payout requires a Settled record, got state="+string(rec.State))
		return
	}
	switch rec.PayoutState {
	case "awaiting_funding", "verification_declined", "submission_failed", "balance_check_failed", "sanctions_blocked":
	default:
		httputilx.Error(w, 400, "payout already in flight or completed: payout_state="+rec.PayoutState)
		return
	}
	a.submitPayout(rec, started)
	fresh, err := a.store.Get(id)
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	httputilx.WriteJSON(w, 200, payoutResp{PayoutID: fresh.PayoutID, PayoutState: fresh.PayoutState})
}

// b4bBeneficiary is the part of B4B's beneficiary record a payment has to
// agree with.
type b4bBeneficiary struct {
	ID                   string `json:"id"`
	AccountName          string `json:"account_name"`
	AccountNumber        string `json:"account_number"`
	FinancialInstitution string `json:"financial_institution"`
	SanctionsStatus      string `json:"sanctions_status"`
}

// fetchB4BBeneficiary GETs {B4B_URL}/oversight/v1/beneficiaries/{id} and
// returns the record.
//
// The response used to be discarded -- the call was made purely to match
// the real call order. That was the gap: B4B checks a payment's creditor
// fields *against* this record and answers 422 on a mismatch, so a caller
// that does not read it is guessing at the account details it is about to
// send, and finds out it guessed wrong only when the payment is refused.
func (a *app) fetchB4BBeneficiary(beneficiaryID, token string) (*b4bBeneficiary, error) {
	req, err := http.NewRequest(http.MethodGet, a.b4bURL+"/oversight/v1/beneficiaries/"+beneficiaryID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := a.b4bClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var ben b4bBeneficiary
	if err := json.Unmarshal(raw, &ben); err != nil {
		return nil, fmt.Errorf("unparseable beneficiary response: %w", err)
	}
	return &ben, nil
}

// b4bJWT mints the RS512-signed bearer token every B4B call carries. The
// format lives in internal/b4b next to the verification B4B itself
// implements, so the two cannot drift.
func (a *app) b4bJWT() (string, error) {
	return b4b.SignBearerToken(a.b4bPrivateKey, a.b4bKeyID)
}

func loadRSAPrivateKey(path string) (*rsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM block found", path)
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: not a valid PKCS#1 or PKCS#8 RSA private key: %w", path, err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: PEM block is not an RSA private key", path)
	}
	return key, nil
}

// b4bWebhookBody is the body B4B POSTs to {B4B_CALLBACK_URL} (registered
// here as POST /internal/b4b-webhook), the exact shape from
// ARCHITECTURE-vendor-corrections.md section 4: "{id, status, payload?,
// banking_circle_api_response?}". payload and banking_circle_api_response
// are decoded as raw JSON since settlement doesn't need their contents,
// only id/status to update the matching record's payout state.
type b4bWebhookBody struct {
	ID                       string          `json:"id"`
	Status                   string          `json:"status"`
	Payload                  json.RawMessage `json:"payload,omitempty"`
	BankingCircleAPIResponse json.RawMessage `json:"banking_circle_api_response,omitempty"`
}

// b4bWebhook handles B4B's payment-status webhook, correlating body.ID
// back to a settlement record by linear-scanning the store for
// PayoutID == body.ID (see Addendum section A -- small N, matches this
// store's existing List-then-filter style elsewhere) and recording the
// new state via the same SetPayout used on initial submission.
func (a *app) b4bWebhook(w http.ResponseWriter, r *http.Request) {
	var body b4bWebhookBody
	if err := httputilx.ReadJSON(r, &body); err != nil {
		httputilx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.ID == "" {
		httputilx.Error(w, http.StatusBadRequest, "id is required")
		return
	}
	for _, rec := range a.store.List() {
		if rec.PayoutID != body.ID {
			continue
		}
		if err := a.store.SetPayout(rec.ID, body.ID, body.Status); err != nil {
			log.Printf("settlement: b4b webhook: persist payout for %s: %v", rec.ID, err)
			httputilx.Error(w, http.StatusInternalServerError, err.Error())
			return
		}
		httputilx.WriteJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	log.Printf("settlement: b4b webhook: no settlement record with payout_id=%s", body.ID)
	httputilx.WriteJSON(w, 200, map[string]string{"status": "ignored"})
}

// --- daily cutoff scheduler ---

// parseClockTime parses "HH:MM" (24h, UTC).
func parseClockTime(s string) (hour, minute int, err error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected HH:MM, got %q", s)
	}
	hour, err = strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, fmt.Errorf("invalid hour in %q", s)
	}
	minute, err = strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("invalid minute in %q", s)
	}
	return hour, minute, nil
}

// durationUntilCutoff returns how long from now until the next occurrence
// of cutoff (UTC, "HH:MM"), today if it hasn't passed yet, else tomorrow.
// An unparseable cutoff falls back to a 24h retry so a typo doesn't spin.
func durationUntilCutoff(now time.Time, cutoff string) time.Duration {
	hour, minute, err := parseClockTime(cutoff)
	if err != nil {
		return 24 * time.Hour
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, time.UTC)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next.Sub(now)
}

// runCutoffLoop runs the daily batch for "yesterday" (UTC) at CUTOFF_TIME,
// matching ARCHITECTURE-settlement-report.md's data flow ("Settlement
// batch job runs at cut-off time"). podman-compose does not enforce start
// order (docs/security/ca-and-tls.md); a bank that isn't reachable yet just
// fails this run's fetch, logged, and the loop tries again tomorrow.
func (a *app) runCutoffLoop(cutoff string) {
	if _, _, err := parseClockTime(cutoff); err != nil {
		log.Printf("settlement: CUTOFF_TIME %q invalid (%v); automatic daily batch disabled", cutoff, err)
		return
	}
	for {
		// On the lab clock: moving it past the cutoff runs the batch, as the
		// day reaching it would.
		labclock.Wait(func(now time.Time) time.Time {
			return now.Add(durationUntilCutoff(now, cutoff))
		})
		yesterday := labclock.Now().AddDate(0, 0, -1).Format("2006-01-02")
		if !a.wiring.Connected() {
			log.Printf("settlement: cutoff batch for %s skipped — disconnected in the lab", yesterday)
			time.Sleep(time.Minute)
			continue
		}
		ids, err := a.runBatch(yesterday, yesterday, "")
		if err != nil {
			log.Printf("settlement: cutoff batch for %s failed: %v", yesterday, err)
		} else {
			log.Printf("settlement: cutoff batch for %s created %d record(s)", yesterday, len(ids))
		}
		time.Sleep(time.Minute) // clear of the cutoff instant before recomputing the next wait
	}
}

// --- SFTP endpoints (section 5 of ARCHITECTURE-sftp-staging.md) ---

type fileEntry struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime string `json:"modTime"`
}

// entriesFor renders os.FileInfo results as path-ful JSON entries for the
// GLOBAL listing (GET /sftp/out, every merchant at once). The relative
// path is {merchant_id}/{name} when the filename matches Worldline's own
// WX naming convention (sftp.ParseFilename recovers the embedded merchant
// id); a filename that doesn't match (e.g. the real Bambora settlement
// CSV, which embeds no merchant id at all) is still listed, just without
// a reconstructed path prefix. GET /sftp/out/{merchant_id} does NOT use
// this -- see listOutMerchant, which lists that merchant's own
// subdirectory directly and so never needs to guess ownership from a
// filename.
func entriesFor(infos []os.FileInfo) []fileEntry {
	out := make([]fileEntry, 0, len(infos))
	for _, info := range infos {
		rel := info.Name()
		if merchantID, _, _, _, err := sftp.ParseFilename(info.Name()); err == nil {
			rel = merchantID + "/" + info.Name()
		}
		out = append(out, fileEntry{Path: rel, Size: info.Size(), ModTime: info.ModTime().UTC().Format(time.RFC3339)})
	}
	return out
}

func (a *app) listOut(w http.ResponseWriter, r *http.Request) {
	infos, err := a.stager.ListOut()
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	httputilx.WriteJSON(w, 200, map[string]any{"files": entriesFor(infos)})
}

func (a *app) listOutMerchant(w http.ResponseWriter, r *http.Request) {
	merchantID := r.PathValue("merchant_id")
	if err := sftp.ValidatePathSegment(merchantID); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	infos, err := a.stager.ListOutMerchant(merchantID)
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	out := make([]fileEntry, 0, len(infos))
	for _, info := range infos {
		out = append(out, fileEntry{Path: merchantID + "/" + info.Name(), Size: info.Size(), ModTime: info.ModTime().UTC().Format(time.RFC3339)})
	}
	httputilx.WriteJSON(w, 200, map[string]any{"files": out})
}

func (a *app) readOut(w http.ResponseWriter, r *http.Request) {
	merchantID, filename := r.PathValue("merchant_id"), r.PathValue("filename")
	if err := sftp.ValidatePathSegment(merchantID); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	if err := sftp.ValidatePathSegment(filename); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	rel := merchantID + "/" + filename
	data, err := a.stager.ReadOut(rel)
	if err != nil {
		httputilx.Error(w, 404, err.Error())
		return
	}
	w.Header().Set("Content-Type", contentTypeFor(r.PathValue("filename")))
	_, _ = w.Write(data)
}

func (a *app) listStaging(w http.ResponseWriter, r *http.Request) {
	infos, err := a.stager.ListStaging()
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	httputilx.WriteJSON(w, 200, map[string]any{"files": entriesFor(infos)})
}

func contentTypeFor(filename string) string {
	switch {
	case strings.HasSuffix(filename, ".csv"):
		return "text/csv"
	case strings.HasSuffix(filename, ".xml"):
		return "application/xml"
	default:
		return "application/json"
	}
}

func shortID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func logReq(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
