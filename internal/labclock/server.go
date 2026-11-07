package labclock

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
)

// The clock service: the one place the lab's business time is decided.
//
// It used to be the platform's. The runner under test owned the offset and
// every vendor polled it, which put the lab downstream of the thing it
// exists to test: a lab without that particular runner had no clock
// control at all, and a lab in the cloud would have had to poll a laptop.
// Now the lab owns time and a platform that wants to move with it follows
// this service the same way the vendors do.
//
// Three modes, the same three the runner had, so nothing a scenario did
// before means something different now:
//
//	real               offset zero
//	pinned             a fixed offset — set to an instant, or advanced
//	auto-business-day  the most recent instant that is a business day on
//	                   every calendar, recomputed on every read
//
// The last one is recomputed rather than stored because a stored offset
// cannot mean "the most recent business day": the real clock keeps moving,
// and an offset written on Friday evening lands an hour inside Saturday.

// Mode is how the offset is decided.
type Mode string

const (
	ModeReal            Mode = "real"
	ModePinned          Mode = "pinned"
	ModeAutoBusinessDay Mode = "auto-business-day"
)

// Calendar is one settlement calendar the business-day rule checks: a zone
// the date is read in, and the bank holidays on top of weekends.
type Calendar struct {
	Code     string
	Zone     string
	loc      *time.Location
	holidays map[string]bool
}

// NewCalendar loads zone and takes holidays as YYYY-MM-DD strings.
func NewCalendar(code, zone string, holidays []string) (Calendar, error) {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return Calendar{}, fmt.Errorf("calendar %s: %w", code, err)
	}
	c := Calendar{Code: code, Zone: zone, loc: loc, holidays: map[string]bool{}}
	for _, h := range holidays {
		if h = strings.TrimSpace(h); h != "" {
			c.holidays[h] = true
		}
	}
	return c, nil
}

// BusinessDay reports whether at is a business day on this calendar.
func (c Calendar) BusinessDay(at time.Time) bool {
	local := at.In(c.loc)
	if wd := local.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false
	}
	return !c.holidays[local.Format("2006-01-02")]
}

// Hold stops the clock being moved while something depends on it staying
// put — a settlement run in flight, whose stages would otherwise land on
// two different days. It expires on its own, so a holder that crashes
// does not freeze the lab.
type Hold struct {
	Holder string    `json:"holder"`
	Reason string    `json:"reason"`
	Until  time.Time `json:"until"`
}

// Server is the clock service's state. The zero value is not usable; use
// NewServer.
type Server struct {
	wall      func() time.Time
	calendars []Calendar
	log       *activity.Log

	mu        sync.Mutex
	mode      Mode
	offset    time.Duration // meaningful in ModePinned
	reason    string
	changedAt time.Time
	holds     map[string]Hold
}

// NewServer starts on the real clock. wall is the real clock; nil means
// time.Now.
func NewServer(calendars []Calendar, wall func() time.Time) *Server {
	if wall == nil {
		wall = time.Now
	}
	return &Server{
		wall:      wall,
		calendars: calendars,
		log: activity.New("changes", "Clock changes",
			"Every move of the lab clock, and every hold taken on it. Amber is a move refused because something held the clock."),
		mode:      ModeReal,
		reason:    "the real clock",
		changedAt: wall(),
		holds:     map[string]Hold{},
	}
}

// Log is the service's activity log, for GET /sim/activity.
func (s *Server) Log() *activity.Log { return s.log }

// BusinessDay reports whether at is a business day on every calendar.
// Deliberately stricter than any single currency's rule: one clock serves a
// run that may carry GBP and EUR files, and a day that satisfies both is
// always safe.
func (s *Server) BusinessDay(at time.Time) bool {
	for _, c := range s.calendars {
		if !c.BusinessDay(at) {
			return false
		}
	}
	return true
}

