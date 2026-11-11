package main

import (
	"encoding/json"
	"log"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/bankingcircle"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// The state file (state.go): a restart of the bank -- a redeploy of the lab
// -- keeps what a platform had at it, and finishes what was in flight.

// openTestApp is a test app opened from the state file at path, as main
// opens one: what the file holds is restored, the work it was in the middle
// of resumed, and every change written back. delay is PROCESSING_DELAY,
// flush the batch flush interval. Its work is stopped when the test ends.
func openTestApp(t *testing.T, path string, delay, flush time.Duration, seed func(*app) error) *app {
	t.Helper()
	a := buildTestApp(t, log.Printf, delay, flush)
	a.payLog = activity.New("payments", "Payments received", "")
	var first func() error
	if seed != nil {
		first = func() error { return seed(a) }
	}
	if err := a.open(path, first); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.bank.Stop)
	return a
}

// rigFor serves a's three surfaces as newEnvRig does, with no business bank
// and no customer pushed.
func rigFor(t *testing.T, a *app) *envRig {
	t.Helper()
	r := &envRig{t: t, a: a}
	r.ctl = controlServer(t, a).URL
	bridge := httptest.NewServer(a.internalMux())
	t.Cleanup(bridge.Close)
	r.bridge = bridge.URL
	vendor := httptest.NewServer(vendorMux(t, a))
	t.Cleanup(vendor.Close)
	r.vendor = vendor.URL
	return r
}

// dayAmounts is an account's balance read as a platform reads it, without
// the read's own timestamp.
func (r *envRig) dayAmounts(tok, account string) [3]string {
	r.t.Helper()
	out := mustCall(r.t, 200, "GET", r.vendor+"/api/v1/accounts/"+account+"/balances", tok, "")
	e := out["result"].([]any)[0].(map[string]any)
	return [3]string{e["financialDate"].(string), e["beginOfDayAmount"].(string), e["intraDayAmount"].(string)}
}

