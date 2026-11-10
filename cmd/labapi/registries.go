package main

import (
	"context"
	"log"
	"net/http"

	"github.com/webduvet/fintechlab-simple/internal/console"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// Each environment has its own merchant registry (docs/lab-api/
// environments.md §6): the default's is the registry.json it always was;
// every other environment's lives in the lab's database, loaded the first
// time a call needs it.

// registry is the merchant registry of the environment ctx names.
func (a *app) registry(ctx context.Context) (*console.Registry, error) {
	env := scopeOf(ctx)
	if env == tenant.Default || env == tenant.All || a.db == nil {
		return a.reg, nil
	}
	a.regMu.Lock()
	defer a.regMu.Unlock()
	if r, ok := a.regs[env]; ok {
		return r, nil
	}
	doc, err := a.db.Registry(ctx, env)
	if err != nil {
		return nil, err
	}
	db := a.db
	r, err := console.NewRegistryIn(doc, func(b []byte) error {
		// The registry calls this with its own lock held, from the
		// request that changed it; a database that will not take the
		// write fails that request.
		return db.SaveRegistry(context.Background(), env, b)
	})
	if err != nil {
		return nil, err
	}
	a.regs[env] = r
	return r, nil
}

// regFor is registry for a handler: it answers 500 itself when the
// registry cannot be read, and reports whether to go on.
func (a *app) regFor(w http.ResponseWriter, r *http.Request) (*console.Registry, bool) {
	reg, err := a.registry(r.Context())
	if err != nil {
		log.Printf("lab-api: registry for %s: %v", scopeOf(r.Context()), err)
		httputilx.Error(w, 500, "this environment's merchant registry: "+err.Error())
		return nil, false
	}
	return reg, true
}

// provFor is the provisioner for the call's environment: its registry,
// with every call it makes to B4B and Worldline naming the environment
// (the request's context carries it to the control ports).
func (a *app) provFor(reg *console.Registry) *console.Provisioner {
	return &console.Provisioner{
		Client: a.client, Registry: reg,
		B4BURL: a.prov.B4BURL, WorldlineURL: a.prov.WorldlineURL,
	}
}

// dropRegistry forgets a deleted environment's registry.
func (a *app) dropRegistry(env string) {
	a.regMu.Lock()
	defer a.regMu.Unlock()
	delete(a.regs, env)
}
