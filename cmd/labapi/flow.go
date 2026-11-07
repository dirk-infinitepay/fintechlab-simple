package main

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
)

// The System-in-test view, server side.
//
// Everything here is derived, nothing is stored: the vendors already keep a
// ring of the traffic that reached them and the registered platform already
// knows its stage chain (its plugin's settlement.status_path), so this
// endpoint's whole job is to read those and put them in the shape a
// sequence diagram needs — participants down the top, one entry per hop
// between them.
//
// Deriving it here rather than in the browser is deliberate. The mapping
// from "b4b recorded 6 events with op payment.create" to "the payouts hop
// carried 6 messages" is the part with judgement in it, and judgement in a
// Go file can be tested.

// flowParticipant is one vertical in the diagram.
type flowParticipant struct {
	ID     string     `json:"id"`
	Label  string     `json:"label"`
	Role   string     `json:"role"`            // what it is, in three words
	Kind   string     `json:"kind"`            // vendor | platform
	Status string     `json:"status"`          // up | down | unknown | not-probed
	Stats  []flowStat `json:"stats"`           // the two numbers on the box
	Error  string     `json:"error,omitempty"` // why this box knows nothing
}

type flowStat struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// flowStep is one horizontal arrow.
//
// Count is what the vendor has actually recorded, and the browser diffs it
// between polls to decide what just happened — which is why this carries a
// number rather than a state. A server that said "active" would be saying
// it about the moment it was asked, and the whole point of the view is the
// moment after that.
type flowStep struct {
	ID    string `json:"id"`
	From  string `json:"from"`
	To    string `json:"to"`
	Label string `json:"label"`
	Note  string `json:"note,omitempty"`
	// Multi marks a hop that carries many messages per run — a batch of
	// payouts, thirty callbacks — which the diagram colours differently
	// because "some of them worked" is a real answer for those and not for
	// the others.
	Multi bool `json:"multi"`
	Count int  `json:"count"`
	// Capped says the window was full, so Count is a floor rather than a
	// total. A diagram that prints "256" off a 256-event buffer is printing
	// the size of its own window and calling it traffic.
	Capped      bool   `json:"capped,omitempty"`
	Failed      int    `json:"failed"`
	Refused     int    `json:"refused"`
	LastAt      string `json:"last_at,omitempty"`
	LastSummary string `json:"last_summary,omitempty"`
	Source      string `json:"source,omitempty"`     // which service's card to open for detail
	SourceLog   string `json:"source_log,omitempty"` // and which of its logs

	// events is what Count counted, newest first. Not in /api/flow — the
	// diagram needs the number — but the hop drawer shows exactly these,
	// so the arrow and the drawer can never disagree.
	events []activity.Event
}

type flowResponse struct {
	Participants []flowParticipant `json:"participants"`
	Steps        []flowStep        `json:"steps"`
	Run          any               `json:"run"`
	Report       []flowStat        `json:"report"`
	Errors       map[string]string `json:"errors,omitempty"`
	// Hidden is what the diagram leaves out because a stand-in's card is
	// hidden, and how much traffic went to it all the same: an arrow taken
	// away is a silence, and a silence has to say why (design-system.md,
	// contract 10).
	Hidden []flowHidden `json:"hidden,omitempty"`
}

type flowHidden struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Steps []string `json:"steps"`
	Count int      `json:"count"`
}

// flowLog is one log as a vendor serves it, pared to what is needed here.
type flowLog struct {
	Name   string           `json:"name"`
	Title  string           `json:"title"`
	Total  int64            `json:"total"`
	Events []activity.Event `json:"events"`
	byOp   map[string][]activity.Event
}

func (l *flowLog) index() {
	l.byOp = map[string][]activity.Event{}
	for _, e := range l.Events {
		l.byOp[e.Op] = append(l.byOp[e.Op], e)
	}
}

type flowLogs struct {
	Logs []flowLog `json:"logs"`
}

