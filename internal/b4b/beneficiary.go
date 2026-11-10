package b4b

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Sanctions statuses. Only "pass" permits payments, and the status can move
// later under continuous screening -- a beneficiary that paid out yesterday
// is not guaranteed to pay out today.
const (
	SanctionsPass   = "pass"
	SanctionsReview = "review"
	SanctionsFail   = "fail"
)

// Beneficiary is B4B's oversight beneficiary record.
type Beneficiary struct {
	ID string `json:"id"`
	// Env is the environment that registered it (env.go): empty for the
	// default. The store and the state file keep it; the copies the store
	// hands out, and so the wire, never carry it.
	Env string `json:"env,omitempty"`
	// CompanyID is the boarded company this payee belongs to. Optional,
	// because this lab's own settlement stand-in names beneficiaries
	// directly without boarding anything -- but when it is set, a payment
	// naming a different company can be refused (see CheckCompany).
	CompanyID            string `json:"company_id,omitempty"`
	ExternalRef          string `json:"external_ref,omitempty"`
	AccountName          string `json:"account_name"`
	AccountNumber        string `json:"account_number"`
	FinancialInstitution string `json:"financial_institution"`
	Country              string `json:"country,omitempty"`
	// Status is active or disabled. A disabled beneficiary still reads
	// back fine and still screens clean -- it just cannot be paid, which
	// is exactly why a client that only ever looks at sanctions_status
	// fails on it and needs somewhere to discover that.
	Status string `json:"status"`
	// DisabledAt is set when Status moves to disabled, and carried on the
	// callback.
	DisabledAt string `json:"disabled_at,omitempty"`
	// SanctionsStatus is the consolidated status: "pass", "review" or
	// "fail". The previous model here returned "CLEAR", which is not one
	// of the three values a client switches on.
	SanctionsStatus string `json:"sanctions_status"`
	// CallbackURL is notified when the consolidated status changes. It is
	// a lab affordance, not a real wire field: the real API posts to one
	// endpoint configured for the client account, which is what
	// B4B_CALLBACK_URL is. Setting it here overrides that for one payee.
	CallbackURL string `json:"callback_url,omitempty"`
}

var (
	// ErrSanctionsNotPassed is returned when a payment names a beneficiary
	// whose consolidated sanctions status is not "pass".
	ErrSanctionsNotPassed = errors.New("b4b: beneficiary sanctions_status is not pass")
	// ErrCreditorMismatch is returned when a payment's creditor fields do
	// not match the beneficiary they name. Maps to 422.
	ErrCreditorMismatch = errors.New("b4b: creditor details do not match the beneficiary")
	// ErrBeneficiaryDisabled is returned when a payment names a
	// beneficiary that has been disabled. Maps to 422.
	ErrBeneficiaryDisabled = errors.New("b4b: beneficiary is disabled")
	// ErrCompanyMismatch is returned when a payment is made by a company
	// other than the one the beneficiary is registered under. Maps to 422,
	// and only checked when B4B_ENFORCE_BENEFICIARY_COMPANY is on -- see
	// CheckCompany.
	ErrCompanyMismatch = errors.New("b4b: beneficiary belongs to a different company")
)

// CheckCompany reports whether payingCompanyID may pay this beneficiary.
//
// Whether the real API enforces this is genuinely unknown to this lab, and
// it matters enormously: a platform that registers a payee under the
// merchant's company and then pays it as itself is either fine or refused
// on every single payout, with no middle ground. So the check exists and is
// off by default -- turn it on to find out what your client does when the
// answer is "refused", rather than finding out in production.
//
// A beneficiary with no CompanyID (this lab's auto-vivified ones, and
// anything registered before boarding existed) is payable by anyone.
func (b *Beneficiary) CheckCompany(payingCompanyID string) error {
	if b.CompanyID == "" || payingCompanyID == "" || b.CompanyID == payingCompanyID {
		return nil
	}
	return fmt.Errorf("%w: beneficiary %s is registered under company %s, not %s",
		ErrCompanyMismatch, b.ID, b.CompanyID, payingCompanyID)
}

