package activity

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func payoutSummary(c *Call) (string, map[string]string) {
	return "payout " + c.JSONField("amount", "amount") + " to " + c.JSONField("beneficiary_id"),
		map[string]string{"payment_id": c.RespField("id")}
}

// TestWatchDoesNotStarveTheHandler is the property that matters most: the
// observer reads the request body, and a handler that then found an empty
// body would fail every call the moment logging was switched on.
func TestWatchDoesNotStarveTheHandler(t *testing.T) {
	l := New("in", "Payments received", "")
	var seen string
	h := l.Watch("payment.create", payoutSummary, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = string(b)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":"pay_1"}`))
	})

	body := `{"beneficiary_id":"ben_7","amount":{"amount":"10.00"}}`
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodPost, "/oversight/v1/payments", strings.NewReader(body)))

	if seen != body {
		t.Fatalf("handler read %q, want the whole body", seen)
	}
	if w.Code != 201 || w.Body.String() != `{"id":"pay_1"}` {
		t.Fatalf("the peer's answer was altered: %d %s", w.Code, w.Body.String())
	}

	ev := l.Recent(1)
	if len(ev) != 1 {
		t.Fatal("the call was not recorded")
	}
	if ev[0].Summary != "payout 10.00 to ben_7" {
		t.Errorf("summary = %q", ev[0].Summary)
	}
	if ev[0].Detail["payment_id"] != "pay_1" {
		t.Errorf("detail should carry what the vendor answered, got %v", ev[0].Detail)
	}
	if ev[0].Status != StatusOK {
		t.Errorf("status = %q, want ok", ev[0].Status)
	}
}

// TestWatchRecordsRefusalsWithTheVendorsOwnWords. The refusals are the
// reason this exists — "payout rejected" tells an operator nothing, and
// the whole value of the panel is that the vendor's sentence is in it.
func TestWatchRecordsRefusalsWithTheVendorsOwnWords(t *testing.T) {
	l := New("in", "Payments received", "")
	h := l.Watch("payment.create", payoutSummary, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		_, _ = w.Write([]byte(`{"error":"beneficiary ben_7 is sanctions-blocked"}`))
	})
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodPost, "/oversight/v1/payments", strings.NewReader(`{"beneficiary_id":"ben_7"}`)))

	ev := l.Recent(1)[0]
	if !strings.Contains(ev.Summary, "sanctions-blocked") {
		t.Errorf("summary = %q, want the vendor's own message", ev.Summary)
	}
	if ev.Status != StatusWarn {
		t.Errorf("status = %q — a refusal is amber, not red", ev.Status)
	}
	if ev.Detail["status"] != "Unprocessable Entity" {
		t.Errorf("detail status = %q", ev.Detail["status"])
	}
}

// TestWatchRecordsAHandlerThatNeverSetsAStatus, because net/http defaults
// a bare Write to 200 and an event that reported 0 would sort as bad.
func TestWatchRecordsAHandlerThatNeverSetsAStatus(t *testing.T) {
	l := New("in", "Calls", "")
	h := l.Watch("call", nil, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	ev := l.Recent(1)[0]
	if ev.Status != StatusOK {
		t.Errorf("status = %q, want ok", ev.Status)
	}
	if ev.Summary != "GET /x" {
		t.Errorf("summary = %q — without a summariser, say what was called", ev.Summary)
	}
}

// TestPeerPrefersTheForwardedAddress: these services are reached through a
// container bridge as often as not, and one bridge address for every call
// tells an operator nothing.
// TestWatchHandsTheHandlerABodyOfAnySize: only the journal's copy is
// capped; a handler that saw a cut-short body would answer for fields that
// were never read.
func TestWatchHandsTheHandlerABodyOfAnySize(t *testing.T) {
	l := New("in", "In", "")
	body := `{"name":"` + strings.Repeat("x", bodyLimit+1000) + `"}`
	var seen int
	var journalled int
	h := l.Watch("op", func(c *Call) (string, map[string]string) {
		journalled = len(c.ReqBody)
		return "", nil
	}, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = len(b)
	})
	h(httptest.NewRecorder(), httptest.NewRequest("POST", "/x", strings.NewReader(body)))
	if seen != len(body) || journalled != bodyLimit {
		t.Errorf("handler read %d bytes, journal kept %d; want %d and %d", seen, journalled, len(body), bodyLimit)
	}
}

// TestASummariserCanMarkARefusalTheVendorAnsweredWithA200: some vendors
// refuse in a 2xx of their own shape, and the journal must not call that ok.
func TestASummariserCanMarkARefusalTheVendorAnsweredWithA200(t *testing.T) {
	l := New("in", "In", "")
	h := l.Watch("op", func(c *Call) (string, map[string]string) {
		c.Refused = c.RespField("error")
		return "", nil
	}, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"error":"IBAN is required"}`))
	})
	h(httptest.NewRecorder(), httptest.NewRequest("POST", "/x", nil))
	e := l.Recent(1)[0]
	if e.Status != StatusWarn || !strings.Contains(e.Summary, "IBAN is required") {
		t.Errorf("event = %+v, want a warning carrying the refusal", e)
	}
}

func TestClearCountsTheEventsItHeld(t *testing.T) {
	l := New("in", "In", "")
	for i := 0; i < DefaultCapacity+44; i++ {
		l.Record(Event{Op: "op"})
	}
	if n := l.Clear(); n != DefaultCapacity {
		t.Errorf("Clear = %d, want the %d it held", n, DefaultCapacity)
	}
}

func TestPeerPrefersTheForwardedAddress(t *testing.T) {
	l := New("in", "Calls", "")
	h := l.Watch("call", nil, func(w http.ResponseWriter, r *http.Request) {})
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.RemoteAddr = "10.89.0.1:34512"
	r.Header.Set("X-Forwarded-For", "192.168.178.128, 10.89.0.1")
	h(httptest.NewRecorder(), r)

	if peer := l.Recent(1)[0].Peer; peer != "192.168.178.128" {
		t.Errorf("peer = %q, want the originating address", peer)
	}
}

// TestJSONFieldRefusesToGuess: a summariser runs on a body the peer
// controls, so a non-object, a missing key and a wrong type all have to be
// "" rather than a panic or an invented value.
func TestJSONFieldRefusesToGuess(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"id":"p1"}`, "p1"},
		{`{"id":42}`, "42"},
		{`{"other":"x"}`, ""},
		{`["not an object"]`, ""},
		{`not json at all`, ""},
		{``, ""},
	} {
		c := &Call{ReqBody: []byte(tc.body)}
		if got := c.JSONField("id"); got != tc.want {
			t.Errorf("JSONField on %s = %q, want %q", tc.body, got, tc.want)
		}
	}
}
