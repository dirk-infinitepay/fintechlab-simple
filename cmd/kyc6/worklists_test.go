package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/vendorsim/vendorsimtest"
)

const (
	worklistPath = "/kyc6/worklists/wl-7"
	companyPath  = worklistPath + "/businesses/company-42"
	ownerPath    = worklistPath + "/individuals/owner-42-3"
	armCompany   = "/sim/kyc6/worklists/wl-7/businesses/company-42/matches"
	readCompany  = "/sim/kyc6/worklists/wl-7/businesses/company-42"
)

var defaultKey = map[string]string{"x-api-key": "k"}

func surfaces(t *testing.T) (svc, ctl http.Handler) {
	t.Helper()
	svc, ctl, _ = testApp(t)
	return svc, ctl
}

// testApp is surfaces with the app behind them, for a test that has to set
// up a state no call can.
func testApp(t *testing.T) (svc, ctl http.Handler, a *app) {
	t.Helper()
	t.Setenv("SIM_CONTROL_LISTEN", "")
	t.Setenv("SIM_CONTROL_ALLOW", "")
	a, err := newApp(":8092")
	if err != nil {
		t.Fatal(err)
	}
	vendor, control := a.sim.Surfaces()
	return vendor, control.Handler(), a
}

// enrol creates the worklist and puts one business and one individual on it,
// as the platform's monitoring client does.
func enrol(t *testing.T, svc http.Handler, key map[string]string) {
	t.Helper()
	if code, out := vendorsimtest.Call(t, svc, "PUT", worklistPath, `{"name":"Partner 7","threshold":85,"datasets":["SAN","PEP"],"frequency":"daily"}`, key); code != 200 || out["worklistId"] != "wl-7" {
		t.Fatalf("create worklist: %d %v", code, out)
	}
	if code, out := vendorsimtest.Call(t, svc, "PUT", companyPath, `{"name":"Southwind Coffee Ltd","addresses":[{"geography":"GB"}]}`, key); code != 200 || out["monitorRecordId"] != "company-42" {
		t.Fatalf("add business: %d %v", code, out)
	}
	if code, out := vendorsimtest.Call(t, svc, "PUT", ownerPath, `{"name":"Ada Lovelace","addresses":[{"geography":"GB","city":"London"}],"dob":"1815-12-10"}`, key); code != 200 {
		t.Fatalf("add individual: %d %v", code, out)
	}
}

func records(t *testing.T, svc http.Handler, path string, key map[string]string) (map[string]any, []any) {
	t.Helper()
	code, out := vendorsimtest.Call(t, svc, "GET", path, "", key)
	if code != 200 {
		t.Fatalf("GET %s: %d %v", path, code, out)
	}
	list, _ := out["monitorRecords"].([]any)
	return out, list
}

func matchesOf(t *testing.T, svc http.Handler, path string, key map[string]string) []any {
	t.Helper()
	code, out := vendorsimtest.Call(t, svc, "GET", path+"/matches", "", key)
	if code != 200 {
		t.Fatalf("GET %s/matches: %d %v", path, code, out)
	}
	list, _ := out["matches"].([]any)
	return list
}

