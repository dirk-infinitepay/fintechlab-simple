package main

import (
	"bytes"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/webduvet/fintechlab-simple/internal/allowlist"
	"github.com/webduvet/fintechlab-simple/internal/b4b"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// Environments (docs/lab-api/environments.md, sim-control.md §8): every
// platform using the lab is one B4B customer, and B4B knows a customer the
// way the real vendor does, by the key its JWTs are signed with. The default
// environment's is b4b's own configuration -- B4B_JWT_KEY_ID and its key,
// B4B_CALLBACK_URL, WEBHOOK_ALLOWLIST -- and every other's is pushed to
// PUT /sim/customers/{env} on the control port: a key id, the public half of
// its key, where it is called back, and the hosts it may be called back on.
//
// A JWT's kid says whose a vendor call is (requireAuth); everything the call
// makes is that environment's, and it sees only its own. The lab's /sim
// hooks act in the environment the call names (simScoped).

// envConfig is what b4b holds of one environment besides the default.
type envConfig struct {
	kid         string
	key         *rsa.PublicKey
	callbackURL string
	// hosts are the customer's webhook_hosts, which its callbacks may
	// reach on top of WEBHOOK_ALLOWLIST; nil when it names none.
	hosts *allowlist.List
}

// newCustomers is b4b's customer store: what simctl serves at
// /sim/customers.
func (a *app) newCustomers() *tenant.Customers {
	a.envs = map[string]envConfig{}
	c := tenant.NewCustomers()
	c.OnPut = func(cu tenant.Customer, _ bool) error { return a.putEnv(cu) }
	c.OnDelete = a.dropEnv
	return c
}

// putEnv takes a customer in. A refusal leaves what b4b held of that
// environment as it was: its key, if it had one, still verifies.
func (a *app) putEnv(c tenant.Customer) error {
	var cfg envConfig
	if len(c.WebhookHosts) > 0 {
		hosts, err := allowlist.Parse(strings.Join(c.WebhookHosts, ","))
		if err != nil {
			return fmt.Errorf("webhook_hosts: %w", err)
		}
		cfg.hosts = hosts
	}
	if k := c.B4B; k != nil {
		cfg.kid = strings.TrimSpace(k.KeyID)
		switch {
		case cfg.kid == "":
			return errors.New("b4b.key_id is required: it is how b4b tells this environment's JWTs apart")
		case cfg.kid == a.kid:
			return fmt.Errorf("b4b.key_id %q is the default environment's (B4B_JWT_KEY_ID)", cfg.kid)
		}
		key, err := b4b.ParsePublicKeyPEM([]byte(k.PublicKeyPEM))
		if err != nil {
			return fmt.Errorf("b4b.public_key_pem: %w", err)
		}
		cfg.key = key
		if k.CallbackURL != "" {
			if err := allowedBy(a.list, cfg.hosts, k.CallbackURL); err != nil {
				return fmt.Errorf("b4b.callback_url rejected: %w", err)
			}
			cfg.callbackURL = k.CallbackURL
		}
	}
	a.envMu.Lock()
	defer a.envMu.Unlock()
	for env, other := range a.envs {
		if env != c.Env && cfg.kid != "" && other.kid == cfg.kid {
			return fmt.Errorf("b4b.key_id %q is environment %s's", cfg.kid, env)
		}
	}
	a.envs[c.Env] = cfg
	log.Printf("b4b: environment %s: kid=%q callback_url=%q webhook_hosts=%v", c.Env, cfg.kid, cfg.callbackURL, c.WebhookHosts)
	return nil
}

// dropEnv is DELETE /sim/customers/{env}: its key stops verifying first, so
// nothing new arrives while its records go -- every one, as an all reset of
// the environment would, with its traps and its activity.
func (a *app) dropEnv(env string) error {
	a.envMu.Lock()
	delete(a.envs, env)
	a.envMu.Unlock()
	traps, steps := a.traps.ResetIn(env)
	res, err := a.reset(simctl.ScopeAll, env)
	log.Printf("b4b: environment %s deleted: cleared %v, %d trap(s), %d trap step(s)", env, res.Cleared, traps, steps)
	return err
}

// keyFor is the environment a JWT's kid names and the key it is verified
// against: B4B_JWT_KEY_ID is the default's, any other a customer's.
func (a *app) keyFor(kid string) (string, *rsa.PublicKey, error) {
	if kid == a.kid {
		return tenant.Default, a.pubKey, nil
	}
	a.envMu.RLock()
	defer a.envMu.RUnlock()
	for env, cfg := range a.envs {
		if cfg.kid != "" && cfg.kid == kid {
			return env, cfg.key, nil
		}
	}
	return "", nil, fmt.Errorf("b4b: kid %q is neither the configured %q nor any environment's", kid, a.kid)
}

// verify checks a bearer token and says whose it is.
func (a *app) verify(token string) (string, error) {
	var env string
	err := b4b.VerifyBearerTokenFor(token, func(kid string) (*rsa.PublicKey, error) {
		e, key, err := a.keyFor(kid)
		env = e
		return key, err
	})
	return env, err
}

// callbackFor is env's client-account callback endpoint: B4B_CALLBACK_URL
// for the default, the customer's own for any other (empty: none).
func (a *app) callbackFor(env string) string {
	if env = tenant.Normalize(env); env == tenant.Default {
		return a.callbackURL
	}
	a.envMu.RLock()
	defer a.envMu.RUnlock()
	return a.envs[env].callbackURL
}

// allowed says whether one of env's callbacks may go to dest: when
// WEBHOOK_ALLOWLIST allows it, or the environment's own webhook_hosts name
// its host.
func (a *app) allowed(env, dest string) error {
	var hosts *allowlist.List
	if env = tenant.Normalize(env); env != tenant.Default {
		a.envMu.RLock()
		hosts = a.envs[env].hosts
		a.envMu.RUnlock()
	}
	return allowedBy(a.list, hosts, dest)
}

func allowedBy(list, hosts *allowlist.List, dest string) error {
	err := list.Allowed(dest)
	switch {
	case err == nil:
		return nil
	case hosts == nil:
		return err
	case hosts.Allowed(dest) == nil:
		return nil
	}
	return fmt.Errorf("%w, nor among the environment's webhook_hosts", err)
}

// envOf is the environment a request acts for: the one its JWT named, or
// the one its /sim call named (simScoped).
func envOf(r *http.Request) string { return tenant.Of(r.Context()) }

// simScoped puts a lab hook in the environment the call names (?env=, the
// Lab-Environment header, else the default). A hook that creates or changes
// a record acts in one environment, so "*" is refused, and so is one b4b
// does not serve.
func (a *app) simScoped(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		env := tenant.FromRequest(r)
		switch {
		case env == tenant.All:
			httputilx.Error(w, 400, r.Method+" "+r.URL.Path+" acts in one environment: name it with ?env= or the "+tenant.Header+" header, not \"*\"")
			return
		case env != tenant.Default:
			if _, ok := a.customers.Get(env); !ok {
				httputilx.Error(w, 404, "no environment "+env+" at b4b")
				return
			}
		}
		next(w, r.WithContext(tenant.With(r.Context(), env)))
	}
}

