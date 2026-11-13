package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/console"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// The lab-wide halves of the control contract: one reset for the whole lab,
// and every service's log lines read as one (docs/api/lab-api.md, Lab).
// Each service does its own part (internal/simctl); these only call them,
// in the right order, and put the answers together.

// resetOrder is the order a lab reset goes in: producers before consumers,
// so nothing new arrives at a service that is already clean, and the clock
// last, so no service is reset while the day under it moves
// (docs/lab-api/lab-api.md §4).
var resetOrder = []string{
	"worldline", "aci", "b4b", "banking-circle", "bank", "verify", "creditsafe", "kyc6", "lexisnexis", "iban-com",
	"settlement", "receiver", "payment-api", "notifier", "clock",
}

// labAPIService is the lab API's own entry in a lab reset: its merchant
// registry, which only an "all" reset clears.
const labAPIService = "lab-api"

type labResetRequest struct {
	Scope    string   `json:"scope"`
	Force    bool     `json:"force"`
	Services []string `json:"services"`
}

// labReset is POST /v1/lab/reset: every service's POST /sim/reset, in
// resetOrder, then the lab API's own state. A failure is reported per
// service, not hidden: ok is false and the answer is 502.
func (a *app) labReset(w http.ResponseWriter, r *http.Request) {
	var req labResetRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<10))
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	if len(bytes.TrimSpace(body)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			httputilx.Error(w, 400, `want {"scope": "flow"|"all", "force": false, "services": [...]}: `+err.Error())
			return
		}
	}
	if req.Scope == "" {
		req.Scope = "flow"
	}
	if req.Scope != "flow" && req.Scope != "all" {
		httputilx.Error(w, 400, fmt.Sprintf("scope is flow or all, not %q", req.Scope))
		return
	}
	targets := append(a.resetTargets(), labAPIService)
	if len(req.Services) > 0 {
		for _, id := range req.Services {
			if !slices.Contains(targets, id) {
				httputilx.Error(w, 400, fmt.Sprintf("%q is not a lab service; one of %s", id, strings.Join(targets, ", ")))
				return
			}
		}
		targets = slices.DeleteFunc(targets, func(id string) bool { return !slices.Contains(req.Services, id) })
	}

	// A held clock is a settlement run in flight: reset nothing under it.
	if !req.Force {
		if why := a.clockHeld(r.Context()); why != "" {
			httputilx.Error(w, 409, why+`; {"force": true} resets anyway`)
			return
		}
	}

	results := make([]map[string]any, 0, len(targets))
	ok := true
	env := scopeOf(r.Context())
	for _, id := range targets {
		var res map[string]any
		switch {
		case id == labAPIService:
			res = a.resetOwnState(r.Context(), req.Scope)
		case !a.inScope(r.Context(), id):
			// A service that does not keep environments apart holds only
			// the default's records: an environment's reset leaves it.
			res = map[string]any{"service": id, "status": 200, "skipped": "does not keep environments apart: it holds only the default's"}
		default:
			res = a.resetService(r.Context(), id, req.Scope, req.Force)
		}
		if st, _ := res["status"].(int); st < 200 || st > 299 {
			ok = false
		}
		results = append(results, res)
	}
	status := 200
	if !ok {
		status = 502
	}
	httputilx.WriteJSON(w, status, map[string]any{
		"scope":    req.Scope,
		"env":      env,
		"at":       time.Now().UTC(),
		"ok":       ok,
		"services": results,
	})
}

// resetTargets is every lab service in the catalogue, in resetOrder; one
// the order does not name goes just before the clock.
func (a *app) resetTargets() []string {
	var out []string
	for _, id := range resetOrder {
		if _, ok := a.labService(id); ok && id != "clock" {
			out = append(out, id)
		}
	}
	for _, s := range a.cat.Services {
		if !slices.Contains(resetOrder, s.ID) {
			out = append(out, s.ID)
		}
	}
	if _, ok := a.labService("clock"); ok {
		out = append(out, "clock")
	}
	return out
}