// mostRecentBusinessDay is the latest instant at or before from that is a
// business day everywhere, stepping back an hour at a time. Six days of
// hours clears any weekend plus a bank holiday; false if none is found.
func (s *Server) mostRecentBusinessDay(from time.Time) (time.Time, bool) {
	at := from
	for step := 0; step < 24*6; step++ {
		if s.BusinessDay(at) {
			return at, true
		}
		at = at.Add(-time.Hour)
	}
	return time.Time{}, false
}

// offsetLocked is the offset in force now. Caller holds s.mu.
//
// Whole milliseconds, always: a follower in JavaScript adds offset_ms to
// Date.now(), and a pinned clock worked out from time.Now() carries
// nanoseconds. A fractional Date.now() is something no library expects —
// PostHog's uuidv7 throws on it and took buddy's orchestrator down.
func (s *Server) offsetLocked() time.Duration {
	switch s.mode {
	case ModePinned:
		return s.offset.Round(time.Millisecond)
	case ModeAutoBusinessDay:
		real := s.wall()
		if target, ok := s.mostRecentBusinessDay(real); ok {
			return target.Sub(real).Round(time.Millisecond)
		}
	}
	return 0
}

// CalendarView is one calendar as the clock stands.
type CalendarView struct {
	Code        string `json:"code"`
	Zone        string `json:"zone"`
	Date        string `json:"date"`
	Weekday     string `json:"weekday"`
	BusinessDay bool   `json:"business_day"`
}

// State is what GET /clock answers. offset_ms is the only field a follower
// needs; the rest is for the people looking at it.
type State struct {
	OffsetMs    float64        `json:"offset_ms"`
	OffsetHours float64        `json:"offset_hours"`
	Now         time.Time      `json:"now"`
	Mode        Mode           `json:"mode"`
	Pinned      bool           `json:"pinned"`
	Reason      string         `json:"reason"`
	ChangedAt   time.Time      `json:"changed_at"`
	BusinessDay bool           `json:"business_day"`
	Calendars   []CalendarView `json:"calendars"`
	Holds       []Hold         `json:"holds"`
}

// State reads the clock.
func (s *Server) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateLocked()
}

func (s *Server) stateLocked() State {
	off := s.offsetLocked()
	now := s.wall().Add(off).UTC()
	st := State{
		OffsetMs:    float64(off) / float64(time.Millisecond),
		OffsetHours: roundTo(off.Hours(), 2),
		Now:         now,
		Mode:        s.mode,
		Pinned:      s.mode == ModePinned,
		Reason:      s.reason,
		ChangedAt:   s.changedAt.UTC(),
		BusinessDay: s.BusinessDay(now),
		Calendars:   []CalendarView{},
		Holds:       s.liveHoldsLocked(),
	}
	for _, c := range s.calendars {
		local := now.In(c.loc)
		st.Calendars = append(st.Calendars, CalendarView{
			Code: c.Code, Zone: c.Zone,
			Date:        local.Format("2006-01-02"),
			Weekday:     local.Format("Mon"),
			BusinessDay: c.BusinessDay(now),
		})
	}
	return st
}

func (s *Server) liveHoldsLocked() []Hold {
	out := []Hold{}
	now := s.wall()
	for k, h := range s.holds {
		if !now.Before(h.Until) {
			delete(s.holds, k)
			continue
		}
		out = append(out, h)
	}
	return out
}

