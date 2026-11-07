package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/webduvet/fintechlab-simple/internal/console"
)

// trapSims: banking-circle and b4b have traps, bank has no hooks (404), and
// aci cannot be reached.
func trapSims(t *testing.T, a *app) *[]string {
	t.Helper()
	var mu sync.Mutex
	deletes := []string{}
	withTraps := func(id, trapsJSON, hooksJSON string) *httptest.Server {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /sim/traps", func(w http.ResponseWriter, r *http.Request) {
			if st := r.URL.Query().Get("state"); st == "bogus" {
				http.Error(w, `{"error":"state is armed, spent, expired or disarmed","status":400}`, 400)
				return
			}
			_, _ = w.Write([]byte(`{"service":"` + id + `","traps":` + trapsJSON + `}`))
		})
		mux.HandleFunc("GET /sim/traps/hooks", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"service":"` + id + `","hooks":` + hooksJSON + `}`))
		})
		mux.HandleFunc("DELETE /sim/traps", func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			deletes = append(deletes, id)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"service":"` + id + `","disarmed":1,"cancelled":[]}`))
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return srv
	}
	bc := withTraps("banking-circle",
		`[{"id":"trp_0002","hook":"payment.outgoing","state":"armed","created_at":"2026-11-07T10:00:02Z"},
		  {"id":"trp_0001","hook":"payment.outgoing","state":"spent","created_at":"2026-11-07T10:00:00Z"}]`,
		`[{"hook":"payment.outgoing","effect":"path"}]`)
	b4b := withTraps("b4b", `[{"id":"trp_0001","hook":"payment.create","state":"armed","created_at":"2026-11-07T10:00:01Z"}]`,
		`[{"hook":"payment.create","effect":"path"}]`)
	bank := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(bank.Close)
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	a.cat = &console.Catalogue{Services: []console.Service{
		{ID: "banking-circle", BaseURL: bc.URL, Control: bc.URL},
		{ID: "b4b", BaseURL: b4b.URL, Control: b4b.URL},
		{ID: "bank", BaseURL: bank.URL, Control: bank.URL},
		{ID: "aci", BaseURL: down.URL, Control: down.URL},
	}}
	return &deletes
}

func TestTrapsAcrossTheLabAreNamedByServiceNewestFirst(t *testing.T) {
	a := testApp(t, nil)
	trapSims(t, a)
	w := call(t, a, "GET", "/v1/traps", "")
	var got struct {
		Traps       []map[string]any    `json:"traps"`
		Unreachable []map[string]string `json:"unreachable"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != 200 || len(got.Traps) != 3 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	order := []string{}
	for _, tr := range got.Traps {
		order = append(order, tr["service"].(string)+"/"+tr["id"].(string))
	}
	if strings.Join(order, " ") != "banking-circle/trp_0002 b4b/trp_0001 banking-circle/trp_0001" {
		t.Errorf("order = %v, want newest first across services", order)
	}
	// bank has no hooks: left out, not a failure. aci is down: said so.
	if len(got.Unreachable) != 1 || got.Unreachable[0]["service"] != "aci" {
		t.Errorf("unreachable = %v", got.Unreachable)
	}
	if w := call(t, a, "GET", "/v1/traps?state=bogus", ""); w.Code != 400 {
		t.Errorf("a bad state = %d, want the sim's 400", w.Code)
	}
}

func TestHooksAcrossTheLabCarryTheirService(t *testing.T) {
	a := testApp(t, nil)
	trapSims(t, a)
	w := call(t, a, "GET", "/v1/traps/hooks", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"hook":"payment.outgoing","service":"banking-circle"`) ||
		!strings.Contains(w.Body.String(), `"hook":"payment.create","service":"b4b"`) {
		t.Errorf("%d %s", w.Code, w.Body)
	}
}

func TestDisarmingEveryTrapReportsEachSim(t *testing.T) {
	a := testApp(t, nil)
	deletes := trapSims(t, a)
	w := call(t, a, "DELETE", "/v1/traps", "")
	if w.Code != 502 || !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Fatalf("%d %s: aci is down, so the answer is partial", w.Code, w.Body)
	}
	if len(*deletes) != 2 {
		t.Errorf("disarmed at %v, want banking-circle and b4b", *deletes)
	}
	if strings.Contains(w.Body.String(), `"service":"bank"`) {
		t.Error("a sim with no hooks is listed as disarmed")
	}
}