func TestAnEnrolledRecordIsListedAndClosedUntilAMatchIsPutOnIt(t *testing.T) {
	svc, ctl := surfaces(t)
	enrol(t, svc, defaultKey)

	_, worklist := vendorsimtest.Call(t, svc, "GET", worklistPath, "", defaultKey)
	for field, want := range map[string]any{"id": "wl-7", "name": "Partner 7", "threshold": float64(85),
		"businessMonitorRecordsCount": float64(1), "individualMonitorRecordsCount": float64(1), "businessOpenMatchesCount": float64(0)} {
		if worklist[field] != want {
			t.Errorf("worklist %s = %v, want %v", field, worklist[field], want)
		}
	}

	all, list := records(t, svc, worklistPath+"/businesses", defaultKey)
	if len(list) != 1 || all["monitorRecordsCount"] != float64(1) {
		t.Fatalf("every business: %v", all)
	}
	company := list[0].(map[string]any)
	if company["id"] != "company-42" || company["name"] != "Southwind Coffee Ltd" || company["openCount"] != float64(0) || company["status"] != "closed" {
		t.Errorf("enrolled business: %v", company)
	}
	if countries, _ := company["countries"].([]any); len(countries) != 1 || countries[0] != "GB" {
		t.Errorf("countries come from the addresses' geography: %v", company["countries"])
	}
	if _, open := records(t, svc, worklistPath+"/businesses?status=open", defaultKey); len(open) != 0 {
		t.Errorf("nothing is open before a match: %v", open)
	}
	if got := matchesOf(t, svc, companyPath, defaultKey); len(got) != 0 {
		t.Errorf("no matches before one is put there: %v", got)
	}

	code, armed := vendorsimtest.Call(t, ctl, "POST", armCompany, `{"datasets":["SAN-CURRENT","PEP-CURRENT"],"score":96}`, nil)
	if code != 201 {
		t.Fatalf("arm: %d %v", code, armed)
	}
	m := armed["match"].(map[string]any)
	if m["resourceId"] == "" || m["qrCode"] == "" || m["matchVersion"] != float64(1) || m["currentStatus"] != "open" || m["name"] != "Southwind Coffee Ltd" || m["score"] != float64(96) {
		t.Errorf("armed match: %v", m)
	}

	openList, open := records(t, svc, worklistPath+"/businesses?status=open", defaultKey)
	if len(open) != 1 || openList["openMonitorRecordsCount"] != float64(1) || openList["openMatchesCount"] != float64(1) {
		t.Fatalf("open businesses: %v", openList)
	}
	record := open[0].(map[string]any)
	if record["openCount"] != float64(1) || record["status"] != "open" {
		t.Errorf("open record: %v", record)
	}
	if datasets, _ := record["openDatasets"].([]any); len(datasets) != 2 {
		t.Errorf("openDatasets: %v", record["openDatasets"])
	}
	if _, open := records(t, svc, worklistPath+"/individuals?status=open", defaultKey); len(open) != 0 {
		t.Errorf("a business's match does not open an individual: %v", open)
	}

	got := matchesOf(t, svc, companyPath, defaultKey)
	if len(got) != 1 || got[0].(map[string]any)["resourceId"] != m["resourceId"] || got[0].(map[string]any)["qrCode"] != m["qrCode"] {
		t.Fatalf("record matches: %v, want the armed one", got)
	}

	_, profile := vendorsimtest.Call(t, svc, "GET", "/kyc6/businesses/"+m["resourceId"].(string), "", defaultKey)
	if profile["name"] != "Southwind Coffee Ltd" || profile["qrCode"] != m["qrCode"] || profile["version"] != float64(1) {
		t.Errorf("the match's profile: %v", profile)
	}

	_, poll := vendorsimtest.Call(t, svc, "GET", worklistPath+"/matches", "", defaultKey)
	polled, _ := poll["matches"].([]any)
	if len(polled) != 1 || poll["totalMatches"] != float64(1) || polled[0].(map[string]any)["recordId"] != "company-42" {
		t.Errorf("worklist matches: %v", poll)
	}

	if code, _ := vendorsimtest.Call(t, svc, "GET", worklistPath+"/businesses?status=maybe", "", defaultKey); code != 400 {
		t.Errorf("an unknown status: %d, want 400", code)
	}
	if code, _ := vendorsimtest.Call(t, ctl, "POST", "/sim/kyc6/worklists/wl-7/businesses/nobody/matches", `{}`, nil); code != 404 {
		t.Errorf("arming on a record nobody enrolled: %d, want 404", code)
	}
	if code, _ := vendorsimtest.Call(t, ctl, "POST", armCompany, `{"colour":"red"}`, nil); code != 400 {
		t.Errorf("an unknown field: %d, want 400", code)
	}
}

func TestAReviewClosesTheMatchAndTheRecordStopsBeingOpen(t *testing.T) {
	svc, ctl := surfaces(t)
	enrol(t, svc, defaultKey)
	_, armed := vendorsimtest.Call(t, ctl, "POST", armCompany, `{}`, nil)
	qrCode := vendorsimtest.At(armed, "match.qrCode").(string)

	code, out := vendorsimtest.Call(t, svc, "PUT", companyPath+"/matches/reviews", `{"reviews":[
		{"qrCode":"`+qrCode+`","referenceId":"11","status":"false-positive","clientComment":"different person","clientUserId":"ada_lovelace_7","matchVersion":1},
		{"qrCode":"nope","referenceId":"12","status":"true-positive","clientComment":"","clientUserId":"ada_lovelace_7","matchVersion":1}]}`, defaultKey)
	if code != 200 || out["hasErrors"] != true {
		t.Fatalf("reviews: %d %v", code, out)
	}
	results := out["results"].([]any)
	first, second := results[0].(map[string]any), results[1].(map[string]any)
	if first["referenceId"] != "11" || first["statusCode"] != float64(200) || first["error"] != nil {
		t.Errorf("the review of the match: %v", first)
	}
	if second["referenceId"] != "12" || second["statusCode"] != float64(404) {
		t.Errorf("a review of a qrCode the record has no match for: %v", second)
	}

	m := matchesOf(t, svc, companyPath, defaultKey)[0].(map[string]any)
	if m["currentStatus"] != "false-positive" || m["lastReviewDescription"] != "different person" || m["lastReviewDateTimeISO"] == nil {
		t.Errorf("the reviewed match: %v", m)
	}
	if _, open := records(t, svc, worklistPath+"/businesses?status=open", defaultKey); len(open) != 0 {
		t.Errorf("a reviewed record is not open: %v", open)
	}
	all, _ := records(t, svc, worklistPath+"/businesses", defaultKey)
	if all["falsePositiveMatchesCount"] != float64(1) || all["openMatchesCount"] != float64(0) {
		t.Errorf("counts after the review: %v", all)
	}

	_, record := vendorsimtest.Call(t, ctl, "GET", readCompany, "", nil)
	reviews, _ := record["reviews"].([]any)
	if len(reviews) != 2 {
		t.Fatalf("the lab's read of the reviews: %v", record)
	}
	sent := reviews[0].(map[string]any)
	if sent["endpoint"] != "reviews" || sent["clientUserId"] != "ada_lovelace_7" || sent["matchVersion"] != float64(1) || sent["statusCode"] != float64(200) {
		t.Errorf("the review as sent: %v", sent)
	}

	// The profile changes: the match is open again on its next version.
	_, rearmed := vendorsimtest.Call(t, ctl, "POST", armCompany, `{"resourceId":"`+m["resourceId"].(string)+`","datasets":["SAN-FORMER"]}`, nil)
	if vendorsimtest.At(rearmed, "match.matchVersion") != float64(2) || vendorsimtest.At(rearmed, "match.currentStatus") != "open" {
		t.Errorf("re-armed: %v", rearmed)
	}
	if got := matchesOf(t, svc, companyPath, defaultKey); len(got) != 1 {
		t.Errorf("re-arming moves the match on rather than adding one: %v", got)
	}

	_, activity := vendorsimtest.Call(t, ctl, "GET", "/sim/activity", "", nil)
	if !journalHas(activity, "PUT "+companyPath+"/matches/reviews — 1 of 2 refused") {
		t.Errorf("the journal marks the refused entry: %v", activity)
	}
}

