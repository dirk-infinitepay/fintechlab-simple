package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/console"
	"github.com/webduvet/fintechlab-simple/internal/labswitch"
)

func boolp(b bool) *bool { return &b }

// standInApp is a console whose settlement stand-in is a real lab switch.
func standInApp(t *testing.T) (*app, *labswitch.Switch) {
	t.Helper()
	sw := labswitch.New("settlement", "pulls files", true)
	mux := http.NewServeMux()
	sw.Routes(mux, "/sim/connected")
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	a := testApp(t, srv)
	for i := range a.cat.Services {
		if a.cat.Services[i].ID == "settlement" {
			a.cat.Services[i].StandIn = &console.StandInInfo{Shown: true}
		}
	}
	return a, sw
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestAPluginSetsTheStandInsWhenItsWishesChangeNotOnEveryRenewal. A
// platform says in its descriptor that it replaces the settlement stand-in.
// That is applied when it registers — and not again ten seconds later, or
// the developer's own "reconnect" button would be undone by the next
// renewal.
func TestAPluginSetsTheStandInsWhenItsWishesChangeNotOnEveryRenewal(t *testing.T) {
	a, sw := standInApp(t)
	p := testPlugin("http://127.0.0.1:1")
	p.Settlement = nil
	p.StandIns = map[string]console.StandInPref{"settlement": {Connected: boolp(false), Shown: boolp(false)}}

	register(t, a, p)
	waitFor(t, "the switch to go off", func() bool { return !sw.Connected() })
	if !a.cat.Hidden("settlement") {
		t.Error("the plugin asked for the settlement card to be hidden")
	}
	if s, _ := a.cat.Get("settlement"); s.StandIn == nil || s.StandIn.SetBy != p.ID {
		t.Errorf("the card should say who hid it: %+v", s.StandIn)
	}

	// The developer brings it back from the console.
	w := call(t, a, http.MethodPost, "/v1/stand-ins/settlement", `{"connected":true,"shown":true}`)
	if w.Code != 200 || !sw.Connected() || a.cat.Hidden("settlement") {
		t.Fatalf("console reconnect = %d %s", w.Code, w.Body.String())
	}
	var v standInView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if v.State != "connected" || v.SetBy != "lab-api" {
		t.Errorf("after reconnecting: %+v", v)
	}

	// A renewal with the same descriptor leaves that alone.
	register(t, a, p)
	time.Sleep(100 * time.Millisecond)
	if !sw.Connected() || a.cat.Hidden("settlement") {
		t.Error("a renewal re-applied the plugin's stand_ins over the console's button")
	}

	// A changed wish is applied.
	p.StandIns = map[string]console.StandInPref{"settlement": {Connected: boolp(false)}}
	register(t, a, p)
	waitFor(t, "the changed wish to apply", func() bool { return !sw.Connected() })
}

// TestAPluginCannotHideAVendor: stand_ins names the lab's stand-ins and
// nothing else. The vendors are the deliverable.
func TestAPluginCannotHideAVendor(t *testing.T) {
	a, _ := standInApp(t)
	p := testPlugin("http://127.0.0.1:1")
	p.StandIns = map[string]console.StandInPref{"worldline": {Shown: boolp(false)}}
	body, _ := json.Marshal(p)
	w := call(t, a, http.MethodPost, "/v1/plugins", string(body))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "worldline") {
		t.Errorf("register = %d %s, want a 400 naming worldline", w.Code, w.Body.String())
	}
	if w := call(t, a, http.MethodPost, "/v1/stand-ins/worldline", `{"shown":false}`); w.Code != 404 {
		t.Errorf("hiding a vendor from the console = %d, want 404", w.Code)
	}
}

// TestAHiddenReceiverLeavesTheDiagram, with the hop into it.
func TestAHiddenReceiverLeavesTheDiagram(t *testing.T) {
	peers := flowPeers(t)
	defer peers.Close()
	a := flowApp(t, peers)
	for i := range a.cat.Services {
		if a.cat.Services[i].ID == "receiver" {
			a.cat.Services[i].StandIn = &console.StandInInfo{Shown: true}
		}
	}
	if _, ok := flowOf(t, a)["notify"]; !ok {
		t.Fatal("a shown receiver has its hop")
	}
	if w := call(t, a, http.MethodPost, "/v1/stand-ins/receiver", `{"shown":false}`); w.Code != 200 {
		t.Fatalf("hide = %d %s", w.Code, w.Body.String())
	}
	steps := flowOf(t, a)
	if _, ok := steps["notify"]; ok {
		t.Error("the hop into a hidden receiver is still drawn")
	}
	if _, ok := steps["confirm"]; !ok {
		t.Error("the platform's own confirmations must stay")
	}
	w := call(t, a, http.MethodGet, "/v1/flow", "")
	var got flowResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, p := range got.Participants {
		if p.ID == "receiver" {
			t.Error("the hidden receiver is still a participant")
		}
	}
	// Taken out of the picture is not the same as nothing happening: the
	// batch that went to it is still counted, under the diagram.
	if len(got.Hidden) != 1 || got.Hidden[0].ID != "receiver" || got.Hidden[0].Count != 1 {
		t.Errorf("hidden = %+v, want the receiver with its one batch", got.Hidden)
	}
}
