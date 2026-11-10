// Labapi is the lab's one API: every lab-level call a person, a script, an
// agent or the console page makes goes here, at /v1 (docs/api/lab-api.md).
//
// It is a client of the simulated services and nothing more. Each route is a
// call to a service's control port, or the lab's own state (the merchant
// registry, the plugin registrations); no vendor traffic goes through it,
// so it being down never stops a platform talking to a vendor. The e2e
// suite steers the lab through it, as an agent does.
//
// The console is a page served elsewhere (cmd/console) that calls this API
// like any other client. The console's old /api/* paths are served here too,
// as aliases, until every plugin registers at /v1 (docs/lab-api/plan.md,
// phase 4).
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/console"
	"github.com/webduvet/fintechlab-simple/internal/labdb"
)

type app struct {
	cat     *console.Catalogue
	mon     *console.Monitor
	reg     *console.Registry
	banks   *console.Banks
	prov    *console.Provisioner
	client  *http.Client
	bcCfg   string
	provErr string
	// keysDir is the lab's keys directory, read for the connect-your-
	// platform downloads: files the lab hands a platform, never ones it
	// uses itself.
	keysDir string
	// bc is Banking Circle's control-port client, the one the Banks view
	// uses, kept directly for the panels that are about Banking Circle
	// alone: subscriptions, payouts, the delivery schedule it is running.
	bc *console.BankingCircleBank
	// token, when set, is the bearer every call but GET /health must carry
	// (LAB_API_TOKEN). Unset is the local default: no auth, so `make up`
	// stays one command.
	token string

	// db is the lab's database: its environments and their registries
	// (internal/labdb). Nil when LAB_DB is "off": the lab is then the
	// default environment alone, as before environments.
	db *labdb.DB
	// envAware is, per lab service, whether it said it supports
	// environments in its GET /sim/info at the last sync.
	envMu    sync.Mutex
	envAware map[string]bool
	// regs are the environments' merchant registries, loaded on first use.
	regMu sync.Mutex
	regs  map[string]*console.Registry
}

func main() {
	addr := env("LISTEN", ":8097")

	// The lab API reaches every simulation on its control port: plain
	// HTTP, no vendor login (internal/simctl). The vendors' credentials are
	// the platform's, and the lab API holds none of them.
	plain := &http.Client{Timeout: 10 * time.Second}

	cat := console.DefaultCatalogue()
	a := &app{
		cat:      cat,
		client:   plain,
		bcCfg:    env("CONSOLE_BC_DELIVERY_CONFIG", "config/banking-circle.json"),
		keysDir:  env("CONSOLE_KEYS_DIR", "keys"),
		token:    os.Getenv("LAB_API_TOKEN"),
		envAware: map[string]bool{},
		regs:     map[string]*console.Registry{},
	}
	// Every call to a control port names the environment the request is
	// in (auth.go): the sims keep each environment's records apart.
	plain.Transport = &scopeTransport{base: http.DefaultTransport, hosts: a.controlHosts(), aware: a.supportsEnvironments}
	a.mon = console.NewMonitor(cat, a.clientFor, envDuration("CONSOLE_PROBE_INTERVAL", 5*time.Second), 3*time.Second)

	stateFile := env("CONSOLE_STATE_FILE", "console-data/registry.json")
	reg, err := console.NewRegistry(stateFile)
	if err != nil {
		log.Fatalf("lab-api: registry: %v", err)
	}
	a.reg = reg

	// The lab's database, beside the registry on the lab API's volume.
	if path := env("LAB_DB", filepath.Join(filepath.Dir(stateFile), "lab.db")); path != "off" {
		db, err := labdb.Open(path)
		if err != nil {
			log.Fatalf("lab-api: database: %v", err)
		}
		defer db.Close()
		a.db = db
	}

	a.bc = &console.BankingCircleBank{BaseURL: a.controlURL("banking-circle"), Client: plain}
	a.banks = console.NewBanks(
		&console.CoreLedgerBank{BaseURL: a.controlURL("bank"), Client: plain},
		a.bc,
	)
	a.prov = &console.Provisioner{
		Client: plain, Registry: reg,
		B4BURL: a.controlURL("b4b"), WorldlineURL: a.controlURL("worldline"),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.mon.Run(ctx)
	go a.syncCustomers(ctx, envDuration("LAB_SYNC_INTERVAL", 5*time.Second))

	log.Printf("lab-api listening on %s (auth: %s)", addr, map[bool]string{true: "bearer token", false: "none"}[a.token != ""])
	log.Fatal(http.ListenAndServe(addr, logReq(a.routes())))
}

func (a *app) routes() http.Handler {
	// The API is the table in routes.go, so what GET /v1 lists is exactly
	// what is served, under its /v1 path and, until phase 4, its /api alias.
	mux := http.NewServeMux()
	table := a.apiRoutes()
	for _, rt := range table {
		h := rt.handler
		if h == nil {
			h = apiIndex(table)
			if rt.Path == "/v1/openapi.json" {
				h = openAPIHandler(table)
			}
		}
		// Each route behind its guard (auth.go): who may call it, and the
		// environment it works in. GET /health stays open so a probe needs
		// no secret, and so does nothing else.
		h = a.guard(rt, h)
		for _, p := range rt.patterns() {
			mux.HandleFunc(p, h)
		}
	}
	return mux
}

// clientFor is the HTTP client for a service: one plain client for every
// control port, and for a plugin's base_url (https there is verified
// against the system's roots).
func (a *app) clientFor(console.Service) *http.Client { return a.client }

func (a *app) baseURL(id string) string {
	s, ok := a.cat.Get(id)
	if !ok {
		return ""
	}
	return s.BaseURL
}

// controlURL is a lab service's control address, where the lab API steers
// it (internal/simctl).
func (a *app) controlURL(id string) string {
	s, ok := a.cat.Get(id)
	if !ok {
		return ""
	}
	return s.Control
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	log.Printf("lab-api: %s=%q is not a duration; using %s", k, v, def)
	return def
}

func logReq(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health probes every few seconds drown out the calls that matter.
		if strings.HasPrefix(r.URL.Path, "/v1") || strings.HasPrefix(r.URL.Path, "/api") {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}