func journalHas(activity map[string]any, summary string) bool {
	logs, _ := activity["logs"].([]any)
	for _, l := range logs {
		events, _ := l.(map[string]any)["events"].([]any)
		for _, e := range events {
			if e.(map[string]any)["summary"] == summary {
				return true
			}
		}
	}
	return false
}

func TestAnAutoReviewClosesItsMatchWhenItAppearsAndCannotBeReplaced(t *testing.T) {
	svc, ctl := surfaces(t)
	enrol(t, svc, defaultKey)
	body := `{"autoReviews":[{"qrCode":"qr-ada","referenceId":"21","status":"false-positive","clientComment":"cleared at onboarding","clientUserId":"ada_lovelace_7","matchVersion":3}]}`

	code, out := vendorsimtest.Call(t, svc, "PUT", ownerPath+"/matches/auto-reviews", body, defaultKey)
	if code != 200 || out["hasErrors"] != false || vendorsimtest.At(out, "results") == nil {
		t.Fatalf("auto-reviews: %d %v", code, out)
	}
	_, again := vendorsimtest.Call(t, svc, "PUT", ownerPath+"/matches/auto-reviews", body, defaultKey)
	result := again["results"].([]any)[0].(map[string]any)
	if again["hasErrors"] != true || vendorsimtest.At(result, "error.code") != "MAPI-312" || result["referenceId"] != "21" {
		t.Errorf("the same auto-review again: %v, want MAPI-312", again)
	}

	_, armed := vendorsimtest.Call(t, ctl, "POST", "/sim/kyc6/worklists/wl-7/individuals/owner-42-3/matches", `{"qrCode":"qr-ada"}`, nil)
	if vendorsimtest.At(armed, "match.currentStatus") != "false-positive" {
		t.Errorf("a match with an auto-reviewed qrCode arrives closed: %v", armed)
	}
	if _, open := records(t, svc, worklistPath+"/individuals?status=open", defaultKey); len(open) != 0 {
		t.Errorf("an auto-reviewed record is not open: %v", open)
	}
}

func TestEachEnvironmentHasItsOwnWorklists(t *testing.T) {
	svc, ctl := surfaces(t)
	vendorsimtest.Call(t, ctl, "PUT", "/sim/customers/sbx", `{"env":"sbx","verification":{"username":"sbx-user","password":"sbx-key"}}`, nil)
	sbxKey := map[string]string{"x-api-key": "sbx-key"}
	enrol(t, svc, sbxKey)

	if code, _ := vendorsimtest.Call(t, svc, "GET", worklistPath, "", defaultKey); code != 404 {
		t.Errorf("the default environment reads sbx's worklist: %d, want 404", code)
	}
	if code, _ := vendorsimtest.Call(t, ctl, "POST", armCompany, `{}`, nil); code != 404 {
		t.Errorf("arming in the default environment on sbx's record: %d, want 404", code)
	}
	if code, out := vendorsimtest.Call(t, ctl, "POST", armCompany+"?env=sbx", `{}`, nil); code != 201 {
		t.Fatalf("arming in sbx: %d %v", code, out)
	}
	if _, open := records(t, svc, worklistPath+"/businesses?status=open", sbxKey); len(open) != 1 {
		t.Errorf("sbx's open records: %v", open)
	}
	if code, _ := vendorsimtest.Call(t, ctl, "POST", armCompany+"?env=*", `{}`, nil); code != 400 {
		t.Errorf("arming in every environment: %d, want 400", code)
	}

	_, every := vendorsimtest.Call(t, ctl, "GET", "/sim/kyc6/worklists?env=*", "", nil)
	if worklists, _ := every["worklists"].([]any); len(worklists) != 1 || worklists[0].(map[string]any)["env"] != "sbx" {
		t.Errorf("every environment's worklists: %v", every)
	}

	vendorsimtest.Call(t, ctl, "DELETE", "/sim/customers/sbx", "", nil)
	_, every = vendorsimtest.Call(t, ctl, "GET", "/sim/kyc6/worklists?env=*", "", nil)
	if worklists, _ := every["worklists"].([]any); len(worklists) != 0 {
		t.Errorf("a deleted customer's worklists stay: %v", every)
	}
}

