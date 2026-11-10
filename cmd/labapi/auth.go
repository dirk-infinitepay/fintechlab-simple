package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labdb"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// Who is calling, and in which environment (docs/lab-api/environments.md
// §3, §6).
//
// Two kinds of token reach the lab API. The admin token is LAB_API_TOKEN:
// it creates environments and reaches any of them by naming it, in the
// Lab-Environment header (the console's selector) or ?env= (a link or a
// download). An environment's token reaches that environment only. A lab
// with no LAB_API_TOKEN is open: a call with no token is the admin's, and
// an environment's token still scopes the call that carries one.
//
// A call that names no environment is the default's, so every script,
// runner and test written before environments works as it did.

// principal is the caller.
type principal struct {
	admin bool
	// env is the environment an environment token belongs to; empty for
	// the admin.
	env string
}

type principalKey struct{}

func principalOf(ctx context.Context) principal {
	p, _ := ctx.Value(principalKey{}).(principal)
	return p
}

// scopeOf is the environment the call works in: one id, tenant.Default,
// or tenant.All.
func scopeOf(ctx context.Context) string { return tenant.Of(ctx) }

// Route classes: who may call a route, and in what scope.
const (
	// classOpen needs no token at all.
	classOpen = "open"
	// classAny answers any token the same: the index, the services and
	// their health, the clock as it stands.
	classAny = "any"
	// classAdmin is lab-wide: the admin token only.
	classAdmin = "admin"
	// classEnv works in one environment: an environment token's own, or
	// the one the admin names.
	classEnv = "env"
)

// routeClasses says which routes are not environment-scoped; everything
// else is classEnv. Keyed by method and path, as the route table has them.
var routeClasses = map[string]string{
	"GET /health":                      classOpen,
	"GET /v1":                          classAny,
	"GET /v1/openapi.json":             classAny,
	"GET /v1/lab":                      classAny,
	"GET /v1/services":                 classAny,
	"GET /v1/services/{id}":            classAny,
	"POST /v1/services/{id}/probe":     classAny,
	"GET /v1/clock":                    classAny,
	"POST /v1/clock/holds":             classAny,
	"DELETE /v1/clock/holds/{holder}":  classAny,
	"GET /v1/environment":              classAny,
	"GET /v1/environments":             classAny,
	"GET /v1/environments/{id}":        classAny,
	"POST /v1/clock":                   classAdmin,
	"GET /v1/config":                   classAdmin,
	"PUT /v1/config/worldline-channel": classAdmin,
	"GET /v1/stand-ins":                classAdmin,
	"POST /v1/stand-ins/{id}":          classAdmin,
	"GET /v1/logs":                     classAdmin,
	"GET /v1/services/{id}/logs":       classAdmin,
	"POST /v1/actions/settlement-pull": classAdmin,
	"POST /v1/environments":            classAdmin,
	"PATCH /v1/environments/{id}":      classAdmin,
	"DELETE /v1/environments/{id}":     classAdmin,
	"POST /v1/environments/{id}/token": classAdmin,
}

// allScoped are the environment-scoped routes that can also take every
// environment at once ("*"): lab-wide reads and resets the sims answer
// for all of them. The rest work in one environment.
var allScoped = map[string]bool{
	"POST /v1/lab/reset":                   true,
	"POST /v1/services/{id}/reset":         true,
	"GET /v1/services/{id}/activity":       true,
	"GET /v1/traps":                        true,
	"DELETE /v1/traps":                     true,
	"GET /v1/banking-circle/payouts":       true,
	"GET /v1/banking-circle/subscriptions": true,
	"ANY /v1/services/{id}/sim/{path...}":  true,
}

func (rt route) class() string {
	if c, ok := routeClasses[rt.Method+" "+rt.Path]; ok {
		return c
	}
	return classEnv
}

// guard wraps a route's handler: it works out who is calling and in which
// environment, refuses what that caller may not do, and hands the handler
// a context carrying both — the environment in internal/tenant's slot, so
// every call the lab API then makes to a sim's control port names it.
func (a *app) guard(rt route, h http.HandlerFunc) http.HandlerFunc {
	class := rt.class()
	key := rt.Method + " " + rt.Path
	return func(w http.ResponseWriter, r *http.Request) {
		if class == classOpen {
			h(w, r)
			return
		}
		p, ok := a.authenticate(w, r)
		if !ok {
			return
		}
		if class == classAdmin && !p.admin {
			httputilx.Error(w, 403, "this is lab-wide: it takes the admin token (LAB_API_TOKEN), not an environment's")
			return
		}
		scope, status, msg := a.scopeFor(r, p)
		if status != 0 {
			httputilx.Error(w, status, msg)
			return
		}
		if class == classEnv && scope == tenant.All && !allScoped[key] {
			httputilx.Error(w, 400, "this works in one environment: name it in the Lab-Environment header or ?env=")
			return
		}
		ctx := context.WithValue(r.Context(), principalKey{}, p)
		ctx = tenant.With(ctx, scope)
		h(w, r.WithContext(ctx))
	}
}

