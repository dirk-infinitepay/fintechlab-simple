package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/console"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
)

// Plugins: the platform under test registers its own card (docs/plugins.md).
//
// Everything here is generic. The console forwards a button to the path the
// plugin declared for it and to nothing else, so a plugin is one POST
// away from a card with working buttons, and this file never learns a
// platform's name.

// renewEvery is what a plugin is told to renew at: a third of the TTL, so a
// single lost renewal is not a lapse.
const renewEvery = console.PluginTTL / 3

func (a *app) registerPlugin(w http.ResponseWriter, r *http.Request) {
	var p console.Plugin
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&p); err != nil {
		httputilx.Error(w, 400, "descriptor: "+err.Error())
		return
	}
	info, err := a.cat.Register(p)
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	a.applyPluginStandIns(p)
	// Probe at once rather than on the next tick, so the card a developer
	// is watching turns green when the platform says it is there.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.mon.Probe(ctx, p.ID)
	}()
	httputilx.WriteJSON(w, 200, map[string]any{
		"id":            p.ID,
		"state":         info.State,
		"ttl_seconds":   info.TTLSeconds,
		"renew_seconds": int(renewEvery / time.Second),
		"clock":         info.Clock,
	})
}

func (a *app) unregisterPlugin(w http.ResponseWriter, r *http.Request) {
	if !a.cat.Unregister(r.PathValue("id")) {
		httputilx.Error(w, 404, "no plugin "+r.PathValue("id"))
		return
	}
	w.WriteHeader(204)
}

func (a *app) listPlugins(w http.ResponseWriter, r *http.Request) {
	httputilx.WriteJSON(w, 200, map[string]any{"plugins": a.cat.Plugins()})
}

// livePlugin finds a plugin a button may be forwarded to. A stopped one
// has said it is not there, so the answer is that, not a connection error.
func (a *app) livePlugin(w http.ResponseWriter, id string) (console.Plugin, bool) {
	p, info, ok := a.cat.Plugin(id)
	if !ok {
		httputilx.Error(w, 404, "no plugin "+id)
		return p, false
	}
	if info.State == console.PluginStopped {
		httputilx.Error(w, 409, p.Name+" has stopped; start it again first")
		return p, false
	}
	return p, true
}

// pluginAction runs one declared button: the method and path the plugin
// registered, with the request's body as it came.
func (a *app) pluginAction(w http.ResponseWriter, r *http.Request) {
	p, ok := a.livePlugin(w, r.PathValue("id"))
	if !ok {
		return
	}
	for _, act := range p.Actions {
		if act.ID == r.PathValue("action") {
			a.forward(w, r, act.Method, p.BaseURL+act.Path)
			return
		}
	}
	httputilx.Error(w, 404, p.Name+" declares no action "+r.PathValue("action"))
}

func (a *app) pluginFiles(w http.ResponseWriter, r *http.Request) {
	p, ok := a.livePlugin(w, r.PathValue("id"))
	if !ok {
		return
	}
	if p.Settlement == nil {
		httputilx.Error(w, 404, p.Name+" runs no settlement files")
		return
	}
	ctx, cancel := reqContext(r)
	defer cancel()
	var out any
	if err := a.getJSON(ctx, a.client, p.BaseURL+p.Settlement.FilesPath, &out); err != nil {
		httputilx.Error(w, 502, err.Error())
		return
	}
	httputilx.WriteJSON(w, 200, out)
}

// pluginRun starts settlement runs — {"files": […]}, forwarded verbatim.
// Several files are still one call: the platform starts them together,
// which is the point of running them together.
func (a *app) pluginRun(w http.ResponseWriter, r *http.Request) {
	p, ok := a.livePlugin(w, r.PathValue("id"))
	if !ok {
		return
	}
	if p.Settlement == nil {
		httputilx.Error(w, 404, p.Name+" runs no settlement files")
		return
	}
	a.forward(w, r, http.MethodPost, p.BaseURL+p.Settlement.RunPath)
}

// forward sends the request's JSON body to url with method and answers with
// whatever url answered, status and all — the peer's refusal in its own
// words.
func (a *app) forward(w http.ResponseWriter, r *http.Request, method, url string) {
	ctx, cancel := reqContext(r)
	defer cancel()
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<10))
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		httputilx.Error(w, 502, err.Error())
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out any
	if len(bytes.TrimSpace(raw)) == 0 {
		out = map[string]any{}
	} else if err := json.Unmarshal(raw, &out); err != nil {
		out = map[string]any{"output": string(raw)}
	}
	httputilx.WriteJSON(w, resp.StatusCode, out)
}

// clockLock says why the lab clock must not move, or "": a live platform
// that cannot follow it would be left on a different day from the vendors
// it talks to.
func (a *app) clockLock() string {
	if name, ok := a.cat.WallClockPlugin(); ok {
		return name + " is registered on the wall clock and cannot follow the lab's; moving the lab would put its vendors on a different day from the platform they serve"
	}
	return ""
}

// --- settlement files: upload, preview, delete ---------------------------
//
// A file from anywhere — not only the fixtures the platform ships — goes to
// the platform, which keeps it where it keeps its own (buddy's runner:
// .runs/uploads/) and lists it with the rest. The console only passes the
// bytes through: it streams them, so a million-line file is never held in
// the console's memory, and it stores nothing.