func TestAFlowResetClearsTheRecordsAndKeepsTheWorklist(t *testing.T) {
	svc, ctl := surfaces(t)
	enrol(t, svc, defaultKey)
	vendorsimtest.Call(t, ctl, "POST", armCompany, `{}`, nil)
	vendorsimtest.Call(t, ctl, "POST", "/sim/outcome", `{"check":"screening","outcome":"hit"}`, nil)

	_, out := vendorsimtest.Call(t, ctl, "POST", "/sim/reset", `{"scope":"flow"}`, nil)
	for key, want := range map[string]float64{"records": 2, "matches": 1, "outcomes": 1} {
		if vendorsimtest.At(out, "cleared."+key) != want {
			t.Errorf("flow reset cleared %s = %v, want %v (%v)", key, vendorsimtest.At(out, "cleared."+key), want, out)
		}
	}
	if code, _ := vendorsimtest.Call(t, svc, "GET", worklistPath, "", defaultKey); code != 200 {
		t.Errorf("the worklist after a flow reset: %d, want kept", code)
	}
	if _, list := records(t, svc, worklistPath+"/businesses", defaultKey); len(list) != 0 {
		t.Errorf("records after a flow reset: %v", list)
	}
	if code, _ := vendorsimtest.Call(t, svc, "GET", companyPath+"/matches", "", defaultKey); code != 404 {
		t.Errorf("a cleared record's matches: %d, want 404", code)
	}
	if _, screen := vendorsimtest.Call(t, svc, "POST", "/kyc6/businesses", `{"name":"Ada"}`, defaultKey); vendorsimtest.At(screen, "results.matchCount") != float64(0) {
		t.Errorf("the screening outcome after a reset: %v", screen)
	}

	_, out = vendorsimtest.Call(t, ctl, "POST", "/sim/reset", `{"scope":"all"}`, nil)
	if vendorsimtest.At(out, "cleared.worklists") != float64(1) {
		t.Errorf("all reset cleared %v, want the worklist", out["cleared"])
	}
	if code, _ := vendorsimtest.Call(t, svc, "GET", worklistPath, "", defaultKey); code != 404 {
		t.Errorf("the worklist after an all reset: %d, want 404", code)
	}
}

func TestRemovingARecordAndAWorklist(t *testing.T) {
	svc, _ := surfaces(t)
	enrol(t, svc, defaultKey)
	if code, _ := vendorsimtest.Call(t, svc, "DELETE", ownerPath, "", defaultKey); code != 204 {
		t.Errorf("remove individual: %d, want 204", code)
	}
	if _, list := records(t, svc, worklistPath+"/individuals", defaultKey); len(list) != 0 {
		t.Errorf("individuals after removal: %v", list)
	}
	if code, _ := vendorsimtest.Call(t, svc, "DELETE", ownerPath, "", defaultKey); code != 404 {
		t.Errorf("removing it twice: %d, want 404", code)
	}
	if code, _ := vendorsimtest.Call(t, svc, "DELETE", worklistPath, "", defaultKey); code != 204 {
		t.Errorf("delete worklist: %d, want 204", code)
	}
	if code, _ := vendorsimtest.Call(t, svc, "PUT", companyPath, `{"name":"x"}`, defaultKey); code != 200 {
		t.Errorf("enrolling on a deleted worklist: %d, want 200, the worklist created again", code)
	}
}

func TestABodyThatDoesNotDecodeIsRefused(t *testing.T) {
	svc, _ := surfaces(t)
	enrol(t, svc, defaultKey)
	for _, call := range []struct{ name, path, body, want string }{
		{"empty worklist", worklistPath, ``, "a JSON body is required"},
		{"null worklist", worklistPath, `null`, "must be a JSON object"},
		{"worklist with something after it", worklistPath, `{"name":"Partner 8"} {}`, "nothing after it"},
		{"truncated worklist", worklistPath, `{"name":"Partner 7",`, "unexpected EOF"},
		{"worklist as an array", worklistPath, `[{"name":"Partner 7"}]`, "cannot unmarshal array"},
		{"address of the wrong type", worklistPath + "/businesses/company-43", `{"name":"North Ltd","addresses":"GB"}`, "cannot unmarshal string"},
		{"truncated reviews", companyPath + "/matches/reviews", `{"reviews":[{"qrCode":"sim-qr-1"`, "unexpected EOF"},
	} {
		code, out := vendorsimtest.Call(t, svc, "PUT", call.path, call.body, defaultKey)
		if msg, _ := out["error"].(string); code != 400 || !strings.Contains(msg, call.want) {
			t.Errorf("%s: %d %v, want 400 saying %q", call.name, code, out, call.want)
		}
	}
	if _, worklist := vendorsimtest.Call(t, svc, "GET", worklistPath, "", defaultKey); worklist["name"] != "Partner 7" {
		t.Errorf("a refused PUT changed the worklist: %v", worklist)
	}
	if _, list := records(t, svc, worklistPath+"/businesses", defaultKey); len(list) != 1 {
		t.Errorf("a refused record was enrolled: %v", list)
	}
}

