package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
	"github.com/webduvet/fintechlab-simple/internal/wlsftp"
	"github.com/webduvet/fintechlab-simple/internal/worldline"
)

// settleDay is the day the tests trade on and settle.
const settleDay = "2026-09-24"

// seedTwo pushes sbx and trades in both environments on settleDay: the
// default's one EUR sale, and sbx's two EUR sales at two MIDs and one GBP.
func seedTwo(t *testing.T, h http.Handler) {
	t.Helper()
	if r := serve(t, h, "PUT", "/sim/customers/sbx", sbxCustomer); r.Code != 200 {
		t.Fatalf("PUT /sim/customers/sbx: %d %s", r.Code, r.Body)
	}
	if r := serve(t, h, "POST", "/sim/transactions", `{"mid":"MID-DEFAULT","currency":"EUR","amount_cents":15000,"date":"`+settleDay+`"}`); r.Code != 201 {
		t.Fatalf("seed the default: %d %s", r.Code, r.Body)
	}
	if r := serve(t, h, "POST", "/sim/transactions?env=sbx", `[
		{"mid":"MID-SBX-A","currency":"EUR","amount_cents":7000,"date":"`+settleDay+`"},
		{"mid":"MID-SBX-B","currency":"EUR","amount_cents":3000,"date":"`+settleDay+`"},
		{"mid":"MID-SBX-A","currency":"GBP","amount_cents":900,"date":"`+settleDay+`"}]`); r.Code != 201 {
		t.Fatalf("seed sbx: %d %s", r.Code, r.Body)
	}
}

// render asks for one file and wants 200.
func render(t *testing.T, h http.Handler, query, body string) renderedFile {
	t.Helper()
	r := serve(t, h, "POST", "/sim/settlement-files/render"+query, body)
	if r.Code != 200 {
		t.Fatalf("render%s %s: %d %s", query, body, r.Code, r.Body)
	}
	var f renderedFile
	decode(t, r, &f)
	return f
}

// publishQuery is publish's query for name in env, wired or not.
func publishQuery(env, name, wire string) string {
	q := url.Values{"name": {name}}
	if env != "" {
		q.Set("env", env)
	}
	if wire != "" {
		q.Set("wire", wire)
	}
	return "/sim/settlement-files/publish?" + q.Encode()
}

