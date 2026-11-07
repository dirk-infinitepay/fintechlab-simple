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
	"sync"
	"time"
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
	if err := getJSON(ctx, c.Client, c.BaseURL+"/accounts", nil, &body); err != nil {
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
	if err := postJSON(ctx, c.Client, c.BaseURL+"/accounts", nil, req, &a); err != nil {
		return BankAccount{}, err
	}
	return BankAccount{ID: a.ID, Number: a.IBAN, Holder: a.Holder, Currency: a.Currency, Balance: a.Balance}, nil
}

// --- Banking Circle ---------------------------------------------------

// BankingCircleBank reads the safeguarding ledger through Banking Circle's
// own credentialed API -- mTLS plus the Basic->Bearer exchange, exactly as
// a real client would. Nothing here reaches around it: the console has no
// privileged back door into the vendor's state, which is what keeps the
// view honest.
type BankingCircleBank struct {
	BaseURL  string
	Client   *http.Client
	User     string
	Password string

	mu      sync.Mutex
	token   string
	expires time.Time
}

func (b *BankingCircleBank) Meta() Bank {
	return Bank{
		ID: "banking-circle", Name: "Banking Circle", Kind: KindVendor, ServiceID: "banking-circle",
		Summary:     "The safeguarding accounts (SGA) the acquirer's lump sum lands in, plus the auto-vivified creditor account per merchant. Read over mTLS with a real bearer token.",
		NumberLabel: "vIBAN", CanOpen: false,
		Note: "Banking Circle has no account-opening endpoint -- accounts appear when they are first paid into. Create a merchant instead and pay it.",
	}
}

// Bearer is the token a call to Banking Circle's credentialed listener
// carries, for a caller that builds its own request (the lab API's
// passthrough to the /sim/* hooks).
func (b *BankingCircleBank) Bearer(ctx context.Context) (string, error) { return b.bearer(ctx) }

// bearer returns a cached token, exchanging Basic credentials for a new one
// when it is missing or close to expiry.
func (b *BankingCircleBank) bearer(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.token != "" && time.Now().Before(b.expires) {
		return b.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.BaseURL+"/api/v1/authorizations/authorize", nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(b.User, b.Password)
	resp, err := b.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("banking-circle authorize: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("banking-circle authorize: HTTP %d", resp.StatusCode)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.AccessToken == "" {
		return "", errors.New("banking-circle authorize: no access_token")
	}
	b.token = body.AccessToken
	ttl := time.Duration(body.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Minute
	}
	// Re-exchange a little early: a token that expires mid-request shows
	// up as a confusing 401 in the UI rather than as an expiry.
	b.expires = time.Now().Add(ttl - ttl/5)
	return b.token, nil
}

func (b *BankingCircleBank) Accounts(ctx context.Context) ([]BankAccount, error) {
	tok, err := b.bearer(ctx)
	if err != nil {
		return nil, err
	}
	var body struct {
		Accounts []struct {
			ID, VIBAN, Holder, Currency, Balance string
		} `json:"accounts"`
	}
	hdr := http.Header{"Authorization": []string{"Bearer " + tok}}
	if err := getJSON(ctx, b.Client, b.BaseURL+"/accounts", hdr, &body); err != nil {
		return nil, err
	}
	out := make([]BankAccount, 0, len(body.Accounts))
	for _, a := range body.Accounts {
		out = append(out, BankAccount{ID: a.ID, Number: a.VIBAN, Holder: a.Holder, Currency: a.Currency, Balance: a.Balance})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// AuthorizedGet performs a GET against Banking Circle carrying a bearer
// token, for the read-only panels that are not about accounts. Same
// credentials, same listener -- the console never gets a privileged
// side-channel into a vendor.
func (b *BankingCircleBank) AuthorizedGet(ctx context.Context, path string, out any) error {
	tok, err := b.bearer(ctx)
	if err != nil {
		return err
	}
	return getJSON(ctx, b.Client, b.BaseURL+path, http.Header{"Authorization": []string{"Bearer " + tok}}, out)
}

// AuthorizedPost is AuthorizedGet's counterpart for the endpoints that do
// something. Same token, same client, so a console action goes through the
// credentialed path the Banks view already established rather than a
// second one that would have to learn mTLS all over again.
func (b *BankingCircleBank) AuthorizedPost(ctx context.Context, path string, out any) error {
	return b.AuthorizedDo(ctx, http.MethodPost, path, nil, out)
}

// AuthorizedDo is the same for any method and extra headers — the
// subscription activate and deactivate calls are PUTs carrying If-Match.
func (b *BankingCircleBank) AuthorizedDo(ctx context.Context, method, path string, hdr http.Header, out any) error {
	tok, err := b.bearer(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, b.BaseURL+path, nil)
	if err != nil {
		return err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := b.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The vendor's own words, not a status code translated into ours.
		var body struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if body.Error != "" {
			return fmt.Errorf("banking-circle %s %s: %s", method, path, body.Error)
		}
		return fmt.Errorf("banking-circle %s %s: HTTP %d", method, path, resp.StatusCode)
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
// internal/harness has its own; these stay separate because the harness's
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

func do(c *http.Client, req *http.Request, out any) error {
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d: %s", req.Method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}
