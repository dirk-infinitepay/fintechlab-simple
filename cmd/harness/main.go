// Harness runs black-box scenarios against a running fintechlab-simple stack.
// Local:       make harness                    (after `make up` + `make up-pods`)
// Containerized: make harness-docker
// Some:        HARNESS_ONLY=banking-circle-trap-paths make harness
// Exit code is 0 iff every registered scenario passed.
package main

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/harness"
)

func main() {
	env, err := harness.EnvFromOS()
	if err != nil {
		log.Fatalf("harness: %v", err)
	}

	timeout := 180 * time.Second
	if v := os.Getenv("HARNESS_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			timeout = d
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	scenarios := []harness.Scenario{
		harness.PaymentAPIToWebhook(),
		harness.WorldlineSettlementToSFTP(),
		harness.BankingCirclePayout(),
		harness.WorldlineConfirmationFile(),
		harness.BankingCircleSubscription(),
		harness.BankingCircleRetryDeactivation(),
		harness.B4BCompanyBoarding(),
		harness.B4BOversightGates(),
		harness.AciPaymentNotification(),
		harness.BankingCircleTrapPaths(),
		// Last: a flow reset clears Banking Circle's payments.
		harness.BankingCircleResetDropsAPayoutInFlight(),
	}

	// HARNESS_ONLY=name,name runs just those scenarios, in their usual
	// order: one service under test, or a scenario being written.
	if only := strings.TrimSpace(os.Getenv("HARNESS_ONLY")); only != "" {
		want := map[string]bool{}
		for _, n := range strings.Split(only, ",") {
			want[strings.TrimSpace(n)] = true
		}
		var picked []harness.Scenario
		for _, s := range scenarios {
			if want[s.Name] {
				picked = append(picked, s)
				delete(want, s.Name)
			}
		}
		for n := range want {
			log.Fatalf("harness: HARNESS_ONLY names %q, which is not a scenario", n)
		}
		scenarios = picked
	}

	results := harness.RunAll(ctx, env, scenarios)
	os.Exit(harness.Report(results, len(scenarios)))
}
