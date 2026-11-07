package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// The Banks view. "Sim banks" in this lab are not one thing: the business
// bank is where a payout ends up — the merchants' own accounts, credited by
// Banking Circle — and Banking Circle is the vendor whose accounts are where
// the safeguarding money sits until then. Each is also a service card on
// the Vendors view; this view is their accounts. Both are listed, and the difference is stated rather than
// smoothed over -- an operator who does not know which of these holds the
// SGA cannot reason about a payout at all.

// ErrUnsupported is returned by a backend asked to do something the real
// vendor has no endpoint for. Not an internal error: a UI that cannot tell
// "this failed" from "this does not exist" teaches the wrong thing.
var ErrUnsupported = errors.New("console: not supported by this bank")

// BankAccount is the common shape across backends.
type BankAccount struct {
	ID       string `json:"id"`
	Number   string `json:"number"`
	Holder   string `json:"holder"`
	Currency string `json:"currency"`
	Balance  string `json:"balance"`
}

// Bank is one entry in the Banks list.
type Bank struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        Kind   `json:"kind"`
	ServiceID   string `json:"service_id"`
	Summary     string `json:"summary"`
	NumberLabel string `json:"number_label"`
	CanOpen     bool   `json:"can_open_account"`
	Note        string `json:"note,omitempty"`
}

// BankBackend reads and (where the real thing allows it) writes accounts.
type BankBackend interface {
	Meta() Bank
	Accounts(ctx context.Context) ([]BankAccount, error)
	OpenAccount(ctx context.Context, holder, currency, opening string) (BankAccount, error)
}

// Banks is the ordered set of backends.
type Banks struct {
	backends []BankBackend
}

// NewBanks assembles the list.
func NewBanks(backends ...BankBackend) *Banks { return &Banks{backends: backends} }

// List returns the metadata for every bank.
func (b *Banks) List() []Bank {
	out := make([]Bank, 0, len(b.backends))
	for _, be := range b.backends {
		out = append(out, be.Meta())
	}
	return out
}

// Backend returns the backend with the given id.
func (b *Banks) Backend(id string) (BankBackend, bool) {
	for _, be := range b.backends {
		if be.Meta().ID == id {
			return be, true
		}
	}
	return nil, false
}

// --- core ledger (the bank service) ----------------------------------

// CoreLedgerBank talks to cmd/bank over plain HTTP.
type CoreLedgerBank struct {
	BaseURL string
	Client  *http.Client
}

func (c *CoreLedgerBank) Meta() Bank {
	return Bank{
		ID: "core-ledger", Name: "Business bank", Kind: KindVendor, ServiceID: "bank",
		Summary:     "The merchants' own bank — the far end of every payout. Banking Circle credits each processed payout here by IBAN, and an outlet's account opens the first time money arrives for it, as a beneficiary bank's would. The InfinitePay accounts get the platform's fee; the demo accounts are make demo-payment's. In memory, fake GB00SIM IBANs.",
		NumberLabel: "IBAN", CanOpen: true,
	}
}

