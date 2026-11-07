package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestEveryRouteSaysWhatItIsForAndWhatItReaches(t *testing.T) {
	a := testApp(t, nil)
	groups := map[string]int{}
	for _, g := range apiGroups {
		groups[g.ID] = 0
	}
	seen := map[string]bool{}
	for _, rt := range a.apiRoutes() {
		for _, p := range rt.patterns() {
			if seen[p] {
				t.Errorf("%s is registered twice", p)
			}
			seen[p] = true
		}
		key := rt.Method + " " + rt.Path
		if rt.Path != "/health" && !strings.HasPrefix(rt.Path, "/v1") {
			t.Errorf("%s: every route but /health is under /v1", key)
		}
		if rt.Alias != "" && !strings.HasPrefix(rt.Alias, "/api") {
			t.Errorf("%s: an alias is one of the console's old /api paths, not %s", key, rt.Alias)
		}
		if _, ok := groups[rt.Group]; !ok {
			t.Errorf("%s: group %q is not one of apiGroups", key, rt.Group)
		}
		groups[rt.Group]++
		if strings.TrimSpace(rt.Summary) == "" || strings.TrimSpace(rt.Reaches) == "" {
			t.Errorf("%s: a route needs a summary and what it reaches", key)
		}
		if rt.handler == nil && key != "GET /v1" {
			t.Errorf("%s has no handler", key)
		}
	}
	for id, n := range groups {
		if n == 0 {
			t.Errorf("group %q has no routes", id)
		}
	}
}

// fill puts a value in every path parameter, so a pattern can be called.
func fill(path string) string {
	return regexp.MustCompile(`\{[^}]*\}`).ReplaceAllString(path, "x")
}

