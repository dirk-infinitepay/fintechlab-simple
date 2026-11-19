package main

// Ongoing monitoring: the worklists a platform creates, the businesses and
// individuals it enrols on them, the matches KYC6's re-screening finds
// against those records, and the reviews the platform sends back to close
// them. Each environment has its own worklists.
//
// KYC6 finds matches on its own schedule; the lab cannot, so a test puts one
// on a record at POST /sim/kyc6/worklists/{worklistId}/{kind}/{recordId}/matches
// and reads what the platform sent back at GET /sim/kyc6/worklists/….
// docs/api/services/kyc6.md says which parts of this are KYC6's documented
// contract and which are the lab's reading of it.

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
	"github.com/webduvet/fintechlab-simple/internal/vendorsim"
)

const (
	kindBusinesses  = "businesses"
	kindIndividuals = "individuals"

	matchOpen          = "open"
	matchTruePositive  = "true-positive"
	matchFalsePositive = "false-positive"

	recordOpen   = "open"
	recordClosed = "closed"

	endpointReviews     = "reviews"
	endpointAutoReviews = "auto-reviews"

	// reviewExistsCode is KYC6's refusal of an auto-review for a match that
	// already holds a review.
	reviewExistsCode    = "MAPI-312"
	reviewExistsMessage = "Auto review already exists"

	defaultMatchScore = 92

	// reviewsKept is how many review entries a record keeps for the lab's
	// read, the newest: as many as the journal keeps calls.
	reviewsKept = activity.DefaultCapacity
)

var defaultMatchDatasets = []string{"SAN-CURRENT"}

var recordKinds = []string{kindBusinesses, kindIndividuals}

// monitorEnv is one environment's ongoing monitoring.
type monitorEnv struct {
	worklists    map[string]*worklist
	matchesArmed int
}

type worklist struct {
	id       string
	settings map[string]any
	created  time.Time
	updated  time.Time
	records  map[string]map[string]*record
}

type record struct {
	id        string
	kind      string
	name      string
	dob       string
	addresses []map[string]any
	created   time.Time
	updated   time.Time
	matches   []*match
	// autoReviews are the auto-reviews the record holds, by qrCode: one
	// closes the match with that qrCode now, or as soon as it appears.
	autoReviews map[string]*review
	// reviews are the review entries sent for the record, in order, with the
	// answer each was given: the newest reviewsKept of them.
	reviews []*review
}

type match struct {
	id         string
	resourceID string
	qrCode     string
	name       string
	score      float64
	datasets   []string
	countries  []string
	version    int
	status     string
	lastReview *review
	created    time.Time
}

type review struct {
	Endpoint      string       `json:"endpoint"`
	QRCode        string       `json:"qrCode"`
	ReferenceID   string       `json:"referenceId"`
	Status        string       `json:"status"`
	ClientComment string       `json:"clientComment"`
	ClientUserID  string       `json:"clientUserId"`
	MatchVersion  any          `json:"matchVersion"`
	At            time.Time    `json:"at"`
	StatusCode    int          `json:"statusCode"`
	Error         *reviewError `json:"error,omitempty"`
}

