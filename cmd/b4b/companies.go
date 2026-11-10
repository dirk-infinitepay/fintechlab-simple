package main

import (
	"log"
	"net/http"

	"github.com/webduvet/fintechlab-simple/internal/b4b"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
)

// The boarding chain: company, addresses, extended profile, vIBANs.
//
// Bodies are read leniently (httputilx.ReadJSONExtras): a field this mock
// has no column for is kept and echoed back under "extra" rather than
// refused. The published schema for these endpoints is not fully known to
// this lab, and answering 400 to a field the real vendor accepts would
// teach a client to remove it -- which is a worse outcome than not
// modelling it.
//
// Every route acts in the caller's environment (envs.go): a company another
// environment boarded reads as unknown, and an external_ref is taken only
// within the environment that used it.

func (a *app) createCompany(w http.ResponseWriter, r *http.Request) {
	var p b4b.CompanyParams
	extra, err := httputilx.ReadJSONExtras(r, &p)
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	p.Extra = extra
	c, err := a.dir.In(envOf(r)).CreateCompany(p)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	log.Printf("b4b: company %s created legal_name=%q external_ref=%q waivers=company:%t/person:%t",
		c.ID, c.LegalName, c.ExternalRef, c.CompanyDocumentsWaived, c.PersonDocumentsWaived)
	httputilx.WriteJSON(w, 201, c)
}

func (a *app) listCompanies(w http.ResponseWriter, r *http.Request) {
	httputilx.WriteJSON(w, 200, map[string]any{"companies": a.dir.In(envOf(r)).Companies()})
}

func (a *app) getCompany(w http.ResponseWriter, r *http.Request) {
	c, err := a.dir.In(envOf(r)).Company(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	httputilx.WriteJSON(w, 200, c)
}

func (a *app) createAddress(w http.ResponseWriter, r *http.Request) {
	var p b4b.AddressParams
	extra, err := httputilx.ReadJSONExtras(r, &p)
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	p.Extra = extra
	addr, err := a.dir.In(envOf(r)).AddAddress(r.PathValue("id"), p)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	httputilx.WriteJSON(w, 201, addr)
}

func (a *app) listAddresses(w http.ResponseWriter, r *http.Request) {
	list, err := a.dir.In(envOf(r)).Addresses(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	httputilx.WriteJSON(w, 200, map[string]any{"addresses": list})
}

// createExtended creates a company's regulatory profile -- once.
//
// The second attempt is a 422 naming the profile that already exists. This
// is the single most valuable refusal in this file: a boarding chain that
// runs company -> addresses -> people -> extended -> beneficiaries and is
// re-run after the last step fails will re-post this, and a client that
// treats the resulting 422 as a hard error sticks on this step forever.
// The fix is to read GET .../extended first, which is why that read exists.
func (a *app) createExtended(w http.ResponseWriter, r *http.Request) {
	var p b4b.ExtendedParams
	extra, err := httputilx.ReadJSONExtras(r, &p)
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	p.Extra = extra
	prof, err := a.dir.In(envOf(r)).CreateExtended(r.PathValue("id"), p)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if len(prof.NaceCodes) == 0 {
		// Not refused: the guide calls nace_codes required and the schema
		// does not, and this lab cannot see which is current. Logged so
		// the disagreement is visible to whoever is looking, rather than
		// being decided silently in either direction.
		log.Printf("b4b: extended profile %s created with no nace_codes -- the vendor guide calls this required, the schema does not; confirm before relying on it", prof.ID)
	}
	httputilx.WriteJSON(w, 201, prof)
}

func (a *app) getExtended(w http.ResponseWriter, r *http.Request) {
	prof, err := a.dir.In(envOf(r)).Extended(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// The id-bearing form of this route is served by the same handler:
	// there is one profile per company, so an id that names a different
	// one is a caller error worth saying out loud rather than ignoring.
	if want := r.PathValue("extendedID"); want != "" && want != prof.ID {
		httputilx.Error(w, 404, "extended profile "+want+" does not belong to company "+prof.CompanyID)
		return
	}
	httputilx.WriteJSON(w, 200, prof)
}

type createVibanReq struct {
	Currency string `json:"currency"`
}

// createViban issues a company a vIBAN per currency. Re-registering an
// existing currency returns the same account rather than a second one:
// this is provisioning, not an append-only log, and a client that retries
// should not end up with two accounts it has to choose between.
func (a *app) createViban(w http.ResponseWriter, r *http.Request) {
	var req createVibanReq
	if _, err := httputilx.ReadJSONExtras(r, &req); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	v, err := a.dir.In(envOf(r)).AddViban(r.PathValue("id"), req.Currency)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	httputilx.WriteJSON(w, 201, v)
}

func (a *app) listVibans(w http.ResponseWriter, r *http.Request) {
	list, err := a.dir.In(envOf(r)).Vibans(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	httputilx.WriteJSON(w, 200, map[string]any{"vibans": list})
}

type setSanctionsReq struct {
	SanctionsStatus    string `json:"sanctions_status"`
	PepSanctionsStatus string `json:"pep_sanctions_status"`
}

// setCompanySanctions moves a company's KYB screening status. Lab-only.
// No callback fires: the real API reports screening through the person and
// beneficiary callbacks, not a company one, and inventing a company
// callback here would let a client be built on something that never
// arrives in production.
func (a *app) setCompanySanctions(w http.ResponseWriter, r *http.Request) {
	var req setSanctionsReq
	if _, err := httputilx.ReadJSONExtras(r, &req); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	c, _, err := a.dir.In(envOf(r)).SetCompanySanctions(r.PathValue("id"), req.SanctionsStatus)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	httputilx.WriteJSON(w, 200, c)
}