// find returns the events for one op, or for every op sharing a prefix when
// the name ends in a dot — "sftp." covers session, list and download.
func (f *flowLogs) find(logName, op string) []activity.Event {
	for i := range f.Logs {
		if f.Logs[i].Name != logName {
			continue
		}
		if !strings.HasSuffix(op, ".") {
			return f.Logs[i].byOp[op]
		}
		var out []activity.Event
		for name, evs := range f.Logs[i].byOp {
			if strings.HasPrefix(name, op) {
				out = append(out, evs...)
			}
		}
		sort.Slice(out, func(a, b int) bool { return out[a].Seq > out[b].Seq })
		return out
	}
	return nil
}

func (f *flowLogs) total(logName string) int64 {
	for i := range f.Logs {
		if f.Logs[i].Name == logName {
			return f.Logs[i].Total
		}
	}
	return 0
}

// flowWindow is how many events are asked of each vendor. The rings hold
// 256 and one settlement run writes well under a hundred, so this is the
// whole of a run and a little history either side.
const flowWindow = 256

// Notification deliveries are split by who was being called.
//
// Banking Circle records the subscriber's endpoint on every batch, and the
// lab's own receiver stub answers only to a name this catalogue knows. So a
// delivery to a known name is the lab talking to itself; anything else is
// the system under test, subscribed with its own endpoint. Those are two
// different arrows, and collapsing them is what let a settlement sit at
// IN_PROGRESS with a green "notifications" hop above it.
//
// A batch whose endpoint cannot be read stays on the lab's side. The
// platform arrow asserts "your own listener was called", and an assertion
// like that must never be a guess.

// endpointHost is the host a subscription's endpoint points at, or "" when
// it is not a URL this can read.
func endpointHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// labDelivery reports whether a notification went to one of the lab's own
// services. Built from the catalogue rather than a hardcoded name, so a
// renamed stub does not quietly start counting as the platform.
func labDelivery(known map[string]bool) func(activity.Event) bool {
	return func(e activity.Event) bool {
		host := endpointHost(e.Detail["endpoint"])
		if host == "" {
			return true
		}
		return known[host]
	}
}

// splitEvents divides one log by that verdict, keeping each side in the
// order it arrived.
func splitEvents(evs []activity.Event, isLab func(activity.Event) bool) (lab, platform []activity.Event) {
	for _, e := range evs {
		if isLab(e) {
			lab = append(lab, e)
			continue
		}
		platform = append(platform, e)
	}
	return lab, platform
}

// flowInputs is everything the diagram is derived from, read once.
type flowInputs struct {
	at     func(string) *flowLogs
	runner map[string]any
	errs   map[string]string
	isLab  func(activity.Event) bool
	platID string
	runs   string
	stages map[string][]string
}

func (a *app) readFlow(ctx context.Context) flowInputs {
	var (
		mu     sync.Mutex
		logs   = map[string]*flowLogs{}
		errs   = map[string]string{}
		runner map[string]any
		wg     sync.WaitGroup
	)

	// The platform is whichever registered plugin settles. With none, the
	// diagram still draws the vendors' side of whatever reached them.
	plat, _, hasPlat := a.cat.SettlementPlugin()
	ids := []string{"worldline", "b4b", "banking-circle", "bank"}
	if hasPlat {
		ids = append(ids, plat.ID)
	}

	// Every vendor is asked at once. Serially this is four round trips on
	// a 1s poll, and the view exists to look live.
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			var got flowLogs
			if err := a.fetchActivity(ctx, id, &got); err != nil {
				mu.Lock()
				errs[id] = err.Error()
				mu.Unlock()
				return
			}
			for i := range got.Logs {
				got.Logs[i].index()
			}
			mu.Lock()
			logs[id] = &got
			mu.Unlock()
		}(id)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		var st map[string]any
		if hasPlat && plat.Settlement.StatusPath != "" {
			if err := a.getJSON(ctx, a.client, plat.BaseURL+plat.Settlement.StatusPath, &st); err != nil {
				mu.Lock()
				errs[plat.ID+"-status"] = err.Error()
				mu.Unlock()
				return
			}
		}
		mu.Lock()
		runner = st
		mu.Unlock()
	}()
	wg.Wait()

	empty := &flowLogs{}
	in := flowInputs{
		at: func(id string) *flowLogs {
			if l, ok := logs[id]; ok {
				return l
			}
			return empty
		},
		runner: runner,
		errs:   errs,
		isLab:  labDelivery(a.knownHosts()),
	}
	if hasPlat {
		in.platID, in.runs, in.stages = plat.ID, plat.Settlement.RunsLog, plat.Settlement.Stages
	}
	return in
}

