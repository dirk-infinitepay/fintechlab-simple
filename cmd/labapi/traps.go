package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
)

// Traps across the lab (docs/lab-api/lab-api.md §6). A trap belongs to the
// sim that applies it, so it is armed there, through the passthrough
// (POST /v1/services/{id}/sim/traps); these routes read and disarm every
// sim's at once. A sim with no hooks answers 404 to /sim/traps and is left
// out, not reported as a failure.

// simAnswer is one service's answer to a fan-out call.
type simAnswer struct {
	id     string
	status int
	body   map[string]any
	err    error
}

// fanOut calls method path on every lab service's control port at once.
func (a *app) fanOut(ctx context.Context, method, path string) []simAnswer {
	var ids []string
	for _, s := range a.cat.Services {
		// In an environment other than the default, only the services
		// that keep environments apart answer for it.
		if _, ok := a.labService(s.ID); ok && a.inScope(ctx, s.ID) {
			ids = append(ids, s.ID)
		}
	}
	out := make([]simAnswer, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			svc, _ := a.labService(id)
			ctx, cancel := context.WithTimeout(ctx, callTimeout)
			defer cancel()
			ans := simAnswer{id: id}
			req, err := http.NewRequestWithContext(ctx, method, svc.Control+path, nil)
			if err == nil {
				var resp *http.Response
				resp, err = a.client.Do(req)
				if err == nil {
					ans.status = resp.StatusCode
					_ = json.NewDecoder(resp.Body).Decode(&ans.body)
					resp.Body.Close()
				}
			}
			ans.err = err
			out[i] = ans
		}(i, id)
	}
	wg.Wait()
	return out
}

// noHooks is a sim that has no traps to answer with: it serves no
// /sim/traps at all.
func (s simAnswer) noHooks() bool { return s.err == nil && s.status == http.StatusNotFound }

func (s simAnswer) failed() string {
	switch {
	case s.err != nil:
		return s.err.Error()
	case s.status < 200 || s.status > 299:
		if e, _ := s.body["error"].(string); e != "" {
			return fmt.Sprintf("HTTP %d: %s", s.status, e)
		}
		return fmt.Sprintf("HTTP %d", s.status)
	}
	return ""
}

// listTraps is GET /v1/traps?state=: every sim's traps, each with its
// service, newest first.
func (a *app) listTraps(w http.ResponseWriter, r *http.Request) {
	path := "/sim/traps"
	if st := r.URL.Query().Get("state"); st != "" {
		path += "?state=" + url.QueryEscape(st)
	}
	var traps []map[string]any
	unreachable := []map[string]string{}
	for _, ans := range a.fanOut(r.Context(), http.MethodGet, path) {
		if ans.noHooks() {
			continue
		}
		if why := ans.failed(); why != "" {
			if ans.status == http.StatusBadRequest {
				httputilx.Error(w, 400, why) // a bad state filter, said by the sim
				return
			}
			unreachable = append(unreachable, map[string]string{"service": ans.id, "error": why})
			continue
		}
		list, _ := ans.body["traps"].([]any)
		for _, x := range list {
			if t, ok := x.(map[string]any); ok {
				t["service"] = ans.id
				traps = append(traps, t)
			}
		}
	}
	sort.SliceStable(traps, func(i, j int) bool { return createdAt(traps[i]).After(createdAt(traps[j])) })
	if traps == nil {
		traps = []map[string]any{}
	}
	httputilx.WriteJSON(w, 200, map[string]any{"traps": traps, "unreachable": unreachable})
}

func createdAt(t map[string]any) time.Time {
	s, _ := t["created_at"].(string)
	at, _ := time.Parse(time.RFC3339Nano, s)
	return at
}

// trapHooks is GET /v1/traps/hooks: every sim's hook catalogue, each hook
// with its service — what an agent reads before arming anything.
func (a *app) trapHooks(w http.ResponseWriter, r *http.Request) {
	hooks := []map[string]any{}
	unreachable := []map[string]string{}
	for _, ans := range a.fanOut(r.Context(), http.MethodGet, "/sim/traps/hooks") {
		if ans.noHooks() {
			continue
		}
		if why := ans.failed(); why != "" {
			unreachable = append(unreachable, map[string]string{"service": ans.id, "error": why})
			continue
		}
		list, _ := ans.body["hooks"].([]any)
		for _, x := range list {
			if h, ok := x.(map[string]any); ok {
				h["service"] = ans.id
				hooks = append(hooks, h)
			}
		}
	}
	httputilx.WriteJSON(w, 200, map[string]any{"hooks": hooks, "unreachable": unreachable})
}

// disarmTraps is DELETE /v1/traps: every sim's traps disarmed, their
// pending steps cancelled. A sim that could not be reached is reported,
// and the answer is 502, as a partial lab reset is.
func (a *app) disarmTraps(w http.ResponseWriter, r *http.Request) {
	results := []map[string]any{}
	ok := true
	for _, ans := range a.fanOut(r.Context(), http.MethodDelete, "/sim/traps") {
		if ans.noHooks() {
			continue
		}
		res := map[string]any{"service": ans.id, "status": ans.status}
		if why := ans.failed(); why != "" {
			ok = false
			res["error"] = why
		} else {
			res["disarmed"] = ans.body["disarmed"]
			res["cancelled"] = ans.body["cancelled"]
		}
		results = append(results, res)
	}
	status := 200
	if !ok {
		status = 502
	}
	httputilx.WriteJSON(w, status, map[string]any{"ok": ok, "services": results})
}