// authenticate finds the caller from its bearer token, answering 401 when
// there is none the lab knows.
func (a *app) authenticate(w http.ResponseWriter, r *http.Request) (principal, bool) {
	got, hasBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if hasBearer && strings.HasPrefix(got, envTokenPrefix) {
		// An environment's token is that environment or nothing: one the
		// lab does not know (rotated, or its environment deleted) is a
		// 401 even on an open lab, never the admin's.
		if a.db != nil {
			e, err := a.db.ByToken(r.Context(), got)
			if err == nil {
				return principal{env: e.ID}, true
			}
			if !errors.Is(err, labdb.ErrNotFound) {
				httputilx.Error(w, 500, "the lab's database: "+err.Error())
				return principal{}, false
			}
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="lab-api"`)
		httputilx.Error(w, 401, "no environment has this token: it was rotated, or its environment deleted")
		return principal{}, false
	}
	if a.token == "" {
		return principal{admin: true}, true
	}
	if hasBearer && subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) == 1 {
		return principal{admin: true}, true
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="lab-api"`)
	httputilx.Error(w, 401, "this lab API wants Authorization: Bearer $LAB_API_TOKEN, or an environment's token")
	return principal{}, false
}

// scopeFor is the environment the call works in. An environment token's
// own, and a 403 if the call names another; for the admin, the one it
// names, which must exist, or the default.
func (a *app) scopeFor(r *http.Request, p principal) (string, int, string) {
	named := strings.TrimSpace(r.Header.Get(tenant.Header))
	if v := r.URL.Query().Get(tenant.Param); v != "" {
		named = strings.TrimSpace(v)
	}
	if !p.admin {
		if named != "" && named != p.env {
			return "", 403, "this token is " + p.env + "'s: it cannot act in " + named
		}
		return p.env, 0, ""
	}
	switch named {
	case "", tenant.Default:
		return tenant.Default, 0, ""
	case tenant.All:
		return tenant.All, 0, ""
	}
	if a.db == nil {
		return "", 404, "no environment " + named + ": this lab has no environments"
	}
	if _, err := a.db.Get(r.Context(), named); err != nil {
		if errors.Is(err, labdb.ErrNotFound) {
			return "", 404, "no environment " + named
		}
		return "", 500, err.Error()
	}
	return named, 0, ""
}

// scopeTransport carries the call's environment to the sims: every request
// the lab API makes to a control port, with a context that names one, gets
// ?env= and the Lab-Environment header, set by the lab API and never by
// the caller — so an environment's token cannot reach another's records
// by adding ?env= to a passthrough. Other destinations (a plugin's
// base_url) get nothing added.
//
// A service that does not keep environments apart holds only the default's
// records, and would answer any environment with them. So a call in
// another environment is not made to it at all: the transport answers 409
// itself, and whatever view asked says so instead of showing the default's
// data as this environment's.
type scopeTransport struct {
	base  http.RoundTripper
	hosts map[string]string // control host:port → service id
	// aware says whether a service keeps environments apart; nil treats
	// every service as one that does.
	aware func(svc string) bool
}

func (t *scopeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	env, named := tenant.Named(req.Context())
	svc, control := t.hosts[req.URL.Host]
	if !named || !control {
		return t.base.RoundTrip(req)
	}
	if env != tenant.Default && env != tenant.All && t.aware != nil && !t.aware(svc) {
		msg := svc + " does not keep environments apart yet: it holds only the default environment's records, so it is not asked for " + env + "'s"
		body, _ := json.Marshal(map[string]any{"error": msg, "status": 409})
		return &http.Response{
			StatusCode: 409, Status: "409 Conflict", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			Header:  http.Header{"Content-Type": {"application/json"}},
			Body:    io.NopCloser(bytes.NewReader(body)),
			Request: req, ContentLength: int64(len(body)),
		}, nil
	}
	req = req.Clone(req.Context())
	q := req.URL.Query()
	q.Set(tenant.Param, env)
	req.URL.RawQuery = q.Encode()
	req.Header.Set(tenant.Header, env)
	return t.base.RoundTrip(req)
}

// controlHosts is every lab service's control address, host:port → id.
func (a *app) controlHosts() map[string]string {
	out := map[string]string{}
	for _, s := range a.cat.Services {
		if u, err := url.Parse(s.Control); err == nil && u.Host != "" {
			out[u.Host] = s.ID
		}
	}
	return out
}
