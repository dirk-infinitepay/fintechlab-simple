package simctl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func noReset(context.Context, ResetRequest) (ResetResult, error) { return ResetResult{}, nil }

func TestTheControlSurfaceHasTheContractAndItsOwnHealth(t *testing.T) {
	t.Setenv("SIM_CONTROL_LISTEN", "")
	t.Setenv("SIM_CONTROL_ALLOW", "")
	c, err := NewControl(Config{Service: "test-sim", Reset: noReset}, ":19999")
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr() != ":19999" {
		t.Errorf("addr = %q, want the default", c.Addr())
	}
	c.Mux.HandleFunc("POST /sim/payments/{id}/return", func(w http.ResponseWriter, r *http.Request) {})
	for _, path := range []string{"/health", "/sim/info", "/sim/logs"} {
		w := httptest.NewRecorder()
		c.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if path != "/sim/logs" && w.Code != 200 {
			t.Errorf("GET %s = %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	c.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/sim/info", nil))
	if !strings.Contains(w.Body.String(), `"control_listener":":19999"`) {
		t.Errorf("info does not name the control listener: %s", w.Body)
	}
}

func TestSimControlListenMovesIt(t *testing.T) {
	t.Setenv("SIM_CONTROL_LISTEN", ":20001")
	c, err := NewControl(Config{Service: "test-sim", Reset: noReset}, ":19999")
	if err != nil || c.Addr() != ":20001" {
		t.Fatalf("addr = %q, %v", c.Addr(), err)
	}
}

func TestTheAllowlistLetsOnlyItsAddressesIn(t *testing.T) {
	t.Setenv("SIM_CONTROL_LISTEN", "")
	t.Setenv("SIM_CONTROL_ALLOW", "10.89.0.0/16, 127.0.0.1")
	c, err := NewControl(Config{Service: "test-sim", Reset: noReset}, ":19999")
	if err != nil {
		t.Fatal(err)
	}
	for remote, want := range map[string]int{
		"10.89.3.7:5000":   200,
		"127.0.0.1:5000":   200,
		"192.168.1.9:5000": 403,
		"[::1]:5000":       403,
	} {
		r := httptest.NewRequest("GET", "/health", nil)
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		c.Handler().ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("from %s = %d, want %d", remote, w.Code, want)
		}
	}
	t.Setenv("SIM_CONTROL_ALLOW", "not-an-address")
	if _, err := NewControl(Config{Service: "test-sim", Reset: noReset}, ":19999"); err == nil {
		t.Error("a bad allowlist was accepted")
	}
}

func TestTheControlAddressIsTheVendorPortPlus1000(t *testing.T) {
	for in, want := range map[string]string{":8085": ":9085", "127.0.0.1:8443": "127.0.0.1:9443", "nope": "", ":65000": ""} {
		if got := ControlAddr(in); got != want {
			t.Errorf("ControlAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNoControlAddressIsRefused(t *testing.T) {
	t.Setenv("SIM_CONTROL_LISTEN", "")
	if _, err := NewControl(Config{Service: "test-sim", Reset: noReset}, ""); err == nil {
		t.Error("a control port with no address was accepted")
	}
}
