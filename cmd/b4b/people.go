package main

import (
	"log"
	"net/http"

	"github.com/webduvet/fintechlab-simple/internal/b4b"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
)

// People: natural persons and shareholding companies, both through the same
// endpoint, and the screening callback they come back on.

func (a *app) createPerson(w http.ResponseWriter, r *http.Request) {
	var p b4b.PersonParams
	extra, err := httputilx.ReadJSONExtras(r, &p)
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	p.Extra = extra
	env := envOf(r)
	var person *b4b.Person
	started := a.engine.GateFor(env).Begin(func() { person, err = a.dir.In(env).AddPerson(r.PathValue("id"), p) })
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if person.ExternalRef != "" {
		// Said out loud because the real API documents external_ref on a
		// person as not saved, and the callback carries none. A client
		// that sends one and plans to correlate on it has already made the
		// mistake by this point; the log is where it can still find out.
		log.Printf("b4b: person %s created with external_ref %q -- it is echoed but is not a lookup key, and the screening callback carries only the id",
			person.ID, person.ExternalRef)
	}
	// Screening is reported asynchronously, not in this response. A client
	// has to be built to receive it: under continuous screening the answer
	// can change again long after boarding finished.
	a.later(env, started, func(started uint64) { a.deliverPersonCallback(env, started, person) })
	httputilx.WriteJSON(w, 201, person)
}

func (a *app) listPeople(w http.ResponseWriter, r *http.Request) {
	list, err := a.dir.In(envOf(r)).People(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	httputilx.WriteJSON(w, 200, map[string]any{"people": list})
}

func (a *app) getPerson(w http.ResponseWriter, r *http.Request) {
	person, err := a.dir.In(envOf(r)).Person(r.PathValue("personID"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	httputilx.WriteJSON(w, 200, person)
}

// setPersonSanctions moves a person's screening statuses independently.
// Lab-only: real screening decides these, and a PEP flag moving without a
// sanctions hit is exactly the case a client that reads only
// sanctions_status gets wrong.
func (a *app) setPersonSanctions(w http.ResponseWriter, r *http.Request) {
	var req setSanctionsReq
	if _, err := httputilx.ReadJSONExtras(r, &req); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	env := envOf(r)
	var person *b4b.Person
	var changed bool
	var err error
	started := a.engine.GateFor(env).Begin(func() {
		person, changed, err = a.dir.In(env).SetPersonSanctions(r.PathValue("id"), req.SanctionsStatus, req.PepSanctionsStatus)
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if changed {
		a.later(env, started, func(started uint64) { a.deliverPersonCallback(env, started, person) })
	}
	httputilx.WriteJSON(w, 200, person)
}

// personCallback is the documented person callback body: the id B4B minted
// and the two screening statuses. Nothing else.
//
// No external_ref, and no entity type -- deliberately, because the real one
// has neither. A client that attached its own reference to a person, or
// that keeps natural persons and shareholding companies in two tables with
// two id sequences, cannot correlate this. That is not an omission in this
// mock; it is the constraint, and the only correct answer is to persist the
// id this endpoint returned at create time and resolve on that.
type personCallback struct {
	ID                 string `json:"id"`
	SanctionsStatus    string `json:"sanctions_status"`
	PepSanctionsStatus string `json:"pep_sanctions_status"`
}

// deliverPersonCallback calls env back about one of its people: always at
// the environment's client-account endpoint, since a person has no
// callback_url of its own.
func (a *app) deliverPersonCallback(env string, started uint64, p *b4b.Person) {
	a.deliverCallback(env, started, "person "+p.ID, a.callbackTarget(env, ""), personCallback{
		ID:                 p.ID,
		SanctionsStatus:    p.SanctionsStatus,
		PepSanctionsStatus: p.PepSanctionsStatus,
	})
}
