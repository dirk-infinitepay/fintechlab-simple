package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/console"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labdb"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// Environments (docs/lab-api/environments.md): one platform using the lab
// is one environment, with its own token here and its own customer at
// every vendor that supports environments. The lab's database is their
// record; the sims hold a copy of their own customers, which the lab API
// pushes when an environment is created or changed, and again whenever a
// sim has lost them (it restarted).

// envTokenPrefix marks an environment's token, so the guard can tell one
// from the admin's without a lookup for every call.
const envTokenPrefix = "lenv_"

// envView is one environment as the API answers it.
type envView struct {
	ID        string                `json:"id"`
	Name      string                `json:"name"`
	Default   bool                  `json:"default,omitempty"`
	Config    labdb.Config          `json:"config"`
	CreatedAt *time.Time            `json:"created_at,omitempty"`
	State     string                `json:"state"`
	LastSeen  *time.Time            `json:"last_seen"`
	Seen      map[string]*time.Time `json:"seen_by"`
	// Token is the environment's token: shown to the admin, and to the
	// environment itself (OPEN 17: stored as given).
	Token string `json:"token,omitempty"`
	// Kit is where its .env downloads from.
	Kit string `json:"kit"`
	// Shared are the vendors that do not yet keep environments apart: an
	// environment's platform meets the default's data there.
	Shared []string `json:"shared"`
}

// The states an environment is shown in: green once a vendor has seen its
// platform, grey until then. It never turns red with age (§5).
const (
	envNeverSeen = "never-seen"
	envSeen      = "seen"
)

type envBody struct {
	ID     string        `json:"id"`
	Name   string        `json:"name"`
	Config *labdb.Config `json:"config"`
}

func (a *app) needDB(w http.ResponseWriter) bool {
	if a.db == nil {
		httputilx.Error(w, 503, "this lab has no database: environments are off")
		return false
	}
	return true
}

func (a *app) createEnvironment(w http.ResponseWriter, r *http.Request) {
	if !a.needDB(w) {
		return
	}
	var b envBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&b); err != nil {
		httputilx.Error(w, 400, `want {"name": "…", "id": "…", "config": {…}}: `+err.Error())
		return
	}
	b.Name = strings.TrimSpace(b.Name)
	if b.ID == "" {
		b.ID = slug(b.Name)
	}
	if b.Name == "" {
		b.Name = b.ID
	}
	if err := tenant.Valid(b.ID); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	cfg := labdb.Config{}
	if b.Config != nil {
		cfg = *b.Config
	}
	if err := validConfig(cfg); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	kit, err := newKit(b.ID)
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	e := labdb.Environment{ID: b.ID, Name: b.Name, Token: newToken(), Config: cfg, Kit: kit, CreatedAt: time.Now().UTC()}
	switch err := a.db.Create(r.Context(), e); {
	case errors.Is(err, labdb.ErrExists):
		httputilx.Error(w, 409, "there is already an environment "+b.ID)
		return
	case err != nil:
		httputilx.Error(w, 500, err.Error())
		return
	}
	pushed := a.pushCustomer(r.Context(), e)
	log.Printf("lab-api: environment %s created; customers pushed: %v", e.ID, pushed)
	v := a.view(r.Context(), e, a.seenBy(r.Context()), true)
	httputilx.WriteJSON(w, 201, map[string]any{"environment": v, "token": e.Token, "pushed": pushed})
}

func (a *app) listEnvironments(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r.Context())
	seen := a.seenBy(r.Context())
	out := []envView{}
	if p.admin {
		out = append(out, a.defaultView(seen))
	}
	if a.db != nil {
		list, err := a.db.List(r.Context())
		if err != nil {
			httputilx.Error(w, 500, err.Error())
			return
		}
		for _, e := range list {
			if p.admin || e.ID == p.env {
				out = append(out, a.view(r.Context(), e, seen, true))
			}
		}
	}
	httputilx.WriteJSON(w, 200, map[string]any{"environments": out})
}

func (a *app) getEnvironment(w http.ResponseWriter, r *http.Request) {
	a.answerEnvironment(w, r, r.PathValue("id"))
}

// currentEnvironment answers which environment the call is in: an
// environment token's own, or the one the admin named.
func (a *app) currentEnvironment(w http.ResponseWriter, r *http.Request) {
	scope := scopeOf(r.Context())
	if scope == tenant.All {
		httputilx.Error(w, 400, "the call names every environment; name one")
		return
	}
	a.answerEnvironment(w, r, scope)
}