// The platform creates its worklist once, at customer setup; a restart or
// an all reset must not leave every later enrolment a 404.
func TestEnrollingAfterAnAllResetCreatesTheWorklist(t *testing.T) {
	svc, ctl := surfaces(t)
	enrol(t, svc, defaultKey)
	vendorsimtest.Call(t, ctl, "POST", "/sim/reset", `{"scope":"all"}`, nil)

	if code, out := vendorsimtest.Call(t, svc, "PUT", companyPath, `{"name":"Southwind Coffee Ltd"}`, defaultKey); code != 200 || out["worklistId"] != "wl-7" {
		t.Fatalf("enrol after an all reset: %d %v", code, out)
	}
	code, worklist := vendorsimtest.Call(t, svc, "GET", worklistPath, "", defaultKey)
	if code != 200 || worklist["businessMonitorRecordsCount"] != float64(1) || worklist["createdDateTimeISO"] == "" {
		t.Errorf("the worklist the enrolment created: %d %v", code, worklist)
	}
	if code, _ := vendorsimtest.Call(t, ctl, "POST", armCompany, `{}`, nil); code != 201 {
		t.Errorf("arming on the record: %d, want 201", code)
	}
}

func TestAGeneratedIDIsNeverOneATestGave(t *testing.T) {
	svc, ctl := surfaces(t)
	enrol(t, svc, defaultKey)
	for _, body := range []string{`{"resourceId":"sim-om-2"}`, `{}`, `{"qrCode":"sim-qr-5"}`, `{}`} {
		if code, out := vendorsimtest.Call(t, ctl, "POST", armCompany, body, nil); code != 201 {
			t.Fatalf("arm %s: %d %v", body, code, out)
		}
	}
	resourceIDs, qrCodes := map[any]bool{}, map[any]bool{}
	for _, m := range matchesOf(t, svc, companyPath, defaultKey) {
		resourceIDs[m.(map[string]any)["resourceId"]] = true
		qrCodes[m.(map[string]any)["qrCode"]] = true
	}
	if len(resourceIDs) != 4 || len(qrCodes) != 4 {
		t.Fatalf("four matches, each its own resourceId and qrCode: %v %v", resourceIDs, qrCodes)
	}

	_, rearmed := vendorsimtest.Call(t, ctl, "POST", armCompany, `{"resourceId":"sim-om-2","name":"Moved On"}`, nil)
	_, profile := vendorsimtest.Call(t, svc, "GET", "/kyc6/businesses/sim-om-2", "", defaultKey)
	if vendorsimtest.At(rearmed, "match.matchVersion") != float64(2) || profile["version"] != float64(2) || profile["name"] != "Moved On" {
		t.Errorf("re-arming sim-om-2 moves the one match the profile shows: %v %v", rearmed, profile)
	}
}

func TestAReArmKeepsAnAbsentListAndClearsAnEmptyOne(t *testing.T) {
	svc, ctl := surfaces(t)
	enrol(t, svc, defaultKey)
	arm := func(body string) map[string]any {
		t.Helper()
		code, out := vendorsimtest.Call(t, ctl, "POST", armCompany, body, nil)
		if code != 200 && code != 201 {
			t.Fatalf("arm %s: %d %v", body, code, out)
		}
		return out["match"].(map[string]any)
	}
	arm(`{"resourceId":"r-1","datasets":["PEP-CURRENT"],"countries":["GB"]}`)

	m := arm(`{"resourceId":"r-1"}`)
	if fmt.Sprint(m["datasets"], m["countries"]) != "[PEP-CURRENT] [GB]" {
		t.Errorf("a re-arm without the lists keeps them: %v %v", m["datasets"], m["countries"])
	}
	m = arm(`{"resourceId":"r-1","datasets":[],"countries":[]}`)
	if fmt.Sprint(m["datasets"], m["countries"]) != "[] []" {
		t.Errorf("a re-arm with empty lists clears them: %v %v", m["datasets"], m["countries"])
	}
}

