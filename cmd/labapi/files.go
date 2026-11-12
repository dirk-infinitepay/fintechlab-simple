package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labdb"
	"github.com/webduvet/fintechlab-simple/internal/money"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
	"github.com/webduvet/fintechlab-simple/internal/worldline"
)

// The environment's platform card (docs/lab-api/plan.md, phase 6): its
// settlement files, kept by the lab per environment on its data volume;
// publishing them at Worldline, in the environment's SFTP folder, as the
// morning's file; and the platform's own poll, which the card can call
// instead of waiting for the next window.
//
//	<data>/files/<env>/generated/  cut from the environment's trading
//	<data>/files/<env>/uploaded/   brought from anywhere

const (
	// sourceGenerated files the platform's seeder made from its seeded
	// merchants (POST /v1/settlement-files/generate).
	sourceGenerated = "generated"
	// sourceCut files Worldline cut from the lab's own trading
	// (POST /v1/settlement-files/cut).
	sourceCut      = "cut"
	sourceUploaded = "uploaded"
)

// sources are the folders a file can be in, in listing order.
var sources = []string{sourceGenerated, sourceCut, sourceUploaded}

var fileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)

// settlementFile is one file as the card lists it.
type settlementFile struct {
	Name     string    `json:"name"`
	Source   string    `json:"source"`
	Bytes    int64     `json:"bytes"`
	Modified time.Time `json:"modified"`
	Currency string    `json:"currency,omitempty"`
	MIDs     []string  `json:"mids,omitempty"`
	Total    string    `json:"total,omitempty"`
	Items    int       `json:"items,omitempty"`
	// Problem is why Worldline's parser refuses it: a file with one cannot
	// be published.
	Problem string `json:"problem,omitempty"`
}

// filesDir is the call's environment's folder for source.
func (a *app) filesDir(ctx context.Context, source string) string {
	return filepath.Join(a.dataDir, "files", scopeOf(ctx), source)
}

// removeEnvFiles takes an environment's settlement files off the lab's
// volume, every source at once, and says how many there were. A deleted
// environment's files go with it, and an environment created under an id
// one had before starts without them: they pay MIDs of a platform that is
// no longer there. The default environment is never one of these.
func (a *app) removeEnvFiles(id string) (int, error) {
	if tenant.Valid(id) != nil {
		return 0, nil
	}
	dir := filepath.Join(a.dataDir, "files", id)
	n := 0
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			n++
		}
		return nil
	})
	return n, os.RemoveAll(dir)
}

// findFile is where name is, in either folder.
func (a *app) findFile(ctx context.Context, name string) (string, string, bool) {
	for _, src := range sources {
		p := filepath.Join(a.filesDir(ctx, src), name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p, src, true
		}
	}
	return "", "", false
}

func validName(w http.ResponseWriter, name string) bool {
	if !fileName.MatchString(name) || strings.Contains(name, "..") {
		httputilx.Error(w, 400, "a file name is letters, digits, dot, dash and underscore, at most 200, not starting with a dot")
		return false
	}
	return true
}

// describe reads a file with Worldline's own parser.
func describe(path, source string, st os.FileInfo) settlementFile {
	f := settlementFile{Name: st.Name(), Source: source, Bytes: st.Size(), Modified: st.ModTime().UTC()}
	r, err := os.Open(path)
	if err != nil {
		f.Problem = err.Error()
		return f
	}
	defer r.Close()
	// Worldline's own check, read row by row: a file may be millions of
	// transactions, and only its summary is kept.
	sum, err := worldline.SummarizeSettlementFile(r)
	if err != nil {
		f.Problem = "not a Worldline settlement file: " + err.Error()
		return f
	}
	f.Currency = sum.Meta.SettlementCurrency
	f.Total = money.Format(sum.Meta.SettlementAmount)
	f.Items = sum.Meta.NumberOfItems
	f.MIDs = sum.MIDs
	if f.Currency == "" {
		// The parser is lenient: text that is not a settlement file parses
		// to nothing, and nothing names no currency.
		f.Problem = "not a Worldline settlement file: it names no settlement currency"
		f.Total, f.Items, f.MIDs = "", 0, nil
	}
	return f
}

