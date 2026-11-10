package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/b4b"
	"github.com/webduvet/fintechlab-simple/internal/bankingcircle"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/money"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// The payout half: create a payment, walk it through the regulatory
// lifecycle, call back on every phase change, and bridge into Banking
// Circle once approved. A payment is its caller's environment's (envs.go):
// only that environment reads it, its callbacks go where that
// environment's go, and the bridge call says it is theirs.

// The B4B Oversight wire shapes, byte-exact: amounts are JSON numbers,
// not strings, and account references carry an optional country.
type amountWire struct {
	Amount   json.Number `json:"amount"`
	Currency string      `json:"currency"`
}

type accountRefWire struct {
	Account              string `json:"account"`
	FinancialInstitution string `json:"financialInstitution,omitempty"`
	Country              string `json:"country,omitempty"`
}

// remittanceWire is the reference shown to the beneficiary, and the field
// transaction monitoring reads. A payment without it still goes through --
// it is not required -- but a client that omits it has given the payee
// nothing to reconcile against.
type remittanceWire struct {
	Line1 string `json:"line1,omitempty"`
}

type createPaymentReq struct {
	ExternalRef            string          `json:"external_ref"`
	BeneficiaryID          string          `json:"beneficiary_id"`
	CompanyID              string          `json:"company_id"`
	CallbackURL            string          `json:"callback_url"`
	SCAApplied             bool            `json:"sca_applied"`
	Amount                 amountWire      `json:"amount"`
	CurrencyOfTransfer     string          `json:"currencyOfTransfer"`
	DebtorViban            accountRefWire  `json:"debtorViban"`
	DebtorAccount          *accountRefWire `json:"debtorAccount,omitempty"`
	CreditorAccount        accountRefWire  `json:"creditorAccount"`
	CreditorName           string          `json:"creditorName"`
	ChargeBearer           string          `json:"chargeBearer"`
	RequestedExecutionDate string          `json:"requestedExecutionDate"`
	RemittanceInformation  *remittanceWire `json:"remittanceInformation,omitempty"`
}

type paymentPayload struct {
	Amount                amountWire      `json:"amount"`
	CurrencyOfTransfer    string          `json:"currencyOfTransfer"`
	DebtorViban           accountRefWire  `json:"debtorViban"`
	CreditorAccount       accountRefWire  `json:"creditorAccount"`
	CreditorName          string          `json:"creditorName"`
	RemittanceInformation *remittanceWire `json:"remittanceInformation,omitempty"`
	DebtorReference       string          `json:"debtorReference,omitempty"`
}

type createPaymentResp struct {
	ID      string         `json:"id"`
	Status  string         `json:"status"`
	Payload paymentPayload `json:"payload"`
}

func payloadOf(p *b4b.Payment) paymentPayload {
	pl := paymentPayload{
		Amount:             amountWire{Amount: json.Number(p.Amount.Amount), Currency: p.Amount.Currency},
		CurrencyOfTransfer: p.CurrencyOfTransfer,
		DebtorViban:        accountRefWire(p.DebtorViban),
		CreditorAccount:    accountRefWire(p.CreditorAccount),
		CreditorName:       p.CreditorName,
		DebtorReference:    p.DebtorReference,
	}
	if p.RemittanceInformation != "" {
		pl.RemittanceInformation = &remittanceWire{Line1: p.RemittanceInformation}
	}
	return pl
}

// getPayment implements GET /oversight/v1/payments/{id}.
//
// Oversight's payment callbacks are unsigned, unordered, may be delivered
// more than once, and have no replay endpoint. Reading the current state
// is the documented fallback for a client that missed one, and a
// simulator without it forces every client to be written as if callbacks
// were reliable -- which they are not.
func (a *app) getPayment(w http.ResponseWriter, r *http.Request) {
	p, err := a.engine.GetIn(envOf(r), r.PathValue("id"))
	if err != nil {
		httputilx.Error(w, 404, "payment not found")
		return
	}
	httputilx.WriteJSON(w, 200, map[string]any{
		"id":           p.ID,
		"status":       string(p.State),
		"external_ref": p.ExternalRef,
		"payload":      payloadOf(p),
	})
}

