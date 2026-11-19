package trap

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestARespondTrapTakesExactlyOneEffect(t *testing.T) {
	s := NewStore("sim", HTTPRequestHook())
	for body, want := range map[string]string{
		`{"hook":"http.request","match":{},"effect":{"respond":{"status":503}}}`:               "",
		`{"hook":"http.request","match":{},"effect":{"delay":"8s"}}`:                           "",
		`{"hook":"http.request","match":{},"effect":{"close":true}}`:                           "",
		`{"hook":"http.request","match":{},"effect":{}}`:                                       "one of",
		`{"hook":"http.request","match":{},"effect":{"close":true,"delay":"1s"}}`:              "one of",
		`{"hook":"http.request","match":{},"effect":{"path":[]}}`:                              "one of",
		`{"hook":"http.request","match":{},"effect":{"respond":{"status":42}}}`:                "100 to 599",
		`{"hook":"http.request","match":{},"effect":{"delay":"soon"}}`:                         "duration",
		`{"hook":"http.request","match":{"header.x":"y"},"effect":{"respond":{"status":503}}}`: "matches on method, path",
		`{"hook":"http.request","match":{},"effect":{"commit":true,"respond":{"status":502}}}`: "",
		`{"hook":"http.request","match":{},"effect":{"commit":true,"close":true}}`:             "",
		`{"hook":"http.request","match":{},"effect":{"commit":true,"delay":"1s"}}`:             "commit goes with respond or close",
		`{"hook":"http.request","match":{},"effect":{"commit":true}}`:                          "one of",
	} {
		spec, err := ParseSpec([]byte(body))
		if err == nil {
			_, err = s.Arm(spec)
		}
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: %v", body, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: %v, want an error about %q", body, err, want)
		}
	}
}

// TestAPathHookRefusesARespondEffect: a path hook takes only a path, so a
// trap whose view says it closes the connection cannot quietly walk a path.
func TestAPathHookRefusesARespondEffect(t *testing.T) {
	s := NewStore("sim", Hook{Name: "payment.outgoing", Effect: EffectPath, Graph: &Graph{Start: "Accepted", Next: map[string][]string{}}})
	for _, body := range []string{
		`{"hook":"payment.outgoing","match":{},"effect":{"path":[],"close":true}}`,
		`{"hook":"payment.outgoing","match":{},"effect":{"path":[],"delay":"1s"}}`,
		`{"hook":"payment.outgoing","match":{},"effect":{"path":[],"respond":{"status":503}}}`,
	} {
		spec, err := ParseSpec([]byte(body))
		if err == nil {
			_, err = s.Arm(spec)
		}
		if err == nil || !strings.Contains(err.Error(), "for http.request") {
			t.Errorf("%s: %v, want it refused", body, err)
		}
	}
	if spec, err := ParseSpec([]byte(`{"hook":"payment.outgoing","match":{},"effect":{"path":[]}}`)); err != nil {
		t.Fatal(err)
	} else if _, err := s.Arm(spec); err != nil {
		t.Errorf("a plain path: %v", err)
	}
}

func TestAPathPatternMatchesOneSegmentPerStar(t *testing.T) {
	for pattern, cases := range map[string]map[string]bool{
		"/a/*/c": {"/a/b/c": true, "/a/b/d": false, "/a/b/c/d": false, "/a/c": false},
		"/a/b":   {"/a/b": true, "/a/b/c": false},
	} {
		for path, want := range cases {
			if got := pathMatches(pattern, path); got != want {
				t.Errorf("%s against %s: %v, want %v", pattern, path, got, want)
			}
		}
	}
}

func serveThrough(t *testing.T, s *Store, effect string) (handled int, rec *httptest.ResponseRecorder) {
	t.Helper()
	spec, err := ParseSpec([]byte(`{"hook":"http.request","match":{"method":"POST","path":"/things"},"effect":` + effect + `}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Arm(spec); err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handled++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"thing_1"}`))
	})
	rec = httptest.NewRecorder()
	s.Serve(rec, httptest.NewRequest("POST", "/things", nil), "", next)
	return handled, rec
}

func TestARespondTrapAnswersInsteadOfTheSim(t *testing.T) {
	s := NewStore("sim", HTTPRequestHook())
	handled, rec := serveThrough(t, s, `{"respond":{"status":503,"body":"down"}}`)
	if handled != 0 || rec.Code != 503 || rec.Body.String() != "down" {
		t.Errorf("handled %d times, answered %d %q; want the trap's answer and no effect", handled, rec.Code, rec.Body.String())
	}
}

func TestACommitTrapLetsTheRequestTakeEffectThenReplacesTheAnswer(t *testing.T) {
	s := NewStore("sim", HTTPRequestHook())
	handled, rec := serveThrough(t, s, `{"commit":true,"respond":{"status":502,"body":{"message":"Bad Gateway"}}}`)
	if handled != 1 || rec.Code != 502 || !strings.Contains(rec.Body.String(), "Bad Gateway") || strings.Contains(rec.Body.String(), "thing_1") {
		t.Errorf("handled %d times, answered %d %q; want it handled once and the trap's answer sent", handled, rec.Code, rec.Body.String())
	}
	traps := s.List("")
	if len(traps) != 1 || len(traps[0].Caught) != 1 {
		t.Fatalf("traps = %+v", traps)
	}
	var steps []string
	for _, a := range traps[0].Caught[0].Applied {
		steps = append(steps, a.To)
	}
	if strings.Join(steps, ", ") != "handled 201, respond 502" {
		t.Errorf("applied = %v", steps)
	}
}

func TestACommitTrapCanDropTheConnectionAfterTheRequestTookEffect(t *testing.T) {
	s := NewStore("sim", HTTPRequestHook())
	defer func() {
		if recover() != http.ErrAbortHandler {
			t.Error("the connection was not dropped")
		}
	}()
	handled, _ := serveThrough(t, s, `{"commit":true,"close":true}`)
	t.Errorf("served after close, handled %d times", handled)
}
