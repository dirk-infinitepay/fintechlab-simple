package httputilx

import (
	"net/http"
	"strings"
)

// Forwarded is the first entry of X-Forwarded-Host or X-Forwarded-Proto,
// trimmed. Each proxy in a chain appends its own entry describing the same
// client request, so after two the header reads "lab.example, b4b"; the
// first is what the client used. Empty when the header is absent.
func Forwarded(r *http.Request, name string) string {
	first, _, _ := strings.Cut(r.Header.Get(name), ",")
	return strings.TrimSpace(first)
}

// ForwardedPrefix is the path the proxies in front stripped, from
// X-Forwarded-Prefix: each proxy adds the part it stripped, in one
// comma-separated value or a header of its own, so behind /lab then /b4b
// the header reads "/lab, /b4b" and the prefix is "/lab/b4b". Empty when
// there is none.
func ForwardedPrefix(r *http.Request) string {
	var parts []string
	for _, value := range r.Header.Values("X-Forwarded-Prefix") {
		for _, entry := range strings.Split(value, ",") {
			if part := strings.Trim(strings.TrimSpace(entry), "/"); part != "" {
				parts = append(parts, part)
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "/" + strings.Join(parts, "/")
}
