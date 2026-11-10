package b4b

import (
	"errors"
	"fmt"
	"strings"
)

// The company-boarding half of the B4B Oversight API: the chain a platform
// walks before it can pay anybody -- company, addresses, people, extended
// profile, vIBANs -- plus the document uploads two of those steps reference.
//
// Why this exists at all: the payout half of this mock (beneficiary +
// payment) was built first, and a client integrating against it could reach
// `POST /payments` without ever proving it could board a merchant. Every
// interesting way boarding fails -- a profile that can only be created
// once, an identity document that is only waivable in some countries, a
// duplicate external reference -- was unreachable, so a client got a green
// run here and a 422 in production.
//
// What is simulated and what is not: the *shapes and the rules the API
// enforces on them* are real, because those are what a client gets wrong.
// The screening behind them is not: sanctions, PEP and KYB checks all
// return "pass" on a delay. Moving a status to review/fail is an explicit
// operation (`PUT /sim/...`), the same way the beneficiary gate already
// works -- see screening.go.

// EntityType distinguishes the two kinds of thing that can be attached to a
// company through the same /people endpoint: a human, and a shareholding
// company. They screen differently and a client has to tell them apart on
// the way back in, which is why the callback carries it.
const (
	EntityNaturalPerson = "natural_person"
	EntityLegalEntity   = "legal_entity"
)

// Address is the address value shape: inline on a company create as the
// registered office, and the body of POST /companies/{id}/addresses.
type Address struct {
	AddressLine1 string `json:"address_line_1"`
	AddressLine2 string `json:"address_line_2,omitempty"`
	City         string `json:"city"`
	Region       string `json:"region,omitempty"`
	PostalCode   string `json:"postal_code"`
	Country      string `json:"country"`
}

// Validate applies the required-field rules a create is refused for.
func (a Address) Validate() error {
	switch {
	case strings.TrimSpace(a.AddressLine1) == "":
		return fmt.Errorf("%w: address_line_1 is required", ErrInvalid)
	case strings.TrimSpace(a.City) == "":
		return fmt.Errorf("%w: city is required", ErrInvalid)
	case strings.TrimSpace(a.Country) == "":
		return fmt.Errorf("%w: country is required", ErrInvalid)
	case len(strings.TrimSpace(a.Country)) != 2:
		return fmt.Errorf("%w: country must be a 2-letter ISO 3166-1 code, got %q", ErrInvalid, a.Country)
	}
	return nil
}