// clockHeld says who holds the lab clock and why, or "" when nobody does or
// the clock cannot be asked (its own reset then reports that).
func (a *app) clockHeld(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var clock struct {
		Holds []struct {
			Holder string `json:"holder"`
			Reason string `json:"reason"`
		} `json:"holds"`
	}
	if err := a.getJSON(ctx, a.client, a.baseURL("clock")+"/clock", &clock); err != nil || len(clock.Holds) == 0 {
		return ""
	}
	h := clock.Holds[0]
	return fmt.Sprintf("the lab clock is held by %s (%s): a settlement run is in flight", h.Holder, h.Reason)
}

// resetService calls one service's POST /sim/reset and reports its answer,
// with "service" and "status" added.
func (a *app) resetService(ctx context.Context, id, scope string, force bool) map[string]any {
	out := map[string]any{"service": id}
	svc, ok := a.labService(id)
	if !ok {
		out["status"], out["error"] = 404, "not in the catalogue"
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"scope": scope, "force": force})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, svc.Control+"/sim/reset", bytes.NewReader(body))
	if err != nil {
		out["status"], out["error"] = 500, err.Error()
		return out
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		out["status"], out["error"] = 502, err.Error()
		return out
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var answer map[string]any
	_ = json.Unmarshal(raw, &answer)
	for k, v := range answer {
		out[k] = v
	}
	out["service"], out["status"] = id, resp.StatusCode
	if resp.StatusCode == 404 && answer["error"] == nil {
		out["error"] = "no POST /sim/reset: a build from before the control contract?"
	}
	return out
}

// resetOwnState is the lab API's part of a lab reset. Plugin registrations
// and stand-in switches are never reset: they are live wiring, not run
// state, and a platform would only register again ten seconds later.
func (a *app) resetOwnState(ctx context.Context, scope string) map[string]any {
	out := map[string]any{"service": labAPIService, "status": 200, "scope": scope, "cleared": map[string]int{}}
	if scope != "all" {
		out["kept"] = []string{"merchants", "plugins", "stand-ins"}
		return out
	}
	// The call's environment's registry; every environment's for "*".
	regs := []*console.Registry{}
	if env := scopeOf(ctx); env == tenant.All {
		regs = append(regs, a.reg)
		if a.db != nil {
			list, _ := a.db.List(ctx)
			for _, e := range list {
				if reg, err := a.registry(tenant.With(context.WithoutCancel(ctx), e.ID)); err == nil {
					regs = append(regs, reg)
				}
			}
		}
	} else if reg, err := a.registry(ctx); err == nil {
		regs = append(regs, reg)
	} else {
		out["status"], out["error"] = 500, err.Error()
		return out
	}
	var merchants, batches int
	var err error
	for _, reg := range regs {
		m, b, e := reg.Clear()
		merchants, batches = merchants+m, batches+b
		if e != nil {
			err = e
		}
	}
	if err != nil {
		out["status"], out["error"] = 500, err.Error()
	}
	out["cleared"] = map[string]int{"merchants": merchants, "batches": batches}
	out["kept"] = []string{"plugins", "stand-ins"}
	return out
}

// --- logs ---------------------------------------------------------------

// logLine is one line in GET /v1/logs: a service's line, named.
type logLine struct {
	Service string    `json:"service"`
	Seq     int64     `json:"seq"`
	At      time.Time `json:"at"`
	Text    string    `json:"text"`
}

type logPage struct {
	Lines []struct {
		Seq  int64     `json:"seq"`
		At   time.Time `json:"at"`
		Text string    `json:"text"`
	} `json:"lines"`
	Next    int64 `json:"next"`
	Dropped int64 `json:"dropped"`
}

