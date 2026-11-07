package e2e

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/bankingcircle"
)

type bcAccountResp struct {
	ID      string `json:"id"`
	VIBAN   string `json:"viban"`
	Balance string `json:"balance"`
}

type bcPayment struct {
	ID           string `json:"id"`
	SettlementID string `json:"settlementId"`
	ToAccountID  string `json:"toAccountId"`
	Amount       string `json:"amount"`
	Currency     string `json:"currency"`
	State        string `json:"state"`
}

type bcPaymentsResp struct {
	Payments []bcPayment `json:"payments"`
}

// payoutResp mirrors cmd/settlement's lab-only introspection view (GET
// /reports/settlement/{id}/payout) of a record's B4B payout submission.
type payoutResp struct {
	PayoutID    string `json:"payout_id"`
	PayoutState string `json:"payout_state"`
}

type bcAuthorizeResp struct {
	AccessToken string `json:"access_token"`
}

// bankingCircleBearer completes Banking Circle's real M2M auth flow --
// mTLS (already presented by env.Client, see EnvFromOS) plus an HTTP
// Basic -> Bearer token exchange (docs/ARCHITECTURE-vendor-corrections.md
// section 3) -- and returns a header map ready to attach to every
// subsequent Banking Circle call. Any non-empty Basic credential is
// accepted by this mock (no real credential store), matching this lab's
// existing "obviously fake, deliberately simple" auth posture elsewhere.
func bankingCircleBearer(ctx context.Context, env *Env) (map[string]string, error) {
	basic := base64.StdEncoding.EncodeToString([]byte("harness:harness"))
	var auth bcAuthorizeResp
	status, err := GetJSONWithHeaders(ctx, env.Client, env.BankingCircleURL+"/api/v1/authorizations/authorize",
		map[string]string{"Authorization": "Basic " + basic}, &auth)
	if err != nil {
		return nil, fmt.Errorf("banking-circle authorize: %w", err)
	}
	if status != 200 || auth.AccessToken == "" {
		return nil, fmt.Errorf("banking-circle authorize: want 200 + access_token, got %d", status)
	}
	return map[string]string{"Authorization": "Bearer " + auth.AccessToken}, nil
}

