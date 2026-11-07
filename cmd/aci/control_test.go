package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// raw makes one request and returns the answer as it came.
func raw(t *testing.T, srv *httptest.Server, method, path, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
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

// The control port serves every /sim route, and none of the /internal
// ones: those stay on ACI's own listener until phase 4.
func TestTheControlPortServesEverySimRoute(t *testing.T) {
	tg := newTarget(t, 200)
	_, _, ctl := testServers(t, tg)

	if st, h := call(t, ctl, "GET", "/health", ""); st != 200 || h["service"] != "aci" || h["surface"] != "control" {
		t.Errorf("GET /health on the control port = %d %v", st, h)
	}
	if st, info := call(t, ctl, "GET", "/sim/info", ""); st != 200 || info["service"] != "aci" ||
		info["control_listener"] != ":9087" || fmt.Sprint(info["vendor_listeners"]) != "[:8087]" {
		t.Errorf("GET /sim/info = %d %v", st, info)
	}
	// 200 with a log ring, 501 without: either is the route's own answer.
	if st, _ := call(t, ctl, "GET", "/sim/logs", ""); st == 404 || st == 405 {
		t.Errorf("GET /sim/logs = %d", st)
	}

	st, res := call(t, ctl, "POST", "/sim/payments", `{"merchant_transaction_id": "tx-ctl"}`)
	id, _ := res["id"].(string)
	if st != 202 || !strings.HasPrefix(id, "aci_evt_") {
		t.Fatalf("POST /sim/payments = %d %v", st, res)
	}
	waitFor(t, "the delivery", func() bool {
		_, rec := call(t, ctl, "GET", "/sim/sent/"+id, "")
		return rec["delivery_status"] == "delivered"
	})
	if tg.attempts() != 1 {
		t.Errorf("%d attempt(s) reached the target, want 1", tg.attempts())
	}
	if st, list := call(t, ctl, "GET", "/sim/sent", ""); st != 200 || len(list["result"].([]any)) != 1 {
		t.Errorf("GET /sim/sent = %d %v", st, list)
	}

	if st, sw := call(t, ctl, "GET", "/sim/connected", ""); st != 200 || sw["connected"] != true {
		t.Errorf("GET /sim/connected = %d %v", st, sw)
	}
	if st, sw := call(t, ctl, "POST", "/sim/connected", `{"connected": false}`); st != 200 || sw["connected"] != false {
		t.Errorf("POST /sim/connected = %d %v", st, sw)
	}
	if st, res := call(t, ctl, "POST", "/sim/reset", ""); st != 200 || res["service"] != "aci" {
		t.Errorf("POST /sim/reset = %d %v", st, res)
	}

	for _, r := range [][2]string{{"POST", "/internal/simulate-payment"}, {"GET", "/internal/sent"}, {"GET", "/internal/sent/" + id}} {
		if st, _ := call(t, ctl, r[0], r[1], `{"merchant_transaction_id": "tx"}`); st != 404 {
			t.Errorf("%s %s on the control port = %d, want 404", r[0], r[1], st)
		}
	}
}

// POST /sim/payments, GET /sim/sent and GET /sim/sent/{id} are the
// /internal routes under their control-port names: the same answers for
// the same calls, refusals included.
func TestTheControlRoutesAnswerAsTheirInternalCounterparts(t *testing.T) {
	tg := newTarget(t, 200)
	a, vendor, ctl := testServers(t, tg)
	// Held, so no delivery moves a record between two reads of it.
	a.wiring.Set(false, "the records must hold still while they are compared")

	for _, body := range []string{
		`{}`,
		`{"merchant_transaction_id": "tx-1", "amount": "1.00"}`, // strict: an unknown field
		`{"merchant_transaction_id": `,
	} {
		vst, vb := raw(t, vendor, "POST", "/internal/simulate-payment", body)
		cst, cb := raw(t, ctl, "POST", "/sim/payments", body)
		if vst != 400 || cst != vst || !bytes.Equal(cb, vb) {
			t.Errorf("%s: POST /sim/payments = %d %s\nPOST /internal/simulate-payment = %d %s", body, cst, cb, vst, vb)
		}
	}

	full := `{"merchant_transaction_id": "tx-2", "payment_type": "RX", "payment_brand": "VISA",
	          "presentation_amount": "42.50", "presentation_currency": "EUR", "plugin_type": "SHOPFY",
	          "source": "OPP", "result_code": "000.300.100", "result_description": "Risk check successful"}`
	vst, vres := call(t, vendor, "POST", "/internal/simulate-payment", full)
	cst, cres := call(t, ctl, "POST", "/sim/payments", full)
	vid, _ := vres["id"].(string)
	cid, _ := cres["id"].(string)
	if vst != 202 || cst != 202 || len(vres) != 1 || len(cres) != 1 ||
		!strings.HasPrefix(vid, "aci_evt_") || !strings.HasPrefix(cid, "aci_evt_") {
		t.Fatalf("POST /sim/payments = %d %v, POST /internal/simulate-payment = %d %v", cst, cres, vst, vres)
	}
	// The two records differ only in what is minted fresh each time: their
	// ids, the payload's ids, the IV and ciphertext, the moment.
	_, vrec := call(t, vendor, "GET", "/internal/sent/"+vid, "")
	_, crec := call(t, vendor, "GET", "/internal/sent/"+cid, "")
	for _, k := range []string{"delivery_status", "delivery_error", "target_url", "body_mode"} {
		if fmt.Sprint(vrec[k]) != fmt.Sprint(crec[k]) {
			t.Errorf("%s: %v by POST /sim/payments, %v by POST /internal/simulate-payment", k, crec[k], vrec[k])
		}
	}
	vp := vrec["notification"].(map[string]any)["payload"].(map[string]any)
	cp := crec["notification"].(map[string]any)["payload"].(map[string]any)
	for _, k := range []string{"merchantTransactionId", "paymentType", "paymentBrand", "presentationAmount", "presentationCurrency", "pluginType", "source", "result"} {
		if fmt.Sprint(vp[k]) != fmt.Sprint(cp[k]) {
			t.Errorf("payload %s: %v by POST /sim/payments, %v by POST /internal/simulate-payment", k, cp[k], vp[k])
		}
	}
	if crec["delivery_status"] != "held" {
		t.Errorf("delivery_status = %v, want held: the switch is off", crec["delivery_status"])
	}

	// The reads, byte for byte.
	for _, p := range [][2]string{
		{"/internal/sent", "/sim/sent"},
		{"/internal/sent/" + vid, "/sim/sent/" + vid},
		{"/internal/sent/" + cid, "/sim/sent/" + cid},
		{"/internal/sent/aci_evt_never", "/sim/sent/aci_evt_never"},
	} {
		vst, vb := raw(t, vendor, "GET", p[0], "")
		cst, cb := raw(t, ctl, "GET", p[1], "")
		if cst != vst || !bytes.Equal(cb, vb) {
			t.Errorf("GET %s = %d %s\nGET %s = %d %s", p[1], cst, cb, p[0], vst, vb)
		}
	}
	if st, list := call(t, ctl, "GET", "/sim/sent", ""); st != 200 || len(list["result"].([]any)) != 2 {
		t.Errorf("GET /sim/sent = %d %v, want both records", st, list)
	}
	if st, _ := raw(t, ctl, "GET", "/sim/sent/aci_evt_never", ""); st != 404 {
		t.Errorf("GET /sim/sent/aci_evt_never = %d, want 404", st)
	}
	if tg.attempts() != 0 {
		t.Errorf("%d attempt(s) with the switch off", tg.attempts())
	}
}
