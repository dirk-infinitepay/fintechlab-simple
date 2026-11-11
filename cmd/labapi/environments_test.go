package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/console"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labdb"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// envSim is a fake lab service: one that keeps environments apart (it has
// customers) or one that does not, recording the environment every call
// to it named.
type envSim struct {
	*httptest.Server
	customers *tenant.Customers
	mu        sync.Mutex
	named     []string  // ?env= of each /sim/echo call
	started   time.Time // its GET /sim/info started_at
}

// restart moves the sim's started_at, as a process that started again
// does, and keeps its customers, as one that keeps them on disk does.
func (s *envSim) restart() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = s.started.Add(time.Minute)
}

func newEnvSim(t *testing.T, aware bool) *envSim {
	s := &envSim{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /sim/info", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		started := s.started
		s.mu.Unlock()
		httputilx.WriteJSON(w, 200, map[string]any{"environments": aware, "started_at": started})
	})
	mux.HandleFunc("GET /sim/echo", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.named = append(s.named, r.URL.Query().Get("env")+"|"+r.Header.Get(tenant.Header))
		s.mu.Unlock()
		httputilx.WriteJSON(w, 200, map[string]any{"env": tenant.FromRequest(r)})
	})
	if aware {
		s.customers = tenant.NewCustomers()
		s.customers.Routes(mux)
	}
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *envSim) lastNamed() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.named) == 0 {
		return ""
	}
	return s.named[len(s.named)-1]
}

// envApp is a lab API with a database, an admin token, Banking Circle as
// a sim that keeps environments apart and the bank as one that does not.
func envApp(t *testing.T) (*app, *envSim, *envSim) {
	t.Helper()
	bc, bank := newEnvSim(t, true), newEnvSim(t, false)
	cat := &console.Catalogue{Services: []console.Service{
		{ID: "banking-circle", Name: "Banking Circle", Kind: console.KindVendor, BaseURL: bc.URL, Control: bc.URL, HealthPath: "/health"},
		{ID: "bank", Name: "Core ledger", Kind: console.KindPlatform, BaseURL: bank.URL, Control: bank.URL, HealthPath: "/health", Activity: "/sim/activity"},
	}}
	reg, err := console.NewRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	db, err := labdb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a := &app{cat: cat, reg: reg, db: db, token: "admin-token", keysDir: t.TempDir(),
		envAware: map[string]bool{}, regs: map[string]*console.Registry{}}
	a.client = &http.Client{Timeout: 5 * time.Second,
		Transport: &scopeTransport{base: http.DefaultTransport, hosts: a.controlHosts(), aware: a.supportsEnvironments}}
	a.prov = &console.Provisioner{Client: a.client, Registry: reg, B4BURL: bc.URL, WorldlineURL: bc.URL}
	a.mon = console.NewMonitor(cat, func(console.Service) *http.Client { return a.client }, time.Hour, time.Second)
	a.syncOnce(context.Background())
	return a, bc, bank
}

func as(t *testing.T, a *app, token, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, req)
	return w
}

