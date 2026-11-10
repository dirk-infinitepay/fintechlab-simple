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
// for explicitly (?host=) it is that. Next, CONSOLE_CONNECT_HOST, when the
// deployment knows its own address: a lab in the cloud is reached by the
// platform at one public name, whatever tunnel the operator opened the
// console through (an SSM port forward is 127.0.0.1). Otherwise it is the
// host the browser used -- a developer who opened the console at
// lab.example.test:8090 reaches the vendors at lab.example.test too. Through
// the console that is X-Forwarded-Host: the console's proxy addresses the
// lab API by its own name (lab-api:8097), which resolves only inside the lab.
var hostPattern = regexp.MustCompile(`^[A-Za-z0-9.:-]+$`)

func kitHost(r *http.Request) (string, error) {
	if h := r.URL.Query().Get("host"); h != "" {
		if !hostPattern.MatchString(h) {
			return "", errors.New("host must be a hostname or an IP address")
		}
		return h, nil
	}
	if h := strings.TrimSpace(os.Getenv("CONSOLE_CONNECT_HOST")); h != "" && hostPattern.MatchString(h) {
		return h, nil
	}
	if fwd, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Host"), ","); strings.TrimSpace(fwd) != "" {
		if h := hostOnly(strings.TrimSpace(fwd)); h != "" && hostPattern.MatchString(h) {
			return h, nil
		}
	}
	host := hostOnly(r.Host)
	if host == "" || !hostPattern.MatchString(host) {
		return "127.0.0.1", nil
	}
	return host, nil
}

// hostOnly is a Host header's host, without its port or IPv6 brackets.
func hostOnly(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	return strings.Trim(host, "[]")
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
	ke, err := a.kitEnvFor(r, labURL(host))
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	httputilx.WriteJSON(w, 200, map[string]any{
		"host": host,
		"env":  scopeOf(r.Context()),
		"kits": ke.Apply(console.Kits(a.cat, host, a.keysDir)),
	})
}

// labURL is the lab API's address for a platform that reaches the lab at
// host: LAB_API_PUBLIC_PORT (default 8097) on that host.
func labURL(host string) string {
	return "http://" + net.JoinHostPort(host, env("LAB_API_PUBLIC_PORT", "8097"))
}

func (a *app) connectEnv(w http.ResponseWriter, r *http.Request) {
	host, err := kitHost(r)
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	// An environment's kit carries its own logins (environments.md §5);
	// the default's is the lab's as it always was.
	ke, err := a.kitEnvFor(r, labURL(host))
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	kits := ke.Apply(console.Kits(a.cat, host, a.keysDir))
	name := "fintechlab.env"
	if ke != nil {
		name = "fintechlab-" + ke.Env + ".env"
	}
	if id := r.PathValue("service"); id != "" {
		k, ok := console.KitFor(a.cat, host, a.keysDir, id)
		if !ok {
			httputilx.Error(w, 404, id+" has nothing a platform needs to connect")
			return
		}
		kits = ke.Apply([]console.Kit{k})
		if ke != nil {
			kits = kits[1:] // one vendor's file: without the lab's own section
		}
		name = "fintechlab-" + id + ".env"
		if ke != nil {
			name = "fintechlab-" + ke.Env + "-" + id + ".env"
		}
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
	// An environment's own key (its B4B signing key) instead of the lab's.
	ke, err := a.kitEnvFor(r, "")
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	if b, ok := ke.File(name); ok {
		download(w, name, "application/octet-stream", b)
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
