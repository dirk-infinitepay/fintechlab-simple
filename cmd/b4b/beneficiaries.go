package main

import (
	"errors"
	"log"
	"net/http"

	"github.com/webduvet/fintechlab-simple/internal/b4b"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
)

// Beneficiary registration, the screening callback, and the gates a payment
// clears before anything is forwarded.
//
// Auto-vivification stays for unknown ids: a caller that just names an id
// still works and a demo needs no registration step first. It is the one
// place this mock is knowingly more forgiving than the real API, which
// answers 404 -- so an id that is wrong reads back as a plausible payee
// here and as nothing at all in production. Register your payees.

type registerBeneficiaryReq struct {
	// CompanyID is the boarded company the payee belongs to. Optional, and
	// checked against the directory when present: a payee registered under
	// a company that was never boarded is a mistake worth catching at
	// registration rather than at the first payout.
	CompanyID            string `json:"company_id"`
	ExternalRef          string `json:"external_ref"`
	AccountName          string `json:"account_name"`
	AccountNumber        string `json:"account_number"`
	FinancialInstitution string `json:"financial_institution"`
	Country              string `json:"country"`
	CallbackURL          string `json:"callback_url"`
	SanctionsStatus      string `json:"sanctions_status"`
}

func (a *app) registerBeneficiary(w http.ResponseWriter, r *http.Request) {
	var req registerBeneficiaryReq
	if _, err := httputilx.ReadJSONExtras(r, &req); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	if req.CallbackURL != "" {
		if err := b4b.ValidateCallbackURL(a.list, req.CallbackURL); err != nil {
			httputilx.Error(w, 400, "callback_url rejected: "+err.Error())
			return
		}
	}
	companyID := req.CompanyID
	if companyID != "" {
		c, err := a.dir.Company(companyID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		companyID = c.ID
	}
	var ben *b4b.Beneficiary
	var err error
	started := a.engine.Gate().Begin(func() {
		ben, err = a.beneficiaries.Register(b4b.RegisterParams{
			CompanyID:            companyID,
			ExternalRef:          req.ExternalRef,
			AccountName:          req.AccountName,
			AccountNumber:        req.AccountNumber,
			FinancialInstitution: req.FinancialInstitution,
			Country:              req.Country,
			CallbackURL:          req.CallbackURL,
			SanctionsStatus:      req.SanctionsStatus,
		})
	})
	if err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	// The screening result is reported through the callback, not only in
	// this response: a client must be built to receive it asynchronously,
	// because under continuous screening it can change again later.
	a.later(started, func(started uint64) { a.deliverSanctionsCallback(started, ben) })
	httputilx.WriteJSON(w, 201, ben)
}

// getBeneficiary returns a registered beneficiary, or auto-vivifies a
// deterministic synthetic one for an unknown id, so this always returns
// 200 and never 404. It is also the documented recovery path when a
// sanctions callback is missed: read the current status rather than trying
// to replay the event.
func (a *app) getBeneficiary(w http.ResponseWriter, r *http.Request) {
	httputilx.WriteJSON(w, 200, a.beneficiaries.Get(r.PathValue("id")))
}

// setSanctions moves a beneficiary's consolidated status. Lab-only: real
// screening decides this, and it can change at any time under continuous
// screening. Exposing it is what lets a scenario exercise a beneficiary
// going from pass to fail between two payments.
func (a *app) setSanctions(w http.ResponseWriter, r *http.Request) {
	var req setSanctionsReq
	if _, err := httputilx.ReadJSONExtras(r, &req); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	var ben *b4b.Beneficiary
	var changed bool
	var err error
	started := a.engine.Gate().Begin(func() {
		ben, changed, err = a.beneficiaries.SetSanctions(r.PathValue("id"), req.SanctionsStatus)
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if changed {
		a.later(started, func(started uint64) { a.deliverSanctionsCallback(started, ben) })
	}
	httputilx.WriteJSON(w, 200, ben)
}

type setStatusReq struct {
	Status string `json:"status"`
}

// setBeneficiaryStatus moves a beneficiary between active and disabled.
// Lab-only, and worth having separately from sanctions: a disabled payee
// screens clean and still cannot be paid, so a client that gates only on
// sanctions_status passes every check it makes and gets a 422 anyway.
func (a *app) setBeneficiaryStatus(w http.ResponseWriter, r *http.Request) {
	var req setStatusReq
	if _, err := httputilx.ReadJSONExtras(r, &req); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	var ben *b4b.Beneficiary
	var changed bool
	var err error
	started := a.engine.Gate().Begin(func() {
		ben, changed, err = a.beneficiaries.SetStatus(r.PathValue("id"), req.Status)
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if changed {
		a.later(started, func(started uint64) { a.deliverSanctionsCallback(started, ben) })
	}
	httputilx.WriteJSON(w, 200, ben)
}

// sanctionsCallback is the beneficiary callback body: B4B's own id, the
// caller's reference, the screening status, and the active/disabled status
// with the moment it was disabled.
//
// Both statuses are on it because both stop a payment, and a client told
// only about one of them has no way to explain the refusal it gets.
type sanctionsCallback struct {
	ID              string `json:"id"`
	ExternalRef     string `json:"external_ref,omitempty"`
	SanctionsStatus string `json:"sanctions_status"`
	Status          string `json:"status"`
	DisabledAt      string `json:"disabled_at,omitempty"`
}

// deliverSanctionsCallback POSTs the status change to the payee's own
// callback_url if it has one, else to the client-account endpoint. Retried
// like every other callback here: the real vendor retries this for about a
// day, and a subscriber that answers 500 once should not lose a screening
// result permanently. The current status is always readable from
// GET /oversight/v1/beneficiaries/{id}, which is the documented recovery
// path for a missed callback.
func (a *app) deliverSanctionsCallback(started uint64, ben *b4b.Beneficiary) {
	a.deliverCallback(started, "beneficiary "+ben.ID, a.callbackTarget(ben.CallbackURL), sanctionsCallback{
		ID:              ben.ID,
		ExternalRef:     ben.ExternalRef,
		SanctionsStatus: ben.SanctionsStatus,
		Status:          ben.Status,
		DisabledAt:      ben.DisabledAt,
	})
}

// gateBeneficiary applies every check a payment has to clear before
// anything is forwarded:
//
//   - the beneficiary's consolidated sanctions status must be "pass";
//   - the beneficiary must not be disabled;
//   - the payment's creditor fields must match the beneficiary they name;
//   - optionally, the paying company must be the one the payee belongs to.
//
// All return 422 with the reason, and nothing reaches Banking Circle.
// Creditor fields are a consistency check, not a recipient override --
// accepting a mismatch silently is how money reaches the wrong account.
func (a *app) gateBeneficiary(w http.ResponseWriter, req createPaymentReq) *b4b.Beneficiary {
	id := req.BeneficiaryID
	ben := a.beneficiaries.Get(id)
	if ben.SanctionsStatus != b4b.SanctionsPass {
		httputilx.Error(w, 422, "beneficiary "+id+" has sanctions_status "+ben.SanctionsStatus+"; only \"pass\" permits payments")
		return nil
	}
	if ben.Status == b4b.StatusDisabled {
		httputilx.Error(w, 422, "beneficiary "+id+" is disabled (since "+ben.DisabledAt+"); a disabled beneficiary cannot be paid even though it screens clean")
		return nil
	}
	if err := ben.CheckCreditor(req.CreditorAccount.Account, req.CreditorAccount.FinancialInstitution, req.CreditorName); err != nil {
		if errors.Is(err, b4b.ErrCreditorMismatch) {
			httputilx.Error(w, 422, err.Error())
			return nil
		}
		httputilx.Error(w, 400, err.Error())
		return nil
	}
	if err := ben.CheckCompany(req.CompanyID); err != nil {
		if !a.enforceCompany {
			// Off by default, because whether the real API enforces this
			// is not known to this lab -- and it decides whether a
			// platform paying its merchants' payees as itself works at all.
			// Logged every time so the question stays visible.
			log.Printf("b4b: %v -- allowed, because B4B_ENFORCE_BENEFICIARY_COMPANY is off; confirm with the vendor whether a real payment here would be refused", err)
		} else {
			httputilx.Error(w, 422, err.Error())
			return nil
		}
	}
	return ben
}
