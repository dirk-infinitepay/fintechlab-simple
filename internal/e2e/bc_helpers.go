package e2e

import (
	"context"
	"fmt"
	"strings"

	"github.com/webduvet/fintechlab-simple/internal/money"
)

// UsesPodBankRails is retained as a constant false.
//
// This tree is the standalone stack only — the kernel/pod half lives on its
// own branches. The scenarios still ask the question in a couple of places,
// and answering it here rather than editing five call sites keeps those
// scenarios readable as descriptions of the vendor flow rather than of a
// migration that is no longer happening.
func (e *Env) UsesPodBankRails() bool { return false }

func (e *Env) bcLabBase() string {
	if e.UsesPodBankRails() {
		return e.BankingCircleURL
	}
	return e.BankingCircleInternalURL
}

// bcReadBalance returns the posted balance string for accountID.
// Standalone: GET /accounts/{id} → {balance}.
// Pod: GET /accounts/{id}/balances → Connect balances DTO (posted_minor /
// result[].intraDayAmount).
func bcReadBalance(ctx context.Context, env *Env, hdr map[string]string, accountID string) (balance string, status int, err error) {
	if env.UsesPodBankRails() {
		var body map[string]any
		status, err = GetJSONWithHeaders(ctx, env.Client, env.BankingCircleURL+"/accounts/"+accountID+"/balances", hdr, &body)
		if err != nil {
			return "", status, fmt.Errorf("pod balance %s: %w", accountID, err)
		}
		if status == 404 {
			return "0.00", status, nil
		}
		if status != 200 {
			return "", status, fmt.Errorf("pod balance %s: status %d", accountID, status)
		}
		if v, ok := body["posted_minor"]; ok {
			if n, ok := asMinor(v); ok {
				return money.Format(n), status, nil
			}
		}
		if results, ok := body["result"].([]any); ok && len(results) > 0 {
			if m, ok := results[0].(map[string]any); ok {
				// The balance is the day's opening plus what has moved since.
				var total int64
				found := false
				for _, k := range []string{"beginOfDayAmount", "intraDayAmount"} {
					if s, ok := m[k].(string); ok && s != "" {
						if n, err := money.Parse(s); err == nil {
							total += n
							found = true
						}
					}
				}
				if found {
					return money.Format(total), status, nil
				}
			}
		}
		return "0.00", status, nil
	}
	var acct bcAccountResp
	status, err = GetJSONWithHeaders(ctx, env.Client, env.BankingCircleURL+"/accounts/"+accountID, hdr, &acct)
	if err != nil {
		return "", status, err
	}
	if status == 404 || status != 200 {
		return "0.00", status, nil
	}
	return acct.Balance, status, nil
}

func asMinor(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	}
	return 0, false
}

// bcFundSGA credits the safeguarding account the way each surface exposes.
// Standalone: POST /internal/incoming-payments (decimal amount).
// Pod: POST /sim/funding (amount_minor, named external source).
func bcFundSGA(ctx context.Context, env *Env, amountMajor, reference string) error {
	if env.UsesPodBankRails() {
		minor, err := money.Parse(amountMajor)
		if err != nil {
			return fmt.Errorf("fund sga: parse %q: %w", amountMajor, err)
		}
		status, err := PostJSON(ctx, env.Client, env.bcLabBase()+"/sim/funding", nil, map[string]any{
			"from":         "external:acquirer-settlement",
			"to":           "sga-eur",
			"amount_minor": minor,
			"currency":     "EUR",
			"reference":    reference,
		}, nil)
		if err != nil {
			return fmt.Errorf("pod sim/funding: %w", err)
		}
		if status != 200 && status != 202 {
			return fmt.Errorf("pod sim/funding: want 200/202, got %d", status)
		}
		return nil
	}
	status, err := PostJSON(ctx, env.Client, env.BankingCircleInternalURL+"/internal/incoming-payments", nil,
		map[string]string{"currency": "EUR", "amount": amountMajor, "reference": reference}, nil)
	if err != nil {
		return fmt.Errorf("simulate worldline lump sum landing: %w", err)
	}
	if status != 202 {
		return fmt.Errorf("simulate worldline lump sum landing: want 202, got %d", status)
	}
	return nil
}

// bcHandoffPayout creates an outgoing payment on the pod twin (POST
// /internal/handoff). Standalone receives this from B4B over
// /internal/payments — the e2e suite only drives handoff when observing the pod,
// until compose rewires standalone B4B at the pod handoff URL.
func bcHandoffPayout(ctx context.Context, env *Env, settlementID, toAccount, amountMajor string) error {
	if !env.UsesPodBankRails() {
		return nil
	}
	minor, err := money.Parse(amountMajor)
	if err != nil {
		return fmt.Errorf("handoff: parse amount %q: %w", amountMajor, err)
	}
	status, err := PostJSON(ctx, env.Client, env.bcLabBase()+"/internal/handoff", nil, map[string]any{
		"correlation":   settlementID,
		"from":          "sga-eur",
		"to":            toAccount,
		"amount_minor":  minor,
		"currency":      "EUR",
		"reference":     settlementID,
		"settlementId":  settlementID,
		"settlement_id": settlementID,
	}, nil)
	if err != nil {
		return fmt.Errorf("pod handoff: %w", err)
	}
	if status != 202 {
		return fmt.Errorf("pod handoff: want 202, got %d", status)
	}
	return nil
}

// ReceiverDeliveryBase is where the twin should POST notification webhooks.
// Host harness + compose twin defaults to the compose DNS name so the
// container can reach receiver; override with RECEIVER_DELIVERY_URL.
// ReceiverDeliveryBase is where a *vendor* should post, which is not always
// where the e2e suite reads from.
//
// The e2e suite runs on the host and reaches the receiver on loopback. The
// vendor posting the notification runs in a container, where 127.0.0.1 is
// itself — so registering the e2e suite's own URL as a destination produces a
// subscription that can never deliver. Translate it to the name the
// compose network uses. RECEIVER_DELIVERY_URL overrides both.
func (e *Env) ReceiverDeliveryBase() string {
	if e.ReceiverDeliveryURL != "" {
		return strings.TrimRight(e.ReceiverDeliveryURL, "/")
	}
	if strings.Contains(e.ReceiverURL, "127.0.0.1") || strings.Contains(e.ReceiverURL, "localhost") {
		return "https://receiver:8443"
	}
	return strings.TrimRight(e.ReceiverURL, "/")
}
