package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
)

// fakeSeeder is a platform's seeder: it needs two databases, remembers what
// it was given, and writes a real settlement file where it is told to.
type fakeSeeder struct {
	mu    sync.Mutex
	calls []map[string]any
}

func newFakeSeeder(t *testing.T) (*fakeSeeder, *httptest.Server) {
	t.Helper()
	fs := &fakeSeeder{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, map[string]any{"service": "lab-seeder", "databases": []string{"platform", "settle"}})
	})
	record := func(r *http.Request) map[string]any {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fs.mu.Lock()
		fs.calls = append(fs.calls, body)
		fs.mu.Unlock()
		return body
	}
	mux.HandleFunc("POST /seed", func(w http.ResponseWriter, r *http.Request) {
		body := record(r)
		httputilx.WriteJSON(w, 200, map[string]any{"seed": 1234, "merchants": body["merchants"]})
	})
	mux.HandleFunc("POST /settlement-files", func(w http.ResponseWriter, r *http.Request) {
		body := record(r)
		dir, _ := body["dir"].(string)
		name := filepath.Join(dir, "20261111090000_payfac_ER_EUR.csv")
		if err := os.WriteFile(name, bambora(t, "EUR", "70000001"), 0o644); err != nil {
			httputilx.Error(w, 500, err.Error())
			return
		}
		httputilx.WriteJSON(w, 200, map[string]any{"seed": 99, "files": []map[string]any{{"name": filepath.Base(name), "path": name}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fs, srv
}

func TestTheSeederSeedsTheEnvironmentsOwnDatabases(t *testing.T) {
	a, _ := newFilesApp(t)
	fs, srv := newFakeSeeder(t)
	a.seederURL = srv.URL
	_, token := createEnvOnly(t, a, `{"id": "sbx"}`)

	if w := as(t, a, token, "POST", "/v1/platform/seed", `{"merchants": 20}`); w.Code != 409 || !strings.Contains(w.Body.String(), "platform, settle") {
		t.Fatalf("no databases yet: %d %s, want 409 naming both", w.Code, w.Body)
	}
	cfg := `{"config": {"databases": {"platform": "postgres://p:secret@db:5432/platform", "settle": "postgres://p:secret@db:5432/settle"}}}`
	if w := as(t, a, "admin-token", "PATCH", "/v1/environments/sbx", cfg); w.Code != 200 {
		t.Fatalf("set databases: %d %s", w.Code, w.Body)
	}
	if view := as(t, a, "admin-token", "GET", "/v1/environments/sbx", "").Body.String(); strings.Contains(view, "secret") || !strings.Contains(view, `"platform":"(set)"`) {
		t.Fatalf("the view shows the databases: %s", view)
	}
	if w := as(t, a, token, "GET", "/v1/platform", ""); !strings.Contains(w.Body.String(), `"available":true`) || !strings.Contains(w.Body.String(), `"settle":"set"`) {
		t.Fatalf("platform state: %s", w.Body)
	}

	// A seed, then a file, then a reseed that drops the files paying MIDs gone.
	if w := as(t, a, token, "POST", "/v1/platform/seed", `{"merchants": 20, "seed": 1234}`); w.Code != 200 {
		t.Fatalf("seed: %d %s", w.Code, w.Body)
	}
	w := as(t, a, token, "POST", "/v1/settlement-files/generate", `{"transactions": 100, "currency": "eur"}`)
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"source":"generated"`) || !strings.Contains(w.Body.String(), "70000001") {
		t.Fatalf("generate: %d %s", w.Code, w.Body)
	}
	fs.mu.Lock()
	gen := fs.calls[1]
	fs.mu.Unlock()
	dbs, _ := gen["databases"].(map[string]any)
	if dbs["platform"] != "postgres://p:secret@db:5432/platform" || gen["currency"] != "EUR" || !strings.HasSuffix(gen["dir"].(string), filepath.Join("files", "sbx", "generated")) {
		t.Fatalf("the seeder was asked %+v", gen)
	}
	w = as(t, a, token, "POST", "/v1/platform/seed", `{"merchants": 5, "mode": "reseed"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"files_deleted":1`) {
		t.Fatalf("reseed: %d %s", w.Code, w.Body)
	}
	// A config change that leaves databases out keeps them.
	as(t, a, "admin-token", "PATCH", "/v1/environments/sbx", `{"config": {"clock": "wall"}}`)
	if w := as(t, a, token, "GET", "/v1/platform", ""); !strings.Contains(w.Body.String(), `"platform":"set"`) {
		t.Fatalf("the databases were dropped by a change that left them out: %s", w.Body)
	}
}

func TestALabWithoutASeederSaysSo(t *testing.T) {
	a, _ := newFilesApp(t)
	_, token := createEnvOnly(t, a, `{"id": "sbx"}`)
	if w := as(t, a, token, "POST", "/v1/platform/seed", `{"merchants": 5}`); w.Code != 503 || !strings.Contains(w.Body.String(), "LAB_SEEDER_URL") {
		t.Fatalf("seed without a seeder: %d %s", w.Code, w.Body)
	}
	if w := as(t, a, token, "GET", "/v1/platform", ""); !strings.Contains(w.Body.String(), `"available":false`) {
		t.Fatalf("platform state: %s", w.Body)
	}
}
