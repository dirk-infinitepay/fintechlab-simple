package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEveryVendorRequestIsInTheJournal: what the platform asked each
// vendor and how it was answered, until a reset clears it.
func TestEveryVendorRequestIsInTheJournal(t *testing.T) {
	a := newApp()
	svc, ctl := surfaces(t, a)
	iban := httptest.NewRequest(http.MethodPost, "/iban/verify", strings.NewReader(`{"IBAN":"GB00X","name":"Southwind"}`))
	iban.Header.Set("x-api-key", "sim-key")
	svc.ServeHTTP(httptest.NewRecorder(), iban)
	svc.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/creditsafe/companies?regNo=1", nil))

	var got struct {
		Logs []struct {
			Name   string `json:"name"`
			Events []struct {
				Op      string `json:"op"`
				Summary string `json:"summary"`
				Status  string `json:"status"`
			} `json:"events"`
		} `json:"logs"`
	}
	if err := json.Unmarshal(serve(ctl.Handler(), http.MethodGet, "/sim/activity", "").Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Logs) != 1 || got.Logs[0].Name != "requests" || len(got.Logs[0].Events) != 2 {
		t.Fatalf("journal = %+v, want both requests", got)
	}
	newest, oldest := got.Logs[0].Events[0], got.Logs[0].Events[1]
	if oldest.Op != "iban.com" || oldest.Summary != "POST /iban/verify" || oldest.Status != "ok" {
		t.Errorf("iban.com request = %+v", oldest)
	}
	if newest.Op != "creditsafe" || newest.Status != "warn" || !strings.Contains(newest.Summary, "bearer token required") {
		t.Errorf("refused Creditsafe request = %+v", newest)
	}

	if cleared, _ := resetVia(t, a, "flow"); cleared["requests"] != 2 {
		t.Errorf("cleared = %v, want the two requests", cleared)
	}
}

// TestALoginIsInItsOwnEnvironmentsJournal: an environment's login is its
// own, not the default's, and its reset clears it.
func TestALoginIsInItsOwnEnvironmentsJournal(t *testing.T) {
	_, svc, ctl := withSbx(t)
	bearerTok(t, svc, "/creditsafe/authenticate", `{"username":"sbx-user","password":"sbx-secret"}`)
	bearerTok(t, svc, "/lexisnexis/oauth/token", `{"client_id":"sbx-user","client_secret":"sbx-secret"}`)

	count := func(env string) int {
		var got struct {
			Logs []struct {
				Events []json.RawMessage `json:"events"`
			} `json:"logs"`
		}
		_ = json.Unmarshal(serve(ctl, http.MethodGet, "/sim/activity?env="+env, "").Body.Bytes(), &got)
		return len(got.Logs[0].Events)
	}
	if sbx, def := count("sbx"), count("default"); sbx != 2 || def != 0 {
		t.Errorf("journal: sbx %d, default %d, want both logins in sbx", sbx, def)
	}
	serve(ctl, http.MethodPost, "/sim/reset", `{"scope":"flow","env":"sbx"}`)
	if sbx := count("sbx"); sbx != 0 {
		t.Errorf("sbx's journal after its reset: %d, want 0", sbx)
	}
}
