package b4b

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/labclock"
)

// Directory is the boarding store: companies and everything hanging off
// one. It is HTTP-free and holds no policy about where its contents are
// written -- cmd/b4b sets OnChange to persist a snapshot after every
// mutation (state.go), which is what makes a boarded merchant survive a
// restart. A merchant boarded yesterday that has to be boarded again today
// is not a test bed, it is a demo.
//
// A Directory is one environment's view of the store (env.go): the one
// NewDirectory returns is the default's, and In gives another's over the
// same records. Snapshot, Restore and the clears act on the whole store,
// whichever view they are called on.
type Directory struct {
	*directory
	own string // the environment this view acts for, as records store it
}

type directory struct {
	mu    sync.Mutex
	newID func(prefix string) string

	companies map[string]*Company
	// companiesByRef indexes companies by refKey(owner, external_ref):
	// an external_ref is unique within an environment.
	companiesByRef  map[string]*Company
	addresses       map[string][]*CompanyAddress
	people          map[string]*Person
	peopleByCompany map[string][]string
	extended        map[string]*ExtendedProfile
	documents       map[string]*Document
	vibans          map[string][]*Viban

	// OnChange fires after any mutation, with no lock held. Nil disables
	// it, which is what tests want.
	OnChange func()
}

// Entity statuses. A disabled company or beneficiary still exists and is
// still readable; it just cannot be used.
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// NewDirectory returns an empty directory. newID may be nil, in which case
// ids are short random hex with the given prefix -- the same shape
// beneficiaries and payments already use.
func NewDirectory(newID func(prefix string) string) *Directory {
	if newID == nil {
		newID = defaultID
	}
	return &Directory{directory: &directory{
		newID:           newID,
		companies:       map[string]*Company{},
		companiesByRef:  map[string]*Company{},
		addresses:       map[string][]*CompanyAddress{},
		people:          map[string]*Person{},
		peopleByCompany: map[string][]string{},
		extended:        map[string]*ExtendedProfile{},
		documents:       map[string]*Document{},
		vibans:          map[string][]*Viban{},
	}}
}

// In is the directory as env sees it: only env's companies, and everything
// hanging off them, and env's uploads.
func (d *Directory) In(env string) *Directory {
	return &Directory{directory: d.directory, own: owner(env)}
}

// companyLocked is one of this view's companies by B4B's own id or by the
// caller's external_ref, in that order. Callers hold d.mu.
func (d *Directory) companyLocked(id string) (*Company, bool) {
	if c, ok := d.companies[id]; ok && c.Env == d.own {
		return c, true
	}
	c, ok := d.companiesByRef[refKey(d.own, id)]
	return c, ok
}

// ownerLocked is the owner of the company cid, as stored: the default's
// for one the store does not have. Callers hold d.mu.
func (d *directory) ownerLocked(cid string) string {
	if c, ok := d.companies[cid]; ok {
		return c.Env
	}
	return ""
}

// vendor is c as the wire shows it: without its owner.
func (c Company) vendor() *Company { c.Env = ""; return &c }

func (doc Document) vendor() *Document { doc.Env = ""; return &doc }

func defaultID(prefix string) string { return prefix + "_" + shortHex() }

func now() string { return labclock.Now().Format(time.RFC3339) }

// changed releases the lock and fires OnChange. Every mutating method ends
// with `defer d.changed()` after taking the lock, so a persist can never
// run while the store is half-written.
func (d *directory) changed() {
	if d.OnChange != nil {
		d.OnChange()
	}
}

// --- companies --------------------------------------------------------

// CreateCompany boards a company.
//
// A repeated external_ref is refused with ErrDuplicate rather than quietly
// creating a second company. That is the single most useful thing this
// endpoint does for a client: a boarding chain that fails at step four and
// is retried from step one has to be able to tell "I already sent this"
// apart from "this failed", and a 422 here is how it does.
func (d *Directory) CreateCompany(p CompanyParams) (*Company, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	if p.ExternalRef != "" {
		if existing, ok := d.companiesByRef[refKey(d.own, p.ExternalRef)]; ok {
			d.mu.Unlock()
			return nil, fmt.Errorf("%w: a company with external_ref %q already exists (id %s)",
				ErrDuplicate, p.ExternalRef, existing.ID)
		}
	}
	ts := now()
	c := &Company{
		ID:                     d.newID("cmp"),
		Env:                    d.own,
		ExternalRef:            p.ExternalRef,
		LegalName:              p.LegalName,
		TradingName:            p.TradingName,
		CompanyType:            p.CompanyType,
		Acquirer:               p.Acquirer,
		PrimaryChannel:         p.PrimaryChannel,
		MerchantCategoryCode:   p.MerchantCategoryCode,
		CompanyDocumentsWaived: p.CompanyDocumentsWaived,
		PersonDocumentsWaived:  p.PersonDocumentsWaived,
		Address:                p.Address,
		Status:                 StatusActive,
		SanctionsStatus:        SanctionsPass,
		Extra:                  p.Extra,
		CreatedAt:              ts,
		UpdatedAt:              ts,
	}
	d.companies[c.ID] = c
	if c.ExternalRef != "" {
		d.companiesByRef[refKey(d.own, c.ExternalRef)] = c
	}
	// The registered office supplied inline is also materialized as a
	// readable address, so GET .../addresses is not mysteriously empty on
	// a company that plainly has one.
	if c.Address != nil {
		a := &CompanyAddress{
			ID:        d.newID("adr"),
			CompanyID: c.ID,
			Type:      "registered",
			Address:   *c.Address,
			CreatedAt: ts,
		}
		d.addresses[c.ID] = append(d.addresses[c.ID], a)
	}
	cp := c.vendor()
	d.mu.Unlock()
	d.changed()
	return cp, nil
}