// BankingCirclePayout confirms the real payout rail this lab now models,
// including the safeguarding-account (SGA) gate (docs/
// ARCHITECTURE-phase3-corrections.md section 1 and 5): a fresh SGA starts
// at zero, so settlement's automatic payout (a side effect of
// WorldlineSettlementToSFTP's batch) may legitimately observe
// "awaiting_funding" on this stack's first-ever payout -- this scenario
// simulates Worldline's lump sum landing (through the lab API) and
// retries (POST .../retry-payout) exactly as a developer demoing this
// would. A repeat e2e run against the same long-lived stack finds
// funds already left over from the previous run and skips straight past
// this step -- both paths are exercised across two consecutive runs, not
// just one. Once funded, B4B drives its own lifecycle to B4BTMApproved;
// only then does it bridge the payment into Banking Circle, which
// independently drives OutgoingPaymentBooked -> OutgoingPaymentProcessed
// and moves ledger funds (docs/ARCHITECTURE-vendor-corrections.md
// sections 2-4). This scenario observes every hop where it happened --
// settlement's own payout-state introspection, then Banking Circle's books
// through the lab API -- not by re-deriving anything from logs.
//
// What this does NOT check: Banking Circle's own webhook *content*. Real
// Banking Circle encrypts webhook notifications with AES-256-GCM (section
// 3) delivered to receiver's raw capture sink (docs section 6) -- that
// proves delivery reached its destination with the right headers, but
// receiver holds no Banking Circle key, so it cannot decrypt or validate
// the payload itself. The AES-GCM algorithm already has a real
// encrypt-then-decrypt round trip covered by internal/bankingcircle's own
// unit tests; this scenario instead proves the stronger, still-genuine
// claim that the money actually moved in Banking Circle's books -- not a
// decorative check.
func BankingCirclePayout() Scenario {
	return Scenario{
		Name: "banking-circle-payout",
		Run: func(ctx context.Context, env *Env, state *State) error {
			settlementID, ok := state.Get("settlement_id")
			if !ok {
				return fmt.Errorf("no settlement_id in state (worldline-settlement-to-sftp must run first)")
			}
			// Captured by worldline-settlement-to-sftp, *before* its own
			// generate() call -- not here. Once the safeguarding account
			// already holds enough funds (any run after the stack's
			// first-ever payout), submitPayout's B4B->BC chain is
			// triggered synchronously from inside that very generate()
			// call and completes asynchronously in the background; by
			// the time this scenario starts (after worldline-settlement-
			// to-sftp's own multi-second SFTP+PGP round trip), it may
			// already be done. Capturing "before" here would race it.
			before, ok := state.Get("bc_merchant_balance_before")
			if !ok {
				return fmt.Errorf("no bc_merchant_balance_before in state (worldline-settlement-to-sftp must run first)")
			}
			// Matches cmd/b4b's approveAndBridge exactly ("bc_acc_" +
			// beneficiary id) and cmd/settlement's submitPayout, which
			// names the beneficiary by the MID -- this merchant's own
			// Banking Circle account, distinct from every other
			// merchant's (docs section 1/4).
			bcAccountID := bankingcircle.AccountIDFor(merchantIBAN)

			var payout payoutResp
			if _, err := GetJSON(ctx, env.Client, env.SettlementURL+"/reports/settlement/"+settlementID+"/payout", &payout); err != nil {
				return fmt.Errorf("get payout state: %w", err)
			}

			if payout.PayoutState == "awaiting_funding" {
				// Simulate Worldline's lump sum landing in the SGA -- the
				// e2e suite's "something has landed" trigger (docs section
				// 1). Funded generously so neither this nor a later run
				// ever re-hits this gate.
				if err := bcFundSGA(ctx, env, "1000000.00", "worldline-lump-sum-harness"); err != nil {
					return err
				}
				if status, err := PostJSON(ctx, env.Client, env.SettlementURL+"/reports/settlement/"+settlementID+"/retry-payout", nil, nil, &payout); err != nil {
					return fmt.Errorf("retry-payout: %w", err)
				} else if status != 200 {
					return fmt.Errorf("retry-payout: want 200, got %d: %+v", status, payout)
				}
			}

			// Hop 1: settlement -> B4B. Poll settlement's own payout
			// introspection until B4B's lifecycle reaches its terminal
			// approved state (B4BFailed here would be a genuine failure --
			// no B4B_FORCE_FAILURE_BENEFICIARY_IDS is configured for this
			// merchant's derived beneficiary id; the other non-approved
			// states would mean the funding/retry step above didn't
			// actually clear the gate it was supposed to).
			if err := PollUntil(ctx, 20*time.Second, 250*time.Millisecond, func() (bool, error) {
				if _, err := GetJSON(ctx, env.Client, env.SettlementURL+"/reports/settlement/"+settlementID+"/payout", &payout); err != nil {
					return false, err
				}
				switch payout.PayoutState {
				case "B4BTMApproved":
					return true, nil
				case "B4BFailed", "awaiting_funding", "verification_declined", "balance_check_failed", "submission_failed":
					return false, fmt.Errorf("settlement %s: payout stuck at %s", settlementID, payout.PayoutState)
				default:
					return false, nil
				}
			}); err != nil {
				return fmt.Errorf("waiting for B4B approval: %w", err)
			}

			// Hop 2: B4B -> Banking Circle. B4B bridges the payout once it
			// is approved; Banking Circle books and processes it on its
			// own, and its books say so.
			var bcPay bcPayment
			if err := PollUntil(ctx, 15*time.Second, 250*time.Millisecond, func() (bool, error) {
				payments, err := bcPayments(ctx, env)
				if err != nil {
					return false, err
				}
				for _, p := range payments {
					if p.SettlementID == settlementID && p.ToAccountID == bcAccountID {
						bcPay = p
						return p.State == "OutgoingPaymentProcessed", nil
					}
				}
				return false, fmt.Errorf("no banking-circle payment yet for settlement %s", settlementID)
			}); err != nil {
				return err
			}

			wantBalance, err := shiftBalance(before, bcPay.Amount)
			if err != nil {
				return err
			}
			gotBalance, err := bcReadBalance(ctx, env, bcAccountID)
			if err != nil {
				return fmt.Errorf("get account balance: %w", err)
			}
			if gotBalance != wantBalance {
				return fmt.Errorf("%s balance: want %s (before %s + payout %s), got %s", bcAccountID, wantBalance, before, bcPay.Amount, gotBalance)
			}
			return nil
		},
	}
}