func loadState(t *testing.T, path string) bankingcircle.State {
	t.Helper()
	s, err := bankingcircle.LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The incident this exists for: a platform's safeguarding account holds
// the money a lump sum landed, it has a subscription and a token, and a
// payout is walking its phases -- and the bank is redeployed. The bank that
// comes back has the same balance (beginOfDay and intraDay alike), the same
// subscription with the same id, rowVersion and key, accepts the same
// token, and finishes the payout, telling the platform everything it had
// not been told yet.
func TestARestartKeepsWhatThePlatformHadAndFinishesWhatWasInFlight(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bc-data", "state.json")
	subscriber, deliveries := captureSubscriber(t)
	const key = "sbx-platform-own-key-32-chars-ok"

	// The first process: an hour's processing delay and an hour's batch
	// flush, so the payout is still booked, and every notification still
	// waiting, when it stops.
	a := openTestApp(t, path, time.Hour, time.Hour, nil)
	r := rigFor(t, a)
	mustCall(t, 200, "PUT", r.ctl+"/sim/customers/sbx", "", sbxCustomer)
	tok := r.login("sbx-client", "sbx-secret")
	subID := mustCall(t, 200, "POST", r.vendor+"/api/v1/notificationselfservice/subscription", tok,
		`{"endpoint": "`+subscriber.URL+`", "encryptionKey": "`+key+`", "maxNotificationsPerMessage": 5}`)["id"].(string)
	for _, et := range []bankingcircle.NotificationType{
		bankingcircle.NotificationOutgoingPaymentBooked, bankingcircle.NotificationOutgoingPaymentProcessed,
		bankingcircle.NotificationIncomingPaymentBooked, bankingcircle.NotificationIncomingPaymentProcessed,
	} {
		mustCall(t, 200, "POST", r.vendor+"/api/v1/notificationselfservice/subscriptionEvent", tok,
			`{"subscriptionId": "`+subID+`", "eventType": "`+string(et)+`", "targetType": 1}`)
	}
	r.bridgeCall(202, "/internal/incoming-payments", "sbx", `{"currency": "EUR", "amount": "250.00", "reference": "WL lump sum"}`)
	r.payout("sbx", "pay-restart")
	balance := r.dayAmounts(tok, sbxSGA)
	if balance[1]+"/"+balance[2] != "0.00/250.00" {
		t.Fatalf("before the restart: beginOfDay/intraDay %s/%s, want 0.00/250.00", balance[1], balance[2])
	}
	sub := mustCall(t, 200, "GET", r.vendor+"/api/v1/notificationselfservice/subscription/"+subID, tok, "")
	select {
	case <-deliveries:
		t.Fatal("a notification went out before the restart; the test wants them all still waiting")
	case <-time.After(30 * time.Millisecond):
	}

	a.stop()
	written := loadState(t, path)
	var booked bool
	for _, p := range written.Payments {
		booked = booked || (p.ID == "pay-restart" && p.State == bankingcircle.NotificationOutgoingPaymentBooked && p.Env == "sbx")
	}
	if !booked || len(written.Notifications) != 1 || len(written.Notifications[0].Notifications) != 2 {
		t.Fatalf("the file at stop: payments %+v, notifications %+v; want the payout booked and both bookings' notifications held",
			written.Payments, written.Notifications)
	}

	// The second process, from the same file.
	b := openTestApp(t, path, 200*time.Millisecond, 5*time.Millisecond, nil)
	rb := rigFor(t, b)
	if got := rb.dayAmounts(tok, sbxSGA); got != balance {
		t.Fatalf("after the restart the token reads %v, want %v", got, balance)
	}
	if got := mustCall(t, 200, "GET", rb.vendor+"/api/v1/notificationselfservice/subscription/"+subID, tok, ""); !reflect.DeepEqual(got, sub) {
		t.Fatalf("the subscription after the restart:\n%v\nwant\n%v", got, sub)
	}
	customers := mustCall(t, 200, "GET", rb.ctl+"/sim/customers", "", "")["customers"].([]any)
	if len(customers) != 2 || customers[1].(map[string]any)["env"] != "sbx" {
		t.Fatalf("customers after the restart = %v", customers)
	}

	// Everything the platform had not been told, under its own key: both
	// bookings from before the restart, both processings after it.
	seen := map[string]string{}
	deadline := time.After(5 * time.Second)
	for len(seen) < 4 {
		select {
		case rec := <-deliveries:
			plain, err := decryptNotification(rec.body, rec.nonce, rec.tag, []byte(key))
			if err != nil {
				t.Fatalf("decrypt with the subscription's own key: %v", err)
			}
			var env bankingcircle.Envelope
			if err := json.Unmarshal(plain, &env); err != nil {
				t.Fatal(err)
			}
			for _, n := range env.Notifications {
				var p struct {
					PaymentID string `json:"paymentId"`
				}
				raw, _ := json.Marshal(n.Payment)
				_ = json.Unmarshal(raw, &p)
				if n.SubscriptionID != subID {
					t.Fatalf("notification for subscription %s, want %s", n.SubscriptionID, subID)
				}
				seen[n.NotificationType] = p.PaymentID
			}
		case <-deadline:
			t.Fatalf("after the restart the platform was told %v; want both bookings and both processings", seen)
		}
	}
	if seen["OutgoingPaymentBooked"] != "pay-restart" || seen["OutgoingPaymentProcessed"] != "pay-restart" {
		t.Fatalf("notifications about the payout: %v", seen)
	}
	if got := rb.balance(tok, sbxSGA); got != "240.00" {
		t.Fatalf("sbx's safeguarding account once the payout processed = %s, want 240.00", got)
	}
	if p, err := b.engine.GetIn("sbx", "pay-restart"); err != nil || p.State != bankingcircle.NotificationOutgoingPaymentProcessed {
		t.Fatalf("the payout after the restart = %+v, %v", p, err)
	}

	// And the file follows: the processed payout is written without a stop.
	waitFor(t, "the processed payout in the file", func() bool {
		for _, p := range loadState(t, path).Payments {
			if p.ID == "pay-restart" {
				return p.State == bankingcircle.NotificationOutgoingPaymentProcessed
			}
		}
		return false
	})
}

// A reset writes the file before it answers, and a deleted customer's
// records leave it before the delete answers: a restart -- or a crash
// right after -- never brings either back.
func TestAResetAndADeletedCustomerStayGoneAfterARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	a := openTestApp(t, path, time.Hour, time.Hour, nil)
	r := rigFor(t, a)
	mustCall(t, 200, "PUT", r.ctl+"/sim/customers/sbx", "", sbxCustomer)
	tok := r.login("sbx-client", "sbx-secret")
	r.bridgeCall(202, "/internal/incoming-payments", "sbx", `{"currency": "EUR", "amount": "100.00"}`)
	r.payout("sbx", "pay-reset")

	mustCall(t, 200, "POST", r.ctl+"/sim/reset", "", `{"scope": "flow", "env": "sbx"}`)
	s := loadState(t, path)
	for _, p := range s.Payments {
		if p.Env == "sbx" {
			t.Fatalf("a payment the reset cleared is still in the file: %+v", p)
		}
	}
	for _, acc := range s.Accounts {
		if acc.ID == sbxSGA && acc.Balance != "0.00" {
			t.Fatalf("sbx's safeguarding account in the file after the reset = %s", acc.Balance)
		}
	}

	mustCall(t, 200, "DELETE", r.ctl+"/sim/customers/sbx", "", "")
	s = loadState(t, path)
	for _, acc := range s.Accounts {
		if acc.Env == "sbx" {
			t.Fatalf("a deleted customer's account is still in the file: %+v", acc)
		}
	}
	for _, tk := range s.Tokens {
		if tk.Env == "sbx" {
			t.Fatal("a deleted customer's token is still in the file")
		}
	}
	waitFor(t, "the customer to leave the file", func() bool { return len(loadState(t, path).Customers) == 0 })

	a.stop()
	b := openTestApp(t, path, 5*time.Millisecond, 5*time.Millisecond, nil)
	rb := rigFor(t, b)
	if got := mustCall(t, 200, "GET", rb.ctl+"/sim/customers", "", "")["customers"].([]any); len(got) != 1 {
		t.Fatalf("customers after the restart = %v, want the default alone", got)
	}
	if code, _ := call(t, "GET", rb.vendor+"/api/v1/accounts/"+sbxSGA+"/balances", tok, ""); code != 401 {
		t.Fatalf("a deleted customer's token after the restart: %d, want 401", code)
	}
	if _, err := b.ledger.Get(sbxSGA); err == nil {
		t.Fatal("a deleted customer's safeguarding account came back with the restart")
	}
}

