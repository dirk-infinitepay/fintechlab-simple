package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
)

// The platform's seeder (docs/lab-api/plan.md, phase 6.1): a container in
// the lab's pod, built from the platform's own repository, that seeds an
// environment's databases with generated merchants and writes settlement
// files that pay them. The lab knows no platform by name: it runs whichever
// seeder its deploy names (LAB_SEEDER_URL), hands it the databases the
// environment's config names, and keeps the files it writes on the lab's
// own volume, which the seeder mounts at the same path.
//
// The seeder's contract:
//
//	GET  /info              {"databases": ["platform", "settle"], …}: the databases it needs
//	POST /seed              {"databases": {…}, "merchants", "mode", "seed"}
//	POST /settlement-files  {"databases": {…}, "transactions", "currency", "seed", "dir"}

// labNow is the lab clock's time, when the lab has a clock.
func (a *app) labNow(ctx context.Context) (time.Time, bool) {
	if _, ok := a.cat.Get("clock"); !ok {
		return time.Time{}, false
	}
	var c struct {
		Now time.Time `json:"now"`
	}
	if err := a.getJSON(ctx, a.client, a.baseURL("clock")+"/clock", &c); err != nil || c.Now.IsZero() {
		return time.Time{}, false
	}
	return c.Now, true
}

// seederClient waits as long as a large population or file takes.
var seederClient = &http.Client{Timeout: 20 * time.Minute}

// seederInfo is the seeder's GET /info, or why it cannot be had.
func (a *app) seederInfo(ctx context.Context) (map[string]any, error) {
	if a.seederURL == "" {
		return nil, fmt.Errorf("this lab runs no seeder: deploy it with one (LAB_SEEDER_URL)")
	}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodGet, a.seederURL+"/info", nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the seeder at %s does not answer: %w", a.seederURL, err)
	}
	defer resp.Body.Close()
	var info map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil || resp.StatusCode != 200 {
		return nil, fmt.Errorf("the seeder's /info: HTTP %d", resp.StatusCode)
	}
	return info, nil
}