func (a *app) createPayment(w http.ResponseWriter, r *http.Request) {
	var req createPaymentReq
	if _, err := httputilx.ReadJSONExtras(r, &req); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	if req.BeneficiaryID == "" {
		httputilx.Error(w, 400, "beneficiary_id required")
		return
	}
	// callback_url is a lab affordance, not a wire field: the real API
	// posts to one endpoint configured for the client account. Supplied,
	// it must pass the allowlist (or the environment's webhook_hosts);
	// omitted, this payment falls back to the environment's endpoint --
	// B4B_CALLBACK_URL for the default -- like every other callback here.
	env := envOf(r)
	if req.CallbackURL != "" {
		if err := a.allowed(env, req.CallbackURL); err != nil {
			httputilx.Error(w, 400, "callback_url rejected: "+err.Error())
			return
		}
	} else if a.callbackFor(env) == "" {
		log.Printf("b4b: payment for beneficiary %s has %s; its lifecycle callbacks will go nowhere",
			req.BeneficiaryID, noCallback(env))
	}
	if _, err := money.Parse(string(req.Amount.Amount)); err != nil {
		httputilx.Error(w, 400, "amount.amount must be a decimal number")
		return
	}
	if req.CurrencyOfTransfer == "" {
		httputilx.Error(w, 400, "currencyOfTransfer required")
		return
	}
	if req.DebtorViban.Account == "" {
		httputilx.Error(w, 400, "debtorViban.account required")
		return
	}
	if req.CreditorAccount.Account == "" {
		httputilx.Error(w, 400, "creditorAccount.account required")
		return
	}
	// SHA, BEN or OUR -- the ISO 20022 values the vendor documents. Not
	// DEBT/CRED, which appear in at least one client's own type and in no
	// specification: a mock that accepted them would confirm a mistake
	// rather than catch it.
	switch req.ChargeBearer {
	case "SHA", "BEN", "OUR":
	default:
		httputilx.Error(w, 400, "chargeBearer must be one of SHA, BEN, OUR")
		return
	}

	// The gates. All answer 422 and forward nothing: creditor fields check
	// the beneficiary, they do not override it, and accepting a mismatch
	// silently is how money reaches the wrong account.
	ben := a.gateBeneficiary(w, env, req)
	if ben == nil {
		return
	}

	remittance := ""
	if req.RemittanceInformation != nil {
		remittance = req.RemittanceInformation.Line1
	}
	p := &b4b.Payment{
		ID:                     paymentID(),
		Env:                    env,
		ExternalRef:            req.ExternalRef,
		BeneficiaryID:          req.BeneficiaryID,
		CompanyID:              req.CompanyID,
		CallbackURL:            a.callbackTarget(env, req.CallbackURL),
		SCAApplied:             req.SCAApplied,
		Amount:                 b4b.Amount{Amount: string(req.Amount.Amount), Currency: req.Amount.Currency},
		CurrencyOfTransfer:     req.CurrencyOfTransfer,
		DebtorViban:            b4b.AccountRef(req.DebtorViban),
		CreditorAccount:        b4b.AccountRef(req.CreditorAccount),
		CreditorName:           req.CreditorName,
		ChargeBearer:           req.ChargeBearer,
		RequestedExecutionDate: req.RequestedExecutionDate,
		RemittanceInformation:  remittance,
		DebtorReference:        "B4BREF" + shortID(),
	}
	if req.DebtorAccount != nil {
		ref := b4b.AccountRef(*req.DebtorAccount)
		p.DebtorAccount = &ref
	}
	// The payment.create traps see it first (traps.go): B4B_FORCE_FAILURE_
	// BENEFICIARY_IDS among them, as standing traps.
	a.accept(p, ben)

	httputilx.WriteJSON(w, 202, createPaymentResp{
		ID:      p.ID,
		Status:  string(b4b.StateAccepted),
		Payload: payloadOf(p),
	})
}