func (a *app) flow(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	in := a.readFlow(ctx)

	out := flowResponse{
		Participants: a.flowParticipants(in.at, in.errs, in.isLab, in.platID, in.runs),
		Steps:        flowSteps(in.at, in.runner, in.isLab, in.stages),
		Run:          in.runner["last_run"],
		Errors:       in.errs,
	}
	if in.runner != nil {
		if live, ok := in.runner["running_now"]; ok && live != nil {
			out.Run = live
		}
	}
	out.Report = flowReport(in.at, in.runner, in.isLab)
	// A hidden stand-in is hidden here too, with the hop into it: the
	// developer said the receiver is not part of their picture. What still
	// went to it is counted under the diagram rather than dropped.
	if a.cat.Hidden("receiver") {
		into := func(s flowStep) bool { return s.To == "receiver" || s.From == "receiver" }
		h := flowHidden{ID: "receiver", Steps: []string{}}
		if svc, ok := a.cat.Get("receiver"); ok {
			h.Name = svc.Name
		}
		for _, st := range out.Steps {
			if into(st) {
				h.Steps = append(h.Steps, st.Label)
				h.Count += st.Count
			}
		}
		out.Hidden = append(out.Hidden, h)
		out.Participants = without(out.Participants, func(p flowParticipant) bool { return p.ID == "receiver" })
		out.Steps = without(out.Steps, into)
	}
	httputilx.WriteJSON(w, 200, out)
}

// flowHopResponse is one arrow and the traffic behind it.
type flowHopResponse struct {
	Step   flowStep         `json:"step"`
	Events []activity.Event `json:"events"`
	// Kept says how many of the counted events are in Events: the drawer
	// shows the newest hopWindow and says so when there were more.
	Kept int `json:"kept"`
	// Stages is set for a hop the platform makes to itself: its stages in
	// the order the plugin declared them, with how each one stands. A stage
	// the run has not reached has no status.
	Stages []flowStage       `json:"stages,omitempty"`
	Errors map[string]string `json:"errors,omitempty"`
}

type flowStage struct {
	Stage  string `json:"stage"`
	Status string `json:"status"`
}

// hopWindow is how many of a hop's events the drawer is sent.
const hopWindow = 100

// flowHop is the hop drawer's data: the same step /api/flow draws, with
// the events it counted. Computed by the same code, never re-derived, so
// an arrow saying 6 and a drawer listing 5 cannot happen.
func (a *app) flowHop(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqContext(r)
	defer cancel()
	id := r.PathValue("id")
	in := a.readFlow(ctx)
	for _, s := range flowSteps(in.at, in.runner, in.isLab, in.stages) {
		if s.ID != id {
			continue
		}
		out := flowHopResponse{Step: s, Events: s.events, Kept: len(s.events), Errors: in.errs}
		if len(out.Events) > hopWindow {
			out.Events = out.Events[:hopWindow]
			out.Kept = hopWindow
		}
		if out.Events == nil {
			out.Events = []activity.Event{}
		}
		if names, ok := in.stages[id]; ok {
			// Stages are the platform's own; its card is where they live.
			out.Step.Source, out.Step.SourceLog = in.platID, in.runs
			ran := runStages(in.runner)
			for _, n := range names {
				out.Stages = append(out.Stages, flowStage{Stage: n, Status: ran[n]})
			}
		}
		httputilx.WriteJSON(w, 200, out)
		return
	}
	httputilx.WriteJSON(w, 404, map[string]string{"error": "no hop " + strconv.Quote(id) + " in the diagram"})
}

// fetchActivity reads one service's rings, through the same path the
// activity panels use so there is one place that knows Banking Circle needs
// a bearer token.
func (a *app) fetchActivity(ctx context.Context, id string, out any) error {
	svc, ok := a.cat.Get(id)
	if !ok || svc.Activity == "" {
		return nil
	}
	path := svc.Activity + "?limit=" + itoa(flowWindow)
	if id == "banking-circle" {
		return a.bc.AuthorizedGet(ctx, path, out)
	}
	return a.getJSON(ctx, a.client, svc.BaseURL+path, out)
}

