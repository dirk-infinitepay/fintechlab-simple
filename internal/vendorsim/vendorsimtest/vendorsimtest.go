// Package vendorsimtest drives a verification vendor sim in its tests.
package vendorsimtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Call drives one request through h the way the vendor's real client does
// and decodes the answer, for a vendor sim's tests.
func Call(t testing.TB, h http.Handler, method, path, body string, hdr map[string]string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// At reads a dotted path through decoded JSON objects.
func At(m map[string]any, path string) any {
	var current any = m
	for _, k := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[k]
	}
	return current
}