// Request is one move of the clock: exactly one of At, Advance or Mode.
type Request struct {
	At      string `json:"at,omitempty"`
	Advance string `json:"advance,omitempty"`
	Mode    Mode   `json:"mode,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// ErrHeld is a move refused because something holds the clock.
var ErrHeld = errors.New("clock is held")

// ErrBadRequest is a move this clock cannot make sense of.
var ErrBadRequest = errors.New("bad clock request")

// Move applies r and returns the clock as it now stands.
func (s *Server) Move(r Request) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if holds := s.liveHoldsLocked(); len(holds) > 0 {
		h := holds[0]
		msg := fmt.Sprintf("%s holds the clock: %s (until %s)", h.Holder, h.Reason, h.Until.UTC().Format("15:04:05Z"))
		s.log.Record(activity.Event{Op: "clock.refused", Summary: "move refused — " + msg, Status: activity.StatusWarn})
		return State{}, fmt.Errorf("%w: %s", ErrHeld, msg)
	}

	real := s.wall()
	mode, off, reason, err := s.resolve(r, real)
	if err != nil {
		return State{}, fmt.Errorf("%w: %s", ErrBadRequest, err.Error())
	}
	s.mode, s.offset, s.reason, s.changedAt = mode, off, reason, real
	st := s.stateLocked()
	day := "a business day"
	if !st.BusinessDay {
		day = "not a business day"
	}
	// The day on each calendar, not the UTC one: 23:20 UTC on a Sunday is
	// Monday in Stockholm, and "Sun …, a business day" reads as a bug.
	var local []string
	for _, c := range st.Calendars {
		local = append(local, c.Weekday+" "+c.Date+" on "+c.Code)
	}
	s.log.Record(activity.Event{
		Op:      "clock." + string(mode),
		Summary: fmt.Sprintf("%s → %s UTC (%s), %s — %s", mode, st.Now.Format("2006-01-02 15:04"), strings.Join(local, ", "), day, reason),
		Status:  activity.StatusOK,
		Detail:  map[string]string{"offset_hours": strconv.FormatFloat(st.OffsetHours, 'f', -1, 64)},
	})
	return st, nil
}

func (s *Server) resolve(r Request, real time.Time) (Mode, time.Duration, string, error) {
	set := 0
	for _, f := range []bool{r.At != "", r.Advance != "", r.Mode != ""} {
		if f {
			set++
		}
	}
	if set != 1 {
		return "", 0, "", errors.New(`send one of {"at": "<RFC 3339>"}, {"advance": "1d"}, {"mode": "real"} or {"mode": "auto-business-day"}`)
	}
	switch {
	case r.At != "":
		at, err := time.Parse(time.RFC3339, r.At)
		if err != nil {
			return "", 0, "", fmt.Errorf("at: %s is not an RFC 3339 timestamp", r.At)
		}
		return ModePinned, at.Sub(real), orDefault(r.Reason, "pinned to "+r.At), nil
	case r.Advance != "":
		d, err := ParseAdvance(r.Advance)
		if err != nil {
			return "", 0, "", err
		}
		return ModePinned, s.offsetLocked() + d, orDefault(r.Reason, "advanced "+r.Advance), nil
	}
	switch r.Mode {
	case ModeReal:
		return ModeReal, 0, orDefault(r.Reason, "back on the real clock"), nil
	case ModeAutoBusinessDay:
		return ModeAutoBusinessDay, 0, orDefault(r.Reason, "the most recent business day"), nil
	}
	return "", 0, "", fmt.Errorf("mode: %q is not real or auto-business-day", r.Mode)
}

// ParseAdvance reads "1d", "-2d", "6h", "90m".
func ParseAdvance(spec string) (time.Duration, error) {
	t := strings.TrimSpace(spec)
	units := map[byte]time.Duration{'d': 24 * time.Hour, 'h': time.Hour, 'm': time.Minute}
	if len(t) >= 2 {
		if unit, ok := units[t[len(t)-1]]; ok {
			if n, err := strconv.ParseFloat(strings.TrimSpace(t[:len(t)-1]), 64); err == nil {
				return time.Duration(n * float64(unit)), nil
			}
		}
	}
	return 0, fmt.Errorf(`advance: %q is not like "1d", "6h" or "-90m"`, spec)
}

// MaxHold caps a hold, so a forgotten one clears within the hour.
const MaxHold = time.Hour

// Take places or renews holder's hold for ttl (default ten minutes).
func (s *Server) Take(holder, reason string, ttl time.Duration) (Hold, error) {
	holder = strings.TrimSpace(holder)
	if holder == "" {
		return Hold{}, fmt.Errorf("%w: holder is required", ErrBadRequest)
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	if ttl > MaxHold {
		ttl = MaxHold
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, renewing := s.holds[holder]
	h := Hold{Holder: holder, Reason: orDefault(reason, "held"), Until: s.wall().Add(ttl).UTC()}
	s.holds[holder] = h
	if !renewing {
		s.log.Record(activity.Event{Op: "hold.take", Summary: fmt.Sprintf("%s holds the clock — %s", holder, h.Reason), Status: activity.StatusOK})
	}
	return h, nil
}

// Release drops holder's hold, reporting whether there was one.
// Reset puts the clock back where it started: holds released, the change
// log emptied, and start applied as on first start (CLOCK_START, or the
// real clock). While a hold is live it refuses with ErrHeld unless force:
// a hold means a settlement run is in flight, and resetting the clock under
// it would move the run's business day mid-run.
func (s *Server) Reset(force bool, start Request) (holds int, changes int64, err error) {
	s.mu.Lock()
	if live := s.liveHoldsLocked(); len(live) > 0 && !force {
		h := live[0]
		s.mu.Unlock()
		return 0, 0, fmt.Errorf("%w: %s holds the clock: %s (until %s)", ErrHeld, h.Holder, h.Reason, h.Until.UTC().Format("15:04:05Z"))
	}
	holds = len(s.holds)
	s.holds = map[string]Hold{}
	changes = s.log.Clear()
	s.mu.Unlock()
	if start.At == "" && start.Advance == "" && start.Mode == "" {
		start = Request{Mode: ModeReal, Reason: "reset to the real clock"}
	}
	if _, err := s.Move(start); err != nil {
		return holds, changes, err
	}
	return holds, changes, nil
}

func (s *Server) Release(holder string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.holds[holder]
	if ok {
		delete(s.holds, holder)
		s.log.Record(activity.Event{Op: "hold.release", Summary: fmt.Sprintf("%s released the clock — %s", holder, h.Reason), Status: activity.StatusOK})
	}
	return ok
}

// Routes mounts the clock's HTTP surface on mux.
//
//	GET    /clock                 the clock as it stands
//	POST   /clock                 move it: {"at"}, {"advance"} or {"mode"}
//	POST   /clock/holds           {"holder", "reason", "ttl_seconds"}
//	DELETE /clock/holds/{holder}  release a hold
//	GET    /sim/activity          the changes log
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /clock", func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, s.State())
	})
	mux.HandleFunc("POST /clock", func(w http.ResponseWriter, r *http.Request) {
		var req Request
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
			httputilx.Error(w, 400, "body: "+err.Error())
			return
		}
		st, err := s.Move(req)
		switch {
		case errors.Is(err, ErrHeld):
			httputilx.Error(w, 409, err.Error())
		case err != nil:
			httputilx.Error(w, 400, err.Error())
		default:
			httputilx.WriteJSON(w, 200, st)
		}
	})
	mux.HandleFunc("POST /clock/holds", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Holder     string `json:"holder"`
			Reason     string `json:"reason"`
			TTLSeconds int    `json:"ttl_seconds"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
			httputilx.Error(w, 400, "body: "+err.Error())
			return
		}
		h, err := s.Take(req.Holder, req.Reason, time.Duration(req.TTLSeconds)*time.Second)
		if err != nil {
			httputilx.Error(w, 400, err.Error())
			return
		}
		httputilx.WriteJSON(w, 200, h)
	})
	mux.HandleFunc("DELETE /clock/holds/{holder}", func(w http.ResponseWriter, r *http.Request) {
		if !s.Release(r.PathValue("holder")) {
			httputilx.Error(w, 404, "no hold by "+r.PathValue("holder"))
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /sim/activity", activity.Handler(s.log))
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func roundTo(f float64, places int) float64 {
	p, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'f', places, 64), 64)
	return p
}
