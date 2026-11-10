package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/allowlist"
	"github.com/webduvet/fintechlab-simple/internal/b4b"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// bridgeCall is one call b4b made to Banking Circle's bridge, with the
// environment it said it was for.
type bridgeCall struct{ env, body string }

// bridge is a stand-in for Banking Circle's bridge that keeps the
// Lab-Environment header of each call.
type bridge struct {
	mu    sync.Mutex
	calls []bridgeCall
	srv   *httptest.Server
}

func newBridge(t *testing.T) *bridge {
	b := &bridge{}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.calls = append(b.calls, bridgeCall{env: r.Header.Get(tenant.Header), body: string(body)})
		b.mu.Unlock()
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *bridge) received() []bridgeCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.calls)
}

// envLab is b4b serving two environments: the default, signing with b4b's
// own key and called back at cb, and sbx, a customer with a key of its own,
// called back at sbxCB.
type envLab struct {
	a         *app
	l         listeners
	sbxToken  string
	cb, sbxCB *peer
	bc        *bridge
	path      string
}

func newEnvLab(t *testing.T, delay time.Duration) *envLab {
	t.Helper()
	cb, bc := newPeer(t), newBridge(t)
	path := filepath.Join(t.TempDir(), "state.json")
	a := testApp(t, cb, newPeer(t), path, delay)
	a.bcURL = bc.srv.URL
	lab := &envLab{a: a, l: serve(t, a, signingKey(t)), cb: cb, sbxCB: newPeer(t), bc: bc, path: path}
	lab.sbxToken = lab.push(t, "sbx", "sbx-key-1", lab.sbxCB.srv.URL, nil)
	return lab
}

