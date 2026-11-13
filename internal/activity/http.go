package activity

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// Watching an inbound call is done as middleware rather than at each
// `return` inside a handler, because the interesting calls are the ones
// that did *not* take the happy path. A handler that refuses a payout has
// a dozen early returns, and instrumenting them one by one guarantees the
// refusal an operator is hunting for is the one nobody instrumented.

// bodyLimit caps what is buffered from a request or response. Generous
// enough for any JSON these vendors exchange, small enough that a
// mistakenly uploaded file does not end up in memory twice.
const bodyLimit = 64 << 10

// Call is what a summariser is given: the request as it arrived, the body
// it carried, and what the handler answered.
type Call struct {
	Request  *http.Request
	ReqBody  []byte
	Status   int
	RespBody []byte
	// Refused is set by a summariser when the vendor refused the call in a
	// 2xx answer of its own shape: the event is a warning and carries it.
	Refused string
}

// JSONField reads a top-level string field out of the request body, or ""
// if the body is not an object or the field is not a string. Summarisers
// are written against bodies a peer controls, so this never panics and
// never reports a guess as a value.
func (c *Call) JSONField(path ...string) string {
	return jsonString(c.ReqBody, path...)
}

// RespField is JSONField over the answer.
func (c *Call) RespField(path ...string) string {
	return jsonString(c.RespBody, path...)
}

func jsonString(body []byte, path ...string) string {
	var cur any
	if err := json.Unmarshal(body, &cur); err != nil {
		return ""
	}
	for _, key := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur, ok = obj[key]
		if !ok {
			return ""
		}
	}
	switch v := cur.(type) {
	case string:
		return v
	case float64:
		return trimFloat(v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

func trimFloat(f float64) string {
	b, err := json.Marshal(f)
	if err != nil {
		return ""
	}
	return string(b)
}

// Summarizer turns one call into the line an operator reads and the
// identifiers they might copy out of it.
type Summarizer func(c *Call) (summary string, detail map[string]string)

// Watch wraps next so every call through it is recorded.
func (l *Log) Watch(op string, sum Summarizer, next http.HandlerFunc) http.HandlerFunc {
	if l == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var reqBody []byte
		if r.Body != nil {
			full, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			// Hand the handler the whole body: it is the one doing the real
			// work and must not be starved by the observer. Only the
			// journal's copy is capped.
			r.Body = io.NopCloser(bytes.NewReader(full))
			reqBody = full[:min(len(full), bodyLimit)]
		}
		// The environment is learnt inside: the vendor's auth sets it once
		// it knows who called (internal/tenant), into a slot bound here.
		ctx := tenant.Bind(r.Context())
		r = r.WithContext(ctx)
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next(rec, r)

		call := &Call{Request: r, ReqBody: reqBody, Status: rec.status, RespBody: rec.body.Bytes()}
		summary, detail := "", map[string]string{}
		if sum != nil {
			summary, detail = sum(call)
		}
		if summary == "" {
			summary = r.Method + " " + r.URL.Path
		}
		status := StatusFor(rec.status)
		if call.Refused != "" && status == StatusOK {
			status = StatusWarn
			summary += " — " + call.Refused
		}
		if rec.status >= 400 {
			// The vendor's own refusal, verbatim. Anything else here would
			// be the lab paraphrasing an error the platform has to handle.
			if msg := jsonString(call.RespBody, "error"); msg != "" {
				summary += " — " + msg
			}
		}
		if detail == nil {
			detail = map[string]string{}
		}
		detail["status"] = http.StatusText(rec.status)
		env, _ := tenant.Named(ctx)
		l.Record(Event{
			Env:     env,
			Op:      op,
			Peer:    peerOf(r),
			Summary: summary,
			Status:  status,
			Detail:  detail,
		})
	}
}

// peerOf names the caller. The forwarded headers are honoured because
// these services are reached through a container bridge as often as not,
// and "10.89.0.1" for every call tells an operator nothing.
func peerOf(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		return strings.TrimSpace(strings.Split(v, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// recorder captures the status and a bounded copy of the response.
type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
	wrote  bool
}

func (w *recorder) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *recorder) Write(b []byte) (int, error) {
	w.wrote = true
	if w.body.Len() < bodyLimit {
		w.body.Write(b[:min(len(b), bodyLimit-w.body.Len())])
	}
	return w.ResponseWriter.Write(b)
}
