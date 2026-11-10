// Verify models buddy's real verification-service: the merchant-decisioning
// gate that advances a merchant application to UNDERWRITING_APPROVED (and
// can later PAUSE_SETTLEMENTS), keyed on merchantApplicationId. This lab
// mocks it as an always-positive stub with a configurable force-decline
// override, per docs/ARCHITECTURE-phase3-corrections.md section 3. Real
// verification-service is plain HTTP even in local dev (no TLS), so this
// mock is too.
//
// Each environment the lab created is a customer here (internal/tenant),
// known by an x-internal-api-key of its own; VERIFICATION_INTERNAL_API_KEY
// is the default environment's. A call is its key's environment's, and so
// is the decision it triggers.
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
	"github.com/webduvet/fintechlab-simple/internal/tenant"
	"github.com/webduvet/fintechlab-simple/internal/verify"
)

type app struct {
	engine *verify.Engine
	// apiKey is the default environment's x-internal-api-key.
	apiKey string
	// customers are the environments besides the default, each with a
	// key of its own. routes makes them when they are not given.
	customers *tenant.Customers
}

func main() {
	logring.Install()
	addr := env("LISTEN", ":8088")
	labclock.FollowEnv(context.Background(), "verify")
	apiKey := env("VERIFICATION_INTERNAL_API_KEY", "sim-verify-key-dev-only")
	delay := envDuration("VERIFY_PROCESSING_DELAY", 100*time.Millisecond)
	forceDeclineSpec := env("VERIFY_FORCE_DECLINE_IDS", "")

	a := &app{
		engine:    verify.NewEngine(delay, parseForceDeclineSet(forceDeclineSpec)),
		apiKey:    apiKey,
		customers: tenant.NewCustomers(),
	}

	mux, ctl, err := a.routes(addr)
	if err != nil {
		log.Fatalf("verify: %v", err)
	}
	ctl.Start()
	log.Printf("verify listening on %s delay=%s", addr, delay)
	log.Fatal(http.ListenAndServe(addr, logReq(mux)))
}

// routes is verify's two surfaces: the platform's verification-service
// routes on addr, and the lab's control surface on a port of its own
// (internal/simctl) — every /sim route and its own GET /health, with no
// x-internal-api-key. The service port still answers the /sim paths until
// phase 4 of docs/lab-api/plan.md retires them.
func (a *app) routes(addr string) (*http.ServeMux, *simctl.Control, error) {
	if a.customers == nil {
		a.customers = tenant.NewCustomers()
	}
	// A customer removed takes its decisions with it.
	a.customers.OnDelete = func(env string) error {
		a.engine.Forget(env)
		return nil
	}
	// Both scopes do the same: verify has nothing a platform sets up once.
	// What a run makes is decisions, recorded or still in their delay, in
	// the environment the reset names; the force-decline list is
	// configuration and stays.
	ctl, err := simctl.NewControl(simctl.Config{
		Service: "verify", Kind: "stand-in", Simulates: "the platform's verification-service (AML decision)",
		Listeners: []string{addr},
		Scopes:    []string{simctl.ScopeFlow, simctl.ScopeAll},
		Reset: func(_ context.Context, req simctl.ResetRequest) (simctl.ResetResult, error) {
			decisions, pending := a.engine.ResetIn(req.Env)
			return simctl.ResetResult{
				Cleared: map[string]int{"decisions": decisions, "pending": pending},
				Kept:    []string{"force_decline_ids"},
			}, nil
		},
		Customers: a.customers,
	}, simctl.ControlAddr(addr))
	if err != nil {
		return nil, nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/verification/health", func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, map[string]string{"status": "ok", "service": "verify"})
	})
	mux.HandleFunc("POST /api/v1/verification/verify/aml-decision", a.requireInternalKey(a.triggerAMLDecision))
	mux.HandleFunc("GET /api/v1/verification/verify/data/verify-decision/{merchantApplicationId}", a.requireInternalKey(a.getDecision))
	// The old /sim paths on the service port, until phase 4.
	mux.Handle("/sim/", ctl.Mux)
	return mux, ctl, nil
}

// requireInternalKey guards every /verify/* route (not /health) with a
// constant-time comparison against VERIFICATION_INTERNAL_API_KEY, matching
// buddy's own internal-api-key.guard.ts, or against a customer's own key,
// which makes the call that customer's environment's.
func (a *app) requireInternalKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		env, ok := a.keyEnv(r.Header.Get("x-internal-api-key"))
		if !ok {
			httputilx.Error(w, 401, "missing or invalid x-internal-api-key")
			return
		}
		a.customers.Seen(env)
		next(w, r.WithContext(tenant.With(r.Context(), env)))
	}
}

// keyEnv is the environment whose key key is: the default's for
// VERIFICATION_INTERNAL_API_KEY, a customer's for its own.
func (a *app) keyEnv(key string) (string, bool) {
	if subtle.ConstantTimeCompare([]byte(key), []byte(a.apiKey)) == 1 {
		return tenant.Default, true
	}
	if key == "" {
		return "", false
	}
	c, ok := a.customers.Find(func(c tenant.Customer) bool {
		return c.Verify != nil && subtle.ConstantTimeCompare([]byte(key), []byte(c.Verify.Key)) == 1
	})
	return c.Env, ok
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

	d := a.engine.TriggerIn(tenant.Of(r.Context()), req.MerchantApplicationID)

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

	d, ok := a.engine.GetIn(tenant.Of(r.Context()), id)
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