// uploadLimit is the largest file the console passes on,
// CONSOLE_UPLOAD_MAX_MB (default 512).
func uploadLimit() int64 {
	mb, err := strconv.ParseInt(env("CONSOLE_UPLOAD_MAX_MB", "512"), 10, 64)
	if err != nil || mb <= 0 {
		mb = 512
	}
	return mb << 20
}

// settlementPath is one of the settlement paths a live plugin declared, or
// an answer saying why there is none: 501 when it does not offer the
// feature, which the page reads as "no button" rather than "failed".
func (a *app) settlementPath(w http.ResponseWriter, r *http.Request, pick func(*console.PluginSettlement) string, what string) (console.Plugin, string, bool) {
	p, ok := a.livePlugin(w, r.PathValue("id"))
	if !ok {
		return p, "", false
	}
	if p.Settlement == nil {
		httputilx.Error(w, 404, p.Name+" runs no settlement files")
		return p, "", false
	}
	path := pick(p.Settlement)
	if path == "" {
		httputilx.Error(w, 501, p.Name+" does not offer "+what)
		return p, "", false
	}
	return p, path, true
}

func withQuery(base string, q url.Values) string {
	if strings.Contains(base, "?") {
		return base + "&" + q.Encode()
	}
	return base + "?" + q.Encode()
}

// pluginUpload streams POST /api/plugins/{id}/files?name= to the plugin's
// upload_path, bytes untouched.
func (a *app) pluginUpload(w http.ResponseWriter, r *http.Request) {
	p, path, ok := a.settlementPath(w, r, func(s *console.PluginSettlement) string { return s.UploadPath }, "uploads")
	if !ok {
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		httputilx.Error(w, 400, "name the file: ?name=<file name>")
		return
	}
	limit := uploadLimit()
	if r.ContentLength > limit {
		httputilx.Error(w, 413, fmt.Sprintf("%d bytes is over the console's %d MB limit (CONSOLE_UPLOAD_MAX_MB)", r.ContentLength, limit>>20))
		return
	}
	// The request's own context, not the usual five seconds: a big file
	// takes as long as it takes, and the operator can cancel by leaving.
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		withQuery(p.BaseURL+path, url.Values{"name": {name}}), http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	req.ContentLength = r.ContentLength
	req.Header.Set("Content-Type", "application/octet-stream")
	client := &http.Client{Transport: a.client.Transport}
	a.relay(w, client, req)
}

// pluginDeleteFile deletes a file: DELETE delete_path?name= when the plugin
// declares one (any file it marked deletable), otherwise DELETE
// upload_path?name= (only the files it was handed).
func (a *app) pluginDeleteFile(w http.ResponseWriter, r *http.Request) {
	p, path, ok := a.settlementPath(w, r, func(s *console.PluginSettlement) string {
		if s.DeletePath != "" {
			return s.DeletePath
		}
		return s.UploadPath
	}, "file deletion")
	if !ok {
		return
	}
	ctx, cancel := reqContext(r)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		withQuery(p.BaseURL+path, url.Values{"name": {r.URL.Query().Get("name")}}), nil)
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	a.relay(w, a.client, req)
}

// pluginReveal asks the platform to open the folder a file is in, in the
// file manager of the machine it runs on: POST reveal_path {"name"}. Only
// the platform can — the console is in a container, and a browser may not
// open a local folder from a web page — so it is forwarded, body and answer
// untouched.
func (a *app) pluginReveal(w http.ResponseWriter, r *http.Request) {
	p, path, ok := a.settlementPath(w, r, func(s *console.PluginSettlement) string { return s.RevealPath }, "opening a file's location")
	if !ok {
		return
	}
	a.forward(w, r, http.MethodPost, p.BaseURL+path)
}

// maxPreviewLines is the most a preview asks for: enough to read a file's
// shape, and a bounded answer however long the file is.
const maxPreviewLines = 1000

// pluginPreview forwards GET …/files/preview?name=&lines= to the plugin's
// preview_path, clamping lines to 1..1000.
func (a *app) pluginPreview(w http.ResponseWriter, r *http.Request) {
	p, path, ok := a.settlementPath(w, r, func(s *console.PluginSettlement) string { return s.PreviewPath }, "previews")
	if !ok {
		return
	}
	lines, err := strconv.Atoi(r.URL.Query().Get("lines"))
	if err != nil || lines <= 0 {
		lines = 100
	}
	if lines > maxPreviewLines {
		lines = maxPreviewLines
	}
	ctx, cancel := reqContext(r)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, withQuery(p.BaseURL+path, url.Values{
		"name": {r.URL.Query().Get("name")}, "lines": {strconv.Itoa(lines)},
	}), nil)
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	a.relay(w, a.client, req)
}

// relay sends req and answers with the peer's status and JSON body — its
// refusal in its own words.
func (a *app) relay(w http.ResponseWriter, client *http.Client, req *http.Request) {
	resp, err := client.Do(req)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httputilx.Error(w, 413, fmt.Sprintf("over the console's %d MB limit (CONSOLE_UPLOAD_MAX_MB)", tooBig.Limit>>20))
			return
		}
		httputilx.Error(w, 502, err.Error())
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var out any
	if len(bytes.TrimSpace(raw)) == 0 {
		out = map[string]any{}
	} else if err := json.Unmarshal(raw, &out); err != nil {
		out = map[string]any{"output": string(raw)}
	}
	httputilx.WriteJSON(w, resp.StatusCode, out)
}
