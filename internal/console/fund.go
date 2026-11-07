package console

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Funding the safeguarding accounts.
//
// Worldline wires each day's settled lump sum into the platform's
// safeguarding account at Banking Circle; the platform's balance check waits
// for it before any payout leaves. In the lab that wire is one call on
// Banking Circle's internal bridge — Worldline itself makes it on its
// morning slot — and this is the same call from a button, for a run that
// did not come through a morning cycle.
//
// It lives here, not with the platform, because both ends are the lab's:
// one simulated vendor's money arriving at another. It used to be a script
// in the platform's runner that knew the bridge's address.

// SafeguardingAccounts are the accounts the lab's Banking Circle opens for
// the platform, one per settlement currency — the ids the connect kit hands
// out as BC_SAFEGUARDING_ACCOUNT_ID_<CCY>.
var SafeguardingAccounts = []struct{ Currency, Account string }{
	{"EUR", kitSGAEUR},
	{"GBP", kitSGAGBP},
}

// FundResult is what happened to one currency's account.
type FundResult struct {
	Currency string `json:"currency"`
	Account  string `json:"account"`
	Before   string `json:"before,omitempty"`
	Funded   string `json:"funded,omitempty"`
	Skipped  string `json:"skipped,omitempty"`
	Error    string `json:"error,omitempty"`
}

// FundSafeguarding credits amount to every safeguarding account that is
// empty, through Banking Circle's control port at controlURL: the same
// money landing as Worldline's wire on the bridge. An account already
// holding money is left alone, so pressing twice is not twice the money.
func FundSafeguarding(ctx context.Context, client *http.Client, controlURL, amount string) []FundResult {
	base := strings.TrimRight(controlURL, "/")
	out := make([]FundResult, 0, len(SafeguardingAccounts))
	for _, sga := range SafeguardingAccounts {
		res := FundResult{Currency: sga.Currency, Account: sga.Account}
		before, err := intraDayBalance(ctx, client, base, sga.Account)
		switch {
		case err != nil:
			res.Error = err.Error()
		case before > 0:
			res.Before = strconv.FormatFloat(before, 'f', 2, 64)
			res.Skipped = "already holds " + res.Before
		default:
			res.Before = strconv.FormatFloat(before, 'f', 2, 64)
			if err := credit(ctx, client, base, sga.Currency, amount); err != nil {
				res.Error = err.Error()
			} else {
				res.Funded = amount
			}
		}
		out = append(out, res)
	}
	return out
}

func intraDayBalance(ctx context.Context, client *http.Client, base, account string) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/sim/accounts/"+account+"/balances", nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("balance of %s: HTTP %d %s", account, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var body struct {
		Balances []struct {
			BeginOfDayAmount string `json:"beginOfDayAmount"`
			IntraDayAmount   string `json:"intraDayAmount"`
		} `json:"balances"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, fmt.Errorf("balance of %s: %w", account, err)
	}
	if len(body.Balances) == 0 {
		return 0, nil
	}
	// The balance is the day's opening plus what has moved since.
	open, err := strconv.ParseFloat(body.Balances[0].BeginOfDayAmount, 64)
	if err != nil {
		return 0, fmt.Errorf("balance of %s: beginOfDayAmount: %w", account, err)
	}
	moved, err := strconv.ParseFloat(body.Balances[0].IntraDayAmount, 64)
	if err != nil {
		return 0, fmt.Errorf("balance of %s: intraDayAmount: %w", account, err)
	}
	return open + moved, nil
}

func credit(ctx context.Context, client *http.Client, base, currency, amount string) error {
	body, _ := json.Marshal(map[string]string{
		"currency": currency, "amount": amount, "reference": "lab API: safeguarding top-up",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/sim/incoming-payments", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 202 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("credit %s: HTTP %d %s", currency, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}
