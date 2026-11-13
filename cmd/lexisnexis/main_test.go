package main

import (
	"testing"

	"github.com/webduvet/fintechlab-simple/internal/vendorsim/vendorsimtest"
)

func TestIdentityFollowsTheIdentityOutcome(t *testing.T) {
	t.Setenv("SIM_CONTROL_LISTEN", "")
	t.Setenv("SIM_CONTROL_ALLOW", "")
	a, err := newApp(":8093")
	if err != nil {
		t.Fatal(err)
	}
	svc, control := a.sim.Surfaces()
	ctl := control.Handler()

	code, out := vendorsimtest.Call(t, svc, "POST", "/lexisnexis/oauth/token", `{"client_id":"c","client_secret":"s"}`, nil)
	tok, _ := out["access_token"].(string)
	if code != 200 || tok == "" {
		t.Fatalf("token: %d %v", code, out)
	}
	auth := map[string]string{"Authorization": "Bearer " + tok}

	_, out = vendorsimtest.Call(t, svc, "POST", "/lexisnexis/idu/v1/reports", `{"reference":"r"}`, auth)
	if vendorsimtest.At(out, "data.assessment.result") != "PASS" || vendorsimtest.At(out, "data.attributes.address_verified") != true {
		t.Errorf("IDU by default: %v", out)
	}
	for _, flag := range []string{"sanction", "pep", "insolvency", "ccj", "death_ddri", "death_gro", "death_halo"} {
		if got := vendorsimtest.At(out, "data.attributes."+flag); got != false {
			t.Errorf("IDU %s = %v, want false", flag, got)
		}
	}

	for outcome, want := range map[string]string{"refer": "REFER", "fail": "FAIL"} {
		vendorsimtest.Call(t, ctl, "POST", "/sim/outcome", `{"check":"identity","outcome":"`+outcome+`"}`, nil)
		_, out = vendorsimtest.Call(t, svc, "POST", "/lexisnexis/idu/v1/reports", `{"reference":"r"}`, auth)
		if vendorsimtest.At(out, "data.assessment.result") != want {
			t.Errorf("IDU %s: %v", outcome, out)
		}
		_, out = vendorsimtest.Call(t, svc, "POST", "/lexisnexis/ivi/api/attribute-query", `{"org_id":"o","api_key":"k","policy":"p"}`, nil)
		if vendorsimtest.At(out, "request_result.review_status") != want {
			t.Errorf("IVI %s: %v", outcome, out)
		}
	}
	if code, _ := vendorsimtest.Call(t, svc, "POST", "/lexisnexis/ivi/api/attribute-query", `{"org_id":"o"}`, nil); code != 401 {
		t.Errorf("IVI without its key: %d, want 401", code)
	}
	if code, _ := vendorsimtest.Call(t, svc, "POST", "/lexisnexis/idu/v1/reports", `{}`, nil); code != 401 {
		t.Errorf("IDU without a token: %d, want 401", code)
	}
}