// Company is a boarded legal entity: everything else in this file hangs off
// one.
type Company struct {
	ID string `json:"id"`
	// Env is the environment that boarded it (env.go): empty for the
	// default. The store and the state file keep it; the copies the store
	// hands out, and so the wire, never carry it.
	Env string `json:"env,omitempty"`
	// ExternalRef is the caller's own reference. Unlike a person's, this
	// one *is* stored and is unique: re-creating a company under a
	// reference that already exists is refused (see ErrDuplicate), which
	// is what makes a reconciler's "422 probably means I already sent
	// this" path reachable.
	ExternalRef string `json:"external_ref,omitempty"`
	LegalName   string `json:"legal_name"`
	TradingName string `json:"trading_name,omitempty"`
	// CompanyType is the boarding flavour ("kyb"). Accepted and echoed;
	// this mock does not branch on it.
	CompanyType string `json:"company_type,omitempty"`
	// Acquirer/PrimaryChannel/MerchantCategoryCode are the acquiring-side
	// fields a payment facilitator sends. Only the MCC is validated (four
	// digits), because a wrong-length MCC is the one a real create refuses.
	Acquirer             string `json:"acquirer,omitempty"`
	PrimaryChannel       string `json:"primary_channel,omitempty"`
	MerchantCategoryCode string `json:"merchant_category_code,omitempty"`
	// CompanyDocumentsWaived/PersonDocumentsWaived switch off the document
	// requirements on the extended profile and on every natural person.
	// They are the difference between a GB boarding that works and a
	// non-GB one that does not, so they are honoured here rather than
	// ignored: see ExtendedParams.Validate and PersonParams.Validate.
	CompanyDocumentsWaived bool `json:"company_documents_waived"`
	PersonDocumentsWaived  bool `json:"person_documents_waived"`
	// Address is the registered office, supplied inline at create time.
	Address *Address `json:"address,omitempty"`
	// Status is the company's own lifecycle: active or disabled.
	Status string `json:"status"`
	// SanctionsStatus is the KYB screening result. No callback is fired
	// for it -- the real API reports screening through the person and
	// beneficiary callbacks, not a company one -- but it is readable, and
	// a client that wants to poll boarding progress has somewhere to look.
	SanctionsStatus string `json:"sanctions_status"`
	// Extra carries every field the caller sent that this mock has no
	// column for. It is echoed back rather than dropped: the published
	// schema for this endpoint is not fully known to this lab, and
	// silently swallowing a field a client believes it sent is exactly the
	// failure this mock exists to prevent.
	Extra     map[string]any `json:"extra,omitempty"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
}

// CompanyAddress is an address registered against a company after create.
type CompanyAddress struct {
	ID        string `json:"id"`
	CompanyID string `json:"company_id"`
	// Type is what the address is for: registered, trading, operating.
	Type string `json:"type"`
	Address
	Extra     map[string]any `json:"extra,omitempty"`
	CreatedAt string         `json:"created_at"`
}

// Person is a natural person or a shareholding company attached to a
// company. Both go through POST /companies/{id}/people, and both come back
// through the same callback.
type Person struct {
	ID        string `json:"id"`
	CompanyID string `json:"company_id"`
	// ExternalRef is accepted and echoed but is deliberately NOT a lookup
	// key here, because it is not one in the real API either: external_ref
	// on a person is documented as not saved, and the person callback
	// carries no external_ref at all. A client that correlates a person
	// callback by its own reference has to find that out here, against a
	// mock, rather than in production. Resolve by ID.
	ExternalRef string `json:"external_ref,omitempty"`
	EntityType  string `json:"entity_type"`

	// Natural-person fields.
	FirstName   string `json:"first_name,omitempty"`
	MiddleName  string `json:"middle_name,omitempty"`
	LastName    string `json:"last_name,omitempty"`
	DateOfBirth string `json:"date_of_birth,omitempty"`
	Nationality string `json:"nationality,omitempty"`
	Email       string `json:"email,omitempty"`
	Phone       string `json:"phone,omitempty"`

	// Legal-entity fields.
	LegalName           string `json:"legal_name,omitempty"`
	RegisteredCompanyNo string `json:"registered_company_no,omitempty"`

	// Roles is what this person is to the company: director, shareholder,
	// ubo, authorised_signatory.
	Roles               []string `json:"roles,omitempty"`
	OwnershipPercentage string   `json:"ownership_percentage,omitempty"`
	IsPEP               bool     `json:"is_pep"`

	// Identity document. Required on a natural person unless the company
	// was created with person_documents_waived -- and the waiver itself is
	// only granted per client account in the real API, so a client that
	// relies on it everywhere is building on something it may not have.
	DocumentType    string `json:"document_type,omitempty"`
	DocumentNumber  string `json:"document_number,omitempty"`
	DocumentCountry string `json:"document_country,omitempty"`

	Address *Address `json:"address,omitempty"`

	// SanctionsStatus and PepSanctionsStatus are reported separately,
	// exactly as the callback reports them. A client that consolidates
	// only the first of the two has an unscreened PEP it believes is
	// clear.
	SanctionsStatus    string         `json:"sanctions_status"`
	PepSanctionsStatus string         `json:"pep_sanctions_status"`
	Extra              map[string]any `json:"extra,omitempty"`
	CreatedAt          string         `json:"created_at"`
	UpdatedAt          string         `json:"updated_at"`
}

// ExtendedProfile is the regulatory KYB profile. One per company, ever:
// there is no update endpoint, and a second create is refused. That is the
// rule most worth simulating here -- a client whose boarding chain re-runs
// after a later step fails will re-post this, get a 422, and stick on this
// step forever unless it checks first.
type ExtendedProfile struct {
	ID        string `json:"id"`
	CompanyID string `json:"company_id"`
	// RegisteredCompanyNo is the registry number. Named registered_company_no,
	// not company_number: the two names both appear in the vendor's own
	// material and only this one is the wire field.
	RegisteredCompanyNo string   `json:"registered_company_no,omitempty"`
	NaceCodes           []string `json:"nace_codes,omitempty"`
	// Documents holds upload ids from POST /uploads.
	Documents         []string       `json:"documents,omitempty"`
	IncorporationDate string         `json:"incorporation_date,omitempty"`
	LegalForm         string         `json:"legal_form,omitempty"`
	Website           string         `json:"website,omitempty"`
	Financial         map[string]any `json:"financial,omitempty"`
	Payments          map[string]any `json:"payments,omitempty"`
	Extra             map[string]any `json:"extra,omitempty"`
	CreatedAt         string         `json:"created_at"`
}

// Document is an uploaded file's receipt. The bytes are never stored: this
// lab has no business holding identity documents, real or invented, and a
// client only ever needs the id back to reference from a profile. Size and
// digest are kept so an upload can still be told apart from another one.
type Document struct {
	ID string `json:"id"`
	// Env is the environment that uploaded it, as on Company.
	Env          string `json:"env,omitempty"`
	DocumentType string `json:"document_type,omitempty"`
	FileName     string `json:"file_name,omitempty"`
	ContentType  string `json:"content_type,omitempty"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256,omitempty"`
	CreatedAt    string `json:"created_at"`
}