// CheckCreditor enforces the consistency rule: the creditor fields on a
// payment are checked *against* the beneficiary, they do not override it.
//
// This is not a formality. A caller that puts its own idea of the account
// number in the payment and gets it silently accepted has no way to
// discover that its beneficiary record says something else -- until money
// goes to the wrong place. A mismatch is rejected and nothing is forwarded
// to Banking Circle.
func (b *Beneficiary) CheckCreditor(account, financialInstitution, name string) error {
	var mismatches []string
	if account != "" && !strings.EqualFold(account, b.AccountNumber) {
		mismatches = append(mismatches, fmt.Sprintf("creditorAccount.account %q != beneficiary account_number %q", account, b.AccountNumber))
	}
	if financialInstitution != "" && !strings.EqualFold(financialInstitution, b.FinancialInstitution) {
		mismatches = append(mismatches, fmt.Sprintf("creditorAccount.financialInstitution %q != beneficiary financial_institution %q",
			financialInstitution, b.FinancialInstitution))
	}
	if name != "" && !strings.EqualFold(name, b.AccountName) {
		mismatches = append(mismatches, fmt.Sprintf("creditorName %q != beneficiary account_name %q", name, b.AccountName))
	}
	if len(mismatches) > 0 {
		return fmt.Errorf("%w: %s", ErrCreditorMismatch, strings.Join(mismatches, "; "))
	}
	return nil
}

// LookupBeneficiary auto-vivifies a deterministic synthetic beneficiary for
// any requested id -- the same "seed on first read" philosophy as bank's
// fake accounts, hash-derived (not random) so the same id always yields the
// same beneficiary.
func LookupBeneficiary(id string) *Beneficiary {
	sum := sha256.Sum256([]byte(id))
	seed := strings.ToUpper(hex.EncodeToString(sum[:]))
	return &Beneficiary{
		ID:                   id,
		AccountName:          "B4B Merchant " + id,
		AccountNumber:        "B4B" + seed[:14],
		FinancialInstitution: "B4BBANK" + seed[14:18],
		SanctionsStatus:      SanctionsPass,
		Status:               StatusActive,
	}
}

// RegisterParams carries POST /oversight/v1/beneficiaries' body.
type RegisterParams struct {
	// CompanyID is the boarded company the payee belongs to. Optional
	// here, validated by the caller against the directory when supplied.
	CompanyID            string
	ExternalRef          string
	AccountName          string
	AccountNumber        string
	FinancialInstitution string
	Country              string
	CallbackURL          string
	// SanctionsStatus lets a caller register a beneficiary that is already
	// under review or blocked, so the paths a client has to handle can be
	// exercised. Empty means "pass".
	SanctionsStatus string
}

// Validate applies the required-field rules.
func (p RegisterParams) Validate() error {
	switch {
	case strings.TrimSpace(p.AccountName) == "":
		return errors.New("account_name is required")
	case strings.TrimSpace(p.AccountNumber) == "":
		return errors.New("account_number is required")
	case strings.TrimSpace(p.FinancialInstitution) == "":
		return errors.New("financial_institution is required")
	}
	switch p.SanctionsStatus {
	case "", SanctionsPass, SanctionsReview, SanctionsFail:
	default:
		return fmt.Errorf("sanctions_status must be one of %q, %q, %q", SanctionsPass, SanctionsReview, SanctionsFail)
	}
	return nil
}

// BeneficiaryStore holds registered beneficiaries and auto-vivifies the
// rest.
//
// Both behaviours are wanted: a client that registers its beneficiaries
// properly gets the real contract, including the consistency check and the
// sanctions gate, while a caller that just names an id still works -- which
// is what this lab's own settlement stand-in does, and what keeps a demo
// from needing a registration step before it can pay anybody.
//
// A BeneficiaryStore is one environment's view of the store (env.go): the
// one NewBeneficiaryStore returns is the default's, and In gives another's
// over the same records.
type BeneficiaryStore struct {
	*beneficiaries
	own string // the environment this view acts for, as records store it
}

type beneficiaries struct {
	mu sync.Mutex
	// byID indexes beneficiaries by refKey(owner, id): an auto-vivified
	// payee a /sim hook materializes keeps the id the caller named, and two
	// environments may name the same one.
	byID map[string]*Beneficiary
	// byRef indexes registered beneficiaries by refKey(owner, external_ref).
	// B4B mints the id, so a client that keys its payouts on its own
	// reference -- a MID, say -- would otherwise never be able to reach the
	// record it registered, and would silently pay against an auto-vivified
	// one carrying account details it never supplied.
	byRef map[string]*Beneficiary
	newID func() string

	// OnChange fires after any mutation, with no lock held. Nil disables
	// it; cmd/b4b uses it to persist.
	OnChange func()
}

// NewBeneficiaryStore returns an empty store. newID may be nil, in which
// case ids are derived from the external ref.
func NewBeneficiaryStore(newID func() string) *BeneficiaryStore {
	return &BeneficiaryStore{beneficiaries: &beneficiaries{
		byID:  map[string]*Beneficiary{},
		byRef: map[string]*Beneficiary{},
		newID: newID,
	}}
}