func (a *app) answerEnvironment(w http.ResponseWriter, r *http.Request, id string) {
	p := principalOf(r.Context())
	if !p.admin && id != p.env {
		httputilx.Error(w, 404, "no environment "+id)
		return
	}
	seen := a.seenBy(r.Context())
	if id == tenant.Default {
		httputilx.WriteJSON(w, 200, a.defaultView(seen))
		return
	}
	if !a.needDB(w) {
		return
	}
	e, err := a.db.Get(r.Context(), id)
	if err != nil {
		envError(w, id, err)
		return
	}
	httputilx.WriteJSON(w, 200, a.view(r.Context(), e, seen, true))
}

func (a *app) patchEnvironment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == tenant.Default {
		httputilx.Error(w, 400, "the default environment is the lab's own: its settings are the lab's deployment")
		return
	}
	if !a.needDB(w) {
		return
	}
	e, err := a.db.Get(r.Context(), id)
	if err != nil {
		envError(w, id, err)
		return
	}
	var b envBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&b); err != nil {
		httputilx.Error(w, 400, `want {"name": "…", "config": {…}}: `+err.Error())
		return
	}
	if b.ID != "" && b.ID != id {
		httputilx.Error(w, 400, "an environment's id does not change")
		return
	}
	if n := strings.TrimSpace(b.Name); n != "" {
		e.Name = n
	}
	if b.Config != nil {
		if err := validConfig(*b.Config); err != nil {
			httputilx.Error(w, 400, err.Error())
			return
		}
		e.Config = *b.Config
	}
	if err := a.db.Update(r.Context(), e); err != nil {
		envError(w, id, err)
		return
	}
	pushed := a.pushCustomer(r.Context(), e)
	httputilx.WriteJSON(w, 200, map[string]any{"environment": a.view(r.Context(), e, a.seenBy(r.Context()), true), "pushed": pushed})
}

func (a *app) deleteEnvironment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == tenant.Default {
		httputilx.Error(w, 400, "the default environment is the lab's own and cannot be deleted; POST /v1/lab/reset clears it")
		return
	}
	if !a.needDB(w) {
		return
	}
	if _, err := a.db.Get(r.Context(), id); err != nil {
		envError(w, id, err)
		return
	}
	// The sims first: if one cannot be reached, the environment stays and
	// the call can be made again, rather than leaving records nothing
	// owns.
	removed := map[string]string{}
	failed := false
	for _, svc := range a.envServices() {
		status, err := a.simCall(context.Background(), http.MethodDelete, svc, "/sim/customers/"+id, nil, nil)
		switch {
		case err != nil:
			removed[svc], failed = err.Error(), true
		case status == 404:
			removed[svc] = "had none"
		case status >= 300:
			removed[svc], failed = fmt.Sprintf("HTTP %d", status), true
		default:
			removed[svc] = "removed"
		}
	}
	if failed {
		httputilx.WriteJSON(w, 502, map[string]any{"error": "not every vendor removed " + id + "; nothing else was removed, try again", "vendors": removed})
		return
	}
	a.cat.RemoveEnv(id)
	a.dropRegistry(id)
	if err := a.db.Delete(r.Context(), id); err != nil {
		envError(w, id, err)
		return
	}
	log.Printf("lab-api: environment %s deleted: %v", id, removed)
	httputilx.WriteJSON(w, 200, map[string]any{"deleted": id, "vendors": removed})
}

func (a *app) rotateEnvironmentToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == tenant.Default {
		httputilx.Error(w, 400, "the default environment has no token of its own: it is the admin's (LAB_API_TOKEN)")
		return
	}
	if !a.needDB(w) {
		return
	}
	e, err := a.db.Get(r.Context(), id)
	if err != nil {
		envError(w, id, err)
		return
	}
	e.Token = newToken()
	if err := a.db.Update(r.Context(), e); err != nil {
		envError(w, id, err)
		return
	}
	httputilx.WriteJSON(w, 200, map[string]any{"id": id, "token": e.Token})
}

func envError(w http.ResponseWriter, id string, err error) {
	if errors.Is(err, labdb.ErrNotFound) {
		httputilx.Error(w, 404, "no environment "+id)
		return
	}
	httputilx.Error(w, 500, err.Error())
}

// --- views and status ------------------------------------------------

