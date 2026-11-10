package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// post sends a credit and returns the decoded response.
func post(t *testing.T, s *store, body string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/internal/credit", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.credit(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func accountFor(s *store, iban string) *account {
	return def(s).byIBAN[iban]
}

// def is the default environment's book: the bank as it was before there
// were environments.
func def(s *store) *book {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.books[tenant.Default]
}

// TestCreditOpensAnAccountOnFirstArrival: a beneficiary bank does not know
// an IBAN until money arrives for it. Opening on demand is what removes the
// need for anything to tell this service that a merchant exists.
func TestCreditOpensAnAccountOnFirstArrival(t *testing.T) {
	s := newStore()
	if accountFor(s, "GB00SIMMERCH0000001") != nil {
		t.Fatal("merchant accounts must not be seeded")
	}

	code, body := post(t, s, `{"iban":"GB00SIMMERCH0000001","holder":"Southwind Coffee Ltd",
		"amount":"125.00","currency":"EUR","reference":"SETL-1"}`)
	if code != http.StatusOK {
		t.Fatalf("status %d body %v", code, body)
	}
	if opened, _ := body["opened"].(bool); !opened {
		t.Error("the response should say an account was opened")
	}

	a := accountFor(s, "GB00SIMMERCH0000001")
	if a == nil {
		t.Fatal("no account was opened")
	}
	if a.Balance != "125.00" {
		t.Errorf("balance = %q, want 125.00", a.Balance)
	}
	if a.Kind != kindMerchant {
		t.Errorf("kind = %q, want %q", a.Kind, kindMerchant)
	}
	// The name the sender supplied, so the account is readable and not
	// merely reconcilable.
	if a.Holder != "Southwind Coffee Ltd" {
		t.Errorf("holder = %q, want the name the rail carried", a.Holder)
	}
	// Which credit opened it is the question asked afterwards.
	if a.OpenedBy != "SETL-1" {
		t.Errorf("opened_by = %q, want the settlement reference", a.OpenedBy)
	}
}

// TestCreditWithoutAHolderStillOpens: the name is a convenience, the IBAN
// is the identity. A rail that sends no name must not lose the money.
func TestCreditWithoutAHolderStillOpens(t *testing.T) {
	s := newStore()
	if code, body := post(t, s, `{"iban":"GB00NONAME","amount":"10.00","currency":"EUR","reference":"r"}`); code != http.StatusOK {
		t.Fatalf("status %d body %v", code, body)
	}
	a := accountFor(s, "GB00NONAME")
	if a == nil || a.Balance != "10.00" {
		t.Fatalf("account = %+v", a)
	}
	if !strings.Contains(a.Holder, "GB00NONAME") {
		t.Errorf("a nameless beneficiary should still be identifiable, got %q", a.Holder)
	}
}

// TestCreditAccumulates: a second settlement to the same merchant adds to
// the account rather than replacing it or opening a duplicate.
func TestCreditAccumulates(t *testing.T) {
	s := newStore()
	post(t, s, `{"iban":"GB00ACC","holder":"X","amount":"125.00","currency":"EUR","reference":"SETL-1"}`)
	_, body := post(t, s, `{"iban":"GB00ACC","holder":"X","amount":"80.50","currency":"EUR","reference":"SETL-2"}`)

	if opened, _ := body["opened"].(bool); opened {
		t.Error("the second credit must not report opening an account")
	}
	a := accountFor(s, "GB00ACC")
	if a.Balance != "205.50" {
		t.Errorf("balance = %q, want 205.50", a.Balance)
	}
	if a.OpenedBy != "SETL-1" {
		t.Errorf("opened_by = %q, it should still name the credit that opened it", a.OpenedBy)
	}
}

// TestCreditRefusesACurrencyMismatch: crediting EUR into a GBP account
// would make the balance a number that means nothing.
func TestCreditRefusesACurrencyMismatch(t *testing.T) {
	s := newStore()
	post(t, s, `{"iban":"GB00CCY","holder":"X","amount":"10.00","currency":"GBP","reference":"r1"}`)
	code, body := post(t, s, `{"iban":"GB00CCY","holder":"X","amount":"10.00","currency":"EUR","reference":"r2"}`)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d body %v", code, body)
	}
	if a := accountFor(s, "GB00CCY"); a.Balance != "10.00" {
		t.Errorf("the refused credit must not move the balance: %q", a.Balance)
	}
}

