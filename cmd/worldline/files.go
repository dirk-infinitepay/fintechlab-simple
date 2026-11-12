package main

// Settlement files by hand (docs/lab-api/plan.md, phase 6): two lab hooks
// on the control port, in the environment the call names, that split the
// cycle in two so the lab can keep files and choose when one is delivered.
//
// render is the cycle's cut without the rest of the cycle: the file the
// environment's transactions settle in, by the cycle's own code and named
// as the cycle would name it now. Nothing is published, wired or recorded.
//
// publish is the rest of the cycle without the cut: a settlement file from
// anywhere, read with Worldline's own parser, then encrypted and signed and
// put in the environment's folders exactly as the cycle puts a file it cut,
// and with wire=true the morning lump sum wired with it over the same path.
// A platform polling its folder cannot tell the two apart, which is the
// point. What it published is the reset's to take back, as a run's files
// are (reset.go).

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
	"github.com/webduvet/fintechlab-simple/internal/wlsftp"
	"github.com/webduvet/fintechlab-simple/internal/worldline"
)

// maxSettlementFileBytes bounds a file handed to publish. A real day's
// file is well under a megabyte; this is room, not a target.
const maxSettlementFileBytes = 64 << 20

// maxPublishName is the 255 bytes a filename may have.
const maxPublishName = 255

// keptByHand is how many published names an environment remembers for its
// reset, as the run history keeps its last 100 runs.
const keptByHand = 100

// opPublished is the activity op a file published by hand is recorded
// under, in the sftp log beside the platform's own traffic.
const opPublished = "settlement-file.published"

var currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)

func (s *sim) fileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /sim/settlement-files/render", s.renderSettlementFile)
	mux.HandleFunc("POST /sim/settlement-files/publish", s.publishSettlementFile)
	mux.HandleFunc("GET /sim/sftp", s.sftpFolders)
}

// renderedFile is render's answer: the file, as plain text, and what it
// settles.
type renderedFile struct {
	Name     string   `json:"name"`
	Content  string   `json:"content"`
	Currency string   `json:"currency"`
	MIDs     []string `json:"mids"`
	Total    string   `json:"total"`
	Items    int      `json:"items"`
	Slot     string   `json:"slot"`
	FromDate string   `json:"from_date"`
	ToDate   string   `json:"to_date"`
}

// renderSettlementFile answers the file of one currency the cycle would
// cut now from the named environment's transactions: slot ER and the
// cycle's coverage day by the lab clock unless the body names others. It
// refuses "*": a file is one environment's.
func (s *sim) renderSettlementFile(w http.ResponseWriter, r *http.Request) {
	e, ok := s.one(w, r)
	if !ok {
		return
	}
	var req struct {
		Currency string `json:"currency"`
		Date     string `json:"date"`
		Slot     string `json:"slot"`
	}
	if err := httputilx.ReadJSON(r, &req); err != nil {
		httputilx.Error(w, 400, `want {"currency": "EUR", "date": "YYYY-MM-DD", "slot": "ER"}: `+err.Error())
		return
	}
	ccy := strings.ToUpper(strings.TrimSpace(req.Currency))
	if ccy == "" {
		httputilx.Error(w, 400, "currency is required: a settlement file is one currency's")
		return
	}
	if !currencyCode.MatchString(ccy) {
		httputilx.Error(w, 400, fmt.Sprintf("currency %q is not a three-letter code", req.Currency))
		return
	}
	slot, err := worldline.SlotName(req.Slot)
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	// As the cycle: the file is named for now, and settles the coverage
	// day unless the caller names one.
	at := labclock.Now().In(time.Local)
	cfg := e.acq.config()
	fromDate, toDate := cfg.CoverageFor(at)
	if req.Date != "" {
		if _, err := time.Parse("2006-01-02", req.Date); err != nil {
			httputilx.Error(w, 400, fmt.Sprintf("date %q is not YYYY-MM-DD", req.Date))
			return
		}
		fromDate, toDate = req.Date, req.Date
	}

	started := e.guard.start()
	f, found, err := e.acq.cutOne(cfg, slot, ccy, fromDate, toDate, at)
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	// A render has no effect to hold off, but one that read the
	// transactions a reset then cleared describes a file that no longer
	// settles anything.
	if e.guard.moved(started) {
		httputilx.Error(w, 409, "worldline was reset while this file was rendered: render it again")
		return
	}
	if !found {
		httputilx.Error(w, 404, nothingSettles(e.env, cfg, slot, ccy, fromDate, toDate))
		return
	}
	httputilx.WriteJSON(w, 200, renderedFile{
		Name: f.Filename, Content: string(f.Bytes), Currency: f.Currency, MIDs: f.MIDs,
		Total: formatMinor(f.AmountCents), Items: f.Items, Slot: f.Slot, FromDate: f.FromDate, ToDate: f.ToDate,
	})
}