// onTransition fires on every B4B payout lifecycle transition. It never
// blocks the engine: the Banking Circle bridge call (StateTMApproved only)
// and webhook delivery (its own retry/backoff loop) both run in their own
// goroutine, so a down or slow callback_url/Banking Circle never stalls
// payment processing.
//
// It also persists. A payment's state is the one piece of this service's
// memory that changes without anybody asking it to, so it is the one that
// most needs writing down before the process can die.
//
// started is the epoch the payment's work began in; the callback and the
// bridge call carry it, so a reset after this transition drops them.
func (a *app) onTransition(p *b4b.Payment, started uint64) {
	a.persist()
	if p.State == b4b.StateTMApproved {
		a.later(p.Env, started, func(started uint64) { a.approveAndBridge(p, started) })
		return
	}
	a.later(p.Env, started, func(started uint64) { a.deliverPaymentCallback(started, p, nil) })
}

// approveAndBridge generates this payment's Banking Circle paymentId and
// calls Banking Circle's internal bridge endpoint (docs/
// ARCHITECTURE-vendor-corrections.md section 3's B4B<->BC bridge, amended
// by Addendum section B to carry externalRef). The bridge call is
// best-effort and non-fatal to B4B's own webhook: on success or failure it
// still fires the B4BTMApproved webhook, with
// banking_circle_api_response.paymentId always set on this one transition.
//
// The bridge call goes through the gate with the payment's epoch: after a
// reset the payment no longer exists, and neither the call nor the
// callback after it is made.
func (a *app) approveAndBridge(p *b4b.Payment, started uint64) {
	bcPaymentID := "bcp_" + shortID()
	bcResp := map[string]any{"paymentId": bcPaymentID}
	if !a.engine.GateFor(p.Env).Do(started, func() { a.bridge(p, bcPaymentID, bcResp) }) {
		log.Printf("b4b: banking circle bridge call for payment %s dropped: b4b was reset", p.ID)
		return
	}
	a.deliverPaymentCallback(started, p, bcResp)
}