func createEnv(t *testing.T, a *app, body string) (envView, string) {
	t.Helper()
	w := as(t, a, "admin-token", "POST", "/v1/environments", body)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var got struct {
		Environment envView           `json:"environment"`
		Token       string            `json:"token"`
		Pushed      map[string]string `json:"pushed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Pushed["banking-circle"] != "ok" {
		t.Fatalf("pushed = %v", got.Pushed)
	}
	if _, ok := got.Pushed["bank"]; ok {
		t.Fatalf("pushed to a sim that keeps no environments: %v", got.Pushed)
	}
	return got.Environment, got.Token
}

func TestAnEnvironmentIsCreatedWithItsTokenAndPushedToTheVendorsThatKeepThem(t *testing.T) {
	a, bc, _ := envApp(t)
	v, token := createEnv(t, a, `{"name": "Infinite sbx", "config": {"settlement_identifier": "Infinite_Payment"}}`)
	if v.ID != "infinite-sbx" || !strings.HasPrefix(token, envTokenPrefix) || v.State != envNeverSeen {
		t.Fatalf("created %+v, token %q", v, token)
	}
	c, ok := bc.customers.Get("infinite-sbx")
	if !ok || c.BankingCircle == nil || c.BankingCircle.Username != "infinite-sbx-bc" || c.Worldline.SettlementIdentifier != "Infinite_Payment" {
		t.Fatalf("banking-circle's customer = %+v, %v", c, ok)
	}
	if c.B4B == nil || !strings.Contains(c.B4B.PublicKeyPEM, "PUBLIC KEY") {
		t.Fatalf("the vendor gets the public half of the B4B key: %+v", c.B4B)
	}
	if w := as(t, a, "admin-token", "POST", "/v1/environments", `{"name": "Infinite sbx"}`); w.Code != 409 {
		t.Fatalf("the same id again: %d", w.Code)
	}
	for _, bad := range []string{`{"id": "default"}`, `{"id": "*"}`, `{"id": "Bad Id"}`, `{"name": "x", "config": {"clock": "sometimes"}}`} {
		if w := as(t, a, "admin-token", "POST", "/v1/environments", bad); w.Code != 400 {
			t.Errorf("%s: %d, want 400", bad, w.Code)
		}
	}
}

func TestAnEnvironmentsTokenActsInThatEnvironmentOnly(t *testing.T) {
	a, bc, bank := envApp(t)
	_, token := createEnv(t, a, `{"id": "sbx"}`)
	createEnv(t, a, `{"id": "e2e"}`)

	w := as(t, a, token, "GET", "/v1/environment", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"sbx"`) {
		t.Fatalf("GET /v1/environment: %d %s", w.Code, w.Body)
	}
	// Its calls to a sim name it, whatever the caller added.
	if w := as(t, a, token, "GET", "/v1/services/banking-circle/sim/echo", ""); w.Code != 200 || bc.lastNamed() != "sbx|sbx" {
		t.Fatalf("passthrough: %d %s; the sim saw %q", w.Code, w.Body, bc.lastNamed())
	}
	if w := as(t, a, token, "GET", "/v1/services/banking-circle/sim/echo?env=e2e", ""); w.Code != 403 {
		t.Fatalf("naming another environment: %d, want 403", w.Code)
	}
	if w := as(t, a, token, "GET", "/v1/environments/e2e", ""); w.Code != 404 {
		t.Fatalf("reading another environment: %d, want 404", w.Code)
	}
	// A sim that keeps no environments holds only the default's.
	if w := as(t, a, token, "GET", "/v1/services/bank/sim/echo", ""); w.Code != 409 {
		t.Fatalf("passthrough to a sim without environments: %d, want 409", w.Code)
	}
	if len(bank.named) != 0 {
		t.Fatal("the call reached the bank")
	}
	// Lab-wide routes are the admin's.
	for _, rt := range [][2]string{{"POST", "/v1/environments"}, {"GET", "/v1/logs"}, {"GET", "/v1/stand-ins"}, {"DELETE", "/v1/environments/sbx"}} {
		if w := as(t, a, token, rt[0], rt[1], `{}`); w.Code != 403 {
			t.Errorf("%s %s with sbx's token: %d, want 403", rt[0], rt[1], w.Code)
		}
	}
	if w := as(t, a, "lenv_nope", "GET", "/v1/environment", ""); w.Code != 401 {
		t.Fatalf("an unknown token: %d, want 401", w.Code)
	}
	if w := as(t, a, token, "GET", "/v1", ""); w.Code != 200 {
		t.Fatalf("the index answers any token: %d", w.Code)
	}
}

func TestTheAdminPicksAnEnvironmentOrGetsTheDefault(t *testing.T) {
	a, bc, _ := envApp(t)
	createEnv(t, a, `{"id": "sbx"}`)
	as(t, a, "admin-token", "GET", "/v1/services/banking-circle/sim/echo", "")
	if got := bc.lastNamed(); got != "default|default" {
		t.Fatalf("no environment named: the sim saw %q", got)
	}
	as(t, a, "admin-token", "GET", "/v1/services/banking-circle/sim/echo", "", tenant.Header, "sbx")
	if got := bc.lastNamed(); got != "sbx|sbx" {
		t.Fatalf("Lab-Environment: sbx: the sim saw %q", got)
	}
	if w := as(t, a, "admin-token", "GET", "/v1/services/banking-circle/sim/echo", "", tenant.Header, "nope"); w.Code != 404 {
		t.Fatalf("an unknown environment: %d, want 404", w.Code)
	}
	if w := as(t, a, "admin-token", "GET", "/v1/merchants", "", tenant.Header, "*"); w.Code != 400 {
		t.Fatalf("a one-environment route across all of them: %d, want 400", w.Code)
	}
	w := as(t, a, "admin-token", "GET", "/v1/environments", "")
	if !strings.Contains(w.Body.String(), `"id":"default"`) || !strings.Contains(w.Body.String(), `"id":"sbx"`) {
		t.Fatalf("the admin's list: %s", w.Body)
	}
}

