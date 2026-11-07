package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/settlement"
	"github.com/webduvet/fintechlab-simple/internal/sftp"
)

// testB4BKey is a single RSA key generated once for the whole test
// binary: submitPayout needs a real key to sign a JWT with, but no test
// here cares which key, only that JWT-building succeeds.
var testB4BKey = func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
}()

// newTestApp wires a default Banking Circle internal-balance fake that
// always reports a large enough balance so submitPayout's SGA gate passes
// (docs/ARCHITECTURE-phase3-corrections.md section 5) -- tests exercising
// that gate itself override a.bcInternalURL after construction. verifyURL
// defaults to an address nothing listens on: checkVerification fails open
// on connectivity (verification is a stub component, not what this lab
// exists to prove), so this default lets every test not specifically
// about verification proceed as if it were approved without needing its
// own fake verify server.
func newTestApp(t *testing.T, dir string) *app {
	t.Helper()
	bc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accountId": "bc_acc_sga_eur",
			"balances":  []map[string]string{{"intraDayAmount": "1000000.00"}},
		})
	}))
	t.Cleanup(bc.Close)
	return &app{
		store:               settlement.NewStore(),
		stager:              &sftp.Stager{OutDir: dir + "/out", StagingDir: dir + "/staging"},
		outboundDir:         dir + "/outbound",
		client:              &http.Client{Timeout: time.Second},
		worldlineIdentifier: "Worldline_Settlement",
		b4bClient:           &http.Client{Timeout: time.Second},
		b4bPrivateKey:       testB4BKey,
		b4bKeyID:            "b4b-mock-1",
		b4bCallbackURL:      "http://settlement:8083/internal/b4b-webhook",
		verifyURL:           "http://127.0.0.1:1", // nothing listens here; see doc comment above
		verifyClient:        &http.Client{Timeout: time.Second},
		verifyKey:           "sim-verify-key-dev-only",
		bcInternalURL:       bc.URL,
		bcInternalClient:    &http.Client{Timeout: time.Second},
		sgaAccounts:         map[string]string{"EUR": "bc_acc_sga_eur", "GBP": "bc_acc_sga_gbp"},
	}
}

func sampleTestReport() *settlement.Report {
	return &settlement.Report{
		MerchantID:          "GB00SIM0000000000003",
		TransactionsDate:    "2026-09-03",
		Currency:            "EUR",
		SettlementState:     "Settled",
		SettlementDate:      "2026-09-04",
		SaleAmountTotal:     15000,
		SettlementNetAmount: 15000,
	}
}