// push sends env's customer to the control port, as the lab API does, with
// a key made for it, and returns a token signed with that key.
func (lab *envLab) push(t *testing.T, env, kid, callbackURL string, hosts []string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if st, b := lab.putCustomer(t, env, kid, &key.PublicKey, callbackURL, hosts); st != 200 {
		t.Fatalf("PUT /sim/customers/%s = %d %s", env, st, b)
	}
	tok, err := b4b.SignBearerToken(key, kid)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (lab *envLab) putCustomer(t *testing.T, env, kid string, pub *rsa.PublicKey, callbackURL string, hosts []string) (int, []byte) {
	t.Helper()
	pem, err := b4b.PublicKeyPEM(pub)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(tenant.Customer{Env: env, Name: env,
		B4B: &tenant.B4BKey{KeyID: kid, PublicKeyPEM: pem, CallbackURL: callbackURL}, WebhookHosts: hosts})
	return send(t, lab.l.control, "PUT", "/sim/customers/"+env, string(body), "")
}

// call is one vendor call, which must answer want; it returns the body.
func (lab *envLab) call(t *testing.T, method, path, body, token string, want int) map[string]any {
	t.Helper()
	st, b := send(t, lab.l.vendor, method, path, body, token)
	if st != want {
		t.Fatalf("%s %s = %d %s, want %d", method, path, st, b, want)
	}
	if strings.Contains(string(b), `"env"`) {
		t.Errorf("%s %s answered with the owner on the wire: %s", method, path, b)
	}
	return object(t, b)
}

// board boards a company with external_ref ref and a payee MID001 under
// it, as the environment token signs for, and returns the two ids.
func (lab *envLab) board(t *testing.T, token, ref string) (company, beneficiary string) {
	t.Helper()
	c := lab.call(t, "POST", "/oversight/v1/companies", `{"external_ref": "`+ref+`", "legal_name": "Southwind Coffee Ltd",
		"address": {"address_line_1": "1 Simulation Way", "city": "London", "postal_code": "EC1A 1AA", "country": "GB"}}`, token, 201)
	b := lab.call(t, "POST", "/oversight/v1/beneficiaries", fmt.Sprintf(`{"company_id": %q, "external_ref": "MID001",
		"account_name": "Southwind Coffee Ltd", "account_number": "GB00SIM0000000000042", "financial_institution": "SC112233"}`, c["id"]), token, 201)
	return c["id"].(string), b["id"].(string)
}

// pay sends a payout to MID001 as the token's environment, and returns its id.
func (lab *envLab) pay(t *testing.T, token, ref string) string {
	t.Helper()
	p := lab.call(t, "POST", "/oversight/v1/payments", `{"external_ref": "`+ref+`", "beneficiary_id": "MID001",
		"amount": {"amount": 12.34, "currency": "EUR"}, "currencyOfTransfer": "EUR", "debtorViban": {"account": "GB00SIM0000000000001"},
		"creditorAccount": {"account": "GB00SIM0000000000042", "financialInstitution": "SC112233"}, "chargeBearer": "SHA"}`, token, 202)
	return p["id"].(string)
}

func (lab *envLab) companies(t *testing.T, token string) []string {
	t.Helper()
	var out []string
	for _, c := range lab.call(t, "GET", "/oversight/v1/companies", "", token, 200)["companies"].([]any) {
		out = append(out, c.(map[string]any)["id"].(string))
	}
	return out
}

// Each environment is the customer its key names: it boards, lists, reads
// and pays only its own, the same external_ref is free in both, its
// callbacks go to its own endpoint, and the wire never shows whose a
// record is.
func TestEachEnvironmentActsAsTheOneItsKeyNames(t *testing.T) {
	lab := newEnvLab(t, 0)
	defCompany, defBen := lab.board(t, lab.l.token, "mer_0001")
	sbxCompany, sbxBen := lab.board(t, lab.sbxToken, "mer_0001")
	if defCompany == sbxCompany || defBen == sbxBen {
		t.Fatalf("two environments boarding the same refs got the same ids: %s %s", defCompany, defBen)
	}
	lab.call(t, "POST", "/oversight/v1/companies", `{"external_ref": "mer_0001", "legal_name": "Again Ltd",
		"address": {"address_line_1": "1 Simulation Way", "city": "London", "country": "GB"}}`, lab.sbxToken, 422)

	if got := lab.companies(t, lab.l.token); !slices.Equal(got, []string{defCompany}) {
		t.Errorf("the default lists %v, want only %s", got, defCompany)
	}
	if got := lab.companies(t, lab.sbxToken); !slices.Equal(got, []string{sbxCompany}) {
		t.Errorf("sbx lists %v, want only %s", got, sbxCompany)
	}
	for _, path := range []string{"", "/addresses", "/people", "/vibans", "/extended"} {
		lab.call(t, "GET", "/oversight/v1/companies/"+defCompany+path, "", lab.sbxToken, 404)
	}
	if c := lab.call(t, "GET", "/oversight/v1/companies/mer_0001", "", lab.sbxToken, 200); c["id"] != sbxCompany {
		t.Errorf("sbx reads mer_0001 as %v, want its own %s", c["id"], sbxCompany)
	}
	if b := lab.call(t, "GET", "/oversight/v1/beneficiaries/MID001", "", lab.sbxToken, 200); b["id"] != sbxBen {
		t.Errorf("sbx reads MID001 as %v, want its own %s", b["id"], sbxBen)
	}
	// Another environment's payee reads as an unknown one: synthetic.
	if b := lab.call(t, "GET", "/oversight/v1/beneficiaries/"+defBen, "", lab.sbxToken, 200); b["company_id"] != nil ||
		!strings.HasPrefix(b["account_name"].(string), "B4B Merchant ") {
		t.Errorf("sbx reads the default's payee %s as %v", defBen, b)
	}
	person := lab.call(t, "POST", "/oversight/v1/companies/"+sbxCompany+"/people", `{"entity_type": "legal_entity", "legal_name": "Holdco Ltd"}`, lab.sbxToken, 201)
	lab.call(t, "GET", "/oversight/v1/companies/"+sbxCompany+"/people/"+person["id"].(string), "", lab.l.token, 404)
	doc := lab.call(t, "POST", "/oversight/v1/uploads", `{"document_type": "extract", "file_name": "a.pdf"}`, lab.sbxToken, 201)
	lab.call(t, "GET", "/oversight/v1/uploads/"+doc["id"].(string), "", lab.l.token, 404)
	lab.call(t, "POST", "/oversight/v1/companies/"+defCompany+"/extended",
		`{"registered_company_no": "1", "documents": ["`+doc["id"].(string)+`"]}`, lab.l.token, 400)

	sbxPay := lab.pay(t, lab.sbxToken, "stl_sbx_1")
	defPay := lab.pay(t, lab.l.token, "stl_def_1")
	idle(t, lab.a)
	lab.call(t, "GET", "/oversight/v1/payments/"+sbxPay, "", lab.l.token, 404)
	if p := lab.call(t, "GET", "/oversight/v1/payments/"+sbxPay, "", lab.sbxToken, 200); p["status"] != string(b4b.StateTMApproved) {
		t.Errorf("sbx's payment = %v", p)
	}
	lab.call(t, "GET", "/oversight/v1/payments/"+defPay, "", lab.sbxToken, 404)

	// Callbacks: each environment's to its own endpoint, about its own.
	onlyAbout := func(who string, p *peer, mine, theirs []string) {
		t.Helper()
		got := strings.Join(p.received(), "\n")
		for _, id := range mine {
			if !strings.Contains(got, id) {
				t.Errorf("%s was not called back about %s", who, id)
			}
		}
		for _, id := range theirs {
			if strings.Contains(got, id) {
				t.Errorf("%s was called back about %s, the other environment's", who, id)
			}
		}
	}
	onlyAbout("the default", lab.cb, []string{defBen, defPay}, []string{sbxBen, sbxPay, person["id"].(string)})
	onlyAbout("sbx", lab.sbxCB, []string{sbxBen, sbxPay, person["id"].(string)}, []string{defBen, defPay})
	if n := lab.sbxCB.count(); n != 7 {
		t.Errorf("sbx was called back %d times, want its payee, its person and five for its payout", n)
	}

	// The activity of each, apart.
	_, b := send(t, lab.l.control, "GET", "/sim/activity?env=sbx", "", "")
	if s := string(b); !strings.Contains(s, sbxPay) || strings.Contains(s, defPay) {
		t.Errorf("sbx's activity: %s", b)
	}
	_, b = send(t, lab.l.control, "GET", "/sim/activity", "", "")
	if s := string(b); strings.Contains(s, sbxPay) || !strings.Contains(s, defPay) {
		t.Errorf("the default's activity: %s", b)
	}

	// A kid nobody holds is refused, as a bad signature is.
	stranger, _ := rsa.GenerateKey(rand.Reader, 2048)
	tok, _ := b4b.SignBearerToken(stranger, "nobody")
	lab.call(t, "GET", "/oversight/v1/companies", "", tok, 401)
	// sbx's kid with another key is refused too.
	tok, _ = b4b.SignBearerToken(stranger, "sbx-key-1")
	lab.call(t, "GET", "/oversight/v1/companies", "", tok, 401)

	// GET /sim/customers says when each was last seen.
	_, b = send(t, lab.l.control, "GET", "/sim/customers", "", "")
	var cs struct{ Customers []tenant.Status }
	if json.Unmarshal(b, &cs) != nil || len(cs.Customers) != 2 || cs.Customers[1].Env != "sbx" || cs.Customers[1].LastSeen == nil || cs.Customers[0].LastSeen == nil {
		t.Errorf("GET /sim/customers = %s", b)
	}
}

// A key id is one environment's: the default's and another customer's are
// refused, and so is a key that is not one. A refused push leaves the
// environment's key as it was.
func TestAKeyIDNamesOneEnvironment(t *testing.T) {
	lab := newEnvLab(t, 0)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	for kid, want := range map[string]string{lab.a.kid: "default environment's", "sbx-key-1": "environment sbx's", "": "key_id is required"} {
		if st, b := lab.putCustomer(t, "other", kid, &key.PublicKey, "", nil); st != 400 || !strings.Contains(string(b), want) {
			t.Errorf("kid %q for another environment = %d %s", kid, st, b)
		}
	}
	if st, b := send(t, lab.l.control, "PUT", "/sim/customers/sbx", `{"b4b": {"key_id": "sbx-key-2", "public_key_pem": "not a key"}}`, ""); st != 400 {
		t.Errorf("a key that is not one = %d %s", st, b)
	}
	lab.call(t, "GET", "/oversight/v1/companies", "", lab.sbxToken, 200)
	if st, b := lab.putCustomer(t, "sbx", "sbx-key-1", &key.PublicKey, "http://elsewhere.example/cb", nil); st != 400 || !strings.Contains(string(b), "callback_url") {
		t.Errorf("a callback_url outside the allowlist = %d %s", st, b)
	}
}

// The bridge call says whose payout it is: sbx's carries Lab-Environment,
// the default's carries nothing new.
func TestThePayoutBridgeSaysWhoseItIs(t *testing.T) {
	lab := newEnvLab(t, 0)
	lab.board(t, lab.l.token, "mer_0001")
	lab.board(t, lab.sbxToken, "mer_0001")
	lab.pay(t, lab.sbxToken, "stl_sbx")
	idle(t, lab.a)
	lab.pay(t, lab.l.token, "stl_def")
	idle(t, lab.a)
	calls := lab.bc.received()
	if len(calls) != 2 {
		t.Fatalf("bridge calls = %+v, want two", calls)
	}
	for _, c := range calls {
		switch {
		case strings.Contains(c.body, `"externalRef":"stl_sbx"`) && c.env == "sbx":
		case strings.Contains(c.body, `"externalRef":"stl_def"`) && c.env == "":
		default:
			t.Errorf("bridge call %s said %s=%q", c.body, tenant.Header, c.env)
		}
	}
}

// A reset names an environment and clears its records only: sbx's all
// reset leaves the default's boarding and payments, and the default's
// reset does not stop sbx's payout walking its phases.
func TestAResetOfOneEnvironmentKeepsTheOthers(t *testing.T) {
	lab := newEnvLab(t, 30*time.Millisecond)
	defCompany, _ := lab.board(t, lab.l.token, "mer_0001")
	lab.board(t, lab.sbxToken, "mer_0001")
	defPay := lab.pay(t, lab.l.token, "stl_def")
	idle(t, lab.a)

	// The default's reset while sbx's payout walks: sbx's goes on to the
	// bridge, its callbacks all made.
	sbxPay := lab.pay(t, lab.sbxToken, "stl_sbx")
	st, b := send(t, lab.l.control, "POST", "/sim/reset", `{"scope": "flow"}`, "")
	if cleared := object(t, b)["cleared"].(map[string]any); st != 200 || cleared["payments"] != float64(1) {
		t.Fatalf("the default's flow reset = %d %s, want its one payment", st, b)
	}
	idle(t, lab.a)
	if p := lab.call(t, "GET", "/oversight/v1/payments/"+sbxPay, "", lab.sbxToken, 200); p["status"] != string(b4b.StateTMApproved) {
		t.Fatalf("sbx's payment after the default's reset = %v", p["status"])
	}
	if calls := lab.bc.received(); len(calls) != 2 || calls[1].env != "sbx" {
		t.Fatalf("bridge calls = %+v, want sbx's made after the default's reset", calls)
	}
	lab.call(t, "GET", "/oversight/v1/payments/"+defPay, "", lab.l.token, 404)

	// sbx's all reset: sbx is as on first start, the default keeps its
	// boarding, and the same ref boards again in sbx.
	st, b = send(t, lab.l.control, "POST", "/sim/reset", `{"scope": "all", "env": "sbx"}`, "")
	res := object(t, b)
	if cleared := res["cleared"].(map[string]any); st != 200 || res["env"] != "sbx" ||
		cleared["companies"] != float64(1) || cleared["beneficiaries"] != float64(1) || cleared["payments"] != float64(1) {
		t.Fatalf("sbx's all reset = %d %s", st, b)
	}
	if got := lab.companies(t, lab.sbxToken); len(got) != 0 {
		t.Errorf("sbx lists %v after its all reset", got)
	}
	if got := lab.companies(t, lab.l.token); !slices.Equal(got, []string{defCompany}) {
		t.Errorf("the default lists %v after sbx's reset, want %s", got, defCompany)
	}
	lab.call(t, "GET", "/oversight/v1/payments/"+sbxPay, "", lab.sbxToken, 404)
	lab.board(t, lab.sbxToken, "mer_0001")

	s, err := b4b.LoadState(lab.path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Companies) != 2 || len(s.Beneficiaries) != 2 || len(s.Payments) != 0 {
		t.Errorf("the state file after both resets: %d companies, %d beneficiaries, %d payments", len(s.Companies), len(s.Beneficiaries), len(s.Payments))
	}
	if st, b := send(t, lab.l.control, "POST", "/sim/reset", `{"scope": "all", "env": "nope"}`, ""); st != 404 {
		t.Errorf("a reset of an environment b4b does not serve = %d %s", st, b)
	}
}

// Deleting an environment drops everything it made, in memory and in the
// state file, and its key with it; the default's is untouched.
func TestDeletingAnEnvironmentDropsWhatItMade(t *testing.T) {
	lab := newEnvLab(t, 0)
	defCompany, _ := lab.board(t, lab.l.token, "mer_0001")
	lab.board(t, lab.sbxToken, "mer_0001")
	lab.pay(t, lab.sbxToken, "stl_sbx")
	idle(t, lab.a)
	if st, b := send(t, lab.l.control, "POST", "/sim/traps?env=sbx", `{"hook": "payment.create", "match": {}, "effect": {"path": []}}`, ""); st != 201 {
		t.Fatalf("arming a trap in sbx = %d %s", st, b)
	}

	if st, b := send(t, lab.l.control, "DELETE", "/sim/customers/sbx", "", ""); st != 200 {
		t.Fatalf("DELETE /sim/customers/sbx = %d %s", st, b)
	}
	lab.call(t, "GET", "/oversight/v1/companies", "", lab.sbxToken, 401)
	if got := lab.companies(t, lab.l.token); !slices.Equal(got, []string{defCompany}) {
		t.Errorf("the default lists %v after sbx went", got)
	}
	if n := len(lab.a.traps.List("")); n != 0 {
		t.Errorf("%d trap(s) after sbx went", n)
	}
	if n := len(lab.a.cbLog.RecentIn("sbx", 0)); n != 0 {
		t.Errorf("%d of sbx's callback events after it went", n)
	}
	s, err := b4b.LoadState(lab.path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Companies) != 1 || len(s.Addresses) != 1 || len(s.Beneficiaries) != 1 || len(s.Payments) != 0 || s.Companies[0].ID != defCompany {
		t.Errorf("the state file after sbx went: %+v", s)
	}
	if st, _ := send(t, lab.l.control, "DELETE", "/sim/customers/sbx", "", ""); st != 404 {
		t.Errorf("deleting sbx again = %d, want 404", st)
	}
}

// The state file keeps each record's owner, so a restart gives each
// environment back its own; and a file from before environments, with no
// owners at all, is the default's.
func TestTheStateFileKeepsWhoseEachRecordIs(t *testing.T) {
	lab := newEnvLab(t, 0)
	lab.board(t, lab.l.token, "mer_0001")
	sbxCompany, sbxBen := lab.board(t, lab.sbxToken, "mer_0002")
	sbxPay := lab.pay(t, lab.sbxToken, "stl_sbx")
	idle(t, lab.a)

	s, err := b4b.LoadState(lab.path)
	if err != nil {
		t.Fatal(err)
	}
	owners := map[string]string{}
	for _, c := range s.Companies {
		owners[c.ExternalRef] = c.Env
	}
	if owners["mer_0001"] != "" || owners["mer_0002"] != "sbx" {
		t.Errorf("company owners in the state file = %v, want the default's left out and sbx's named", owners)
	}
	raw, _ := os.ReadFile(lab.path)
	if strings.Count(string(raw), `"env": "sbx"`) != 3 {
		t.Errorf("want sbx named on its company, beneficiary and payment:\n%s", raw)
	}

	// A restart: the same file, a fresh b4b, and the lab API pushing sbx
	// again.
	again := testApp(t, lab.cb, newPeer(t), lab.path, 0)
	again.dir.Restore(s)
	again.beneficiaries.Restore(s)
	again.engine.Restore(s)
	l := serve(t, again, signingKey(t))
	relab := &envLab{a: again, l: l, sbxCB: lab.sbxCB}
	relab.sbxToken = relab.push(t, "sbx", "sbx-key-1", lab.sbxCB.srv.URL, nil)
	if got := relab.companies(t, relab.sbxToken); !slices.Equal(got, []string{sbxCompany}) {
		t.Errorf("sbx lists %v after a restart, want %s", got, sbxCompany)
	}
	if b := relab.call(t, "GET", "/oversight/v1/beneficiaries/MID001", "", relab.sbxToken, 200); b["id"] != sbxBen {
		t.Errorf("sbx's MID001 after a restart = %v, want %s", b["id"], sbxBen)
	}
	relab.call(t, "GET", "/oversight/v1/payments/"+sbxPay, "", relab.sbxToken, 200)
	relab.call(t, "GET", "/oversight/v1/payments/"+sbxPay, "", relab.l.token, 404)

	// A state file written before environments.
	old := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(old, []byte(`{
  "companies": [{"id": "cmp_old", "external_ref": "mer_old", "legal_name": "Old Ltd", "status": "active", "sanctions_status": "pass", "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z"}],
  "addresses": [{"id": "adr_old", "company_id": "cmp_old", "type": "registered", "address_line_1": "1 Old Road", "city": "London", "postal_code": "", "country": "GB", "created_at": "2026-01-01T00:00:00Z"}],
  "beneficiaries": [{"id": "ben_old", "external_ref": "MIDOLD", "account_name": "Old Ltd", "account_number": "GB00SIM0000000000777", "financial_institution": "SC112233", "status": "active", "sanctions_status": "pass"}],
  "payments": [{"id": "pay_old", "beneficiary_id": "MIDOLD", "amount": {"amount": "1.00", "currency": "EUR"}, "currency_of_transfer": "EUR", "debtor_viban": {"account": "x"}, "creditor_account": {"account": "y"}, "creditor_name": "", "charge_bearer": "SHA", "sca_applied": false, "state": "B4BTMApproved", "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z"}]
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err = b4b.LoadState(old)
	if err != nil {
		t.Fatal(err)
	}
	legacy := testApp(t, lab.cb, newPeer(t), "", 0)
	legacy.dir.Restore(s)
	legacy.beneficiaries.Restore(s)
	legacy.engine.Restore(s)
	ll := serve(t, legacy, signingKey(t))
	oldLab := &envLab{a: legacy, l: ll}
	oldLab.sbxToken = oldLab.push(t, "sbx", "sbx-key-1", "", nil)
	if got := oldLab.companies(t, ll.token); !slices.Equal(got, []string{"cmp_old"}) {
		t.Errorf("the default lists %v from an old state file", got)
	}
	if got := oldLab.companies(t, oldLab.sbxToken); len(got) != 0 {
		t.Errorf("sbx lists %v from an old state file", got)
	}
	if b := oldLab.call(t, "GET", "/oversight/v1/beneficiaries/MIDOLD", "", ll.token, 200); b["id"] != "ben_old" {
		t.Errorf("the default's MIDOLD = %v", b)
	}
	oldLab.call(t, "GET", "/oversight/v1/payments/pay_old", "", ll.token, 200)
	oldLab.call(t, "GET", "/oversight/v1/payments/pay_old", "", oldLab.sbxToken, 404)
}

// The lab's hooks act in the environment the call names: a payee
// registered for sbx is sbx's, a setter reaches only the named
// environment's records, "*" and an environment b4b does not serve are
// refused, and a trap armed in sbx takes only sbx's payouts.
func TestSimHooksActInTheNamedEnvironment(t *testing.T) {
	lab := newEnvLab(t, 0)
	sbxCompany, _ := lab.board(t, lab.sbxToken, "mer_0001")
	person := lab.call(t, "POST", "/oversight/v1/companies/"+sbxCompany+"/people", `{"entity_type": "legal_entity", "legal_name": "Holdco Ltd"}`, lab.sbxToken, 201)["id"].(string)

	payee := `{"external_ref": "MID777", "account_name": "Outlet 777 Ltd", "account_number": "GB00SIM0000000000777", "financial_institution": "SC112233"}`
	st, b := send(t, lab.l.control, "POST", "/sim/beneficiaries?env=sbx", payee, "")
	if st != 201 {
		t.Fatalf("POST /sim/beneficiaries?env=sbx = %d %s", st, b)
	}
	if got := lab.call(t, "GET", "/oversight/v1/beneficiaries/MID777", "", lab.sbxToken, 200); got["account_name"] != "Outlet 777 Ltd" {
		t.Errorf("sbx reads MID777 as %v", got)
	}
	if got := lab.call(t, "GET", "/oversight/v1/beneficiaries/MID777", "", lab.l.token, 200); got["account_name"] == "Outlet 777 Ltd" {
		t.Errorf("the default reads sbx's MID777: %v", got)
	}
	for q, want := range map[string]int{"?env=*": 400, "?env=nope": 404} {
		if st, b := send(t, lab.l.control, "POST", "/sim/beneficiaries"+q, payee, ""); st != want {
			t.Errorf("POST /sim/beneficiaries%s = %d %s, want %d", q, st, b, want)
		}
	}

	for _, c := range []struct{ path, body string }{
		{"/sim/companies/" + sbxCompany + "/sanctions", `{"sanctions_status": "review"}`},
		{"/sim/people/" + person + "/sanctions", `{"pep_sanctions_status": "review"}`},
	} {
		if st, b := send(t, lab.l.control, "PUT", c.path, c.body, ""); st != 404 {
			t.Errorf("PUT %s in the default = %d %s, want 404: it is sbx's", c.path, st, b)
		}
		req, _ := http.NewRequest("PUT", lab.l.control.URL+c.path, strings.NewReader(c.body))
		req.Header.Set(tenant.Header, "sbx")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("PUT %s with %s: sbx = %d", c.path, tenant.Header, resp.StatusCode)
		}
	}
	if st, b := send(t, lab.l.control, "PUT", "/sim/beneficiaries/MID777/status?env=sbx", `{"status": "disabled"}`, ""); st != 200 {
		t.Fatalf("disabling sbx's MID777 = %d %s", st, b)
	}
	if got := lab.call(t, "GET", "/oversight/v1/beneficiaries/MID777", "", lab.l.token, 200); got["status"] != "active" {
		t.Errorf("disabling sbx's MID777 reached the default's: %v", got)
	}

	// A trap armed in sbx takes sbx's payout, not the default's.
	if st, b := send(t, lab.l.control, "POST", "/sim/traps?env=sbx", `{"hook": "payment.create", "match": {"beneficiary_id": "MID001"}, "effect": {"path": [{"to": "B4BTMPending"}]}}`, ""); st != 201 {
		t.Fatalf("arming a trap in sbx = %d %s", st, b)
	}
	lab.board(t, lab.l.token, "mer_0001")
	defPay := lab.pay(t, lab.l.token, "stl_def")
	sbxPay := lab.pay(t, lab.sbxToken, "stl_sbx")
	idle(t, lab.a)
	waitFor(t, "sbx's payout to hang", func() bool {
		p, err := lab.a.engine.Get(sbxPay)
		return err == nil && p.State == b4b.StateTMPending
	})
	if p, _ := lab.a.engine.Get(defPay); p.State != b4b.StateTMApproved || p.Trap != "" {
		t.Errorf("the default's payout = %s, trap %q: sbx's trap took it", p.State, p.Trap)
	}
}

// An environment's callbacks may go to its own webhook_hosts on top of
// WEBHOOK_ALLOWLIST; the default's, and another environment's, may not.
func TestWebhookHostsLetAnEnvironmentReachItsOwnPlatform(t *testing.T) {
	lab := newEnvLab(t, 0)
	// An allowlist that names none of the test's own servers.
	var err error
	if lab.a.list, err = allowlist.Parse("settlement"); err != nil {
		t.Fatal(err)
	}
	lab.a.callbackURL = ""
	own := newPeer(t)
	host := strings.TrimPrefix(own.srv.URL, "http://")
	if st, b := lab.putCustomer(t, "other", "other-key", &signingKey(t).PublicKey, own.srv.URL, nil); st != 400 {
		t.Errorf("a callback_url on neither list = %d %s", st, b)
	}
	tok := lab.push(t, "own", "own-key-1", own.srv.URL+"/cb", []string{host})

	body := `{"external_ref": "MID9", "account_name": "A Ltd", "account_number": "1", "financial_institution": "2", "callback_url": "` + own.srv.URL + `/ben"}`
	lab.call(t, "POST", "/oversight/v1/beneficiaries", body, lab.l.token, 400)
	lab.call(t, "POST", "/oversight/v1/beneficiaries", body, lab.sbxToken, 400)
	lab.call(t, "POST", "/oversight/v1/beneficiaries", body, tok, 201)
	lab.call(t, "POST", "/oversight/v1/beneficiaries", strings.Replace(body, `, "callback_url": "`+own.srv.URL+`/ben"`, "", 1), tok, 201)
	idle(t, lab.a)
	if n := own.count(); n != 2 {
		t.Errorf("the environment's own host took %d callback(s), want its payee's two: one at its callback_url, one at its client-account endpoint", n)
	}
}

// The old /sim paths on the vendor port are the default environment's: a
// customer's JWT is refused there, and naming another environment does not
// reach it.
func TestTheOldSimPathsAreTheDefaultEnvironments(t *testing.T) {
	lab := newEnvLab(t, 0)
	sbxCompany, _ := lab.board(t, lab.sbxToken, "mer_0001")
	if st, b := send(t, lab.l.vendor, "GET", "/sim/info", "", lab.sbxToken); st != 403 {
		t.Errorf("GET /sim/info on the vendor port with sbx's JWT = %d %s", st, b)
	}
	if st, b := send(t, lab.l.vendor, "PUT", "/sim/companies/"+sbxCompany+"/sanctions?env=sbx", `{"sanctions_status": "fail"}`, lab.l.token); st != 404 {
		t.Errorf("reaching sbx's company through the vendor port = %d %s", st, b)
	}
	if st, b := send(t, lab.l.vendor, "POST", "/sim/reset", `{"scope": "all", "env": "sbx"}`, lab.l.token); st != 403 {
		t.Errorf("resetting sbx through the vendor port = %d %s", st, b)
	}
	if st, b := send(t, lab.l.vendor, "GET", "/sim/customers", "", lab.l.token); st != 404 {
		t.Errorf("GET /sim/customers on the vendor port = %d %s", st, b)
	}
	if got := lab.companies(t, lab.sbxToken); len(got) != 1 {
		t.Errorf("sbx lists %v, want its company still there", got)
	}
}
