package httputilx

import (
	"net/http"
	"testing"
)

func TestForwardedPrefixJoinsEveryProxysPart(t *testing.T) {
	for name, values := range map[string][]string{
		"one value":         {"/lab, /b4b"},
		"a value per proxy": {"/lab", "/b4b/"},
	} {
		r, _ := http.NewRequest("GET", "/", nil)
		for _, value := range values {
			r.Header.Add("X-Forwarded-Prefix", value)
		}
		if got := ForwardedPrefix(r); got != "/lab/b4b" {
			t.Errorf("%s: ForwardedPrefix = %q, want /lab/b4b", name, got)
		}
	}
}
