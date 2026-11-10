package tenant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTheLabsOwnIdsAreNotForCreating(t *testing.T) {
	for _, id := range []string{Default, All, "", "Sbx", "1sbx", "sbx_1", strings.Repeat("a", 41)} {
		if Valid(id) == nil {
			t.Errorf("Valid(%q) = nil, want a reason", id)
		}
	}
	for _, id := range []string{"sbx", "e2e-20261110", "a"} {
		if err := Valid(id); err != nil {
			t.Errorf("Valid(%q) = %v", id, err)
		}
	}
}

func TestACallThatNamesNoEnvironmentIsTheDefaults(t *testing.T) {
	r := httptest.NewRequest("GET", "/sim/activity", nil)
	if got := FromRequest(r); got != Default {
		t.Fatalf("FromRequest = %q, want %q", got, Default)
	}
	r.Header.Set(Header, "sbx")
	if got := FromRequest(r); got != "sbx" {
		t.Fatalf("header: FromRequest = %q", got)
	}
	r = httptest.NewRequest("GET", "/sim/activity?env=e2e", nil)
	r.Header.Set(Header, "sbx")
	if got := FromRequest(r); got != "e2e" {
		t.Fatalf("the query wins over the header: %q", got)
	}
}

func TestAScopeTakesItsOwnOrEverything(t *testing.T) {
	cases := []struct {
		scope, owner string
		in           bool
	}{
		{"sbx", "sbx", true}, {"sbx", "e2e", false}, {"", "", true}, {Default, "", true},
		{"", "sbx", false}, {All, "sbx", true}, {All, "", true},
	}
	for _, c := range cases {
		if got := InScope(c.scope, c.owner); got != c.in {
			t.Errorf("InScope(%q, %q) = %v, want %v", c.scope, c.owner, got, c.in)
		}
	}
}

func TestAnInnerHandlerTellsAnOuterOneWhoCalled(t *testing.T) {
	outer := Bind(context.Background())
	if _, named := Named(outer); named {
		t.Fatal("a fresh slot names no one")
	}
	// The inner handler gets a derived context, as r.WithContext gives it.
	inner, cancel := context.WithCancel(Bind(outer))
	defer cancel()
	Set(inner, "sbx")
	if got := Of(outer); got != "sbx" {
		t.Fatalf("Of(outer) = %q after the inner handler set sbx", got)
	}
	if got := Of(context.Background()); got != Default {
		t.Fatalf("no slot: %q", got)
	}
}

func TestSafeguardingAccountsAreTheLabsForDefaultAndDerivedOtherwise(t *testing.T) {
	if got := SafeguardingAccount("", "eur"); got != DefaultSafeguardingEUR {
		t.Fatalf("default EUR = %s", got)
	}
	if got := SafeguardingAccount(Default, "GBP"); got != DefaultSafeguardingGBP {
		t.Fatalf("default GBP = %s", got)
	}
	a, b := SafeguardingAccount("sbx", "EUR"), SafeguardingAccount("sbx", "GBP")
	if a == b || a == DefaultSafeguardingEUR || a != SafeguardingAccount("sbx", "EUR") {
		t.Fatalf("sbx: EUR %s, GBP %s: want distinct, stable, not the default's", a, b)
	}
	if SafeguardingAccount("e2e", "EUR") == a {
		t.Fatal("two environments share a safeguarding account")
	}
}

func TestCustomersArePushedFoundSeenAndDeleted(t *testing.T) {
	s := NewCustomers()
	var put, deleted []string
	s.OnPut = func(c Customer, created bool) error {
		put = append(put, c.Env)
		return nil
	}
	s.OnDelete = func(env string) error { deleted = append(deleted, env); return nil }
	mux := http.NewServeMux()
	s.Routes(mux)

	body := `{"banking_circle": {"username": "sbx-bc", "password": "p"}}`
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("PUT", "/sim/customers/sbx", strings.NewReader(body)))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"created":true`) {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("PUT", "/sim/customers/default", strings.NewReader(`{}`)))
	if rec.Code != 400 {
		t.Fatalf("PUT default: %d, want 400", rec.Code)
	}

	c, ok := s.Find(func(c Customer) bool { return c.BankingCircle != nil && c.BankingCircle.Username == "sbx-bc" })
	if !ok || c.Env != "sbx" {
		t.Fatalf("Find = %+v, %v", c, ok)
	}
	s.Seen("sbx")

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/sim/customers", nil))
	var got struct{ Customers []Status }
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Customers) != 2 || got.Customers[0].Env != Default || got.Customers[1].LastSeen == nil {
		t.Fatalf("GET: %s", rec.Body)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("DELETE", "/sim/customers/sbx", nil))
	if rec.Code != 200 {
		t.Fatalf("DELETE: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("DELETE", "/sim/customers/sbx", nil))
	if rec.Code != 404 {
		t.Fatalf("second DELETE: %d", rec.Code)
	}
	if strings.Join(put, ",") != "sbx" || strings.Join(deleted, ",") != "sbx" {
		t.Fatalf("hooks: put %v, deleted %v", put, deleted)
	}
}
