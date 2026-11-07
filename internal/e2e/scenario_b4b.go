package e2e

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/b4b"
)

type b4bBeneficiaryResp struct {
	ID                   string `json:"id"`
	ExternalRef          string `json:"external_ref"`
	AccountName          string `json:"account_name"`
	AccountNumber        string `json:"account_number"`
	FinancialInstitution string `json:"financial_institution"`
	SanctionsStatus      string `json:"sanctions_status"`
}

type b4bPaymentStateResp struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	ExternalRef string `json:"external_ref"`
}

// b4bPaymentBody is a payout through B4B's Oversight API, as the platform
// sends one: to beneficiary benID, with creditor details that must agree
// with it. Lifecycle callbacks go to the settlement stand-in, as the
// platform's would.
func b4bPaymentBody(env *Env, benID, creditorAccount, creditorFI, creditorName, ref, amount string) map[string]any {
	return map[string]any{
		"external_ref":           ref,
		"beneficiary_id":         benID,
		"company_id":             "cmp_harness",
		"callback_url":           env.SettlementURL + "/internal/b4b-webhook",
		"sca_applied":            true,
		"amount":                 map[string]any{"amount": json.Number(amount), "currency": "EUR"},
		"currencyOfTransfer":     "EUR",
		"debtorViban":            map[string]any{"account": "GB00SIM0000000000001", "country": "GB"},
		"creditorAccount":        map[string]any{"account": creditorAccount, "financialInstitution": creditorFI, "country": "GB"},
		"creditorName":           creditorName,
		"chargeBearer":           "SHA",
		"requestedExecutionDate": time.Now().UTC().Format("2006-01-02"),
	}
}

// B4BOversightGates drives the two checks B4B applies before a payment
// reaches Banking Circle, and the recovery path for a missed callback.
//
// All three matter to a client and none could be exercised before: every
// beneficiary auto-vivified with a status of "CLEAR" (not one of the three
// real values), the creditor fields on a payment were never checked
// against the beneficiary they named, and there was no way to read a
// payment's current state after losing its callback.
func B4BOversightGates() Scenario {
	return Scenario{
		Name: "b4b-oversight-gates",
		Run: func(ctx context.Context, env *Env, state *State) error {
			hdr, err := b4bHeaders(env, state)
			if err != nil {
				return err
			}
			base := env.B4BURL + "/oversight/v1"

			// Register a beneficiary with real details, so the consistency
			// check has something concrete to check against.
			var ben b4bBeneficiaryResp
			status, err := PostJSON(ctx, env.Client, base+"/beneficiaries", hdr, map[string]any{
				"external_ref":          "harness-merchant-1",
				"account_name":          "Harness Merchant Ltd",
				"account_number":        "GB00SIM0000000000042",
				"financial_institution": "SC112233",
				"country":               "GB",
			}, &ben)
			if err != nil {
				return fmt.Errorf("register beneficiary: %w", err)
			}
			if status != 201 || ben.ID == "" {
				return fmt.Errorf("register beneficiary: want 201 + an id, got %d", status)
			}
			if ben.SanctionsStatus != "pass" {
				return fmt.Errorf("a freshly registered beneficiary has sanctions_status %q, want pass -- "+
					"only pass permits payments, and the value must be one of pass/review/fail",
					ben.SanctionsStatus)
			}

			payment := func(creditorAccount, creditorFI, creditorName, ref string) map[string]any {
				return b4bPaymentBody(env, ben.ID, creditorAccount, creditorFI, creditorName, ref, "12.34")
			}

			// Creditor details that contradict the beneficiary are refused
			// with 422 and nothing is forwarded. They are a consistency
			// check, not a recipient override: silently accepting a
			// mismatch is how money reaches the wrong account.
			mismatch, err := PostJSON(ctx, env.Client, base+"/payments", hdr,
				payment("GB00SIM9999999999999", "SC112233", "Harness Merchant Ltd", "harness-mismatch"), nil)
			if err != nil {
				return fmt.Errorf("payment with a mismatched creditor account: %w", err)
			}
			if mismatch != 422 {
				return fmt.Errorf("a payment whose creditorAccount contradicts its beneficiary = %d, want 422", mismatch)
			}
			nameMismatch, err := PostJSON(ctx, env.Client, base+"/payments", hdr,
				payment("GB00SIM0000000000042", "SC112233", "Somebody Else Ltd", "harness-name-mismatch"), nil)
			if err != nil {
				return fmt.Errorf("payment with a mismatched creditor name: %w", err)
			}
			if nameMismatch != 422 {
				return fmt.Errorf("a payment whose creditorName contradicts its beneficiary = %d, want 422", nameMismatch)
			}

			// Matching details are accepted.
			var created b4bPaymentStateResp
			okStatus, err := PostJSON(ctx, env.Client, base+"/payments", hdr,
				payment("GB00SIM0000000000042", "SC112233", "Harness Merchant Ltd", "harness-ok"), &created)
			if err != nil {
				return fmt.Errorf("payment with matching creditor details: %w", err)
			}
			if okStatus != 202 || created.ID == "" {
				return fmt.Errorf("payment with matching creditor details = %d, want 202 + an id", okStatus)
			}

			// The recovery path: callbacks are unsigned, unordered and may
			// repeat, with no replay endpoint, so reading the current
			// state is how a client that lost one catches up.
			var fetched b4bPaymentStateResp
			if err := PollUntil(ctx, 15*time.Second, 250*time.Millisecond, func() (bool, error) {
				st, err := GetJSONWithHeaders(ctx, env.Client, base+"/payments/"+created.ID, hdr, &fetched)
				if err != nil {
					return false, err
				}
				if st != 200 {
					return false, fmt.Errorf("GET /payments/%s = %d, want 200", created.ID, st)
				}
				if fetched.Status == "B4BTMApproved" || fetched.Status == "B4BFailed" {
					return true, nil
				}
				return false, fmt.Errorf("payment is at %s, still working", fetched.Status)
			}); err != nil {
				return fmt.Errorf("reading a payment's current state: %w", err)
			}
			if fetched.Status != "B4BTMApproved" {
				return fmt.Errorf("payment reached %s, want B4BTMApproved", fetched.Status)
			}
			if fetched.ExternalRef != "harness-ok" {
				return fmt.Errorf("external_ref = %q, want it echoed back for correlation", fetched.ExternalRef)
			}

			// Sanctions can move at any time under continuous screening. A
			// beneficiary that paid out a moment ago must stop paying out
			// the moment it does.
			var blocked b4bBeneficiaryResp
			// Screening is B4B's own, so the lab API flips it: the
			// beneficiary's sanctions hook on b4b.
			if _, err := env.Lab.Put(ctx, Sim("b4b", "beneficiaries/"+ben.ID+"/sanctions"),
				map[string]any{"sanctions_status": "fail"}, &blocked); err != nil {
				return fmt.Errorf("block beneficiary: %w", err)
			}
			if blocked.SanctionsStatus != "fail" {
				return fmt.Errorf("beneficiary sanctions_status = %q after blocking, want fail", blocked.SanctionsStatus)
			}
			afterBlock, err := PostJSON(ctx, env.Client, base+"/payments", hdr,
				payment("GB00SIM0000000000042", "SC112233", "Harness Merchant Ltd", "harness-after-block"), nil)
			if err != nil {
				return fmt.Errorf("payment against a blocked beneficiary: %w", err)
			}
			if afterBlock != 422 {
				return fmt.Errorf("a payment to a beneficiary with sanctions_status=fail = %d, want 422", afterBlock)
			}

			// And the current status is readable, which is the fallback
			// when the sanctions callback is missed.
			var reread b4bBeneficiaryResp
			if _, err := GetJSONWithHeaders(ctx, env.Client, base+"/beneficiaries/"+ben.ID, hdr, &reread); err != nil {
				return fmt.Errorf("re-read beneficiary: %w", err)
			}
			if reread.SanctionsStatus != "fail" {
				return fmt.Errorf("re-read beneficiary sanctions_status = %q, want fail", reread.SanctionsStatus)
			}
			return nil
		},
	}
}

