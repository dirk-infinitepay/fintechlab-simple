package console

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every service in the catalogue has an API reference,
// docs/api/services/<id>.md, which documents each route under a heading
// "### `METHOD /path`". These tests hold the references to the source: a
// route registered without its documentation, or documented after it was
// removed, fails here.

// serviceSource is where each service's routes are registered, relative to
// the repo root. A service added to the catalogue has to be added here too,
// which is the point: it cannot arrive without a reference.
var serviceSource = map[string][]string{
	"worldline":      {"cmd/worldline"},
	"b4b":            {"cmd/b4b"},
	"banking-circle": {"cmd/bankingcircle"},
	"aci":            {"cmd/aci"},
	"bank":           {"cmd/bank"},
	"verify":         {"cmd/verify"},
	"verification":   {"cmd/verification"},
	"clock":          {"cmd/clock", "internal/labclock"},
	"settlement":     {"cmd/settlement"},
	"receiver":       {"cmd/receiver"},
	"payment-api":    {"cmd/paymentapi"},
	"notifier":       {"cmd/notifier"},
}

const repoRoot = "../.."

var (
	// A Go 1.22 mux pattern as a string literal: "POST /sim/outcome".
	routeLiteral = regexp.MustCompile(`"((?:GET|POST|PUT|PATCH|DELETE|HEAD) /[^"]*)"`)
	// internal/labswitch mounts GET and POST on the path it is given.
	switchRoutes = regexp.MustCompile(`\.Routes\([\w.]+,\s*"(/[^"]*)"\)`)
	// internal/simctl mounts the control contract every sim serves.
	simctlMount  = regexp.MustCompile(`simctl\.(Mount|NewControl)\(`)
	simctlRoutes = []string{"GET /sim/info", "GET /sim/logs", "POST /sim/reset"}
	// The reference's route headings.
	docHeading = regexp.MustCompile("(?m)^### `([A-Z]+ /[^`]*)`\\s*$")
)

func TestEveryServiceHasAnAPIReference(t *testing.T) {
	for _, s := range DefaultCatalogue().Services {
		if _, ok := serviceSource[s.ID]; !ok {
			t.Errorf("%s: add where its routes are registered to serviceSource", s.ID)
		}
		doc := filepath.Join(repoRoot, "docs/api/services", s.ID+".md")
		if _, err := os.Stat(doc); err != nil {
			t.Errorf("%s has no API reference: %v", s.ID, err)
		}
	}
}

func TestEachReferenceDocumentsExactlyTheRoutesItsServiceRegisters(t *testing.T) {
	for id, dirs := range serviceSource {
		t.Run(id, func(t *testing.T) {
			registered := registeredRoutes(t, dirs)
			if len(registered) == 0 {
				t.Fatalf("found no routes under %v: has the service moved?", dirs)
			}
			doc := filepath.Join(repoRoot, "docs/api/services", id+".md")
			b, err := os.ReadFile(doc)
			if err != nil {
				t.Fatal(err)
			}
			documented := map[string]int{}
			for _, m := range docHeading.FindAllStringSubmatch(string(b), -1) {
				documented[m[1]]++
			}
			var missing, extra, twice []string
			for r := range registered {
				if documented[r] == 0 {
					missing = append(missing, r)
				}
			}
			for r, n := range documented {
				if !registered[r] {
					extra = append(extra, r)
				}
				if n > 1 {
					twice = append(twice, r)
				}
			}
			sort.Strings(missing)
			sort.Strings(extra)
			sort.Strings(twice)
			if len(missing) > 0 {
				t.Errorf("%s does not document:\n  %s", doc, strings.Join(missing, "\n  "))
			}
			if len(extra) > 0 {
				t.Errorf("%s documents routes %v does not register:\n  %s", doc, dirs, strings.Join(extra, "\n  "))
			}
			if len(twice) > 0 {
				t.Errorf("%s documents these twice:\n  %s", doc, strings.Join(twice, "\n  "))
			}
		})
	}
}

// The endpoints on a service's card (Endpoints in the catalogue) are the
// ones the console shows a person, on the card and on Docs → API reference.
// Each has to be a route the service serves: a card that names a path the
// service does not answer sends someone to curl a 404.
func TestEveryEndpointOnACardIsServed(t *testing.T) {
	param := regexp.MustCompile(`\{[^}]*\}`)
	norm := func(route string) string { return param.ReplaceAllString(route, "{}") }
	for _, s := range DefaultCatalogue().Services {
		dirs, ok := serviceSource[s.ID]
		if !ok {
			continue // TestEveryServiceHasAnAPIReference reports it
		}
		served := map[string]bool{}
		for r := range registeredRoutes(t, dirs) {
			served[norm(r)] = true
		}
		for _, e := range s.Endpoints {
			for _, method := range strings.Split(e.Method, "|") {
				for _, path := range expandCataloguePath(e.Path) {
					if r := method + " " + path; !served[norm(r)] {
						t.Errorf("%s: the card lists %s (from %q), which %v does not register", s.ID, r, e.Method+" "+e.Path, dirs)
					}
				}
			}
		}
	}
}

// expandCataloguePath turns the catalogue's shorthand into the routes it
// stands for: a listener prefix (":8095 /internal/…") and a query string are
// dropped, "[/{x}]" is optional, and "a|b" in a segment is either.
func expandCataloguePath(p string) []string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	if strings.HasPrefix(p, ":") {
		if i := strings.IndexByte(p, ' '); i >= 0 {
			p = p[i+1:]
		}
	}
	if i := strings.IndexByte(p, '['); i >= 0 {
		depth := 0
		for j := i; j < len(p); j++ {
			switch p[j] {
			case '[':
				depth++
			case ']':
				depth--
				if depth == 0 {
					without := expandCataloguePath(p[:i] + p[j+1:])
					with := expandCataloguePath(p[:i] + p[i+1:j] + p[j+1:])
					return append(without, with...)
				}
			}
		}
	}
	out := []string{""}
	for k, seg := range strings.Split(p, "/") {
		var next []string
		for _, prefix := range out {
			for _, alt := range strings.Split(seg, "|") {
				if k == 0 {
					next = append(next, alt)
				} else {
					next = append(next, prefix+"/"+alt)
				}
			}
		}
		out = next
	}
	return out
}

// registeredRoutes reads the mux patterns out of a service's Go source
// (tests excluded).
func registeredRoutes(t *testing.T, dirs []string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(repoRoot, dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range routeLiteral.FindAllStringSubmatch(string(b), -1) {
				out[m[1]] = true
			}
			for _, m := range switchRoutes.FindAllStringSubmatch(string(b), -1) {
				out["GET "+m[1]] = true
				out["POST "+m[1]] = true
			}
			if simctlMount.Match(b) {
				for _, r := range simctlRoutes {
					out[r] = true
				}
			}
			if bytes.Contains(b, []byte("simctl.NewControl(")) {
				// The control port's own health check.
				out["GET /health"] = true
			}
		}
	}
	return out
}
