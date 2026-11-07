// Console is the lab's control panel: one page that shows which simulated
// services are up, what the sim banks hold, and lets you create the
// merchants the money flow needs before any of it means anything.
//
// It is a page and nothing more. Every button is a call to the lab API
// (cmd/labapi, /v1) — the same API a script or an agent calls — which this
// server forwards to so the page calls its own origin and needs no CORS.
// It keeps no state, holds no credential and knows no vendor: nothing in
// this lab can only be reached through it, and it being down is never why
// a scenario cannot run.
//
// It also forwards the console's old /api/* paths, which the lab API still
// answers as aliases, because a running plugin registers at
// {console}/api/plugins (docs/lab-api/plan.md, phase 4 retires them).
package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
)

//go:embed web
var webFS embed.FS

func main() {
	addr := env("LISTEN", ":8090")
	target := env("LAB_API_URL", "http://127.0.0.1:8097")
	h, err := handler(target, os.Getenv("LAB_API_TOKEN"))
	if err != nil {
		log.Fatalf("console: %v", err)
	}
	log.Printf("console listening on %s, lab API at %s", addr, target)
	log.Fatal(http.ListenAndServe(addr, h))
}

// handler serves the embedded page and forwards the API to labAPI. token,
// when set, is added to every forwarded call: the lab API asks for it, and
// the page has no way to hold a secret (docs/lab-api/architecture.md §8).
func handler(labAPI, token string) (http.Handler, error) {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return nil, fmt.Errorf("embedded assets: %w", err)
	}
	up, err := url.Parse(labAPI)
	if err != nil || up.Scheme == "" || up.Host == "" {
		return nil, fmt.Errorf("LAB_API_URL %q is not a URL", labAPI)
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(up)
			pr.SetXForwarded()
			if token != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+token)
			}
		},
		// Streams pass through as they come: a settlement file upload on
		// the way in, a log stream on the way out.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// The page says this in its error line, so it names what is
			// missing rather than looking like a broken console.
			httputilx.Error(w, 502, fmt.Sprintf("the lab API at %s is not answering: %v", labAPI, err))
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, map[string]string{"status": "ok", "service": "console", "lab_api": labAPI})
	})
	mux.Handle("/v1", proxy)
	mux.Handle("/v1/", proxy)
	mux.Handle("/api", proxy)
	mux.Handle("/api/", proxy)

	// Embedded files carry no modification time, so a browser has nothing to
	// revalidate against and can keep last build's app.js after a rebuild.
	// no-cache makes it ask every time; the assets are a few hundred KB, local.
	static := http.FileServer(http.FS(sub))
	// "/" with no method: the mux will not register "GET /" beside the
	// method-less API prefixes above, so the method is checked here.
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			httputilx.Error(w, 405, "the console serves its page here; the API is under /v1")
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	}))

	// no-cache only reaches the next load. A tab left open across a rebuild
	// keeps running the page it loaded — buttons that post what an older
	// console posted — so every answer, the API's included, names the page
	// this console serves, and the page says so when it is no longer that
	// one (design-system.md, contract 11).
	build := webBuild(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Console-Build", build)
		if strings.HasPrefix(r.URL.Path, "/v1") || strings.HasPrefix(r.URL.Path, "/api") {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		mux.ServeHTTP(w, r)
	}), nil
}

// webBuild identifies the embedded page: a hash of every file under web/.
func webBuild(web fs.FS) string {
	h := sha256.New()
	_ = fs.WalkDir(web, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(web, path)
		if err != nil {
			return err
		}
		h.Write([]byte(path))
		h.Write(b)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
