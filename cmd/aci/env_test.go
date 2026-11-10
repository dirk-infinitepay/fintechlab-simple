package main

import (
	"strings"
	"testing"
)

// localhost is tg's URL by the name "localhost": a host the tests'
// WEBHOOK_ALLOWLIST ("127.0.0.1") does not name, so only an environment's
// webhook_hosts can let a delivery through to it.
func localhost(tg *target) string {
	return strings.Replace(tg.srv.URL, "127.0.0.1", "localhost", 1)
}

func sentIDs(t *testing.T, list map[string]any) []string {
	t.Helper()
	var ids []string
	for _, r := range list["result"].([]any) {
		ids = append(ids, r.(map[string]any)["id"].(string))
	}
	return ids
}

// Each environment's card payments go to its own platform, behind its own
// allowance, and each reads only its own sent log. The connected switch is
// the default's receiver stand-in, and holds only the default's.
func TestEachEnvironmentIsDeliveredToItsOwnTarget(t *testing.T) {
	def, sbxTarget := newTarget(t, 200), newTarget(t, 200)
	a, _, ctl := testServers(t, def)
	if st, res := call(t, ctl, "PUT", "/sim/customers/sbx",
		`{"env":"sbx","aci":{"webhook_url":"`+localhost(sbxTarget)+`/hook"},"webhook_hosts":["localhost"]}`); st != 200 {
		t.Fatalf("PUT /sim/customers/sbx = %d %v", st, res)
	}
	a.wiring.Set(false, "a platform listens itself")

	st, res := call(t, ctl, "POST", "/sim/payments?env=sbx", `{"merchant_transaction_id": "tx-sbx"}`)
	sbxID, _ := res["id"].(string)
	if st != 202 || sbxID == "" {
		t.Fatalf("POST /sim/payments?env=sbx = %d %v", st, res)
	}
	waitFor(t, "sbx's delivery", func() bool {
		_, rec := call(t, ctl, "GET", "/sim/sent/"+sbxID+"?env=sbx", "")
		return rec["delivery_status"] == "delivered"
	})
	_, defRes := call(t, ctl, "POST", "/sim/payments", `{"merchant_transaction_id": "tx-default"}`)
	defID, _ := defRes["id"].(string)
	_, rec := call(t, ctl, "GET", "/sim/sent/"+defID, "")
	if rec["delivery_status"] != "held" {
		t.Errorf("the default's payment with the switch off = %v, want held", rec)
	}
	if sbxTarget.attempts() != 1 || def.attempts() != 0 {
		t.Errorf("attempts: sbx's target %d, the default's %d; want 1, 0", sbxTarget.attempts(), def.attempts())
	}

	// Each reads its own.
	_, rec = call(t, ctl, "GET", "/sim/sent/"+sbxID+"?env=sbx", "")
	if rec["env"] != "sbx" || rec["target_url"] != localhost(sbxTarget)+"/hook" {
		t.Errorf("sbx's record = %v", rec)
	}
	if st, _ := call(t, ctl, "GET", "/sim/sent/"+sbxID, ""); st != 404 {
		t.Errorf("sbx's record read as the default's = %d, want 404", st)
	}
	if st, _ := call(t, ctl, "GET", "/sim/sent/"+defID+"?env=sbx", ""); st != 404 {
		t.Errorf("the default's record read as sbx's = %d, want 404", st)
	}
	// Oldest first.
	for q, want := range map[string]string{"": defID, "?env=sbx": sbxID, "?env=*": sbxID + "," + defID} {
		_, list := call(t, ctl, "GET", "/sim/sent"+q, "")
		if got := strings.Join(sentIDs(t, list), ","); got != want {
			t.Errorf("GET /sim/sent%s = %s, want %s", q, got, want)
		}
	}
}

// The trigger names one environment ACI has a target for.
func TestTheTriggerNamesAnEnvironmentWithATarget(t *testing.T) {
	tg := newTarget(t, 200)
	_, _, ctl := testServers(t, tg)
	call(t, ctl, "PUT", "/sim/customers/bare", `{"env":"bare"}`)
	for q, want := range map[string]int{"?env=*": 400, "?env=nope": 404, "?env=bare": 409} {
		if st, res := call(t, ctl, "POST", "/sim/payments"+q, `{"merchant_transaction_id": "tx"}`); st != want {
			t.Errorf("POST /sim/payments%s = %d %v, want %d", q, st, res, want)
		}
	}
	if st, _ := call(t, ctl, "GET", "/sim/sent?env=nope", ""); st != 404 {
		t.Errorf("GET /sim/sent in an unknown environment = %d, want 404", st)
	}
	if _, list := call(t, ctl, "GET", "/sim/sent?env=*", ""); len(list["result"].([]any)) != 0 {
		t.Errorf("refused triggers left records: %v", list)
	}
	if tg.attempts() != 0 {
		t.Errorf("%d attempt(s) from refused triggers", tg.attempts())
	}
}

