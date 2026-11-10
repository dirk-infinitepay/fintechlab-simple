package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// in sends one request in environment env (the Lab-Environment header, as
// Banking Circle sends it; none for "").
func in(t *testing.T, h http.Handler, env, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if env != "" {
		r.Header.Set("Lab-Environment", env)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// accounts is GET path's accounts as "iban=balance[@env]", sorted.
func accounts(t *testing.T, h http.Handler, path string) []string {
	t.Helper()
	w := in(t, h, "", "GET", path, "")
	if w.Code != 200 {
		t.Fatalf("GET %s = %d %s", path, w.Code, w.Body)
	}
	var out struct {
		Accounts []account `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range out.Accounts {
		s := a.IBAN + "=" + a.Balance
		if a.Env != "" {
			s += "@" + a.Env
		}
		got = append(got, s)
	}
	sort.Strings(got)
	return got
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// withSbx is the bank with sbx pushed as a customer.
func withSbx(t *testing.T) (*store, http.Handler, http.Handler) {
	t.Helper()
	s := newStore()
	credits, ledger := newLogs()
	mux, ctl := testSurfaces(t, s, credits, ledger)
	if w := in(t, ctl.Handler(), "", "PUT", "/sim/customers/sbx", `{"env":"sbx","name":"Sandbox"}`); w.Code != 200 {
		t.Fatalf("PUT /sim/customers/sbx = %d %s", w.Code, w.Body)
	}
	return s, mux, ctl.Handler()
}

const merchant = "GB00SIMMERCH0000001"

func credit(t *testing.T, h http.Handler, env, amount string) {
	t.Helper()
	if w := in(t, h, env, "POST", "/internal/credit",
		`{"iban":"`+merchant+`","holder":"Southwind","amount":"`+amount+`","currency":"EUR","reference":"SETL-`+env+`"}`); w.Code != 200 {
		t.Fatalf("credit in %q = %d %s", env, w.Code, w.Body)
	}
}

// The same merchant paid from two environments is two accounts, each
// listed, logged and moved only in its own environment.
func TestEachEnvironmentHasItsOwnAccounts(t *testing.T) {
	_, mux, ctl := withSbx(t)
	credit(t, mux, "sbx", "125.00")
	credit(t, mux, "", "10.00")

	def := accounts(t, ctl, "/sim/accounts")
	if len(def) != 6 || !has(def, merchant+"=10.00") {
		t.Errorf("the default's accounts = %v, want the five seeded and its own credit", def)
	}
	if sbx := accounts(t, ctl, "/sim/accounts?env=sbx"); len(sbx) != 1 || sbx[0] != merchant+"=125.00@sbx" {
		t.Errorf("sbx's accounts = %v, want only its own", sbx)
	}
	if all := accounts(t, ctl, "/sim/accounts?env=*"); len(all) != 7 {
		t.Errorf("every environment's accounts = %v", all)
	}
	// The bank's own API answers the same way.
	if sbx := accounts(t, mux, "/accounts?env=sbx"); len(sbx) != 1 {
		t.Errorf("GET /accounts?env=sbx = %v", sbx)
	}

	// An account opened by hand is the named environment's.
	if w := in(t, ctl, "sbx", "POST", "/sim/accounts", `{"holder":"Acme"}`); w.Code != 201 || !strings.Contains(w.Body.String(), `"env":"sbx"`) {
		t.Errorf("POST /sim/accounts in sbx = %d %s", w.Code, w.Body)
	}
	if n := len(accounts(t, ctl, "/sim/accounts")); n != 6 {
		t.Errorf("the default has %d accounts after sbx opened one, want 6", n)
	}

	// A transfer reaches only the environment's own accounts.
	if w := in(t, mux, "sbx", "POST", "/transfers", `{"fromAccountId":"acc_alice","toAccountId":"acc_bob","amount":"1.00"}`); w.Code != 404 {
		t.Errorf("sbx moving the default's money = %d %s, want 404", w.Code, w.Body)
	}
	if w := in(t, mux, "sbx", "GET", "/accounts/acc_alice", ""); w.Code != 404 {
		t.Errorf("sbx reading the default's account = %d, want 404", w.Code)
	}
	if w := in(t, mux, "sbx", "GET", "/ledger", ""); w.Code != 200 || strings.Count(w.Body.String(), `"env":"sbx"`) != 1 || strings.Contains(w.Body.String(), "10.00") {
		t.Errorf("sbx's ledger = %s, want its one credit", w.Body)
	}

	// Each environment's activity is its own.
	for env, want := range map[string]string{"sbx": "125.00", "default": "10.00"} {
		w := in(t, ctl, "", "GET", "/sim/activity?env="+env, "")
		var got struct {
			Logs []struct {
				Name   string                     `json:"name"`
				Events []struct{ Summary string } `json:"events"`
			} `json:"logs"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if evs := got.Logs[0].Events; len(evs) != 1 || !strings.Contains(evs[0].Summary, want) {
			t.Errorf("%s's credits = %+v, want its one credit of %s", env, evs, want)
		}
	}
}

// A call names one environment the bank has; a change names exactly one.
func TestTheBankRefusesAnEnvironmentItDoesNotHave(t *testing.T) {
	s, mux, ctl := withSbx(t)
	for _, c := range []struct {
		env, method, path, body string
		want                    int
	}{
		{"nope", "POST", "/internal/credit", `{"iban":"X","amount":"1.00"}`, 404},
		{"*", "POST", "/internal/credit", `{"iban":"X","amount":"1.00"}`, 400},
		{"*", "POST", "/sim/accounts", `{"holder":"Acme"}`, 400},
		{"nope", "GET", "/sim/accounts", "", 404},
		{"*", "POST", "/transfers", `{"fromAccountId":"acc_alice","toAccountId":"acc_bob","amount":"1.00"}`, 400},
	} {
		h := mux
		if strings.HasPrefix(c.path, "/sim/") {
			h = ctl
		}
		if w := in(t, h, c.env, c.method, c.path, c.body); w.Code != c.want {
			t.Errorf("%s %s in %q = %d %s, want %d", c.method, c.path, c.env, w.Code, w.Body, c.want)
		}
	}
	if n := len(def(s).entries); n != 0 {
		t.Errorf("refused calls wrote %d ledger entries", n)
	}
}

// A reset of one environment clears only its accounts, ledger and
// activity; the default's seeded accounts come back only with its own.
func TestAResetOfOneEnvironmentKeepsTheOthers(t *testing.T) {
	s, mux, ctl := withSbx(t)
	credit(t, mux, "sbx", "125.00")
	credit(t, mux, "", "10.00")

	w := in(t, ctl, "", "POST", "/sim/reset", `{"env":"sbx"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"cleared":{"accounts":1,"activity":1,"balances":0,"credits":1,"transfers":0}`) {
		t.Fatalf("reset sbx = %d %s", w.Code, w.Body)
	}
	if sbx := accounts(t, ctl, "/sim/accounts?env=sbx"); len(sbx) != 0 {
		t.Errorf("sbx's accounts after its reset = %v", sbx)
	}
	if def := accounts(t, ctl, "/sim/accounts"); !has(def, merchant+"=10.00") {
		t.Errorf("the default's accounts after sbx's reset = %v, want its credit kept", def)
	}

	// And the other way round.
	credit(t, mux, "sbx", "5.00")
	if w := in(t, ctl, "", "POST", "/sim/reset", ""); w.Code != 200 {
		t.Fatalf("reset default = %d %s", w.Code, w.Body)
	}
	if sbx := accounts(t, ctl, "/sim/accounts?env=sbx"); len(sbx) != 1 || sbx[0] != merchant+"=5.00@sbx" {
		t.Errorf("sbx's accounts after the default's reset = %v", sbx)
	}
	if n := len(def(s).accts); n != 5 {
		t.Errorf("the default has %d accounts after its reset, want the 5 seeded", n)
	}
}

// Deleting a customer drops its accounts and its activity.
func TestDeletingACustomerDropsItsAccounts(t *testing.T) {
	s, mux, ctl := withSbx(t)
	credit(t, mux, "sbx", "125.00")
	if w := in(t, ctl, "", "DELETE", "/sim/customers/sbx", ""); w.Code != 200 {
		t.Fatalf("DELETE /sim/customers/sbx = %d %s", w.Code, w.Body)
	}
	s.mu.Lock()
	_, held := s.books["sbx"]
	s.mu.Unlock()
	if held {
		t.Error("sbx's book outlived it")
	}
	if all := accounts(t, ctl, "/sim/accounts?env=*"); len(all) != 5 {
		t.Errorf("every account after sbx was deleted = %v, want the default's five", all)
	}
	if w := in(t, ctl, "", "GET", "/sim/activity?env=*", ""); strings.Contains(w.Body.String(), "125.00") {
		t.Errorf("sbx's credit is still in the activity: %s", w.Body)
	}
	if w := in(t, mux, "sbx", "POST", "/internal/credit", `{"iban":"X","amount":"1.00"}`); w.Code != 404 {
		t.Errorf("a credit for the deleted sbx = %d, want 404", w.Code)
	}
}