func (a *app) listSettlementFiles(w http.ResponseWriter, r *http.Request) {
	out := []settlementFile{}
	for _, src := range sources {
		dir := a.filesDir(r.Context(), src)
		entries, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			httputilx.Error(w, 500, err.Error())
			return
		}
		for _, e := range entries {
			st, err := e.Info()
			if err != nil || !st.Mode().IsRegular() {
				continue
			}
			out = append(out, describe(filepath.Join(dir, e.Name()), src, st))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	httputilx.WriteJSON(w, 200, map[string]any{"env": scopeOf(r.Context()), "files": out})
}

func (a *app) uploadSettlementFile(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if !validName(w, name) {
		return
	}
	if _, src, ok := a.findFile(r.Context(), name); ok && src != sourceUploaded {
		httputilx.Error(w, 409, name+" is a "+src+" file of this environment's; upload it under another name")
		return
	}
	dir := a.filesDir(r.Context(), sourceUploaded)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, http.MaxBytesReader(w, r.Body, uploadLimit()))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		httputilx.Error(w, 413, fmt.Sprintf("larger than CONSOLE_UPLOAD_MAX_MB (%d MB)", uploadLimit()>>20))
		return
	case err != nil:
		httputilx.Error(w, 400, err.Error())
		return
	case n == 0:
		httputilx.Error(w, 400, "the file is empty")
		return
	}
	path := filepath.Join(dir, name)
	if err := os.Rename(tmp.Name(), path); err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	st, _ := os.Stat(path)
	f := describe(path, sourceUploaded, st)
	note := "kept as " + name
	if f.Problem != "" {
		note += "; " + f.Problem
	}
	httputilx.WriteJSON(w, 201, map[string]any{"file": f, "note": note})
}

// cutSettlementFile keeps the file Worldline would cut from the
// environment's trading in the lab: for a platform with no seeder of its
// own, or a file whose MIDs the platform need not know.
func (a *app) cutSettlementFile(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Currency string `json:"currency"`
		Date     string `json:"date"`
		Slot     string `json:"slot"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
		httputilx.Error(w, 400, `want {"currency": "EUR", "date": "YYYY-MM-DD", "slot": "ER"}: `+err.Error())
		return
	}
	if b.Currency == "" {
		httputilx.Error(w, 400, "currency is required")
		return
	}
	var rendered struct {
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	ctx, cancel := reqContext(r)
	defer cancel()
	status, msg, err := a.controlJSON(ctx, http.MethodPost, "worldline", "/sim/settlement-files/render", b, &rendered)
	switch {
	case err != nil:
		httputilx.Error(w, 502, "worldline: "+err.Error())
		return
	case status != 200:
		httputilx.Error(w, status, msg)
		return
	}
	if !fileName.MatchString(rendered.Name) {
		httputilx.Error(w, 502, "worldline named the file "+strconv.Quote(rendered.Name))
		return
	}
	dir := a.filesDir(r.Context(), sourceCut)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	path := filepath.Join(dir, rendered.Name)
	if err := os.WriteFile(path, []byte(rendered.Content), 0o644); err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	st, _ := os.Stat(path)
	httputilx.WriteJSON(w, 201, map[string]any{"file": describe(path, sourceCut, st)})
}

func (a *app) previewSettlementFile(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if !validName(w, name) {
		return
	}
	path, _, ok := a.findFile(r.Context(), name)
	if !ok {
		httputilx.Error(w, 404, "no file "+name+" in this environment")
		return
	}
	lines := 100
	if v, err := strconv.Atoi(r.URL.Query().Get("lines")); err == nil {
		lines = min(max(v, 1), 1000)
	}
	f, err := os.Open(path)
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	out, long := []string{}, false
	for len(out) < lines && sc.Scan() {
		line := sc.Text()
		if len(line) > 2000 {
			line, long = line[:2000], true
		}
		out = append(out, line)
	}
	httputilx.WriteJSON(w, 200, map[string]any{"name": name, "lines": out, "truncated": sc.Scan(), "long_lines": long, "bytes": st.Size()})
}

// downloadSettlementFile hands one of the environment's files over whole,
// as a download named as it is listed.
func (a *app) downloadSettlementFile(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if !validName(w, name) {
		return
	}
	path, _, ok := a.findFile(r.Context(), name)
	if !ok {
		httputilx.Error(w, 404, "no file "+name+" in this environment")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func (a *app) deleteSettlementFile(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if !validName(w, name) {
		return
	}
	path, _, ok := a.findFile(r.Context(), name)
	if !ok {
		httputilx.Error(w, 404, "no file "+name+" in this environment")
		return
	}
	if err := os.Remove(path); err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

// publishSettlementFiles has Worldline publish the files the card ticked
// in the environment's SFTP folder: at most one per currency, since a
// platform settles two currencies at once but never two files of one.
func (a *app) publishSettlementFiles(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Files []string `json:"files"`
		Wire  *bool    `json:"wire"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&b); err != nil {
		httputilx.Error(w, 400, `want {"files": ["…"], "wire": true}: `+err.Error())
		return
	}
	if len(b.Files) == 0 {
		httputilx.Error(w, 400, "files names at least one file")
		return
	}
	wire := b.Wire == nil || *b.Wire
	type pick struct {
		name, path string
		file       settlementFile
	}
	var picks []pick
	byCurrency := map[string]string{}
	for _, name := range b.Files {
		if !validName(w, name) {
			return
		}
		path, src, ok := a.findFile(r.Context(), name)
		if !ok {
			httputilx.Error(w, 404, "no file "+name+" in this environment")
			return
		}
		st, _ := os.Stat(path)
		f := describe(path, src, st)
		if f.Problem != "" {
			httputilx.Error(w, 400, name+": "+f.Problem)
			return
		}
		if other, dup := byCurrency[f.Currency]; dup {
			httputilx.Error(w, 409, fmt.Sprintf("%s and %s are both %s: a platform settles one file per currency at a time", other, name, f.Currency))
			return
		}
		byCurrency[f.Currency] = name
		picks = append(picks, pick{name, path, f})
	}
	results := make([]map[string]any, 0, len(picks))
	ok := 0
	for _, p := range picks {
		body, err := os.ReadFile(p.path)
		if err != nil {
			results = append(results, map[string]any{"name": p.name, "error": err.Error()})
			continue
		}
		q := url.Values{"name": {p.name}, "wire": {strconv.FormatBool(wire)}}
		ctx, cancel := reqContext(r)
		var out map[string]any
		status, msg, err := a.controlRaw(ctx, "worldline", "/sim/settlement-files/publish?"+q.Encode(), body, &out)
		cancel()
		switch {
		case err != nil:
			results = append(results, map[string]any{"name": p.name, "error": "worldline: " + err.Error()})
		case status >= 300:
			results = append(results, map[string]any{"name": p.name, "error": fmt.Sprintf("worldline: HTTP %d: %s", status, msg)})
		default:
			ok++
			// Worldline names the file it published (a Worldline-named file
			// gets the moment's timestamp); file is the lab's copy.
			out["file"] = p.name
			results = append(results, out)
		}
	}
	status := 200
	if ok == 0 {
		status = 502
	}
	httputilx.WriteJSON(w, status, map[string]any{
		"env": scopeOf(r.Context()), "published": ok, "results": results,
		"note": fmt.Sprintf("%d of %d published in the environment's SFTP folder; the platform's next poll picks them up, or press Poll now", ok, len(picks)),
	})
}

