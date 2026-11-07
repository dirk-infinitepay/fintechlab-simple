package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func serve(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, r))
	return w
}

func mustHandler(t *testing.T, labAPI, token string) http.Handler {
	t.Helper()
	h, err := handler(labAPI, token)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestUIIsServedAndNeedsNoNetwork(t *testing.T) {
	h := mustHandler(t, "http://127.0.0.1:1", "")
	w := serve(t, h, "GET", "/", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<!doctype html>") {
		t.Fatalf("index not served: %d", w.Code)
	}
	external := regexp.MustCompile(`(?i)(https?:)?//(?:cdn|unpkg|fonts\.|ajax\.|code\.jquery)`)
	for _, asset := range []string{"/app.css", "/app.js"} {
		w := serve(t, h, "GET", asset, "")
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("%s not served: %d", asset, w.Code)
		}
		// The lab's promise is one `make up` on a laptop with no network.
		// A CDN reference in the console would quietly break that.
		if loc := external.FindString(w.Body.String()); loc != "" {
			t.Errorf("%s reaches out to %q; the console must work offline", asset, loc)
		}
	}
}

func TestHiddenElementsStayHidden(t *testing.T) {
	h := mustHandler(t, "http://127.0.0.1:1", "")
	css := serve(t, h, "GET", "/app.css", "").Body.String()
	// .modal-backdrop sets display, and a class selector beats the
	// browser's own bare [hidden] rule -- so without an explicit override
	// the modal backdrop covers and blurs the whole page from first paint,
	// which is exactly what it did.
	if !regexp.MustCompile(`\[hidden\]\s*\{[^}]*display:\s*none\s*!important`).MatchString(css) {
		t.Fatal("app.css does not force [hidden] to display:none; anything with a display-setting class will show through the attribute")
	}
	html := serve(t, h, "GET", "/", "").Body.String()
	if !strings.Contains(html, `id="modal-backdrop" hidden`) {
		t.Fatal("the modal backdrop does not start hidden")
	}
}

// The page calls the lab API through this origin, so every call it makes
// has to arrive at the lab API as it was made: the /v1 path, and the
// console's old /api path that a running plugin still registers at.
func TestTheAPIIsForwardedAsItWasMade(t *testing.T) {
	type seen struct{ method, path, query, body, auth string }
	var got []seen
	lab := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, seen{r.Method, r.URL.Path, r.URL.RawQuery, string(b), r.Header.Get("Authorization")})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":"the lab API's own words","status":409}`))
	}))
	defer lab.Close()
	h := mustHandler(t, lab.URL, "")

	w := serve(t, h, "POST", "/v1/clock?x=1", `{"advance":"1d"}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "the lab API's own words") {
		t.Fatalf("answer %d %s: the lab API's answer should come back as it was", w.Code, w.Body.String())
	}
	serve(t, h, "POST", "/api/plugins", `{"id":"p"}`)
	serve(t, h, "GET", "/v1", "")
	want := []seen{
		{"POST", "/v1/clock", "x=1", `{"advance":"1d"}`, ""},
		{"POST", "/api/plugins", "", `{"id":"p"}`, ""},
		{"GET", "/v1", "", "", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("lab API saw %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d: lab API saw %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestTheTokenIsAddedWhenOneIsSet(t *testing.T) {
	var auth string
	lab := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
	}))
	defer lab.Close()
	serve(t, mustHandler(t, lab.URL, "s3cret"), "GET", "/v1/lab", "")
	if auth != "Bearer s3cret" {
		t.Errorf("lab API saw Authorization %q", auth)
	}
}

func TestAnUnreachableLabAPIIsSaidSo(t *testing.T) {
	lab := httptest.NewServer(http.NotFoundHandler())
	url := lab.URL
	lab.Close()
	w := serve(t, mustHandler(t, url, ""), "GET", "/v1/lab", "")
	if w.Code != 502 || !strings.Contains(w.Body.String(), "lab API") {
		t.Fatalf("answer %d %s: the page shows this, so it should name the lab API", w.Code, w.Body.String())
	}
}

// TestEveryAnswerNamesThePageItServes: a tab left open across a rebuild runs
// the page it loaded, so the page compares this header with the one it first
// saw. The page and the API's answers must carry the same value, and it must
// not move between requests to the same build.
func TestEveryAnswerNamesThePageItServes(t *testing.T) {
	lab := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer lab.Close()
	h := mustHandler(t, lab.URL, "")
	builds := map[string]bool{}
	for _, path := range []string{"/", "/app.js", "/health", "/v1/lab", "/api/overview"} {
		builds[serve(t, h, "GET", path, "").Header().Get("X-Console-Build")] = true
	}
	if len(builds) != 1 || builds[""] {
		t.Fatalf("X-Console-Build across the page and the API: %v", builds)
	}
}

func TestALabAPIURLThatIsNotOneIsRefused(t *testing.T) {
	if _, err := handler("lab-api:8097", ""); err == nil {
		t.Error("a LAB_API_URL without a scheme was accepted")
	}
}