type reviewError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// routeMonitoring serves the worklist routes. Each is written out whole, so
// the API reference test finds every one.
func (a *app) routeMonitoring() {
	sim := a.sim
	sim.Vendor("PUT /kyc6/worklists/{worklistId}", sim.APIKey(a.putWorklist))
	sim.Vendor("GET /kyc6/worklists/{worklistId}", sim.APIKey(a.getWorklist))
	sim.Vendor("DELETE /kyc6/worklists/{worklistId}", sim.APIKey(a.deleteWorklist))
	sim.Vendor("GET /kyc6/worklists/{worklistId}/matches", sim.APIKey(a.worklistMatches))

	sim.Vendor("GET /kyc6/worklists/{worklistId}/businesses", sim.APIKey(a.listRecords(kindBusinesses)))
	sim.Vendor("PUT /kyc6/worklists/{worklistId}/businesses/{recordId}", sim.APIKey(a.putRecord(kindBusinesses)))
	sim.Vendor("DELETE /kyc6/worklists/{worklistId}/businesses/{recordId}", sim.APIKey(a.deleteRecord(kindBusinesses)))
	sim.Vendor("GET /kyc6/worklists/{worklistId}/businesses/{recordId}/matches", sim.APIKey(a.recordMatches(kindBusinesses)))
	sim.Vendor("PUT /kyc6/worklists/{worklistId}/businesses/{recordId}/matches/reviews", sim.APIKey(a.reviewMatches(kindBusinesses, endpointReviews)))
	sim.Vendor("PUT /kyc6/worklists/{worklistId}/businesses/{recordId}/matches/auto-reviews", sim.APIKey(a.reviewMatches(kindBusinesses, endpointAutoReviews)))

	sim.Vendor("GET /kyc6/worklists/{worklistId}/individuals", sim.APIKey(a.listRecords(kindIndividuals)))
	sim.Vendor("PUT /kyc6/worklists/{worklistId}/individuals/{recordId}", sim.APIKey(a.putRecord(kindIndividuals)))
	sim.Vendor("DELETE /kyc6/worklists/{worklistId}/individuals/{recordId}", sim.APIKey(a.deleteRecord(kindIndividuals)))
	sim.Vendor("GET /kyc6/worklists/{worklistId}/individuals/{recordId}/matches", sim.APIKey(a.recordMatches(kindIndividuals)))
	sim.Vendor("PUT /kyc6/worklists/{worklistId}/individuals/{recordId}/matches/reviews", sim.APIKey(a.reviewMatches(kindIndividuals, endpointReviews)))
	sim.Vendor("PUT /kyc6/worklists/{worklistId}/individuals/{recordId}/matches/auto-reviews", sim.APIKey(a.reviewMatches(kindIndividuals, endpointAutoReviews)))

	sim.Control("GET /sim/kyc6/worklists", a.readWorklists)
	sim.Control("GET /sim/kyc6/worklists/{worklistId}/businesses/{recordId}", a.readRecord(kindBusinesses))
	sim.Control("POST /sim/kyc6/worklists/{worklistId}/businesses/{recordId}/matches", a.armMatch(kindBusinesses))
	sim.Control("GET /sim/kyc6/worklists/{worklistId}/individuals/{recordId}", a.readRecord(kindIndividuals))
	sim.Control("POST /sim/kyc6/worklists/{worklistId}/individuals/{recordId}/matches", a.armMatch(kindIndividuals))
}

// --- state --------------------------------------------------------------

func (a *app) monitorIn(env string) *monitorEnv {
	env = tenant.Normalize(env)
	m := a.monitoring[env]
	if m == nil {
		m = &monitorEnv{worklists: map[string]*worklist{}}
		a.monitoring[env] = m
	}
	return m
}

// findRecord is the caller's record, or the answer that it is not there.
// The caller holds a.mu.
func (a *app) findRecord(env, worklistID, kind, recordID string) (*worklist, *record, string) {
	wl := a.monitorIn(env).worklists[worklistID]
	if wl == nil {
		return nil, nil, "no worklist " + worklistID
	}
	rec := wl.records[kind][recordID]
	if rec == nil {
		return wl, nil, "no " + singular(kind) + " " + recordID + " on worklist " + worklistID
	}
	return wl, rec, ""
}

func singular(kind string) string {
	if kind == kindBusinesses {
		return "business"
	}
	return "individual"
}

// resetMonitoring is the flow reset's share: the records, and with them
// their matches and reviews. The worklists stay, as the platform creates one
// once per customer and would not create it again; all clears them too.
func (a *app) resetMonitoring(scope string, all bool) map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	cleared := map[string]int{"records": 0, "matches": 0, "reviews": 0}
	worklists := 0
	for env, m := range a.monitoring {
		if !tenant.InScope(scope, env) {
			continue
		}
		for _, wl := range m.worklists {
			for _, kind := range recordKinds {
				for _, rec := range wl.records[kind] {
					cleared["records"]++
					cleared["matches"] += len(rec.matches)
					cleared["reviews"] += len(rec.reviews)
				}
			}
			wl.records = emptyRecords()
		}
		m.matchesArmed = 0
		if all {
			worklists += len(m.worklists)
			m.worklists = map[string]*worklist{}
		}
	}
	if all {
		cleared["worklists"] = worklists
	}
	return cleared
}

func emptyRecords() map[string]map[string]*record {
	return map[string]map[string]*record{kindBusinesses: {}, kindIndividuals: {}}
}

// reply is an answer built while holding a.mu and written once it is
// released, so a reader slow to take a large answer off the wire never holds
// up another call. Whatever body holds is the call's own copy: nothing
// another call changes after a.mu is released.
type reply struct {
	status int
	// body is written as JSON; with no body and no errorMessage, only the
	// status is.
	body         any
	errorMessage string
}