// labLogs is GET /v1/logs?services=&q=&since=&limit=: every service's
// GET /sim/logs, merged by time. The cursor (since, next) is opaque: it
// holds one seq per service, base64 of {"bank": 12, …}.
func (a *app) labLogs(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	limit := 200
	if v := qs.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			httputilx.Error(w, 400, "limit is 1 to 1000")
			return
		}
		limit = n
	}
	ids, err := a.logServices(qs.Get("services"))
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	cursor := map[string]int64{}
	hasCursor := qs.Get("since") != ""
	if hasCursor {
		raw, err := base64.RawURLEncoding.DecodeString(qs.Get("since"))
		if err == nil {
			err = json.Unmarshal(raw, &cursor)
		}
		if err != nil {
			httputilx.Error(w, 400, "since is a previous answer's next, as it was given")
			return
		}
	}

	type fetched struct {
		id   string
		page logPage
		err  error
	}
	results := make([]fetched, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			v := url.Values{"limit": {strconv.Itoa(limit)}}
			if q := qs.Get("q"); q != "" {
				v.Set("q", q)
			}
			if hasCursor {
				// A service the cursor does not name yet is read from its
				// first line: {} is "from the start".
				v.Set("since", strconv.FormatInt(cursor[id], 10))
			}
			p, err := a.fetchLogs(r.Context(), id, v)
			results[i] = fetched{id, p, err}
		}(i, id)
	}
	wg.Wait()

	var lines []logLine
	unreachable := []map[string]string{}
	dropped := map[string]int64{}
	next := map[string]int64{}
	for k, v := range cursor {
		next[k] = v // a service that does not answer keeps its place
	}
	for _, f := range results {
		if f.err != nil {
			unreachable = append(unreachable, map[string]string{"service": f.id, "error": f.err.Error()})
			continue
		}
		dropped[f.id] = f.page.Dropped
		for _, l := range f.page.Lines {
			lines = append(lines, logLine{Service: f.id, Seq: l.Seq, At: l.At, Text: l.Text})
		}
	}
	sort.SliceStable(lines, func(i, j int) bool {
		if !lines[i].At.Equal(lines[j].At) {
			return lines[i].At.Before(lines[j].At)
		}
		if lines[i].Service != lines[j].Service {
			return lines[i].Service < lines[j].Service
		}
		return lines[i].Seq < lines[j].Seq
	})

	if !hasCursor {
		// The newest limit lines across the lab; the next read starts after
		// everything each service holds now.
		if len(lines) > limit {
			lines = lines[len(lines)-limit:]
		}
		for _, f := range results {
			if f.err == nil {
				next[f.id] = f.page.Next
			}
		}
	} else {
		// The oldest limit lines after the cursor. A service whose lines
		// all made it moves on to its own next; one cut short moves only
		// to its last line shown, so the next read picks up the rest.
		cut := len(lines) > limit
		if cut {
			lines = lines[:limit]
		}
		shown := map[string]int64{}
		for _, l := range lines {
			shown[l.Service] = l.Seq
		}
		for _, f := range results {
			if f.err != nil {
				continue
			}
			allShown := !cut || len(f.page.Lines) == 0 || shown[f.id] == f.page.Lines[len(f.page.Lines)-1].Seq
			switch {
			case allShown:
				next[f.id] = f.page.Next
			case shown[f.id] > 0:
				next[f.id] = shown[f.id]
			}
		}
	}
	if lines == nil {
		lines = []logLine{}
	}
	raw, _ := json.Marshal(next)
	httputilx.WriteJSON(w, 200, map[string]any{
		"lines":       lines,
		"next":        base64.RawURLEncoding.EncodeToString(raw),
		"dropped":     dropped,
		"unreachable": unreachable,
	})
}

// logServices is the services= list, or every lab service in catalogue
// order.
func (a *app) logServices(list string) ([]string, error) {
	var all []string
	for _, s := range a.cat.Services {
		all = append(all, s.ID)
	}
	if strings.TrimSpace(list) == "" {
		return all, nil
	}
	var out []string
	for _, id := range strings.Split(list, ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if !slices.Contains(all, id) {
			return nil, fmt.Errorf("%q is not a lab service; one of %s", id, strings.Join(all, ", "))
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

func (a *app) fetchLogs(ctx context.Context, id string, v url.Values) (logPage, error) {
	var p logPage
	svc, ok := a.labService(id)
	if !ok {
		return p, fmt.Errorf("not in the catalogue")
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, svc.Control+"/sim/logs?"+v.Encode(), nil)
	if err != nil {
		return p, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return p, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return p, fmt.Errorf("no GET /sim/logs: a build from before the control contract?")
	}
	if resp.StatusCode != 200 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return p, fmt.Errorf("HTTP %d %s", resp.StatusCode, e.Error)
	}
	return p, json.NewDecoder(resp.Body).Decode(&p)
}