func (a *app) defaultView(seen map[string]map[string]*time.Time) envView {
	v := envView{ID: tenant.Default, Name: "Default", Default: true, Config: labdb.Config{},
		Kit: "/v1/connect/env", Shared: []string{}}
	v.Seen, v.LastSeen, v.State = summarise(seen[tenant.Default])
	return v
}

func (a *app) view(ctx context.Context, e labdb.Environment, seen map[string]map[string]*time.Time, withToken bool) envView {
	created := e.CreatedAt
	v := envView{ID: e.ID, Name: e.Name, Config: e.Config, CreatedAt: &created,
		Kit: "/v1/connect/env?env=" + e.ID, Shared: a.sharedServices()}
	if withToken {
		v.Token = e.Token
	}
	v.Seen, v.LastSeen, v.State = summarise(seen[e.ID])
	return v
}

func summarise(by map[string]*time.Time) (map[string]*time.Time, *time.Time, string) {
	if by == nil {
		by = map[string]*time.Time{}
	}
	var last *time.Time
	for _, t := range by {
		if t != nil && (last == nil || t.After(*last)) {
			last = t
		}
	}
	state := envNeverSeen
	if last != nil {
		state = envSeen
	}
	return by, last, state
}

// seenBy is, per environment, when each vendor last saw its platform: the
// sims' own GET /sim/customers, asked now.
func (a *app) seenBy(ctx context.Context) map[string]map[string]*time.Time {
	out := map[string]map[string]*time.Time{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, svc := range a.envServices() {
		wg.Add(1)
		go func(svc string) {
			defer wg.Done()
			var body struct {
				Customers []tenant.Status `json:"customers"`
			}
			c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if _, err := a.simCall(c, http.MethodGet, svc, "/sim/customers", nil, &body); err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, st := range body.Customers {
				if out[st.Env] == nil {
					out[st.Env] = map[string]*time.Time{}
				}
				out[st.Env][svc] = st.LastSeen
			}
		}(svc)
	}
	wg.Wait()
	return out
}

// --- the sims' customers ----------------------------------------------

// customerOf is what a vendor learns of an environment: the logins its
// platform presents, and nothing a vendor would not hold itself.
func customerOf(e labdb.Environment) tenant.Customer {
	k := e.Kit
	return tenant.Customer{
		Env: e.ID, Name: e.Name,
		BankingCircle: &tenant.Login{Username: k.BankingCircleUsername, Password: k.BankingCirclePassword},
		B4B:           &tenant.B4BKey{KeyID: k.B4BKeyID, PublicKeyPEM: k.B4BPublicKeyPEM, CallbackURL: e.Config.B4BCallbackURL},
		Worldline:     &tenant.SFTP{Username: k.SFTPUsername, Password: k.SFTPPassword, SettlementIdentifier: e.Config.SettlementIdentifier},
		Verification:  &tenant.Login{Username: k.VerificationUsername, Password: k.VerificationSecret},
		Verify:        &tenant.APIKey{Key: k.VerifyKey},
		ACI:           &tenant.Delivery{WebhookURL: e.Config.ACIWebhookURL},
		WebhookHosts:  e.Config.WebhookHosts,
	}
}

// pushCustomer sends e's customer to every sim that supports
// environments, and says how each took it. One that is down gets it from
// the sync loop once it is back.
func (a *app) pushCustomer(ctx context.Context, e labdb.Environment) map[string]string {
	c := customerOf(e)
	out := map[string]string{}
	for _, svc := range a.envServices() {
		status, err := a.simCall(context.WithoutCancel(ctx), http.MethodPut, svc, "/sim/customers/"+e.ID, c, nil)
		switch {
		case err != nil:
			out[svc] = "not reached: " + err.Error()
		case status >= 300:
			out[svc] = fmt.Sprintf("HTTP %d", status)
		default:
			out[svc] = "ok"
		}
	}
	return out
}

// envServices are the lab services that support environments, as each
// said in its GET /sim/info at the last sync.
func (a *app) envServices() []string {
	a.envMu.Lock()
	defer a.envMu.Unlock()
	var out []string
	for id, ok := range a.envAware {
		if ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// sharedServices are the vendors a platform talks to that do not keep
// environments apart yet.
func (a *app) sharedServices() []string {
	a.envMu.Lock()
	defer a.envMu.Unlock()
	out := []string{}
	for _, id := range []string{"worldline", "b4b", "banking-circle", "aci", "verify", "verification", "bank"} {
		if _, ok := a.cat.Get(id); !ok {
			continue
		}
		if !a.envAware[id] {
			out = append(out, id)
		}
	}
	return out
}

// inScope is whether a call in ctx's environment may reach svc: every
// service for the default environment or all of them, and only the ones
// that keep environments apart for any other — one that does not holds
// only the default's records.
func (a *app) inScope(ctx context.Context, svc string) bool {
	switch scopeOf(ctx) {
	case tenant.Default, tenant.All:
		return true
	}
	return a.supportsEnvironments(svc)
}

// supportsEnvironments is whether svc keeps environments apart.
func (a *app) supportsEnvironments(svc string) bool {
	a.envMu.Lock()
	defer a.envMu.Unlock()
	return a.envAware[svc]
}

// syncCustomers runs until ctx ends: every interval it asks each lab
// service whether it supports environments, and gives each one that does
// exactly the customers the database has — so a sim that restarted (and
// lost them) has them back within one interval, and one that missed a
// delete forgets it.
func (a *app) syncCustomers(ctx context.Context, every time.Duration) {
	for {
		a.syncOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func (a *app) syncOnce(ctx context.Context) {
	var envs []labdb.Environment
	if a.db != nil {
		list, err := a.db.List(ctx)
		if err != nil {
			log.Printf("lab-api: sync: %v", err)
			return
		}
		envs = list
	}
	for _, s := range a.cat.Services {
		if s.Plugin != nil || s.Control == "" {
			continue
		}
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		var info struct {
			Environments bool `json:"environments"`
		}
		_, err := a.simCall(c, http.MethodGet, s.ID, "/sim/info", nil, &info)
		cancel()
		if err != nil {
			continue // down: keep what we knew
		}
		a.envMu.Lock()
		a.envAware[s.ID] = info.Environments
		a.envMu.Unlock()
		if !info.Environments {
			continue
		}
		var body struct {
			Customers []tenant.Status `json:"customers"`
		}
		c, cancel = context.WithTimeout(ctx, 3*time.Second)
		_, err = a.simCall(c, http.MethodGet, s.ID, "/sim/customers", nil, &body)
		cancel()
		if err != nil {
			continue
		}
		has := map[string]bool{}
		for _, st := range body.Customers {
			has[st.Env] = true
		}
		want := map[string]bool{}
		for _, e := range envs {
			want[e.ID] = true
			if !has[e.ID] {
				c, cancel := context.WithTimeout(ctx, 3*time.Second)
				if status, err := a.simCall(c, http.MethodPut, s.ID, "/sim/customers/"+e.ID, customerOf(e), nil); err != nil || status >= 300 {
					log.Printf("lab-api: sync: %s did not take %s: %v %d", s.ID, e.ID, err, status)
				} else {
					log.Printf("lab-api: sync: gave %s environment %s", s.ID, e.ID)
				}
				cancel()
			}
		}
		for env := range has {
			if env != tenant.Default && !want[env] {
				c, cancel := context.WithTimeout(ctx, 3*time.Second)
				_, _ = a.simCall(c, http.MethodDelete, s.ID, "/sim/customers/"+env, nil, nil)
				cancel()
				log.Printf("lab-api: sync: %s forgot environment %s, which the lab no longer has", s.ID, env)
			}
		}
	}
}

// simCall makes one JSON call to a lab service's control port. ctx names
// no environment of its own here: these calls are about environments,
// not in one.
func (a *app) simCall(ctx context.Context, method, svc, path string, in, out any) (int, error) {
	base := a.controlURL(svc)
	if base == "" {
		return 0, fmt.Errorf("%s has no control port", svc)
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// --- kits ------------------------------------------------------------

// newKit makes an environment's credentials at every vendor: random,
// fake-and-obvious, and its own.
func newKit(id string) (labdb.Kit, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return labdb.Kit{}, fmt.Errorf("b4b key: %w", err)
	}
	priv, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return labdb.Kit{}, err
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return labdb.Kit{}, err
	}
	return labdb.Kit{
		BankingCircleUsername: id + "-bc",
		BankingCirclePassword: "sim-" + randHex(12),
		B4BKeyID:              id + "-b4b-1",
		B4BPrivateKeyPEM:      string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: priv})),
		B4BPublicKeyPEM:       string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub})),
		// Never the default's own SFTP logins (infinitepay, harness): a
		// suffix keeps an environment named after one apart from it.
		SFTPUsername:         id + "-sftp",
		SFTPPassword:         "sim-" + randHex(12),
		VerificationUsername: id + "-verification",
		VerificationSecret:   "sim-" + randHex(12),
		VerifyKey:            "sim-" + randHex(12),
	}, nil
}