func jsonReply(status int, body any) reply {
	return reply{status: status, body: body}
}

func errorReply(status int, message string) reply {
	return reply{status: status, errorMessage: message}
}

func statusReply(status int) reply {
	return reply{status: status}
}

func (rp reply) write(w http.ResponseWriter) {
	switch {
	case rp.errorMessage != "":
		httputilx.Error(w, rp.status, rp.errorMessage)
	case rp.body != nil:
		httputilx.WriteJSON(w, rp.status, rp.body)
	default:
		w.WriteHeader(rp.status)
	}
}

// underLock builds a reply holding a.mu, for the caller to write after it.
func (a *app) underLock(build func() reply) reply {
	a.mu.Lock()
	defer a.mu.Unlock()
	return build()
}

// --- worklists ----------------------------------------------------------

// putWorklist creates a worklist, or replaces the settings of one that
// exists and keeps its records.
func (a *app) putWorklist(w http.ResponseWriter, r *http.Request) {
	var settings map[string]any
	if err := decodeBody(r, &settings); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	if settings == nil {
		httputilx.Error(w, 400, "the worklist's settings must be a JSON object")
		return
	}
	id := r.PathValue("worklistId")
	now := labclock.Now()
	a.underLock(func() reply {
		m := a.monitorIn(tenant.Of(r.Context()))
		wl := m.worklists[id]
		if wl == nil {
			wl = &worklist{id: id, created: now, records: emptyRecords()}
			m.worklists[id] = wl
		}
		wl.settings = settings
		wl.updated = now
		out := wl.view()
		out["worklistId"] = id
		return jsonReply(200, out)
	}).write(w)
}

func (a *app) getWorklist(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("worklistId")
	a.underLock(func() reply {
		wl := a.monitorIn(tenant.Of(r.Context())).worklists[id]
		if wl == nil {
			return errorReply(404, "no worklist "+id)
		}
		return jsonReply(200, wl.view())
	}).write(w)
}

func (a *app) deleteWorklist(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("worklistId")
	a.underLock(func() reply {
		m := a.monitorIn(tenant.Of(r.Context()))
		if m.worklists[id] == nil {
			return errorReply(404, "no worklist "+id)
		}
		delete(m.worklists, id)
		return statusReply(http.StatusNoContent)
	}).write(w)
}

// view is the worklist as KYC6 describes it: its settings as they were
// sent, and the counts of what is on it. The settings map is the view's own;
// a value in it is shared with the worklist, which only ever replaces its
// settings whole and never changes a value in them.
func (wl *worklist) view() map[string]any {
	out := map[string]any{}
	for k, v := range wl.settings {
		out[k] = v
	}
	out["id"] = wl.id
	out["createdDateTimeISO"] = iso(wl.created)
	out["lastUpdatedDateTimeISO"] = iso(wl.updated)
	out["businessMonitorRecordsCount"] = len(wl.records[kindBusinesses])
	out["individualMonitorRecordsCount"] = len(wl.records[kindIndividuals])
	out["businessOpenMatchesCount"] = openMatches(wl.records[kindBusinesses])
	out["individualOpenMatchesCount"] = openMatches(wl.records[kindIndividuals])
	return out
}

func openMatches(records map[string]*record) int {
	n := 0
	for _, rec := range records {
		n += rec.tally()[matchOpen]
	}
	return n
}

// worklistMatches is every open match on the worklist, businesses' and
// individuals' alike, in the order they were found; matches found at the
// same moment by kind, record id and the order they were put on the record.
func (a *app) worklistMatches(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("worklistId")
	a.underLock(func() reply {
		wl := a.monitorIn(tenant.Of(r.Context())).worklists[id]
		if wl == nil {
			return errorReply(404, "no worklist "+id)
		}
		matches := openMatchViews(wl)
		return jsonReply(200, map[string]any{"matches": matches, "totalMatches": len(matches)})
	}).write(w)
}

