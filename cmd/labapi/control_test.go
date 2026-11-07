package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/webduvet/fintechlab-simple/internal/console"
	"github.com/webduvet/fintechlab-simple/internal/logring"
)

// fakeSims is a set of services, each on its own address, that answer the
// control contract: POST /sim/reset recorded in the order it arrives, and
// GET /sim/logs from a real ring.
type fakeSims struct {
	mu     sync.Mutex
	resets []string // "service scope force"
	held   bool     // the clock's GET /clock reports a hold
	fail   string   // this service answers its reset with 500
	rings  map[string]*logring.Ring
}

func newFakeSims(t *testing.T, a *app, ids ...string) *fakeSims {
	t.Helper()
	f := &fakeSims{rings: map[string]*logring.Ring{}}
	var svcs []console.Service
	for _, id := range ids {
		id := id
		ring := logring.New(100)
		f.rings[id] = ring
		mux := http.NewServeMux()
		mux.HandleFunc("POST /sim/reset", func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Scope string `json:"scope"`
				Force bool   `json:"force"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.resets = append(f.resets, fmt.Sprintf("%s %s %v", id, body.Scope, body.Force))
			fail := f.fail == id
			f.mu.Unlock()
			if fail {
				w.WriteHeader(500)
				_, _ = w.Write([]byte(`{"error":"remove /sftp/x: permission denied","status":500}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"service":%q,"scope":%q,"cleared":{"things":1},"kept":[]}`, id, body.Scope)
		})
		mux.HandleFunc("GET /sim/logs", ring.Handler(id))
		mux.HandleFunc("GET /clock", func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			held := f.held
			f.mu.Unlock()
			if held {
				_, _ = w.Write([]byte(`{"holds":[{"holder":"runner","reason":"settlement run GBP"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"holds":[]}`))
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		svcs = append(svcs, console.Service{ID: id, Name: id, BaseURL: srv.URL, Control: srv.URL})
	}
	a.cat = &console.Catalogue{Services: svcs}
	return f
}

func decode(t *testing.T, w *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), into); err != nil {
		t.Fatalf("%d %s: %v", w.Code, w.Body, err)
	}
}

type labResetAnswer struct {
	Scope    string           `json:"scope"`
	OK       bool             `json:"ok"`
	Services []map[string]any `json:"services"`
}

func TestALabResetGoesProducersFirstAndTheClockLast(t *testing.T) {
	a := testApp(t, nil)
	// Catalogue order is not reset order: the reset puts them in its own.
	f := newFakeSims(t, a, "clock", "settlement", "bank", "worldline")
	w := call(t, a, "POST", "/v1/lab/reset", "")
	var got labResetAnswer
	decode(t, w, &got)
	if w.Code != 200 || !got.OK || got.Scope != "flow" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	want := []string{"worldline flow false", "bank flow false", "settlement flow false", "clock flow false"}
	if fmt.Sprint(f.resets) != fmt.Sprint(want) {
		t.Errorf("reset order %v, want %v", f.resets, want)
	}
	last := got.Services[len(got.Services)-1]
	if last["service"] != "lab-api" || !strings.Contains(fmt.Sprint(last["kept"]), "merchants") {
		t.Errorf("the lab API's own entry = %v; flow keeps the merchant registry", last)
	}
	if got.Services[0]["cleared"] == nil {
		t.Errorf("a service's own answer is not passed on: %v", got.Services[0])
	}
}

func TestAHeldClockStopsALabResetUnlessForced(t *testing.T) {
	a := testApp(t, nil)
	f := newFakeSims(t, a, "worldline", "clock")
	f.held = true
	w := call(t, a, "POST", "/v1/lab/reset", `{"scope":"flow"}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "runner") {
		t.Fatalf("%d %s, want 409 naming the holder", w.Code, w.Body)
	}
	if len(f.resets) != 0 {
		t.Fatalf("something was reset under a held clock: %v", f.resets)
	}
	if w := call(t, a, "POST", "/v1/lab/reset", `{"force":true}`); w.Code != 200 {
		t.Fatalf("forced = %d %s", w.Code, w.Body)
	}
	if fmt.Sprint(f.resets) != "[worldline flow true clock flow true]" {
		t.Errorf("forced resets %v: force goes to every service", f.resets)
	}
}

func TestAFailedServiceIsReportedAndTheRestStillReset(t *testing.T) {
	a := testApp(t, nil)
	f := newFakeSims(t, a, "worldline", "bank", "clock")
	f.fail = "worldline"
	w := call(t, a, "POST", "/v1/lab/reset", "")
	var got labResetAnswer
	decode(t, w, &got)
	if w.Code != 502 || got.OK {
		t.Fatalf("%d ok=%v: a partial failure is not a success", w.Code, got.OK)
	}
	if got.Services[0]["status"] != float64(500) || !strings.Contains(fmt.Sprint(got.Services[0]["error"]), "permission denied") {
		t.Errorf("worldline's entry = %v: its own words should be there", got.Services[0])
	}
	if len(f.resets) != 3 {
		t.Errorf("resets %v: the others still go", f.resets)
	}
}

func TestOnlyTheNamedServicesAndAnUnknownOneIsRefused(t *testing.T) {
	a := testApp(t, nil)
	f := newFakeSims(t, a, "worldline", "bank", "clock")
	if w := call(t, a, "POST", "/v1/lab/reset", `{"services":["clock","bank"]}`); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if fmt.Sprint(f.resets) != "[bank flow false clock flow false]" {
		t.Errorf("resets %v: only those named, still in order", f.resets)
	}
	for _, body := range []string{`{"services":["nope"]}`, `{"scope":"some"}`, `{"scop":"all"}`} {
		if w := call(t, a, "POST", "/v1/lab/reset", body); w.Code != 400 {
			t.Errorf("%s = %d, want 400", body, w.Code)
		}
	}
}

func TestAnAllResetClearsTheMerchantRegistry(t *testing.T) {
	a := testApp(t, nil)
	newFakeSims(t, a, "clock")
	if w := call(t, a, "POST", "/v1/merchants", `{"legal_name":"Acme Ltd","country":"GB"}`); w.Code >= 300 {
		t.Fatalf("create merchant = %d %s", w.Code, w.Body)
	}
	w := call(t, a, "POST", "/v1/lab/reset", `{"scope":"all"}`)
	var got labResetAnswer
	decode(t, w, &got)
	own := got.Services[len(got.Services)-1]
	if fmt.Sprint(own["cleared"]) != "map[batches:0 merchants:1]" {
		t.Errorf("lab API entry = %v", own)
	}
	if n := len(a.reg.Merchants()); n != 0 {
		t.Errorf("%d merchants left after an all reset", n)
	}
}

type labLogsAnswer struct {
	Lines []struct {
		Service string `json:"service"`
		Text    string `json:"text"`
	} `json:"lines"`
	Next        string              `json:"next"`
	Unreachable []map[string]string `json:"unreachable"`
}

func (l labLogsAnswer) texts() string {
	var s []string
	for _, x := range l.Lines {
		s = append(s, x.Text)
	}
	return strings.Join(s, " ")
}

func TestLogsAreMergedByTimeAndTheCursorMissesNothing(t *testing.T) {
	a := testApp(t, nil)
	f := newFakeSims(t, a, "worldline", "bank")
	for i := 1; i <= 3; i++ {
		fmt.Fprintf(f.rings["worldline"], "w%d\n", i)
		fmt.Fprintf(f.rings["bank"], "b%d\n", i)
	}

	var newest labLogsAnswer
	decode(t, call(t, a, "GET", "/v1/logs?limit=4", ""), &newest)
	if newest.texts() != "w2 b2 w3 b3" {
		t.Fatalf("newest 4 = %q", newest.texts())
	}

	// Page from before the start: every line once, in time order.
	start := "e30" // base64 of {}
	var all []string
	since := start
	for i := 0; i < 10; i++ {
		var page labLogsAnswer
		decode(t, call(t, a, "GET", "/v1/logs?limit=2&since="+since, ""), &page)
		if len(page.Lines) == 0 {
			break
		}
		all = append(all, page.texts())
		since = page.Next
	}
	if got := strings.Join(all, " "); got != "w1 b1 w2 b2 w3 b3" {
		t.Errorf("paged = %q", got)
	}

	// Lines written after the last read are the next read's.
	fmt.Fprintln(f.rings["bank"], "b4")
	var after labLogsAnswer
	decode(t, call(t, a, "GET", "/v1/logs?since="+since, ""), &after)
	if after.texts() != "b4" {
		t.Errorf("after = %q", after.texts())
	}
}

func TestLogsSayWhichServiceDidNotAnswer(t *testing.T) {
	a := testApp(t, nil)
	f := newFakeSims(t, a, "worldline", "bank")
	fmt.Fprintln(f.rings["bank"], "still here")
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	a.cat.Services[0].Control = down.URL
	var got labLogsAnswer
	w := call(t, a, "GET", "/v1/logs?q=still", "")
	decode(t, w, &got)
	if w.Code != 200 || got.texts() != "still here" || len(got.Unreachable) != 1 || got.Unreachable[0]["service"] != "worldline" {
		t.Errorf("%d %s", w.Code, w.Body)
	}
	if w := call(t, a, "GET", "/v1/logs?services=nope", ""); w.Code != 400 {
		t.Errorf("unknown service = %d", w.Code)
	}
	if w := call(t, a, "GET", "/v1/logs?since=***", ""); w.Code != 400 {
		t.Errorf("bad cursor = %d", w.Code)
	}
}

func TestServiceLogsAndResetGoToTheService(t *testing.T) {
	a := testApp(t, nil)
	f := newFakeSims(t, a, "bank")
	fmt.Fprintln(f.rings["bank"], "hello")
	w := call(t, a, "GET", "/v1/services/bank/logs?q=hello", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "hello") {
		t.Errorf("logs = %d %s", w.Code, w.Body)
	}
	w = call(t, a, "POST", "/v1/services/bank/reset", `{"scope":"all"}`)
	if w.Code != 200 || fmt.Sprint(f.resets) != "[bank all false]" {
		t.Errorf("reset = %d %s, resets %v", w.Code, w.Body, f.resets)
	}
	_ = io.Discard
}
