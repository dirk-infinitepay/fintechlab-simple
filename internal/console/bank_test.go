package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCoreLedgerBankListsAndOpens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/sim/accounts":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": []map[string]string{
				{"id": "acc_b", "iban": "GB00SIM2", "holder": "Bob", "currency": "EUR", "balance": "50.00"},
				{"id": "acc_a", "iban": "GB00SIM1", "holder": "Alice", "currency": "EUR", "balance": "10.00"},
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/sim/accounts":
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

// The lab reads Banking Circle's books over its control port, with no
// vendor login: the platform's credentials are the platform's.
func TestBankingCircleIsReadOverItsControlPortWithNoCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.TLS != nil {
			http.Error(w, "the control port takes no vendor credentials", 400)
			return
		}
		switch r.URL.Path {
		case "/sim/accounts":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": []map[string]string{
				{"id": "bc_acc_sga_eur", "viban": "BC00SIM1", "holder": "SGA", "currency": "EUR", "balance": "1000.00"},
			}})
		case "/sim/payments/p1/return":
			http.Error(w, `{"error":"payment p1 is not processed"}`, 409)
		default:
			http.Error(w, "no", 404)
		}
	}))
	defer srv.Close()
	b := &BankingCircleBank{BaseURL: srv.URL, Client: srv.Client()}
	got, err := b.Accounts(context.Background())
	if err != nil || len(got) != 1 || got[0].Number != "BC00SIM1" {
		t.Fatalf("accounts = %+v, %v", got, err)
	}
	if err := b.Post(context.Background(), "/sim/payments/p1/return", nil); err == nil || !strings.Contains(err.Error(), "not processed") {
		t.Errorf("a refusal = %v, want the bank's own words", err)
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