// openMatchViews is the worklist's open matches as worklistMatches lists
// them. The caller holds a.mu.
func openMatchViews(wl *worklist) []map[string]any {
	type found struct {
		rec *record
		m   *match
	}
	var open []found
	for _, kind := range recordKinds {
		for _, rec := range sortedRecords(wl.records[kind]) {
			for _, m := range rec.matches {
				if m.status == matchOpen {
					open = append(open, found{rec, m})
				}
			}
		}
	}
	sort.SliceStable(open, func(i, j int) bool { return open[i].m.created.Before(open[j].m.created) })
	matches := []map[string]any{}
	for _, f := range open {
		matches = append(matches, map[string]any{
			"matchId":    f.m.id,
			"recordId":   f.rec.id,
			"resourceId": f.m.resourceID,
			"score":      f.m.score,
			"matchName":  f.m.name,
			"datasets":   slices.Clone(f.m.datasets),
			"status":     f.m.status,
		})
	}
	return matches
}

// --- records ------------------------------------------------------------

type recordBody struct {
	Name      string           `json:"name"`
	DOB       string           `json:"dob"`
	Addresses []map[string]any `json:"addresses"`
}

// putRecord enrols a business or an individual, or updates one already
// enrolled and keeps its matches. A worklist the environment does not have
// is created, with no settings: the platform creates its worklist once, at
// customer setup, and the lab loses it on a restart or an all reset.
func (a *app) putRecord(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body recordBody
		if err := decodeBody(r, &body); err != nil {
			httputilx.Error(w, 400, err.Error())
			return
		}
		worklistID, recordID := r.PathValue("worklistId"), r.PathValue("recordId")
		if strings.TrimSpace(body.Name) == "" {
			httputilx.Error(w, 400, "name is required")
			return
		}
		now := labclock.Now()
		a.underLock(func() reply {
			m := a.monitorIn(tenant.Of(r.Context()))
			wl := m.worklists[worklistID]
			if wl == nil {
				wl = &worklist{id: worklistID, settings: map[string]any{}, created: now, updated: now, records: emptyRecords()}
				m.worklists[worklistID] = wl
			}
			rec := wl.records[kind][recordID]
			if rec == nil {
				rec = &record{id: recordID, kind: kind, created: now, autoReviews: map[string]*review{}}
				wl.records[kind][recordID] = rec
			}
			rec.name, rec.dob, rec.addresses, rec.updated = body.Name, body.DOB, body.Addresses, now
			out := rec.view(rec.tally())
			out["monitorRecordId"] = recordID
			out["worklistId"] = worklistID
			return jsonReply(200, out)
		}).write(w)
	}
}

func (a *app) deleteRecord(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		worklistID, recordID := r.PathValue("worklistId"), r.PathValue("recordId")
		a.underLock(func() reply {
			wl, _, missing := a.findRecord(tenant.Of(r.Context()), worklistID, kind, recordID)
			if missing != "" {
				return errorReply(404, missing)
			}
			delete(wl.records[kind], recordID)
			return statusReply(http.StatusNoContent)
		}).write(w)
	}
}

// listRecords is the worklist's businesses or individuals. status=open is
// the records with a match nobody has reviewed yet, status=closed the rest;
// without it, every record. The counts are the whole worklist's, whatever
// the filter.
func (a *app) listRecords(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		worklistID := r.PathValue("worklistId")
		status := r.URL.Query().Get("status")
		if status != "" && status != recordOpen && status != recordClosed {
			httputilx.Error(w, 400, fmt.Sprintf("status %q is not one of open, closed", status))
			return
		}
		a.underLock(func() reply {
			wl := a.monitorIn(tenant.Of(r.Context())).worklists[worklistID]
			if wl == nil {
				return errorReply(404, "no worklist "+worklistID)
			}
			records := sortedRecords(wl.records[kind])
			listed := []map[string]any{}
			openRecords, open, truePositive, falsePositive := 0, 0, 0, 0
			for _, rec := range records {
				counts := rec.tally()
				open += counts[matchOpen]
				truePositive += counts[matchTruePositive]
				falsePositive += counts[matchFalsePositive]
				recStatus := recordStatus(counts)
				if recStatus == recordOpen {
					openRecords++
				}
				if status == "" || recStatus == status {
					listed = append(listed, rec.view(counts))
				}
			}
			return jsonReply(200, map[string]any{
				"monitorRecords":            listed,
				"monitorRecordsCount":       len(records),
				"openMonitorRecordsCount":   openRecords,
				"openMatchesCount":          open,
				"truePositiveMatchesCount":  truePositive,
				"falsePositiveMatchesCount": falsePositive,
				"discardedMatchesCount":     0,
			})
		}).write(w)
	}
}