// Company returns one company by B4B's own id or by the caller's
// external_ref, in that order.
func (d *Directory) Company(id string) (*Company, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.companyLocked(id)
	if !ok {
		return nil, fmt.Errorf("%w: company %q", ErrNotFound, id)
	}
	return c.vendor(), nil
}

// Companies returns every one of this view's companies, newest last.
func (d *Directory) Companies() []*Company {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*Company, 0, len(d.companies))
	for _, c := range d.companies {
		if c.Env == d.own {
			out = append(out, c.vendor())
		}
	}
	sortByCreated(out, func(c *Company) string { return c.CreatedAt + c.ID })
	return out
}

// SetCompanySanctions moves a company's KYB screening status. Lab-only:
// real screening decides this.
func (d *Directory) SetCompanySanctions(id, status string) (*Company, bool, error) {
	if err := validSanctions(status); err != nil {
		return nil, false, err
	}
	d.mu.Lock()
	c, ok := d.companyLocked(id)
	if !ok {
		d.mu.Unlock()
		return nil, false, fmt.Errorf("%w: company %q", ErrNotFound, id)
	}
	changed := c.SanctionsStatus != status
	c.SanctionsStatus = status
	c.UpdatedAt = now()
	cp := c.vendor()
	d.mu.Unlock()
	d.changed()
	return cp, changed, nil
}

// --- addresses --------------------------------------------------------

// AddAddress registers an address against a company.
func (d *Directory) AddAddress(companyID string, p AddressParams) (*CompanyAddress, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	c, ok := d.companyLocked(companyID)
	if !ok {
		d.mu.Unlock()
		return nil, fmt.Errorf("%w: company %q", ErrNotFound, companyID)
	}
	a := &CompanyAddress{
		ID:        d.newID("adr"),
		CompanyID: c.ID,
		Type:      p.Type,
		Address:   p.Address,
		Extra:     p.Extra,
		CreatedAt: now(),
	}
	d.addresses[c.ID] = append(d.addresses[c.ID], a)
	cp := *a
	d.mu.Unlock()
	d.changed()
	return &cp, nil
}

// Addresses lists a company's addresses.
func (d *Directory) Addresses(companyID string) ([]*CompanyAddress, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.companyLocked(companyID)
	if !ok {
		return nil, fmt.Errorf("%w: company %q", ErrNotFound, companyID)
	}
	out := make([]*CompanyAddress, 0, len(d.addresses[c.ID]))
	for _, a := range d.addresses[c.ID] {
		cp := *a
		out = append(out, &cp)
	}
	return out, nil
}

// --- people -----------------------------------------------------------

// AddPerson attaches a natural person or a shareholding company.
func (d *Directory) AddPerson(companyID string, p PersonParams) (*Person, error) {
	d.mu.Lock()
	c, ok := d.companyLocked(companyID)
	if !ok {
		d.mu.Unlock()
		return nil, fmt.Errorf("%w: company %q", ErrNotFound, companyID)
	}
	waived := c.PersonDocumentsWaived
	cid := c.ID
	d.mu.Unlock()

	if err := p.Validate(waived); err != nil {
		return nil, err
	}
	entity := p.EntityType
	if entity == "" {
		entity = EntityNaturalPerson
	}
	ts := now()
	person := &Person{
		ID:                  d.newID("per"),
		CompanyID:           cid,
		ExternalRef:         p.ExternalRef,
		EntityType:          entity,
		FirstName:           p.FirstName,
		MiddleName:          p.MiddleName,
		LastName:            p.LastName,
		DateOfBirth:         p.DateOfBirth,
		Nationality:         p.Nationality,
		Email:               p.Email,
		Phone:               p.Phone,
		LegalName:           p.LegalName,
		RegisteredCompanyNo: p.RegisteredCompanyNo,
		Roles:               p.Roles,
		OwnershipPercentage: p.OwnershipPercentage,
		IsPEP:               p.IsPEP,
		DocumentType:        p.DocumentType,
		DocumentNumber:      p.DocumentNumber,
		DocumentCountry:     p.DocumentCountry,
		Address:             p.Address,
		SanctionsStatus:     SanctionsPass,
		PepSanctionsStatus:  SanctionsPass,
		Extra:               p.Extra,
		CreatedAt:           ts,
		UpdatedAt:           ts,
	}
	d.mu.Lock()
	d.people[person.ID] = person
	d.peopleByCompany[cid] = append(d.peopleByCompany[cid], person.ID)
	cp := *person
	d.mu.Unlock()
	d.changed()
	return &cp, nil
}