func TestEachEnvironmentHasItsOwnMerchants(t *testing.T) {
	a, _, _ := envApp(t)
	_, token := createEnv(t, a, `{"id": "sbx"}`)
	body := `{"legal_name": "Sbx Bakery Ltd", "country": "GB"}`
	if w := as(t, a, token, "POST", "/v1/merchants", body); w.Code != 201 {
		t.Fatalf("create in sbx: %d %s", w.Code, w.Body)
	}
	if w := as(t, a, "admin-token", "GET", "/v1/merchants", ""); strings.Contains(w.Body.String(), "Sbx Bakery") {
		t.Fatal("the default's registry shows sbx's merchant")
	}
	if w := as(t, a, token, "GET", "/v1/merchants", ""); !strings.Contains(w.Body.String(), "Sbx Bakery") {
		t.Fatalf("sbx's registry: %s", w.Body)
	}
	// Kept in the database: a fresh registry cache reads it back.
	a.dropRegistry("sbx")
	if w := as(t, a, "admin-token", "GET", "/v1/merchants", "", tenant.Header, "sbx"); !strings.Contains(w.Body.String(), "Sbx Bakery") {
		t.Fatalf("after reload: %s", w.Body)
	}
}

func TestAnEnvironmentsKitCarriesItsOwnLogins(t *testing.T) {
	a, _, _ := envApp(t)
	cat := a.cat
	cat.Services = append(cat.Services, console.Service{ID: "b4b", Name: "B4B", Kind: console.KindVendor, Ports: []string{"8086/http"}})
	_, token := createEnv(t, a, `{"id": "sbx"}`)
	w := as(t, a, token, "GET", "/v1/connect/env", "")
	env := w.Body.String()
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Disposition"), "fintechlab-sbx.env") {
		t.Fatalf("download: %d %s", w.Code, w.Header())
	}
	for _, want := range []string{"FINTECH_SIM_LAB_TOKEN=" + token, "FINTECH_SIM_LAB_ENVIRONMENT=sbx", "B4B_JWT_KEY_ID=sbx-b4b-1",
		"BC_SAFEGUARDING_ACCOUNT_ID_EUR=" + tenant.SafeguardingAccount("sbx", "EUR")} {
		if !strings.Contains(env, want) {
			t.Errorf("the kit has no %s", want)
		}
	}
	w = as(t, a, token, "GET", "/v1/connect/files/b4b/b4b-jwt-private.pem", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "PRIVATE KEY") {
		t.Fatalf("sbx's own B4B key: %d %s", w.Code, w.Body)
	}
	if w := as(t, a, "admin-token", "GET", "/v1/connect/env", ""); strings.Contains(w.Body.String(), "FINTECH_SIM_LAB_TOKEN") {
		t.Fatal("the default's kit names an environment token")
	}
}

func TestDeletingAnEnvironmentClearsItAtTheVendorsAndRevokesItsToken(t *testing.T) {
	a, bc, _ := envApp(t)
	_, token := createEnv(t, a, `{"id": "sbx"}`)
	if w := as(t, a, "admin-token", "DELETE", "/v1/environments/sbx", ""); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if _, ok := bc.customers.Get("sbx"); ok {
		t.Fatal("banking-circle still has sbx")
	}
	if w := as(t, a, token, "GET", "/v1/environment", ""); w.Code != 401 {
		t.Fatalf("the deleted environment's token: %d, want 401", w.Code)
	}
	if w := as(t, a, "admin-token", "DELETE", "/v1/environments/default", ""); w.Code != 400 {
		t.Fatalf("delete default: %d", w.Code)
	}
}