func sortedRecords(records map[string]*record) []*record {
	out := make([]*record, 0, len(records))
	for _, rec := range records {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// tally is how many of the record's matches are in each match status.
func (rec *record) tally() map[string]int {
	counts := map[string]int{}
	for _, m := range rec.matches {
		counts[m.status]++
	}
	return counts
}

// recordStatus is open while a record's tally has an open match.
func recordStatus(counts map[string]int) string {
	if counts[matchOpen] > 0 {
		return recordOpen
	}
	return recordClosed
}

// view is the record as KYC6 lists it; counts is its tally. Its addresses are
// shared with the record, which only ever replaces them whole and never
// changes one.
func (rec *record) view(counts map[string]int) map[string]any {
	addresses := rec.addresses
	if addresses == nil {
		addresses = []map[string]any{}
	}
	countries := []string{}
	seen := map[string]bool{}
	for _, address := range addresses {
		if geography, _ := address["geography"].(string); geography != "" && !seen[geography] {
			seen[geography] = true
			countries = append(countries, geography)
		}
	}
	openDatasets := []string{}
	seenDataset := map[string]bool{}
	for _, m := range rec.matches {
		if m.status != matchOpen {
			continue
		}
		for _, dataset := range m.datasets {
			if !seenDataset[dataset] {
				seenDataset[dataset] = true
				openDatasets = append(openDatasets, dataset)
			}
		}
	}
	out := map[string]any{
		"id":                     rec.id,
		"name":                   rec.name,
		"countries":              countries,
		"addresses":              addresses,
		"openCount":              counts[matchOpen],
		"truePositiveCount":      counts[matchTruePositive],
		"falsePositiveCount":     counts[matchFalsePositive],
		"discardedCount":         0,
		"status":                 recordStatus(counts),
		"openDatasets":           openDatasets,
		"createdDateTimeISO":     iso(rec.created),
		"lastUpdatedDateTimeISO": iso(rec.updated),
	}
	if rec.dob != "" {
		out["dob"] = rec.dob
	}
	return out
}

// --- matches ------------------------------------------------------------

func (a *app) recordMatches(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.underLock(func() reply {
			_, rec, missing := a.findRecord(tenant.Of(r.Context()), r.PathValue("worklistId"), kind, r.PathValue("recordId"))
			if missing != "" {
				return errorReply(404, missing)
			}
			return jsonReply(200, map[string]any{"matches": rec.matchViews()})
		}).write(w)
	}
}

func (rec *record) matchViews() []map[string]any {
	out := []map[string]any{}
	for _, m := range rec.matches {
		out = append(out, m.view())
	}
	return out
}

// view is the match as KYC6 lists it on its record.
func (m *match) view() map[string]any {
	out := map[string]any{
		"currentStatus": m.status,
		"resourceId":    m.resourceID,
		"qrCode":        m.qrCode,
		"score":         m.score,
		"match":         m.name,
		"name":          m.name,
		"datasets":      slices.Clone(m.datasets),
		"countries":     slices.Clone(m.countries),
		"version":       m.version,
		"matchVersion":  m.version,
	}
	if m.lastReview != nil {
		out["lastReviewDateTimeISO"] = iso(m.lastReview.At)
		out["lastReviewDescription"] = m.lastReview.ClientComment
	}
	return out
}

// matchByResource is the match on any of env's records whose resourceId is
// id, for the profile lookup. The caller holds a.mu.
func (a *app) matchByResource(env, kind, id string) *match {
	for _, wl := range a.monitorIn(env).worklists {
		for _, rec := range wl.records[kind] {
			for _, m := range rec.matches {
				if m.resourceID == id {
					return m
				}
			}
		}
	}
	return nil
}

// --- reviews ------------------------------------------------------------

// reviewMatches takes a batch of reviews (endpoint "reviews") or
// auto-reviews ("auto-reviews") for one record. KYC6 rules on each entry and
// answers 200 for the batch, with one result per entry echoing its
// referenceId.
//
// A review closes the open match with its qrCode, or replaces the review of
// one already closed. An auto-review is held by the record and closes the
// match with its qrCode now or when it appears; one for a qrCode that already
// holds a review is refused with MAPI-312.
func (a *app) reviewMatches(kind, endpoint string) http.HandlerFunc {
	field := "reviews"
	if endpoint == endpointAutoReviews {
		field = "autoReviews"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := decodeBody(r, &body); err != nil {
			httputilx.Error(w, 400, err.Error())
			return
		}
		entries, _ := body[field].([]any)
		if len(entries) == 0 {
			httputilx.Error(w, 400, field+" is required: a list of at least one entry")
			return
		}
		now := labclock.Now()
		a.underLock(func() reply {
			_, rec, missing := a.findRecord(tenant.Of(r.Context()), r.PathValue("worklistId"), kind, r.PathValue("recordId"))
			if missing != "" {
				return errorReply(404, missing)
			}
			results := []map[string]any{}
			refused := 0
			for _, entry := range entries {
				rv := newReview(endpoint, entry, now)
				rec.apply(rv)
				rec.reviews = append(rec.reviews, rv)
				result := map[string]any{"referenceId": rv.ReferenceID, "statusCode": rv.StatusCode}
				if rv.Error != nil {
					refused++
					errorCopy := *rv.Error
					result["error"] = &errorCopy
				}
				results = append(results, result)
			}
			if len(rec.reviews) > reviewsKept {
				// A copy, so the older reviews are not kept alive by the array.
				rec.reviews = append([]*review(nil), rec.reviews[len(rec.reviews)-reviewsKept:]...)
			}
			rec.updated = now
			hasErrors := refused > 0
			if hasErrors {
				// KYC6 answers such a batch 200 all the same; the journal
				// marks it a warning.
				activity.Refuse(r.Context(), fmt.Sprintf("%d of %d refused", refused, len(entries)))
			}
			return jsonReply(200, map[string]any{"hasErrors": hasErrors, "results": results})
		}).write(w)
	}
}

func newReview(endpoint string, entry any, at time.Time) *review {
	fields, _ := entry.(map[string]any)
	return &review{
		Endpoint:      endpoint,
		QRCode:        vendorsim.StrOrNumber(fields["qrCode"]),
		ReferenceID:   vendorsim.StrOrNumber(fields["referenceId"]),
		Status:        vendorsim.StrOrNumber(fields["status"]),
		ClientComment: vendorsim.StrOrNumber(fields["clientComment"]),
		ClientUserID:  vendorsim.StrOrNumber(fields["clientUserId"]),
		MatchVersion:  fields["matchVersion"],
		At:            at,
	}
}

// apply rules on one review entry and records the ruling on it.
func (rec *record) apply(rv *review) {
	refuse := func(code int, vendorCode, message string) {
		rv.StatusCode = code
		rv.Error = &reviewError{Code: vendorCode, Message: message}
	}
	switch {
	case rv.QRCode == "":
		refuse(400, "", "qrCode is required")
		return
	case rv.Status != matchTruePositive && rv.Status != matchFalsePositive:
		refuse(400, "", fmt.Sprintf("status %q is not one of true-positive, false-positive", rv.Status))
		return
	}
	m := rec.matchByQRCode(rv.QRCode)
	if rv.Endpoint == endpointReviews {
		if m == nil {
			refuse(404, "", "no match with qrCode "+rv.QRCode+" on this record")
			return
		}
		m.close(rv)
		rv.StatusCode = 200
		return
	}
	if rec.autoReviews[rv.QRCode] != nil || (m != nil && m.lastReview != nil) {
		refuse(409, reviewExistsCode, reviewExistsMessage)
		return
	}
	rec.autoReviews[rv.QRCode] = rv
	if m != nil {
		m.close(rv)
	}
	rv.StatusCode = 200
}

func (rec *record) matchByQRCode(qrCode string) *match {
	for _, m := range rec.matches {
		if m.qrCode == qrCode {
			return m
		}
	}
	return nil
}

func (m *match) close(rv *review) {
	m.status = rv.Status
	m.lastReview = rv
}

// --- lab control ----------------------------------------------------------

type armBody struct {
	ResourceID string   `json:"resourceId"`
	QRCode     string   `json:"qrCode"`
	Name       string   `json:"name"`
	Score      *float64 `json:"score"`
	// Datasets and Countries are nil when not given: a re-arm keeps them.
	// [] clears them.
	Datasets  []string `json:"datasets"`
	Countries []string `json:"countries"`
}

// armMatch is POST /sim/kyc6/worklists/{worklistId}/{kind}/{recordId}/matches:
// KYC6's re-screening found this match on the record. A resourceId the
// record already has a match for is that profile changing: the match takes
// the fields given, its version moves on and it is open again.
func (a *app) armMatch(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		env, ok := a.sim.OneEnv(w, r, "a match is put on a record")
		if !ok {
			return
		}
		var body armBody
		if err := httputilx.ReadJSON(r, &body); err != nil && !errors.Is(err, io.EOF) {
			httputilx.Error(w, 400, err.Error())
			return
		}
		if body.Score != nil && (*body.Score < 0 || *body.Score > 100) {
			httputilx.Error(w, 400, "score is from 0 to 100")
			return
		}
		for _, dataset := range body.Datasets {
			if strings.TrimSpace(dataset) == "" {
				httputilx.Error(w, 400, "a dataset cannot be empty")
				return
			}
		}
		worklistID, recordID := r.PathValue("worklistId"), r.PathValue("recordId")
		now := labclock.Now()
		a.underLock(func() reply {
			_, rec, missing := a.findRecord(env, worklistID, kind, recordID)
			if missing != "" {
				return errorReply(404, missing+" in environment "+env)
			}
			m, created, conflict := a.monitorIn(env).arm(rec, body, now)
			if conflict != "" {
				return errorReply(409, conflict+" in environment "+env)
			}
			code := 200
			if created {
				code = 201
			}
			log.Printf("kyc6: match %s (version %d) on %s %s, worklist %s, environment %s", m.resourceID, m.version, singular(kind), recordID, worklistID, env)
			return jsonReply(code, map[string]any{
				"env": env, "worklistId": worklistID, "kind": kind, "recordId": recordID, "match": m.view(),
			})
		}).write(w)
	}
}