func TestAutoReviewsAndWorklistMatchesKeepOneOrder(t *testing.T) {
	svc, ctl, a := testApp(t)
	enrol(t, svc, defaultKey)

	vendorsimtest.Call(t, svc, "PUT", ownerPath+"/matches/auto-reviews", `{"autoReviews":[
		{"qrCode":"qr-e","status":"false-positive"},{"qrCode":"qr-b","status":"false-positive"},
		{"qrCode":"qr-g","status":"false-positive"},{"qrCode":"qr-a","status":"false-positive"},
		{"qrCode":"qr-d","status":"false-positive"},{"qrCode":"qr-h","status":"false-positive"},
		{"qrCode":"qr-c","status":"false-positive"},{"qrCode":"qr-f","status":"false-positive"}]}`, defaultKey)
	for i := 0; i < 20; i++ {
		_, record := vendorsimtest.Call(t, ctl, "GET", "/sim/kyc6/worklists/wl-7/individuals/owner-42-3", "", nil)
		var got []string
		for _, rv := range record["autoReviews"].([]any) {
			got = append(got, rv.(map[string]any)["qrCode"].(string))
		}
		if want := []string{"qr-a", "qr-b", "qr-c", "qr-d", "qr-e", "qr-f", "qr-g", "qr-h"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("auto-reviews held in one batch: %v, want by qrCode %v", got, want)
		}
	}

	vendorsimtest.Call(t, ctl, "POST", "/sim/kyc6/worklists/wl-7/individuals/owner-42-3/matches", `{"resourceId":"r-owner"}`, nil)
	vendorsimtest.Call(t, ctl, "POST", armCompany, `{"resourceId":"r-company-1"}`, nil)
	vendorsimtest.Call(t, ctl, "POST", armCompany, `{"resourceId":"r-company-2"}`, nil)
	vendorsimtest.Call(t, svc, "PUT", worklistPath+"/businesses/company-7", `{"name":"North Ltd"}`, defaultKey)
	vendorsimtest.Call(t, ctl, "POST", "/sim/kyc6/worklists/wl-7/businesses/company-7/matches", `{"resourceId":"r-company-7"}`, nil)
	// Found at the same moment, as a fast clock reads it.
	same := time.Now().UTC()
	a.mu.Lock()
	for _, wl := range a.monitorIn("default").worklists {
		for _, kind := range recordKinds {
			for _, rec := range wl.records[kind] {
				for _, m := range rec.matches {
					m.created = same
				}
			}
		}
	}
	a.mu.Unlock()
	for i := 0; i < 20; i++ {
		_, poll := vendorsimtest.Call(t, svc, "GET", worklistPath+"/matches", "", defaultKey)
		var got []string
		for _, m := range poll["matches"].([]any) {
			got = append(got, m.(map[string]any)["resourceId"].(string))
		}
		if want := []string{"r-company-1", "r-company-2", "r-company-7", "r-owner"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("worklist matches found at once: %v, want %v", got, want)
		}
	}
}

// reviewBatch is a reviews body of n entries, the first for qrCode and the
// rest for a qrCode the record has no match for.
func reviewBatch(n int, qrCode string) string {
	entries := make([]string, n)
	for i := range entries {
		code := "nope"
		if i == 0 {
			code = qrCode
		}
		entries[i] = fmt.Sprintf(`{"qrCode":%q,"referenceId":%d,"status":"false-positive"}`, code, i)
	}
	return `{"reviews":[` + strings.Join(entries, ",") + `]}`
}

// The journal keeps 64 KiB of an answer; a batch's refusals are marked from
// what the sim ruled, not from that copy.
func TestALargeBatchWithRefusedEntriesIsAJournalWarning(t *testing.T) {
	svc, ctl := surfaces(t)
	enrol(t, svc, defaultKey)
	_, armed := vendorsimtest.Call(t, ctl, "POST", armCompany, `{}`, nil)

	code, out := vendorsimtest.Call(t, svc, "PUT", companyPath+"/matches/reviews", reviewBatch(1000, vendorsimtest.At(armed, "match.qrCode").(string)), defaultKey)
	if code != 200 || out["hasErrors"] != true || len(out["results"].([]any)) != 1000 {
		t.Fatalf("a batch of 1000: %d hasErrors=%v", code, out["hasErrors"])
	}
	if first := out["results"].([]any)[0].(map[string]any); first["referenceId"] != "0" {
		t.Errorf("a numeric referenceId is echoed as a string: %v", first)
	}
	_, activity := vendorsimtest.Call(t, ctl, "GET", "/sim/activity", "", nil)
	if !journalHas(activity, "PUT "+companyPath+"/matches/reviews — 999 of 1000 refused") {
		t.Errorf("the journal marks the refused entries of a batch bigger than it keeps")
	}
}

func TestARecordKeepsTheNewestReviews(t *testing.T) {
	svc, ctl := surfaces(t)
	enrol(t, svc, defaultKey)
	vendorsimtest.Call(t, svc, "PUT", companyPath+"/matches/reviews", reviewBatch(200, "nope"), defaultKey)
	vendorsimtest.Call(t, svc, "PUT", companyPath+"/matches/reviews", reviewBatch(100, "nope"), defaultKey)

	_, record := vendorsimtest.Call(t, ctl, "GET", readCompany, "", nil)
	reviews := record["reviews"].([]any)
	if len(reviews) != reviewsKept {
		t.Fatalf("reviews kept: %d, want %d", len(reviews), reviewsKept)
	}
	if first, last := reviews[0].(map[string]any), reviews[len(reviews)-1].(map[string]any); first["referenceId"] != "44" || last["referenceId"] != "99" {
		t.Errorf("kept from %v to %v, want the newest: the first batch's 44 to the second's 99", first["referenceId"], last["referenceId"])
	}
}

