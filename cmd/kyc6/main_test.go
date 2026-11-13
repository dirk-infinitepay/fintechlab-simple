package main

import (
	"testing"

	"github.com/webduvet/fintechlab-simple/internal/vendorsim/vendorsimtest"
)

func TestAScreenIsClearUntilItHitsACurrentSanction(t *testing.T) {
	t.Setenv("SIM_CONTROL_LISTEN", "")
	t.Setenv("SIM_CONTROL_ALLOW", "")
	a, err := newApp(":8092")
	if err != nil {
		t.Fatal(err)
	}
	svc, control := a.sim.Surfaces()
	ctl := control.Handler()
	key := map[string]string{"x-api-key": "k"}

	for _, path := range []string{"/kyc6/individuals", "/kyc6/businesses"} {
		if _, out := vendorsimtest.Call(t, svc, "POST", path, `{"name":"Ada"}`, key); vendorsimtest.At(out, "results.matchCount") != float64(0) {
			t.Errorf("%s by default: %v, want clear", path, out)
		}
	}
	vendorsimtest.Call(t, ctl, "POST", "/sim/outcome", `{"check":"screening","outcome":"hit"}`, nil)
	_, out := vendorsimtest.Call(t, svc, "POST", "/kyc6/businesses", `{"name":"Ada"}`, key)
	matches, _ := vendorsimtest.At(out, "results.matches").([]any)
	if len(matches) != 1 {
		t.Fatalf("hit: %v", out)
	}
	match := matches[0].(map[string]any)
	datasets, _ := match["datasets"].([]any)
	if match["resourceId"] == nil || len(datasets) != 1 || datasets[0] != "SAN-CURRENT" {
		t.Errorf("a hit carries a resourceId and SAN-CURRENT: %v", match)
	}
	if code, _ := vendorsimtest.Call(t, svc, "POST", "/kyc6/individuals", `{}`, nil); code != 401 {
		t.Errorf("no key: %d, want 401", code)
	}
}
