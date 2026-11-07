// Verify models buddy's real verification-service: the merchant-decisioning
// gate that advances a merchant application to UNDERWRITING_APPROVED (and
// can later PAUSE_SETTLEMENTS), keyed on merchantApplicationId. This lab
// mocks it as an always-positive stub with a configurable force-decline
// override, per docs/ARCHITECTURE-phase3-corrections.md section 3. Real
// verification-service is plain HTTP even in local dev (no TLS), so this
// mock is too.
package main

import (
	"context"
	"crypto/subtle"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/logring"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
	"github.com/webduvet/fintechlab-simple/internal/verify"
)

type app struct {
	engine *verify.Engine
	apiKey string
}

func main() {
	logring.Install()
	addr := env("LISTEN", ":8088")
	labclock.FollowEnv(context.Background(), "verify")
	apiKey := env("VERIFICATION_INTERNAL_API_KEY", "sim-verify-key-dev-only")
	delay := envDuration("VERIFY_PROCESSING_DELAY", 100*time.Millisecond)
	forceDeclineSpec := env("VERIFY_FORCE_DECLINE_IDS", "")

	a := &app{
		engine: verify.NewEngine(delay, parseForceDeclineSet(forceDeclineSpec)),
		apiKey: apiKey,
	}

	log.Printf("verify listening on %s delay=%s", addr, delay)
	log.Fatal(http.ListenAndServe(addr, logReq(a.routes(addr))))
}

// routes is verify's HTTP surface: the platform's verification-service
// routes, and the sim control contract beside them.
func (a *app) routes(addr string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/verification/health", func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, map[string]string{"status": "ok", "service": "verify"})
	})
	mux.HandleFunc("POST /api/v1/verification/verify/aml-decision", a.requireInternalKey(a.triggerAMLDecision))
	mux.HandleFunc("GET /api/v1/verification/verify/data/verify-decision/{merchantApplicationId}", a.requireInternalKey(a.getDecision))

	// Both scopes do the same: verify has nothing a platform sets up once.
	// What a run makes is decisions, recorded or still in their delay; the
	// force-decline list is configuration and stays.
	simctl.Mount(mux, simctl.Config{
		Service: "verify", Kind: "vendor", Simulates: "the platform's verification-service (AML decision)",
		Listeners: []string{addr},
		Scopes:    []string{simctl.ScopeFlow, simctl.ScopeAll},
		Reset: func(context.Context, simctl.ResetRequest) (simctl.ResetResult, error) {
			decisions, pending := a.engine.Reset()
			return simctl.ResetResult{
				Cleared: map[string]int{"decisions": decisions, "pending": pending},
				Kept:    []string{"force_decline_ids"},
			}, nil
		},
	})
	return mux
}

// requireInternalKey guards every /verify/* route (not /health) with a
// constant-time comparison against VERIFICATION_INTERNAL_API_KEY, matching
// buddy's own internal-api-key.guard.ts.
func (a *app) requireInternalKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("x-internal-api-key")
		if subtle.ConstantTimeCompare([]byte(key), []byte(a.apiKey)) != 1 {
			httputilx.Error(w, 401, "missing or invalid x-internal-api-key")
			return
		}
		next(w, r)
	}
}

type amlDecisionReq struct {
	MerchantApplicationID int `json:"merchantApplicationId"`
}

type journey struct {
	JourneyType        string `json:"journeyType"`
	OverallDecision    string `json:"overallDecision"`
	RiskScore          int    `json:"riskScore"`
	RiskClassification string `json:"riskClassification"`
}

// triggerAMLDecision implements POST /verify/aml-decision. The response
// body echoes the decision Trigger will settle on, computed synchronously;
// Get (polled next via /verify/data/verify-decision/{id}) is what actually
// reports readiness once VERIFY_PROCESSING_DELAY elapses.
func (a *app) triggerAMLDecision(w http.ResponseWriter, r *http.Request) {
	var req amlDecisionReq
	if err := httputilx.ReadJSON(r, &req); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	if req.MerchantApplicationID == 0 {
		httputilx.Error(w, 400, "merchantApplicationId required")
		return
	}

	d := a.engine.Trigger(req.MerchantApplicationID)

	httputilx.WriteJSON(w, 202, map[string]any{
		"merchantApplicationId": req.MerchantApplicationID,
		"journeys": []journey{
			{
				JourneyType:        "AML",
				OverallDecision:    d.OverallDecision,
				RiskScore:          d.RiskScore,
				RiskClassification: d.RiskClassification,
			},
		},
	})
}

// decisionResp is the wire shape of GET /verify/data/verify-decision/{id},
// matching VerifyDecisionSnapshotResponseDto's field names verbatim.
// ManualOverrideDecision is never set by this lab, so it stays "" and its
// omitempty tag drops it -- always null/omitted, as real.
type decisionResp struct {
	OverallDecision        string `json:"overallDecision"`
	ManualOverrideDecision string `json:"manualOverrideDecision,omitempty"`
	AmlDecision            string `json:"amlDecision"`
	AmlCompanyDecision     string `json:"amlCompanyDecision"`
	RiskDecision           string `json:"riskDecision"`
	RiskCompanyDecision    string `json:"riskCompanyDecision"`
	RiskScore              int    `json:"riskScore"`
	RiskClassification     string `json:"riskClassification"`
	CompletedAt            string `json:"completedAt"`
}

func (a *app) getDecision(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("merchantApplicationId"))
	if err != nil {
		httputilx.Error(w, 400, "merchantApplicationId must be an integer")
		return
	}

	d, ok := a.engine.Get(id)
	if !ok {
		httputilx.Error(w, 404, "decision not found")
		return
	}

	httputilx.WriteJSON(w, 200, decisionResp{
		OverallDecision:        d.OverallDecision,
		ManualOverrideDecision: d.ManualOverrideDecision,
		AmlDecision:            d.AmlDecision,
		AmlCompanyDecision:     d.AmlCompanyDecision,
		RiskDecision:           d.RiskDecision,
		RiskCompanyDecision:    d.RiskCompanyDecision,
		RiskScore:              d.RiskScore,
		RiskClassification:     d.RiskClassification,
		CompletedAt:            d.CompletedAt,
	})
}

// parseForceDeclineSet parses VERIFY_FORCE_DECLINE_IDS (comma-separated
// merchantApplicationIds) into a lookup set, same convention as B4B's
// B4B_FORCE_FAILURE_BENEFICIARY_IDS -- any non-integer entry is logged and
// skipped rather than failing startup.
func parseForceDeclineSet(spec string) map[int]bool {
	set := map[int]bool{}
	for _, raw := range strings.Split(spec, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		id, err := strconv.Atoi(raw)
		if err != nil {
			log.Printf("verify: skipping non-integer VERIFY_FORCE_DECLINE_IDS entry %q: %v", raw, err)
			continue
		}
		set[id] = true
	}
	return set
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func logReq(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
