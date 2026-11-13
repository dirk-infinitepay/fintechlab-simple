package main

import (
	"net/http"
	"testing"

	"github.com/webduvet/fintechlab-simple/internal/vendorsim/vendorsimtest"
)

func surfaces(t *testing.T) (svc, ctl http.Handler) {
	t.Helper()
	t.Setenv("SIM_CONTROL_LISTEN", "")
	t.Setenv("SIM_CONTROL_ALLOW", "")
	a, err := newApp(":8091")
	if err != nil {
		t.Fatal(err)
	}
	vendor, control := a.sim.Surfaces()
	return vendor, control.Handler()
}

func token(t *testing.T, svc http.Handler) map[string]string {
	t.Helper()
	code, out := vendorsimtest.Call(t, svc, "POST", "/creditsafe/authenticate", `{"username":"u","password":"p"}`, nil)
	tok, _ := out["token"].(string)
	if code != 200 || tok == "" {
		t.Fatalf("authenticate: %d %v", code, out)
	}
	return map[string]string{"Authorization": "Bearer " + tok}
}

func TestTheBankCheckFollowsTheBankAccountOutcome(t *testing.T) {
	svc, ctl := surfaces(t)
	auth := token(t, svc)
	for outcome, want := range map[string][3]any{
		"pass":     {true, "Full", true},
		"mismatch": {true, "No Match", true},
		"invalid":  {false, "No Match", false},
	} {
		vendorsimtest.Call(t, ctl, "POST", "/sim/outcome", `{"check":"bank-account","outcome":"`+outcome+`"}`, nil)
		_, out := vendorsimtest.Call(t, svc, "POST", "/creditsafe/localSolutions/GB/bankVerification/search", `{"accountNumber":"1","sortCode":"2"}`, auth)
		got := [3]any{vendorsimtest.At(out, "supplierResponse.result"), vendorsimtest.At(out, "supplierResponse.nameMatchResult"), vendorsimtest.At(out, "supplierResponse.accountTypeResult")}
		if got != want {
			t.Errorf("%s: result/nameMatchResult/accountTypeResult = %v, want %v", outcome, got, want)
		}
	}
}

func TestARegisteredCompanyIsInItsReport(t *testing.T) {
	svc, ctl := surfaces(t)
	code, out := vendorsimtest.Call(t, ctl, "POST", "/sim/creditsafe/companies", `{"country":"GB","regNo":"29210000",
		"businessName":"Southwind Coffee Ltd","registrationDate":"2015-04-01T00:00:00Z","address":"1 Test Street London SW1A 1AA",
		"directors":[{"name":"Ada Lovelace"}],"shareholders":[{"name":"Ada Lovelace","percentSharesHeld":100}],
		"personsWithSignificantControl":[{"name":"Ada Lovelace","countryOfResidence":"United Kingdom"}]}`, nil)
	if code != 200 {
		t.Fatalf("register: %d %v", code, out)
	}
	auth := token(t, svc)
	_, found := vendorsimtest.Call(t, svc, "GET", "/creditsafe/companies?countries=GB&regNo=29210000", "", auth)
	companies, _ := found["companies"].([]any)
	if len(companies) != 1 {
		t.Fatalf("search: %v", found)
	}
	id, _ := companies[0].(map[string]any)["id"].(string)
	_, report := vendorsimtest.Call(t, svc, "GET", "/creditsafe/companies/"+id, "", auth)
	for path, want := range map[string]any{
		"report.companySummary.businessName":                                       "Southwind Coffee Ltd",
		"report.companySummary.companyRegistrationNumber":                          "29210000",
		"report.companySummary.companyStatus.status":                               "Active",
		"report.companySummary.creditRating.commonValue":                           "A",
		"report.companyIdentification.basicInformation.companyRegistrationDate":    "2015-04-01T00:00:00Z",
		"report.companyIdentification.basicInformation.contactAddress.simpleValue": "1 Test Street London SW1A 1AA",
		"report.creditScore.currentCreditRating.pod":                               0.5,
		"report.negativeInformation.ccjSummary.numberOfExact":                      float64(0),
	} {
		if got := vendorsimtest.At(report, path); got != want {
			t.Errorf("%s = %v, want %v", path, got, want)
		}
	}
	shareholders, _ := vendorsimtest.At(report, "report.shareCapitalStructure.shareHolders").([]any)
	if len(shareholders) != 1 || shareholders[0].(map[string]any)["percentSharesHeld"] != float64(100) {
		t.Errorf("shareholders = %v, want the registered one as given", shareholders)
	}
	if code, _ := vendorsimtest.Call(t, ctl, "POST", "/sim/creditsafe/companies", `{"country":"GB","regNo":"1","directors":[{"role":"x"}]}`, nil); code != 400 {
		t.Errorf("a director with no name: %d, want 400", code)
	}

	vendorsimtest.Call(t, ctl, "POST", "/sim/outcome", `{"check":"company","outcome":"dissolved"}`, nil)
	_, report = vendorsimtest.Call(t, svc, "GET", "/creditsafe/companies/"+id, "", auth)
	if vendorsimtest.At(report, "report.companySummary.companyStatus.status") != "NonActive" ||
		vendorsimtest.At(report, "report.companySummary.companyStatus.providerStatus") != "Dissolved" {
		t.Errorf("dissolved: %v", vendorsimtest.At(report, "report.companySummary.companyStatus"))
	}

	_, out = vendorsimtest.Call(t, ctl, "POST", "/sim/reset", `{"scope":"flow"}`, nil)
	if vendorsimtest.At(out, "cleared.companies") != float64(1) {
		t.Errorf("flow reset cleared %v, want the company", out["cleared"])
	}
	_, report = vendorsimtest.Call(t, svc, "GET", "/creditsafe/companies/"+id, "", auth)
	if vendorsimtest.At(report, "report.companySummary.businessName") != nil {
		t.Errorf("after a reset the company is not registered: %v", report)
	}
}

func TestPortfolioIdsSurviveAFlowResetAndRestartOnAnAllReset(t *testing.T) {
	svc, ctl := surfaces(t)
	auth := token(t, svc)
	create := func() any {
		_, out := vendorsimtest.Call(t, svc, "POST", "/creditsafe/monitoring/portfolios", `{"name":"p"}`, auth)
		return out["portfolioId"]
	}
	create()
	vendorsimtest.Call(t, ctl, "POST", "/sim/reset", `{"scope":"flow"}`, nil)
	if id := create(); id != float64(2) {
		t.Errorf("after a flow reset: portfolio %v, want 2", id)
	}
	_, out := vendorsimtest.Call(t, ctl, "POST", "/sim/reset", `{"scope":"all"}`, nil)
	if vendorsimtest.At(out, "cleared.portfolios") != float64(2) {
		t.Errorf("all reset cleared %v", out["cleared"])
	}
	auth = token(t, svc)
	if id := create(); id != float64(1) {
		t.Errorf("after an all reset: portfolio %v, want 1", id)
	}
}