// kitEnv is e's kit as the connect downloads apply it.
func kitEnv(e labdb.Environment, labURL string) *console.KitEnv {
	k := e.Kit
	v := map[string]string{
		"WORLDLINE_SFTP_USERNAME":        k.SFTPUsername,
		"WORLDLINE_SFTP_USER":            k.SFTPUsername,
		"WORLDLINE_SFTP_PASSWORD":        k.SFTPPassword,
		"B4B_JWT_KEY_ID":                 k.B4BKeyID,
		"B4B_JWT_PRIVATE_KEY":            strings.TrimSpace(k.B4BPrivateKeyPEM),
		"BC_API_M2M_USERNAME":            k.BankingCircleUsername,
		"BC_API_M2M_PASSWORD":            k.BankingCirclePassword,
		"BC_SAFEGUARDING_ACCOUNT_ID_EUR": tenant.SafeguardingAccount(e.ID, "EUR"),
		"BC_SAFEGUARDING_ACCOUNT_ID_GBP": tenant.SafeguardingAccount(e.ID, "GBP"),
		"CREDITSAFE_USERNAME":            k.VerificationUsername,
		"CREDITSAFE_PASSWORD":            k.VerificationSecret,
		"IBAN_COM_API_KEY":               k.VerificationSecret,
		"KYC6_API_KEY":                   k.VerificationSecret,
		"LEXISNEXIS_CLIENT_ID":           k.VerificationUsername,
		"LEXISNEXIS_CLIENT_SECRET":       k.VerificationSecret,
		"LEXISNEXIS_IVI_API_KEY":         k.VerificationSecret,
		"VERIFICATION_INTERNAL_API_KEY":  k.VerifyKey,
	}
	if e.Config.SettlementIdentifier != "" {
		v["WORLDLINE_SETTLEMENT_IDENTIFIER"] = e.Config.SettlementIdentifier
	}
	return &console.KitEnv{
		Env: e.ID, Name: e.Name, Values: v,
		Files: map[string][]byte{
			"b4b-jwt-private.pem": []byte(k.B4BPrivateKeyPEM),
			"b4b-jwt-public.pem":  []byte(k.B4BPublicKeyPEM),
		},
		Lab: []console.KitVar{
			{Key: "FINTECH_SIM_LAB_URL", Value: labURL},
			{Key: "FINTECH_SIM_LAB_TOKEN", Value: e.Token, Note: "This environment's own token: it reaches only this environment."},
			{Key: "FINTECH_SIM_LAB_ENVIRONMENT", Value: e.ID},
		},
	}
}