// neededDatabases are the names the seeder asks for.
func neededDatabases(info map[string]any) []string {
	var out []string
	if list, ok := info["databases"].([]any); ok {
		for _, v := range list {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// platformState answers GET /v1/platform: what the environment's card can
// do on the platform's side — seed, generate, poll — and what it lacks.
func (a *app) platformState(w http.ResponseWriter, r *http.Request) {
	cfg, err := a.envConfig(r.Context())
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	seeder := map[string]any{"available": false}
	dbs := map[string]string{}
	if info, err := a.seederInfo(r.Context()); err != nil {
		seeder["error"] = err.Error()
	} else {
		seeder["available"] = true
		seeder["info"] = info
		for _, name := range neededDatabases(info) {
			dbs[name] = "missing"
			if cfg.Databases[name] != "" {
				dbs[name] = "set"
			}
		}
	}
	httputilx.WriteJSON(w, 200, map[string]any{
		"env": scopeOf(r.Context()), "seeder": seeder, "databases": dbs, "poll": cfg.PollURL != "",
	})
}

// seederDatabases are the environment's databases for the seeder, or a
// reason it cannot be asked.
func (a *app) seederDatabases(ctx context.Context) (map[string]string, int, string) {
	info, err := a.seederInfo(ctx)
	if err != nil {
		return nil, 503, err.Error()
	}
	cfg, err := a.envConfig(ctx)
	if err != nil {
		return nil, 500, err.Error()
	}
	out := map[string]string{}
	var missing []string
	for _, name := range neededDatabases(info) {
		if cfg.Databases[name] == "" {
			missing = append(missing, name)
			continue
		}
		out[name] = cfg.Databases[name]
	}
	if len(missing) > 0 {
		return nil, 409, fmt.Sprintf("this environment's config names no %s database: PATCH /v1/environments/%s {\"config\": {\"databases\": {…}}}",
			strings.Join(missing, ", "), scopeOf(ctx))
	}
	return out, 0, ""
}

// callSeeder POSTs to the seeder and relays its answer.
func (a *app) callSeeder(w http.ResponseWriter, r *http.Request, path string, body map[string]any) (int, map[string]any, bool) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, a.seederURL+path, bytes.NewReader(b))
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return 0, nil, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := seederClient.Do(req)
	if err != nil {
		httputilx.Error(w, 502, "the seeder: "+err.Error())
		return 0, nil, false
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		httputilx.Error(w, 502, fmt.Sprintf("the seeder answered HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw))))
		return 0, nil, false
	}
	if resp.StatusCode >= 300 {
		msg, _ := out["error"].(string)
		httputilx.Error(w, resp.StatusCode, "the seeder: "+msg)
		return resp.StatusCode, out, false
	}
	return resp.StatusCode, out, true
}

// seedPlatform seeds the environment's databases with generated merchants:
// the runner's Seed merchants, for any environment.
func (a *app) seedPlatform(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Merchants int     `json:"merchants"`
		Mode      string  `json:"mode"`
		Files     string  `json:"files"`
		Seed      *uint32 `json:"seed"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
		httputilx.Error(w, 400, `want {"merchants": 50, "mode": "add"|"reseed"|"remove", "files": "delete"|"keep", "seed": null}: `+err.Error())
		return
	}
	if b.Mode == "" {
		b.Mode = "add"
	}
	if b.Files == "" {
		b.Files = "delete"
	}
	if b.Files != "delete" && b.Files != "keep" {
		httputilx.Error(w, 400, `files is "delete" or "keep"`)
		return
	}
	dbs, status, msg := a.seederDatabases(r.Context())
	if status != 0 {
		httputilx.Error(w, status, msg)
		return
	}
	_, out, ok := a.callSeeder(w, r, "/seed", map[string]any{
		"databases": dbs, "merchants": b.Merchants, "mode": b.Mode, "seed": b.Seed,
	})
	if !ok {
		return
	}
	// The generated files pay MIDs a reseed or a removal took away: gone
	// with them, unless asked to keep them.
	deleted := 0
	if b.Mode != "add" && b.Files == "delete" {
		dir := a.filesDir(r.Context(), sourceGenerated)
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.Type().IsRegular() && os.Remove(filepath.Join(dir, e.Name())) == nil {
				deleted++
			}
		}
	}
	httputilx.WriteJSON(w, 200, map[string]any{"env": scopeOf(r.Context()), "result": out, "files_deleted": deleted})
}

// generateSettlementFile has the seeder write settlement files for the
// environment's seeded merchants straight into its files: the runner's
// Create settlement file, for any environment. A file of millions of
// transactions is streamed to disk by the seeder, never through the lab.
func (a *app) generateSettlementFile(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Transactions int     `json:"transactions"`
		Currency     string  `json:"currency"`
		Seed         *uint32 `json:"seed"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
		httputilx.Error(w, 400, `want {"transactions": 1000, "currency": "EUR"|"GBP"|"", "seed": null}: `+err.Error())
		return
	}
	if b.Transactions <= 0 {
		httputilx.Error(w, 400, "transactions is a whole number from 1")
		return
	}
	dbs, status, msg := a.seederDatabases(r.Context())
	if status != 0 {
		httputilx.Error(w, status, msg)
		return
	}
	dir := a.filesDir(r.Context(), sourceGenerated)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	var currency any
	if b.Currency != "" {
		currency = strings.ToUpper(b.Currency)
	}
	body := map[string]any{
		"databases": dbs, "transactions": b.Transactions, "currency": currency, "seed": b.Seed, "dir": dir,
	}
	// The files are dated by the lab's clock when it has one, as the
	// runner's were by the clock it followed.
	if now, ok := a.labNow(r.Context()); ok {
		body["now"] = now.UTC().Format(time.RFC3339)
	}
	_, out, ok := a.callSeeder(w, r, "/settlement-files", body)
	if !ok {
		return
	}
	// The files the seeder says it wrote, read as every file is.
	files := []settlementFile{}
	if list, ok := out["files"].([]any); ok {
		for _, item := range list {
			f, _ := item.(map[string]any)
			name, _ := f["name"].(string)
			if name == "" {
				p, _ := f["path"].(string)
				name = filepath.Base(p)
			}
			path := filepath.Join(dir, filepath.Base(name))
			if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
				files = append(files, describe(path, sourceGenerated, st))
			}
		}
	}
	httputilx.WriteJSON(w, 201, map[string]any{"env": scopeOf(r.Context()), "result": out, "files": files})
}
