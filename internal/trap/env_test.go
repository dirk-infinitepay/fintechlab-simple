package trap

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

func TestATrapTakesOnlyItsOwnEnvironmentsEvents(t *testing.T) {
	s := newStore()
	if _, err := s.Arm(Spec{Env: "sbx", Hook: "payment.outgoing", Match: map[string]any{}, Path: []Step{}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Offer(payout("GB00", "1.00")); ok {
		t.Fatal("sbx's trap caught the default environment's payout")
	}
	ev := payout("GB00", "1.00")
	ev.Env = "sbx"
	if _, ok := s.Offer(ev); !ok {
		t.Fatal("sbx's trap missed sbx's payout")
	}
}

func TestReadsDisarmsAndResetsStayInTheirEnvironment(t *testing.T) {
	s := newStore()
	mine, _ := s.Arm(Spec{Env: "sbx", Hook: "payment.outgoing", Match: map[string]any{}, Path: []Step{}})
	theirs, _ := s.Arm(Spec{Hook: "payment.outgoing", Match: map[string]any{}, Path: []Step{}})

	if got := s.ListIn("sbx", ""); len(got) != 1 || got[0].ID != mine.ID || got[0].Env != "sbx" {
		t.Fatalf("ListIn(sbx) = %+v", got)
	}
	if _, _, ok := s.DisarmIn("sbx", theirs.ID); ok {
		t.Fatal("sbx disarmed the default's trap")
	}
	if n, _ := s.DisarmAllIn("sbx"); n != 1 {
		t.Fatalf("DisarmAllIn(sbx) = %d", n)
	}
	if got, _ := s.GetIn(tenant.Default, theirs.ID); got.State != StateArmed {
		t.Fatalf("the default's trap is %s", got.State)
	}

	s.Arm(Spec{Env: "sbx", Hook: "payment.outgoing", Match: map[string]any{}, Path: []Step{}})
	if traps, _ := s.ResetIn("sbx"); traps != 2 {
		t.Fatalf("ResetIn(sbx) dropped %d, want 2", traps)
	}
	if got := s.List(""); len(got) != 1 || got[0].ID != theirs.ID {
		t.Fatalf("after sbx's reset: %+v", got)
	}
}

func TestTheRoutesArmInTheEnvironmentTheCallNames(t *testing.T) {
	s := newStore()
	mux := http.NewServeMux()
	s.Routes(mux)
	body := `{"hook": "payment.outgoing", "match": {}, "effect": {"path": []}}`

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/sim/traps?env=sbx", strings.NewReader(body)))
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"env":"sbx"`) {
		t.Fatalf("arm in sbx: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/sim/traps?env=*", strings.NewReader(body)))
	if rec.Code != 400 {
		t.Fatalf("arm in every environment: %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/sim/traps", nil))
	if strings.Contains(rec.Body.String(), "trp_") {
		t.Fatalf("the default's list shows sbx's trap: %s", rec.Body)
	}
}
