package console

import (
	"context"
	"crypto/rsa"
	"fmt"
	"net/http"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/b4b"
)

// Provisioning is what makes the Merchants view more than a form: a
// merchant only exists to the money flow once its outlets are known to the
// payout rail and the acquirer has traded on them.
//
// Both steps go through the vendors' own public surfaces -- B4B's
// beneficiary registration and Worldline's transaction seeding -- so
// anything the console can do here, a shell script can do too. That is
// deliberate: a control panel that is the only way to reach a state makes
// that state untestable.

// Provisioner registers outlets with B4B and seeds acquired transactions
// at Worldline.
type Provisioner struct {
	Client       *http.Client
	B4BURL       string
	B4BKey       *rsa.PrivateKey
	B4BKeyID     string
	WorldlineURL string
	Registry     *Registry
}

// NewProvisioner loads B4B's keypair so the console can sign the RS512
// bearer token B4B verifies. A missing key is not fatal: the rest of the
// console works without it, and the Merchants view reports the reason
// rather than failing a click with a bare 500.
func NewProvisioner(client *http.Client, reg *Registry, b4bURL, keyPath, keyID, worldlineURL string) (*Provisioner, error) {
	p := &Provisioner{Client: client, Registry: reg, B4BURL: b4bURL, B4BKeyID: keyID, WorldlineURL: worldlineURL}
	key, err := b4b.LoadPrivateKey(keyPath)
	if err != nil {
		return p, err
	}
	p.B4BKey = key
	return p, nil
}

// Ready reports whether payout-rail provisioning is available.
func (p *Provisioner) Ready() bool { return p != nil && p.B4BKey != nil }

// AuthHeader is the RS512 bearer B4B's API asks for, signed with the key
// b4b generated, for a caller that builds its own request (the lab API's
// passthrough to the /sim/* hooks).
func (p *Provisioner) AuthHeader() (http.Header, error) { return p.authHeader() }

func (p *Provisioner) authHeader() (http.Header, error) {
	if p.B4BKey == nil {
		return nil, fmt.Errorf("%w: b4b signing key unavailable, set B4B_JWT_PRIVATE_KEY_PATH to the key b4b generated", ErrInvalid)
	}
	tok, err := b4b.SignBearerToken(p.B4BKey, p.B4BKeyID)
	if err != nil {
		return nil, err
	}
	return http.Header{"Authorization": []string{"Bearer " + tok}}, nil
}

// OutletProvision is the per-outlet result, reported whether or not it
// worked -- a partial provisioning run is a normal outcome and the UI has
// to be able to show which outlet failed and why.
type OutletProvision struct {
	OutletID        string `json:"outlet_id"`
	MID             string `json:"mid"`
	BeneficiaryID   string `json:"beneficiary_id,omitempty"`
	SanctionsStatus string `json:"sanctions_status,omitempty"`
	Error           string `json:"error,omitempty"`
}

// ProvisionMerchant registers every outlet of a merchant as a B4B
// beneficiary, keyed by MID so a payout can find it later.
func (p *Provisioner) ProvisionMerchant(ctx context.Context, merchantID string) ([]OutletProvision, error) {
	m, err := p.Registry.Merchant(merchantID)
	if err != nil {
		return nil, err
	}
	hdr, err := p.authHeader()
	if err != nil {
		return nil, err
	}
	out := make([]OutletProvision, 0, len(m.Outlets))
	for _, o := range m.Outlets {
		res := OutletProvision{OutletID: o.ID, MID: o.MID}
		req := map[string]string{
			"external_ref":          o.MID,
			"account_name":          o.AccountName,
			"account_number":        o.AccountNumber,
			"financial_institution": o.FinancialInstitution,
			"country":               o.Address.Country,
		}
		var ben struct {
			ID              string `json:"id"`
			SanctionsStatus string `json:"sanctions_status"`
		}
		if err := postJSON(ctx, p.Client, p.B4BURL+"/oversight/v1/beneficiaries", hdr, req, &ben); err != nil {
			res.Error = err.Error()
			out = append(out, res)
			continue
		}
		res.BeneficiaryID, res.SanctionsStatus = ben.ID, ben.SanctionsStatus
		if err := p.Registry.SetOutletBeneficiary(o.MID, ben.ID, ben.SanctionsStatus); err != nil {
			res.Error = err.Error()
		}
		out = append(out, res)
	}
	return out, nil
}

