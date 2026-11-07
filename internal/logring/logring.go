// Package logring keeps a process's own log lines in a fixed-size ring and
// serves them at GET /sim/logs (docs/lab-api/sim-control.md §5).
//
// Every service logs through the standard log package, so one tee on
// log.SetOutput is all it takes: stdout carries exactly what it did before,
// and the ring holds the last lines for anyone who asks over HTTP instead of
// reaching for `podman logs`. Like an activity log, it is a window onto a
// run, not a record of one: a reader that falls behind is told how many
// lines it missed.
package logring

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
)

// Line is one log line.
type Line struct {
	Seq  int64     `json:"seq"`
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// Ring is a fixed-size ring of log lines, safe for concurrent use. It is an
// io.Writer, so it can sit behind log.SetOutput.
type Ring struct {
	mu    sync.Mutex
	lines []Line
	next  int // where the next line is written
	n     int // how many slots are in use
	seq   int64
	part  []byte // a line written without its newline yet
	now   func() time.Time
}

// DefaultCapacity is how many lines a ring keeps unless SIM_LOG_RING says
// otherwise.
const DefaultCapacity = 2000

// New creates a ring that holds capacity lines.
func New(capacity int) *Ring {
	if capacity < 1 {
		capacity = DefaultCapacity
	}
	return &Ring{lines: make([]Line, capacity), now: time.Now}
}

// Default is the ring Install put behind the log package, for the
// /sim/logs handler of a service that does not keep its own.
var Default *Ring

// Install puts a ring of SIM_LOG_RING lines (default 2000) behind the log
// package, alongside its existing output, and returns it. Call it first in
// main, so the ring holds the start-up lines too.
func Install() *Ring {
	capacity := DefaultCapacity
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("SIM_LOG_RING"))); err == nil && v > 0 {
		capacity = v
	}
	r := New(capacity)
	log.SetOutput(io.MultiWriter(log.Writer(), r))
	Default = r
	return r
}

// stdPrefix is what log.LstdFlags (and Lmicroseconds) put in front of a
// line. The ring stamps its own time, so the line keeps only its words.
var stdPrefix = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(\.\d+)? `)

// Write adds every complete line in p. A line split across two writes is
// joined; the log package never does that, but a writer that is not the
// log package might.
func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	buf := append(r.part, p...)
	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		r.add(string(buf[:i]))
		buf = buf[i+1:]
	}
	r.part = append(r.part[:0], buf...)
	return len(p), nil
}

// noise is the request lines that come from watching the lab rather than
// from anything happening in it: the lab API's health probe every few
// seconds, and reading these very logs. Kept, they would fill the ring with
// the reader's own polling in an hour. They still go to stdout.
var noise = regexp.MustCompile(`^(GET|HEAD) /(health|sim/logs)(\?.*)?$`)

func (r *Ring) add(text string) {
	text = strings.TrimRight(stdPrefix.ReplaceAllString(text, ""), "\r")
	if noise.MatchString(text) {
		return
	}
	r.seq++
	r.lines[r.next] = Line{Seq: r.seq, At: r.now().UTC(), Text: text}
	r.next = (r.next + 1) % len(r.lines)
	if r.n < len(r.lines) {
		r.n++
	}
}

// Held is how many lines the ring holds, and how many it can.
func (r *Ring) Held() (held, capacity int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n, len(r.lines)
}

// Page is one answer of GET /sim/logs.
type Page struct {
	Lines []Line `json:"lines"`
	// Next is the cursor to pass as since: the newest line this read
	// looked at, matched or not, so a filtered reader still moves forward.
	Next int64 `json:"next"`
	// Dropped counts lines after since that the ring no longer holds: the
	// reader fell behind, and the gap is said rather than silent.
	Dropped int64 `json:"dropped"`
}

// Read returns up to limit lines matching q (a case-insensitive substring;
// empty matches every line), oldest first. Without since it is the newest
// limit lines; with since, the lines after that seq.
func (r *Ring) Read(since int64, hasSince bool, limit int, q string) Page {
	r.mu.Lock()
	defer r.mu.Unlock()
	q = strings.ToLower(q)
	match := func(l Line) bool { return q == "" || strings.Contains(strings.ToLower(l.Text), q) }
	at := func(i int) Line { // i-th held line, oldest first
		start := (r.next - r.n + len(r.lines)) % len(r.lines)
		return r.lines[(start+i)%len(r.lines)]
	}
	page := Page{Lines: []Line{}, Next: r.seq}

	if !hasSince {
		for i := r.n - 1; i >= 0 && len(page.Lines) < limit; i-- {
			if l := at(i); match(l) {
				page.Lines = append(page.Lines, l)
			}
		}
		for i, j := 0, len(page.Lines)-1; i < j; i, j = i+1, j-1 {
			page.Lines[i], page.Lines[j] = page.Lines[j], page.Lines[i]
		}
		return page
	}

	oldest := r.seq - int64(r.n) + 1
	if r.n > 0 && since < oldest-1 {
		page.Dropped = oldest - 1 - since
	}
	for i := 0; i < r.n; i++ {
		l := at(i)
		if l.Seq <= since {
			continue
		}
		if len(page.Lines) == limit {
			page.Next = page.Lines[len(page.Lines)-1].Seq
			return page
		}
		if match(l) {
			page.Lines = append(page.Lines, l)
		}
	}
	// A cursor from before this process restarted is past the head:
	// nothing is newer, and next says where to read from now.
	return page
}

// Handler serves GET /sim/logs?since=&limit=&q= for service.
func (r *Ring) Handler(service string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		qs := req.URL.Query()
		limit := 200
		if v := qs.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 1000 {
				httputilx.Error(w, 400, "limit is 1 to 1000")
				return
			}
			limit = n
		}
		var since int64
		hasSince := false
		if v := qs.Get("since"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				httputilx.Error(w, 400, "since is a line's seq, as a previous answer's next gave it")
				return
			}
			since, hasSince = n, true
		}
		page := r.Read(since, hasSince, limit, qs.Get("q"))
		httputilx.WriteJSON(w, 200, map[string]any{
			"service": service,
			"lines":   page.Lines,
			"next":    page.Next,
			"dropped": page.Dropped,
		})
	}
}
