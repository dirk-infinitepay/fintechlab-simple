package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// faultServer is verification's vendor surface behind its http.request
// traps, as main serves it, and the control port to arm them on.
func faultServer(t *testing.T) (*httptest.Server, http.Handler) {
	t.Helper()
	a := newApp()
	svc, ctl := surfaces(t, a)
	srv := httptest.NewServer(a.faults(svc))
	t.Cleanup(srv.Close)
	return srv, ctl.Handler()
}

func armFault(t *testing.T, ctl http.Handler, body string) {
	t.Helper()
	if w := serve(ctl, http.MethodPost, "/sim/traps", body); w.Code != http.StatusCreated {
		t.Fatalf("arm: %d %s", w.Code, w.Body)
	}
}

func ibanCheck(srv *httptest.Server) (*http.Response, error) {
	r, _ := http.NewRequest(http.MethodPost, srv.URL+"/iban/verify", strings.NewReader(`{"IBAN":"GB00X","name":"Southwind"}`))
	r.Header.Set("x-api-key", "sim-key")
	return http.DefaultClient.Do(r)
}

// TestATrapAnswersTheNextRequestInsteadOfTheVendor: an outage for the next
// IBAN.com check, then the vendor answers again.
func TestATrapAnswersTheNextRequestInsteadOfTheVendor(t *testing.T) {
	srv, ctl := faultServer(t)
	armFault(t, ctl, `{"hook":"http.request","match":{"method":"POST","path":"/iban/verify"},
		"effect":{"respond":{"status":503,"body":{"error":"Service Unavailable"}}}}`)

	res, err := ibanCheck(srv)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 503 || !strings.Contains(string(body), "Service Unavailable") {
		t.Errorf("trapped: %d %s, want the trap's 503", res.StatusCode, body)
	}

	res, err = ibanCheck(srv)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		t.Errorf("after the trap was spent: %d, want the vendor's 200", res.StatusCode)
	}
}

// TestATrapCanSendAReplyThatIsNotJSON: a string body goes out as it is.
func TestATrapCanSendAReplyThatIsNotJSON(t *testing.T) {
	srv, ctl := faultServer(t)
	armFault(t, ctl, `{"hook":"http.request","match":{"path":"/iban/verify"},
		"effect":{"respond":{"status":200,"body":"<html>gateway hiccup</html>"}}}`)

	res, err := ibanCheck(srv)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || string(body) != "<html>gateway hiccup</html>" || res.Header.Get("Content-Type") != "text/plain" {
		t.Errorf("unreadable reply: %d %q %q", res.StatusCode, body, res.Header.Get("Content-Type"))
	}
}

// TestATrapPathMatchesOneSegmentPerStar: a company report by any id.
func TestATrapPathMatchesOneSegmentPerStar(t *testing.T) {
	srv, ctl := faultServer(t)
	armFault(t, ctl, `{"hook":"http.request","match":{"path":"/creditsafe/companies/*"},
		"effect":{"respond":{"status":500,"body":{"message":"Internal Server Error"}}},"times":null}`)

	for path, want := range map[string]int{"/creditsafe/companies/GB-0-1": 500, "/creditsafe/companies": 401} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != want {
			t.Errorf("GET %s: %d, want %d", path, res.StatusCode, want)
		}
	}
}

// TestATrapCanDropTheConnection: the client gets no answer at all.
func TestATrapCanDropTheConnection(t *testing.T) {
	srv, ctl := faultServer(t)
	armFault(t, ctl, `{"hook":"http.request","match":{"path":"/iban/verify"},"effect":{"close":true}}`)

	if res, err := ibanCheck(srv); err == nil {
		t.Errorf("a dropped connection answered %d", res.StatusCode)
	}
}

// TestATrapCanDelayTheVendor: the vendor answers as usual, late.
func TestATrapCanDelayTheVendor(t *testing.T) {
	srv, ctl := faultServer(t)
	armFault(t, ctl, `{"hook":"http.request","match":{"path":"/iban/verify"},"effect":{"delay":"200ms"}}`)

	started := time.Now()
	res, err := ibanCheck(srv)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || time.Since(started) < 200*time.Millisecond {
		t.Errorf("delayed: %d after %v, want 200 after at least 200ms", res.StatusCode, time.Since(started))
	}
}

// TestTheLabSeamIsNeverTrapped: a trap on every request leaves /health and
// the /sim paths alone, so it cannot lock the lab out.
func TestTheLabSeamIsNeverTrapped(t *testing.T) {
	srv, ctl := faultServer(t)
	armFault(t, ctl, `{"hook":"http.request","match":{},"effect":{"respond":{"status":503}},"times":null}`)

	for _, path := range []string{"/health", "/sim/outcome"} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != 200 {
			t.Errorf("GET %s: %d, want 200", path, res.StatusCode)
		}
	}
}
