package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/webduvet/fintechlab-simple/internal/trap"
)

func (lab *gatewayLab) armTrap(t *testing.T, body string) string {
	t.Helper()
	st, b := send(t, lab.l.control, "POST", "/sim/traps", body, "")
	if st != 201 {
		t.Fatalf("POST /sim/traps = %d %s", st, b)
	}
	return object(t, b)["id"].(string)
}

func (lab *gatewayLab) trap(t *testing.T, id string) trap.Trap {
	t.Helper()
	st, b := send(t, lab.l.control, "GET", "/sim/traps/"+id, "", "")
	var tr trap.Trap
	if st != 200 || json.Unmarshal(b, &tr) != nil {
		t.Fatalf("GET /sim/traps/%s = %d %s", id, st, b)
	}
	return tr
}

func (lab *gatewayLab) trapActivity(t *testing.T, op, id string) []string {
	t.Helper()
	st, b := send(t, lab.l.control, "GET", "/sim/activity", "", "")
	var act struct {
		Logs []struct {
			Name   string
			Events []struct {
				Op      string
				Summary string
			}
		}
	}
	if st != 200 || json.Unmarshal(b, &act) != nil {
		t.Fatalf("GET /sim/activity = %d %s", st, b)
	}
	var out []string
	for _, l := range act.Logs {
		for _, e := range l.Events {
			if e.Op == op && strings.HasPrefix(e.Summary, id+" ") {
				out = append(out, e.Summary)
			}
		}
	}
	return out
}

func TestAnHTTPRequestTrapRefusesAValidTokenOnce(t *testing.T) {
	lab := newGatewayLab(t)
	id := lab.armTrap(t, `{"hook": "http.request", "match": {"method": "POST", "path": "/oversight/v1/companies"}, "times": 1,
		"effect": {"respond": {"status": 401, "body": {"message": "invalid bearer token"}}}}`)

	refused := lab.call(t, "POST", "/oversight/v1/companies", gatewayCreateCompany, 401)
	if refused["message"] != "invalid bearer token" {
		t.Errorf("refused answer = %v", refused)
	}
	company := lab.call(t, "POST", "/oversight/v1/companies", gatewayCreateCompany, 201)
	if company["id"] == nil {
		t.Fatalf("the same token and company after the refusal = %v, want it created", company)
	}

	tr := lab.trap(t, id)
	if tr.State != "spent" || len(tr.Caught) != 1 || tr.Caught[0].Subject != "POST /oversight/v1/companies" ||
		len(tr.Caught[0].Applied) != 1 || tr.Caught[0].Applied[0].To != "respond 401" {
		t.Errorf("trap = %+v", tr)
	}
	if caught := lab.trapActivity(t, "trap.caught", id); len(caught) != 1 {
		t.Errorf("trap.caught in GET /sim/activity = %v, want one", caught)
	}
}

func TestAnHTTPRequestTrapNeverAnswersForABadToken(t *testing.T) {
	lab := newGatewayLab(t)
	id := lab.armTrap(t, `{"hook": "http.request", "match": {}, "times": null, "effect": {"respond": {"status": 503}}}`)

	if st, b := send(t, lab.l.vendor, "POST", "/oversight/v1/companies", gatewayCreateCompany, "not.a.jwt"); st != 401 {
		t.Errorf("a bad token with a trap armed = %d %s, want B4B's own 401", st, b)
	}
	if tr := lab.trap(t, id); len(tr.Caught) != 0 {
		t.Errorf("the trap caught %d requests B4B refused itself", len(tr.Caught))
	}
}

func TestACommitTrapCreatesTheCompanyThenFailsTheReply(t *testing.T) {
	lab := newGatewayLab(t)
	id := lab.armTrap(t, `{"hook": "http.request", "match": {"method": "POST", "path": "/oversight/v1/companies"}, "times": 1,
		"effect": {"commit": true, "respond": {"status": 502, "body": {"message": "Bad Gateway"}}}}`)

	failed := lab.call(t, "POST", "/oversight/v1/companies", gatewayCreateCompany, 502)
	if failed["message"] != "Bad Gateway" || failed["id"] != nil {
		t.Errorf("the reply = %v, want the trap's 502 and no company", failed)
	}
	again := lab.call(t, "POST", "/oversight/v1/companies", gatewayCreateCompany, 422)
	if msg, _ := again["error"].(string); !strings.Contains(msg, "IP000123") {
		t.Errorf("creating it again = %v, want it refused as one that exists", again)
	}

	tr := lab.trap(t, id)
	if len(tr.Caught) != 1 || len(tr.Caught[0].Applied) != 2 ||
		tr.Caught[0].Applied[0].To != "handled 201" || tr.Caught[0].Applied[1].To != "respond 502" {
		t.Errorf("trap = %+v", tr)
	}
}

func TestACommitTrapCanDropTheConnectionOnceTheCompanyExists(t *testing.T) {
	lab := newGatewayLab(t)
	lab.armTrap(t, `{"hook": "http.request", "match": {"method": "POST", "path": "/oversight/v1/companies"}, "times": 1,
		"effect": {"commit": true, "close": true}}`)

	req, err := http.NewRequest("POST", lab.l.vendor.URL+"/oversight/v1/companies", strings.NewReader(gatewayCreateCompany))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+lab.token)
	req.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Fatalf("the create answered %d, want the connection dropped", resp.StatusCode)
	}
	lab.call(t, "POST", "/oversight/v1/companies", gatewayCreateCompany, 422)
}
