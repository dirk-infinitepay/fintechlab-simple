// Command e2e runs black-box scenarios against a running fintechlab-simple stack.
// Local:       make e2e                    (after `make up` + `make up-pods`)
// Containerized: make e2e-docker
// Some:        E2E_ONLY=banking-circle-trap-paths make e2e
// Exit code is 0 iff every registered scenario passed.
package main

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/e2e"
)

func main() {
	env, err := e2e.EnvFromOS()
	if err != nil {
		log.Fatalf("e2e: %v", err)
	}

	timeout := 180 * time.Second
	if v := os.Getenv("E2E_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			timeout = d
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	scenarios := []e2e.Scenario{
		e2e.PaymentAPIToWebhook(),
		e2e.WorldlineSettlementToSFTP(),
		e2e.BankingCirclePayout(),
		e2e.WorldlineConfirmationFile(),
		e2e.BankingCircleSubscription(),
		e2e.BankingCircleRetryDeactivation(),
		e2e.B4BCompanyBoarding(),
		e2e.B4BOversightGates(),
		e2e.AciPaymentNotification(),
		e2e.BankingCircleTrapPaths(),
		// Last: a flow reset clears Banking Circle's payments.
		e2e.BankingCircleResetDropsAPayoutInFlight(),
	}

	// E2E_ONLY=name,name runs just those scenarios, in their usual
	// order: one service under test, or a scenario being written.
	if only := strings.TrimSpace(os.Getenv("E2E_ONLY")); only != "" {
		want := map[string]bool{}
		for _, n := range strings.Split(only, ",") {
			want[strings.TrimSpace(n)] = true
		}
		var picked []e2e.Scenario
		for _, s := range scenarios {
			if want[s.Name] {
				picked = append(picked, s)
				delete(want, s.Name)
			}
		}
		for n := range want {
			log.Fatalf("e2e: E2E_ONLY names %q, which is not a scenario", n)
		}
		scenarios = picked
	}

	results := e2e.RunAll(ctx, env, scenarios)
	os.Exit(e2e.Report(results, len(scenarios)))
}