func TestTheSyncGivesARestartedVendorItsCustomersBack(t *testing.T) {
	a, bc, _ := envApp(t)
	createEnv(t, a, `{"id": "sbx"}`)
	// The vendor restarted: its customers are gone, and it has one the
	// lab never made.
	_ = bc.customers.Delete("sbx")
	_, _ = bc.customers.Put(tenant.Customer{Env: "stale"})
	a.syncOnce(context.Background())
	if _, ok := bc.customers.Get("sbx"); !ok {
		t.Fatal("the sync did not give sbx back")
	}
	if _, ok := bc.customers.Get("stale"); ok {
		t.Fatal("the sync left an environment the lab does not have")
	}
}

// A vendor that keeps its customers on disk comes back from a restart
// still listing them, possibly as they were before a change it missed
// while it was down. The sync gives it every customer again once its
// started_at moves, and only then.
func TestTheSyncGivesARestartedVendorEveryCustomerAgain(t *testing.T) {
	a, bc, _ := envApp(t)
	createEnv(t, a, `{"id": "sbx", "name": "Sandbox"}`)
	if _, err := bc.customers.Put(tenant.Customer{Env: "sbx", Name: "as it was"}); err != nil {
		t.Fatal(err)
	}
	a.syncOnce(context.Background())
	if c, _ := bc.customers.Get("sbx"); c.Name != "as it was" {
		t.Fatalf("a vendor that did not restart was given sbx again: %+v", c)
	}

	bc.restart()
	a.syncOnce(context.Background())
	c, ok := bc.customers.Get("sbx")
	if !ok || c.Name != "Sandbox" || c.BankingCircle == nil || c.BankingCircle.Username == "" {
		t.Fatalf("after the vendor restarted, sbx = %+v, %v; want the database's", c, ok)
	}

	if _, err := bc.customers.Put(tenant.Customer{Env: "sbx", Name: "changed since"}); err != nil {
		t.Fatal(err)
	}
	a.syncOnce(context.Background())
	if c, _ := bc.customers.Get("sbx"); c.Name != "changed since" {
		t.Fatalf("the sync pushed every customer again without a second restart: %+v", c)
	}
}

func TestAnEnvironmentIsGreenOnceAVendorHasSeenIt(t *testing.T) {
	a, bc, _ := envApp(t)
	createEnv(t, a, `{"id": "sbx"}`)
	bc.customers.Seen("sbx")
	w := as(t, a, "admin-token", "GET", "/v1/environments/sbx", "")
	var v envView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if v.State != envSeen || v.LastSeen == nil || v.Seen["banking-circle"] == nil {
		t.Fatalf("after a vendor saw it: %+v", v)
	}
	if strings.Join(v.Shared, ",") != "bank" {
		t.Fatalf("shared = %v, want the bank", v.Shared)
	}
}