// arm puts body's match on rec, or moves the one it names on a version, and
// reports whether it is new. A held auto-review for its qrCode closes it.
// It changes nothing and says why when body gives a resourceId or qrCode
// another match in the environment has.
func (m *monitorEnv) arm(rec *record, body armBody, now time.Time) (*match, bool, string) {
	var found *match
	if body.ResourceID != "" {
		for _, existing := range rec.matches {
			if existing.resourceID == body.ResourceID {
				found = existing
				break
			}
		}
	}
	if conflict := m.conflict(found, body); conflict != "" {
		return nil, false, conflict
	}
	created := found == nil
	if created {
		n := m.nextFreeNumber()
		found = &match{
			id:         fmt.Sprintf("sim-om-match-%d", n),
			resourceID: cmp.Or(body.ResourceID, fmt.Sprintf("sim-om-%d", n)),
			qrCode:     cmp.Or(body.QRCode, fmt.Sprintf("sim-qr-%d", n)),
			name:       cmp.Or(body.Name, rec.name),
			score:      defaultMatchScore,
			datasets:   defaultMatchDatasets,
			countries:  []string{},
			created:    now,
		}
		rec.matches = append(rec.matches, found)
	} else {
		if body.QRCode != "" {
			found.qrCode = body.QRCode
		}
		if body.Name != "" {
			found.name = body.Name
		}
	}
	if body.Score != nil {
		found.score = *body.Score
	}
	if body.Datasets != nil {
		found.datasets = body.Datasets
	}
	if body.Countries != nil {
		found.countries = body.Countries
	}
	found.version++
	found.status = matchOpen
	found.lastReview = nil
	if held := rec.autoReviews[found.qrCode]; held != nil {
		found.close(held)
	}
	rec.updated = now
	return found, created, ""
}

