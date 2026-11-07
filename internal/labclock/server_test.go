package labclock

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testServer(t *testing.T, now *time.Time, holidaysGB ...string) *Server {
	t.Helper()
	gb, err := NewCalendar("GB", "Europe/London", holidaysGB)
	if err != nil {
		t.Fatal(err)
	}
	se, err := NewCalendar("SE", "Europe/Stockholm", nil)
	if err != nil {
		t.Fatal(err)
	}
	return NewServer([]Calendar{gb, se}, func() time.Time { return *now })
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// TestPinAndAdvanceAreOffsetsFromTheRealClock: pinning to an instant reads
// that instant back, advancing adds to whatever is in force, and real puts
// the offset back to zero.
func TestPinAndAdvanceAreOffsetsFromTheRealClock(t *testing.T) {
	now := mustTime(t, "2026-09-27T12:00:00Z") // a Sunday
	s := testServer(t, &now)

	st, err := s.Move(Request{At: "2026-09-25T09:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Now.Equal(mustTime(t, "2026-09-25T09:00:00Z")) || st.Mode != ModePinned || !st.BusinessDay {
		t.Fatalf("pinned: %+v", st)
	}
	// The real clock moves on; a pinned offset moves with it.
	now = now.Add(10 * time.Minute)
	if got := s.State().Now; !got.Equal(mustTime(t, "2026-09-25T09:10:00Z")) {
		t.Fatalf("ten real minutes later the clock reads %s", got)
	}

	st, err = s.Move(Request{Advance: "1h"})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Now.Equal(mustTime(t, "2026-09-25T10:10:00Z")) {
		t.Fatalf("advanced: %s", st.Now)
	}

	st, err = s.Move(Request{Mode: ModeReal})
	if err != nil {
		t.Fatal(err)
	}
	if st.OffsetMs != 0 || !st.Now.Equal(now.UTC()) || st.BusinessDay {
		t.Fatalf("real on a Sunday: %+v", st)
	}
}

// TestOffsetIsWholeMilliseconds: the real clock carries nanoseconds, so an
// offset worked out from it does too — and a JavaScript follower adds
// offset_ms to Date.now(). A fractional Date.now() crashed buddy's
// orchestrator (PostHog's uuidv7 refuses it), so the offset is whole
// milliseconds in every mode.
func TestOffsetIsWholeMilliseconds(t *testing.T) {
	now := mustTime(t, "2026-09-27T12:00:00Z").Add(766812 * time.Nanosecond)
	s := testServer(t, &now)
	for _, req := range []Request{{At: "2026-09-25T09:00:00Z"}, {Advance: "1h"}, {Mode: ModeAutoBusinessDay}} {
		st, err := s.Move(req)
		if err != nil {
			t.Fatal(err)
		}
		if st.OffsetMs != float64(int64(st.OffsetMs)) {
			t.Errorf("%+v: offset_ms %v is not whole milliseconds", req, st.OffsetMs)
		}
	}
}

// TestAutoBusinessDayIsRecomputedNotStored: on a weekend the auto mode lands
// on Friday evening, and once the real clock reaches Monday the offset is
// back to zero without anyone moving it.
func TestAutoBusinessDayIsRecomputedNotStored(t *testing.T) {
	now := mustTime(t, "2026-09-26T12:00:00Z") // Saturday
	s := testServer(t, &now)
	st, err := s.Move(Request{Mode: ModeAutoBusinessDay})
	if err != nil {
		t.Fatal(err)
	}
	if !st.BusinessDay || st.Now.Weekday() != time.Friday {
		t.Fatalf("auto on a Saturday: %s (%s)", st.Now, st.Now.Weekday())
	}
	now = mustTime(t, "2026-09-28T10:00:00Z") // Monday
	if st := s.State(); st.OffsetMs != 0 || st.Mode != ModeAutoBusinessDay {
		t.Fatalf("auto on a Monday: %+v", st)
	}
}

// TestHolidaysAreNotBusinessDays: a listed holiday on one calendar makes the
// day a non-business day for the whole clock.
func TestHolidaysAreNotBusinessDays(t *testing.T) {
	now := mustTime(t, "2026-12-25T11:00:00Z") // a Friday
	s := testServer(t, &now, "2026-12-25")
	st := s.State()
	if st.BusinessDay {
		t.Fatal("Christmas on the GB calendar counted as a business day")
	}
	if st.Calendars[0].BusinessDay || !st.Calendars[1].BusinessDay {
		t.Fatalf("calendars: %+v", st.Calendars)
	}
}

// TestAHoldRefusesMovesUntilReleasedOrExpired: a run in flight holds the
// clock; a move is refused with the holder's reason, and allowed again once
// released — or once the hold lapses, so a crashed holder cannot freeze it.
func TestAHoldRefusesMovesUntilReleasedOrExpired(t *testing.T) {
	now := mustTime(t, "2026-09-25T09:00:00Z")
	s := testServer(t, &now)
	if _, err := s.Take("infinite-local-runner", "settlement run r-1 in flight", time.Minute); err != nil {
		t.Fatal(err)
	}
	_, err := s.Move(Request{Advance: "1h"})
	if !errors.Is(err, ErrHeld) || !strings.Contains(err.Error(), "settlement run r-1") {
		t.Fatalf("move under a hold: %v", err)
	}
	if !s.Release("infinite-local-runner") {
		t.Fatal("release found no hold")
	}
	if _, err := s.Move(Request{Advance: "1h"}); err != nil {
		t.Fatalf("move after release: %v", err)
	}

	if _, err := s.Take("crashed", "never released", time.Minute); err != nil {
		t.Fatal(err)
	}
	now = now.Add(61 * time.Second)
	if _, err := s.Move(Request{Advance: "1h"}); err != nil {
		t.Fatalf("move after the hold lapsed: %v", err)
	}
	if n := len(s.State().Holds); n != 0 {
		t.Fatalf("%d holds listed after expiry", n)
	}
}

// TestHTTPSurface: the routes answer in the shapes the followers and the
// console read, and refusals carry the right status.
func TestHTTPSurface(t *testing.T) {
	now := mustTime(t, "2026-09-25T09:00:00Z")
	s := testServer(t, &now)
	mux := http.NewServeMux()
	s.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	post := func(path, body string) *http.Response {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := post("/clock", `{"advance":"2h"}`)
	var st State
	_ = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if resp.StatusCode != 200 || st.OffsetHours != 2 {
		t.Fatalf("advance: %d %+v", resp.StatusCode, st)
	}

	// What a follower reads.
	d, err := Fetch(context.Background(), http.DefaultClient, srv.URL+"/clock")
	if err != nil || d != 2*time.Hour {
		t.Fatalf("Fetch = %s, %v", d, err)
	}

	if resp := post("/clock", `{"advance":"soon"}`); resp.StatusCode != 400 {
		t.Fatalf("nonsense advance: %d", resp.StatusCode)
	}
	if resp := post("/clock", `{"advance":"1h","mode":"real"}`); resp.StatusCode != 400 {
		t.Fatalf("two moves at once: %d", resp.StatusCode)
	}
	if resp := post("/clock/holds", `{"holder":"runner","reason":"run","ttl_seconds":60}`); resp.StatusCode != 200 {
		t.Fatalf("hold: %d", resp.StatusCode)
	}
	if resp := post("/clock", `{"mode":"real"}`); resp.StatusCode != 409 {
		t.Fatalf("move under a hold: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/clock/holds/runner", nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 204 {
		t.Fatalf("release: %v %v", resp, err)
	}
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 404 {
		t.Fatalf("second release: %v %v", resp, err)
	}
}

func TestResetRefusesWhileHeldAndOtherwiseGoesBackToTheStart(t *testing.T) {
	s := NewServer(nil, nil)
	if _, err := s.Move(Request{Advance: "48h"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Take("runner", "a settlement run", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Reset(false, Request{}); !errors.Is(err, ErrHeld) {
		t.Fatalf("reset under a hold = %v, want ErrHeld", err)
	}
	holds, changes, err := s.Reset(true, Request{})
	if err != nil || holds != 1 || changes == 0 {
		t.Fatalf("forced reset = %d holds, %d changes, %v", holds, changes, err)
	}
	st := s.State()
	if st.Mode != ModeReal || len(st.Holds) != 0 {
		t.Errorf("after reset: mode %s, holds %v; want the real clock and no holds", st.Mode, st.Holds)
	}
}