func (c *CoreLedgerBank) Accounts(ctx context.Context) ([]BankAccount, error) {
	var body struct {
		Accounts []struct {
			ID, IBAN, Holder, Currency, Balance string
		} `json:"accounts"`
	}
	if err := getJSON(ctx, c.Client, c.BaseURL+"/sim/accounts", nil, &body); err != nil {
		return nil, err
	}
	out := make([]BankAccount, 0, len(body.Accounts))
	for _, a := range body.Accounts {
		out = append(out, BankAccount{ID: a.ID, Number: a.IBAN, Holder: a.Holder, Currency: a.Currency, Balance: a.Balance})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (c *CoreLedgerBank) OpenAccount(ctx context.Context, holder, currency, opening string) (BankAccount, error) {
	req := map[string]string{"holder": holder, "currency": currency, "openingBalance": opening}
	var a struct{ ID, IBAN, Holder, Currency, Balance string }
	if err := postJSON(ctx, c.Client, c.BaseURL+"/sim/accounts", nil, req, &a); err != nil {
		return BankAccount{}, err
	}
	return BankAccount{ID: a.ID, Number: a.IBAN, Holder: a.Holder, Currency: a.Currency, Balance: a.Balance}, nil
}

// --- Banking Circle ---------------------------------------------------

// BankingCircleBank reads and steers Banking Circle through its control
// port (internal/simctl): the lab's own back link to its simulation, plain
// HTTP with no vendor login. Banking Circle's mTLS and Basic->Bearer
// exchange are for the platform calling its API; the lab looking at the
// bank's books, or making a payout come back, is not that, and holding the
// vendor's credentials to do it would make the lab one of the vendor's
// clients. Every control route answers what its vendor counterpart answers,
// so what is shown is what a client of the bank would see.
type BankingCircleBank struct {
	// BaseURL is Banking Circle's control address (http://banking-circle:9085).
	BaseURL string
	Client  *http.Client
}

func (b *BankingCircleBank) Meta() Bank {
	return Bank{
		ID: "banking-circle", Name: "Banking Circle", Kind: KindVendor, ServiceID: "banking-circle",
		Summary:     "The safeguarding accounts (SGA) the acquirer's lump sum lands in, plus the auto-vivified creditor account per merchant. Read over mTLS with a real bearer token.",
		NumberLabel: "vIBAN", CanOpen: false,
		Note: "Banking Circle has no account-opening endpoint -- accounts appear when they are first paid into. Create a merchant instead and pay it.",
	}
}

func (b *BankingCircleBank) Accounts(ctx context.Context) ([]BankAccount, error) {
	var body struct {
		Accounts []struct {
			ID, VIBAN, Holder, Currency, Balance string
		} `json:"accounts"`
	}
	if err := b.Get(ctx, "/sim/accounts", &body); err != nil {
		return nil, err
	}
	out := make([]BankAccount, 0, len(body.Accounts))
	for _, a := range body.Accounts {
		out = append(out, BankAccount{ID: a.ID, Number: a.VIBAN, Holder: a.Holder, Currency: a.Currency, Balance: a.Balance})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Get reads one of Banking Circle's control routes.
func (b *BankingCircleBank) Get(ctx context.Context, path string, out any) error {
	return getJSON(ctx, b.Client, b.BaseURL+path, nil, out)
}

// Post calls one of Banking Circle's control routes that does something.
// A refusal comes back in the bank's own words.
func (b *BankingCircleBank) Post(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.BaseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := b.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var body struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if body.Error != "" {
			return &statusError{resp.StatusCode, fmt.Sprintf("banking-circle POST %s: %s", path, body.Error)}
		}
		return &statusError{resp.StatusCode, fmt.Sprintf("banking-circle POST %s: HTTP %d", path, resp.StatusCode)}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (b *BankingCircleBank) OpenAccount(ctx context.Context, holder, currency, opening string) (BankAccount, error) {
	return BankAccount{}, fmt.Errorf("%w: banking circle accounts are auto-vivified on first payment", ErrUnsupported)
}

// --- tiny JSON helpers ------------------------------------------------
//
// internal/e2e has its own; these stay separate because the e2e suite's
// are shaped for assertions (they return the status code so a scenario can
// insist on a 422) and these are shaped for a UI (an error carries the
// server's own message so it can be shown verbatim).

func getJSON(ctx context.Context, c *http.Client, url string, hdr http.Header, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	return do(c, req, out)
}

func postJSON(ctx context.Context, c *http.Client, url string, hdr http.Header, in, out any) error {
	req, err := newJSONRequest(ctx, http.MethodPost, url, hdr, in)
	if err != nil {
		return err
	}
	return do(c, req, out)
}

func newJSONRequest(ctx context.Context, method, url string, hdr http.Header, in any) (*http.Request, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header[k] = v
	}
	return req, nil
}

// statusError is a peer's non-2xx answer: its message as the UI shows it,
// and the status kept for whoever has to act on it (a 401 re-authorizes).
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

func do(c *http.Client, req *http.Request, out any) error {
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &statusError{resp.StatusCode, fmt.Sprintf("%s %s: HTTP %d: %s", req.Method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(body)))}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}