// legacySim is the old /sim paths on the vendor port, until phase 4 retires
// them: behind the JWT as they were, and the default environment's alone.
// They are lab hooks, which act in the environment a call names, and a
// vendor login names only its own; another environment's JWT is refused
// here, and a call naming anything but the default is held to it.
func (a *app) legacySim(ctl http.Handler) http.HandlerFunc {
	return a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if env := envOf(r); env != tenant.Default {
			httputilx.Error(w, 403, "the old /sim paths on b4b's vendor port are the default environment's; environment "+env+"'s are on the control port")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/sim/customers") {
			httputilx.Error(w, 404, r.URL.Path+" is on b4b's control port only")
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/sim/reset" {
			body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
			if err != nil {
				httputilx.Error(w, 400, err.Error())
				return
			}
			var named struct {
				Env string `json:"env"`
			}
			if json.Unmarshal(body, &named) == nil && tenant.Normalize(named.Env) != tenant.Default {
				httputilx.Error(w, 403, "the old POST /sim/reset on b4b's vendor port resets the default environment only")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		ctl.ServeHTTP(w, defaultScoped(r))
	})
}

// defaultScoped is r naming the default environment, whatever it named.
func defaultScoped(r *http.Request) *http.Request {
	r = r.Clone(r.Context())
	q := r.URL.Query()
	q.Del(tenant.Param)
	r.URL.RawQuery = q.Encode()
	r.Header.Del(tenant.Header)
	return r
}