func TestParseDateRangeDefaults(t *testing.T) {
	fromDate, toDate, err := parseDateRange("", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	wantTo := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	wantFrom := wantTo.AddDate(0, 0, -7)
	if !toDate.Equal(wantTo) {
		t.Fatalf("toDate = %v, want today (%v)", toDate, wantTo)
	}
	if !fromDate.Equal(wantFrom) {
		t.Fatalf("fromDate = %v, want 7 days ago (%v)", fromDate, wantFrom)
	}
}

func TestParseDateRangeExplicit(t *testing.T) {
	fromDate, toDate, err := parseDateRange("2026-09-01", "2026-09-03")
	if err != nil {
		t.Fatal(err)
	}
	if fromDate.Format("2006-01-02") != "2026-09-01" || toDate.Format("2006-01-02") != "2026-09-03" {
		t.Fatalf("got from=%s to=%s", fromDate, toDate)
	}
}

func TestParseDateRangeInvalidFormat(t *testing.T) {
	if _, _, err := parseDateRange("not-a-date", ""); err == nil {
		t.Fatal("expected error for invalid from_date")
	}
	if _, _, err := parseDateRange("", "not-a-date"); err == nil {
		t.Fatal("expected error for invalid to_date")
	}
}

func TestParseDateRangeToBeforeFrom(t *testing.T) {
	if _, _, err := parseDateRange("2026-09-03", "2026-09-01"); err == nil {
		t.Fatal("expected error when to_date precedes from_date")
	}
}

func TestParseDateRangeExceeds90Days(t *testing.T) {
	if _, _, err := parseDateRange("2026-01-01", "2026-06-01"); err == nil {
		t.Fatal("expected error for a range exceeding 90 days")
	}
}

func TestParseDateRangeExactly90DaysOK(t *testing.T) {
	if _, _, err := parseDateRange("2026-01-01", "2026-04-01"); err != nil {
		t.Fatalf("90-day-ish range should be accepted: %v", err)
	}
}

func TestParseClockTime(t *testing.T) {
	cases := []struct {
		in         string
		wantHour   int
		wantMinute int
		wantErr    bool
	}{
		{"00:00", 0, 0, false},
		{"23:59", 23, 59, false},
		{"9:05", 9, 5, false},
		{"24:00", 0, 0, true},
		{"12:60", 0, 0, true},
		{"garbage", 0, 0, true},
		{"", 0, 0, true},
	}
	for _, tc := range cases {
		h, m, err := parseClockTime(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseClockTime(%q): expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseClockTime(%q): %v", tc.in, err)
			continue
		}
		if h != tc.wantHour || m != tc.wantMinute {
			t.Errorf("parseClockTime(%q) = %d:%d, want %d:%d", tc.in, h, m, tc.wantHour, tc.wantMinute)
		}
	}
}

func TestDurationUntilCutoffLaterToday(t *testing.T) {
	now := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	d := durationUntilCutoff(now, "14:30")
	want := 4*time.Hour + 30*time.Minute
	if d != want {
		t.Fatalf("got %v, want %v", d, want)
	}
}

func TestDurationUntilCutoffAlreadyPassedRollsToTomorrow(t *testing.T) {
	now := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	d := durationUntilCutoff(now, "00:00")
	want := 14 * time.Hour
	if d != want {
		t.Fatalf("got %v, want %v", d, want)
	}
}

func TestDurationUntilCutoffInvalidFallsBackTo24h(t *testing.T) {
	d := durationUntilCutoff(time.Now().UTC(), "garbage")
	if d != 24*time.Hour {
		t.Fatalf("got %v, want 24h fallback", d)
	}
}

// TestReadOutRejectsPathTraversalMerchantID is a regression test for the
// path-traversal class of bug reported against the SFT channel: a
// merchant_id path segment that decodes to something containing "/" or
// ".." (e.g. from a %2f-encoded slash, which net/http's router treats as
// one clean segment before decoding) must be rejected with 400, not
// concatenated into a filesystem read.
func TestReadOutRejectsPathTraversalMerchantID(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)

	req := httptest.NewRequest(http.MethodGet, "/sftp/out/x/y", nil)
	req.SetPathValue("merchant_id", "../../../etc")
	req.SetPathValue("filename", "passwd")
	rec := httptest.NewRecorder()
	a.readOut(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestReadOutRejectsPathTraversalFilename(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)

	req := httptest.NewRequest(http.MethodGet, "/sftp/out/x/y", nil)
	req.SetPathValue("merchant_id", "GB00SIM0000000000003")
	req.SetPathValue("filename", "../../../../etc/passwd")
	rec := httptest.NewRecorder()
	a.readOut(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestReadOutServesLegitimateFile(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)
	report := sampleTestReport()
	if _, err := a.stager.Stage(report, "csv"); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/sftp/out/x/y", nil)
	req.SetPathValue("merchant_id", report.MerchantID)
	req.SetPathValue("filename", report.MerchantID+"_"+report.TransactionsDate+"_"+report.Currency+"_WX.csv")
	rec := httptest.NewRecorder()
	a.readOut(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), report.MerchantID) {
		t.Fatalf("body missing merchant id: %s", rec.Body.String())
	}
}

func TestWriteOutboundRejectsPathTraversalMerchantID(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)
	report := sampleTestReport()
	report.MerchantID = "../../../etc"
	if err := a.writeOutbound(report, "csv"); err == nil {
		t.Fatal("expected writeOutbound to reject a path-traversal merchant ID")
	}
}

func TestWriteOutboundWritesSecondCopy(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)
	report := sampleTestReport()
	if err := a.writeOutbound(report, "json"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(a.outboundDir + "/" + report.MerchantID + "/daily")
	if err != nil {
		t.Fatalf("outbound directory not created: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 outbound file, got %d: %v", len(entries), entries)
	}
	wantName := report.MerchantID + "_" + report.TransactionsDate + "_" + report.Currency + "_WX.json"
	if entries[0].Name() != wantName {
		t.Fatalf("filename = %q, want %q", entries[0].Name(), wantName)
	}
}

// --- end-to-end batch generation against a fake bank ---