// pollClient is the one for a platform's poll, which downloads files
// before it answers.
var pollClient = &http.Client{Timeout: 90 * time.Second}

// platformPoll calls the poll the environment's config names: the
// platform's own Worldline poll, run now instead of at its next window.
func (a *app) platformPoll(w http.ResponseWriter, r *http.Request) {
	var b struct {
		FileType string `json:"fileType"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
		httputilx.Error(w, 400, `want {"fileType": "ER"|"AR"}: `+err.Error())
		return
	}
	switch b.FileType = strings.ToUpper(b.FileType); b.FileType {
	case "ER", "AR":
	default:
		httputilx.Error(w, 400, `fileType is "ER" (the morning file) or "AR" (the afternoon's)`)
		return
	}
	cfg, err := a.envConfig(r.Context())
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	if cfg.PollURL == "" {
		httputilx.Error(w, 409, "this environment's config names no poll_url: PATCH /v1/environments/"+scopeOf(r.Context())+` {"config": {"poll_url": "…", "poll_token": "…"}}`)
		return
	}
	body, _ := json.Marshal(b)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, cfg.PollURL, bytes.NewReader(body))
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.PollToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.PollToken)
	}
	resp, err := pollClient.Do(req)
	if err != nil {
		httputilx.Error(w, 502, "the platform's poll at "+cfg.PollURL+": "+err.Error())
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 1<<20))
}

// envConfig is the call's environment's config, the default's included.
func (a *app) envConfig(ctx context.Context) (labdb.Config, error) {
	if a.db == nil {
		return labdb.Config{}, nil
	}
	env := scopeOf(ctx)
	if env == tenant.Default {
		return a.db.DefaultConfig(ctx)
	}
	e, err := a.db.Get(ctx, env)
	return e.Config, err
}

// controlJSON makes one JSON call to a lab service's control port in the
// call's environment (ctx names it) and decodes a 2xx answer into out; any
// other answer comes back as its status and its error message.
func (a *app) controlJSON(ctx context.Context, method, svc, path string, in, out any) (int, string, error) {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return 0, "", err
		}
	}
	return a.controlCall(ctx, method, svc, path, "application/json", body, out)
}

// controlRaw POSTs raw bytes.
func (a *app) controlRaw(ctx context.Context, svc, path string, body []byte, out any) (int, string, error) {
	return a.controlCall(ctx, http.MethodPost, svc, path, "application/octet-stream", body, out)
}

func (a *app) controlCall(ctx context.Context, method, svc, path, ctype string, body []byte, out any) (int, string, error) {
	base := a.controlURL(svc)
	if base == "" {
		return 0, "", fmt.Errorf("%s has no control port", svc)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	if body != nil {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return resp.StatusCode, e.Error, nil
		}
		return resp.StatusCode, strings.TrimSpace(string(raw)), nil
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, "", err
		}
	}
	return resp.StatusCode, "", nil
}