// names is the regular files in root/dir.
func names(t *testing.T, root, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// opened decrypts a published file with the lab's PGP keys.
func opened(t *testing.T, w *sim, sealed []byte) string {
	t.Helper()
	plain, err := wlsftp.Decrypt(sealed, w.acq.keys.Keyring())
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	return string(plain)
}

// Render is the file the cycle would cut from the environment's own
// trading, named with its own identifier, and nothing more: nothing is
// published, wired or recorded. The cycle run afterwards cuts the same
// bytes.
func TestRenderIsTheFileTheCycleWouldCut(t *testing.T) {
	bc, wires := bridgeFake(t)
	w := newTestSim(t, bc.URL)
	_, ctl := testSurfaces(t, w)
	h := ctl.Handler()
	seedTwo(t, h)
	jail := filepath.Join(w.tenants.root, "sbx")

	sbx := render(t, h, "?env=sbx", `{"currency":"eur","date":"`+settleDay+`"}`)
	if !worldline.IsCutFilename("Infinite_Payment", sbx.Name) || !strings.HasSuffix(sbx.Name, "_Infinite_Payment_ER_EUR.csv") {
		t.Errorf("sbx's file is named %q, want the cycle's name under sbx's identifier", sbx.Name)
	}
	if strings.Join(sbx.MIDs, ",") != "MID-SBX-A,MID-SBX-B" || sbx.Total != "100.00" || sbx.Items != 2 || sbx.Currency != "EUR" ||
		sbx.Slot != worldline.SlotMorning || sbx.FromDate != settleDay || sbx.ToDate != settleDay {
		t.Errorf("sbx's file: %+v", sbx)
	}
	parsed, err := worldline.ParseSettlementFile(strings.NewReader(sbx.Content))
	if err != nil || parsed.Meta.SettlementAmount != 10000 || parsed.Meta.SettlementCurrency != "EUR" {
		t.Fatalf("sbx's content does not parse as its file: %v %+v", err, parsed)
	}
	if strings.Contains(sbx.Content, "MID-DEFAULT") {
		t.Error("sbx's file settles the default's trading")
	}

	def := render(t, h, "", `{"currency":"EUR","date":"`+settleDay+`"}`)
	if !strings.HasSuffix(def.Name, "_Worldline_Settlement_ER_EUR.csv") || strings.Join(def.MIDs, ",") != "MID-DEFAULT" || def.Total != "150.00" {
		t.Errorf("the default's file: %+v", def)
	}
	if strings.Contains(def.Content, "MID-SBX") {
		t.Error("the default's file settles sbx's trading")
	}

	// The Lab-Environment header names the environment as ?env= does.
	req := httptest.NewRequest("POST", "/sim/settlement-files/render", strings.NewReader(`{"currency":"EUR","date":"`+settleDay+`"}`))
	req.Header.Set(tenant.Header, "sbx")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var byHeader renderedFile
	decode(t, rec, &byHeader)
	if rec.Code != 200 || byHeader.Content != sbx.Content {
		t.Errorf("render with Lab-Environment: sbx: %d, %+v", rec.Code, byHeader)
	}

	// The afternoon slot is the same content under its own code.
	ar := render(t, h, "?env=sbx", `{"currency":"EUR","date":"`+settleDay+`","slot":"afternoon"}`)
	if !strings.HasSuffix(ar.Name, "_Infinite_Payment_AR_EUR.csv") || ar.Slot != worldline.SlotAfternoon || ar.Content != sbx.Content {
		t.Errorf("sbx's AR file: %+v", ar)
	}

	// No date: the day the cycle would settle now, by the lab clock.
	due, _ := w.acq.config().CoverageFor(labclock.Now().In(time.Local))
	if r := serve(t, h, "POST", "/sim/transactions?env=sbx", `{"mid":"MID-DUE","currency":"GBP","amount_cents":500,"date":"`+due+`"}`); r.Code != 201 {
		t.Fatalf("seed: %d %s", r.Code, r.Body)
	}
	now := render(t, h, "?env=sbx", `{"currency":"GBP"}`)
	if now.FromDate != due || now.ToDate != due || strings.Join(now.MIDs, ",") != "MID-DUE" || now.Slot != worldline.SlotMorning {
		t.Errorf("render with no date: %+v, want %s's ER file", now, due)
	}

	// Nothing of it happened.
	for _, root := range []string{w.acq.root, jail} {
		for _, d := range []string{wlsftp.DirDownload, wlsftp.DirToWLNordic} {
			if got := names(t, root, d); len(got) != 0 {
				t.Errorf("render left %v in %s/%s", got, root, d)
			}
		}
	}
	e, _ := w.at("sbx")
	if len(wires()) != 0 || len(w.acq.history()) != 0 || len(e.acq.history()) != 0 {
		t.Errorf("render wired %d lump sum(s) or recorded a run", len(wires()))
	}

	// The cycle cuts what render answered, byte for byte.
	if r := serve(t, h, "POST", "/sim/settlement-cycle/run?slot=morning&date="+settleDay+"&env=sbx", ""); r.Code != 200 {
		t.Fatalf("run sbx's cycle: %d %s", r.Code, r.Body)
	}
	run := e.acq.history()[0]
	for _, name := range run.Published {
		if !strings.Contains(name, "_EUR.csv") {
			continue
		}
		sealed, err := os.ReadFile(filepath.Join(jail, wlsftp.DirDownload, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := opened(t, w, sealed); got != sbx.Content {
			t.Errorf("the cycle cut\n%s\nrender answered\n%s", got, sbx.Content)
		}
	}
}

func TestRenderRefusals(t *testing.T) {
	w := newTestSim(t, "")
	_, ctl := testSurfaces(t, w)
	h := ctl.Handler()
	seedTwo(t, h)
	for _, c := range []struct {
		query, body string
		code        int
		says        string
	}{
		{"?env=sbx", `{"currency":"GBP","date":"2026-01-01"}`, 404, "nothing settles in GBP on 2026-01-01 in sbx"},
		{"", `{"currency":"USD","date":"` + settleDay + `"}`, 404, "nothing settles"},
		{"?env=sbx", `{"date":"` + settleDay + `"}`, 400, "currency is required"},
		{"?env=sbx", `{"currency":"EURO"}`, 400, "three-letter"},
		{"?env=sbx", `{"currency":"EUR","date":"24/09/2026"}`, 400, "YYYY-MM-DD"},
		{"?env=sbx", `{"currency":"EUR","slot":"noon"}`, 400, "unknown settlement slot"},
		{"?env=sbx", `{"currency":"EUR","colour":"red"}`, 400, "unknown field"},
		{"?env=sbx", ``, 400, "want {"},
		{"?env=*", `{"currency":"EUR"}`, 400, "name one environment"},
		{"?env=nope", `{"currency":"EUR"}`, 404, "no environment nope"},
	} {
		r := serve(t, h, "POST", "/sim/settlement-files/render"+c.query, c.body)
		if r.Code != c.code || !strings.Contains(r.Body.String(), c.says) {
			t.Errorf("render%s %s: %d %s, want %d saying %q", c.query, c.body, r.Code, r.Body, c.code, c.says)
		}
	}
}

// Publish delivers a file exactly as the cycle delivers one it cut: in the
// environment's download/ and to_WLNORDIC/, encrypted and signed, as
// {name}.pgp, collected over the environment's SFTP login; with wire=true
// the lump sum goes to the environment's own safeguarding account. It is
// recorded in the environment's activity, not as a cycle run, and the
// reset takes it back whatever its name.
func TestPublishIsTheCyclesDelivery(t *testing.T) {
	bc, wires := bridgeFake(t)
	w := newTestSim(t, bc.URL)
	w.acq.sgaAccount = map[string]string{"EUR": tenant.DefaultSafeguardingEUR, "GBP": tenant.DefaultSafeguardingGBP}
	_, ctl := testSurfaces(t, w)
	h := ctl.Handler()
	seedTwo(t, h)
	jail := filepath.Join(w.tenants.root, "sbx")
	e, _ := w.at("sbx")

	f := render(t, h, "?env=sbx", `{"currency":"EUR","date":"`+settleDay+`"}`)
	r := serve(t, h, "POST", publishQuery("sbx", f.Name, "true"), f.Content)
	if r.Code != 201 {
		t.Fatalf("publish in sbx: %d %s", r.Code, r.Body)
	}
	var out publishedFile
	decode(t, r, &out)
	if out.Published != f.Name+".pgp" || out.Name != f.Name || out.Currency != "EUR" || out.Total != "100.00" ||
		strings.Join(out.MIDs, ",") != "MID-SBX-A,MID-SBX-B" {
		t.Errorf("publish answered %+v", out)
	}
	sga := tenant.SafeguardingAccount("sbx", "EUR")
	if ls := out.LumpSum; ls == nil || ls.AccountID != sga || ls.Status != "credited" || ls.AmountCents != 10000 ||
		ls.Reference != "WORLDLINE SETTLEMENT EUR "+settleDay {
		t.Errorf("publish's lump sum: %+v, want 100.00 credited to %s", out.LumpSum, sga)
	}
	got := wires()
	if len(got) != 1 || got[0].env != "sbx" || got[0].body["amount"] != "100.00" || got[0].body["currency"] != "EUR" ||
		got[0].body["reference"] != "WORLDLINE SETTLEMENT EUR "+settleDay {
		t.Fatalf("the bridge received %+v, want sbx's one lump sum of 100.00", got)
	}

	// In sbx's folders, as the cycle puts its own, and nowhere else.
	for _, d := range []string{wlsftp.DirDownload, wlsftp.DirToWLNordic} {
		sealed, err := os.ReadFile(filepath.Join(jail, d, out.Published))
		if err != nil {
			t.Fatalf("sbx's %s: %v", d, err)
		}
		if got := opened(t, w, sealed); got != f.Content {
			t.Errorf("sbx's %s/%s decrypts to something else", d, out.Published)
		}
		if got := names(t, w.acq.root, d); len(got) != 0 {
			t.Errorf("the default's %s holds %v", d, got)
		}
	}
	addr := serveSFTP(t, w, "default-secret")
	c, err := login(t, addr, "sbx-platform", "sbx-secret")
	if err != nil {
		t.Fatal(err)
	}
	if got := listing(t, c, wlsftp.DirDownload); strings.Join(got, ",") != out.Published {
		t.Errorf("sbx's login lists %v in download/, want %s", got, out.Published)
	}
	rf, err := c.Open(wlsftp.DirDownload + "/" + out.Published)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := io.ReadAll(rf)
	rf.Close()
	if err != nil {
		t.Fatal(err)
	}
	if opened(t, w, sealed) != f.Content {
		t.Error("the file collected over SFTP is not the one published")
	}

	// Not a cycle run; sbx's activity, not the default's.
	if len(e.acq.history()) != 0 {
		t.Errorf("publish recorded a cycle run: %+v", e.acq.history())
	}
	var act struct {
		Logs []struct {
			Events []struct {
				Env    string            `json:"env"`
				Op     string            `json:"op"`
				Status string            `json:"status"`
				Detail map[string]string `json:"detail"`
			} `json:"events"`
		} `json:"logs"`
	}
	decode(t, serve(t, h, "GET", "/sim/activity?env=sbx", ""), &act)
	found := false
	for _, ev := range act.Logs[0].Events {
		if ev.Op == opPublished {
			found = true
			if ev.Env != "sbx" || ev.Status != "ok" || ev.Detail["file"] != out.Published || ev.Detail["lump_sum"] != "credited" {
				t.Errorf("sbx's publish event: %+v", ev)
			}
		}
	}
	if !found {
		t.Errorf("sbx's activity has no %s event: %+v", opPublished, act)
	}
	if strings.Contains(serve(t, h, "GET", "/sim/activity", "").Body.String(), opPublished) {
		t.Error("sbx's publish is in the default's activity")
	}

	// The default's, unwired: no lump sum, in the default's folders only.
	def := render(t, h, "", `{"currency":"EUR","date":"`+settleDay+`"}`)
	r = serve(t, h, "POST", publishQuery("", def.Name, "false"), def.Content)
	if r.Code != 201 || !strings.Contains(r.Body.String(), `"lump_sum":null`) {
		t.Fatalf("publish in the default, unwired: %d %s", r.Code, r.Body)
	}
	if !exists(filepath.Join(w.acq.root, wlsftp.DirDownload, def.Name+".pgp")) || exists(filepath.Join(jail, wlsftp.DirDownload, def.Name+".pgp")) {
		t.Error("the default's file is not in the default's download/ alone")
	}

	// An afternoon file is a confirmation: wire=true wires nothing, as the
	// afternoon slot never does.
	ar := render(t, h, "?env=sbx", `{"currency":"GBP","date":"`+settleDay+`","slot":"AR"}`)
	r = serve(t, h, "POST", publishQuery("sbx", ar.Name, "true"), ar.Content)
	if r.Code != 201 || !strings.Contains(r.Body.String(), `"lump_sum":null`) {
		t.Fatalf("publish an AR file with wire=true: %d %s", r.Code, r.Body)
	}
	if n := len(wires()); n != 1 {
		t.Errorf("%d lump sums wired, want still the one", n)
	}

	// Any name, not only the cycle's: the reset takes it back all the same.
	if r := serve(t, h, "POST", publishQuery("sbx", "hand-made.csv", ""), f.Content); r.Code != 201 || !strings.Contains(r.Body.String(), `"published":"hand-made.csv.pgp"`) {
		t.Fatalf("publish hand-made.csv: %d %s", r.Code, r.Body)
	}
	res, err := w.reset(context.Background(), simctl.ResetRequest{Scope: simctl.ScopeFlow, Env: "sbx"})
	if err != nil {
		t.Fatal(err)
	}
	checkCleared(t, res.Cleared, map[string]int{"settlement_files": 6, "lump_sums": 1, "cycle_runs": 0})
	for _, d := range []string{wlsftp.DirDownload, wlsftp.DirToWLNordic} {
		if got := names(t, jail, d); len(got) != 0 {
			t.Errorf("sbx's reset left %v in %s", got, d)
		}
	}
	if !exists(filepath.Join(w.acq.root, wlsftp.DirDownload, def.Name+".pgp")) {
		t.Error("sbx's reset took the default's file")
	}
	if strings.Contains(serve(t, h, "GET", "/sim/activity?env=sbx", "").Body.String(), opPublished) {
		t.Error("sbx's reset left its publish events")
	}
}

func TestPublishRefusals(t *testing.T) {
	bc, wires := bankingCircleFake(t, nil)
	w := newTestSim(t, bc.URL)
	_, ctl := testSurfaces(t, w)
	h := ctl.Handler()
	seedTwo(t, h)
	f := render(t, h, "?env=sbx", `{"currency":"EUR","date":"`+settleDay+`"}`)
	jail := filepath.Join(w.tenants.root, "sbx")

	for _, c := range []struct {
		what, path, body string
		code             int
		says             string
	}{
		{"plain text", publishQuery("sbx", f.Name, "true"), "hello, this is not a settlement file", 400, "not a settlement file"},
		{"an empty body", publishQuery("sbx", f.Name, ""), "", 400, "not a settlement file"},
		{"a third decimal", publishQuery("sbx", f.Name, ""), strings.Replace(f.Content, `"100.00"`, `"100.005"`, 1), 400, "at most two decimals"},
		{"no name", publishQuery("sbx", "", ""), f.Content, 400, "name is required"},
		{"a parent", publishQuery("sbx", "../escape.csv", ""), f.Content, 400, "no path separator"},
		{"a folder", publishQuery("sbx", "download/x.csv", ""), f.Content, 400, "no path separator"},
		{"a backslash", publishQuery("sbx", `a\b.csv`, ""), f.Content, 400, "no path separator"},
		{"dot dot", publishQuery("sbx", "..", ""), f.Content, 400, "no path separator"},
		{"a control character", publishQuery("sbx", "a\x00b.csv", ""), f.Content, 400, "control character"},
		{"a long name", publishQuery("sbx", strings.Repeat("x", 251), ""), f.Content, 400, "at most 250"},
		{"a bad wire", publishQuery("sbx", f.Name, "maybe"), f.Content, 400, "want true or false"},
		{"every environment", publishQuery("*", f.Name, ""), f.Content, 400, "name one environment"},
		{"an unknown environment", publishQuery("nope", f.Name, ""), f.Content, 404, "no environment nope"},
	} {
		r := serve(t, h, "POST", c.path, c.body)
		if r.Code != c.code || !strings.Contains(r.Body.String(), c.says) {
			t.Errorf("%s: %d %s, want %d saying %q", c.what, r.Code, r.Body, c.code, c.says)
		}
	}
	for _, root := range []string{w.acq.root, jail} {
		for _, d := range []string{wlsftp.DirDownload, wlsftp.DirToWLNordic} {
			if got := names(t, root, d); len(got) != 0 {
				t.Errorf("a refused publish left %v in %s/%s", got, root, d)
			}
		}
	}
	if n := wires.Load(); n != 0 {
		t.Errorf("a refused publish wired %d lump sum(s)", n)
	}
}

// A publish a reset lands in: one that started before the reset publishes
// and wires nothing after it; a wire already on its way is waited for, and
// the reset then takes back the file it went with.
func TestAPublishAResetLandsInIsDropped(t *testing.T) {
	g := newGate()
	bc, wires := bankingCircleFake(t, g.hold)
	w := newTestSim(t, bc.URL)
	_, ctl := testSurfaces(t, w)
	h := ctl.Handler()
	if _, err := w.acq.store.Add(worldline.Transaction{MID: testMerchant, Currency: "EUR", AmountCents: 10000, Date: settleDay}); err != nil {
		t.Fatal(err)
	}
	f := render(t, h, "", `{"currency":"EUR","date":"`+settleDay+`"}`)
	parsed, err := worldline.ParseSettlementFile(strings.NewReader(f.Content))
	if err != nil {
		t.Fatal(err)
	}
	def, _ := w.at(tenant.Default)

	// Started before the reset: nothing lands.
	started := w.guard.start()
	resetScope(t, w, simctl.ScopeFlow)
	if _, err := w.publishFile(def, started, f.Name, []byte(f.Content), parsed, true); err != errReset {
		t.Fatalf("publish after the reset: %v, want errReset", err)
	}
	if got := names(t, w.acq.root, wlsftp.DirDownload); len(got) != 0 || wires.Load() != 0 {
		t.Fatalf("a dropped publish left %v and wired %d", got, wires.Load())
	}

	// The wire on its way when the reset lands.
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- serve(t, h, "POST", publishQuery("", f.Name, "true"), f.Content) }()
	<-g.entered
	reset := make(chan simctl.ResetResult, 1)
	go func() {
		res, _ := w.reset(context.Background(), simctl.ResetRequest{Scope: simctl.ScopeFlow})
		reset <- res
	}()
	waitForWriter(t, w.guard)
	select {
	case <-reset:
		t.Fatal("the reset finished while a wire was on its way")
	default:
	}
	close(g.release)
	r := <-answered
	res := <-reset
	if r.Code != 201 || wires.Load() != 1 {
		t.Fatalf("the publish whose wire was on its way: %d %s, %d wire(s)", r.Code, r.Body, wires.Load())
	}
	checkCleared(t, res.Cleared, map[string]int{"settlement_files": 2, "lump_sums": 1})
	if got := names(t, w.acq.root, wlsftp.DirDownload); len(got) != 0 {
		t.Errorf("the reset left %v", got)
	}
	var out map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil || out["lump_sum"] == nil {
		t.Errorf("the answer: %s", r.Body)
	}
}

func TestAWorldlineNamedFileIsPublishedUnderTheMomentsTimestamp(t *testing.T) {
	at := time.Date(2026, 11, 11, 8, 30, 0, 0, time.UTC) // 09:30 in Paris
	if got := restamp("20261110233849_Infinite_Payment_ER_EUR.csv", at); got != "20261111093000_Infinite_Payment_ER_EUR.csv" {
		t.Fatalf("restamp = %s", got)
	}
	if got := restamp("my-own-file.csv", at); got != "my-own-file.csv" {
		t.Fatalf("another name was changed: %s", got)
	}
}
