// LexisNexis simulates the LexisNexis Risk Solutions APIs the platform calls
// for identity: OAuth, then IDU identity reports and IVI attribute queries.
// Every person passes until the identity outcome is flipped
// (docs/api/services/lexisnexis.md).
package main

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/vendorsim"
)

const checkIdentity = "identity"

type app struct{ sim *vendorsim.Sim }

func newApp(addr string) (*app, error) {
	a := &app{}
	sim, err := vendorsim.New(vendorsim.Config{
		Service: "lexisnexis", Simulates: "LexisNexis",
		Checks: []vendorsim.Check{{Name: checkIdentity, Outcomes: []string{"pass", "refer", "fail"}}},
	}, addr)
	if err != nil {
		return nil, err
	}
	a.sim = sim
	sim.Vendor("POST /lexisnexis/oauth/token", a.token)
	sim.Vendor("POST /lexisnexis/idu/v1/reports", sim.Bearer(a.idu))
	sim.Vendor("POST /lexisnexis/ivi/api/attribute-query", a.ivi)
	return a, nil
}

func main() {
	addr := vendorsim.Listen(":8093")
	a, err := newApp(addr)
	if err != nil {
		log.Fatalf("lexisnexis: %v", err)
	}
	a.sim.Run(addr)
}

func (a *app) token(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	clientID, secret := vendorsim.Str(body["client_id"]), vendorsim.Str(body["client_secret"])
	if clientID == "" || secret == "" {
		httputilx.Error(w, 401, "client_id and client_secret required")
		return
	}
	env, ok := a.sim.ByLogin(clientID, secret)
	if !ok {
		httputilx.Error(w, 401, "invalid client_id or client_secret")
		return
	}
	a.sim.As(r, env)
	httputilx.WriteJSON(w, 200, map[string]any{
		"access_token": a.sim.Issue("ln", env),
		"expires_in":   3600,
		"token_type":   "Bearer",
	})
}

// result is the identity outcome as LexisNexis states it, with a score
// against the threshold the client also reads.
func (a *app) result(r *http.Request) (string, int) {
	switch a.sim.Answer(r, checkIdentity) {
	case "refer":
		return "REFER", 500
	case "fail":
		return "FAIL", 100
	}
	return "PASS", 850
}

// idu answers the identity check. Its attributes describe a clean person:
// the address is verified and no sanction, PEP, insolvency, CCJ or death
// register flags them.
func (a *app) idu(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	result, score := a.result(r)
	httputilx.WriteJSON(w, 200, map[string]any{
		"data": map[string]any{
			"id":         "sim-idu-" + vendorsim.Str(body["reference"]),
			"status":     result,
			"assessment": map[string]any{"result": result, "score": score},
			"context":    map[string]any{"pass_threshold": 700},
			"attributes": map[string]any{
				"dob_count":        1,
				"address_verified": true,
				"sanction":         false,
				"pep":              false,
				"insolvency":       false,
				"ccj":              false,
				"death_ddri":       false,
				"death_gro":        false,
				"death_halo":       false,
			},
		},
	})
}

func (a *app) ivi(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if vendorsim.Str(body["org_id"]) == "" || vendorsim.Str(body["api_key"]) == "" || vendorsim.Str(body["policy"]) == "" {
		httputilx.Error(w, 401, "org_id, api_key and policy required")
		return
	}
	r = a.sim.As(r, a.sim.ByKey(vendorsim.Str(body["api_key"])))
	status, score := a.result(r)
	httputilx.WriteJSON(w, 200, map[string]any{
		"request_id": "sim-ivi-" + vendorsim.Str(body["reference"]),
		"request_result": map[string]any{
			"review_status": status,
			"policy_score":  score,
		},
		"integration_hub_results": []map[string]any{
			{"Products": []map[string]any{{"ProductStatus": status}}},
		},
	})
}
