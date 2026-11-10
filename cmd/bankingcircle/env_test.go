package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/bankingcircle"
	"github.com/webduvet/fintechlab-simple/internal/money"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// Environments: two platforms on one bank (docs/lab-api/environments.md).
// The default environment is every login no customer has; "sbx" is a
// customer the lab API pushed, with a login of its own.

// sbxSGA is sbx's EUR safeguarding account.
var sbxSGA = tenant.SafeguardingAccount("sbx", "EUR")

const sbxCustomer = `{"env": "sbx", "name": "Sandbox",
	"banking_circle": {"username": "sbx-client", "password": "sbx-secret"},
	"webhook_hosts": ["platform.sbx.test"]}`

// credit is one POST the business bank received.
type credit struct{ env, iban string }

// envRig is Banking Circle wired as main wires it, with sbx pushed as a
// customer, and a business bank that records whose each credit was.
type envRig struct {
	t                   *testing.T
	a                   *app
	ctl, bridge, vendor string

	mu      sync.Mutex
	credits []credit
}

func newEnvRig(t *testing.T, delay time.Duration) *envRig {
	t.Helper()
	a := newControlTestApp(t)
	if delay > 0 {
		a.engine = bankingcircle.NewEngine(a.ledger, delay, a.onTransition)
		a.bank = bankingcircle.NewBank(a.engine, a.dispatch)
	}
	r := &envRig{t: t, a: a}
	bank := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			IBAN string `json:"iban"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		r.mu.Lock()
		r.credits = append(r.credits, credit{env: req.Header.Get(tenant.Header), iban: body.IBAN})
		r.mu.Unlock()
	}))
	t.Cleanup(bank.Close)
	a.bankCreditURL = bank.URL
	r.ctl = controlServer(t, a).URL
	bridge := httptest.NewServer(a.internalMux())
	t.Cleanup(bridge.Close)
	r.bridge = bridge.URL
	vendor := httptest.NewServer(vendorMux(t, a))
	t.Cleanup(vendor.Close)
	r.vendor = vendor.URL

	// sbx arrives as the lab API pushes it.
	if out := mustCall(t, 200, "PUT", r.ctl+"/sim/customers/sbx", "", sbxCustomer); out["created"] != true {
		t.Fatalf("PUT /sim/customers/sbx = %v", out)
	}
	return r
}

// creditsBy waits until the business bank has had n credits, and answers
// the Lab-Environment each came with, sorted. It fails the test if more
// arrive in the next moment: the credit is fire-and-forget.
func (r *envRig) creditsBy(n int) []string {
	r.t.Helper()
	count := func() int {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.credits)
	}
	deadline := time.Now().Add(3 * time.Second)
	for count() < n {
		if time.Now().After(deadline) {
			r.t.Fatalf("%d credit(s) at the business bank, want %d", count(), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.credits) != n {
		r.t.Fatalf("credits = %v, want %d", r.credits, n)
	}
	envs := make([]string, 0, n)
	for _, c := range r.credits {
		envs = append(envs, c.env)
	}
	sort.Strings(envs)
	return envs
}

// authorize is the platform's Basic→Bearer exchange.
func (r *envRig) authorize(user, pass string) (int, string) {
	r.t.Helper()
	req, _ := http.NewRequest("GET", r.vendor+"/api/v1/authorizations/authorize", nil)
	req.SetBasicAuth(user, pass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body.AccessToken
}

func (r *envRig) login(user, pass string) string {
	r.t.Helper()
	code, tok := r.authorize(user, pass)
	if code != 200 || tok == "" {
		r.t.Fatalf("authorize %s = %d", user, code)
	}
	return tok
}

// subscribe makes a subscription to endpoint for every payment event, with
// tok, as a platform does on boot.
func (r *envRig) subscribe(tok, endpoint string) string {
	r.t.Helper()
	sub := mustCall(r.t, 200, "POST", r.vendor+"/api/v1/notificationselfservice/subscription", tok,
		`{"endpoint": "`+endpoint+`", "maxNotificationsPerMessage": 5}`)
	id := sub["id"].(string)
	for _, et := range []bankingcircle.NotificationType{
		bankingcircle.NotificationOutgoingPaymentBooked, bankingcircle.NotificationOutgoingPaymentProcessed,
		bankingcircle.NotificationOutgoingPaymentRejected, bankingcircle.NotificationMissingFunding,
		bankingcircle.NotificationIncomingPaymentBooked, bankingcircle.NotificationIncomingPaymentProcessed,
	} {
		mustCall(r.t, 200, "POST", r.vendor+"/api/v1/notificationselfservice/subscriptionEvent", tok,
			`{"subscriptionId": "`+id+`", "eventType": "`+string(et)+`", "targetType": 1}`)
	}
	return id
}

// bridgeCall is a call on the bridge, naming env in Lab-Environment unless
// env is "".
func (r *envRig) bridgeCall(want int, path, env, body string) map[string]any {
	r.t.Helper()
	hdr := http.Header{}
	if env != "" {
		hdr.Set(tenant.Header, env)
	}
	code, b := raw(r.t, "POST", r.bridge+path, "", hdr, body)
	if code != want {
		r.t.Fatalf("POST %s (env %q) = %d %s, want %d", path, env, code, b, want)
	}
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	return out
}

// payout is B4B's hand-off of one payout for env.
func (r *envRig) payout(env, id string) {
	r.t.Helper()
	r.bridgeCall(202, "/internal/payments", env,
		`{"paymentId": "`+id+`", "accountId": "`+merchantAccount+`", "amount": "10.00", "currency": "EUR", "iban": "GB00SIM0000000000003"}`)
}

// paymentIDs is what a payments list holds, by id, sorted.
func paymentIDs(t *testing.T, out map[string]any) []string {
	t.Helper()
	var ids []string
	for _, p := range out["payments"].([]any) {
		ids = append(ids, p.(map[string]any)["id"].(string))
	}
	sort.Strings(ids)
	return ids
}

// payouts is the payment ids in a list that are payouts (not money in).
func payouts(t *testing.T, out map[string]any) []string {
	t.Helper()
	var ids []string
	for _, p := range out["payments"].([]any) {
		if m := p.(map[string]any); m["fromAccountId"] != "external_worldline" {
			ids = append(ids, m["id"].(string))
		}
	}
	sort.Strings(ids)
	return ids
}

// balance is an account's balance on the vendor's balance read, as a
// platform sums it.
func (r *envRig) balance(tok, account string) string {
	r.t.Helper()
	out := mustCall(r.t, 200, "GET", r.vendor+"/api/v1/accounts/"+account+"/balances", tok, "")
	e := out["result"].([]any)[0].(map[string]any)
	open, _ := money.Parse(e["beginOfDayAmount"].(string))
	intraday, _ := money.Parse(e["intraDayAmount"].(string))
	return money.Format(open + intraday)
}

// drain decrypts every notification delivered to ch until it goes quiet.
func drain(t *testing.T, ch chan received, key []byte) []bankingcircle.Notification {
	t.Helper()
	var out []bankingcircle.Notification
	for {
		select {
		case rec := <-ch:
			plain, err := decryptNotification(rec.body, rec.nonce, rec.tag, key)
			if err != nil {
				t.Fatalf("decrypt: %v", err)
			}
			var env bankingcircle.Envelope
			if err := json.Unmarshal(plain, &env); err != nil {
				t.Fatal(err)
			}
			out = append(out, env.Notifications...)
		case <-time.After(150 * time.Millisecond):
			return out
		}
	}
}

// aboutPayments is the payment ids the notifications are about.
func aboutPayments(t *testing.T, notes []bankingcircle.Notification) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, n := range notes {
		raw, _ := json.Marshal(n.Payment)
		var p struct {
			PaymentID string `json:"paymentId"`
		}
		_ = json.Unmarshal(raw, &p)
		out[p.PaymentID] = true
	}
	return out
}

// TestTwoEnvironmentsShareTheBankAndNothingElse is the whole story: two
// platforms, each with its own login, fund their own safeguarding accounts
// and pay the same beneficiary at the same moment; each sees only its own
// payouts, balances and notifications; one's reset and then its deletion
// leave the other's alone; a login no customer has is still the default's.
func TestTwoEnvironmentsShareTheBankAndNothingElse(t *testing.T) {
	r := newEnvRig(t, 0)
	a := r.a

	// Each platform authorizes with its own login. A customer's username
	// with another password is a bad login; anything else is the default's.
	sbxTok := r.login("sbx-client", "sbx-secret")
	defTok := r.login("default-client", "whatever")
	if code, _ := r.authorize("sbx-client", "wrong"); code != 401 {
		t.Fatalf("sbx's username with the wrong password = %d, want 401", code)
	}

	defSrv, defCh := captureSubscriber(t)
	sbxSrv, sbxCh := captureSubscriber(t)
	defSub := r.subscribe(defTok, defSrv.URL)
	sbxSub := r.subscribe(sbxTok, sbxSrv.URL)

	// Money in: Worldline's lump sum to each one's safeguarding account.
	r.bridgeCall(202, "/internal/incoming-payments", "", `{"currency": "EUR", "amount": "100.00", "reference": "lump default"}`)
	r.bridgeCall(202, "/internal/incoming-payments", "sbx", `{"currency": "EUR", "amount": "50.00", "reference": "lump sbx"}`)
	a.engine.Wait()

	// Both pay the same beneficiary, in the same currency, at the same time.
	var wg sync.WaitGroup
	for env, id := range map[string]string{"": "bcp_def1", "sbx": "bcp_sbx1"} {
		wg.Add(1)
		go func() { defer wg.Done(); r.payout(env, id) }()
	}
	wg.Wait()
	a.engine.Wait()

	// Each sees only its own payouts.
	if got := payouts(t, mustCall(t, 200, "GET", r.vendor+"/payments", defTok, "")); !slices.Equal(got, []string{"bcp_def1"}) {
		t.Fatalf("the default's payouts = %v", got)
	}
	if got := payouts(t, mustCall(t, 200, "GET", r.vendor+"/payments", sbxTok, "")); !slices.Equal(got, []string{"bcp_sbx1"}) {
		t.Fatalf("sbx's payouts = %v", got)
	}
	mustCall(t, 404, "GET", r.vendor+"/payments/bcp_def1", sbxTok, "")
	mustCall(t, 404, "GET", r.vendor+"/api/v1/payments/singles/bcp_sbx1/status", defTok, "")
	if got := mustCall(t, 200, "GET", r.vendor+"/api/v1/payments/singles/bcp_sbx1/status", sbxTok, ""); got["status"] != "Processed" {
		t.Fatalf("sbx's payout status = %v", got)
	}

	// Each debited its own safeguarding account, and can read only its own.
	if got := r.balance(defTok, sgaEUR); got != "90.00" {
		t.Fatalf("the default's safeguarding account = %s, want 90.00", got)
	}
	if got := r.balance(sbxTok, sbxSGA); got != "40.00" {
		t.Fatalf("sbx's safeguarding account = %s, want 40.00", got)
	}
	mustCall(t, 404, "GET", r.vendor+"/api/v1/accounts/"+sgaEUR+"/balances", sbxTok, "")
	mustCall(t, 404, "GET", r.vendor+"/api/v1/accounts/"+sbxSGA+"/balances", defTok, "")

	// One beneficiary, two accounts: B4B names the same one for both.
	sbxCreditor := bankingcircle.AccountIDIn("sbx", merchantAccount)
	if sbxCreditor == merchantAccount {
		t.Fatal("sbx's creditor account is the default's")
	}
	for tok, id := range map[string]string{defTok: merchantAccount, sbxTok: sbxCreditor} {
		if acc := mustCall(t, 200, "GET", r.vendor+"/accounts/"+id, tok, ""); acc["balance"] != "10.00" {
			t.Fatalf("creditor %s = %v, want 10.00", id, acc)
		}
	}
	if p := mustCall(t, 200, "GET", r.vendor+"/payments/bcp_sbx1", sbxTok, ""); p["toAccountId"] != sbxCreditor || p["fromAccountId"] != sbxSGA || p["env"] != "sbx" {
		t.Fatalf("sbx's payout = %v", p)
	}

	// Each subscription heard only of its own environment's payments.
	defNotes, sbxNotes := aboutPayments(t, drain(t, defCh, a.notifKey)), aboutPayments(t, drain(t, sbxCh, a.notifKey))
	if !defNotes["bcp_def1"] || defNotes["bcp_sbx1"] {
		t.Fatalf("the default's subscription heard of %v", defNotes)
	}
	if !sbxNotes["bcp_sbx1"] || sbxNotes["bcp_def1"] {
		t.Fatalf("sbx's subscription heard of %v", sbxNotes)
	}
	for id := range defNotes {
		if sbxNotes[id] {
			t.Fatalf("both subscriptions heard of %s", id)
		}
	}

	// The business bank was credited twice, and told whose each was.
	if envs := r.creditsBy(2); !slices.Equal(envs, []string{"", "sbx"}) {
		t.Fatalf("credits at the business bank by Lab-Environment = %q, want one unnamed (the default's) and one sbx", envs)
	}

	// The lab's reads take ?env=: one environment, or every one.
	if got := mustCall(t, 200, "GET", r.ctl+"/sim/payments?env=sbx", "", ""); len(paymentIDs(t, got)) != 2 || !slices.Contains(paymentIDs(t, got), "bcp_sbx1") {
		t.Fatalf("/sim/payments?env=sbx = %v, want sbx's money in and payout", paymentIDs(t, got))
	}
	if got := paymentIDs(t, mustCall(t, 200, "GET", r.ctl+"/sim/payments", "", "")); len(got) != 2 || !slices.Contains(got, "bcp_def1") {
		t.Fatalf("/sim/payments = %v, want the default's two", got)
	}
	if got := paymentIDs(t, mustCall(t, 200, "GET", r.ctl+"/sim/payments?env=*", "", "")); len(got) != 4 {
		t.Fatalf("/sim/payments?env=* = %v, want all four", got)
	}
	hdr := http.Header{tenant.Header: {"sbx"}}
	if code, b := raw(t, "GET", r.ctl+"/sim/subscriptions", "", hdr, ""); code != 200 || !strings.Contains(string(b), sbxSub) || strings.Contains(string(b), defSub) {
		t.Fatalf("/sim/subscriptions with Lab-Environment: sbx = %d %s", code, b)
	}
	if got := mustCall(t, 200, "GET", r.ctl+"/sim/accounts/"+sbxSGA+"/balances?env=sbx", "", ""); got["accountId"] != sbxSGA {
		t.Fatalf("sbx's balance on the control port = %v", got)
	}
	mustCall(t, 404, "GET", r.ctl+"/sim/accounts/"+sbxSGA+"/balances", "", "")

	// A reset of sbx leaves the default's run alone.
	out := mustCall(t, 200, "POST", r.ctl+"/sim/reset", "", `{"scope": "flow", "env": "sbx"}`)
	if cleared := out["cleared"].(map[string]any); out["env"] != "sbx" || cleared["payments"] != float64(2) {
		t.Fatalf("sbx's reset = %v, want its two payments cleared", out)
	}
	if got := paymentIDs(t, mustCall(t, 200, "GET", r.vendor+"/payments", defTok, "")); len(got) != 2 || !slices.Contains(got, "bcp_def1") {
		t.Fatalf("the default's payments after sbx's reset = %v", got)
	}
	if got := r.balance(defTok, sgaEUR); got != "90.00" {
		t.Fatalf("the default's safeguarding account after sbx's reset = %s", got)
	}
	if got := r.balance(sbxTok, sbxSGA); got != "0.00" {
		t.Fatalf("sbx's safeguarding account after its reset = %s, want 0.00", got)
	}
	if got := mustCall(t, 200, "GET", r.vendor+"/api/v1/notificationselfservice/subscription", sbxTok, ""); got["totalItems"] != float64(1) {
		t.Fatalf("sbx's subscriptions after its flow reset = %v, want its one kept", got)
	}

	// Deleting sbx takes everything it made with it.
	r.payout("sbx", "bcp_sbx2")
	a.engine.Wait()
	mustCall(t, 200, "DELETE", r.ctl+"/sim/customers/sbx", "", "")
	if got := paymentIDs(t, mustCall(t, 200, "GET", r.ctl+"/sim/payments?env=*", "", "")); len(got) != 2 || !slices.Contains(got, "bcp_def1") {
		t.Fatalf("every payment after sbx was deleted = %v, want the default's two", got)
	}
	for _, raw := range mustCall(t, 200, "GET", r.ctl+"/sim/accounts?env=*", "", "")["accounts"].([]any) {
		if acc := raw.(map[string]any); acc["env"] != nil {
			t.Fatalf("an account of %v survived its environment: %v", acc["env"], acc)
		}
	}
	if got := mustCall(t, 200, "GET", r.ctl+"/sim/subscriptions?env=*", "", ""); got["totalItems"] != float64(1) {
		t.Fatalf("subscriptions after sbx was deleted = %v, want the default's one", got)
	}
	mustCall(t, 401, "GET", r.vendor+"/payments", sbxTok, "")
	if code, _ := r.authorize("sbx-client", "sbx-secret"); code != 200 {
		t.Fatalf("sbx's old login once sbx is gone = %d, want 200 (an unknown login is the default's)", code)
	}

	// A login no customer has is the default's, as every login was.
	other := r.login("anyone", "anypass")
	if got := r.balance(other, sgaEUR); got != "90.00" {
		t.Fatalf("an unknown login reads the default's safeguarding account as %s", got)
	}
}

// TestAnOutcomeQueuedForOneEnvironmentTakesOnlyItsPayout: the outcome
// queue's traps have an empty match, so scoping them is what keeps sbx's
// Rejected from taking the default's next payout.
func TestAnOutcomeQueuedForOneEnvironmentTakesOnlyItsPayout(t *testing.T) {
	r := newEnvRig(t, 0)
	r.bridgeCall(202, "/internal/incoming-payments", "", `{"currency": "EUR", "amount": "100.00"}`)
	r.bridgeCall(202, "/internal/incoming-payments", "sbx", `{"currency": "EUR", "amount": "100.00"}`)
	r.a.engine.Wait()

	if code, b := raw(t, "POST", r.ctl+"/sim/payments/outcomes?env=*", "", nil, `{"outcomes": ["Rejected"]}`); code != 400 {
		t.Fatalf("an outcome for every environment = %d %s, want 400", code, b)
	}
	if code, b := raw(t, "POST", r.ctl+"/sim/payments/outcomes?env=nobody", "", nil, `{"outcomes": ["Rejected"]}`); code != 404 {
		t.Fatalf("an outcome for an unknown environment = %d %s, want 404", code, b)
	}
	if got := mustCall(t, 200, "POST", r.ctl+"/sim/payments/outcomes?env=sbx", "", `{"outcomes": ["Rejected"]}`)["queued"]; !slices.Equal(anyStrings(got), []string{"Rejected"}) {
		t.Fatalf("sbx's queue = %v", got)
	}
	if got := mustCall(t, 200, "GET", r.ctl+"/sim/payments/outcomes", "", "")["queued"]; len(anyStrings(got)) != 0 {
		t.Fatalf("the default's queue = %v, want empty", got)
	}
	traps := mustCall(t, 200, "GET", r.ctl+"/sim/traps?env=sbx", "", "")["traps"].([]any)
	if len(traps) != 1 || traps[0].(map[string]any)["env"] != "sbx" {
		t.Fatalf("sbx's traps = %v", traps)
	}

	r.payout("", "bcp_def")
	r.a.engine.Wait()
	r.payout("sbx", "bcp_sbx")
	r.a.engine.Wait()

	// The trap's step is on the trap's own schedule, not the engine's.
	states := map[string]string{}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		for _, raw := range mustCall(t, 200, "GET", r.ctl+"/sim/payments?env=*", "", "")["payments"].([]any) {
			m := raw.(map[string]any)
			states[m["id"].(string)] = m["state"].(string)
		}
		if states["bcp_sbx"] != string(bankingcircle.NotificationOutgoingPaymentBooked) {
			break
		}
	}
	if states["bcp_def"] != "OutgoingPaymentProcessed" || states["bcp_sbx"] != "OutgoingPaymentRejected" {
		t.Fatalf("states = %v: sbx's outcome must take sbx's payout, and only it", states)
	}
}

func anyStrings(v any) []string {
	out := []string{}
	for _, s := range v.([]any) {
		out = append(out, s.(string))
	}
	return out
}

// TestAResetOfOneEnvironmentLeavesAnothersWorkInFlight: both payouts are
// still sleeping out PROCESSING_DELAY when sbx is reset. sbx's is dropped
// -- no money moves, no notification, no credit -- and the default's
// carries on to processed.
func TestAResetOfOneEnvironmentLeavesAnothersWorkInFlight(t *testing.T) {
	r := newEnvRig(t, 150*time.Millisecond)
	defTok, sbxTok := r.login("x", "y"), r.login("sbx-client", "sbx-secret")
	defSrv, defCh := captureSubscriber(t)
	sbxSrv, sbxCh := captureSubscriber(t)
	r.subscribe(defTok, defSrv.URL)
	r.subscribe(sbxTok, sbxSrv.URL)
	r.bridgeCall(202, "/internal/incoming-payments", "", `{"currency": "EUR", "amount": "100.00"}`)
	r.bridgeCall(202, "/internal/incoming-payments", "sbx", `{"currency": "EUR", "amount": "100.00"}`)
	r.a.engine.Wait()
	drain(t, defCh, r.a.notifKey)
	drain(t, sbxCh, r.a.notifKey)

	r.payout("", "bcp_def")
	r.payout("sbx", "bcp_sbx")
	mustCall(t, 200, "POST", r.ctl+"/sim/reset?env=sbx", "", "")
	within(t, 2*time.Second, "the payments in flight", r.a.engine.Wait)

	if p := mustCall(t, 200, "GET", r.vendor+"/payments/bcp_def", defTok, ""); p["state"] != "OutgoingPaymentProcessed" {
		t.Fatalf("the default's payout after sbx's reset = %v, want processed", p["state"])
	}
	mustCall(t, 404, "GET", r.vendor+"/payments/bcp_sbx", sbxTok, "")
	if got := r.balance(sbxTok, sbxSGA); got != "0.00" {
		t.Fatalf("sbx's safeguarding account = %s: a dropped payout moved money", got)
	}
	if got := r.balance(defTok, sgaEUR); got != "90.00" {
		t.Fatalf("the default's safeguarding account = %s, want 90.00", got)
	}
	if notes := aboutPayments(t, drain(t, defCh, r.a.notifKey)); !notes["bcp_def"] {
		t.Fatalf("the default's subscription heard of %v, want its processed payout", notes)
	}
	for _, n := range drain(t, sbxCh, r.a.notifKey) {
		if n.NotificationType != string(bankingcircle.NotificationOutgoingPaymentBooked) {
			t.Fatalf("sbx's subscription was sent %s for a payout its reset dropped", n.NotificationType)
		}
	}
	if envs := r.creditsBy(1); envs[0] != "" {
		t.Fatalf("credits by Lab-Environment = %q, want the default's one", envs)
	}
}

// TestTheOldSimPathsOnLISTENAreTheCallersOwn: behind the bearer, a /sim
// path acts in the token's environment whatever the call names, and the
// lab's own routes are not served there.
func TestTheOldSimPathsOnLISTENAreTheCallersOwn(t *testing.T) {
	r := newEnvRig(t, 0)
	defTok, sbxTok := r.login("x", "y"), r.login("sbx-client", "sbx-secret")
	r.bridgeCall(202, "/internal/incoming-payments", "", `{"currency": "EUR", "amount": "100.00"}`)
	r.bridgeCall(202, "/internal/incoming-payments", "sbx", `{"currency": "EUR", "amount": "100.00"}`)
	r.payout("", "bcp_def")
	r.payout("sbx", "bcp_sbx")
	r.a.engine.Wait()

	for _, q := range []string{"", "?env=default", "?env=*"} {
		if got := payouts(t, mustCall(t, 200, "GET", r.vendor+"/sim/payments"+q, sbxTok, "")); !slices.Equal(got, []string{"bcp_sbx"}) {
			t.Fatalf("sbx's /sim/payments%s on LISTEN = %v, want only its own", q, got)
		}
	}
	mustCall(t, 404, "GET", r.vendor+"/sim/customers", defTok, "")
	mustCall(t, 404, "PUT", r.vendor+"/sim/customers/evil", defTok, `{"env": "evil"}`)
	mustCall(t, 403, "GET", r.vendor+"/sim/logs", sbxTok, "")

	// sbx asking LISTEN to reset everything resets sbx.
	if out := mustCall(t, 200, "POST", r.vendor+"/sim/reset", sbxTok, `{"scope": "flow", "env": "*"}`); out["env"] != "sbx" {
		t.Fatalf("sbx's reset on LISTEN = %v, want sbx's own", out)
	}
	if got := payouts(t, mustCall(t, 200, "GET", r.ctl+"/sim/payments", "", "")); !slices.Equal(got, []string{"bcp_def"}) {
		t.Fatalf("the default's payouts after sbx's reset on LISTEN = %v", got)
	}
}

// TestSubscriptionsAreTheirOwnersAlone: another customer's subscription is
// a 404 on every route that names one, a customer's webhook_hosts let it
// subscribe where the sim's allowlist would not, and one endpoint may be
// subscribed by two customers.
func TestSubscriptionsAreTheirOwnersAlone(t *testing.T) {
	r := newEnvRig(t, 0)
	defTok, sbxTok := r.login("x", "y"), r.login("sbx-client", "sbx-secret")
	sink := sinkSubscriber(t)
	defSub := r.subscribe(defTok, sink)
	sbxSub := r.subscribe(sbxTok, sink) // the same endpoint is no conflict across customers
	ev := mustCall(t, 200, "GET", r.vendor+"/api/v1/notificationselfservice/subscription/"+defSub, defTok, "")
	defEvent := ev["subscriptionEvents"].([]any)[0].(map[string]any)["id"].(string)
	hdr := http.Header{"If-Match": {ev["rowVersion"].(string)}}

	base := r.vendor + "/api/v1/notificationselfservice"
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/subscription/" + defSub, ""},
		{"PUT", "/subscription/" + defSub, `{"email": "x@example.test"}`},
		{"PUT", "/subscription/" + defSub + "/activate", ""},
		{"PUT", "/subscription/" + defSub + "/deactivate", ""},
		{"DELETE", "/subscription/" + defSub, ""},
		{"POST", "/subscriptionEvent", `{"subscriptionId": "` + defSub + `", "eventType": "Reversed", "targetType": 1}`},
		{"PUT", "/subscriptionEvent/" + defEvent + "/targets", `{"targetType": 0, "targetIds": ["x"]}`},
		{"DELETE", "/subscriptionEvent/" + defEvent, ""},
		{"POST", "/clienttest/" + defSub, ""},
	} {
		if code, b := raw(t, c.method, base+c.path, sbxTok, hdr, c.body); code != 404 {
			t.Errorf("sbx %s %s on the default's subscription = %d %s, want 404", c.method, c.path, code, b)
		}
	}
	if got := mustCall(t, 200, "GET", base+"/subscription", sbxTok, ""); got["totalItems"] != float64(1) || !strings.Contains(mustJSON(t, got), sbxSub) {
		t.Fatalf("sbx's list = %v", got)
	}
	for _, path := range []string{"/pause", "/resume", "/test", "/activate", "/deactivate"} {
		mustCall(t, 404, "POST", r.ctl+"/sim/subscription/"+defSub+path+"?env=sbx", "", "")
	}
	if got := mustCall(t, 200, "GET", r.ctl+"/sim/subscription/"+defSub+"/pending?env=sbx", "", ""); got["pending"] != float64(0) {
		t.Fatalf("another environment's pending = %v, want zeros", got)
	}

	// webhook_hosts: sbx's platform host is allowed for sbx alone.
	body := `{"endpoint": "https://platform.sbx.test/hooks"}`
	mustCall(t, 400, "POST", base+"/subscription", defTok, body)
	mustCall(t, 200, "POST", base+"/subscription", sbxTok, body)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestMoneyInIsTheReceivingAccountsEnvironments: on the bridge an
// accountId's owner says whose the money is, and a call naming another is
// refused; the control port acts in the environment it names.
func TestMoneyInIsTheReceivingAccountsEnvironments(t *testing.T) {
	r := newEnvRig(t, 0)
	sbxTok := r.login("sbx-client", "sbx-secret")

	p := r.bridgeCall(202, "/internal/incoming-payments", "", `{"currency": "EUR", "amount": "5.00", "accountId": "`+sbxSGA+`"}`)
	if p["env"] != "sbx" || p["toAccountId"] != sbxSGA {
		t.Fatalf("money in to sbx's account = %v, want sbx's", p)
	}
	r.bridgeCall(400, "/internal/incoming-payments", "default", `{"currency": "EUR", "amount": "5.00", "accountId": "`+sbxSGA+`"}`)
	r.bridgeCall(400, "/internal/incoming-payments", "", `{"currency": "GBP", "amount": "5.00", "accountId": "`+sbxSGA+`"}`)
	r.bridgeCall(404, "/internal/incoming-payments", "nobody", `{"currency": "EUR", "amount": "5.00"}`)
	r.bridgeCall(400, "/internal/incoming-payments", "*", `{"currency": "EUR", "amount": "5.00"}`)
	r.bridgeCall(400, "/internal/incoming-payments", "*", `{"currency": "EUR", "amount": "5.00", "accountId": "`+sbxSGA+`"}`)
	r.bridgeCall(400, "/internal/payments", "*", `{"paymentId": "bcp_x", "accountId": "`+merchantAccount+`", "amount": "1.00"}`)
	r.bridgeCall(404, "/internal/payments", "nobody", `{"paymentId": "bcp_x", "accountId": "`+merchantAccount+`", "amount": "1.00"}`)

	mustCall(t, 202, "POST", r.ctl+"/sim/incoming-payments?env=sbx", "", `{"currency": "EUR", "amount": "5.00"}`)
	mustCall(t, 404, "POST", r.ctl+"/sim/incoming-payments", "", `{"currency": "EUR", "amount": "5.00", "accountId": "`+sbxSGA+`"}`)
	mustCall(t, 400, "POST", r.ctl+"/sim/incoming-payments?env=*", "", `{"currency": "EUR", "amount": "5.00"}`)
	r.a.engine.Wait()
	if got := r.balance(sbxTok, sbxSGA); got != "10.00" {
		t.Fatalf("sbx's safeguarding account = %s, want 10.00", got)
	}

	// The bridge's balance read is the default's unless the call names
	// another.
	mustCall(t, 404, "GET", r.bridge+"/internal/accounts/"+sbxSGA+"/balances", "", "")
	code, _ := raw(t, "GET", r.bridge+"/internal/accounts/"+sbxSGA+"/balances", "", http.Header{tenant.Header: {"sbx"}}, "")
	if code != 200 {
		t.Fatalf("sbx's balance on the bridge with Lab-Environment: sbx = %d", code)
	}

	// The payments log has sbx's money in as sbx's.
	var envs []string
	for _, e := range r.a.payLog.RecentIn(tenant.All, 0) {
		if e.Op == "payment.incoming" && e.Detail["status"] == "Accepted" {
			envs = append(envs, e.Env)
		}
	}
	if len(envs) != 2 || envs[0] != "sbx" || envs[1] != "sbx" {
		t.Fatalf("payment.incoming events by environment = %q, want sbx's two", envs)
	}
}

// TestTheCustomersAreOnTheControlPort: /sim/info says the bank supports
// environments, and /sim/customers when each was last seen on the vendor
// surface.
func TestTheCustomersAreOnTheControlPort(t *testing.T) {
	r := newEnvRig(t, 0)
	if info := mustCall(t, 200, "GET", r.ctl+"/sim/info", "", ""); info["environments"] != true || info["customers"] != float64(1) {
		t.Fatalf("/sim/info = %v", info)
	}
	seen := func() map[string]any {
		out := map[string]any{}
		for _, c := range mustCall(t, 200, "GET", r.ctl+"/sim/customers", "", "")["customers"].([]any) {
			m := c.(map[string]any)
			out[m["env"].(string)] = m["last_seen"]
		}
		return out
	}
	if got := seen(); got["default"] != nil || got["sbx"] != nil {
		t.Fatalf("last seen before any call = %v", got)
	}
	tok := r.login("sbx-client", "sbx-secret")
	mustCall(t, 200, "GET", r.vendor+"/api/v1/notificationselfservice/subscription", tok, "")
	if got := seen(); got["sbx"] == nil || got["default"] != nil {
		t.Fatalf("last seen after sbx's calls = %v", got)
	}

	// A customer replaced keeps its safeguarding accounts; its holder is
	// the new name.
	mustCall(t, 200, "PUT", r.ctl+"/sim/customers/sbx", "", strings.Replace(sbxCustomer, "Sandbox", "Renamed", 1))
	acc := mustCall(t, 200, "GET", r.vendor+"/accounts/"+sbxSGA, tok, "")
	if !strings.HasPrefix(acc["holder"].(string), "Renamed") || acc["env"] != "sbx" {
		t.Fatalf("sbx's safeguarding account after the customer was replaced = %v", acc)
	}

	// A reset of an environment the bank does not serve is a 404.
	if code, b := raw(t, "POST", r.ctl+"/sim/reset", "", nil, `{"env": "nobody"}`); code != 404 {
		t.Fatalf("reset of an unknown environment = %d %s", code, b)
	}
}