// SetSanctions moves an outlet's beneficiary between pass/review/fail
// through B4B's lab-only endpoint, so an operator can watch the gate stop
// a payout instead of reading that it would.
func (p *Provisioner) SetSanctions(ctx context.Context, mid, status string) error {
	hdr, err := p.authHeader()
	if err != nil {
		return err
	}
	var ben struct {
		ID              string `json:"id"`
		SanctionsStatus string `json:"sanctions_status"`
	}
	if err := putJSON(ctx, p.Client, p.B4BURL+"/sim/beneficiaries/"+mid+"/sanctions", hdr,
		map[string]string{"sanctions_status": status}, &ben); err != nil {
		return err
	}
	return p.Registry.SetOutletBeneficiary(mid, ben.ID, ben.SanctionsStatus)
}

// TradeParams describes the card trading to invent for a merchant.
type TradeParams struct {
	Count       int    `json:"count"`
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
	Date        string `json:"date"` // YYYY-MM-DD; empty means yesterday
}

// SeedTrading tells Worldline that a merchant's outlets took card
// payments. This is the acquirer's own data, posted to the acquirer --
// the platform never learns about a transaction except through the
// settlement file, and nothing here shortcuts that.
func (p *Provisioner) SeedTrading(ctx context.Context, merchantID string, tp TradeParams) (int, string, error) {
	m, err := p.Registry.Merchant(merchantID)
	if err != nil {
		return 0, "", err
	}
	if len(m.Outlets) == 0 {
		return 0, "", fmt.Errorf("%w: merchant %s has no outlets to trade on", ErrInvalid, merchantID)
	}
	if tp.Count <= 0 {
		tp.Count = 3
	}
	if tp.Count > 200 {
		return 0, "", fmt.Errorf("%w: count %d is too many, 200 is the cap", ErrInvalid, tp.Count)
	}
	if tp.AmountCents <= 0 {
		tp.AmountCents = 2500
	}
	if tp.Currency == "" {
		tp.Currency = m.Currency
	}
	date := tp.Date
	if date == "" {
		// Yesterday: Worldline settles T+1, so transactions dated today
		// are not in the file the next cycle cuts, and a demo that seeds
		// "now" and then runs a cycle would silently produce nothing.
		date = time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	}
	type txn struct {
		MID         string `json:"mid"`
		Currency    string `json:"currency"`
		AmountCents int64  `json:"amount_cents"`
		Date        string `json:"date"`
		MCC         string `json:"mcc,omitempty"`
		TerminalID  string `json:"terminal_id,omitempty"`
	}
	var batch []txn
	for _, o := range m.Outlets {
		for i := 0; i < tp.Count; i++ {
			batch = append(batch, txn{
				MID: o.MID, Currency: tp.Currency, AmountCents: tp.AmountCents,
				Date: date, MCC: o.MCC, TerminalID: o.TerminalID,
			})
		}
	}
	var resp struct {
		Accepted int `json:"accepted"`
	}
	if err := postJSON(ctx, p.Client, p.WorldlineURL+"/sim/transactions", nil, batch, &resp); err != nil {
		return 0, date, err
	}
	return resp.Accepted, date, nil
}

func putJSON(ctx context.Context, c *http.Client, url string, hdr http.Header, in, out any) error {
	req, err := newJSONRequest(ctx, http.MethodPut, url, hdr, in)
	if err != nil {
		return err
	}
	return do(c, req, out)
}
