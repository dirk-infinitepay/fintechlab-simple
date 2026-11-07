package main

import (
	"bytes"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/webduvet/fintechlab-simple/internal/b4b"
)

// listeners is b4b as main serves it: the vendor port, the control port,
// and a bearer token the vendor port takes.
type listeners struct {
	vendor, control *httptest.Server
	token           string
}

// signingKey is a key pair as b4b generates one on first start.
func signingKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := b4b.LoadOrGenerateKeyPair(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// serve puts a behind both listeners, verifying tokens against key.
func serve(t *testing.T, a *app, key *rsa.PrivateKey) listeners {
	t.Helper()
	// Where the control port listens and who may call it come from the
	// environment; the one the tests run in must not leak in.
	t.Setenv("SIM_CONTROL_LISTEN", "")
	t.Setenv("SIM_CONTROL_ALLOW", "")
	a.pubKey = &key.PublicKey
	tok, err := b4b.SignBearerToken(key, a.kid)
	if err != nil {
		t.Fatal(err)
	}
	ctl, err := a.control(":8086")
	if err != nil {
		t.Fatal(err)
	}
	l := listeners{
		vendor:  httptest.NewServer(a.routes(ctl.Mux)),
		control: httptest.NewServer(ctl.Handler()),
		token:   tok,
	}
	t.Cleanup(l.vendor.Close)
	t.Cleanup(l.control.Close)
	return l
}

// send makes one request, signed when token is not empty, and returns the
// answer as it came.
func send(t *testing.T, srv *httptest.Server, method, path, body, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

func object(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("not a JSON object: %s", b)
	}
	return out
}

func (p *peer) received() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.bodies)
}

// simCall is one call to a /sim route that succeeds on what boardForSim
// boarded. want 0 takes any answer the route itself gives.
type simCall struct {
	method, path, body string
	want               int
}

// simCalls is every /sim route b4b serves, the reset last: it clears the
// rest.
func simCalls(personID string) []simCall {
	return []simCall{
		{"GET", "/sim/info", "", 200},
		{"GET", "/sim/logs", "", 0}, // 200 with a log ring, 501 without
		{"GET", "/sim/activity", "", 200},
		{"POST", "/sim/beneficiaries", `{"external_ref": "MID002", "account_name": "Second Outlet Ltd", "account_number": "GB00SIM0000000000043", "financial_institution": "SC112233"}`, 201},
		{"PUT", "/sim/beneficiaries/MID001/sanctions", `{"sanctions_status": "review"}`, 200},
		{"PUT", "/sim/beneficiaries/MID001/status", `{"status": "active"}`, 200},
		{"PUT", "/sim/companies/mer_0001/sanctions", `{"sanctions_status": "review"}`, 200},
		{"PUT", "/sim/people/" + personID + "/sanctions", `{"pep_sanctions_status": "review"}`, 200},
		{"GET", "/sim/traps/hooks", "", 200},
		{"POST", "/sim/traps", `{"hook": "payment.create", "match": {"beneficiary_id": "MID009"}, "effect": {"path": [{"to": "B4BTMPending"}]}}`, 201},
		{"GET", "/sim/traps", "", 200},
		{"GET", "/sim/traps/trp_0001", "", 200},
		{"DELETE", "/sim/traps/trp_0001", "", 200},
		{"DELETE", "/sim/traps", "", 200},
		{"POST", "/sim/reset", `{"scope": "all"}`, 200},
	}
}

// answered reports whether st is the route's own answer, not a refusal at
// the door or a route that is not there.
func (c simCall) answered(st int) bool {
	if c.want != 0 {
		return st == c.want
	}
	return st != 401 && st != 404 && st != 405
}

// boardForSim is board plus a person, so every /sim setter has something
// to set. It returns the person's id.
func boardForSim(t *testing.T, a *app) string {
	t.Helper()
	board(t, a)
	p, err := a.dir.AddPerson("mer_0001", b4b.PersonParams{FirstName: "Ada", LastName: "Lovelace", DateOfBirth: "1815-12-10"})
	if err != nil {
		t.Fatal(err)
	}
	return p.ID
}

