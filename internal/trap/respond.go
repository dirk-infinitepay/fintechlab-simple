package trap

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/activity"
)

// EffectRespond answers a vendor-shaped request instead of the sim
// (traps.md §4.3): with a response of the trap's own, after a delay, or by
// dropping the connection.
const EffectRespond = "respond"

// HookHTTPRequest is the hook every sim's vendor surface can offer
// (traps.md §9.7).
const HookHTTPRequest = "http.request"

// TypePath is a request path; "*" in a matched value stands for one
// segment.
const TypePath = "path"

// Response is what a respond trap answers with. A string body is sent as
// it is, so a test can send a reply that isn't JSON; anything else is
// sent as JSON.
type Response struct {
	Status  int               `json:"status"`
	Body    any               `json:"body,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// HTTPRequestHook is the http.request hook: any request on the sim's
// vendor surface, never on its control port.
func HTTPRequestHook() Hook {
	return Hook{
		Name: HookHTTPRequest,
		About: "Any request on the vendor surface. respond answers with the trap's status, body and headers instead, " +
			"and the request has no effect; delay handles it normally after the wait; close drops the connection with no answer.",
		Subject: "request",
		Match: []Field{
			{Name: "method", Type: TypeString, About: "GET, POST, …"},
			{Name: "path", Type: TypePath, About: "the request path, without the query; * stands for one segment"},
		},
		Effect: EffectRespond,
	}
}

func validateRespond(h Hook, spec Spec) error {
	set := 0
	if spec.Respond != nil {
		set++
		if spec.Respond.Status < 100 || spec.Respond.Status > 599 {
			return fmt.Errorf("%w: respond.status is an HTTP status, 100 to 599", ErrInvalid)
		}
	}
	if spec.Delay != "" {
		set++
		if _, err := parseDuration(spec.Delay); err != nil {
			return fmt.Errorf("%w: delay is a duration, like \"8s\"", ErrInvalid)
		}
	}
	if spec.Close {
		set++
	}
	if set != 1 || spec.Path != nil {
		return fmt.Errorf("%w: %s's effect is one of {\"respond\": {\"status\": …, \"body\": …, \"headers\": {…}}}, {\"delay\": \"8s\"} or {\"close\": true}", ErrInvalid, h.Name)
	}
	return nil
}

// Intercept offers r to the http.request traps armed in env. It reports
// true when a trap answered, or dropped the connection, so the sim must not
// handle the request; after a delay it reports false and the sim handles it
// as usual.
func (s *Store) Intercept(w http.ResponseWriter, r *http.Request, env string) bool {
	c, ok := s.Offer(Event{Env: env, Hook: HookHTTPRequest, Subject: r.Method + " " + r.URL.Path,
		Fields: map[string]string{"method": r.Method, "path": r.URL.Path}})
	if !ok {
		return false
	}
	effect := c.t.Effect
	switch {
	case effect.Respond != nil:
		c.applied(fmt.Sprintf("respond %d", effect.Respond.Status))
		for k, v := range effect.Respond.Headers {
			w.Header().Set(k, v)
		}
		if text, isText := effect.Respond.Body.(string); isText {
			if w.Header().Get("Content-Type") == "" {
				w.Header().Set("Content-Type", "text/plain")
			}
			w.WriteHeader(effect.Respond.Status)
			_, _ = w.Write([]byte(text))
			return true
		}
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(effect.Respond.Status)
		if effect.Respond.Body != nil {
			_ = json.NewEncoder(w).Encode(effect.Respond.Body)
		}
		return true
	case effect.Close:
		c.applied("close")
		panic(http.ErrAbortHandler)
	default:
		d, _ := parseDuration(effect.Delay)
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
			c.applied("delay " + effect.Delay)
		case <-r.Context().Done():
		case <-c.t.ctx.Done():
		}
		return false
	}
}

func (c *Caught) applied(what string) {
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()
	c.t.Caught[c.idx].Applied = append(c.t.Caught[c.idx].Applied, AppliedStep{To: what, At: s.Wall().UTC()})
	s.log.Record(activity.Event{Env: c.t.Env, Op: "trap.step", Summary: fmt.Sprintf("%s: %s — %s", c.TrapID, c.Subject, what),
		Detail: map[string]string{"trap": c.TrapID, "subject": c.Subject, "to": what}})
	log.Printf("%s: trap %s: %s — %s", s.service, c.TrapID, c.Subject, what)
}

func pathMatches(pattern, path string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == path
	}
	want, got := strings.Split(pattern, "/"), strings.Split(path, "/")
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if want[i] != "*" && want[i] != got[i] {
			return false
		}
	}
	return true
}
