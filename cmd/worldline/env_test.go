package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/webduvet/fintechlab-simple/internal/tenant"
	"github.com/webduvet/fintechlab-simple/internal/wlsftp"
	"github.com/webduvet/fintechlab-simple/internal/worldline"
)

// sbx is the environment the tests push: an SFTP user of its own, and its
// own name for its settlement files.
const sbxCustomer = `{"env": "sbx", "name": "Sandbox",
  "worldline": {"username": "sbx-platform", "password": "sbx-secret", "settlement_identifier": "Infinite_Payment"}}`

// wire is one lump sum as Banking Circle's bridge received it.
type wire struct {
	env  string
	body map[string]any
}

// bridgeFake is Banking Circle's bridge, keeping every lump sum with the
// environment its header named.
func bridgeFake(t *testing.T) (*httptest.Server, func() []wire) {
	t.Helper()
	var mu sync.Mutex
	var got []wire
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = append(got, wire{env: r.Header.Get(tenant.Header), body: body})
		mu.Unlock()
		w.WriteHeader(201)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []wire {
		mu.Lock()
		defer mu.Unlock()
		return append([]wire(nil), got...)
	}
}

// serveSFTP runs worldline's SFTP server as main does, with the default's
// own password, and returns its address.
func serveSFTP(t *testing.T, w *sim, password string) string {
	t.Helper()
	host, err := wlsftp.LoadOrGenerateHostKey(filepath.Join(t.TempDir(), "host_key"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := wlsftp.Server{Root: w.acq.root, HostSigner: host, Password: password,
		Accounts: w.sftpAccounts, Logf: t.Logf, Observe: recordSFTP(w.sftpLog, w.tenants.customers)}
	go func() { _ = srv.Serve(ctx, ln) }()
	return ln.Addr().String()
}

// login opens an SFTP session as user, or returns the refusal.
func login(t *testing.T, addr, user, password string) (*sftp.Client, error) {
	t.Helper()
	conn, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User: user, Auth: []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	c, err := sftp.NewClient(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	t.Cleanup(func() { c.Close(); conn.Close() })
	return c, nil
}

// listing is the names in dir, as the session sees it.
func listing(t *testing.T, c *sftp.Client, dir string) []string {
	t.Helper()
	infos, err := c.ReadDir(dir)
	if err != nil {
		t.Fatalf("list %s: %v", dir, err)
	}
	var out []string
	for _, fi := range infos {
		out = append(out, fi.Name())
	}
	return out
}

func decode(t *testing.T, r *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %s: %v", r.Body, err)
	}
}

// Two platforms on one acquirer: each logs in with its own SFTP user, sees
// only its own jail, is settled in files of its own named with its own
// identifier, and is paid into its own safeguarding account. A reset or a
// deletion of one leaves the other as it was, and a login no customer has
// is the default's, exactly as before environments.
func TestEnvironmentsAreSettledApart(t *testing.T) {
	bc, wires := bridgeFake(t)
	w := newTestSim(t, bc.URL)
	w.acq.sgaAccount = map[string]string{"EUR": tenant.DefaultSafeguardingEUR, "GBP": tenant.DefaultSafeguardingGBP}
	_, ctl := testSurfaces(t, w)
	h := ctl.Handler()

	// The lab pushes sbx's customer; its jail is made with the default's
	// folders, outside the default's root.
	if r := serve(t, h, "PUT", "/sim/customers/sbx", sbxCustomer); r.Code != 200 || !strings.Contains(r.Body.String(), `"created":true`) {
		t.Fatalf("PUT /sim/customers/sbx: %d %s", r.Code, r.Body)
	}
	jail := filepath.Join(w.tenants.root, "sbx")
	for _, d := range []string{wlsftp.DirDownload, wlsftp.DirDisputesDownload, wlsftp.DirToWLNordic, wlsftp.DirFromWLNordic} {
		if fi, err := os.Stat(filepath.Join(jail, filepath.FromSlash(d))); err != nil || !fi.IsDir() {
			t.Errorf("sbx's jail has no %s: %v", d, err)
		}
	}
	if within(w.acq.root, jail) {
		t.Fatalf("sbx's jail %s is inside the default's root %s", jail, w.acq.root)
	}
	var info map[string]any
	decode(t, serve(t, h, "GET", "/sim/info", ""), &info)
	if info["environments"] != true {
		t.Errorf("GET /sim/info does not say environments: %v", info)
	}

	// Card payments for each, with different MIDs, in each one's scope.
	const day = "2026-09-24"
	if r := serve(t, h, "POST", "/sim/transactions", `{"mid":"MID-DEFAULT","currency":"EUR","amount_cents":15000,"date":"`+day+`"}`); r.Code != 201 {
		t.Fatalf("POST /sim/transactions: %d %s", r.Code, r.Body)
	}
	if r := serve(t, h, "POST", "/sim/transactions?env=sbx", `[{"mid":"MID-SBX","currency":"EUR","amount_cents":7000,"date":"`+day+`","env":"default"}]`); r.Code != 201 || !strings.Contains(r.Body.String(), `"env":"sbx"`) {
		t.Fatalf("POST /sim/transactions?env=sbx: %d %s", r.Code, r.Body)
	}
	if r := serve(t, h, "POST", "/sim/transactions?env=*", `{"mid":"M","currency":"EUR","amount_cents":1,"date":"`+day+`"}`); r.Code != 400 {
		t.Errorf("POST /sim/transactions?env=*: %d %s, want 400", r.Code, r.Body)
	}
	if r := serve(t, h, "GET", "/sim/transactions?env=nope", ""); r.Code != 404 {
		t.Errorf("GET /sim/transactions?env=nope: %d %s, want 404", r.Code, r.Body)
	}
	var txns struct {
		Count        int                     `json:"count"`
		Transactions []worldline.Transaction `json:"transactions"`
	}
	for scope, want := range map[string]string{"": "MID-DEFAULT", "?env=sbx": "MID-SBX", "?env=*": "MID-DEFAULT,MID-SBX"} {
		decode(t, serve(t, h, "GET", "/sim/transactions"+scope, ""), &txns)
		var mids []string
		for _, tx := range txns.Transactions {
			mids = append(mids, tx.MID)
		}
		if strings.Join(mids, ",") != want {
			t.Errorf("GET /sim/transactions%s holds %v, want %s", scope, mids, want)
		}
	}
	if r := serve(t, h, "GET", "/sim/transactions", ""); strings.Contains(r.Body.String(), `"env"`) {
		t.Errorf("the default's transactions name an environment: %s", r.Body)
	}

	// One cycle for every environment: a file each, under each one's own
	// identifier, in each one's own folder, and each lump sum to its own
	// safeguarding account.
	r := serve(t, h, "POST", "/sim/settlement-cycle/run?slot=morning&date="+day+"&env=*", "")
	var all struct {
		Count int        `json:"count"`
		Runs  []CycleRun `json:"runs"`
	}
	decode(t, r, &all)
	if r.Code != 200 || all.Count != 2 {
		t.Fatalf("POST /sim/settlement-cycle/run?env=*: %d %s", r.Code, r.Body)
	}
	byEnv := map[string]CycleRun{}
	for _, run := range all.Runs {
		byEnv[tenant.Normalize(run.Env)] = run
	}
	def, sbx := byEnv[tenant.Default], byEnv["sbx"]
	if len(def.Published) != 1 || !strings.Contains(def.Published[0], "_Worldline_Settlement_ER_EUR") || len(def.Files) != 1 || def.Files[0].MIDs[0] != "MID-DEFAULT" {
		t.Fatalf("the default's run: %+v", def)
	}
	if len(sbx.Published) != 1 || !strings.Contains(sbx.Published[0], "_Infinite_Payment_ER_EUR") || len(sbx.Files) != 1 || sbx.Files[0].MIDs[0] != "MID-SBX" {
		t.Fatalf("sbx's run: %+v", sbx)
	}
	defFile, sbxFile := def.Published[0], sbx.Published[0]
	for _, c := range []struct {
		root, file string
		there      bool
	}{
		{w.acq.root, defFile, true}, {w.acq.root, sbxFile, false},
		{jail, sbxFile, true}, {jail, defFile, false},
	} {
		for _, d := range []string{wlsftp.DirDownload, wlsftp.DirToWLNordic} {
			if got := exists(filepath.Join(c.root, d, c.file)); got != c.there {
				t.Errorf("%s in %s/%s: %v, want %v", c.file, c.root, d, got, c.there)
			}
		}
	}
	if len(def.LumpSums) != 1 || def.LumpSums[0].AccountID != tenant.DefaultSafeguardingEUR || def.LumpSums[0].Status != "credited" {
		t.Errorf("the default's lump sum: %+v", def.LumpSums)
	}
	if want := tenant.SafeguardingAccount("sbx", "EUR"); len(sbx.LumpSums) != 1 || sbx.LumpSums[0].AccountID != want || sbx.LumpSums[0].AmountCents != 7000 {
		t.Errorf("sbx's lump sum: %+v, want to %s", sbx.LumpSums, want)
	}
	got := wires()
	if len(got) != 2 {
		t.Fatalf("%d lump sums reached the bridge, want 2: %+v", len(got), got)
	}
	for _, wr := range got {
		switch wr.body["amount"] {
		case "150.00":
			if wr.env != "" {
				t.Errorf("the default's lump sum names environment %q", wr.env)
			}
		case "70.00":
			if wr.env != "sbx" {
				t.Errorf("sbx's lump sum names environment %q, want sbx", wr.env)
			}
		default:
			t.Errorf("unexpected wire %+v", wr)
		}
	}

	// Each run history is its own.
	var runs struct {
		Count int        `json:"count"`
		Runs  []CycleRun `json:"runs"`
	}
	for scope, want := range map[string]int{"": 1, "?env=sbx": 1, "?env=*": 2} {
		decode(t, serve(t, h, "GET", "/sim/settlement-cycle/runs"+scope, ""), &runs)
		if runs.Count != want {
			t.Errorf("GET /sim/settlement-cycle/runs%s: %d runs, want %d", scope, runs.Count, want)
		}
	}

	// Over SFTP: each login sees only its own files.
	addr := serveSFTP(t, w, "default-secret")
	defUser, err := login(t, addr, "infinitepay", "default-secret")
	if err != nil {
		t.Fatalf("the default's login: %v", err)
	}
	if got := listing(t, defUser, wlsftp.DirDownload); strings.Join(got, ",") != defFile {
		t.Errorf("the default sees %v in download/, want only %s", got, defFile)
	}
	for _, p := range []string{"../environments/sbx/download/" + sbxFile, filepath.Join(jail, "download", sbxFile)} {
		if f, err := defUser.Open(p); err == nil {
			f.Close()
			t.Errorf("the default's login opened sbx's %s", p)
		}
	}
	sbxUser, err := login(t, addr, "sbx-platform", "sbx-secret")
	if err != nil {
		t.Fatalf("sbx's login: %v", err)
	}
	if got := listing(t, sbxUser, wlsftp.DirDownload); strings.Join(got, ",") != sbxFile {
		t.Errorf("sbx sees %v in download/, want only %s", got, sbxFile)
	}
	if _, err := login(t, addr, "sbx-platform", "default-secret"); err == nil {
		t.Error("sbx's username logged in with the default's password")
	}
	// A login no customer has is the default's, as every login was.
	harness, err := login(t, addr, "harness", "default-secret")
	if err != nil {
		t.Fatalf("an unknown username with the default's password: %v", err)
	}
	if got := listing(t, harness, wlsftp.DirDownload); strings.Join(got, ",") != defFile {
		t.Errorf("an unknown login sees %v, want the default's %s", got, defFile)
	}

	// Enrolment: sbx's batch, uploaded over its own login, is boarded in
	// sbx's folder, with MIDs apart from the default's for the same
	// pf_account_id.
	batch := `<enrollment><submerchant><pf_account_id>pf-1001</pf_account_id><legal_name>Acme</legal_name></submerchant></enrollment>`
	f, err := sbxUser.Create(wlsftp.DirToWLNordic + "/INFINITEENROLL_01.20260925.pgp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(batch)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	writeFile(t, filepath.Join(w.acq.root, wlsftp.DirToWLNordic, "INFINITEENROLL_01.20260925.pgp"), batch)
	var desk map[string]any
	decode(t, serve(t, h, "GET", "/sim/enrolment?env=sbx", ""), &desk)
	if waiting, _ := desk["waiting"].([]any); len(waiting) != 1 {
		t.Errorf("sbx's desk: %v", desk)
	}
	if r := serve(t, h, "POST", "/sim/enrolment/reject?env=*", `{"pf_account_id":"pf-1","reason_code":"01"}`); r.Code != 400 {
		t.Errorf("POST /sim/enrolment/reject?env=*: %d, want 400", r.Code)
	}
	if r := serve(t, h, "POST", "/sim/enrolment/process?env=sbx", ""); r.Code != 200 || !strings.Contains(r.Body.String(), `"answered":1`) {
		t.Fatalf("POST /sim/enrolment/process?env=sbx: %d %s", r.Code, r.Body)
	}
	receipt := worldline.ReceiptFilename(1, "20260925")
	if exists(filepath.Join(w.acq.root, wlsftp.DirFromWLNordic, receipt)) {
		t.Fatal("processing sbx's desk answered the default's batch")
	}
	if r := serve(t, h, "POST", "/sim/enrolment/process", ""); r.Code != 200 || !strings.Contains(r.Body.String(), `"answered":1`) {
		t.Fatalf("POST /sim/enrolment/process: %d %s", r.Code, r.Body)
	}
	mid := func(root string) string {
		t.Helper()
		sealed, err := os.ReadFile(filepath.Join(root, wlsftp.DirFromWLNordic, receipt))
		if err != nil {
			t.Fatal(err)
		}
		plain, err := wlsftp.Decrypt(sealed, w.acq.keys.Keyring())
		if err != nil {
			t.Fatal(err)
		}
		rc, err := worldline.ParseReceipt(plain)
		if err != nil || len(rc.Approvals) != 1 {
			t.Fatalf("receipt in %s: %v %+v", root, err, rc)
		}
		return rc.Approvals[0].ProxyMID
	}
	defMID, sbxMID := mid(w.acq.root), mid(jail)
	if defMID != worldline.ProxyMIDFor("6001", "pf-1001") {
		t.Errorf("the default boarded pf-1001 as %s, want %s as before environments", defMID, worldline.ProxyMIDFor("6001", "pf-1001"))
	}
	if sbxMID == defMID || sbxMID != worldline.EnvProxyMID("sbx", "pf-1001") {
		t.Errorf("sbx boarded pf-1001 as %s (the default as %s)", sbxMID, defMID)
	}
	if got := listing(t, sbxUser, wlsftp.DirFromWLNordic); strings.Join(got, ",") != receipt {
		t.Errorf("sbx collects %v from from_WLNORDIC/", got)
	}

	// Activity and last seen are each one's own.
	type activityAnswer struct {
		Logs []struct {
			Events []struct {
				Env     string `json:"env"`
				Summary string `json:"summary"`
			} `json:"events"`
		} `json:"logs"`
	}
	var act activityAnswer
	decode(t, serve(t, h, "GET", "/sim/activity?env=sbx", ""), &act)
	if len(act.Logs) != 1 || len(act.Logs[0].Events) == 0 {
		t.Fatalf("sbx has no activity: %+v", act)
	}
	for _, ev := range act.Logs[0].Events {
		if ev.Env != "sbx" || !strings.Contains(ev.Summary, "sbx-platform") {
			t.Errorf("sbx's activity holds %+v", ev)
		}
	}
	act = activityAnswer{}
	decode(t, serve(t, h, "GET", "/sim/activity", ""), &act)
	for _, ev := range act.Logs[0].Events {
		if ev.Env != "" || strings.Contains(ev.Summary, "sbx-platform") {
			t.Errorf("the default's activity holds %+v", ev)
		}
	}
	for _, st := range w.tenants.customers.Status() {
		if st.LastSeen == nil {
			t.Errorf("%s was never seen", st.Env)
		}
	}

	// A reset of sbx takes sbx's run and files, and leaves the default's.
	r = serve(t, h, "POST", "/sim/reset", `{"scope":"flow","env":"sbx"}`)
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"transactions":1`) || !strings.Contains(r.Body.String(), `"settlement_files":2`) {
		t.Fatalf("POST /sim/reset sbx: %d %s", r.Code, r.Body)
	}
	if exists(filepath.Join(jail, wlsftp.DirDownload, sbxFile)) || !exists(filepath.Join(w.acq.root, wlsftp.DirDownload, defFile)) {
		t.Error("the reset of sbx did not take only sbx's file")
	}
	if len(w.acq.store.All()) != 1 || len(w.acq.history()) != 1 {
		t.Error("the reset of sbx cleared the default's transactions or runs")
	}
	if !exists(filepath.Join(jail, wlsftp.DirFromWLNordic, receipt)) {
		t.Error("a flow reset of sbx took its enrolment receipt")
	}
	act = activityAnswer{}
	decode(t, serve(t, h, "GET", "/sim/activity", ""), &act)
	if len(act.Logs[0].Events) == 0 {
		t.Error("the reset of sbx cleared the default's activity")
	}

	// Deleting sbx takes its jail and everything in it; the default stays,
	// and sbx's login opens nothing any more.
	if r := serve(t, h, "DELETE", "/sim/customers/sbx", ""); r.Code != 200 {
		t.Fatalf("DELETE /sim/customers/sbx: %d %s", r.Code, r.Body)
	}
	if exists(jail) {
		t.Error("sbx's jail survived its deletion")
	}
	if _, err := login(t, addr, "sbx-platform", "sbx-secret"); err == nil {
		t.Error("sbx's login still works after its deletion")
	}
	if r := serve(t, h, "GET", "/sim/transactions?env=sbx", ""); r.Code != 404 {
		t.Errorf("GET /sim/transactions?env=sbx after the deletion: %d", r.Code)
	}
	if len(w.acq.store.All()) != 1 || !exists(filepath.Join(w.acq.root, wlsftp.DirDownload, defFile)) {
		t.Error("deleting sbx touched the default's")
	}
	if c, err := login(t, addr, "infinitepay", "default-secret"); err != nil {
		t.Errorf("the default's login after sbx's deletion: %v", err)
	} else if got := listing(t, c, wlsftp.DirDownload); len(got) != 1 {
		t.Errorf("the default sees %v", got)
	}
}

// A customer put again keeps what it made and takes its new identifier; a
// bad identifier is refused, and the login of a customer whose folder was
// never made opens nothing rather than the default's.
func TestACustomerPutAgainAndARefusedOne(t *testing.T) {
	w := newTestSim(t, "")
	_, ctl := testSurfaces(t, w)
	h := ctl.Handler()
	if r := serve(t, h, "PUT", "/sim/customers/sbx", sbxCustomer); r.Code != 200 {
		t.Fatalf("PUT: %d %s", r.Code, r.Body)
	}
	if r := serve(t, h, "POST", "/sim/transactions?env=sbx", `{"mid":"M","currency":"EUR","amount_cents":100,"date":"2026-09-24"}`); r.Code != 201 {
		t.Fatalf("POST /sim/transactions?env=sbx: %d %s", r.Code, r.Body)
	}
	again := strings.Replace(sbxCustomer, "Infinite_Payment", "Sandbox_Settlement", 1)
	if r := serve(t, h, "PUT", "/sim/customers/sbx", again); r.Code != 200 || !strings.Contains(r.Body.String(), `"created":false`) {
		t.Fatalf("PUT again: %d %s", r.Code, r.Body)
	}
	e, _ := w.at("sbx")
	if n := len(e.acq.store.All()); n != 1 || e.acq.config().Identifier != "Sandbox_Settlement" {
		t.Errorf("put again: %d transaction(s), identifier %q", n, e.acq.config().Identifier)
	}
	// No identifier of its own: the lab's.
	if r := serve(t, h, "PUT", "/sim/customers/dev", `{"worldline":{"username":"dev","password":"pw"}}`); r.Code != 200 {
		t.Fatalf("PUT dev: %d %s", r.Code, r.Body)
	}
	if e, ok := w.at("dev"); !ok || e.acq.config().Identifier != w.acq.config().Identifier {
		t.Errorf("dev's identifier is not the lab's")
	}

	if r := serve(t, h, "PUT", "/sim/customers/bad", `{"worldline":{"username":"bad","password":"pw","settlement_identifier":"../escape"}}`); r.Code != 400 {
		t.Fatalf("PUT with a bad identifier: %d %s, want 400", r.Code, r.Body)
	}
	if _, ok := w.at("bad"); ok {
		t.Error("a refused customer has an acquirer")
	}
	accts := w.sftpAccounts("bad")
	if len(accts) != 1 || accts[0].Password != "" {
		t.Errorf("a refused customer's login: %+v, want named and opening nothing", accts)
	}
	addr := serveSFTP(t, w, "")
	if _, err := login(t, addr, "bad", "pw"); err == nil {
		t.Error("a refused customer's login opened a session")
	}
	if _, err := login(t, addr, "anyone", "anything"); err != nil {
		t.Errorf("with no password set, an unknown login is still let in as the default's: %v", err)
	}
	if c, err := login(t, addr, "dev", "pw"); err != nil {
		t.Errorf("dev's login: %v", err)
	} else if f, err := c.Create("download/x"); err != nil {
		t.Error(err)
	} else {
		f.Close()
		if !exists(filepath.Join(w.tenants.root, "dev", "download", "x")) {
			t.Error("dev's write did not land in dev's jail")
		}
	}
}

// The reset of "*" reaches every environment, and the default's reset only
// the default.
func TestResetNamesItsEnvironment(t *testing.T) {
	w := newTestSim(t, "")
	_, ctl := testSurfaces(t, w)
	h := ctl.Handler()
	if r := serve(t, h, "PUT", "/sim/customers/sbx", sbxCustomer); r.Code != 200 {
		t.Fatalf("PUT: %d %s", r.Code, r.Body)
	}
	seed := func() {
		t.Helper()
		for _, q := range []string{"", "?env=sbx"} {
			if r := serve(t, h, "POST", "/sim/transactions"+q, `{"mid":"M","currency":"EUR","amount_cents":100,"date":"2026-09-24"}`); r.Code != 201 {
				t.Fatalf("seed%s: %d %s", q, r.Code, r.Body)
			}
		}
	}
	e, _ := w.at("sbx")
	seed()
	if r := serve(t, h, "POST", "/sim/reset", `{}`); r.Code != 200 || !strings.Contains(r.Body.String(), `"transactions":1`) {
		t.Fatalf("reset of the default: %d %s", r.Code, r.Body)
	}
	if len(w.acq.store.All()) != 0 || len(e.acq.store.All()) != 1 {
		t.Error("the default's reset did not reset the default alone")
	}
	seed()
	if r := serve(t, h, "POST", "/sim/reset", `{"scope":"all","env":"*"}`); r.Code != 200 || !strings.Contains(r.Body.String(), `"transactions":3`) {
		t.Fatalf("reset of every environment: %d %s", r.Code, r.Body)
	}
	if len(w.acq.store.All()) != 0 || len(e.acq.store.All()) != 0 {
		t.Error("the reset of * left transactions")
	}
	if r := serve(t, h, "POST", "/sim/reset", `{"env":"nope"}`); r.Code != 404 {
		t.Errorf("reset of an unknown environment: %d %s", r.Code, r.Body)
	}
}

// WORLDLINE_SFTP_ENV_ROOT may not overlap the default's root, either way.
func TestTheEnvironmentRootIsOutsideTheDefaults(t *testing.T) {
	if got := defaultEnvRoot("/wlsftp/"); got != "/wlsftp-environments" {
		t.Errorf("defaultEnvRoot = %q", got)
	}
	for _, c := range []struct {
		root, envRoot string
		ok            bool
	}{
		{"/wlsftp", "/wlsftp-environments", true},
		{"/wlsftp", "/wlsftp", false},
		{"/wlsftp", "/wlsftp/environments", false},
		{"/data/wlsftp", "/data", false},
		{"/a/wlsftp", "/b/environments", true},
	} {
		if err := checkEnvRoot(c.root, c.envRoot); (err == nil) != c.ok {
			t.Errorf("checkEnvRoot(%s, %s) = %v", c.root, c.envRoot, err)
		}
	}
}