// conflict is why body cannot arm found (nil for a new match), or "": a
// qrCode it gives that a different match in the environment has. A review
// finds its match by qrCode, so a second match with it could never be
// reviewed. A resourceId names a KYC6 profile, which can match several
// records at the real vendor; the profile lookup describes the same
// profile whichever of them it finds.
func (m *monitorEnv) conflict(found *match, body armBody) string {
	for _, existing := range m.matches() {
		if existing == found {
			continue
		}
		if body.QRCode != "" && existing.qrCode == body.QRCode {
			return "qrCode " + body.QRCode + " is already match " + existing.resourceID + "'s"
		}
	}
	return ""
}

// nextFreeNumber moves the environment's match count on to the next n
// whose sim-om-<n> and sim-qr-<n> no match in the environment has: a test
// may have given either to a match it armed.
func (m *monitorEnv) nextFreeNumber() int {
	for {
		m.matchesArmed++
		n := m.matchesArmed
		if !m.used(fmt.Sprintf("sim-om-%d", n)) && !m.used(fmt.Sprintf("sim-qr-%d", n)) {
			return n
		}
	}
}

// used reports whether a match in the environment has value as its
// resourceId or its qrCode.
func (m *monitorEnv) used(value string) bool {
	for _, existing := range m.matches() {
		if existing.resourceID == value || existing.qrCode == value {
			return true
		}
	}
	return false
}

