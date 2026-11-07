package activity

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestRecentIsNewestFirst: the console renders this list top-down and an
// operator reads the top line. Oldest-first would put the event they came
// for at the bottom of a hundred rows.
func TestRecentIsNewestFirst(t *testing.T) {
	l := New("calls", "Calls", "")
	for i := 1; i <= 3; i++ {
		l.Record(Event{Op: "call", Summary: fmt.Sprintf("call %d", i)})
	}
	got := l.Recent(0)
	if len(got) != 3 {
		t.Fatalf("Recent returned %d events, want 3", len(got))
	}
	if got[0].Summary != "call 3" || got[2].Summary != "call 1" {
		t.Errorf("order = %q…%q, want newest first", got[0].Summary, got[2].Summary)
	}
	if got[0].Seq != 3 {
		t.Errorf("seq = %d, want 3 — a sequence the caller cannot set wrong", got[0].Seq)
	}
	if got[0].At.IsZero() {
		t.Error("At was not filled in")
	}
}

// TestRingDropsOldest: the buffer is fixed so a lab left running overnight
// cannot grow a heap. What it must never do is drop the newest.
func TestRingDropsOldest(t *testing.T) {
	l := New("calls", "Calls", "")
	for i := 0; i < DefaultCapacity+10; i++ {
		l.Record(Event{Op: "call", Summary: fmt.Sprintf("call %d", i)})
	}
	got := l.Recent(0)
	if len(got) != DefaultCapacity {
		t.Fatalf("kept %d events, want the capacity %d", len(got), DefaultCapacity)
	}
	newest := fmt.Sprintf("call %d", DefaultCapacity+9)
	if got[0].Summary != newest {
		t.Errorf("newest = %q, want %q", got[0].Summary, newest)
	}
	oldestKept := fmt.Sprintf("call %d", 10)
	if got[len(got)-1].Summary != oldestKept {
		t.Errorf("oldest kept = %q, want %q", got[len(got)-1].Summary, oldestKept)
	}
}

// TestSnapshotDistinguishesTotalFromKept: "4000 calls, showing the last
// 256" and "256 calls" are different facts, and an operator debugging a
// run needs to know which one they are looking at.
func TestSnapshotDistinguishesTotalFromKept(t *testing.T) {
	l := New("calls", "Calls", "note")
	for i := 0; i < DefaultCapacity+5; i++ {
		l.Record(Event{Op: "call"})
	}
	snap := l.Snapshot(10)
	if snap.Total != int64(DefaultCapacity+5) {
		t.Errorf("Total = %d, want %d", snap.Total, DefaultCapacity+5)
	}
	if snap.Kept != DefaultCapacity {
		t.Errorf("Kept = %d, want %d", snap.Kept, DefaultCapacity)
	}
	if len(snap.Events) != 10 {
		t.Errorf("returned %d events for a limit of 10", len(snap.Events))
	}
	if snap.Last == nil || snap.Last.Seq != int64(DefaultCapacity+5) {
		t.Error("Last should be the most recent event, for the collapsed header")
	}
}

// TestEmptySnapshotIsAnEmptyList, not null: the console counts the list to
// decide between an empty state and rows, and null would leave a service
// nobody has touched showing "loading" forever.
func TestEmptySnapshotIsAnEmptyList(t *testing.T) {
	body, _ := json.Marshal(New("calls", "Calls", "").Snapshot(0))
	if !strings.Contains(string(body), `"events":[]`) {
		t.Errorf("empty snapshot marshalled as %s", body)
	}
}

// TestHandlerServesEveryLog: a service with two conversations worth
// watching passes two logs and the console draws two panels, in order.
func TestHandlerServesEveryLog(t *testing.T) {
	in := New("in", "Payments received", "")
	out := New("out", "Webhooks sent", "")
	in.Record(Event{Op: "payment.create", Summary: "one"})
	out.Record(Event{Op: "webhook", Summary: "two", Status: StatusBad})

	w := httptest.NewRecorder()
	Handler(in, nil, out)(w, httptest.NewRequest(http.MethodGet, "/sim/activity?limit=5", nil))

	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var got struct {
		Logs []Snapshot `json:"logs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Logs) != 2 {
		t.Fatalf("got %d logs, want 2 (a nil log is skipped, not rendered)", len(got.Logs))
	}
	if got.Logs[0].Name != "in" || got.Logs[1].Name != "out" {
		t.Errorf("order = %q,%q — the service decides the order", got.Logs[0].Name, got.Logs[1].Name)
	}
	if got.Logs[1].Events[0].Status != StatusBad {
		t.Errorf("status = %q, want it carried through", got.Logs[1].Events[0].Status)
	}
}

// TestStatusForTreatsRefusalAsAmber. A vendor refusing a malformed payout
// is the simulation working; painting it red teaches an operator to
// ignore red.
func TestStatusForTreatsRefusalAsAmber(t *testing.T) {
	for code, want := range map[int]string{200: StatusOK, 201: StatusOK, 400: StatusWarn, 422: StatusWarn, 500: StatusBad, 502: StatusBad} {
		if got := StatusFor(code); got != want {
			t.Errorf("StatusFor(%d) = %q, want %q", code, got, want)
		}
	}
}

// TestRecordOnNilLogIsANoop: instrumentation gets added to a code path
// before every caller has been given a log, and a nil dereference there
// would take the vendor down over an observability feature.
func TestRecordOnNilLogIsANoop(t *testing.T) {
	var l *Log
	l.Record(Event{Op: "call"})
	if got := l.Recent(5); got != nil {
		t.Errorf("Recent on a nil log = %v, want nil", got)
	}
}

// TestConcurrentRecordAndRead is what `go test -race` is for: the HTTP
// handler reads this ring while request goroutines write it.
func TestConcurrentRecordAndRead(t *testing.T) {
	l := New("calls", "Calls", "")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				l.Record(Event{Op: "call"})
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = l.Snapshot(10)
			}
		}()
	}
	wg.Wait()
	if total := l.Snapshot(1).Total; total != 800 {
		t.Errorf("Total = %d, want 800 — every record must be counted once", total)
	}
}

func TestClearEmptiesTheLogAndSeqKeepsCounting(t *testing.T) {
	l := New("x", "X", "")
	l.Record(Event{Op: "a"})
	l.Record(Event{Op: "b"})
	if n := l.Clear(); n != 2 {
		t.Errorf("cleared %d, want 2", n)
	}
	if s := l.Snapshot(10); s.Total != 0 || len(s.Events) != 0 {
		t.Errorf("after clear: %+v", s)
	}
	l.Record(Event{Op: "c"})
	if e := l.Recent(1); len(e) != 1 || e[0].Seq != 3 {
		t.Errorf("after clear the next seq is %v, want 3", e)
	}
}
