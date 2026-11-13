package main

import (
	"strings"
	"testing"

	"github.com/webduvet/fintechlab-simple/internal/vendorsim/vendorsimtest"
)

func TestTheIbanCheckFollowsTheBankAccountOutcome(t *testing.T) {
	t.Setenv("SIM_CONTROL_LISTEN", "")
	t.Setenv("SIM_CONTROL_ALLOW", "")
	a, err := newApp(":8094")
	if err != nil {
		t.Fatal(err)
	}
	svc, control := a.sim.Surfaces()
	ctl := control.Handler()
	key := map[string]string{"x-api-key": "k"}

	for outcome, want := range map[string][2]any{"pass": {true, "yes"}, "mismatch": {true, "no"}, "invalid": {false, "no"}} {
		vendorsimtest.Call(t, ctl, "POST", "/sim/outcome", `{"check":"bank-account","outcome":"`+outcome+`"}`, nil)
		_, out := vendorsimtest.Call(t, svc, "POST", "/iban/verify", `{"IBAN":"IE29AIBK93115212345678","name":"Ada"}`, key)
		if got := [2]any{vendorsimtest.At(out, "result.valid"), vendorsimtest.At(out, "result.name_match")}; got != want {
			t.Errorf("%s: valid/name_match = %v, want %v", outcome, got, want)
		}
	}
	if code, _ := vendorsimtest.Call(t, svc, "POST", "/iban/verify", `{"IBAN":"x"}`, nil); code != 401 {
		t.Errorf("no key: %d, want 401", code)
	}
}

func TestAnIbanComRefusalIsAWarningInTheJournal(t *testing.T) {
	t.Setenv("SIM_CONTROL_LISTEN", "")
	t.Setenv("SIM_CONTROL_ALLOW", "")
	a, err := newApp(":8094")
	if err != nil {
		t.Fatal(err)
	}
	svc, control := a.sim.Surfaces()
	ctl := control.Handler()

	code, out := vendorsimtest.Call(t, svc, "POST", "/iban/verify", `{"name":"Ada"}`, map[string]string{"x-api-key": "k"})
	if code != 200 || out["error"] == nil {
		t.Fatalf("a check with no IBAN: %d %v, want iban.com's 200 with an error", code, out)
	}
	_, activity := vendorsimtest.Call(t, ctl, "GET", "/sim/activity", "", nil)
	logs, _ := activity["logs"].([]any)
	journal, _ := logs[0].(map[string]any)
	events, _ := journal["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("journal = %v", journal)
	}
	event, _ := events[0].(map[string]any)
	if event["status"] != "warn" || !strings.Contains(event["summary"].(string), out["error"].(string)) {
		t.Errorf("refused check = %v, want a warning with iban.com's error", event)
	}
}
