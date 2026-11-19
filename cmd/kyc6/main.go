// KYC6 simulates the Acuris KYC6 API the platform calls for sanctions, PEP
// and adverse-media screening, profile lookups and monitoring worklists.
// Every screen is clear until the screening outcome is flipped to hit, and a
// monitored record has a match only once a test puts one there
// (docs/api/services/kyc6.md).
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
	"github.com/webduvet/fintechlab-simple/internal/vendorsim"
)

const checkScreening = "screening"

type app struct {
	sim *vendorsim.Sim

	mu         sync.Mutex
	monitoring map[string]*monitorEnv
}

func newApp(addr string) (*app, error) {
	a := &app{monitoring: map[string]*monitorEnv{}}
	sim, err := vendorsim.New(vendorsim.Config{
		Service: "kyc6", Simulates: "KYC6",
		Checks: []vendorsim.Check{{Name: checkScreening, Outcomes: []string{"clear", "hit"}}},
		Kept:   []string{"worklists"},
		Reset:  a.resetMonitoring,
		Forget: a.forget,
	}, addr)
	if err != nil {
		return nil, err
	}
	a.sim = sim
	sim.Vendor("POST /kyc6/individuals", sim.APIKey(a.screen))
	sim.Vendor("POST /kyc6/businesses", sim.APIKey(a.screen))
	sim.Vendor("GET /kyc6/individuals/{resourceId}", sim.APIKey(a.profile(kindIndividuals)))
	sim.Vendor("GET /kyc6/businesses/{resourceId}", sim.APIKey(a.profile(kindBusinesses)))
	sim.Vendor("GET /kyc6/lifecycle/ongoing-monitoring/{alertId}", sim.APIKey(a.alert))
	a.routeMonitoring()
	return a, nil
}

func main() {
	addr := vendorsim.Listen(":8092")
	labclock.FollowEnv(context.Background(), "kyc6")
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

func (a *app) forget(env string) {
	a.mu.Lock()
	delete(a.monitoring, env)
	a.mu.Unlock()
}

// profile is a match's profile. A monitoring match on one of the caller's
// records is described as it was put there; any other id gets a bare
// current-sanctions profile.
func (a *app) profile(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("resourceId")
		a.mu.Lock()
		m := a.matchByResource(tenant.Of(r.Context()), kind, id)
		var out map[string]any
		if m != nil {
			out = map[string]any{
				"resourceId": m.resourceID,
				"qrCode":     m.qrCode,
				"name":       m.name,
				"datasets":   m.datasets,
				"countries":  m.countries,
				"version":    m.version,
				"isDeleted":  false,
			}
		}
		a.mu.Unlock()
		if out == nil {
			out = map[string]any{"resourceId": id, "datasets": []string{"SAN-CURRENT"}}
		}
		httputilx.WriteJSON(w, 200, out)
	}
}

func (a *app) alert(w http.ResponseWriter, r *http.Request) {
	httputilx.WriteJSON(w, 200, map[string]any{
		"alertId": r.PathValue("alertId"),
		"status":  "closed",
		"matches": []any{},
	})
}