// The control port serves every /sim route with no bearer token, and
// nothing a platform calls.
func TestTheControlPortServesEverySimRouteWithoutAToken(t *testing.T) {
	cb, bc := newPeer(t), newPeer(t)
	a := testApp(t, cb, bc, "", 0)
	l := serve(t, a, signingKey(t))
	person := boardForSim(t, a)

	st, body := send(t, l.control, "GET", "/health", "", "")
	if h := object(t, body); st != 200 || h["service"] != "b4b" || h["surface"] != "control" {
		t.Errorf("GET /health on the control port = %d %s", st, body)
	}
	st, body = send(t, l.control, "GET", "/sim/info", "", "")
	if info := object(t, body); st != 200 || info["service"] != "b4b" || info["kind"] != "vendor" ||
		info["control_listener"] != ":9086" || fmt.Sprint(info["vendor_listeners"]) != "[:8086]" {
		t.Errorf("GET /sim/info = %d %s", st, body)
	}
	for _, c := range simCalls(person) {
		if st, body := send(t, l.control, c.method, c.path, c.body, ""); !c.answered(st) {
			t.Errorf("%s %s on the control port, no token = %d %s", c.method, c.path, st, body)
		}
	}
	for _, r := range [][2]string{{"GET", "/oversight/v1/companies"}, {"POST", "/oversight/v1/payments"}, {"POST", "/oversight/v1/beneficiaries"}} {
		if st, _ := send(t, l.control, r[0], r[1], "{}", l.token); st != 404 {
			t.Errorf("%s %s on the control port = %d, want 404: it is the vendor's", r[0], r[1], st)
		}
	}
	idle(t, a)
}

// Until phase 4 the old paths answer on the vendor port as they did: every
// /sim route behind the bearer JWT, but GET /sim/activity, which never
// asked for one.
func TestTheOldSimPathsOnTheVendorPortStillWantTheJWT(t *testing.T) {
	cb, bc := newPeer(t), newPeer(t)
	a := testApp(t, cb, bc, "", 0)
	l := serve(t, a, signingKey(t))
	person := boardForSim(t, a)

	for _, c := range simCalls(person) {
		want := 401
		if c.path == "/sim/activity" {
			want = 200
		}
		if st, body := send(t, l.vendor, c.method, c.path, c.body, ""); st != want {
			t.Errorf("%s %s on the vendor port, no token = %d %s, want %d", c.method, c.path, st, body, want)
		}
	}
	if st, _ := send(t, l.vendor, "PUT", "/sim/beneficiaries/MID001/sanctions", `{"sanctions_status": "fail"}`, "not.a.jwt"); st != 401 {
		t.Errorf("PUT /sim/beneficiaries/MID001/sanctions with a bad token = %d, want 401", st)
	}
	// A refused call changed nothing.
	if ben := a.beneficiaries.Get("MID001"); ben.SanctionsStatus != b4b.SanctionsPass || ben.Status != b4b.StatusDisabled {
		t.Fatalf("after refused calls MID001 is %s/%s, want pass/disabled as boarded", ben.SanctionsStatus, ben.Status)
	}
	if n := len(cb.received()); n != 0 {
		t.Fatalf("refused calls sent %d callback(s)", n)
	}

	for _, c := range simCalls(person) {
		if st, body := send(t, l.vendor, c.method, c.path, c.body, l.token); !c.answered(st) {
			t.Errorf("%s %s on the vendor port, signed = %d %s", c.method, c.path, st, body)
		}
	}
	idle(t, a)
}

