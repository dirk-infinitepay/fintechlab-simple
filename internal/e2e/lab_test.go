package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The suite reaches the lab only through the lab API and the vendor
// surfaces: no /sim or /internal path on a service's own URL, and no
// control port. A lab in the cloud exposes neither, and phase 4 retires the
// /sim aliases on the vendor ports.
func TestTheSuiteReachesTheLabOnlyThroughTheLabAPI(t *testing.T) {
	direct := regexp.MustCompile(`URL\s*\+\s*"/(sim|internal)/`)
	// Handed to B4B as where to call back; B4B calls it, not the suite.
	allowed := regexp.MustCompile(`SettlementURL \+ "/internal/b4b-webhook"`)
	controlPort := regexp.MustCompile(`(CONTROL_URL|INTERNAL_URL|:9[0-9]{3}\b|:19[0-9]{3}\b)`)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if direct.MatchString(line) && !allowed.MatchString(line) {
				t.Errorf("%s:%d calls a lab hook on a service's own URL; go through env.Lab: %s", f, i+1, strings.TrimSpace(line))
			}
			if controlPort.MatchString(line) {
				t.Errorf("%s:%d names a control port or a bridge URL: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

func TestLabCarriesTheTokenAndSaysWhatIsWrong(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer s3cret" {
			http.Error(w, `{"error":"token","status":401}`, http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v1/services/banking-circle/sim/subscription/sub_1/notifications" && r.URL.RawQuery == "count=5" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	ctx := context.Background()

	for token, want := range map[string]string{"": "wants a token: set LAB_API_TOKEN", "nope": "refused LAB_API_TOKEN"} {
		if err := newLab(srv.URL, token, "").Check(ctx); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("token %q: %v, want %q", token, err, want)
		}
	}
	lab := newLab(srv.URL+"/", "s3cret", "")
	if err := lab.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if status, err := lab.Post(ctx, Sim("banking-circle", "subscription/sub_1/notifications?count=5"), nil, nil); err != nil || status != 202 {
		t.Errorf("passthrough with a query = %d %v, want 202", status, err)
	}

	srv.Close()
	if err := lab.Check(ctx); err == nil || !strings.Contains(err.Error(), "does not answer (LAB_API_URL)") {
		t.Errorf("a lab API that is down: %v", err)
	}
}