// A target neither WEBHOOK_ALLOWLIST nor the environment's webhook_hosts
// names is not sent to, and the record says why.
func TestAnEnvironmentsTargetMustBeAllowed(t *testing.T) {
	def, sbxTarget := newTarget(t, 200), newTarget(t, 200)
	_, _, ctl := testServers(t, def)
	call(t, ctl, "PUT", "/sim/customers/sbx", `{"env":"sbx","aci":{"webhook_url":"`+localhost(sbxTarget)+`"}}`)
	_, res := call(t, ctl, "POST", "/sim/payments?env=sbx", `{"merchant_transaction_id": "tx"}`)
	id, _ := res["id"].(string)
	waitFor(t, "the refusal", func() bool {
		_, rec := call(t, ctl, "GET", "/sim/sent/"+id+"?env=sbx", "")
		return rec["delivery_status"] == "failed"
	})
	if _, rec := call(t, ctl, "GET", "/sim/sent/"+id+"?env=sbx", ""); !strings.Contains(rec["delivery_error"].(string), "not permitted") {
		t.Errorf("delivery_error = %v", rec["delivery_error"])
	}
	if sbxTarget.attempts() != 0 {
		t.Errorf("%d attempt(s) reached a target nobody allowed", sbxTarget.attempts())
	}
}

// A reset of one environment empties its sent log and drops its
// deliveries; the other's records stay and its retries go on. Deleting a
// customer does the same for it.
func TestAResetOfOneEnvironmentKeepsTheOthersDeliveries(t *testing.T) {
	def, sbxTarget := newTarget(t, 500), newTarget(t, 200)
	a, _, ctl := testServers(t, def)
	call(t, ctl, "PUT", "/sim/customers/sbx", `{"env":"sbx","aci":{"webhook_url":"`+sbxTarget.srv.URL+`"}}`)

	call(t, ctl, "POST", "/sim/payments", `{"merchant_transaction_id": "tx-default"}`)
	waitFor(t, "the default's first attempt", func() bool { return def.attempts() == 1 })
	_, res := call(t, ctl, "POST", "/sim/payments?env=sbx", `{"merchant_transaction_id": "tx-sbx"}`)
	sbxID, _ := res["id"].(string)
	waitFor(t, "sbx's delivery", func() bool {
		_, rec := call(t, ctl, "GET", "/sim/sent/"+sbxID+"?env=sbx", "")
		return rec["delivery_status"] == "delivered"
	})

	st, out := call(t, ctl, "POST", "/sim/reset", `{"env":"sbx"}`)
	if cleared, _ := out["cleared"].(map[string]any); st != 200 || out["env"] != "sbx" || cleared["sent"] != 1.0 {
		t.Fatalf("reset sbx = %d %v", st, out)
	}
	if _, list := call(t, ctl, "GET", "/sim/sent?env=sbx", ""); len(list["result"].([]any)) != 0 {
		t.Errorf("sbx's sent log after its reset = %v", list)
	}
	if _, list := call(t, ctl, "GET", "/sim/sent", ""); len(list["result"].([]any)) != 1 {
		t.Errorf("the default's sent log after sbx's reset = %v", list)
	}
	// The default's delivery is still retrying: MAX_ATTEMPTS (3) reach it.
	waitFor(t, "the default's retries", func() bool { return def.attempts() == 3 })

	call(t, ctl, "POST", "/sim/payments?env=sbx", `{"merchant_transaction_id": "tx-sbx-2"}`)
	if st, _ := call(t, ctl, "DELETE", "/sim/customers/sbx", ""); st != 200 {
		t.Fatalf("DELETE /sim/customers/sbx = %d", st)
	}
	if _, list := call(t, ctl, "GET", "/sim/sent?env=*", ""); len(list["result"].([]any)) != 1 {
		t.Errorf("after sbx was deleted, every sent log = %v, want the default's one", list)
	}
	waitFor(t, "every delivery to stop", func() bool { return a.sent.InFlight() == 0 })
}
