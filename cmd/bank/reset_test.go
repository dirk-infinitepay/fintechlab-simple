package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/webduvet/fintechlab-simple/internal/simctl"
)

func serve(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// TestResetPutsTheLedgerBackToItsSeed: a reset leaves the bank as it was on
// first start — the seeded accounts at their seed balance, nothing a run
// opened, an empty ledger and empty logs — and says what it cleared.
func TestResetPutsTheLedgerBackToItsSeed(t *testing.T) {
	for _, scope := range []string{simctl.ScopeFlow, simctl.ScopeAll} {
		t.Run(scope, func(t *testing.T) {
			s := newStore()
			credits, ledger := newLogs()
			mux := routes(s, credits, ledger)
			simctl.Mount(mux, control(s, credits, ledger, ":8081"))

			// A run: two payouts (one opens an account, one lands in a seeded
			// one), a transfer between seeded accounts, an account by hand.
			for _, body := range []string{
				`{"iban":"GB00SIMMERCH0000001","holder":"Southwind","amount":"125.00","currency":"EUR","reference":"SETL-1"}`,
				`{"iban":"GB00SIMINF00000001","amount":"3.00","currency":"EUR","reference":"SETL-1"}`,
			} {
				if w := serve(t, mux, "POST", "/internal/credit", body); w.Code != 200 {
					t.Fatalf("credit: %d %s", w.Code, w.Body)
				}
			}
			if w := serve(t, mux, "POST", "/transfers", `{"fromAccountId":"acc_alice","toAccountId":"acc_bob","amount":"25.00"}`); w.Code != 201 {
				t.Fatalf("transfer: %d %s", w.Code, w.Body)
			}
			if w := serve(t, mux, "POST", "/accounts", `{"holder":"Acme Ltd","openingBalance":"10.00"}`); w.Code != 201 {
				t.Fatalf("open: %d %s", w.Code, w.Body)
			}

			w := serve(t, mux, "POST", "/sim/reset", `{"scope":"`+scope+`"}`)
			if w.Code != 200 {
				t.Fatalf("reset: %d %s", w.Code, w.Body)
			}
			var got struct {
				Cleared map[string]int `json:"cleared"`
				Kept    []string       `json:"kept"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			want := map[string]int{"accounts": 2, "balances": 3, "credits": 2, "transfers": 1, "activity": 4}
			for k, n := range want {
				if got.Cleared[k] != n {
					t.Errorf("cleared[%s] = %d, want %d (all: %v)", k, got.Cleared[k], n, got.Cleared)
				}
			}
			if !slices.Equal(got.Kept, []string{"seeded_accounts"}) {
				t.Errorf("kept = %v", got.Kept)
			}

			seed := newStore()
			if len(s.accts) != len(seed.accts) || len(s.byIBAN) != len(seed.byIBAN) {
				t.Fatalf("accounts after reset = %d, want the %d seeded", len(s.accts), len(seed.accts))
			}
			for id, a := range seed.accts {
				if b := s.accts[id]; b == nil || b.Balance != a.Balance || b.cents != a.cents {
					t.Errorf("%s after reset = %+v, want %+v", id, b, a)
				}
			}
			if len(s.entries) != 0 {
				t.Errorf("ledger after reset = %v", s.entries)
			}
			if len(credits.Recent(10)) != 0 || len(ledger.Recent(10)) != 0 {
				t.Error("the activity logs should be empty after a reset")
			}

			// The bank works as it did on first start: the same payout opens
			// its account again, with the first ledger id.
			w = serve(t, mux, "POST", "/internal/credit", `{"iban":"GB00SIMMERCH0000001","amount":"1.00","currency":"EUR","reference":"SETL-2"}`)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"opened":true`) {
				t.Fatalf("credit after reset: %d %s", w.Code, w.Body)
			}
			if len(s.entries) != 1 || s.entries[0].ID != "ent_1" {
				t.Errorf("ledger after reset and one credit = %+v", s.entries)
			}
		})
	}
}

// TestResetOfAFreshBankClearsNothing: the counts are what was there, not
// what could have been.
func TestResetOfAFreshBankClearsNothing(t *testing.T) {
	s := newStore()
	credits, ledger := newLogs()
	mux := routes(s, credits, ledger)
	simctl.Mount(mux, control(s, credits, ledger, ":8081"))
	w := serve(t, mux, "POST", "/sim/reset", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"cleared":{"accounts":0,"activity":0,"balances":0,"credits":0,"transfers":0}`) {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
}