func TestTheIndexListsExactlyTheRoutesServed(t *testing.T) {
	a := testApp(t, nil)
	w := call(t, a, "GET", "/v1", "")
	if w.Code != 200 {
		t.Fatalf("GET /v1 = %d", w.Code)
	}
	var got struct {
		Build  string       `json:"build"`
		Docs   string       `json:"docs"`
		Groups []routeGroup `json:"groups"`
		Routes []route      `json:"routes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	var again struct {
		Build string `json:"build"`
	}
	_ = json.Unmarshal(call(t, a, "GET", "/api", "").Body.Bytes(), &again)
	if got.Build == "" || got.Build != again.Build {
		t.Errorf("build = %q, then %q: it names the table, and must not move between calls", got.Build, again.Build)
	}
	if got.Docs != apiDoc {
		t.Errorf("docs = %q", got.Docs)
	}
	table := a.apiRoutes()
	if len(got.Routes) != len(table) || len(got.Groups) != len(apiGroups) {
		t.Fatalf("index has %d routes in %d groups, table %d in %d", len(got.Routes), len(got.Groups), len(table), len(apiGroups))
	}
	// Each listed route is served, at its path and at its alias: the mux
	// answers 404 for a path it does not know and 405 for a method it does
	// not take on a known path.
	for _, rt := range got.Routes {
		method := rt.Method
		if method == "ANY" {
			method = "GET"
		}
		for _, p := range []string{rt.Path, rt.Alias} {
			if p == "" {
				continue
			}
			w := call(t, a, method, fill(p), "")
			if w.Code == 405 || (w.Code == 404 && strings.Contains(w.Body.String(), "404 page not found")) {
				t.Errorf("%s %s is listed but not served (%d)", method, p, w.Code)
			}
		}
	}
}

// An alias is the same route, not a copy of it: the console's old path and
// the /v1 one answer alike.
func TestAnAliasAnswersAsItsRouteDoes(t *testing.T) {
	a := testApp(t, nil)
	for _, pair := range [][2]string{
		{"/v1/lab", "/api/overview"},
		{"/v1/merchants", "/api/merchants"},
		{"/v1/stand-ins", "/api/stand-ins"},
	} {
		v1 := call(t, a, "GET", pair[0], "")
		old := call(t, a, "GET", pair[1], "")
		if v1.Code != old.Code || v1.Body.String() != old.Body.String() {
			t.Errorf("%s (%d) and %s (%d) differ", pair[0], v1.Code, pair[1], old.Code)
		}
	}
}

func TestATokenIsAskedForWhenOneIsSet(t *testing.T) {
	a := testApp(t, nil)
	a.token = "s3cret"
	h := a.routes()
	do := func(method, path, auth string) int {
		r := httptest.NewRequest(method, path, nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := do("GET", "/health", ""); c != 200 {
		t.Errorf("health without a token = %d; a probe needs no secret", c)
	}
	if c := do("GET", "/v1/merchants", ""); c != 401 {
		t.Errorf("no token = %d, want 401", c)
	}
	if c := do("GET", "/v1/merchants", "Bearer wrong"); c != 401 {
		t.Errorf("wrong token = %d, want 401", c)
	}
	if c := do("GET", "/v1/merchants", "Bearer s3cret"); c != 200 {
		t.Errorf("right token = %d, want 200", c)
	}
}

// The passthrough is the service's own answer, status and body as given,
// and it carries no credential of the caller's on to the service.
func TestThePassthroughIsTheServicesOwnAnswer(t *testing.T) {
	var gotPath, gotQuery, gotBody, gotAuth, gotMethod string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotMethod, gotPath, gotQuery, gotBody, gotAuth = r.Method, r.URL.Path, r.URL.RawQuery, string(b), r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(422)
		_, _ = w.Write([]byte(`{"error":"the vendor's own words"}`))
	}))
	defer peer.Close()
	a := testApp(t, peer)
	r := httptest.NewRequest("POST", "/v1/services/worldline/sim/settlement-cycle/run?slot=morning", strings.NewReader(`{"x":1}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer the-lab-token")
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != 422 || !strings.Contains(w.Body.String(), "the vendor's own words") {
		t.Fatalf("answer %d %s, want the service's 422 verbatim", w.Code, w.Body.String())
	}
	if gotMethod != "POST" || gotPath != "/sim/settlement-cycle/run" || gotQuery != "slot=morning" || gotBody != `{"x":1}` {
		t.Errorf("service saw %s %s?%s %s", gotMethod, gotPath, gotQuery, gotBody)
	}
	if gotAuth != "" {
		t.Errorf("the caller's Authorization reached the service: %q", gotAuth)
	}
	if w := call(t, a, "GET", "/v1/services/nope/sim/activity", ""); w.Code != 404 {
		t.Errorf("an unknown service = %d, want 404", w.Code)
	}
}

// docs/api/lab-api.md documents each route under a heading
// "### `METHOD /path`". It is the long form of the table, so it has to name
// the same routes: a route added without its documentation, or documented
// after it was removed, fails here.
func TestTheReferenceDocumentsExactlyTheseRoutes(t *testing.T) {
	b, err := os.ReadFile("../../" + apiDoc)
	if err != nil {
		t.Fatal(err)
	}
	heading := regexp.MustCompile("(?m)^### `([A-Z]+ /[^`]*)`\\s*$")
	documented := map[string]int{}
	for _, m := range heading.FindAllStringSubmatch(string(b), -1) {
		documented[m[1]]++
	}
	a := testApp(t, nil)
	served := map[string]bool{}
	for _, rt := range a.apiRoutes() {
		served[rt.Method+" "+rt.Path] = true
	}
	var missing, extra, twice []string
	for k := range served {
		if documented[k] == 0 {
			missing = append(missing, k)
		}
	}
	for k, n := range documented {
		if !served[k] {
			extra = append(extra, k)
		}
		if n > 1 {
			twice = append(twice, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	sort.Strings(twice)
	if len(missing) > 0 {
		t.Errorf("%s does not document:\n  %s", apiDoc, strings.Join(missing, "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("%s documents routes the lab API does not serve:\n  %s", apiDoc, strings.Join(extra, "\n  "))
	}
	if len(twice) > 0 {
		t.Errorf("%s documents these twice:\n  %s", apiDoc, strings.Join(twice, "\n  "))
	}
}
