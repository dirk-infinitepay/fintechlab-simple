package main

import (
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
func (a *app) serviceViews() ([]serviceView, map[string]int) {
	all := a.cat.All()
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
	out, _ := a.serviceViews()
	httputilx.WriteJSON(w, 200, map[string]any{"services": out})
}

func (a *app) getService(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	all, _ := a.serviceViews()
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
// API, and the service's credentials are added here, per service.
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
	svc, ok := a.cat.Get(id)
	if !ok || svc.Plugin != nil {
		// A plugin's /sim is the platform's own business, reached through
		// its declared actions, not through the lab's control surface.
		httputilx.Error(w, 404, "no lab service "+id)
		return
	}
	prefix := "/v1/services/" + id + "/sim/"
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), prefix)
	if !ok {
		httputilx.Error(w, 404, "no such path")
		return
	}
	target := svc.BaseURL + "/sim/" + rest
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
	if err := a.serviceCredentials(r, svc, req); err != nil {
		httputilx.Error(w, 502, fmt.Sprintf("%s: credentials for its /sim hooks: %v", id, err))
		return
	}

	resp, err := a.clientFor(svc).Do(req)
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

// serviceCredentials adds what a service's /sim hooks ask of a caller.
func (a *app) serviceCredentials(r *http.Request, svc console.Service, req *http.Request) error {
	switch svc.ID {
	case "banking-circle":
		tok, err := a.bc.Bearer(req.Context())
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	case "b4b":
		hdr, err := a.prov.AuthHeader()
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", hdr.Get("Authorization"))
	}
	return nil
}

// --- clock holds --------------------------------------------------------

// clockHold and clockRelease forward to the clock service as they are: a
// hold is the clock's own contract (docs/plugins.md, The clock), and a run
// that holds the clock through the lab API holds the same clock.
func (a *app) clockHold(w http.ResponseWriter, r *http.Request) {
	a.forward(w, r, http.MethodPost, a.baseURL("clock")+"/clock/holds")
}

func (a *app) clockRelease(w http.ResponseWriter, r *http.Request) {
	a.forward(w, r, http.MethodDelete, a.baseURL("clock")+"/clock/holds/"+url.PathEscape(r.PathValue("holder")))
}
