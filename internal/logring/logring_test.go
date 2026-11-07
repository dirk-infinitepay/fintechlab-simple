package logring

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http/httptest"
	"testing"
)

func write(r *Ring, lines ...string) {
	for _, l := range lines {
		fmt.Fprintln(r, l)
	}
}

func texts(p Page) []string {
	var out []string
	for _, l := range p.Lines {
		out = append(out, l.Text)
	}
	return out
}

func TestTheLogPackagesPrefixIsDroppedAndTheWordsKept(t *testing.T) {
	r := New(10)
	l := log.New(r, "", log.LstdFlags|log.Lmicroseconds)
	l.Printf("banking-circle: next 1 outgoing payment(s) will end [Rejected]")
	p := r.Read(0, false, 10, "")
	if got := texts(p); len(got) != 1 || got[0] != "banking-circle: next 1 outgoing payment(s) will end [Rejected]" {
		t.Fatalf("lines = %q", got)
	}
	if p.Lines[0].At.IsZero() || p.Lines[0].Seq != 1 {
		t.Errorf("line = %+v: the ring stamps its own time and seq", p.Lines[0])
	}
}

func TestALineSplitAcrossWritesIsOneLine(t *testing.T) {
	r := New(10)
	fmt.Fprint(r, "half a ")
	fmt.Fprint(r, "line\nand another\n")
	if got := texts(r.Read(0, false, 10, "")); len(got) != 2 || got[0] != "half a line" || got[1] != "and another" {
		t.Fatalf("lines = %q", got)
	}
}

func TestWithoutSinceItIsTheNewestLinesOldestFirst(t *testing.T) {
	r := New(10)
	write(r, "a", "b", "c", "d")
	p := r.Read(0, false, 2, "")
	if got := texts(p); len(got) != 2 || got[0] != "c" || got[1] != "d" {
		t.Fatalf("lines = %q", got)
	}
	if p.Next != 4 {
		t.Errorf("next = %d, want the head, 4", p.Next)
	}
}

func TestSinceReadsOnAndTheCursorNeverSkipsALine(t *testing.T) {
	r := New(10)
	write(r, "a", "b", "c", "d", "e")
	var seen []string
	since := int64(0)
	for i := 0; i < 5; i++ {
		p := r.Read(since, true, 2, "")
		seen = append(seen, texts(p)...)
		if p.Next == since {
			break
		}
		since = p.Next
	}
	if fmt.Sprint(seen) != "[a b c d e]" {
		t.Fatalf("paged through %v", seen)
	}
}

func TestAFilteredReaderStillMovesForward(t *testing.T) {
	r := New(10)
	write(r, "bcp_1 booked", "noise", "noise", "bcp_1 processed")
	p := r.Read(0, true, 10, "BCP_1")
	if got := texts(p); len(got) != 2 {
		t.Fatalf("lines = %q", got)
	}
	if p.Next != 4 {
		t.Errorf("next = %d: a reader past every line it did not match should not read them again", p.Next)
	}
}

func TestAReaderThatFellBehindIsToldHowFar(t *testing.T) {
	r := New(3)
	write(r, "1", "2", "3", "4", "5", "6")
	p := r.Read(1, true, 10, "")
	if p.Dropped != 2 {
		t.Errorf("dropped = %d, want 2 (lines 2 and 3 are gone)", p.Dropped)
	}
	if got := texts(p); fmt.Sprint(got) != "[4 5 6]" {
		t.Errorf("lines = %q", got)
	}
}

func TestTheHandlerAnswersInTheContractsShape(t *testing.T) {
	r := New(10)
	write(r, "one", "two")
	w := httptest.NewRecorder()
	r.Handler("clock").ServeHTTP(w, httptest.NewRequest("GET", "/sim/logs?since=1&limit=5", nil))
	var got struct {
		Service string `json:"service"`
		Lines   []Line `json:"lines"`
		Next    int64  `json:"next"`
		Dropped int64  `json:"dropped"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got.Service != "clock" || len(got.Lines) != 1 || got.Lines[0].Text != "two" || got.Next != 2 {
		t.Errorf("answer = %+v", got)
	}
	for _, bad := range []string{"limit=0", "limit=1001", "since=x", "since=-1"} {
		w := httptest.NewRecorder()
		r.Handler("clock").ServeHTTP(w, httptest.NewRequest("GET", "/sim/logs?"+bad, nil))
		if w.Code != 400 {
			t.Errorf("%s = %d, want 400", bad, w.Code)
		}
	}
}

func TestWatchingTheLabDoesNotFillTheRing(t *testing.T) {
	r := New(10)
	write(r, "GET /health", "GET /sim/logs", "GET /sim/logs?since=4", "POST /payments", "GET /healthz-not-a-probe", "bank: GET /health came from a peer")
	if got := texts(r.Read(0, false, 10, "")); fmt.Sprint(got) != "[POST /payments GET /healthz-not-a-probe bank: GET /health came from a peer]" {
		t.Errorf("kept %q: only the probe and the log reads themselves are dropped", got)
	}
}
