package b4b

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
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
// registered office, the body of POST /companies/{id}/addresses, and a
// person's own address.
//
// The wire names are those of a platform gateway's B4B client (line_1,
// postcode), because that client is one run against the real vendor. This lab's first reading of the vendor docs
// spelled them address_line_1 and postal_code; those are still accepted on
// the way in, so a body or a state file written in them keeps working, and
// answers carry both spellings, so a client reading either keeps working.
type Address struct {
	Line1    string `json:"line_1"`
	Line2    string `json:"line_2,omitempty"`
	Line3    string `json:"line_3,omitempty"`
	City     string `json:"city"`
	Region   string `json:"region"`
	Postcode string `json:"postcode"`
	Country  string `json:"country"`

	// The lab's earlier spellings, accepted on input and answered beside the gateway's.
	AddressLine1 string `json:"address_line_1"`
	AddressLine2 string `json:"address_line_2,omitempty"`
	PostalCode   string `json:"postal_code"`
}

// normalized is a with the earlier spellings folded into the gateway's (a
// value under the gateway's name wins) and mirrored back, so both spellings
// carry the same value.
func (a Address) normalized() Address {
	if a.Line1 == "" {
		a.Line1 = a.AddressLine1
	}
	if a.Line2 == "" {
		a.Line2 = a.AddressLine2
	}
	if a.Postcode == "" {
		a.Postcode = a.PostalCode
	}
	a.AddressLine1, a.AddressLine2, a.PostalCode = a.Line1, a.Line2, a.Postcode
	return a
}

