package e2e

import (
	"context"
	"fmt"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/money"
)

type paymentResp struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type accountResp struct {
	ID      string `json:"id"`
	IBAN    string `json:"iban"`
	Balance string `json:"balance"`
}

type eventsResp struct {
	Events []struct {
		ID        string `json:"id"`
		Type      string `json:"type"`
		PaymentID string `json:"paymentId"`
	} `json:"events"`
}

// PaymentAPIToWebhook is the e2e version of scripts/demo-payment.sh,
// with stronger assertions: it also checks the idempotency replay debits
// nothing a second time, and that both account balances moved by exactly
// the payment amount (not just that a webhook eventually showed up).
//
// This exercises cmd/paymentapi (a generic B4B-style payment facade
// simulation) end to end through bank/notifier/receiver — it does NOT
// touch the real B4B mock (cmd/b4b) at all, despite this scenario's
// predecessor being misnamed "b4b-payment-to-webhook". Real B4B is a
// payout rail Infinite calls, not an inbound-payment facade (see
// docs/ARCHITECTURE-vendor-corrections.md section 4); the genuine B4B
// flow is exercised by BankingCirclePayout, triggered as a side effect of
// WorldlineSettlementToSFTP's settlement batch.
func PaymentAPIToWebhook() Scenario {
	return Scenario{
		Name: "payment-api-to-webhook",
		Run: func(ctx context.Context, env *Env, state *State) error {
			var alice0, merchant0 accountResp
			if _, err := GetJSON(ctx, env.Client, env.BankURL+"/accounts/acc_alice", &alice0); err != nil {
				return fmt.Errorf("baseline alice: %w", err)
			}
			if _, err := GetJSON(ctx, env.Client, env.BankURL+"/accounts/acc_merchant", &merchant0); err != nil {
				return fmt.Errorf("baseline merchant: %w", err)
			}

			key := fmt.Sprintf("harness-%d", time.Now().UnixNano())
			reqBody := map[string]string{
				"debtorAccountId": "acc_alice",
				"creditorIban":    "GB00SIM0000000000003",
				"amount":          "25.00",
				"currency":        "EUR",
				"reference":       "harness-b4b",
			}
			hdr := map[string]string{"Idempotency-Key": key}

			var p paymentResp
			status, err := PostJSON(ctx, env.Client, env.PaymentAPIURL+"/payments", hdr, reqBody, &p)
			if err != nil {
				return fmt.Errorf("create payment: %w", err)
			}
			if status != 201 {
				return fmt.Errorf("create payment: want 201, got %d", status)
			}
			if p.Status != "accepted" {
				return fmt.Errorf("payment status: want accepted, got %q", p.Status)
			}

			// Idempotency replay: same key must return the same id, 200 not
			// 201, and must not debit a second time (checked below).
			var replay paymentResp
			status, err = PostJSON(ctx, env.Client, env.PaymentAPIURL+"/payments", hdr, reqBody, &replay)
			if err != nil {
				return fmt.Errorf("replay payment: %w", err)
			}
			if status != 200 || replay.ID != p.ID {
				return fmt.Errorf("idempotency replay: want 200/%s, got %d/%s", p.ID, status, replay.ID)
			}

			var alice1, merchant1 accountResp
			if _, err := GetJSON(ctx, env.Client, env.BankURL+"/accounts/acc_alice", &alice1); err != nil {
				return fmt.Errorf("post alice: %w", err)
			}
			if _, err := GetJSON(ctx, env.Client, env.BankURL+"/accounts/acc_merchant", &merchant1); err != nil {
				return fmt.Errorf("post merchant: %w", err)
			}
			wantAlice, err := shiftBalance(alice0.Balance, "-25.00")
			if err != nil {
				return err
			}
			wantMerchant, err := shiftBalance(merchant0.Balance, "25.00")
			if err != nil {
				return err
			}
			if alice1.Balance != wantAlice {
				return fmt.Errorf("alice balance: want %s (single debit), got %s", wantAlice, alice1.Balance)
			}
			if merchant1.Balance != wantMerchant {
				return fmt.Errorf("merchant balance: want %s (single credit), got %s", wantMerchant, merchant1.Balance)
			}

			// Not decorative: the webhook must have landed for THIS payment,
			// not merely be "eventually consistent" for some payment.
			if err := PollUntil(ctx, 20*time.Second, 500*time.Millisecond, func() (bool, error) {
				var events eventsResp
				if _, err := GetJSON(ctx, env.Client, env.ReceiverURL+"/events", &events); err != nil {
					return false, err
				}
				for _, e := range events.Events {
					if e.PaymentID == p.ID && e.Type == "payment.accepted" {
						return true, nil
					}
				}
				return false, fmt.Errorf("no payment.accepted event yet for %s", p.ID)
			}); err != nil {
				return err
			}
			state.Set("payment_id", p.ID)
			return nil
		},
	}
}

func shiftBalance(balance, delta string) (string, error) {
	b, err := money.Parse(balance)
	if err != nil {
		return "", fmt.Errorf("e2e: parse balance %q: %w", balance, err)
	}
	d, err := money.Parse(delta)
	if err != nil {
		return "", fmt.Errorf("e2e: parse delta %q: %w", delta, err)
	}
	return money.Format(b + d), nil
}
