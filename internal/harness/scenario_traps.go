package harness

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/uuidx"
)

// Traps and reset, over Banking Circle's control port (docs/api/README.md,
// The control contract, Traps). Both drive a payout in over the bridge, as
// B4B does, and read what happened from the control port — the lab's own
// back link, with no vendor login.

type trapView struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Caught []struct {
		Subject  string                `json:"subject"`
		Applied  []struct{ To string } `json:"applied"`
		Diverted string                `json:"diverted"`
	} `json:"caught"`
	PendingSteps []struct {
		Subject string `json:"subject"`
	} `json:"pending_steps"`
}

type simPayments struct {
	Payments []struct {
		ID    string `json:"id"`
		State string `json:"state"`
	} `json:"payments"`
}

// harnessIBAN is a fake IBAN of this run's own, so a trap matching it
// catches only the payout this scenario sends.
func harnessIBAN(tag string) string {
	return fmt.Sprintf("GB00SIM%013d", (time.Now().UnixNano()/1000+int64(len(tag))*7919)%1e13)
}

func armTrap(ctx context.Context, env *Env, body map[string]any) (trapView, error) {
	var t trapView
	status, err := PostJSON(ctx, env.Client, env.BankingCircleControlURL+"/sim/traps", nil, body, &t)
	if err != nil {
		return t, fmt.Errorf("arm trap: %w", err)
	}
	if status != http.StatusCreated {
		return t, fmt.Errorf("arm trap: want 201, got %d", status)
	}
	return t, nil
}

func getTrap(ctx context.Context, env *Env, id string) (trapView, error) {
	var t trapView
	_, err := GetJSON(ctx, env.Client, env.BankingCircleControlURL+"/sim/traps/"+id, &t)
	return t, err
}

// bridgePayout hands Banking Circle a payout as B4B does at B4BTMApproved.
func bridgePayout(ctx context.Context, env *Env, paymentID, iban string) error {
	status, err := PostJSON(ctx, env.Client, env.BankingCircleInternalURL+"/internal/payments", nil, map[string]string{
		"paymentId": paymentID, "accountId": uuidx.Derive(uuidx.NamespaceLab, "harness-trap/"+paymentID),
		"amount": "1.00", "currency": "EUR", "externalRef": "harness-" + paymentID,
		"holder": "Harness Trap Ltd", "iban": iban,
	}, nil)
	if err != nil {
		return fmt.Errorf("bridge payout %s: %w", paymentID, err)
	}
	if status != http.StatusAccepted {
		return fmt.Errorf("bridge payout %s: want 202, got %d", paymentID, status)
	}
	return nil
}

func paymentState(ctx context.Context, env *Env, id string) (string, bool, error) {
	var ps simPayments
	if _, err := GetJSON(ctx, env.Client, env.BankingCircleControlURL+"/sim/payments", &ps); err != nil {
		return "", false, err
	}
	for _, p := range ps.Payments {
		if p.ID == id {
			return p.State, true, nil
		}
	}
	return "", false, nil
}

