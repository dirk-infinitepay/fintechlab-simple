package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/allowlist"
	"github.com/webduvet/fintechlab-simple/internal/b4b"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// peer is a stand-in for whoever b4b calls -- the client's callback
// endpoint, or Banking Circle's bridge -- that counts what arrives.
type peer struct {
	mu     sync.Mutex
	bodies []string
	status func(n int) int // the answer to the nth request (1-based); nil is 200
	srv    *httptest.Server
}

func newPeer(t *testing.T) *peer {
	p := &peer{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.bodies = append(p.bodies, string(b))
		n, status := len(p.bodies), p.status
		p.mu.Unlock()
		if status != nil {
			w.WriteHeader(status(n))
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *peer) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.bodies)
}

// testApp is b4b as main builds it, calling back to cb, bridging to bc and
// keeping its state at statePath.
func testApp(t *testing.T, cb, bc *peer, statePath string, delay time.Duration) *app {
	t.Helper()
	list, err := allowlist.Parse("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	a := &app{
		beneficiaries: b4b.NewBeneficiaryStore(func() string { return "ben_" + shortID() }),
		dir:           b4b.NewDirectory(nil),
		kid:           "b4b-test",
		client:        &http.Client{Timeout: 2 * time.Second},
		list:          list,
		backs:         []time.Duration{100 * time.Millisecond},
		maxTry:        3,
		bcURL:         bc.srv.URL,
		statePath:     statePath,
		callbackURL:   cb.srv.URL,
		payLog:        activity.New("payments", "Payouts received", ""),
		cbLog:         activity.New("callbacks", "Callbacks sent", ""),
	}
	a.customers = a.newCustomers()
	a.engine = b4b.NewEngine(delay, a.onTransition)
	if a.traps, err = newTraps(delay, nil); err != nil {
		t.Fatal(err)
	}
	a.dir.OnChange = a.persist
	a.beneficiaries.OnChange = a.persist
	return a
}

// idle waits until nothing b4b started is still running: no payment
// walking its phases, no callback or bridge call being delivered. Work a
// reset dropped counts until it has woken and seen the reset, so this is
// the positive signal that it did.
func idle(t *testing.T, a *app) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for a.engine.Walking() != 0 || a.engine.Running() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("still running: %d payment(s) walking, %d delivery(ies)", a.engine.Walking(), a.engine.Running())
		}
		time.Sleep(time.Millisecond)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func payment(id, callbackURL string) *b4b.Payment {
	return &b4b.Payment{
		ID: id, BeneficiaryID: "MID001", CallbackURL: callbackURL,
		Amount: b4b.Amount{Amount: "12.34", Currency: "EUR"}, CurrencyOfTransfer: "EUR",
		CreditorAccount: b4b.AccountRef{Account: "GB00SIM0000000000042"}, CreditorName: "Harness Merchant Ltd",
	}
}