// b4bBearer signs the RS512 bearer token B4B's Oversight API requires,
// using the keypair b4b generated and shares read-only. Signing with the
// key b4b itself verifies against is the point: a scenario that skipped
// the token would not be exercising the auth a real client has to get
// right.
// b4bHeaders is B4B's login, as the platform presents it: an RS512 bearer
// JWT, minted once a run.
func b4bHeaders(env *Env, state *State) (map[string]string, error) {
	token, ok := state.Get("b4b_bearer")
	if !ok {
		var err error
		if token, err = b4bBearer(env); err != nil {
			return nil, err
		}
		state.Set("b4b_bearer", token)
	}
	return map[string]string{"Authorization": "Bearer " + token}, nil
}

func b4bBearer(env *Env) (string, error) {
	pem, err := os.ReadFile(env.B4BJWTPrivateKeyPath)
	if err != nil {
		return "", fmt.Errorf("e2e: read B4B private key %s: %w", env.B4BJWTPrivateKeyPath, err)
	}
	block, _ := pemDecode(pem)
	if block == nil {
		return "", fmt.Errorf("e2e: %s holds no PEM block", env.B4BJWTPrivateKeyPath)
	}
	key, err := parseRSAPrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("e2e: parse B4B private key: %w", err)
	}
	return b4b.SignBearerToken(key, env.B4BJWTKeyID)
}

func pemDecode(b []byte) (*pem.Block, []byte) { return pem.Decode(b) }

// parseRSAPrivateKey accepts either PKCS#1 or PKCS#8, because which one a
// key file holds depends on how it was generated and a client that only
// reads one of them fails for reasons nobody will guess.
func parseRSAPrivateKey(der []byte) (*rsa.PrivateKey, error) {
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	any, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, err
	}
	key, ok := any.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("key is %T, want *rsa.PrivateKey", any)
	}
	return key, nil
}
