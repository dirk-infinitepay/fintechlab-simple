package main

import (
	"encoding/json"
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
		key := rt.Method + " " + rt.Path
		if seen[key] {
			t.Errorf("%s is in the table twice", key)
		}
		seen[key] = true
		if _, ok := groups[rt.Group]; !ok {
			t.Errorf("%s: group %q is not one of apiGroups", key, rt.Group)
		}
		groups[rt.Group]++
		if strings.TrimSpace(rt.Summary) == "" || strings.TrimSpace(rt.Reaches) == "" {
			t.Errorf("%s: a route needs a summary and what it reaches", key)
		}
		if rt.handler == nil && key != "GET /api" {
			t.Errorf("%s has no handler", key)
		}
	}
	for id, n := range groups {
		if n == 0 {
			t.Errorf("group %q has no routes", id)
		}
	}
}

func TestTheIndexListsExactlyTheRoutesServed(t *testing.T) {
	a := testApp(t, nil)
	w := call(t, a, "GET", "/api", "")
	if w.Code != 200 {
		t.Fatalf("GET /api = %d", w.Code)
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
	if got.Build == "" || got.Build != w.Header().Get("X-Console-Build") {
		t.Errorf("build = %q, header = %q: the index should name the page this console serves", got.Build, w.Header().Get("X-Console-Build"))
	}
	if got.Docs != apiDoc {
		t.Errorf("docs = %q", got.Docs)
	}
	table := a.apiRoutes()
	if len(got.Routes) != len(table) || len(got.Groups) != len(apiGroups) {
		t.Fatalf("index has %d routes in %d groups, table %d in %d", len(got.Routes), len(got.Groups), len(table), len(apiGroups))
	}
	// Each listed route is served: the mux answers 404 for a path it does
	// not know and 405 for a method it does not take on a known path.
	for _, rt := range got.Routes {
		path := strings.NewReplacer("{id}", "x", "{action}", "x", "{mid}", "x", "{service}", "x", "{name}", "x").Replace(rt.Path)
		w := call(t, a, rt.Method, path, "")
		if w.Code == 405 || (w.Code == 404 && strings.Contains(w.Body.String(), "404 page not found")) {
			t.Errorf("%s %s is listed but not served (%d)", rt.Method, rt.Path, w.Code)
		}
	}
}

// docs/api/console-api.md documents each route under a heading
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
		t.Errorf("%s documents routes the console does not serve:\n  %s", apiDoc, strings.Join(extra, "\n  "))
	}
	if len(twice) > 0 {
		t.Errorf("%s documents these twice:\n  %s", apiDoc, strings.Join(twice, "\n  "))
	}
}