// bridge makes the bridge call and notes in bcResp what went wrong, if
// anything did. A call between sims says whose it is (sim-control.md §8):
// a payout of any environment but the default carries it in the
// Lab-Environment header, and Banking Circle debits that environment's
// safeguarding account.
func (a *app) bridge(p *b4b.Payment, bcPaymentID string, bcResp map[string]any) {
	reqBody, err := json.Marshal(a.bridgeBody(p, bcPaymentID))
	if err != nil {
		log.Printf("b4b: marshal banking circle bridge request for payment %s: %v", p.ID, err)
		bcResp["error"] = err.Error()
		return
	}
	req, err := http.NewRequest(http.MethodPost, a.bcURL+"/internal/payments", bytes.NewReader(reqBody))
	if err != nil {
		log.Printf("b4b: build banking circle bridge request for payment %s: %v", p.ID, err)
		bcResp["error"] = err.Error()
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if env := tenant.Normalize(p.Env); env != tenant.Default {
		req.Header.Set(tenant.Header, env)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		log.Printf("b4b: banking circle bridge call failed for payment %s: %v; continuing, own webhook still fires", p.ID, err)
		bcResp["error"] = err.Error()
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		log.Printf("b4b: banking circle bridge call for payment %s returned %s; continuing, own webhook still fires", p.ID, resp.Status)
		bcResp["error"] = fmt.Sprintf("banking circle returned %s", resp.Status)
	} else {
		log.Printf("b4b: banking circle bridge call for payment %s ok, bcPaymentId=%s", p.ID, bcPaymentID)
	}
}

// deliverPaymentCallback POSTs the lifecycle callback for one transition.
func (a *app) deliverPaymentCallback(started uint64, p *b4b.Payment, bcResp map[string]any) {
	body := map[string]any{
		"id":      p.ID,
		"status":  string(p.State),
		"payload": payloadOf(p),
	}
	if bcResp != nil {
		body["banking_circle_api_response"] = bcResp
	}
	a.deliverMarked(p.Env, started, fmt.Sprintf("payment %s status=%s", p.ID, p.State), p.CallbackURL, body, trapMark(p))
}

// summarizePayment writes the one line an operator reads in the console's
// payouts panel.
//
// It reads the request the platform sent rather than anything this service
// derived from it, so a call that was refused before a Payment ever
// existed still says what was asked for -- which is the case where an
// operator most needs to know.
func summarizePayment(c *activity.Call) (string, map[string]string) {
	amount := c.JSONField("amount", "amount")
	currency := c.JSONField("amount", "currency")
	beneficiary := c.JSONField("beneficiary_id")
	externalRef := c.JSONField("external_ref")

	summary := fmt.Sprintf("payout %s %s to %s", amount, currency, beneficiary)
	if creditor := c.JSONField("creditorName"); creditor != "" {
		summary += " (" + creditor + ")"
	}

	detail := map[string]string{}
	for k, v := range map[string]string{
		"payment_id":   c.RespField("id"),
		"external_ref": externalRef,
		"beneficiary":  beneficiary,
		"amount":       strings.TrimSpace(amount + " " + currency),
		"debtor_viban": c.JSONField("debtorViban", "account"),
		"creditor":     c.JSONField("creditorAccount", "account"),
		"callback_url": c.JSONField("callback_url"),
	} {
		if v != "" {
			detail[k] = v
		}
	}
	return summary, detail
}

// summarizeCreate is summarizePayment plus the trap that caught the payout,
// if one did (detail.trap): the payouts panel then says what decides its
// fate.
func (a *app) summarizeCreate(c *activity.Call) (string, map[string]string) {
	summary, detail := summarizePayment(c)
	if id := detail["payment_id"]; id != "" {
		if p, err := a.engine.Get(id); err == nil && p.Trap != "" {
			detail["trap"] = p.Trap
		}
	}
	return summary, detail
}

// bridgeBody is what B4B hands Banking Circle for an approved payout —
// including where the money is going, so Banking Circle can pass it on to
// the beneficiary's own bank: the creditor account the payout named, or
// failing that the account the beneficiary was registered with. An IBAN
// travels as one; anything else as an account number and institution.
func (a *app) bridgeBody(p *b4b.Payment, bcPaymentID string) map[string]any {
	acct, inst, holder := p.CreditorAccount.Account, p.CreditorAccount.FinancialInstitution, p.CreditorName
	if acct == "" {
		ben := a.beneficiaries.In(p.Env).Get(p.BeneficiaryID)
		acct, inst = ben.AccountNumber, ben.FinancialInstitution
		if holder == "" {
			holder = ben.AccountName
		}
	}
	body := map[string]any{
		"paymentId":   bcPaymentID,
		"accountId":   bankingcircle.AccountIDFor(p.BeneficiaryID),
		"amount":      p.Amount.Amount,
		"currency":    p.CurrencyOfTransfer,
		"externalRef": p.ExternalRef,
		"holder":      holder,
	}
	if looksLikeIBAN(acct) {
		body["iban"] = strings.ReplaceAll(acct, " ", "")
	} else {
		body["accountNumber"], body["financialInstitution"] = acct, inst
	}
	return body
}

// ibanShape is an IBAN's outline: country, check digits, then the account.
// Shape only — the lab's GB00SIM… accounts pass it and fail any real check.
var ibanShape = regexp.MustCompile(`^[A-Z]{2}[0-9]{2}[A-Z0-9]{8,30}$`)

func looksLikeIBAN(s string) bool { return ibanShape.MatchString(strings.ReplaceAll(s, " ", "")) }