func TestGenerateEndpointCreatesSettledRecordAndStagesFiles(t *testing.T) {
	bank := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/accounts":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"accounts": []map[string]string{
					{"id": "acc_merchant", "iban": "GB00SIM0000000000003", "currency": "EUR"},
				},
			})
		case "/ledger":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"entries": []map[string]string{
					{"accountId": "acc_merchant", "amount": "100.00", "currency": "EUR", "at": "2026-09-03T10:00:00Z"},
					{"accountId": "acc_merchant", "amount": "50.00", "currency": "EUR", "at": "2026-09-03T11:00:00Z"},
					// a debit leg on the same account: must not count as a sale.
					{"accountId": "acc_merchant", "amount": "-20.00", "currency": "EUR", "at": "2026-09-03T12:00:00Z"},
				},
			})
		default:
			w.WriteHeader(404)
		}
	}))
	defer bank.Close()

	b4b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/oversight/v1/beneficiaries/"):
			// A real beneficiary body, not a bare 200: the creditor
			// details on the payment are checked against this record, so
			// settlement has to read it rather than guess.
			_ = json.NewEncoder(w).Encode(map[string]string{
				"id":                    strings.TrimPrefix(r.URL.Path, "/oversight/v1/beneficiaries/"),
				"account_name":          "B4B Merchant",
				"account_number":        "B4B00000000000001",
				"financial_institution": "B4BBANKAA",
				"sanctions_status":      "pass",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/oversight/v1/payments":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "b4b_test1", "status": "B4BAccepted"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer b4b.Close()

	dir := t.TempDir()
	a := newTestApp(t, dir)
	a.bankURL = bank.URL
	a.b4bURL = b4b.URL

	ids, err := a.runBatch("2026-09-03", "2026-09-03", "")
	if err != nil {
		t.Fatalf("runBatch: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("expected 1 record, got %d: %v", len(ids), ids)
	}

	rec, err := a.store.Get(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != settlement.StateSettled {
		t.Fatalf("state = %s, want Settled", rec.State)
	}
	if rec.MerchantID != "GB00SIM0000000000003" {
		t.Fatalf("MerchantID = %q", rec.MerchantID)
	}
	if rec.Report == nil || rec.Report.SaleAmountTotal != 15000 || rec.Report.ApprovedTransactionCount != 2 {
		t.Fatalf("report aggregates wrong: %+v", rec.Report)
	}
	// No payout: this endpoint generates the platform's own report from
	// its own ledger. Payouts are owed because the acquirer settled, and
	// are driven by the settlement file worldline_pull.go takes delivery
	// of -- paying out from both sources would pay every merchant twice.
	if rec.PayoutID != "" || rec.PayoutState != "" {
		t.Fatalf("report generation triggered a payout: id=%q state=%q", rec.PayoutID, rec.PayoutState)
	}

	// Staged in both SFTP_OUT_DIR and SFTP_OUTBOUND_DIR.
	outFiles, err := a.stager.ListOut()
	if err != nil {
		t.Fatal(err)
	}
	if len(outFiles) != 3 {
		t.Fatalf("expected 3 staged files (the platform's own csv/xml/json), got %d", len(outFiles))
	}
	// The acquirer's settlement file must NOT be here. The platform
	// receives that file over SFTP; generating a copy of it locally was
	// the inversion this lab removed, and a staged Bambora file is how it
	// would come back.
	for _, f := range outFiles {
		if strings.Contains(f.Name(), "_ER_") || strings.Contains(f.Name(), "_AR_") {
			t.Fatalf("platform staged an acquirer settlement file %q -- it should only ever receive one", f.Name())
		}
	}
	if _, err := os.ReadFile(a.outboundDir + "/GB00SIM0000000000003/daily/GB00SIM0000000000003_2026-09-03_EUR_WX.csv"); err != nil {
		t.Fatalf("outbound copy missing: %v", err)
	}
}

func TestSubmitPayoutMarksSubmissionFailedOnUnreachableB4B(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)
	a.b4bURL = "http://127.0.0.1:1" // nothing listens here
	rec := &settlement.SettlementRecord{
		ID:         "set_unreachable",
		MerchantID: "GB00SIM0000000000003",
		Currency:   "EUR",
		Report:     sampleTestReport(),
	}
	if err := a.store.Create(rec); err != nil {
		t.Fatal(err)
	}
	a.submitPayout(rec, a.guard.start())
	got, err := a.store.Get(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PayoutState != "submission_failed" {
		t.Fatalf("PayoutState = %q, want submission_failed", got.PayoutState)
	}
	if got.PayoutID != "" {
		t.Fatalf("PayoutID = %q, want empty on failure", got.PayoutID)
	}
}

// balanceServer returns an httptest server implementing Banking Circle's
// internal balance-proxy shape (docs/ARCHITECTURE-phase3-corrections.md
// section 1) reporting a fixed intraDayAmount.
func balanceServer(t *testing.T, intraDayAmount string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accountId": "bc_acc_sga_eur",
			"balances":  []map[string]string{{"intraDayAmount": intraDayAmount}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSubmitPayoutAwaitsFundingWhenSgaBalanceInsufficient(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)
	a.b4bURL = "http://127.0.0.1:1" // must never be reached: the gate should stop before B4B
	a.bcInternalURL = balanceServer(t, "0.00").URL
	rec := &settlement.SettlementRecord{
		ID:         "set_awaiting",
		MerchantID: "GB00SIM0000000000003",
		Currency:   "EUR",
		Report:     sampleTestReport(),
	}
	if err := a.store.Create(rec); err != nil {
		t.Fatal(err)
	}
	a.submitPayout(rec, a.guard.start())
	got, err := a.store.Get(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PayoutState != "awaiting_funding" {
		t.Fatalf("PayoutState = %q, want awaiting_funding", got.PayoutState)
	}
	if got.PayoutID != "" {
		t.Fatalf("PayoutID = %q, want empty while awaiting funding", got.PayoutID)
	}
}

func TestSubmitPayoutMarksBalanceCheckFailedWhenBankingCircleUnreachable(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)
	a.b4bURL = "http://127.0.0.1:1"        // must never be reached
	a.bcInternalURL = "http://127.0.0.1:1" // nothing listens here either
	rec := &settlement.SettlementRecord{
		ID:         "set_bc_unreachable",
		MerchantID: "GB00SIM0000000000003",
		Currency:   "EUR",
		Report:     sampleTestReport(),
	}
	if err := a.store.Create(rec); err != nil {
		t.Fatal(err)
	}
	a.submitPayout(rec, a.guard.start())
	got, err := a.store.Get(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PayoutState != "balance_check_failed" {
		t.Fatalf("PayoutState = %q, want balance_check_failed", got.PayoutState)
	}
}

func TestSubmitPayoutDeclinedByVerification(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)
	a.b4bURL = "http://127.0.0.1:1" // must never be reached: declined before B4B
	verify := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/aml-decision"):
			w.WriteHeader(202)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/verify-decision/"):
			_ = json.NewEncoder(w).Encode(map[string]string{"overallDecision": "DECLINE"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer verify.Close()
	a.verifyURL = verify.URL
	rec := &settlement.SettlementRecord{
		ID:         "set_declined",
		MerchantID: "GB00SIM0000000000004",
		Currency:   "EUR",
		Report:     sampleTestReport(),
	}
	if err := a.store.Create(rec); err != nil {
		t.Fatal(err)
	}
	a.submitPayout(rec, a.guard.start())
	got, err := a.store.Get(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PayoutState != "verification_declined" {
		t.Fatalf("PayoutState = %q, want verification_declined", got.PayoutState)
	}
}

func TestRetryPayoutRejectsRecordNotYetSettled(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)
	rec := &settlement.SettlementRecord{ID: "set_notsettled", MerchantID: "m1", Currency: "EUR", State: settlement.StateScheduled}
	if err := a.store.Create(rec); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/reports/settlement/set_notsettled/retry-payout", nil)
	req.SetPathValue("id", "set_notsettled")
	w := httptest.NewRecorder()
	a.retryPayout(w, req)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestRetryPayoutRejectsPayoutAlreadyInFlight(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)
	rec := &settlement.SettlementRecord{ID: "set_inflight", MerchantID: "m1", Currency: "EUR", State: settlement.StateSettled, PayoutState: "B4BTMApproved"}
	if err := a.store.Create(rec); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/reports/settlement/set_inflight/retry-payout", nil)
	req.SetPathValue("id", "set_inflight")
	w := httptest.NewRecorder()
	a.retryPayout(w, req)
	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestRetryPayoutSucceedsAfterFundingArrives exercises the exact causal
// chain this phase's SGA modeling exists to demonstrate (docs/
// ARCHITECTURE-phase3-corrections.md section 5): a payout attempted
// against an empty safeguarding account waits; once funds are available,
// retrying it proceeds through to B4B.
func TestRetryPayoutSucceedsAfterFundingArrives(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)
	a.bcInternalURL = balanceServer(t, "0.00").URL
	a.b4bURL = "http://127.0.0.1:1" // must not be reached on the first, insufficient attempt
	rec := &settlement.SettlementRecord{
		ID:         "set_retry",
		MerchantID: "GB00SIM0000000000005",
		Currency:   "EUR",
		State:      settlement.StateSettled,
		Report:     sampleTestReport(),
	}
	if err := a.store.Create(rec); err != nil {
		t.Fatal(err)
	}

	a.submitPayout(rec, a.guard.start())
	got, err := a.store.Get(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PayoutState != "awaiting_funding" {
		t.Fatalf("first attempt: PayoutState = %q, want awaiting_funding", got.PayoutState)
	}

	// Worldline's lump sum "lands": Banking Circle now reports enough funds,
	// and a real B4B is reachable.
	a.bcInternalURL = balanceServer(t, "1000000.00").URL
	b4b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/oversight/v1/beneficiaries/"):
			// A real beneficiary body, not a bare 200: the creditor
			// details on the payment are checked against this record, so
			// settlement has to read it rather than guess.
			_ = json.NewEncoder(w).Encode(map[string]string{
				"id":                    strings.TrimPrefix(r.URL.Path, "/oversight/v1/beneficiaries/"),
				"account_name":          "B4B Merchant",
				"account_number":        "B4B00000000000001",
				"financial_institution": "B4BBANKAA",
				"sanctions_status":      "pass",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/oversight/v1/payments":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "b4b_retry1", "status": "B4BAccepted"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer b4b.Close()
	a.b4bURL = b4b.URL

	req := httptest.NewRequest(http.MethodPost, "/reports/settlement/set_retry/retry-payout", nil)
	req.SetPathValue("id", "set_retry")
	w := httptest.NewRecorder()
	a.retryPayout(w, req)
	if w.Code != 200 {
		t.Fatalf("retry-payout status = %d, want 200", w.Code)
	}
	got2, err := a.store.Get(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.PayoutState != "B4BAccepted" || got2.PayoutID != "b4b_retry1" {
		t.Fatalf("after retry: payout id=%q state=%q, want id=b4b_retry1 state=B4BAccepted", got2.PayoutID, got2.PayoutState)
	}
}

// A beneficiary whose sanctions status is not "pass" must not be paid.
// Only "pass" permits payments, and the status can move at any time under
// continuous screening -- so it is checked per payment, not once at
// onboarding.
func TestSubmitPayoutStopsOnABlockedBeneficiary(t *testing.T) {
	dir := t.TempDir()
	a := newTestApp(t, dir)
	a.bcInternalURL = balanceServer(t, "1000000.00").URL

	var paymentAttempted bool
	b4bSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/oversight/v1/beneficiaries/"):
			_ = json.NewEncoder(w).Encode(map[string]string{
				"id":                    "ben_blocked",
				"account_name":          "B4B Merchant",
				"account_number":        "B4B00000000000001",
				"financial_institution": "B4BBANKAA",
				"sanctions_status":      "fail",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/oversight/v1/payments":
			paymentAttempted = true
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "b4b_should_not_happen", "status": "B4BAccepted"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer b4bSrv.Close()
	a.b4bURL = b4bSrv.URL

	rec := &settlement.SettlementRecord{
		ID:         "set_blocked",
		MerchantID: "GB00SIM0000000000007",
		Currency:   "EUR",
		State:      settlement.StateSettled,
		Report:     sampleTestReport(),
	}
	if err := a.store.Create(rec); err != nil {
		t.Fatal(err)
	}
	a.submitPayout(rec, a.guard.start())

	if paymentAttempted {
		t.Fatal("a payment was submitted for a beneficiary whose sanctions_status is fail")
	}
	got, err := a.store.Get(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PayoutState != "sanctions_blocked" {
		t.Fatalf("PayoutState = %q, want sanctions_blocked", got.PayoutState)
	}
}
