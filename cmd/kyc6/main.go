// KYC6 simulates the Acuris KYC6 API the platform calls for sanctions, PEP
// and adverse-media screening, profile lookups and monitoring worklists.
// Every screen is clear until the screening outcome is flipped to hit
// (docs/api/services/kyc6.md).
package main

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/vendorsim"
)

const checkScreening = "screening"

type app struct{ sim *vendorsim.Sim }

func newApp(addr string) (*app, error) {
	a := &app{}
	sim, err := vendorsim.New(vendorsim.Config{
		Service: "kyc6", Simulates: "KYC6",
		Checks: []vendorsim.Check{{Name: checkScreening, Outcomes: []string{"clear", "hit"}}},
	}, addr)
	if err != nil {
		return nil, err
	}
	a.sim = sim
	sim.Vendor("POST /kyc6/individuals", sim.APIKey(a.screen))
	sim.Vendor("POST /kyc6/businesses", sim.APIKey(a.screen))
	sim.Vendor("GET /kyc6/individuals/{resourceId}", sim.APIKey(a.profile))
	sim.Vendor("GET /kyc6/businesses/{resourceId}", sim.APIKey(a.profile))
	sim.Vendor("PUT /kyc6/worklists/{worklistId}", sim.APIKey(a.createWorklist))
	sim.Vendor("GET /kyc6/lifecycle/ongoing-monitoring/{alertId}", sim.APIKey(a.monitoring))
	return a, nil
}

func main() {
	addr := vendorsim.Listen(":8092")
	a, err := newApp(addr)
	if err != nil {
		log.Fatalf("kyc6: %v", err)
	}
	a.sim.Run(addr)
}

// screen answers a sanctions, PEP and adverse-media screen. Clear is zero
// matches: the client reads matchCount and the matches array. A hit carries
// SAN-CURRENT, KYC6's dataset key for a current sanctions listing.
func (a *app) screen(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	matches := []map[string]any{}
	if a.sim.Answer(r, checkScreening) == "hit" {
		matches = append(matches, map[string]any{
			"resourceId": "sim-match-1",
			"name":       vendorsim.Str(body["name"]),
			"score":      0.92,
			"datasets":   []string{"SAN-CURRENT"},
			"matchTypes": []string{"name"},
		})
	}
	httputilx.WriteJSON(w, 200, map[string]any{
		"results": map[string]any{"matchCount": len(matches), "matches": matches},
	})
}

func (a *app) profile(w http.ResponseWriter, r *http.Request) {
	httputilx.WriteJSON(w, 200, map[string]any{
		"resourceId": r.PathValue("resourceId"),
		"datasets":   []string{"SAN-CURRENT"},
	})
}

func (a *app) createWorklist(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	httputilx.WriteJSON(w, 200, map[string]any{
		"worklistId": r.PathValue("worklistId"),
		"name":       vendorsim.Str(body["name"]),
	})
}

func (a *app) monitoring(w http.ResponseWriter, r *http.Request) {
	httputilx.WriteJSON(w, 200, map[string]any{
		"alertId": r.PathValue("alertId"),
		"status":  "closed",
		"matches": []any{},
	})
}
