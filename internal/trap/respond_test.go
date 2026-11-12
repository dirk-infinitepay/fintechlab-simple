package trap

import (
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
