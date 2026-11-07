package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/labswitch"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
	"github.com/webduvet/fintechlab-simple/internal/wlsftp"
)

// testSurfaces builds the stand-in's two surfaces as main does, with
// nothing in the environment moving or narrowing the control port.
func testSurfaces(t *testing.T, a *app, p *worldlinePuller) (*http.ServeMux, *simctl.Control) {
	t.Helper()
	t.Setenv("SIM_CONTROL_LISTEN", "")
	t.Setenv("SIM_CONTROL_ALLOW", "")
	if a.wiring == nil {
		a.wiring = labswitch.New("settlement", "test", true)
	}
	mux, ctl, err := a.surfaces(":8083", p)
	if err != nil {
		t.Fatal(err)
	}
	return mux, ctl
}

// controlRoutes is every route the stand-in's control port serves when a
// Worldline host is configured.
var controlRoutes = []string{
	"GET /sim/connected",
	"POST /sim/connected",
	"POST /sim/pull",
	"GET /sim/info",
	"GET /sim/logs",
	"POST /sim/reset",
}

func serve(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// pattern is the route mux would hand a request to, without calling it.
func pattern(mux *http.ServeMux, route string) string {
	method, path, _ := strings.Cut(route, " ")
	_, p := mux.Handler(httptest.NewRequest(method, path, nil))
	return p
}

// sameAnswer fails unless both calls answered the same status and body.
func sameAnswer(t *testing.T, what string, old, now *httptest.ResponseRecorder) {
	t.Helper()
	if old.Code != now.Code || old.Body.String() != now.Body.String() {
		t.Errorf("%s: %d %s\nagainst %d %s", what, old.Code, old.Body, now.Code, now.Body)
	}
}

// Every /sim route is the control port's, on :9083; the stand-in's own
// port passes the same paths through to it, and keeps nothing of the lab's.
func TestTheControlPortServesEverySimRoute(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	p := &worldlinePuller{app: a, archiveDir: t.TempDir(), seen: map[string]bool{}}
	mux, ctl := testSurfaces(t, a, p)
	if ctl.Addr() != ":9083" {
		t.Errorf("control port = %q, want :9083", ctl.Addr())
	}
	for _, route := range controlRoutes {
		if got := pattern(ctl.Mux, route); got != route {
			t.Errorf("control port: %s is handled by %q", route, got)
		}
		if got := pattern(mux, route); got != "/sim/" {
			t.Errorf("settlement port: %s is handled by %q, want the /sim/ alias", route, got)
		}
	}
	// The stand-in's own routes stay on its own port.
	for _, route := range []string{"POST /worldline/pull", "GET /worldline/files", "GET /reports/settlement", "POST /internal/b4b-webhook"} {
		if got := pattern(ctl.Mux, route); got != "" {
			t.Errorf("control port serves %s (as %q)", route, got)
		}
		if got := pattern(mux, route); got != route {
			t.Errorf("settlement port: %s is handled by %q", route, got)
		}
	}
	if r := serve(t, ctl.Handler(), "GET", "/health", ""); r.Code != 200 || !strings.Contains(r.Body.String(), `"surface":"control"`) {
		t.Errorf("control GET /health: %d %s", r.Code, r.Body)
	}

	// With no Worldline host there is no pull, on either port.
	b := newTestApp(t, t.TempDir())
	mux, ctl = testSurfaces(t, b, nil)
	if got := pattern(ctl.Mux, "POST /sim/pull"); got != "" {
		t.Errorf("with no puller, the control port serves POST /sim/pull (as %q)", got)
	}
	sameAnswer(t, "POST /worldline/pull and POST /sim/pull with no puller",
		serve(t, mux, "POST", "/worldline/pull", ""), serve(t, ctl.Handler(), "POST", "/sim/pull", ""))
}

// The switch answers the same on both ports, and a change through the old
// path is the control port's.
func TestTheOldSimPathsStillAnswerOnTheSettlementPort(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	mux, ctl := testSurfaces(t, a, nil)
	h := ctl.Handler()

	sameAnswer(t, "GET /sim/connected", serve(t, mux, "GET", "/sim/connected", ""), serve(t, h, "GET", "/sim/connected", ""))
	if r := serve(t, mux, "POST", "/sim/connected", `{"connected": false, "reason": "a platform is plugged in"}`); r.Code != 200 {
		t.Fatalf("POST /sim/connected on the settlement port: %d %s", r.Code, r.Body)
	}
	if a.wiring.Connected() {
		t.Error("the switch on the settlement port did not reach the stand-in")
	}
	old, now := serve(t, mux, "GET", "/sim/connected", ""), serve(t, h, "GET", "/sim/connected", "")
	if !strings.Contains(now.Body.String(), `"connected":false`) {
		t.Errorf("GET /sim/connected after disconnecting: %s", now.Body)
	}
	sameAnswer(t, "GET /sim/connected after disconnecting", old, now)
	sameAnswer(t, "POST /sim/connected {}", serve(t, mux, "POST", "/sim/connected", `{}`), serve(t, h, "POST", "/sim/connected", `{}`))
	if r := serve(t, mux, "POST", "/sim/reset", ""); r.Code != 200 || !strings.Contains(r.Body.String(), `"kept":["connected"]`) {
		t.Errorf("POST /sim/reset on the settlement port: %d %s", r.Code, r.Body)
	}
	if r := serve(t, mux, "GET", "/sim/info", ""); r.Code != 200 || !strings.Contains(r.Body.String(), `"control_listener":":9083"`) {
		t.Errorf("GET /sim/info on the settlement port: %d %s", r.Code, r.Body)
	}
}

// POST /sim/pull answers as POST /worldline/pull does for the same state:
// a Worldline that cannot be reached, and one with nothing new on it.
func TestPullOnTheControlPortAnswersAsTheStandInsOwn(t *testing.T) {
	a := newTestApp(t, t.TempDir())
	p := &worldlinePuller{app: a, archiveDir: t.TempDir(), seen: map[string]bool{}}
	p.pulled = []pulledFile{{Name: "20260925083000_Worldline_Settlement_ER_EUR.csv.pgp", Slot: "ER",
		PulledAt: time.Date(2026, 9, 25, 8, 30, 4, 0, time.UTC), Bytes: 2210, Processed: true, Settlements: []string{"set_1"}, PayoutsByMID: 1}}
	mux, ctl := testSurfaces(t, a, p)
	h := ctl.Handler()

	// No credentials: the pull fails before it dials.
	p.cfg = wlsftp.ClientConfig{Host: "worldline", Port: "2222"}
	old, now := serve(t, mux, "POST", "/worldline/pull", ""), serve(t, h, "POST", "/sim/pull", "")
	if old.Code != 502 || !strings.Contains(old.Body.String(), "pull from worldline:2222 failed") {
		t.Errorf("POST /worldline/pull with no credentials: %d %s", old.Code, old.Body)
	}
	sameAnswer(t, "an unreachable Worldline", old, now)

	// A real SFTP server with nothing new on it.
	host, port := sftpServer(t, "test-password")
	p.cfg = wlsftp.ClientConfig{Host: host, Port: port, User: "infinitepay", Password: "test-password", Timeout: 5 * time.Second}
	old, now = serve(t, mux, "POST", "/worldline/pull", ""), serve(t, h, "POST", "/sim/pull", "")
	if old.Code != 200 || !strings.Contains(old.Body.String(), `"pulled":0`) || !strings.Contains(old.Body.String(), `"settlement_ids":["set_1"]`) {
		t.Errorf("POST /worldline/pull with nothing new: %d %s", old.Code, old.Body)
	}
	sameAnswer(t, "a Worldline with nothing new", old, now)
	sameAnswer(t, "POST /sim/pull on the settlement port", serve(t, mux, "POST", "/sim/pull", ""), now)
}

// sftpServer serves an empty Worldline SFTP root on a loopback port until
// the test ends.
func sftpServer(t *testing.T, password string) (host, port string) {
	t.Helper()
	root := t.TempDir()
	if err := wlsftp.EnsureLayout(root); err != nil {
		t.Fatal(err)
	}
	signer, err := wlsftp.LoadOrGenerateHostKey(filepath.Join(t.TempDir(), "host_key"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = wlsftp.Serve(ctx, ln, root, signer, password, "", func(string, ...any) {})
	}()
	t.Cleanup(func() { cancel(); <-done })
	host, port, _ = net.SplitHostPort(ln.Addr().String())
	return host, port
}