// knownHosts is every name the lab's own services answer to on its network.
// The catalogue ids double as the compose service names, which is how a
// vendor reaches the receiver stub in the first place.
func (a *app) knownHosts() map[string]bool {
	out := map[string]bool{}
	for _, svc := range a.cat.Services {
		out[svc.ID] = true
	}
	return out
}

func (a *app) flowParticipants(at func(string) *flowLogs, errs map[string]string, isLab func(activity.Event) bool, platID, runsLog string) []flowParticipant {
	state := func(id string) string {
		if id == "" {
			return "unknown"
		}
		st, _ := a.mon.Get(id)
		if st.State == "" {
			return "unknown"
		}
		return st.State
	}
	label := func(id, fallback string) string {
		if s, ok := a.cat.Get(id); ok {
			return s.Name
		}
		return fallback
	}

	wl := at("worldline")
	b4b := at("b4b")
	bc := at("banking-circle")
	bank := at("bank")
	labBatches, _ := splitEvents(bc.find("notifications", "notification"), isLab)
	opened := 0
	for _, e := range bank.find("credits", "credit") {
		if e.Detail["opened"] == "true" {
			opened++
		}
	}

	return []flowParticipant{
		{
			ID: "worldline", Label: label("worldline", "Worldline"), Role: "acquirer",
			Kind: "vendor", Status: state("worldline"), Error: errs["worldline"],
			Stats: []flowStat{
				{Label: "files collected", Value: itoa(len(wl.find("sftp", "sftp.download")))},
				{Label: "sessions", Value: itoa(len(wl.find("sftp", "sftp.session")))},
			},
		},
		{
			ID: "platform", Label: label(platID, "Platform"), Role: "system under test",
			Kind: "platform", Status: state(platID), Error: errs[platID],
			Stats: []flowStat{
				{Label: "runs", Value: itoa64(at(platID).total(runsLog))},
				{Label: "callbacks taken", Value: itoa(len(b4b.find("callbacks", "callback")))},
			},
		},
		{
			ID: "b4b", Label: label("b4b", "B4B Payments"), Role: "payout rail",
			Kind: "vendor", Status: state("b4b"), Error: errs["b4b"],
			Stats: []flowStat{
				{Label: "payouts received", Value: itoa(len(b4b.find("payments", "payment.create")))},
				{Label: "callbacks sent", Value: itoa64(b4b.total("callbacks"))},
			},
		},
		{
			ID: "banking-circle", Label: label("banking-circle", "Banking Circle"), Role: "safeguarding + rails",
			Kind: "vendor", Status: state("banking-circle"), Error: errs["banking-circle"],
			Stats: []flowStat{
				{Label: "payments in", Value: itoa(len(bc.find("payments", "payment.create")))},
				// Deliveries, not everything in the notifications log. That
				// log also carries what a paused subscription has queued,
				// and a notification waiting to be released is precisely
				// not one this vendor has sent.
				{Label: "notifications", Value: itoa(len(bc.find("notifications", "notification")))},
			},
		},
		{
			// The far end of the money: the merchants' own bank, credited by
			// Banking Circle once a payout is processed.
			ID: "bank", Label: label("bank", "Business bank"), Role: "merchants' bank",
			Kind: "vendor", Status: state("bank"), Error: errs["bank"],
			Stats: []flowStat{
				{Label: "payouts credited", Value: itoa(countOK(bank.find("credits", "credit")))},
				{Label: "accounts opened", Value: itoa(opened)},
			},
		},
		{
			ID: "receiver", Label: label("receiver", "Webhook receiver"), Role: "notification sink",
			Kind: "platform", Status: state("receiver"),
			Stats: []flowStat{
				{Label: "batches taken", Value: itoa(countOK(labBatches))},
			},
		},
	}
}