// POST /sim/beneficiaries is POST /oversight/v1/beneficiaries without the
// JWT: for the same body and the same state, the same answer byte for
// byte, refusals included, the same callbacks and the same state file.
func TestRegisteringOnTheControlPortIsTheOversightRegistration(t *testing.T) {
	key := signingKey(t)
	type side struct {
		a    *app
		l    listeners
		cb   *peer
		path string
	}
	newSide := func() side {
		cb, bc := newPeer(t), newPeer(t)
		path := filepath.Join(t.TempDir(), "state.json")
		a := testApp(t, cb, bc, path, 0)
		// Ids in sequence, so two apps given the same calls mint the same.
		var nd, nb int
		a.dir = b4b.NewDirectory(func(prefix string) string { nd++; return fmt.Sprintf("%s_%d", prefix, nd) })
		a.dir.OnChange = a.persist
		a.beneficiaries = b4b.NewBeneficiaryStore(func() string { nb++; return fmt.Sprintf("ben_%d", nb) })
		a.beneficiaries.OnChange = a.persist
		if _, err := a.dir.CreateCompany(b4b.CompanyParams{
			ExternalRef: "mer_0001", LegalName: "Southwind Coffee Ltd", CompanyType: "kyb",
			Address: &b4b.Address{AddressLine1: "1 Simulation Way", City: "London", PostalCode: "EC1A 1AA", Country: "GB"},
		}); err != nil {
			t.Fatal(err)
		}
		return side{a, serve(t, a, key), cb, path}
	}
	oversight, control := newSide(), newSide()

	cases := []struct {
		name, body string
		want       int
	}{
		{"a payee of a boarded company", `{"company_id": "mer_0001", "external_ref": "MID0001", "account_name": "Southwind Coffee Ltd", "account_number": "GB00SIM0000000000042", "financial_institution": "SC112233", "country": "GB"}`, 201},
		{"a required field missing", `{"external_ref": "MID0002", "account_number": "GB00SIM0000000000043", "financial_institution": "SC112233"}`, 400},
		{"a sanctions status that is not one", `{"account_name": "A Ltd", "account_number": "1", "financial_institution": "2", "sanctions_status": "maybe"}`, 400},
		{"a callback outside the allowlist", `{"account_name": "A Ltd", "account_number": "1", "financial_institution": "2", "callback_url": "http://elsewhere.example/cb"}`, 400},
		{"a company never boarded", `{"company_id": "cmp_never", "account_name": "A Ltd", "account_number": "1", "financial_institution": "2"}`, 404},
		{"not JSON", `{"account_name": `, 400},
		{"blocked at registration, with a field b4b keeps no column for", `{"external_ref": "MID0003", "account_name": "Blocked Ltd", "account_number": "GB00SIM0000000000099", "financial_institution": "SC112233", "sanctions_status": "fail", "merchant_category_code": "5814"}`, 201},
		{"the same external_ref again", `{"external_ref": "MID0001", "account_name": "Southwind Coffee Ltd", "account_number": "GB00SIM0000000000042", "financial_institution": "SC112233"}`, 201},
	}
	for _, c := range cases {
		vst, vbody := send(t, oversight.l.vendor, "POST", "/oversight/v1/beneficiaries", c.body, oversight.l.token)
		cst, cbody := send(t, control.l.control, "POST", "/sim/beneficiaries", c.body, "")
		if vst != c.want {
			t.Errorf("%s: POST /oversight/v1/beneficiaries = %d %s, want %d", c.name, vst, vbody, c.want)
		}
		if cst != vst || !bytes.Equal(cbody, vbody) {
			t.Errorf("%s: the control port answered %d %s\nthe Oversight API %d %s", c.name, cst, cbody, vst, vbody)
		}
	}

	idle(t, oversight.a)
	idle(t, control.a)
	vcb, ccb := oversight.cb.received(), control.cb.received()
	slices.Sort(vcb)
	slices.Sort(ccb)
	if len(vcb) != 3 || !slices.Equal(vcb, ccb) {
		t.Errorf("callbacks: the Oversight API sent %d %v\nthe control port %d %v", len(vcb), vcb, len(ccb), ccb)
	}

	saved := func(s side) string {
		st, err := b4b.LoadState(s.path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(st.Beneficiaries)
		if len(st.Beneficiaries) != 3 {
			t.Errorf("%d beneficiaries in the state file, want 3", len(st.Beneficiaries))
		}
		return string(b)
	}
	if v, c := saved(oversight), saved(control); v != c {
		t.Errorf("state file: the Oversight API saved %s\nthe control port %s", v, c)
	}
}