func board(t *testing.T, a *app) {
	t.Helper()
	c, err := a.dir.CreateCompany(b4b.CompanyParams{
		ExternalRef: "mer_0001", LegalName: "Harness Merchant Ltd", CompanyType: "kyb",
		CompanyDocumentsWaived: true, PersonDocumentsWaived: true,
		Address: &b4b.Address{AddressLine1: "1 Simulation Way", City: "London", PostalCode: "EC1A 1AA", Country: "GB"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.beneficiaries.Register(b4b.RegisterParams{CompanyID: c.ID, ExternalRef: "MID001",
		AccountName: "Harness Merchant Ltd", AccountNumber: "GB00SIM0000000000042", FinancialInstitution: "SC112233"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.beneficiaries.SetStatus("MID001", b4b.StatusDisabled); err != nil {
		t.Fatal(err)
	}
}

// A flow reset between two runs: the payments, their callbacks and the
// activity go; the boarding stays, and the state file says so -- a restart
// after the reset comes back to exactly what the reset left.
func TestFlowResetKeepsBoardingAndWritesTheStateFile(t *testing.T) {
	cb, bc := newPeer(t), newPeer(t)
	path := filepath.Join(t.TempDir(), "state.json")
	a := testApp(t, cb, bc, path, 0)
	board(t, a)
	a.engine.Submit(payment("b4bp_1", cb.srv.URL))
	idle(t, a)
	if cb.count() != 5 || bc.count() != 1 {
		t.Fatalf("a payment's run: %d callbacks and %d bridge calls, want 5 and 1", cb.count(), bc.count())
	}

	res, err := a.reset("flow", "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"payments": 1, "payments_in_flight": 0, "callbacks_in_flight": 0, "activity_events": 5}
	for k, n := range want {
		if res.Cleared[k] != n {
			t.Errorf("cleared[%s] = %d, want %d (all: %v)", k, res.Cleared[k], n, res.Cleared)
		}
	}
	if len(res.Cleared) != len(want) {
		t.Errorf("cleared = %v, want only %v", res.Cleared, want)
	}
	for _, k := range append(b4b.Boarding, "jwt_keys", "force_failure_beneficiary_ids") {
		if !slices.Contains(res.Kept, k) {
			t.Errorf("kept = %v, missing %s", res.Kept, k)
		}
	}
	if n := len(a.cbLog.Recent(0)); n != 0 {
		t.Errorf("%d callback events after the reset", n)
	}

	s, err := b4b.LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Payments) != 0 {
		t.Errorf("the state file still holds %d payment(s)", len(s.Payments))
	}
	if len(s.Companies) != 1 || len(s.Addresses) != 1 || len(s.Beneficiaries) != 1 {
		t.Fatalf("boarding in the state file: %d companies, %d addresses, %d beneficiaries", len(s.Companies), len(s.Addresses), len(s.Beneficiaries))
	}
	if s.Beneficiaries[0].Status != b4b.StatusDisabled {
		t.Errorf("a beneficiary's status did not survive a flow reset: %s", s.Beneficiaries[0].Status)
	}

	// all: the state file as on first start.
	res, err = a.reset("all", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Cleared["companies"] != 1 || res.Cleared["beneficiaries"] != 1 || res.Cleared["addresses"] != 1 {
		t.Errorf("cleared = %v, want the company, its address and the beneficiary", res.Cleared)
	}
	if !slices.Equal(res.Kept, []string{"jwt_keys", "force_failure_beneficiary_ids"}) {
		t.Errorf("kept = %v, want the configuration only", res.Kept)
	}
	s, err = b4b.LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Companies)+len(s.Addresses)+len(s.People)+len(s.Extended)+len(s.Documents)+len(s.Vibans)+len(s.Beneficiaries)+len(s.Payments) != 0 {
		t.Errorf("the state file after an all reset: %+v", s)
	}
}

// The rule that matters most: a payment still walking its phases when the
// reset lands produces no later callback and no bridge call, and the state
// file it leaves holds nothing a restart would resume.
func TestAPaymentInFlightWhenTheResetLandsIsNeverCalledBackAbout(t *testing.T) {
	cb, bc := newPeer(t), newPeer(t)
	path := filepath.Join(t.TempDir(), "state.json")
	a := testApp(t, cb, bc, path, 50*time.Millisecond)
	a.engine.Submit(payment("b4bp_walking", cb.srv.URL))
	waitFor(t, "the B4BAccepted callback", func() bool { return cb.count() == 1 })

	res, err := a.reset("flow", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Cleared["payments"] != 1 || res.Cleared["payments_in_flight"] != 1 {
		t.Fatalf("cleared = %v, want the one payment, still walking", res.Cleared)
	}
	callbacks := cb.count()
	idle(t, a)
	if cb.count() != callbacks || bc.count() != 0 {
		t.Fatalf("after the reset: %d more callback(s), %d bridge call(s); want none",
			cb.count()-callbacks, bc.count())
	}

	s, err := b4b.LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Payments) != 0 {
		t.Fatalf("the state file still holds %d payment(s): a restart would resume them", len(s.Payments))
	}
	// The restart itself: nothing resumes.
	restarted := b4b.NewEngine(time.Millisecond, func(p *b4b.Payment, _ uint64) {
		t.Errorf("a restart resumed %s at %s", p.ID, p.State)
	})
	restarted.Restore(s)
	if restarted.Walking() != 0 {
		t.Fatalf("a restart resumed %d payment(s) the reset dropped", restarted.Walking())
	}
}

// The bridge call and the B4BTMApproved callback after it carry the epoch
// the payment's work began in. Begun before a reset, neither is made.
func TestTheBridgeCallIsNotMadeForAPaymentAResetDropped(t *testing.T) {
	cb, bc := newPeer(t), newPeer(t)
	a := testApp(t, cb, bc, "", 0)
	p := payment("b4bp_approved", cb.srv.URL)
	p.State = b4b.StateTMApproved

	before := a.engine.Gate().Begin(nil)
	if _, err := a.reset("flow", ""); err != nil {
		t.Fatal(err)
	}
	a.approveAndBridge(p, before)
	a.deliverPaymentCallback(before, p, nil)
	if bc.count() != 0 || cb.count() != 0 {
		t.Fatalf("work begun before the reset: %d bridge call(s), %d callback(s); want none", bc.count(), cb.count())
	}
	if n := len(a.cbLog.Recent(0)); n != 0 {
		t.Fatalf("work begun before the reset left %d activity event(s)", n)
	}

	// The same work begun after it goes through: the test can tell.
	a.approveAndBridge(p, a.engine.Gate().Begin(nil))
	if bc.count() != 1 || cb.count() != 1 {
		t.Fatalf("work begun after the reset: %d bridge call(s), %d callback(s); want 1 and 1", bc.count(), cb.count())
	}
}

// A callback waiting out its backoff when the reset lands is never retried.
func TestACallbackRetryingWhenTheResetLandsStops(t *testing.T) {
	cb, bc := newPeer(t), newPeer(t)
	cb.status = func(int) int { return 500 }
	a := testApp(t, cb, bc, "", 0)
	a.later(tenant.Default, a.engine.Gate().Begin(nil), func(started uint64) {
		a.deliverCallback(tenant.Default, started, "beneficiary ben_1", cb.srv.URL, map[string]string{"id": "ben_1"})
	})
	waitFor(t, "the first attempt", func() bool { return cb.count() == 1 })

	res, err := a.reset("flow", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Cleared["callbacks_in_flight"] != 1 {
		t.Fatalf("cleared = %v, want the one callback in flight", res.Cleared)
	}
	idle(t, a)
	if cb.count() != 1 {
		t.Fatalf("%d attempts, want the one before the reset", cb.count())
	}
	if n := len(a.cbLog.Recent(0)); n != 0 {
		t.Fatalf("a dropped callback was recorded as refused: %d event(s)", n)
	}
}