// In is the store as env sees it: only env's beneficiaries. Another
// environment's id reads as an unknown one, and so auto-vivifies.
func (s *BeneficiaryStore) In(env string) *BeneficiaryStore {
	return &BeneficiaryStore{beneficiaries: s.beneficiaries, own: owner(env)}
}

// resolve finds one of this view's registered beneficiaries by B4B's own id
// or by the caller's external_ref, in that order. Callers hold s.mu.
func (s *BeneficiaryStore) resolve(id string) (*Beneficiary, bool) {
	if b, ok := s.byID[refKey(s.own, id)]; ok {
		return b, true
	}
	b, ok := s.byRef[refKey(s.own, id)]
	return b, ok
}

// materialize is resolve, or the auto-vivified payee for id stored as this
// view's, so a change to it sticks. Callers hold s.mu.
func (s *BeneficiaryStore) materialize(id string) *Beneficiary {
	if b, ok := s.resolve(id); ok {
		return b
	}
	b := LookupBeneficiary(id)
	b.Env = s.own
	s.byID[refKey(s.own, id)] = b
	return b
}

// vendor is b as the wire shows it: without its owner.
func (b Beneficiary) vendor() *Beneficiary { b.Env = ""; return &b }

// Register creates a beneficiary.
func (s *BeneficiaryStore) Register(p RegisterParams) (*Beneficiary, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	status := p.SanctionsStatus
	if status == "" {
		status = SanctionsPass
	}
	id := ""
	if s.newID != nil {
		id = s.newID()
	} else {
		sum := sha256.Sum256([]byte(p.ExternalRef + p.AccountNumber))
		id = "ben_" + hex.EncodeToString(sum[:6])
	}
	b := &Beneficiary{
		ID:                   id,
		Env:                  s.own,
		CompanyID:            p.CompanyID,
		ExternalRef:          p.ExternalRef,
		Status:               StatusActive,
		AccountName:          p.AccountName,
		AccountNumber:        p.AccountNumber,
		FinancialInstitution: p.FinancialInstitution,
		Country:              p.Country,
		SanctionsStatus:      status,
		CallbackURL:          p.CallbackURL,
	}
	s.mu.Lock()
	s.byID[refKey(s.own, b.ID)] = b
	if b.ExternalRef != "" {
		// Last registration wins, matching the id index above: a client
		// re-registering the same reference is correcting the record, not
		// creating a second payee.
		s.byRef[refKey(s.own, b.ExternalRef)] = b
	}
	cp := b.vendor()
	s.mu.Unlock()
	s.changed()
	return cp, nil
}

func (s *beneficiaries) changed() {
	if s.OnChange != nil {
		s.OnChange()
	}
}

// Get returns one of this view's registered beneficiaries, or an
// auto-vivified one for an id it does not have.
func (s *BeneficiaryStore) Get(id string) *Beneficiary {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.resolve(id); ok {
		return b.vendor()
	}
	return LookupBeneficiary(id)
}

// SetSanctions moves a beneficiary's consolidated status and returns the
// updated record plus whether it actually changed -- callbacks fire on a
// change, not on every write.
//
// A status can move at any time under continuous screening, which is why
// this exists as an operation rather than only being set at registration.
func (s *BeneficiaryStore) SetSanctions(id, status string) (*Beneficiary, bool, error) {
	switch status {
	case SanctionsPass, SanctionsReview, SanctionsFail:
	default:
		return nil, false, fmt.Errorf("sanctions_status must be one of %q, %q, %q", SanctionsPass, SanctionsReview, SanctionsFail)
	}
	s.mu.Lock()
	// Materialize the auto-vivified one so the change sticks.
	b := s.materialize(id)
	changed := b.SanctionsStatus != status
	b.SanctionsStatus = status
	cp := b.vendor()
	s.mu.Unlock()
	s.changed()
	return cp, changed, nil
}

// SetStatus moves a beneficiary between active and disabled.
//
// The real API disables a payee on its own -- a closed account, a failed
// re-screen -- and reports it on the same callback as the sanctions status.
// Exposing it here is what lets a client be tested against a payee that is
// clean and still unpayable, which is the case a client written against
// sanctions_status alone gets wrong.
func (s *BeneficiaryStore) SetStatus(id, status string) (*Beneficiary, bool, error) {
	switch status {
	case StatusActive, StatusDisabled:
	default:
		return nil, false, fmt.Errorf("%w: status must be %q or %q", ErrInvalid, StatusActive, StatusDisabled)
	}
	s.mu.Lock()
	b := s.materialize(id)
	changed := b.Status != status
	b.Status = status
	if status == StatusDisabled {
		b.DisabledAt = now()
	} else {
		b.DisabledAt = ""
	}
	cp := b.vendor()
	s.mu.Unlock()
	s.changed()
	return cp, changed, nil
}