// matches is every match on every record in the environment.
func (m *monitorEnv) matches() []*match {
	var out []*match
	for _, wl := range m.worklists {
		for _, kind := range recordKinds {
			for _, rec := range wl.records[kind] {
				out = append(out, rec.matches...)
			}
		}
	}
	return out
}

// readRecord is GET /sim/kyc6/worklists/{worklistId}/{kind}/{recordId}: the
// record as KYC6 lists it, its matches, the auto-reviews it holds and every
// review entry sent for it, with the answer each was given.
func (a *app) readRecord(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		env, ok := a.sim.OneEnv(w, r, "a record is read")
		if !ok {
			return
		}
		worklistID, recordID := r.PathValue("worklistId"), r.PathValue("recordId")
		a.underLock(func() reply {
			_, rec, missing := a.findRecord(env, worklistID, kind, recordID)
			if missing != "" {
				return errorReply(404, missing+" in environment "+env)
			}
			out := rec.labView()
			out["env"], out["worklistId"], out["kind"] = env, worklistID, kind
			return jsonReply(200, out)
		}).write(w)
	}
}

// labView is the record as readRecord shows it. Its reviews are copies of
// the record's.
func (rec *record) labView() map[string]any {
	out := rec.view(rec.tally())
	out["matches"] = rec.matchViews()
	autoReviews := []review{}
	for _, rv := range rec.autoReviews {
		autoReviews = append(autoReviews, rv.copy())
	}
	sort.Slice(autoReviews, func(i, j int) bool {
		if !autoReviews[i].At.Equal(autoReviews[j].At) {
			return autoReviews[i].At.Before(autoReviews[j].At)
		}
		return autoReviews[i].QRCode < autoReviews[j].QRCode
	})
	out["autoReviews"] = autoReviews
	reviews := make([]review, 0, len(rec.reviews))
	for _, rv := range rec.reviews {
		reviews = append(reviews, rv.copy())
	}
	out["reviews"] = reviews
	return out
}

// copy is the review with its own error. Its matchVersion is the decoded
// JSON value it was sent with, which nothing changes.
func (rv *review) copy() review {
	out := *rv
	if rv.Error != nil {
		errorCopy := *rv.Error
		out.Error = &errorCopy
	}
	return out
}

// readWorklists is GET /sim/kyc6/worklists: every worklist in the
// environment the call names, or in every one for *, with its records as
// GET /sim/kyc6/worklists/{worklistId}/{kind}/{recordId} shows each.
func (a *app) readWorklists(w http.ResponseWriter, r *http.Request) {
	scope := tenant.FromRequest(r)
	if !a.sim.Known(scope) {
		httputilx.Error(w, 404, "no environment "+scope+" at kyc6")
		return
	}
	a.underLock(func() reply {
		return jsonReply(200, map[string]any{"env": scope, "worklists": a.worklistViews(scope)})
	}).write(w)
}

// worklistViews is every worklist in scope as readWorklists shows it. The
// caller holds a.mu.
func (a *app) worklistViews(scope string) []map[string]any {
	out := []map[string]any{}
	envs := make([]string, 0, len(a.monitoring))
	for env := range a.monitoring {
		envs = append(envs, env)
	}
	sort.Strings(envs)
	for _, env := range envs {
		if !tenant.InScope(scope, env) {
			continue
		}
		worklists := a.monitoring[env].worklists
		ids := make([]string, 0, len(worklists))
		for id := range worklists {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			wl := worklists[id]
			view := wl.view()
			view["env"] = env
			for _, kind := range recordKinds {
				records := []map[string]any{}
				for _, rec := range sortedRecords(wl.records[kind]) {
					records = append(records, rec.labView())
				}
				view[kind] = records
			}
			out = append(out, view)
		}
	}
	return out
}

// decodeBody reads a vendor body into v: any field, but exactly one JSON
// value of the right shape. An empty body, one that does not decode, or
// one with anything after the value is the caller's 400, as at KYC6. A
// number it keeps as sent (json.Number), so a review's numeric referenceId
// or qrCode comes back with every digit, however large.
func decodeBody(r *http.Request, v any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	if err := decoder.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("a JSON body is required")
		}
		return err
	}
	if err := decoder.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		return errors.New("the body must hold one JSON value and nothing after it")
	}
	return nil
}

func iso(t time.Time) string { return t.UTC().Format(time.RFC3339) }