func TestAPluginIsItsEnvironments(t *testing.T) {
	a, _, _ := envApp(t)
	_, token := createEnv(t, a, `{"id": "sbx"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	p := testPlugin(srv.URL)
	b, _ := json.Marshal(p)
	if w := as(t, a, token, "POST", "/v1/plugins", string(b)); w.Code != 200 {
		t.Fatalf("register in sbx: %d %s", w.Code, w.Body)
	}
	if w := as(t, a, "admin-token", "GET", "/v1/plugins", ""); strings.Contains(w.Body.String(), p.ID) {
		t.Fatal("the default sees sbx's plugin")
	}
	if w := as(t, a, "admin-token", "POST", "/v1/plugins", string(b)); w.Code != 400 {
		t.Fatalf("the default claiming sbx's plugin id: %d, want 400", w.Code)
	}
	if got := a.cat.PluginsIn("sbx"); len(got) != 1 || got[0].Plugin.Env != "sbx" {
		t.Fatalf("sbx's plugins: %+v", got)
	}
	_ = url.Values{}
}

func TestAVendorWithoutEnvironmentsIsNotAskedForAnotherEnvironmentsRecords(t *testing.T) {
	a, _, bank := envApp(t)
	_, token := createEnv(t, a, `{"id": "sbx"}`)
	// Not the passthrough: a route that reads one sim directly.
	w := as(t, a, token, "GET", "/v1/services/bank/activity", "")
	if w.Code == 200 {
		t.Fatalf("sbx read the bank's activity: %s", w.Body)
	}
	if !strings.Contains(w.Body.String(), "does not keep environments apart") {
		t.Fatalf("the answer does not say why: %d %s", w.Code, w.Body)
	}
	bank.mu.Lock()
	defer bank.mu.Unlock()
	if len(bank.named) != 0 {
		t.Fatal("the call reached the bank")
	}
}

func TestARevokedEnvironmentTokenIsNeverTheAdminsEvenOnAnOpenLab(t *testing.T) {
	a, _, _ := envApp(t)
	a.token = "" // an open lab: no token is the admin's
	_, token := createEnv(t, a, `{"id": "sbx"}`)
	if w := as(t, a, "", "POST", "/v1/environments/sbx/token", ""); w.Code != 200 {
		t.Fatalf("rotate on an open lab: %d %s", w.Code, w.Body)
	}
	if w := as(t, a, token, "GET", "/v1/environments", ""); w.Code != 401 {
		t.Fatalf("the rotated-out token: %d, want 401", w.Code)
	}
}

// A change names only what changes: what it leaves out keeps its value,
// so a script can register an environment's databases and jump host
// without restating, or dropping, the rest of its config.
func TestAConfigChangeKeepsWhatItLeavesOut(t *testing.T) {
	a, _ := newFilesApp(t)
	createEnvOnly(t, a, `{"id": "sbx", "config": {"settlement_identifier": "Infinite_Payment", "webhook_hosts": ["alb.example"],
		"poll_url": "http://alb.example/poll", "poll_token": "poll-secret"}}`)
	key, _ := clientKey(t, "")
	body, _ := json.Marshal(map[string]any{"config": map[string]any{
		"databases": map[string]string{"platform": "postgres://p:db-secret@db:5432/platform", "settle": "postgres://p:db-secret@db:5432/settle"},
		"tunnel":    map[string]any{"host": "bastion.example", "user": "jump", "private_key": key},
	}})
	if w := as(t, a, "admin-token", "PATCH", "/v1/environments/sbx", string(body)); w.Code != 200 {
		t.Fatalf("set the databases alone: %d %s", w.Code, w.Body)
	}
	cfg := func() labdb.Config {
		t.Helper()
		e, err := a.db.Get(context.Background(), "sbx")
		if err != nil {
			t.Fatal(err)
		}
		return e.Config
	}
	c := cfg()
	if c.SettlementIdentifier != "Infinite_Payment" || len(c.WebhookHosts) != 1 || c.PollURL != "http://alb.example/poll" ||
		c.PollToken != "poll-secret" || len(c.Databases) != 2 || c.Tunnel == nil || c.Tunnel.PrivateKey != key {
		t.Fatalf("after setting the databases alone: %+v", c)
	}
	// One database, the tunnel's port and pin: the rest stays.
	if w := as(t, a, "admin-token", "PATCH", "/v1/environments/sbx",
		`{"config": {"databases": {"settle": "postgres://p:other@db2:5432/settle"}, "tunnel": {"port": 2222, "host_key": "SHA256:`+strings.Repeat("A", 43)+`"}}}`); w.Code != 200 {
		t.Fatalf("change one database and the tunnel's port: %d %s", w.Code, w.Body)
	}
	c = cfg()
	if c.Databases["platform"] != "postgres://p:db-secret@db:5432/platform" || c.Databases["settle"] != "postgres://p:other@db2:5432/settle" ||
		c.Tunnel.Host != "bastion.example" || c.Tunnel.Port != 2222 || c.Tunnel.PrivateKey != key || c.Tunnel.HostKey == "" {
		t.Fatalf("after changing one database and the port: %+v %+v", c.Databases, *c.Tunnel)
	}
	// "" clears a setting and removes a database; null removes the tunnel.
	if w := as(t, a, "admin-token", "PATCH", "/v1/environments/sbx",
		`{"config": {"poll_url": "", "webhook_hosts": [], "databases": {"settle": ""}, "tunnel": {"host_key": ""}}}`); w.Code != 200 {
		t.Fatalf("clear: %d %s", w.Code, w.Body)
	}
	c = cfg()
	if c.PollURL != "" || len(c.WebhookHosts) != 0 || c.Databases["settle"] != "" || c.Databases["platform"] == "" ||
		c.Tunnel.HostKey != "" || c.SettlementIdentifier != "Infinite_Payment" {
		t.Fatalf("after clearing: %+v", c)
	}
	if w := as(t, a, "admin-token", "PATCH", "/v1/environments/sbx", `{"config": {"tunnel": null}}`); w.Code != 200 || cfg().Tunnel != nil {
		t.Fatalf("remove the tunnel: %d %s", w.Code, w.Body)
	}
}