// Person returns one person by id: one of this view's companies' people.
func (d *Directory) Person(id string) (*Person, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p, ok := d.personLocked(id)
	if !ok {
		return nil, fmt.Errorf("%w: person %q", ErrNotFound, id)
	}
	cp := *p
	return &cp, nil
}

// personLocked is one of this view's people. Callers hold d.mu.
func (d *Directory) personLocked(id string) (*Person, bool) {
	p, ok := d.people[id]
	if !ok || d.ownerLocked(p.CompanyID) != d.own {
		return nil, false
	}
	return p, true
}

// People lists a company's people.
func (d *Directory) People(companyID string) ([]*Person, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.companyLocked(companyID)
	if !ok {
		return nil, fmt.Errorf("%w: company %q", ErrNotFound, companyID)
	}
	ids := d.peopleByCompany[c.ID]
	out := make([]*Person, 0, len(ids))
	for _, id := range ids {
		if p, ok := d.people[id]; ok {
			cp := *p
			out = append(out, &cp)
		}
	}
	return out, nil
}

// SetPersonSanctions moves a person's screening statuses. An empty pep
// leaves the PEP status alone, so the two can be moved independently --
// which is the point: they are reported separately and a client that reads
// only one of them is the failure worth reproducing.
func (d *Directory) SetPersonSanctions(id, status, pep string) (*Person, bool, error) {
	if status != "" {
		if err := validSanctions(status); err != nil {
			return nil, false, err
		}
	}
	if pep != "" {
		if err := validSanctions(pep); err != nil {
			return nil, false, err
		}
	}
	if status == "" && pep == "" {
		return nil, false, fmt.Errorf("%w: one of sanctions_status or pep_sanctions_status is required", ErrInvalid)
	}
	d.mu.Lock()
	p, ok := d.personLocked(id)
	if !ok {
		d.mu.Unlock()
		return nil, false, fmt.Errorf("%w: person %q", ErrNotFound, id)
	}
	changed := false
	if status != "" && p.SanctionsStatus != status {
		p.SanctionsStatus, changed = status, true
	}
	if pep != "" && p.PepSanctionsStatus != pep {
		p.PepSanctionsStatus, changed = pep, true
	}
	p.UpdatedAt = now()
	cp := *p
	d.mu.Unlock()
	d.changed()
	return &cp, changed, nil
}

// --- extended profile -------------------------------------------------

// CreateExtended creates a company's regulatory profile, once.
//
// The second call is refused with ErrDuplicate and the existing profile's
// id in the message. There is no update endpoint in the real API and there
// is none here: a client that re-runs its boarding chain has to read
// GET .../extended before it posts, and the only way it learns that is by
// hitting this.
func (d *Directory) CreateExtended(companyID string, p ExtendedParams) (*ExtendedProfile, error) {
	d.mu.Lock()
	c, ok := d.companyLocked(companyID)
	if !ok {
		d.mu.Unlock()
		return nil, fmt.Errorf("%w: company %q", ErrNotFound, companyID)
	}
	waived := c.CompanyDocumentsWaived
	cid := c.ID
	if existing, ok := d.extended[cid]; ok {
		d.mu.Unlock()
		return nil, fmt.Errorf("%w: company %s already has an extended profile (id %s); it can only be created once and there is no update endpoint",
			ErrDuplicate, cid, existing.ID)
	}
	unknownDocs := []string{}
	for _, ref := range p.Documents {
		if doc, ok := d.documents[ref]; !ok || doc.Env != d.own {
			unknownDocs = append(unknownDocs, ref)
		}
	}
	d.mu.Unlock()

	if err := p.Validate(waived); err != nil {
		return nil, err
	}
	if len(unknownDocs) > 0 {
		return nil, fmt.Errorf("%w: documents %s were never uploaded; POST /oversight/v1/uploads first",
			ErrInvalid, strings.Join(unknownDocs, ", "))
	}
	prof := &ExtendedProfile{
		ID:                  d.newID("ext"),
		CompanyID:           cid,
		RegisteredCompanyNo: p.RegisteredCompanyNo,
		NaceCodes:           p.NaceCodes,
		Documents:           p.Documents,
		IncorporationDate:   p.IncorporationDate,
		LegalForm:           p.LegalForm,
		Website:             p.Website,
		Financial:           p.Financial,
		Payments:            p.Payments,
		Extra:               p.Extra,
		CreatedAt:           now(),
	}
	d.mu.Lock()
	// Re-check under the lock: two concurrent creates must not both win.
	if existing, ok := d.extended[cid]; ok {
		d.mu.Unlock()
		return nil, fmt.Errorf("%w: company %s already has an extended profile (id %s); it can only be created once and there is no update endpoint",
			ErrDuplicate, cid, existing.ID)
	}
	d.extended[cid] = prof
	cp := *prof
	d.mu.Unlock()
	d.changed()
	return &cp, nil
}

