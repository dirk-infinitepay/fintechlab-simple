package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCoreLedgerBankListsAndOpens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/accounts":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": []map[string]string{
				{"id": "acc_b", "iban": "GB00SIM2", "holder": "Bob", "currency": "EUR", "balance": "50.00"},
				{"id": "acc_a", "iban": "GB00SIM1", "holder": "Alice", "currency": "EUR", "balance": "10.00"},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/accounts":
			var req map[string]string
			_ = json.NewDecoder(r.Body).Decode(&req)
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"id": "acc_new", "iban": "GB00SIM9", "holder": req["holder"],
				"currency": req["currency"], "balance": req["openingBalance"],
			})
		default:
			http.Error(w, "no", 404)
		}
	}))
	defer srv.Close()

	b := &CoreLedgerBank{BaseURL: srv.URL, Client: srv.Client()}
	got, err := b.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Ordered, so the list does not reshuffle itself between polls.
	if len(got) != 2 || got[0].ID != "acc_a" {
		t.Fatalf("accounts = %+v", got)
	}
	if got[0].Number != "GB00SIM1" {
		t.Fatalf("IBAN not mapped onto the common shape: %+v", got[0])
	}

	made, err := b.OpenAccount(context.Background(), "Carol", "GBP", "12.34")
	if err != nil {
		t.Fatal(err)
	}
	if made.Holder != "Carol" || made.Currency != "GBP" || made.Balance != "12.34" {
		t.Fatalf("opened account = %+v", made)
	}
}

func TestBankErrorsCarryTheServersOwnMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"holder required"}`, 400)
	}))
	defer srv.Close()
	b := &CoreLedgerBank{BaseURL: srv.URL, Client: srv.Client()}
	_, err := b.OpenAccount(context.Background(), "", "EUR", "")
	if err == nil {
		t.Fatal("a 400 was reported as success")
	}
	// The UI shows this verbatim; a bare "request failed" would make the
	// operator go and read container logs for a form-validation error.
	if got := err.Error(); !strings.Contains(got, "holder required") {
		t.Fatalf("error = %q, lost the server's message", got)
	}
}

func TestBankingCircleUsesRealCredentialsAndCachesTheToken(t *testing.T) {
	var authorizes int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/authorizations/authorize":
			if u, p, ok := r.BasicAuth(); !ok || u == "" || p == "" {
				http.Error(w, "basic auth required", 401)
				return
			}
			atomic.AddInt64(&authorizes, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "tok", "expires_in": 900, "token_type": "bearer",
			})
		case "/accounts":
			if r.Header.Get("Authorization") != "Bearer tok" {
				http.Error(w, "unauthorized", 401)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": []map[string]string{
				{"id": "bc_acc_sga_eur", "viban": "BC00SIM1", "holder": "SGA", "currency": "EUR", "balance": "1000.00"},
			}})
		default:
			http.Error(w, "no", 404)
		}
	}))
	defer srv.Close()

	b := &BankingCircleBank{BaseURL: srv.URL, Client: srv.Client(), User: "sim", Password: "sim"}
	for i := 0; i < 3; i++ {
		got, err := b.Accounts(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Number != "BC00SIM1" {
			t.Fatalf("accounts = %+v", got)
		}
	}
	// One exchange for three reads: re-authorizing per request would be a
	// different thing from what a real client does.
	if n := atomic.LoadInt64(&authorizes); n != 1 {
		t.Fatalf("authorize called %d times, want 1", n)
	}
}

func TestBankingCircleCannotOpenAccounts(t *testing.T) {
	b := &BankingCircleBank{}
	// Real Banking Circle has no account-creation endpoint. The UI needs
	// to be able to tell "not supported" from "failed".
	if _, err := b.OpenAccount(context.Background(), "X", "EUR", ""); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if b.Meta().CanOpen {
		t.Fatal("the UI would offer an account-opening form that cannot work")
	}
}

func TestBanksLookup(t *testing.T) {
	core := &CoreLedgerBank{}
	bs := NewBanks(core, &BankingCircleBank{})
	if got := bs.List(); len(got) != 2 {
		t.Fatalf("List = %d banks", len(got))
	}
	if _, ok := bs.Backend("core-ledger"); !ok {
		t.Fatal("core-ledger not found by id")
	}
	if _, ok := bs.Backend("nope"); ok {
		t.Fatal("Backend invented a bank")
	}
}

// Banking Circle's tokens do not survive a restart or an "all" reset. A
// client holding one gets 401 until it asks for a new one, which it does
// once, at once, rather than when its cached token would have expired.
func TestBankingCircleReauthorizesWhenItsTokenIsNoLongerHonoured(t *testing.T) {
	var mu sync.Mutex
	valid := map[string]bool{}
	issued, authorizes := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/authorizations/authorize":
			authorizes++
			issued++
			tok := fmt.Sprintf("tok%d", issued)
			valid[tok] = true
			fmt.Fprintf(w, `{"access_token":%q,"expires_in":3600}`, tok)
		case "/accounts":
			if !valid[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] {
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"error":"invalid token"}`))
				return
			}
			_, _ = w.Write([]byte(`{"accounts":[]}`))
		}
	}))
	defer srv.Close()
	b := &BankingCircleBank{BaseURL: srv.URL, Client: srv.Client()}
	if _, err := b.Accounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	valid = map[string]bool{} // the bank was reset
	mu.Unlock()
	if _, err := b.Accounts(context.Background()); err != nil {
		t.Fatalf("after the bank dropped its tokens: %v", err)
	}
	if authorizes != 2 {
		t.Errorf("authorized %d times, want 2: once at first, once after the 401", authorizes)
	}
	// A 401 that a fresh token does not cure is reported, not retried forever.
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/accounts" {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"x","expires_in":3600}`))
	})
	if _, err := b.Accounts(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("a lasting 401 = %v, want it reported", err)
	}
}