// Validate applies the required-field rules a create is refused for, on the
// normalized address.
func (a Address) Validate() error {
	a = a.normalized()
	switch {
	case strings.TrimSpace(a.Line1) == "":
		return fmt.Errorf("%w: line_1 is required (address_line_1 is accepted too)", ErrInvalid)
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
	// Address is the registered office, supplied inline at create time
	// and stored as one of the company's addresses. Its id is what a
	// client sends as the extended profile's registered_office_address_id,
	// which is why a GET of the company carries it: the gateway reads it
	// from there.
	Address *CompanyAddress `json:"address,omitempty"`
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
	// Type is what the address is for. The gateway sends registered_office,
	// business, trading, cardholder or ubo; any non-empty value is kept.
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

	// TelephoneNumber, PositionsWithinBusiness and CountriesOfCitizenship
	// are the gateway's natural-person fields, kept as sent.
	TelephoneNumber         string   `json:"telephone_number,omitempty"`
	PositionsWithinBusiness []string `json:"positions_within_business,omitempty"`
	CountriesOfCitizenship  []string `json:"countries_of_citizenship,omitempty"`

	// Legal-entity fields.
	LegalName           string `json:"legal_name,omitempty"`
	RegisteredCompanyNo string `json:"registered_company_no,omitempty"`

	// Role is the gateway's single role: key_individual or shareholder.
	Role string `json:"role,omitempty"`
	// Roles is the lab's earlier list form: director, shareholder, ubo,
	// authorised_signatory. Kept as sent; neither is validated.
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
	// AddressID names one of the company's addresses (POST
	// .../addresses), which is how the gateway attaches a person's own
	// address. An id the company does not have is refused.
	AddressID string `json:"address_id,omitempty"`
	// CallbackURL is where this person's screening callback goes; without
	// one it goes to the environment's endpoint. The gateway sends one on
	// every person.
	CallbackURL string `json:"callback_url,omitempty"`

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
	// CompanyType is the legal form in the vendor's enum (limited,
	// public_limited_company, sole_trader, ...). The gateway always sends
	// it and calls it required; this lab does not refuse its absence.
	CompanyType string `json:"company_type,omitempty"`
	// RegisteredCompanyNo is the registry number. Named registered_company_no,
	// not company_number: the two names both appear in the vendor's own
	// material and only this one is the wire field. Optional, as the
	// gateway's client has it.
	RegisteredCompanyNo string   `json:"registered_company_no,omitempty"`
	NaceCodes           []string `json:"nace_codes,omitempty"`
	// RegisteredOfficeAddressID and BusinessAddressID name addresses of
	// this company; an id it does not have is refused.
	RegisteredOfficeAddressID string   `json:"registered_office_address_id,omitempty"`
	BusinessAddressID         string   `json:"business_address_id,omitempty"`
	TaxNumbers                []string `json:"tax_numbers,omitempty"`
	VatNumber                 string   `json:"vat_number,omitempty"`
	CompanyWebsite            string   `json:"company_website,omitempty"`
	BusinessActivities        string   `json:"business_activities,omitempty"`
	// Compliance carries risk_category; kept as sent.
	Compliance map[string]any `json:"compliance,omitempty"`
	// OwnershipStructure names the company's people by the ids POST
	// .../people returned; an id that is not one of its people is refused.
	OwnershipStructure *OwnershipStructure `json:"ownership_structure,omitempty"`
	// Documents are uploads (POST /uploads or POST
	// /companies/{id}/uploads) the profile references, each either a bare
	// id or the gateway's {type, document}.
	Documents         []ExtendedDocument `json:"documents,omitempty"`
	IncorporationDate string             `json:"incorporation_date,omitempty"`
	LegalForm         string             `json:"legal_form,omitempty"`
	Website           string             `json:"website,omitempty"`
	Financial         map[string]any     `json:"financial,omitempty"`
	Payments          map[string]any     `json:"payments,omitempty"`
	Extra             map[string]any     `json:"extra,omitempty"`
	CreatedAt         string             `json:"created_at"`
}

// ExtendedDocument is one entry of an extended profile's documents. The
// gateway sends {type, document}, where document is an upload's id; this
// lab's first shape was the bare id. Both decode, and each is answered in
// the form it came in.
type ExtendedDocument struct {
	Type     string `json:"type,omitempty"`
	Document string `json:"document"`
}

// UnmarshalJSON takes a bare id string or a {type, document} object.
func (e *ExtendedDocument) UnmarshalJSON(b []byte) error {
	var id string
	if err := json.Unmarshal(b, &id); err == nil {
		*e = ExtendedDocument{Document: id}
		return nil
	}
	type plain ExtendedDocument
	var obj plain
	if err := json.Unmarshal(b, &obj); err != nil {
		return fmt.Errorf("a documents entry must be an upload id or {\"type\", \"document\"}: %w", err)
	}
	*e = ExtendedDocument(obj)
	return nil
}

// MarshalJSON answers a bare id for an entry that came without a type.
func (e ExtendedDocument) MarshalJSON() ([]byte, error) {
	if e.Type == "" {
		return json.Marshal(e.Document)
	}
	type plain ExtendedDocument
	return json.Marshal(plain(e))
}

// OwnershipStructure is the extended profile's account of who owns and
// runs the company, by person id.
type OwnershipStructure struct {
	Shareholders   []Shareholder   `json:"shareholders"`
	KeyIndividuals []KeyIndividual `json:"key_individuals"`
}

// Shareholder is one owner of the company.
type Shareholder struct {
	PersonID        string   `json:"person_id"`
	SharePercentage *float64 `json:"share_percentage,omitempty"`
	Nominee         bool     `json:"nominee"`
}

// KeyIndividual is one person who runs the company: director,
// authorised_representative, ubo.
type KeyIndividual struct {
	PersonID        string   `json:"person_id"`
	SharePercentage *float64 `json:"share_percentage,omitempty"`
	Roles           []string `json:"roles"`
}

// personIDs is every person id the structure names, in order.
func (o *OwnershipStructure) personIDs() []string {
	if o == nil {
		return nil
	}
	var ids []string
	for _, s := range o.Shareholders {
		ids = append(ids, s.PersonID)
	}
	for _, k := range o.KeyIndividuals {
		ids = append(ids, k.PersonID)
	}
	return ids
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
	// CompanyID is set on an upload slot requested through
	// POST /companies/{id}/uploads.
	CompanyID string `json:"company_id,omitempty"`
	// Checksum is the slot's base64 MD5, as the client declared it.
	Checksum string `json:"checksum,omitempty"`
	// AwaitingBytes is a slot whose PUT has not arrived yet. An extended
	// profile naming one is refused, as the vendor refuses a signed id with
	// no file behind it. A receipt from POST /uploads never awaits: its
	// bytes came with it.
	AwaitingBytes bool `json:"awaiting_bytes,omitempty"`
	// UploadToken is the secret in the slot's presigned URL. Kept in the
	// state file so a slot survives a restart; never on the wire.
	UploadToken string `json:"upload_token,omitempty"`
	CreatedAt   string `json:"created_at"`
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
	ExternalRef            string `json:"external_ref"`
	LegalName              string `json:"legal_name"`
	TradingName            string `json:"trading_name"`
	CompanyType            string `json:"company_type"`
	Acquirer               string `json:"acquirer"`
	PrimaryChannel         string `json:"primary_channel"`
	MerchantCategoryCode   string `json:"merchant_category_code"`
	CompanyDocumentsWaived bool   `json:"company_documents_waived"`
	PersonDocumentsWaived  bool   `json:"person_documents_waived"`
	// Address is the registered office. The gateway sends its type
	// (registered_office) inside it; without one it is stored as registered.
	Address *AddressParams `json:"address"`
	Extra   map[string]any `json:"-"`
}

// RegisteredOffice is the gateway's type for the inline company address.
const RegisteredOffice = "registered_office"

// defaultInlineAddressType is what an inline company address is stored
// under when it names no type, as before the gateway's type was accepted.
const defaultInlineAddressType = "registered"

// Validate applies the rules a real create refuses on.
func (p CompanyParams) Validate() error {
	if strings.TrimSpace(p.LegalName) == "" {
		return fmt.Errorf("%w: legal_name is required", ErrInvalid)
	}
	if p.Address == nil {
		return fmt.Errorf("%w: address (the registered office) is required", ErrInvalid)
	}
	// The type is optional here: it defaults to registered.
	if err := p.Address.Address.Validate(); err != nil {
		return err
	}
	if p.MerchantCategoryCode != "" && !isFourDigits(p.MerchantCategoryCode) {
		return fmt.Errorf("%w: merchant_category_code must be four digits, got %q", ErrInvalid, p.MerchantCategoryCode)
	}
	return nil
}

// AddressParams is POST /oversight/v1/companies/{id}/addresses' body, and
// the inline address on a company create.
type AddressParams struct {
	// ID is the client's own id for the address. The gateway sends one (a
	// UUIDv5 of the record it came from) and uses the id the answer
	// carries; it is kept as the address's id, and a second address under
	// the same id on the same company is refused.
	ID   string `json:"id"`
	Type string `json:"type"`
	Address
	Extra map[string]any `json:"-"`
}

// Validate applies the required-field rules.
func (p AddressParams) Validate() error {
	if strings.TrimSpace(p.Type) == "" {
		return fmt.Errorf("%w: type is required (registered_office, business, trading, cardholder or ubo)", ErrInvalid)
	}
	return p.Address.Validate()
}

// CompanyPatch is PATCH /oversight/v1/companies/{id}'s body: every field
// optional, and only the ones sent change. The gateway sends it to bring
// an already-boarded company's KYB fields up to date instead of creating
// it again.
type CompanyPatch struct {
	ExternalRef            *string `json:"external_ref"`
	LegalName              *string `json:"legal_name"`
	TradingName            *string `json:"trading_name"`
	CompanyType            *string `json:"company_type"`
	Acquirer               *string `json:"acquirer"`
	PrimaryChannel         *string `json:"primary_channel"`
	MerchantCategoryCode   *string `json:"merchant_category_code"`
	CompanyDocumentsWaived *bool   `json:"company_documents_waived"`
	PersonDocumentsWaived  *bool   `json:"person_documents_waived"`
	// Address changes the registered office: each field sent replaces
	// that field, the rest stay, and a field sent as "" clears it.
	Address *AddressPatch  `json:"address"`
	Extra   map[string]any `json:"-"`
}

// AddressPatch is a PATCH's address: a nil field was not sent and keeps
// its value; a sent one, even "", replaces it. The earlier spellings are
// taken too, a value under the current name winning.
type AddressPatch struct {
	Line1    *string `json:"line_1"`
	Line2    *string `json:"line_2"`
	Line3    *string `json:"line_3"`
	City     *string `json:"city"`
	Region   *string `json:"region"`
	Postcode *string `json:"postcode"`
	Country  *string `json:"country"`

	AddressLine1 *string `json:"address_line_1"`
	AddressLine2 *string `json:"address_line_2"`
	PostalCode   *string `json:"postal_code"`
}

// applyTo is current with every field the patch sent in its place.
func (p AddressPatch) applyTo(current Address) Address {
	setString(&current.Line1, firstSent(p.Line1, p.AddressLine1))
	setString(&current.Line2, firstSent(p.Line2, p.AddressLine2))
	setString(&current.Line3, p.Line3)
	setString(&current.City, p.City)
	setString(&current.Region, p.Region)
	setString(&current.Postcode, firstSent(p.Postcode, p.PostalCode))
	setString(&current.Country, p.Country)
	// normalized refills an emptied field from its earlier spelling, so
	// both spellings take the new value.
	current.AddressLine1, current.AddressLine2, current.PostalCode = current.Line1, current.Line2, current.Postcode
	return current
}

// firstSent is the first of values that was sent, or nil.
func firstSent(values ...*string) *string {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

// Validate applies the create's rules to the fields sent.
func (p CompanyPatch) Validate() error {
	if p.LegalName != nil && strings.TrimSpace(*p.LegalName) == "" {
		return fmt.Errorf("%w: legal_name cannot be blank", ErrInvalid)
	}
	if p.MerchantCategoryCode != nil && *p.MerchantCategoryCode != "" && !isFourDigits(*p.MerchantCategoryCode) {
		return fmt.Errorf("%w: merchant_category_code must be four digits, got %q", ErrInvalid, *p.MerchantCategoryCode)
	}
	return nil
}

// PersonParams is POST /oversight/v1/companies/{id}/people' body.
//
// The gateway's names are accepted beside the lab's earlier ones: type for
// entity_type and name for legal_name are folded into the lab's fields by
// normalized, since the rules key on them; its other fields are kept as
// their own.
type PersonParams struct {
	ExternalRef             string         `json:"external_ref"`
	EntityType              string         `json:"entity_type"`
	Type                    string         `json:"type"`
	Name                    string         `json:"name"`
	Role                    string         `json:"role"`
	CallbackURL             string         `json:"callback_url"`
	TelephoneNumber         string         `json:"telephone_number"`
	PositionsWithinBusiness []string       `json:"positions_within_business"`
	CountriesOfCitizenship  []string       `json:"countries_of_citizenship"`
	AddressID               string         `json:"address_id"`
	FirstName               string         `json:"first_name"`
	MiddleName              string         `json:"middle_name"`
	LastName                string         `json:"last_name"`
	DateOfBirth             string         `json:"date_of_birth"`
	Nationality             string         `json:"nationality"`
	Email                   string         `json:"email"`
	Phone                   string         `json:"phone"`
	LegalName               string         `json:"legal_name"`
	RegisteredCompanyNo     string         `json:"registered_company_no"`
	Roles                   []string       `json:"roles"`
	OwnershipPercentage     string         `json:"ownership_percentage"`
	IsPEP                   bool           `json:"is_pep"`
	DocumentType            string         `json:"document_type"`
	DocumentNumber          string         `json:"document_number"`
	DocumentCountry         string         `json:"document_country"`
	Address                 *Address       `json:"address"`
	Extra                   map[string]any `json:"-"`
}

// normalized is p with the gateway's type and name folded into entity_type
// and legal_name, where those are empty. The gateway's name is a legal
// entity's name only: a natural person is named by first_name and
// last_name, and has no legal_name.
func (p PersonParams) normalized() PersonParams {
	if p.EntityType == "" {
		p.EntityType = p.Type
	}
	if p.LegalName == "" && p.EntityType == EntityLegalEntity {
		p.LegalName = p.Name
	}
	p.Type, p.Name = "", ""
	return p
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
	p = p.normalized()
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
			return fmt.Errorf("%w: name (or legal_name) is required on a legal entity", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: entity_type must be %q or %q, got %q",
			ErrInvalid, EntityNaturalPerson, EntityLegalEntity, p.EntityType)
	}
	return nil
}

// ExtendedParams is POST /oversight/v1/companies/{id}/extended' body.
type ExtendedParams struct {
	CompanyType               string              `json:"company_type"`
	RegisteredCompanyNo       string              `json:"registered_company_no"`
	NaceCodes                 []string            `json:"nace_codes"`
	RegisteredOfficeAddressID string              `json:"registered_office_address_id"`
	BusinessAddressID         string              `json:"business_address_id"`
	TaxNumbers                []string            `json:"tax_numbers"`
	VatNumber                 string              `json:"vat_number"`
	CompanyWebsite            string              `json:"company_website"`
	BusinessActivities        string              `json:"business_activities"`
	Compliance                map[string]any      `json:"compliance"`
	OwnershipStructure        *OwnershipStructure `json:"ownership_structure"`
	Documents                 []ExtendedDocument  `json:"documents"`
	IncorporationDate         string              `json:"incorporation_date"`
	LegalForm                 string              `json:"legal_form"`
	Website                   string              `json:"website"`
	Financial                 map[string]any      `json:"financial"`
	Payments                  map[string]any      `json:"payments"`
	Extra                     map[string]any      `json:"-"`
}

// Validate applies the extended profile's own rules. documentsWaived is the
// company's company_documents_waived flag: without it a business registry
// extract is required, and a client that never built an upload step finds
// that out here.
//
// registered_company_no is not required: the gateway's client, whose
// required markers were checked against the vendor's spec, has it optional.
func (p ExtendedParams) Validate(documentsWaived bool) error {
	for i, doc := range p.Documents {
		if strings.TrimSpace(doc.Document) == "" {
			return fmt.Errorf("%w: documents[%d] names no upload id", ErrInvalid, i)
		}
	}
	for _, id := range p.OwnershipStructure.personIDs() {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("%w: every ownership_structure entry needs a person_id", ErrInvalid)
		}
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

// UploadSlotParams is POST /oversight/v1/companies/{id}/uploads' body: the
// metadata of a file the client will PUT to the slot's presigned URL.
type UploadSlotParams struct {
	Filename    string `json:"filename"`
	ByteSize    int64  `json:"byte_size"`
	Checksum    string `json:"checksum"`
	ContentType string `json:"content_type"`
}

// Validate applies the slot's rules. checksum is the base64 MD5 of the
// bytes -- the gateway's client notes the vendor validates it as base64,
// not hex -- and byte_size is enforced on the PUT, so both must be real.
func (p UploadSlotParams) Validate(maxBytes int64) error {
	switch {
	case strings.TrimSpace(p.Filename) == "":
		return fmt.Errorf("%w: filename is required", ErrInvalid)
	case strings.TrimSpace(p.ContentType) == "":
		return fmt.Errorf("%w: content_type is required", ErrInvalid)
	case p.ByteSize <= 0:
		return fmt.Errorf("%w: byte_size must be positive", ErrInvalid)
	case p.ByteSize > maxBytes:
		return fmt.Errorf("%w: byte_size %d is over this lab's %d-byte limit", ErrInvalid, p.ByteSize, maxBytes)
	}
	sum, err := base64.StdEncoding.DecodeString(p.Checksum)
	if err != nil || len(sum) != md5.Size {
		return fmt.Errorf("%w: checksum must be the base64-encoded MD5 of the file, got %q", ErrInvalid, p.Checksum)
	}
	return nil
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