// TestBothCurrenciesHaveAnInfiniteAccount: INFINITE_SETTLEMENT lands here,
// and "did the platform get its share" should be a balance rather than an
// inference from somebody else's ledger.
func TestBothCurrenciesHaveAnInfiniteAccount(t *testing.T) {
	s := newStore()
	seen := map[string]bool{}
	for _, a := range def(s).accts {
		if a.Kind == kindInfinite {
			seen[a.Currency] = true
		}
	}
	for _, ccy := range []string{"EUR", "GBP"} {
		if !seen[ccy] {
			t.Errorf("no InfinitePay account in %s", ccy)
		}
	}
}

// TestCreditRefusesNonsense: an amount that is not money, or no IBAN at
// all, is a bad request rather than a silently opened empty account.
func TestCreditRefusesNonsense(t *testing.T) {
	s := newStore()
	for _, body := range []string{
		`{"amount":"10.00","currency":"EUR"}`,
		`{"iban":"GB00X","amount":"not-money","currency":"EUR"}`,
		`{"iban":"GB00X","amount":"-5.00","currency":"EUR"}`,
	} {
		if code, _ := post(t, s, body); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, code)
		}
	}
	if len(def(s).entries) != 0 {
		t.Error("a refused credit must not write a ledger entry")
	}
}

// TestACreditIsLoggedWhereTheConsoleReadsIt: the far end of a payout is
// visible — the credits log says what landed, where, and whether the
// account was opened by it — so the Dashboard's last arrow has something
// true to count.
func TestACreditIsLoggedWhereTheConsoleReadsIt(t *testing.T) {
	credits, ledger := newLogs()
	mux, ctl := testSurfaces(t, newStore(), credits, ledger)
	r := httptest.NewRequest(http.MethodPost, "/internal/credit", strings.NewReader(
		`{"iban":"GB00SIMMERCH0000009","holder":"Quiet Coffee","amount":"10.50","currency":"EUR","reference":"SETL-9"}`))
	mux.ServeHTTP(httptest.NewRecorder(), r)

	// On the control port, where the lab API reads it.
	w := httptest.NewRecorder()
	ctl.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/sim/activity", nil))
	var got struct {
		Logs []struct {
			Name   string `json:"name"`
			Events []struct {
				Op, Summary, Status string
				Detail              map[string]string
			} `json:"events"`
		} `json:"logs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.Logs) != 2 || got.Logs[0].Name != "credits" {
		t.Fatalf("activity = %s", w.Body.String())
	}
	evs := got.Logs[0].Events
	if len(evs) != 1 || evs[0].Op != "credit" || evs[0].Status != "ok" ||
		evs[0].Summary != "credited 10.50 EUR to GB00SIMMERCH0000009 (Quiet Coffee) — account opened" ||
		evs[0].Detail["reference"] != "SETL-9" {
		t.Fatalf("credit event = %+v", evs)
	}
}

// TestACreditByAccountNumberOpensThatAccount: a UK merchant is paid by
// account number and sort code, not IBAN. The account opens under them.
func TestACreditByAccountNumberOpensThatAccount(t *testing.T) {
	s := newStore()
	code, body := post(t, s, `{"account_number":"12345678","financial_institution":"SC112233",
		"holder":"Quiet Coffee","amount":"12.50","currency":"GBP","reference":"sttl_1"}`)
	if code != http.StatusOK {
		t.Fatalf("status %d body %v", code, body)
	}
	a := accountFor(s, "SC112233 12345678")
	if a == nil || a.Balance != "12.50" || a.Holder != "Quiet Coffee" || a.Currency != "GBP" {
		t.Fatalf("account = %+v", a)
	}
	if code, _ := post(t, s, `{"amount":"1.00"}`); code != http.StatusBadRequest {
		t.Errorf("a credit naming no account = %d, want 400", code)
	}
}