func flowSteps(at func(string) *flowLogs, runner map[string]any, isLab func(activity.Event) bool, stages map[string][]string) []flowStep {
	wl := at("worldline")
	b4b := at("b4b")
	bc := at("banking-circle")
	bank := at("bank")
	labBatches, platformBatches := splitEvents(bc.find("notifications", "notification"), isLab)

	step := func(id, from, to, label, note, source string, multi bool, evs []activity.Event) flowStep {
		// source is "service" or "service:log" — where the whole log lives.
		svc, log, _ := strings.Cut(source, ":")
		s := flowStep{
			ID: id, From: from, To: to, Label: label, Note: note,
			Multi: multi, Count: len(evs), Source: svc, SourceLog: log,
			Capped: len(evs) >= flowWindow, events: evs,
		}
		for _, e := range evs {
			switch e.Status {
			case activity.StatusBad:
				s.Failed++
			case activity.StatusWarn:
				s.Refused++
			}
		}
		if len(evs) > 0 {
			// The events arrive newest-first, which is what the panels want
			// and what a diagram's "last thing that happened" wants too.
			s.LastAt = evs[0].At.UTC().Format(time.RFC3339Nano)
			s.LastSummary = evs[0].Summary
		}
		return s
	}

	steps := []flowStep{
		step("pull", "platform", "worldline",
			"connect + list", "the lab's own settlement service polls this on a timer; a run triggered from S3 does not use it", "worldline:sftp", false,
			append(wl.find("sftp", "sftp.session"), wl.find("sftp", "sftp.list")...)),
		step("collect", "worldline", "platform",
			"settlement file", "the Bambora file, PGP-encrypted — collected by acquirer-fts, not by a run started here", "worldline:sftp", false,
			wl.find("sftp", "sftp.download")),
		step("ingest", "platform", "platform",
			"parse + daily movements", "", "", false, nil),
		step("fund", "platform", "banking-circle",
			"safeguarding balance", "funds must cover the run before it leaves", "banking-circle:payments", false,
			bc.find("payments", "payment.incoming")),
		step("payouts", "platform", "b4b",
			"create payouts", "one per merchant settlement", "b4b:payments", true,
			b4b.find("payments", "payment.create")),
		step("bridge", "b4b", "banking-circle",
			"bridge approved payouts", "B4B moves the money at the bank", "banking-circle:payments", true,
			bc.find("payments", "payment.create")),
		step("land", "banking-circle", "bank",
			"payouts credited", "a processed payout arrives at the merchant's own bank, by IBAN — the end of the money's path",
			"bank:credits", true, bank.find("credits", "credit")),
		step("callbacks", "b4b", "platform",
			"lifecycle callbacks", "SUBMITTED → IN_PROGRESS happens here", "b4b:callbacks", true,
			b4b.find("callbacks", "callback")),
		step("notify", "banking-circle", "receiver",
			"notification batches", "up to five events per batch, to the lab's own stub", "banking-circle:notifications", true,
			labBatches),
		step("confirm", "banking-circle", "platform",
			"payout confirmations",
			"the same batches to the platform's own subscriber — what moves a payout off IN_PROGRESS; "+
				"grey under a green hop above means nothing of yours heard it",
			"banking-circle:notifications", true, platformBatches),
		step("reports", "platform", "platform",
			"daily reports", "five report jobs; the mail hop fails alone when no mailer runs", "", true, nil),
		// Last, because it comes last: the platform's reconciliation sweep
		// runs an hour or more after the settlement it checks. Drawn from the
		// bank to the platform, as the bookings it returns travel.
		step("recon", "banking-circle", "platform",
			"intraday reconciliation",
			"the platform's sweep reading the day's bookings — a payout it finds processed moves to SUCCESS "+
				"without waiting for a webhook; refused reads are amber",
			"banking-circle:reports", false, bc.find("reports", "report.intraday")),
	}

	// The two self-arrows are stages, not vendor traffic, so they are read
	// from the platform's run rather than from a ring — under the names the
	// platform's plugin gave them.
	ran := runStages(runner)
	fill := func(id string, names ...string) {
		for i := range steps {
			if steps[i].ID != id {
				continue
			}
			for _, n := range names {
				st, ok := ran[n]
				if !ok {
					continue
				}
				steps[i].Count++
				switch st {
				case "FAILED":
					steps[i].Failed++
				case "PROCESSING", "PENDING":
					steps[i].Refused++ // in flight, not refused; the client reads it as "not finished"
				}
			}
			steps[i].LastSummary = strings.Join(names, ", ")
		}
	}
	fill("ingest", stages["ingest"]...)
	fill("reports", stages["reports"]...)
	return steps
}

