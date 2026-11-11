package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/console"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labdb"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
	"github.com/webduvet/fintechlab-simple/internal/worldline"
)

// bambora is a real Worldline settlement file for currency.
func bambora(t *testing.T, currency, mid string) []byte {
	t.Helper()
	f := worldline.GenerateSettlementFile([]worldline.Transaction{
		{ID: "t1", MID: mid, Currency: currency, AmountCents: 4200, Date: "2026-11-09"},
	}, currency, "2026-11-09", "2026-11-09", worldline.BamboraOptions{})
	var b bytes.Buffer
	if err := f.WriteCSV(&b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// fakeWorldline renders and publishes as Worldline's lab hooks do, and
// remembers what it was asked to publish, in which environment.
type fakeWorldline struct {
	mu        sync.Mutex
	published []string // env/name?wire
}

func newFilesApp(t *testing.T) (*app, *fakeWorldline) {
	t.Helper()
	wl := &fakeWorldline{}
	customers := tenant.NewCustomers()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /sim/info", func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, map[string]any{"environments": true})
	})
	customers.Routes(mux)
	mux.HandleFunc("POST /sim/settlement-files/render", func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Currency string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b.Currency == "USD" {
			httputilx.Error(w, 404, "nothing settles in USD")
			return
		}
		httputilx.WriteJSON(w, 200, map[string]any{
			"name": tenant.FromRequest(r) + "_ER_" + b.Currency + ".csv", "content": string(bambora(t, b.Currency, "MID-"+tenant.FromRequest(r))),
		})
	})
	mux.HandleFunc("POST /sim/settlement-files/publish", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if _, err := worldline.ParseBamboraCSV(bytes.NewReader(body)); err != nil {
			httputilx.Error(w, 400, err.Error())
			return
		}
		wl.mu.Lock()
		wl.published = append(wl.published, tenant.FromRequest(r)+"/"+r.URL.Query().Get("name")+"?"+r.URL.Query().Get("wire"))
		wl.mu.Unlock()
		httputilx.WriteJSON(w, 201, map[string]any{"published": "/download/" + r.URL.Query().Get("name") + ".pgp"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cat := &console.Catalogue{Services: []console.Service{
		{ID: "worldline", Name: "Worldline", Kind: console.KindVendor, BaseURL: srv.URL, Control: srv.URL, HealthPath: "/health"},
	}}
	reg, _ := console.NewRegistry("")
	db, err := labdb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a := &app{cat: cat, reg: reg, db: db, token: "admin-token", dataDir: t.TempDir(),
		envAware: map[string]bool{}, regs: map[string]*console.Registry{}}
	a.client = &http.Client{Timeout: 5 * time.Second,
		Transport: &scopeTransport{base: http.DefaultTransport, hosts: a.controlHosts(), aware: a.supportsEnvironments}}
	a.prov = &console.Provisioner{Client: a.client, Registry: reg}
	a.mon = console.NewMonitor(cat, func(console.Service) *http.Client { return a.client }, time.Hour, time.Second)
	a.syncOnce(context.Background())
	return a, wl
}

// createEnvOnly creates an environment on a lab with only Worldline.
func createEnvOnly(t *testing.T, a *app, body string) (envView, string) {
	t.Helper()
	w := as(t, a, "admin-token", "POST", "/v1/environments", body)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var got struct {
		Environment envView `json:"environment"`
		Token       string  `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got.Environment, got.Token
}

func TestAnEnvironmentKeepsItsOwnSettlementFiles(t *testing.T) {
	a, _ := newFilesApp(t)
	_, token := createEnvOnly(t, a, `{"id": "sbx"}`)

	w := as(t, a, token, "POST", "/v1/settlement-files/generate", `{"currency": "EUR"}`)
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"currency":"EUR"`) || !strings.Contains(w.Body.String(), "MID-sbx") {
		t.Fatalf("generate in sbx: %d %s", w.Code, w.Body)
	}
	if w := as(t, a, token, "POST", "/v1/settlement-files/generate", `{"currency": "USD"}`); w.Code != 404 {
		t.Fatalf("nothing to settle: %d, want Worldline's 404", w.Code)
	}
	req := httptest.NewRequest("POST", "/v1/settlement-files?name=mine_GBP.csv", bytes.NewReader(bambora(t, "GBP", "M2")))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	a.routes().ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	if w := as(t, a, token, "POST", "/v1/settlement-files?name=junk.txt", "not a settlement file"); w.Code != 201 || !strings.Contains(w.Body.String(), "not a Worldline settlement file") {
		t.Fatalf("an upload the parser refuses is kept, with why: %d %s", w.Code, w.Body)
	}
	if w := as(t, a, token, "POST", "/v1/settlement-files?name=../escape", "x"); w.Code != 400 {
		t.Fatalf("a path for a name: %d", w.Code)
	}

	var list struct{ Files []settlementFile }
	_ = json.Unmarshal(as(t, a, token, "GET", "/v1/settlement-files", "").Body.Bytes(), &list)
	if len(list.Files) != 3 {
		t.Fatalf("sbx's files: %+v", list.Files)
	}
	if w := as(t, a, "admin-token", "GET", "/v1/settlement-files", ""); strings.Contains(w.Body.String(), "mine_GBP") {
		t.Fatal("the default environment lists sbx's file")
	}
	if w := as(t, a, token, "GET", "/v1/settlement-files/preview?name=mine_GBP.csv&lines=2", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"lines":[`) {
		t.Fatalf("preview: %d %s", w.Code, w.Body)
	}
	if w := as(t, a, token, "DELETE", "/v1/settlement-files?name=junk.txt", ""); w.Code != 204 {
		t.Fatalf("delete: %d", w.Code)
	}
}

func TestPublishingTakesOneFilePerCurrencyAndNamesTheEnvironment(t *testing.T) {
	a, wl := newFilesApp(t)
	_, token := createEnvOnly(t, a, `{"id": "sbx"}`)
	as(t, a, token, "POST", "/v1/settlement-files/generate", `{"currency": "EUR"}`)
	as(t, a, token, "POST", "/v1/settlement-files/generate", `{"currency": "GBP"}`)
	req := httptest.NewRequest("POST", "/v1/settlement-files?name=another_EUR.csv", bytes.NewReader(bambora(t, "EUR", "M9")))
	req.Header.Set("Authorization", "Bearer "+token)
	a.routes().ServeHTTP(httptest.NewRecorder(), req)

	if w := as(t, a, token, "POST", "/v1/settlement-files/publish", `{"files": ["sbx_ER_EUR.csv", "another_EUR.csv"]}`); w.Code != 409 {
		t.Fatalf("two EUR files: %d %s, want 409", w.Code, w.Body)
	}
	w := as(t, a, token, "POST", "/v1/settlement-files/publish", `{"files": ["sbx_ER_EUR.csv", "sbx_ER_GBP.csv"], "wire": false}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"published":2`) {
		t.Fatalf("publish: %d %s", w.Code, w.Body)
	}
	wl.mu.Lock()
	defer wl.mu.Unlock()
	if strings.Join(wl.published, ",") != "sbx/sbx_ER_EUR.csv?false,sbx/sbx_ER_GBP.csv?false" {
		t.Fatalf("Worldline was asked to publish %v", wl.published)
	}
}

func TestPollNowCallsThePlatformsOwnPollWithItsToken(t *testing.T) {
	a, _ := newFilesApp(t)
	var got struct {
		auth, fileType string
	}
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct{ FileType string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		got.auth, got.fileType = r.Header.Get("Authorization"), b.FileType
		httputilx.WriteJSON(w, 200, map[string]any{"fileType": b.FileType, "downloaded": []string{"x.pgp"}})
	}))
	defer platform.Close()
	_, token := createEnvOnly(t, a, `{"id": "sbx"}`)

	if w := as(t, a, token, "POST", "/v1/platform/poll", `{"fileType": "ER"}`); w.Code != 409 {
		t.Fatalf("no poll_url yet: %d, want 409", w.Code)
	}
	body := `{"config": {"poll_url": "` + platform.URL + `/api/v1/worldline-downloader/poll", "poll_token": "trigger-secret"}}`
	if w := as(t, a, "admin-token", "PATCH", "/v1/environments/sbx", body); w.Code != 200 {
		t.Fatalf("set poll: %d %s", w.Code, w.Body)
	}
	w := as(t, a, token, "POST", "/v1/platform/poll", `{"fileType": "er"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "x.pgp") || got.auth != "Bearer trigger-secret" || got.fileType != "ER" {
		t.Fatalf("poll: %d %s; the platform saw %+v", w.Code, w.Body, got)
	}
	// The token is the lab's to send, never to show, and survives a change
	// that leaves it out.
	view := as(t, a, "admin-token", "GET", "/v1/environments/sbx", "").Body.String()
	if strings.Contains(view, "trigger-secret") || !strings.Contains(view, `"poll_token":"(set)"`) {
		t.Fatalf("the view: %s", view)
	}
	as(t, a, "admin-token", "PATCH", "/v1/environments/sbx", `{"config": {"poll_url": "`+platform.URL+`/api/v1/worldline-downloader/poll", "clock": "wall"}}`)
	as(t, a, token, "POST", "/v1/platform/poll", `{"fileType": "AR"}`)
	if got.auth != "Bearer trigger-secret" {
		t.Fatalf("the token was dropped by a change that left it out: %q", got.auth)
	}
	if w := as(t, a, token, "POST", "/v1/platform/poll", `{"fileType": "XX"}`); w.Code != 400 {
		t.Fatalf("bad fileType: %d", w.Code)
	}
}

func TestTheDefaultEnvironmentsPollCanBeSet(t *testing.T) {
	a, _ := newFilesApp(t)
	if w := as(t, a, "admin-token", "PATCH", "/v1/environments/default", `{"config": {"poll_url": "http://runner:3109/sim/poll"}}`); w.Code != 200 {
		t.Fatalf("set the default's poll: %d %s", w.Code, w.Body)
	}
	if w := as(t, a, "admin-token", "PATCH", "/v1/environments/default", `{"config": {"settlement_identifier": "X"}}`); w.Code != 400 {
		t.Fatalf("a vendor setting on the default: %d, want 400", w.Code)
	}
	if w := as(t, a, "admin-token", "PATCH", "/v1/environments/default", `{"name": "Mine"}`); w.Code != 400 {
		t.Fatalf("renaming the default: %d, want 400", w.Code)
	}
	if w := as(t, a, "admin-token", "GET", "/v1/environments/default", ""); !strings.Contains(w.Body.String(), "runner:3109") {
		t.Fatalf("the default's view: %s", w.Body)
	}
}
