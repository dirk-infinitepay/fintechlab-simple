package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var update = flag.Bool("update", false, "rewrite docs/api/openapi.yaml from the route table")

const openAPIHeader = "# The lab API as OpenAPI 3.1, generated from cmd/labapi/routes.go and\n" +
	"# cmd/labapi/openapi.go. Do not edit: change those, then\n" +
	"#   go test ./cmd/labapi -run OpenAPI -update\n" +
	"# The lab API serves the same document at GET /v1/openapi.json.\n"

func renderOpenAPIYAML(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString(openAPIHeader)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(openAPI(testApp(t, nil).apiRoutes())); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The checked-in document is the generated one: a route, a summary or an
// annotation changed without rewriting it fails here.
func TestOpenAPIYAMLIsTheGeneratedDocument(t *testing.T) {
	want := renderOpenAPIYAML(t)
	path := "../../" + openAPIDoc
	if *update {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v; generate it: go test ./cmd/labapi -run OpenAPI -update", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is not what the route table generates; rewrite it: go test ./cmd/labapi -run OpenAPI -update", openAPIDoc)
	}
}

// Every route that changes something, or reads a query, says what it takes;
// and nothing is annotated that is not served.
func TestEveryRouteThatTakesSomethingIsAnnotated(t *testing.T) {
	table := testApp(t, nil).apiRoutes()
	served := map[string]bool{}
	for _, rt := range table {
		key := rt.Method + " " + rt.Path
		served[key] = true
		if rt.Method != "GET" {
			if _, ok := opSpecs[key]; !ok {
				t.Errorf("%s takes no annotation in opSpecs: give it its Body, or Bodyless", key)
			}
		}
	}
	for key, spec := range opSpecs {
		if !served[key] {
			t.Errorf("opSpecs annotates %s, which is not a route", key)
		}
		if spec.Body != nil && spec.Bodyless {
			t.Errorf("%s has a body and is Bodyless", key)
		}
		if props, ok := spec.Body["properties"].(map[string]any); ok {
			req, _ := spec.Body["required"].([]string)
			for _, r := range req {
				if _, ok := props[r]; !ok {
					t.Errorf("%s requires %q, which is not one of its properties", key, r)
				}
			}
		}
	}
}

func TestOperationsAreNamedOnceAndLinkToTheirReference(t *testing.T) {
	doc := openAPI(testApp(t, nil).apiRoutes())
	ref, err := os.ReadFile("../../" + apiDoc)
	if err != nil {
		t.Fatal(err)
	}
	anchors := map[string]bool{}
	for _, m := range regexp.MustCompile("(?m)^### `([A-Z]+ /[^`]*)`").FindAllStringSubmatch(string(ref), -1) {
		anchors[anchor(m[1])] = true
	}
	ids := map[string]string{}
	n := 0
	for path, ops := range doc.Paths {
		for method, op := range ops {
			n++
			where := strings.ToUpper(method) + " " + path
			if prev, dup := ids[op.OperationID]; dup {
				t.Errorf("operationId %s is both %s and %s", op.OperationID, prev, where)
			}
			ids[op.OperationID] = where
			if op.Summary == "" {
				t.Errorf("%s has no summary", where)
			}
			frag := op.ExternalDocs.URL[strings.Index(op.ExternalDocs.URL, "#")+1:]
			if !anchors[frag] {
				t.Errorf("%s links to #%s, which is no heading in %s", where, frag, apiDoc)
			}
			for _, p := range op.Parameters {
				if p.In == "path" && !strings.Contains(path, "{"+p.Name+"}") {
					t.Errorf("%s declares path parameter %s it does not have", where, p.Name)
				}
			}
		}
	}
	// ANY is four methods.
	want := 0
	for _, rt := range testApp(t, nil).apiRoutes() {
		if rt.Method == "ANY" {
			want += 4
		} else {
			want++
		}
	}
	if n != want {
		t.Errorf("%d operations, want %d", n, want)
	}
}

func TestTheDocumentIsServed(t *testing.T) {
	a := testApp(t, nil)
	w := call(t, a, "GET", "/v1/openapi.json", "")
	var got struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 || got.OpenAPI != "3.1.0" {
		t.Fatalf("%d %v %.200s", w.Code, err, w.Body)
	}
	var paths []string
	for p := range got.Paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	if _, ok := got.Paths["/v1/services/{id}/sim/{path}"]["post"]; !ok {
		t.Errorf("the passthrough is missing: %v", paths)
	}
	for _, p := range paths {
		if strings.HasPrefix(p, "/api") {
			t.Errorf("an alias is listed: %s", p)
		}
	}
}