// runStages pulls stage -> status out of whatever the runner answered,
// tolerating every shape it might not be: this is another service's JSON
// and a view that panics on it would take the console down with it.
func runStages(runner map[string]any) map[string]string {
	out := map[string]string{}
	if runner == nil {
		return out
	}
	var run map[string]any
	for _, key := range []string{"running_now", "last_run"} {
		if v, ok := runner[key].(map[string]any); ok && v != nil {
			run = v
			break
		}
	}
	if run == nil {
		return out
	}
	list, _ := run["stages"].([]any)
	for _, item := range list {
		s, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := s["stage"].(string)
		status, _ := s["status"].(string)
		if name != "" {
			out[name] = status
		}
	}
	return out
}

// flowReport is the closing summary. It reports what this lab can actually
// see — every number here is one another service already publishes. Fees
// are the honest omission: they are computed inside the platform's workers
// and never leave them, so rather than invent a figure the view says where
// they live.
func flowReport(at func(string) *flowLogs, runner map[string]any, isLab func(activity.Event) bool) []flowStat {
	b4b := at("b4b")
	bc := at("banking-circle")
	wl := at("worldline")
	bank := at("bank")
	stages := runStages(runner)
	batches := bc.find("notifications", "notification")
	_, platformBatches := splitEvents(batches, isLab)

	done, failed := 0, 0
	for _, st := range stages {
		switch st {
		case "COMPLETED":
			done++
		case "FAILED":
			failed++
		}
	}

	payouts, amount, confirmed := "0", "", ""
	if runner != nil {
		for _, key := range []string{"running_now", "last_run"} {
			run, _ := runner[key].(map[string]any)
			if run == nil {
				continue
			}
			if p, ok := run["payouts"].(map[string]any); ok && p != nil {
				payouts = fmtAny(p["count"])
				amount = fmtAny(p["total"])
				// A payout the bank has confirmed, as the platform's own
				// books record it. Reported as "0 of 6" rather than left
				// out when none have landed, because none landing is the
				// answer somebody is looking for.
				confirmed = itoa(statusCount(p["statuses"], "SUCCESS")) + " of " + payouts
			}
			break
		}
	}

	stats := []flowStat{
		{Label: "files collected", Value: itoa(len(wl.find("sftp", "sftp.download")))},
		{Label: "stages completed", Value: itoa(done) + " of " + itoa(len(stages))},
		{Label: "merchant payouts", Value: payouts},
	}
	if amount != "" {
		stats = append(stats, flowStat{Label: "settled", Value: amount})
	}
	stats = append(stats,
		flowStat{Label: "payouts at the bank", Value: itoa(len(bc.find("payments", "payment.create")))},
		flowStat{Label: "credited to merchants' banks", Value: itoa(countOK(bank.find("credits", "credit")))},
		flowStat{Label: "callbacks delivered", Value: itoa(countOK(b4b.find("callbacks", "callback")))},
		flowStat{Label: "notification batches", Value: itoa(len(batches))},
		flowStat{Label: "confirmations to the platform", Value: itoa(len(platformBatches))},
	)
	if confirmed != "" {
		stats = append(stats, flowStat{Label: "payouts confirmed", Value: confirmed})
	}
	if failed > 0 {
		stats = append(stats, flowStat{Label: "stages failed", Value: itoa(failed)})
	}
	return stats
}

// statusCount reads one bucket out of the runner's payout breakdown,
// tolerating every shape another service's JSON might not be.
func statusCount(v any, status string) int {
	m, ok := v.(map[string]any)
	if !ok {
		return 0
	}
	n, ok := m[status].(float64)
	if !ok {
		return 0
	}
	return int(n)
}

func countOK(evs []activity.Event) int {
	n := 0
	for _, e := range evs {
		if e.Status == activity.StatusOK {
			n++
		}
	}
	return n
}

func without[T any](in []T, drop func(T) bool) []T {
	out := in[:0:0]
	for _, v := range in {
		if !drop(v) {
			out = append(out, v)
		}
	}
	return out
}

func itoa(n int) string     { return strconv.Itoa(n) }
func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

func fmtAny(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return ""
	}
}
