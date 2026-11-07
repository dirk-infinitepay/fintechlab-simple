package e2e

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Traps and reset, through the lab API (docs/api/lab-api.md, Traps). Each
// sends its payout the way the platform does: through B4B's Oversight API,
// which approves it and hands it to Banking Circle, where the
// payment.outgoing trap takes it. What Banking Circle did is read back
// through the lab API too.

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

// e2eIBAN is a fake IBAN of this run's own, so a trap matching it catches
// only the payout this scenario sends.
func e2eIBAN(tag string) string {
	return fmt.Sprintf("GB00SIM%013d", (time.Now().UnixNano()/1000+int64(len(tag))*7919)%1e13)
}

func armTrap(ctx context.Context, env *Env, body map[string]any) (trapView, error) {
	var t trapView
	status, err := env.Lab.Post(ctx, Sim("banking-circle", "traps"), body, &t)
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
	_, err := env.Lab.Get(ctx, Sim("banking-circle", "traps/"+id), &t)
	return t, err
}

// payoutThroughB4B pays 1.00 to a new beneficiary at iban, as the platform
// would: register the beneficiary at B4B, then a payment to it. B4B takes
// it to B4BTMApproved on its own and only then hands it to Banking Circle.
func payoutThroughB4B(ctx context.Context, env *Env, state *State, iban, ref string) error {
	hdr, err := b4bHeaders(env, state)
	if err != nil {
		return err
	}
	base := env.B4BURL + "/oversight/v1"
	const holder, institution = "Harness Trap Ltd", "SC112233"
	var ben b4bBeneficiaryResp
	status, err := PostJSON(ctx, env.Client, base+"/beneficiaries", hdr, map[string]any{
		"external_ref": ref, "account_name": holder, "account_number": iban,
		"financial_institution": institution, "country": "GB",
	}, &ben)
	if err != nil {
		return fmt.Errorf("register beneficiary %s: %w", iban, err)
	}
	if status != http.StatusCreated || ben.ID == "" {
		return fmt.Errorf("register beneficiary %s: want 201 + an id, got %d", iban, status)
	}
	status, err = PostJSON(ctx, env.Client, base+"/payments", hdr,
		b4bPaymentBody(env, ben.ID, iban, institution, holder, ref, "1.00"), nil)
	if err != nil {
		return fmt.Errorf("pay %s: %w", iban, err)
	}
	if status != http.StatusAccepted {
		return fmt.Errorf("pay %s: want 202, got %d", iban, status)
	}
	return nil
}

func paymentState(ctx context.Context, env *Env, id string) (string, bool, error) {
	payments, err := bcPayments(ctx, env)
	if err != nil {
		return "", false, err
	}
	for _, p := range payments {
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
			if err := bcFundSGA(ctx, env, "1000.00", "e2e trap paths"); err != nil {
				return fmt.Errorf("fund the safeguarding account: %w", err)
			}
			ibanA, ibanB := e2eIBAN("a"), e2eIBAN("bb")
			reversed, err := armTrap(ctx, env, map[string]any{
				"label": "e2e: processed, then reversed", "hook": "payment.outgoing",
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
				"label": "e2e: rejected", "hook": "payment.outgoing",
				"match": map[string]any{"creditor.iban": ibanB},
				"effect": map[string]any{"path": []map[string]any{
					{"to": "OutgoingPaymentRejected", "reason_code": "AC01", "reason_description": "Incorrect account number"},
				}},
			})
			if err != nil {
				return err
			}
			stamp := time.Now().UnixNano()
			if err := payoutThroughB4B(ctx, env, state, ibanA, fmt.Sprintf("e2e-trap-%d-a", stamp)); err != nil {
				return err
			}
			if err := payoutThroughB4B(ctx, env, state, ibanB, fmt.Sprintf("e2e-trap-%d-b", stamp)); err != nil {
				return err
			}

			// The first trap takes its payout and walks both steps.
			if err := PollUntil(ctx, 30*time.Second, 200*time.Millisecond, func() (bool, error) {
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
			if t.Caught[0].Applied[0].To != "OutgoingPaymentProcessed" || t.Caught[0].Applied[1].To != "Reversed" {
				return fmt.Errorf("processed-then-reversed trap took %+v", t.Caught)
			}
			payA := t.Caught[0].Subject
			if st, ok, err := paymentState(ctx, env, payA); err != nil || st != "Reversed" {
				return fmt.Errorf("payout %s is %q (found %v, %v), want Reversed", payA, st, ok, err)
			}

			// The second takes the other payout, and Banking Circle rejects it.
			var payB string
			if err := PollUntil(ctx, 30*time.Second, 200*time.Millisecond, func() (bool, error) {
				t, err := getTrap(ctx, env, rejected.ID)
				if err != nil || len(t.Caught) == 0 {
					return false, err
				}
				payB = t.Caught[0].Subject
				st, _, err := paymentState(ctx, env, payB)
				return st == "OutgoingPaymentRejected", err
			}); err != nil {
				st, _, _ := paymentState(ctx, env, payB)
				return fmt.Errorf("payout %q ended %q, want OutgoingPaymentRejected: %w", payB, st, err)
			}
			if t, _ := getTrap(ctx, env, rejected.ID); len(t.Caught) != 1 || payB == payA {
				return fmt.Errorf("rejected trap caught %+v, want only its own payout", t.Caught)
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
			if err := bcFundSGA(ctx, env, "10.00", "e2e reset"); err != nil {
				return fmt.Errorf("fund the safeguarding account: %w", err)
			}
			iban := e2eIBAN("reset")
			trap, err := armTrap(ctx, env, map[string]any{
				"label": "e2e: processed in three seconds", "hook": "payment.outgoing",
				"match":  map[string]any{"creditor.iban": iban},
				"effect": map[string]any{"path": []map[string]any{{"to": "OutgoingPaymentProcessed", "after": "3s"}}},
			})
			if err != nil {
				return err
			}
			if err := payoutThroughB4B(ctx, env, state, iban, fmt.Sprintf("e2e-reset-%d", time.Now().UnixNano())); err != nil {
				return err
			}
			var pay string
			if err := PollUntil(ctx, 15*time.Second, 50*time.Millisecond, func() (bool, error) {
				t, err := getTrap(ctx, env, trap.ID)
				if err != nil || len(t.PendingSteps) != 1 {
					return false, err
				}
				pay = t.PendingSteps[0].Subject
				return true, nil
			}); err != nil {
				return fmt.Errorf("the payout's step never became pending: %w", err)
			}
			if status, err := env.Lab.Post(ctx, "/v1/services/banking-circle/reset",
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
			if status, _ := env.Lab.Get(ctx, Sim("banking-circle", "traps/"+trap.ID), nil); status != http.StatusNotFound {
				return fmt.Errorf("trap %s survived the reset (GET answered %d)", trap.ID, status)
			}
			return nil
		},
	}
}