// A payout a trap holds is the trap's to move, and traps do not survive a
// restart: the payout stays where the trap had it rather than being walked
// the bank's own way.
func TestAPayoutATrapHeldStaysWhereItStoppedAfterARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	a := openTestApp(t, path, 5*time.Millisecond, 5*time.Millisecond, nil)
	r := rigFor(t, a)
	r.bridgeCall(202, "/internal/incoming-payments", "", `{"currency": "EUR", "amount": "50.00"}`)
	mustCall(t, 200, "POST", r.ctl+"/sim/payments/outcomes", "", `{"outcomes": ["Pending"]}`)
	r.payout("", "pay-held")
	r.payout("", "pay-walked")
	waitFor(t, "the untouched payout", func() bool {
		p, err := a.engine.Get("pay-walked")
		return err == nil && p.State == bankingcircle.NotificationOutgoingPaymentProcessed
	})
	a.stop()

	b := openTestApp(t, path, 5*time.Millisecond, 5*time.Millisecond, nil)
	time.Sleep(50 * time.Millisecond) // ten times the processing delay
	if p, err := b.engine.Get("pay-held"); err != nil || p.State != bankingcircle.NotificationOutgoingPaymentBooked {
		t.Fatalf("the held payout after the restart = %+v, %v; want still booked", p, err)
	}
	if acc, _ := b.ledger.Get(bankingcircle.SGAAccountEUR); acc.Balance != "40.00" {
		t.Fatalf("the safeguarding account after the restart = %s, want 40.00 (only the untouched payout out)", acc.Balance)
	}
}

// Start-up seeds on the first start only: after that the seed is the
// file's, and a reset of scope all after a restart puts back the same
// seeded subscription, id and all.
func TestTheFirstStartsSeedIsTheOneARestartKeeps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	seeds := 0
	seed := func(a *app) error {
		seeds++
		_, err := a.subs.Create(bankingcircle.CreateParams{Endpoint: sinkSubscriber(t), EncryptionKey: string(a.notifKey), Status: bankingcircle.StatusActive})
		return err
	}
	a := openTestApp(t, path, 5*time.Millisecond, 5*time.Millisecond, seed)
	list, _ := a.subs.List(1, 10)
	if seeds != 1 || len(list) != 1 {
		t.Fatalf("first start: %d seed(s), %d subscription(s)", seeds, len(list))
	}
	seeded := list[0].ID
	a.stop()

	b := openTestApp(t, path, 5*time.Millisecond, 5*time.Millisecond, seed)
	if seeds != 1 {
		t.Fatal("a restart seeded again")
	}
	rb := rigFor(t, b)
	tok := rb.login("anyone", "anything")
	rb.subscribe(tok, sinkSubscriber(t))
	mustCall(t, 200, "POST", rb.ctl+"/sim/reset", "", `{"scope": "all"}`)
	if list, total := b.subs.ListIn(tenant.Default, 1, 10); total != 1 || list[0].ID != seeded {
		t.Fatalf("a reset of scope all after the restart left %+v; want the first start's seed %s", list, seeded)
	}
}

// No path, no file: everything stays in memory, as the tests and a
// deliberately ephemeral run want.
func TestAnEmptyStatePathKeepsEverythingInMemory(t *testing.T) {
	a := openTestApp(t, "", 5*time.Millisecond, 5*time.Millisecond, func(*app) error { return nil })
	a.state.touch()
	if err := a.state.flush(); err != nil {
		t.Fatal(err)
	}
	a.stop()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
