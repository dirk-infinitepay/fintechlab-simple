package activity

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

func TestEachEnvironmentReadsItsOwnEvents(t *testing.T) {
	l := New("calls", "Calls", "")
	l.Record(Event{Op: "a"})
	l.Record(Event{Op: "b", Env: "sbx"})
	l.Record(Event{Op: "c", Env: tenant.Default})

	if got := l.SnapshotIn(tenant.Default, 0); got.Total != 2 || got.Events[0].Op != "c" || got.Events[0].Env != "" {
		t.Fatalf("default: %+v", got)
	}
	if got := l.SnapshotIn("sbx", 0); got.Total != 1 || got.Events[0].Env != "sbx" {
		t.Fatalf("sbx: %+v", got)
	}
	all := l.SnapshotIn(tenant.All, 0)
	if all.Total != 3 || all.Events[0].Op != "c" || all.Events[2].Op != "a" {
		t.Fatalf("all, newest first across environments: %+v", all)
	}

	if n := l.ClearIn("sbx"); n != 1 {
		t.Fatalf("ClearIn(sbx) = %d", n)
	}
	if got := l.Recent(0); len(got) != 2 {
		t.Fatalf("after clearing sbx, the default keeps its own: %d", len(got))
	}
}

func TestOneBusyEnvironmentDoesNotPushAnothersOut(t *testing.T) {
	l := New("calls", "Calls", "")
	l.Record(Event{Op: "quiet", Env: "sbx"})
	for i := 0; i < DefaultCapacity*2; i++ {
		l.Record(Event{Op: "busy"})
	}
	if got := l.RecentIn("sbx", 0); len(got) != 1 || got[0].Op != "quiet" {
		t.Fatalf("sbx lost its event to the default's burst: %+v", got)
	}
}

func TestTheHandlerServesTheEnvironmentTheCallNames(t *testing.T) {
	l := New("calls", "Calls", "")
	l.Record(Event{Op: "mine", Env: "sbx"})
	l.Record(Event{Op: "default's"})
	h := Handler(l)

	read := func(url string) []Event {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", url, nil))
		var body struct{ Logs []Snapshot }
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Logs[0].Events
	}
	if got := read("/sim/activity"); len(got) != 1 || got[0].Op != "default's" {
		t.Fatalf("no env: %+v", got)
	}
	if got := read("/sim/activity?env=sbx"); len(got) != 1 || got[0].Op != "mine" {
		t.Fatalf("env=sbx: %+v", got)
	}
	if got := read("/sim/activity?env=*"); len(got) != 2 {
		t.Fatalf("env=*: %+v", got)
	}
}

func TestWatchRecordsWhoTheVendorsAuthFoundInside(t *testing.T) {
	l := New("calls", "Calls", "")
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			ctx := tenant.With(r.Context(), "sbx")
			next(w, r.WithContext(ctx))
		}
	}
	h := l.Watch("call", nil, auth(func(w http.ResponseWriter, r *http.Request) {
		if got := tenant.Of(r.Context()); got != "sbx" {
			t.Errorf("inside: %q", got)
		}
		w.WriteHeader(204)
	}))
	h(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/v1/payments", nil))
	if got := l.RecentIn("sbx", 0); len(got) != 1 {
		t.Fatalf("the call was not recorded as sbx's: %+v", l.Recent(0))
	}
}
