package main

import (
	"errors"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/console"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
)

// Connect your platform: the .env and key downloads on the vendor cards.
//
// Downloads are plain GETs with Content-Disposition, so the button in the UI
// and `curl -OJ` save the same file under the same name.

// kitHost is the address the platform should use to reach the lab. Asked
// for explicitly (?host=) it is that; otherwise it is the host this request
// came in on -- a developer who opened the console at lab.example.test:8090
// reaches the vendors at lab.example.test too, which is the one case a
// fixed default would get wrong.
var hostPattern = regexp.MustCompile(`^[A-Za-z0-9.:-]+$`)

func kitHost(r *http.Request) (string, error) {
	if h := r.URL.Query().Get("host"); h != "" {
		if !hostPattern.MatchString(h) {
			return "", errors.New("host must be a hostname or an IP address")
		}
		return h, nil
	}
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	host = strings.Trim(host, "[]")
	if host == "" || !hostPattern.MatchString(host) {
		return "127.0.0.1", nil
	}
	return host, nil
}

// connect lists the kits: which variables each sets and which files it can
// hand out, and whether those exist yet. No values -- the page needs names,
// and a JSON listing is not where key material should be copied from.
func (a *app) connect(w http.ResponseWriter, r *http.Request) {
	host, err := kitHost(r)
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	httputilx.WriteJSON(w, 200, map[string]any{
		"host": host,
		"kits": console.Kits(a.cat, host, a.keysDir),
	})
}

func (a *app) connectEnv(w http.ResponseWriter, r *http.Request) {
	host, err := kitHost(r)
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	kits := console.Kits(a.cat, host, a.keysDir)
	name := "fintechlab.env"
	if id := r.PathValue("service"); id != "" {
		k, ok := console.KitFor(a.cat, host, a.keysDir, id)
		if !ok {
			httputilx.Error(w, 404, id+" has nothing a platform needs to connect")
			return
		}
		kits = []console.Kit{k}
		name = "fintechlab-" + id + ".env"
	}
	download(w, name, "text/plain; charset=utf-8", []byte(console.EnvFile(kits, host, time.Now())))
}

func (a *app) connectFile(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("service"), r.PathValue("name")
	k, ok := console.KitFor(a.cat, "127.0.0.1", a.keysDir, id)
	if !ok {
		httputilx.Error(w, 404, id+" has no keys to hand out")
		return
	}
	path, err := console.KitFilePath(k, a.keysDir, name)
	if err != nil {
		httputilx.Error(w, 404, err.Error())
		return
	}
	body, err := os.ReadFile(path)
	if err != nil {
		// Not generated yet, or the keys directory is not mounted: say
		// which, rather than a bare 404 that reads like a wrong URL.
		httputilx.Error(w, 503, name+" is not available: "+err.Error())
		return
	}
	download(w, name, "application/octet-stream", body)
}

func download(w http.ResponseWriter, name, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	// Key material: never from a cache, never kept by one.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	_, _ = w.Write(body)
}