// BankingCircleTrapPaths: the owner's two examples, end to end. A payout to
// one IBAN is processed and then reversed a second later; a payout to
// another is rejected with a reason code. Each trap catches only its own.
func BankingCircleTrapPaths() Scenario {
	return Scenario{
		Name: "banking-circle-trap-paths",
		Run: func(ctx context.Context, env *Env, state *State) error {
			if status, err := PostJSON(ctx, env.Client, env.BankingCircleControlURL+"/sim/incoming-payments", nil,
				map[string]string{"currency": "EUR", "amount": "1000.00", "reference": "harness trap paths"}, nil); err != nil || status != http.StatusAccepted {
				return fmt.Errorf("fund the safeguarding account: %d %v", status, err)
			}
			ibanA, ibanB := harnessIBAN("a"), harnessIBAN("bb")
			reversed, err := armTrap(ctx, env, map[string]any{
				"label": "harness: processed, then reversed", "hook": "payment.outgoing",
				"match": map[string]any{"creditor.iban": ibanA},
				"effect": map[string]any{"path": []map[string]any{
					{"to": "OutgoingPaymentProcessed", "after": "100ms"},
					{"to": "Reversed", "after": "1s", "reason": "scheme"},
				}},
			})
			if err != nil {
				return err
			}
			rejected, err := armTrap(ctx, env, map[string]any{
				"label": "harness: rejected", "hook": "payment.outgoing",
				"match": map[string]any{"creditor.iban": ibanB},
				"effect": map[string]any{"path": []map[string]any{
					{"to": "OutgoingPaymentRejected", "reason_code": "AC01", "reason_description": "Incorrect account number"},
				}},
			})
			if err != nil {
				return err
			}
			stamp := time.Now().UnixNano()
			payA, payB := fmt.Sprintf("bcp_harness_%d_a", stamp), fmt.Sprintf("bcp_harness_%d_b", stamp)
			if err := bridgePayout(ctx, env, payA, ibanA); err != nil {
				return err
			}
			if err := bridgePayout(ctx, env, payB, ibanB); err != nil {
				return err
			}

			if err := PollUntil(ctx, 15*time.Second, 200*time.Millisecond, func() (bool, error) {
				t, err := getTrap(ctx, env, reversed.ID)
				if err != nil {
					return false, err
				}
				return len(t.Caught) == 1 && len(t.Caught[0].Applied) == 2, nil
			}); err != nil {
				t, _ := getTrap(ctx, env, reversed.ID)
				return fmt.Errorf("processed-then-reversed trap never finished its path: %+v: %w", t, err)
			}
			t, _ := getTrap(ctx, env, reversed.ID)
			if t.Caught[0].Subject != payA || t.Caught[0].Applied[0].To != "OutgoingPaymentProcessed" || t.Caught[0].Applied[1].To != "Reversed" {
				return fmt.Errorf("processed-then-reversed trap took %+v", t.Caught)
			}

			if err := PollUntil(ctx, 15*time.Second, 200*time.Millisecond, func() (bool, error) {
				st, ok, err := paymentState(ctx, env, payB)
				return ok && st == "OutgoingPaymentRejected", err
			}); err != nil {
				st, _, _ := paymentState(ctx, env, payB)
				return fmt.Errorf("payout %s ended %q, want OutgoingPaymentRejected: %w", payB, st, err)
			}
			if t, _ := getTrap(ctx, env, rejected.ID); len(t.Caught) != 1 || t.Caught[0].Subject != payB {
				return fmt.Errorf("rejected trap caught %+v, want only %s", t.Caught, payB)
			}
			return nil
		},
	}
}

// BankingCircleResetDropsAPayoutInFlight: a reset in the middle of a payout
// leaves nothing behind it — the step still waiting never lands, and the
// payout is gone. Runs last: a flow reset clears Banking Circle's payments.
func BankingCircleResetDropsAPayoutInFlight() Scenario {
	return Scenario{
		Name: "banking-circle-reset-drops-a-payout-in-flight",
		Run: func(ctx context.Context, env *Env, state *State) error {
			iban := harnessIBAN("reset")
			trap, err := armTrap(ctx, env, map[string]any{
				"label": "harness: processed in three seconds", "hook": "payment.outgoing",
				"match":  map[string]any{"creditor.iban": iban},
				"effect": map[string]any{"path": []map[string]any{{"to": "OutgoingPaymentProcessed", "after": "3s"}}},
			})
			if err != nil {
				return err
			}
			pay := fmt.Sprintf("bcp_harness_%d_reset", time.Now().UnixNano())
			if err := bridgePayout(ctx, env, pay, iban); err != nil {
				return err
			}
			if err := PollUntil(ctx, 5*time.Second, 50*time.Millisecond, func() (bool, error) {
				t, err := getTrap(ctx, env, trap.ID)
				return len(t.PendingSteps) == 1, err
			}); err != nil {
				return fmt.Errorf("the payout's step never became pending: %w", err)
			}
			if status, err := PostJSON(ctx, env.Client, env.BankingCircleControlURL+"/sim/reset", nil,
				map[string]any{"scope": "flow"}, nil); err != nil || status != http.StatusOK {
				return fmt.Errorf("reset: %d %v", status, err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3500 * time.Millisecond): // past the step's due time
			}
			if st, ok, err := paymentState(ctx, env, pay); err != nil || ok {
				return fmt.Errorf("after the reset payout %s is still there (%q, %v)", pay, st, err)
			}
			if status, _ := GetJSON(ctx, env.Client, env.BankingCircleControlURL+"/sim/traps/"+trap.ID, nil); status != http.StatusNotFound {
				return fmt.Errorf("trap %s survived the reset (GET answered %d)", trap.ID, status)
			}
			return nil
		},
	}
}
