package e2e

import (
	"context"
	"fmt"
	"strings"
)

// bcAccounts is Banking Circle's ledger as the lab sees it: every account
// with its balance, read through the lab API (GET /sim/accounts). The
// creditor accounts are the lab's bookkeeping of the beneficiaries' banks,
// which a platform could never read from Banking Circle, so this is a lab
// read, not a vendor one.
type bcAccounts struct {
	Accounts []bcAccountResp `json:"accounts"`
}

// bcReadBalance returns accountID's balance, "0.00" for an account Banking
// Circle has not opened yet (one is opened when it is first paid into).
func bcReadBalance(ctx context.Context, env *Env, accountID string) (string, error) {
	var all bcAccounts
	status, err := env.Lab.Get(ctx, Sim("banking-circle", "accounts"), &all)
	if err != nil {
		return "", fmt.Errorf("banking-circle accounts: %w", err)
	}
	if status != 200 {
		return "", fmt.Errorf("banking-circle accounts: want 200, got %d", status)
	}
	for _, a := range all.Accounts {
		if a.ID == accountID {
			return a.Balance, nil
		}
	}
	return "0.00", nil
}

// bcPayments is every payment Banking Circle holds, through the lab API
// (GET /sim/payments).
func bcPayments(ctx context.Context, env *Env) ([]bcPayment, error) {
	var ps bcPaymentsResp
	status, err := env.Lab.Get(ctx, Sim("banking-circle", "payments"), &ps)
	if err != nil {
		return nil, fmt.Errorf("banking-circle payments: %w", err)
	}
	if status != 200 {
		return nil, fmt.Errorf("banking-circle payments: want 200, got %d", status)
	}
	return ps.Payments, nil
}

// bcFundSGA lands money on the EUR safeguarding account, as Worldline's
// wire would: the lab API's POST /sim/incoming-payments on Banking Circle.
func bcFundSGA(ctx context.Context, env *Env, amountMajor, reference string) error {
	status, err := env.Lab.Post(ctx, Sim("banking-circle", "incoming-payments"),
		map[string]string{"currency": "EUR", "amount": amountMajor, "reference": reference}, nil)
	if err != nil {
		return fmt.Errorf("simulate worldline lump sum landing: %w", err)
	}
	if status != 202 {
		return fmt.Errorf("simulate worldline lump sum landing: want 202, got %d", status)
	}
	return nil
}

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