// cutOne is the file of ccy the cycle under cfg would cut at `at`, by the
// cycle's own Cut: nothing is written. A fixture's figures are read from
// its bytes, which the cut leaves to the reader.
func (s *settlementApp) cutOne(cfg worldline.CycleConfig, slot, ccy, fromDate, toDate string, at time.Time) (worldline.CutFile, bool, error) {
	files, err := worldline.Cut(s.store, cfg, slot, fromDate, toDate, at)
	if err != nil {
		return worldline.CutFile{}, false, err
	}
	for _, f := range files {
		if f.Currency == "" {
			p, err := worldline.ParseSettlementFile(bytes.NewReader(f.Bytes))
			if err != nil {
				continue
			}
			f.Currency, f.MIDs = strings.ToUpper(p.Meta.SettlementCurrency), p.MIDs()
			f.AmountCents, f.Items = p.Meta.SettlementAmount, p.Meta.NumberOfItems
		}
		if f.Currency == ccy {
			return f, true, nil
		}
	}
	return worldline.CutFile{}, false, nil
}

// nothingSettles is render's 404: why there is no file.
func nothingSettles(env string, cfg worldline.CycleConfig, slot, ccy, fromDate, toDate string) string {
	day := fromDate
	if toDate != fromDate {
		day = fromDate + " to " + toDate
	}
	if cfg.Source == worldline.FileSourceFixture {
		return fmt.Sprintf("nothing settles in %s in %s's %s file: worldline serves fixtures, and none for the %s slot is a %s settlement file",
			ccy, tenant.Normalize(env), slot, slot, ccy)
	}
	return fmt.Sprintf("nothing settles in %s on %s in %s: worldline holds no %s transaction of that day there",
		ccy, day, tenant.Normalize(env), ccy)
}

// publishedFile is publish's answer.
type publishedFile struct {
	// Published is the name the file has in download/ and to_WLNORDIC/,
	// as a cycle run lists it.
	Published string   `json:"published"`
	Name      string   `json:"name"`
	Currency  string   `json:"currency"`
	Total     string   `json:"total"`
	MIDs      []string `json:"mids"`
	LumpSum   *LumpSum `json:"lump_sum"`
}

// publishSettlementFile publishes the settlement file in the body, plain
// text, in the named environment's folders under ?name=, as the cycle
// publishes one it cut, and with ?wire=true wires its total as the morning
// cycle does. It refuses "*": a file is one environment's.
func (s *sim) publishSettlementFile(w http.ResponseWriter, r *http.Request) {
	e, ok := s.one(w, r)
	if !ok {
		return
	}
	// Work in flight when a reset lands is dropped from here on, as a
	// cycle run's is from its start.
	started := e.guard.start()
	q := r.URL.Query()
	name := q.Get("name")
	if err := checkPublishName(name); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	wire := false
	if v := q.Get("wire"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			httputilx.Error(w, 400, fmt.Sprintf("wire=%q: want true or false", v))
			return
		}
		wire = b
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSettlementFileBytes+1))
	if err != nil {
		httputilx.Error(w, 400, "read body: "+err.Error())
		return
	}
	if len(body) > maxSettlementFileBytes {
		httputilx.Error(w, 413, fmt.Sprintf("a settlement file here is at most %d MiB", maxSettlementFileBytes>>20))
		return
	}
	parsed, err := worldline.ParseSettlementFile(bytes.NewReader(body))
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	name = delivered(name, e.acq.config().Identifier, labclock.Now())
	out, err := s.publishFile(e, started, name, body, parsed, wire)
	switch {
	case errors.Is(err, errReset):
		httputilx.Error(w, 409, "worldline was reset while this file was being published: nothing of it is left, publish it again")
		return
	case err != nil:
		httputilx.Error(w, 500, err.Error())
		return
	}
	httputilx.WriteJSON(w, 201, out)
}