func TestMonitoringTimestampsAreOnTheLabClock(t *testing.T) {
	labclock.Set(72 * time.Hour)
	defer labclock.Set(0)
	svc, ctl := surfaces(t)
	enrol(t, svc, defaultKey)
	_, armed := vendorsimtest.Call(t, ctl, "POST", armCompany, `{}`, nil)
	qrCode := vendorsimtest.At(armed, "match.qrCode").(string)
	vendorsimtest.Call(t, svc, "PUT", companyPath+"/matches/reviews", `{"reviews":[{"qrCode":"`+qrCode+`","referenceId":"1","status":"false-positive"}]}`, defaultKey)

	onLabClock := func(what string, value any) {
		t.Helper()
		at, err := time.Parse(time.RFC3339, fmt.Sprint(value))
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if drift := labclock.Now().Sub(at); drift < 0 || drift > time.Minute {
			t.Errorf("%s = %s, want the lab clock's %s, not the wall clock", what, at, labclock.Now().Format(time.RFC3339))
		}
	}
	_, worklist := vendorsimtest.Call(t, svc, "GET", worklistPath, "", defaultKey)
	onLabClock("worklist createdDateTimeISO", worklist["createdDateTimeISO"])
	onLabClock("worklist lastUpdatedDateTimeISO", worklist["lastUpdatedDateTimeISO"])
	_, list := records(t, svc, worklistPath+"/businesses", defaultKey)
	company := list[0].(map[string]any)
	onLabClock("record createdDateTimeISO", company["createdDateTimeISO"])
	onLabClock("record lastUpdatedDateTimeISO", company["lastUpdatedDateTimeISO"])
	m := matchesOf(t, svc, companyPath, defaultKey)[0].(map[string]any)
	onLabClock("match lastReviewDateTimeISO", m["lastReviewDateTimeISO"])
}

func TestAQRCodeAnotherMatchHasIsRefused(t *testing.T) {
	svc, ctl := surfaces(t)
	enrol(t, svc, defaultKey)
	arm := func(path, body string, want int) {
		t.Helper()
		if code, out := vendorsimtest.Call(t, ctl, "POST", path, body, nil); code != want {
			t.Errorf("arm %s %s: %d %v, want %d", path, body, code, out, want)
		}
	}
	armOwner := "/sim/kyc6/worklists/wl-7/individuals/owner-42-3/matches"
	arm(armCompany, `{"resourceId":"r-1","qrCode":"Q1"}`, 201)
	arm(armCompany, `{"resourceId":"r-2","qrCode":"Q1"}`, 409)
	arm(armOwner, `{"qrCode":"Q1"}`, 409)
	// One profile can match two records, as at KYC6.
	arm(armOwner, `{"resourceId":"r-1"}`, 201)
	arm(armCompany, `{"resourceId":"r-1","qrCode":"Q1"}`, 200)

	if got := matchesOf(t, svc, companyPath, defaultKey); len(got) != 1 {
		t.Fatalf("a refused arm puts no match: %v", got)
	}
	vendorsimtest.Call(t, svc, "PUT", companyPath+"/matches/reviews", `{"reviews":[{"qrCode":"Q1","referenceId":"1","status":"false-positive"}]}`, defaultKey)
	if _, open := records(t, svc, worklistPath+"/businesses?status=open", defaultKey); len(open) != 0 {
		t.Errorf("reviewing Q1 closes the record: %v", open)
	}
}

// One KYC6 profile can match two monitored businesses, as at the vendor, and
// its lookup describes it from either match.
func TestOneProfileCanMatchTwoRecords(t *testing.T) {
	svc, ctl := surfaces(t)
	enrol(t, svc, defaultKey)
	vendorsimtest.Call(t, svc, "PUT", worklistPath+"/businesses/company-7", `{"name":"North Ltd"}`, defaultKey)
	for path, qrCode := range map[string]string{
		armCompany: "Q42",
		"/sim/kyc6/worklists/wl-7/businesses/company-7/matches": "Q7",
	} {
		if code, out := vendorsimtest.Call(t, ctl, "POST", path, `{"resourceId":"r-1","qrCode":"`+qrCode+`"}`, nil); code != 201 {
			t.Fatalf("arm r-1 on %s: %d %v, want 201", path, code, out)
		}
	}
	code, profile := vendorsimtest.Call(t, svc, "GET", "/kyc6/businesses/r-1", "", defaultKey)
	if code != 200 || (profile["qrCode"] != "Q42" && profile["qrCode"] != "Q7") {
		t.Errorf("the profile both businesses match: %d %v, want one of their matches", code, profile)
	}
}

