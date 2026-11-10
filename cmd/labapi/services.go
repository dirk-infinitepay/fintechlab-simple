package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/webduvet/fintechlab-simple/internal/console"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
)

// --- services -----------------------------------------------------------

// serviceViews is every service with its last probe: the lab's own and the
// registered plugins, as the overview and /v1/services both report them.
func (a *app) serviceViews(ctx context.Context) ([]serviceView, map[string]int) {
	all := a.cat.AllIn(scopeOf(ctx))
	out := make([]serviceView, 0, len(all))
	counts := map[string]int{}
	for _, s := range all {
		st, _ := a.mon.Get(s.ID)
		stopped := s.Plugin != nil && s.Plugin.State == console.PluginStopped
		lapsed := s.Plugin != nil && s.Plugin.State == console.PluginLapsed && st.State != console.StateUp
		if stopped || lapsed {
			// A platform that stopped, or stopped renewing, is not a lab
			// service that is down: grey with the reason, not red. A
			// stopped one is no longer probed, so its last probe — "up",
			// a second before it said goodbye — is not believed either.
			st.State = console.StateUnknown
			st.Detail = "registration " + s.Plugin.State
		}
		counts[st.State]++
		out = append(out, serviceView{Service: s, Status: st})
	}
	return out, counts
}

func (a *app) listServices(w http.ResponseWriter, r *http.Request) {
	out, _ := a.serviceViews(r.Context())
	httputilx.WriteJSON(w, 200, map[string]any{"services": out})
}

func (a *app) getService(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	all, _ := a.serviceViews(r.Context())
	for _, s := range all {
		if s.ID == id {
			httputilx.WriteJSON(w, 200, s)
			return
		}
	}
	httputilx.Error(w, 404, "unknown service "+id)
}

// --- the passthrough ----------------------------------------------------

// passthroughMaxBody bounds what one passthrough call carries up. The /sim
// hooks take small JSON bodies; a seeded batch of transactions is the
// largest, and it is far below this.
const passthroughMaxBody = 32 << 20

// passthroughHeaders are the request headers a passthrough call carries on.
// Authorization is not among them: the lab API's own token is for the lab
// API, and a control port asks for none.
var passthroughHeaders = []string{"Content-Type", "Accept", "If-Match", "If-None-Match"}

// simPassthrough forwards /v1/services/{id}/sim/{path...} to the service's
// own /sim/{path}: method, query and body as sent, and the service's answer,
// status and body, as it gave it. It adds the one thing a caller would
// otherwise need per service: Banking Circle's hooks sit behind its mTLS and
// bearer token, and B4B's behind an RS512 JWT; the lab API holds both.
//
// So a hook a service gains is reachable here the moment it exists, with no
// change to the lab API (docs/lab-api/lab-api.md §5).
func (a *app) simPassthrough(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/v1/services/"+id+"/sim/")
	if !ok {
		httputilx.Error(w, 404, "no such path")
		return
	}
	a.toSim(w, r, id, rest)
}

// serviceLogs and serviceReset are the two hooks every service serves
// (internal/simctl), named so a caller need not know they are /sim routes.
func (a *app) serviceLogs(w http.ResponseWriter, r *http.Request) {
	a.toSim(w, r, r.PathValue("id"), "logs")
}

func (a *app) serviceReset(w http.ResponseWriter, r *http.Request) {
	a.toSim(w, r, r.PathValue("id"), "reset")
}

// labService is a lab service by id, with a control port — not a plugin: a
// platform's own hooks are its declared actions, not the lab's control
// surface.
func (a *app) labService(id string) (console.Service, bool) {
	svc, ok := a.cat.Get(id)
	return svc, ok && svc.Plugin == nil && svc.Control != ""
}

// toSim forwards r to /sim/{rest} on the service's control port: method,
// query and body as sent, and its answer as it gave it. The control port
// asks for no vendor credentials, and the caller's own are not passed on.
func (a *app) toSim(w http.ResponseWriter, r *http.Request, id, rest string) {
	svc, ok := a.labService(id)
	if !ok {
		httputilx.Error(w, 404, "no lab service "+id)
		return
	}
	if !a.inScope(r.Context(), id) {
		httputilx.Error(w, 409, id+" does not keep environments apart yet: it holds only the default environment's records, so it is not reachable in "+scopeOf(r.Context()))
		return
	}
	target := svc.Control + "/sim/" + rest
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	ctx, cancel := reqContext(r)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, r.Method, target, http.MaxBytesReader(w, r.Body, passthroughMaxBody))
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	req.ContentLength = r.ContentLength
	for _, h := range passthroughHeaders {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	resp, err := a.client.Do(req)
	if err != nil {
		httputilx.Error(w, 502, fmt.Sprintf("%s %s: %v", r.Method, target, err))
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Disposition", "ETag", "Location"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// --- clock holds --------------------------------------------------------

// clockHold and clockRelease forward to the clock service as they are: a
// hold is the clock's own contract (docs/plugins.md, The clock), and a run
// that holds the clock through the lab API holds the same clock.
//
// A lab deployed without a clock has nothing to hold: a hold is the 501 a
// move is, and a release is the 404 of a hold that does not exist -- which
// a runner already treats as released, so a run is never stopped by it.
func (a *app) clockHold(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.cat.Get("clock"); !ok {
		httputilx.Error(w, 501, noClockNote)
		return
	}
	a.forward(w, r, http.MethodPost, a.baseURL("clock")+"/clock/holds")
}

func (a *app) clockRelease(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.cat.Get("clock"); !ok {
		httputilx.Error(w, 404, "no hold by "+r.PathValue("holder")+": "+noClockNote)
		return
	}
	a.forward(w, r, http.MethodDelete, a.baseURL("clock")+"/clock/holds/"+url.PathEscape(r.PathValue("holder")))
}