// Viban is a virtual IBAN issued to a company, per currency. Accounts are
// obviously fake (GB00SIM...), per this lab's no-real-bank-data rule.
type Viban struct {
	ID        string `json:"id"`
	CompanyID string `json:"company_id"`
	Currency  string `json:"currency"`
	IBAN      string `json:"iban"`
	BIC       string `json:"bic,omitempty"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

// Errors the HTTP layer maps to status codes. They are distinct because
// the codes are: a duplicate external_ref and a second extended profile are
// both 422 and both mean "you already did this", while a missing required
// field is a 400 the caller can fix by resending.
var (
	// ErrNotFound is an unknown id. Maps to 404.
	ErrNotFound = errors.New("b4b: not found")
	// ErrInvalid is input the caller can correct. Maps to 400.
	ErrInvalid = errors.New("b4b: invalid input")
	// ErrDuplicate is a create for something that already exists. Maps to
	// 422, which is what a real create answers and what a reconciler reads
	// as "probably already sent, go and look".
	ErrDuplicate = errors.New("b4b: already exists")
	// ErrRequirementUnmet is a create refused by a rule rather than by a
	// missing field -- an unwaived document requirement, most often. Maps
	// to 422.
	ErrRequirementUnmet = errors.New("b4b: requirement not met")
)

// CompanyParams is POST /oversight/v1/companies' body.
type CompanyParams struct {
	ExternalRef            string         `json:"external_ref"`
	LegalName              string         `json:"legal_name"`
	TradingName            string         `json:"trading_name"`
	CompanyType            string         `json:"company_type"`
	Acquirer               string         `json:"acquirer"`
	PrimaryChannel         string         `json:"primary_channel"`
	MerchantCategoryCode   string         `json:"merchant_category_code"`
	CompanyDocumentsWaived bool           `json:"company_documents_waived"`
	PersonDocumentsWaived  bool           `json:"person_documents_waived"`
	Address                *Address       `json:"address"`
	Extra                  map[string]any `json:"-"`
}

// Validate applies the rules a real create refuses on.
func (p CompanyParams) Validate() error {
	if strings.TrimSpace(p.LegalName) == "" {
		return fmt.Errorf("%w: legal_name is required", ErrInvalid)
	}
	if p.Address == nil {
		return fmt.Errorf("%w: address (the registered office) is required", ErrInvalid)
	}
	if err := p.Address.Validate(); err != nil {
		return err
	}
	if p.MerchantCategoryCode != "" && !isFourDigits(p.MerchantCategoryCode) {
		return fmt.Errorf("%w: merchant_category_code must be four digits, got %q", ErrInvalid, p.MerchantCategoryCode)
	}
	return nil
}

// AddressParams is POST /oversight/v1/companies/{id}/addresses' body.
type AddressParams struct {
	Type string `json:"type"`
	Address
	Extra map[string]any `json:"-"`
}

// Validate applies the required-field rules.
func (p AddressParams) Validate() error {
	if strings.TrimSpace(p.Type) == "" {
		return fmt.Errorf("%w: type is required (registered, trading or operating)", ErrInvalid)
	}
	return p.Address.Validate()
}

// PersonParams is POST /oversight/v1/companies/{id}/people' body.
type PersonParams struct {
	ExternalRef         string         `json:"external_ref"`
	EntityType          string         `json:"entity_type"`
	FirstName           string         `json:"first_name"`
	MiddleName          string         `json:"middle_name"`
	LastName            string         `json:"last_name"`
	DateOfBirth         string         `json:"date_of_birth"`
	Nationality         string         `json:"nationality"`
	Email               string         `json:"email"`
	Phone               string         `json:"phone"`
	LegalName           string         `json:"legal_name"`
	RegisteredCompanyNo string         `json:"registered_company_no"`
	Roles               []string       `json:"roles"`
	OwnershipPercentage string         `json:"ownership_percentage"`
	IsPEP               bool           `json:"is_pep"`
	DocumentType        string         `json:"document_type"`
	DocumentNumber      string         `json:"document_number"`
	DocumentCountry     string         `json:"document_country"`
	Address             *Address       `json:"address"`
	Extra               map[string]any `json:"-"`
}

// Validate applies the required-field rules, including the identity-document
// requirement the company's person_documents_waived flag switches off.
//
// documentsWaived is the company's flag, not the person's: the waiver is
// granted to a client account and recorded on the company, and a client
// that boards GB merchants with it set and then tries an EU one with the
// same payload discovers here that the document fields it never captured
// are not optional after all.
func (p PersonParams) Validate(documentsWaived bool) error {
	switch p.EntityType {
	case "", EntityNaturalPerson:
		if strings.TrimSpace(p.FirstName) == "" {
			return fmt.Errorf("%w: first_name is required on a natural person", ErrInvalid)
		}
		if strings.TrimSpace(p.LastName) == "" {
			return fmt.Errorf("%w: last_name is required on a natural person", ErrInvalid)
		}
		if strings.TrimSpace(p.DateOfBirth) == "" {
			return fmt.Errorf("%w: date_of_birth is required on a natural person", ErrInvalid)
		}
		if !documentsWaived {
			var missing []string
			if strings.TrimSpace(p.DocumentType) == "" {
				missing = append(missing, "document_type")
			}
			if strings.TrimSpace(p.DocumentNumber) == "" {
				missing = append(missing, "document_number")
			}
			if strings.TrimSpace(p.DocumentCountry) == "" {
				missing = append(missing, "document_country")
			}
			if len(missing) > 0 {
				return fmt.Errorf("%w: %s required on a natural person unless the company was created with person_documents_waived",
					ErrRequirementUnmet, strings.Join(missing, ", "))
			}
		}
	case EntityLegalEntity:
		if strings.TrimSpace(p.LegalName) == "" {
			return fmt.Errorf("%w: legal_name is required on a legal entity", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: entity_type must be %q or %q, got %q",
			ErrInvalid, EntityNaturalPerson, EntityLegalEntity, p.EntityType)
	}
	return nil
}

// ExtendedParams is POST /oversight/v1/companies/{id}/extended' body.
type ExtendedParams struct {
	RegisteredCompanyNo string         `json:"registered_company_no"`
	NaceCodes           []string       `json:"nace_codes"`
	Documents           []string       `json:"documents"`
	IncorporationDate   string         `json:"incorporation_date"`
	LegalForm           string         `json:"legal_form"`
	Website             string         `json:"website"`
	Financial           map[string]any `json:"financial"`
	Payments            map[string]any `json:"payments"`
	Extra               map[string]any `json:"-"`
}

// Validate applies the extended profile's own rules. documentsWaived is the
// company's company_documents_waived flag: without it a business registry
// extract is required, and a client that never built an upload step finds
// that out here.
func (p ExtendedParams) Validate(documentsWaived bool) error {
	if strings.TrimSpace(p.RegisteredCompanyNo) == "" {
		return fmt.Errorf("%w: registered_company_no is required", ErrInvalid)
	}
	if !documentsWaived && len(p.Documents) == 0 {
		return fmt.Errorf("%w: documents is required (a business registry extract) unless the company was created with company_documents_waived",
			ErrRequirementUnmet)
	}
	return nil
}

// DocumentParams is POST /oversight/v1/uploads' metadata. The bytes
// themselves are read, measured, digested and discarded.
type DocumentParams struct {
	DocumentType string
	FileName     string
	ContentType  string
	Size         int64
	SHA256       string
}

func isFourDigits(s string) bool {
	if len(s) != 4 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