// checkPublishName refuses a name that is not one file's in the folder it
// is published to.
func checkPublishName(name string) error {
	switch {
	case name == "":
		return errors.New("name is required: the file's name, as the cycle names its own ({YYYYMMDDHHMMSS}_{identifier}_{ER|AR}_{CCY}.csv)")
	case strings.ContainsAny(name, `/\`) || strings.Contains(name, ".."):
		return fmt.Errorf("name %q: a file name, with no path separator and no ..", name)
	case len(name) > maxPublishName:
		return fmt.Errorf("name is %d bytes, at most %d", len(name), maxPublishName)
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		return fmt.Errorf("name %q has a control character", name)
	}
	return nil
}

// publishFile puts f, whose bytes are body, in e's download/ and
// to_WLNORDIC/ under name, plain, as the cycle publishes a file it cut,
// and with wire wires its total to e's safeguarding account
// as the morning cycle does: only a file that is not an afternoon (AR)
// confirmation, and only a positive total. Each effect lands only if no
// reset came since started; the first that would land after one ends it
// with errReset, and what landed before is the reset's to take back.
func (s *sim) publishFile(e *envSim, started uint64, name string, body []byte, f *worldline.ParsedFile, wire bool) (publishedFile, error) {
	ccy := strings.ToUpper(f.Meta.SettlementCurrency)
	amount := f.Meta.SettlementAmount
	out := publishedFile{Name: name, Currency: ccy, Total: formatMinor(amount), MIDs: f.MIDs()}
	wire = wire && worldline.FilenameSlot(name) != worldline.SlotAfternoon && amount > 0

	var err error
	if !e.guard.apply(started, func() {
		out.Published, err = wlsftp.Publish(e.acq.root, name, body)
		if err != nil {
			return
		}
		e.acq.publishedByHand(out.Published)
		if !wire {
			s.recordPublished(e.env, out)
		}
	}) {
		return out, errReset
	}
	if err != nil {
		return out, err
	}
	if wire {
		// The cycle's reference names the day settled, its toDate, which
		// is the file's value date.
		valueDate := f.Meta.ValueDate
		if valueDate == "" {
			valueDate = labclock.Now().UTC().Format("2006-01-02")
		}
		if !e.guard.apply(started, func() {
			ls := e.acq.wireLumpSum(ccy, amount, valueDate)
			e.acq.wiredByHand()
			out.LumpSum = &ls
			s.recordPublished(e.env, out)
		}) {
			return out, errReset
		}
	}
	status := "none"
	if out.LumpSum != nil {
		status = out.LumpSum.Status
	}
	log.Printf("worldline: settlement file published by hand env=%s file=%s currency=%s total=%s mids=%d lump_sum=%s",
		tenant.Normalize(e.env), out.Published, ccy, out.Total, len(out.MIDs), status)
	return out, nil
}

// publishedByHand remembers a file a hook published, for the reset.
func (s *settlementApp) publishedByHand(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.byHand {
		if n == name {
			return
		}
	}
	s.byHand = append(s.byHand, name)
	if len(s.byHand) > keptByHand {
		s.byHand = s.byHand[len(s.byHand)-keptByHand:]
	}
}

// wiredByHand counts a lump sum a hook wired, for the reset.
func (s *settlementApp) wiredByHand() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handLumpSums++
}

// recordPublished puts a file published by hand in env's sftp activity,
// beside the platform's own traffic. The caller holds env's guard.
func (s *sim) recordPublished(env string, out publishedFile) {
	ev := activity.Event{
		Env: env, Op: opPublished, Status: activity.StatusOK,
		Summary: fmt.Sprintf("published %s: %s %s for %d MID(s)", out.Published, out.Currency, out.Total, len(out.MIDs)),
		Detail: map[string]string{
			"path": wlsftp.DirDownload + "/" + out.Published, "file": out.Published,
			"currency": out.Currency, "total": out.Total, "mids": strings.Join(out.MIDs, ","),
		},
	}
	if ls := out.LumpSum; ls != nil {
		ev.Summary += "; lump sum " + ls.Status
		if ls.AccountID != "" {
			ev.Summary += " to " + ls.AccountID
			ev.Detail["account_id"] = ls.AccountID
		}
		ev.Detail["lump_sum"] = ls.Status
		if ls.Status != "credited" {
			ev.Status = activity.StatusWarn
		}
	}
	s.sftpLog.Record(ev)
}

// worldlineStamp is the timestamp Worldline's own file names start with
// (worldline.BamboraFilename).
var worldlineStamp = regexp.MustCompile(`^[0-9]{14}_`)

// worldlineName is a whole name in Worldline's shape: its slot and currency.
var worldlineName = regexp.MustCompile(`^[0-9]{14}_.+_(ER|AR)_([A-Z]{3})\.csv$`)

// delivered names a file as Worldline names the files it drops: under the
// environment's identifier, at the moment it is published. A platform
// takes only its own identifier's files, and only those of its own day, so
// a file rendered yesterday, or named for another customer, must read as
// this customer's file of today. A name stamped as Worldline stamps its
// files but otherwise its own keeps the rest of it; any other name is kept.
func delivered(name, identifier string, at time.Time) string {
	if m := worldlineName.FindStringSubmatch(name); m != nil && identifier != "" {
		return worldline.BamboraFilename(identifier, at, m[1], m[2])
	}
	if !worldlineStamp.MatchString(name) {
		return name
	}
	return worldline.FileStamp(at) + name[14:]
}