// Extended returns a company's profile. This is the read a client should
// make before re-posting one.
func (d *Directory) Extended(companyID string) (*ExtendedProfile, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.companyLocked(companyID)
	if !ok {
		return nil, fmt.Errorf("%w: company %q", ErrNotFound, companyID)
	}
	prof, ok := d.extended[c.ID]
	if !ok {
		return nil, fmt.Errorf("%w: company %s has no extended profile", ErrNotFound, c.ID)
	}
	cp := *prof
	return &cp, nil
}

// --- documents --------------------------------------------------------

// AddDocument records an upload. The bytes are not stored -- see Document.
func (d *Directory) AddDocument(p DocumentParams) *Document {
	doc := &Document{
		ID:           d.newID("doc"),
		Env:          d.own,
		DocumentType: p.DocumentType,
		FileName:     p.FileName,
		ContentType:  p.ContentType,
		Size:         p.Size,
		SHA256:       p.SHA256,
		CreatedAt:    now(),
	}
	d.mu.Lock()
	d.documents[doc.ID] = doc
	cp := doc.vendor()
	d.mu.Unlock()
	d.changed()
	return cp
}

// Document returns one of this view's upload receipts.
func (d *Directory) Document(id string) (*Document, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	doc, ok := d.documents[id]
	if !ok || doc.Env != d.own {
		return nil, fmt.Errorf("%w: document %q", ErrNotFound, id)
	}
	return doc.vendor(), nil
}

// --- vIBANs -----------------------------------------------------------

// AddViban issues a company a virtual IBAN for a currency. The account is
// derived from the company id and the currency, so the same company always
// gets the same vIBAN back and a re-registration corrects rather than
// multiplies.
func (d *Directory) AddViban(companyID, currency string) (*Viban, error) {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if len(currency) != 3 {
		return nil, fmt.Errorf("%w: currency must be a 3-letter ISO 4217 code, got %q", ErrInvalid, currency)
	}
	d.mu.Lock()
	c, ok := d.companyLocked(companyID)
	if !ok {
		d.mu.Unlock()
		return nil, fmt.Errorf("%w: company %q", ErrNotFound, companyID)
	}
	for _, v := range d.vibans[c.ID] {
		if v.Currency == currency {
			cp := *v
			d.mu.Unlock()
			return &cp, nil
		}
	}
	sum := sha256.Sum256([]byte(c.ID + ":" + currency))
	seed := strings.ToUpper(hex.EncodeToString(sum[:]))
	v := &Viban{
		ID:        d.newID("vib"),
		CompanyID: c.ID,
		Currency:  currency,
		IBAN:      "GB00SIM" + seed[:14],
		BIC:       "B4BBSIM" + seed[14:16],
		Status:    StatusActive,
		CreatedAt: now(),
	}
	d.vibans[c.ID] = append(d.vibans[c.ID], v)
	cp := *v
	d.mu.Unlock()
	d.changed()
	return &cp, nil
}

// Vibans lists a company's virtual IBANs.
func (d *Directory) Vibans(companyID string) ([]*Viban, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.companyLocked(companyID)
	if !ok {
		return nil, fmt.Errorf("%w: company %q", ErrNotFound, companyID)
	}
	out := make([]*Viban, 0, len(d.vibans[c.ID]))
	for _, v := range d.vibans[c.ID] {
		cp := *v
		out = append(out, &cp)
	}
	return out, nil
}

func validSanctions(status string) error {
	switch status {
	case SanctionsPass, SanctionsReview, SanctionsFail:
		return nil
	}
	return fmt.Errorf("%w: sanctions status must be one of %q, %q, %q", ErrInvalid, SanctionsPass, SanctionsReview, SanctionsFail)
}