// kitEnvFor is the call's environment's kit, nil for the default's.
func (a *app) kitEnvFor(r *http.Request, labURL string) (*console.KitEnv, error) {
	scope := scopeOf(r.Context())
	if scope == tenant.Default || a.db == nil {
		return nil, nil
	}
	e, err := a.db.Get(r.Context(), scope)
	if err != nil {
		return nil, err
	}
	return kitEnv(e, labURL), nil
}

func newToken() string { return envTokenPrefix + randHex(24) }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slug is a name as an environment id: "Infinite sbx" → "infinite-sbx".
func slug(name string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	return s
}

// settlementID is what Worldline accepts as a file name's identifier.
var settlementID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func validConfig(c labdb.Config) error {
	if c.SettlementIdentifier != "" && !settlementID.MatchString(c.SettlementIdentifier) {
		return fmt.Errorf("config.settlement_identifier becomes part of a file name: letters, digits, _ and -, at most 64 (got %q)", c.SettlementIdentifier)
	}
	switch c.Clock {
	case "", string(console.ClockFollows), string(console.ClockWall):
	default:
		return fmt.Errorf(`config.clock is "follows" or "wall", not %q`, c.Clock)
	}
	for _, h := range c.WebhookHosts {
		if strings.TrimSpace(h) == "" || strings.ContainsAny(h, " /") {
			return fmt.Errorf("config.webhook_hosts are host names or host:port, not %q", h)
		}
	}
	for _, u := range []string{c.ACIWebhookURL, c.B4BCallbackURL} {
		if u != "" && !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return fmt.Errorf("a webhook or callback URL is http(s)://…, not %q", u)
		}
	}
	return nil
}