func TestANumericReviewIDComesBackWithEveryDigit(t *testing.T) {
	svc, ctl := surfaces(t)
	enrol(t, svc, defaultKey)
	vendorsimtest.Call(t, ctl, "POST", armCompany, `{"qrCode":"9007199254740993"}`, nil)

	_, out := vendorsimtest.Call(t, svc, "PUT", companyPath+"/matches/reviews", `{"reviews":[{"qrCode":9007199254740993,"referenceId":9007199254740993,"status":"false-positive"}]}`, defaultKey)
	result := out["results"].([]any)[0].(map[string]any)
	if result["referenceId"] != "9007199254740993" || result["statusCode"] != float64(200) {
		t.Errorf("a numeric qrCode and referenceId above 2^53: %v, want 9007199254740993 echoed and the match reviewed", result)
	}
}

// stalledWriter is a reader that takes nothing off the wire: its first Write
// blocks until the test releases it.
type stalledWriter struct {
	header  http.Header
	once    sync.Once
	writing chan struct{} // closed when the first Write starts
	release chan struct{}
}

func newStalledWriter() *stalledWriter {
	return &stalledWriter{header: http.Header{}, writing: make(chan struct{}), release: make(chan struct{})}
}

func (s *stalledWriter) Header() http.Header { return s.header }

func (s *stalledWriter) WriteHeader(int) {}

func (s *stalledWriter) Write(p []byte) (int, error) {
	s.once.Do(func() { close(s.writing) })
	<-s.release
	return len(p), nil
}

func TestASlowReaderDoesNotHoldUpOtherCalls(t *testing.T) {
	stalled := []struct {
		name    string
		control bool
		path    string
	}{
		{"every worklist", true, "/sim/kyc6/worklists?env=*"},
		{"one record, on the control port", true, readCompany},
		{"the worklist's matches", false, worklistPath + "/matches"},
		{"the record's matches", false, companyPath + "/matches"},
		{"the worklist", false, worklistPath},
		{"the records", false, worklistPath + "/businesses"},
	}
	for _, tc := range stalled {
		t.Run(tc.name, func(t *testing.T) {
			svc, ctl := surfaces(t)
			enrol(t, svc, defaultKey)
			if code, out := vendorsimtest.Call(t, ctl, "POST", armCompany, `{}`, nil); code != 201 {
				t.Fatalf("arm: %d %v", code, out)
			}

			handler := svc
			if tc.control {
				handler = ctl
			}
			slow := newStalledWriter()
			slowDone := make(chan struct{})
			go func() {
				defer close(slowDone)
				r := httptest.NewRequest("GET", tc.path, nil)
				r.Header.Set("x-api-key", "k")
				handler.ServeHTTP(slow, r)
			}()
			select {
			case <-slow.writing:
			case <-time.After(5 * time.Second):
				t.Fatal("the slow call never started writing")
			}

			otherDone := make(chan int, 1)
			go func() {
				code, _ := vendorsimtest.Call(t, svc, "PUT", ownerPath, `{"name":"Ada Lovelace"}`, defaultKey)
				otherDone <- code
			}()
			waited := false
			select {
			case code := <-otherDone:
				if code != 200 {
					t.Errorf("the other call: %d, want 200", code)
				}
			case <-time.After(2 * time.Second):
				waited = true
				t.Error("another call waited on a reader that takes nothing off the wire")
			}

			close(slow.release)
			<-slowDone
			if waited {
				<-otherDone
			}
		})
	}
}

// TestAnswersAreTheirOwnCopies reads every monitoring answer while other
// calls change what it shows, with nothing ordering the two: under -race, an
// answer written after a.mu is released that still shares state a change
// writes to is a reported race.
func TestAnswersAreTheirOwnCopies(t *testing.T) {
	svc, ctl := surfaces(t)
	enrol(t, svc, defaultKey)
	if code, out := vendorsimtest.Call(t, ctl, "POST", armCompany, `{"resourceId":"om-1","qrCode":"qr-1","countries":["GB"]}`, nil); code != 201 {
		t.Fatalf("arm: %d %v", code, out)
	}
	reads := []struct {
		handler http.Handler
		path    string
	}{
		{ctl, "/sim/kyc6/worklists?env=*"},
		{ctl, readCompany},
		{svc, worklistPath},
		{svc, worklistPath + "/matches"},
		{svc, worklistPath + "/businesses"},
		{svc, companyPath + "/matches"},
		{svc, "/kyc6/businesses/om-1"},
	}
	var wg sync.WaitGroup
	call := func(handler http.Handler, method, path, body string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			vendorsimtest.Call(t, handler, method, path, body, defaultKey)
		}()
	}
	for range 20 {
		for _, read := range reads {
			call(read.handler, "GET", read.path, "")
		}
		call(ctl, "POST", armCompany, `{"resourceId":"om-1","datasets":["PEP"],"countries":["FR"]}`)
		call(svc, "PUT", companyPath+"/matches/reviews", `{"reviews":[{"qrCode":"qr-1","status":"false-positive","referenceId":"r"}]}`)
		call(svc, "PUT", companyPath, `{"name":"Southwind Coffee Ltd","addresses":[{"geography":"IE"}]}`)
		call(svc, "PUT", worklistPath, `{"name":"Partner 7","threshold":90}`)
	}
	wg.Wait()
}
