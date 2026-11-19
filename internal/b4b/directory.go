package b4b

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
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
	// uploadsByToken indexes upload slots by the secret token their URL
	// carries, so a PUT finds its slot without scanning every document.
	uploadsByToken map[string]string
	vibans         map[string][]*Viban

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
		uploadsByToken:  map[string]string{},
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

// vendor is c as the wire shows it: without its owner, and with a copy of
// its registered office rather than the stored one.
func (c Company) vendor() *Company {
	c.Env = ""
	if c.Address != nil {
		a := *c.Address
		c.Address = &a
	}
	return &c
}

// vendor is doc as the wire shows it: without its owner or the secret in
// its upload URL.
func (doc Document) vendor() *Document { doc.Env = ""; doc.UploadToken = ""; return &doc }

func defaultID(prefix string) string { return prefix + "_" + RandomHex(6) }

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
	cid := d.newID("cmp")
	// The registered office supplied inline is also one of the company's
	// addresses, so GET .../addresses is not mysteriously empty on a
	// company that plainly has one, and its id can be named as the
	// extended profile's registered_office_address_id.
	office := &CompanyAddress{
		ID:        p.Address.ID,
		CompanyID: cid,
		Type:      p.Address.Type,
		Address:   p.Address.Address.normalized(),
		CreatedAt: ts,
	}
	if office.ID == "" {
		office.ID = d.newID("adr")
	}
	if office.Type == "" {
		office.Type = defaultInlineAddressType
	}
	c := &Company{
		ID:                     cid,
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
		Address:                office,
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
	stored := *office
	d.addresses[c.ID] = append(d.addresses[c.ID], &stored)
	cp := c.vendor()
	d.mu.Unlock()
	d.changed()
	return cp, nil
}

// UpdateCompany applies a PATCH: the fields sent replace the company's,
// the rest stay. A new external_ref must not be another company's, for
// the same reason a create's must not.
//
// Every change replaces the record rather than editing it in place, so a
// copy already handed out (to an answer, or to a state-file write) never
// changes under its reader.
func (d *Directory) UpdateCompany(id string, p CompanyPatch) (*Company, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	current, ok := d.companyLocked(id)
	if !ok {
		d.mu.Unlock()
		return nil, fmt.Errorf("%w: company %q", ErrNotFound, id)
	}
	if p.ExternalRef != nil && *p.ExternalRef != "" && *p.ExternalRef != current.ExternalRef {
		if other, taken := d.companiesByRef[refKey(d.own, *p.ExternalRef)]; taken {
			d.mu.Unlock()
			return nil, fmt.Errorf("%w: a company with external_ref %q already exists (id %s)",
				ErrDuplicate, *p.ExternalRef, other.ID)
		}
	}
	c := *current
	setString(&c.LegalName, p.LegalName)
	setString(&c.TradingName, p.TradingName)
	setString(&c.CompanyType, p.CompanyType)
	setString(&c.Acquirer, p.Acquirer)
	setString(&c.PrimaryChannel, p.PrimaryChannel)
	setString(&c.MerchantCategoryCode, p.MerchantCategoryCode)
	if p.CompanyDocumentsWaived != nil {
		c.CompanyDocumentsWaived = *p.CompanyDocumentsWaived
	}
	if p.PersonDocumentsWaived != nil {
		c.PersonDocumentsWaived = *p.PersonDocumentsWaived
	}
	if p.Address != nil && c.Address != nil {
		office := *c.Address
		office.Address = p.Address.applyTo(office.Address).normalized()
		if err := office.Address.Validate(); err != nil {
			d.mu.Unlock()
			return nil, err
		}
		c.Address = &office
		d.replaceAddressLocked(office)
	}
	if len(p.Extra) > 0 {
		extra := make(map[string]any, len(c.Extra)+len(p.Extra))
		for k, v := range c.Extra {
			extra[k] = v
		}
		for k, v := range p.Extra {
			extra[k] = v
		}
		c.Extra = extra
	}
	if p.ExternalRef != nil && *p.ExternalRef != c.ExternalRef {
		if c.ExternalRef != "" {
			delete(d.companiesByRef, refKey(d.own, c.ExternalRef))
		}
		c.ExternalRef = *p.ExternalRef
	}
	c.UpdatedAt = now()
	d.companies[c.ID] = &c
	if c.ExternalRef != "" {
		d.companiesByRef[refKey(d.own, c.ExternalRef)] = &c
	}
	cp := c.vendor()
	d.mu.Unlock()
	d.changed()
	return cp, nil
}

func setString(field *string, value *string) {
	if value != nil {
		*field = *value
	}
}

// replaceAddressLocked swaps the stored address with a's id for a copy of
// a. Callers hold d.mu.
func (d *directory) replaceAddressLocked(a CompanyAddress) {
	list := d.addresses[a.CompanyID]
	for i, existing := range list {
		if existing.ID == a.ID {
			replaced := a
			list[i] = &replaced
			return
		}
	}
}

// addressLocked reports whether the company cid has an address with id.
// Callers hold d.mu.
func (d *directory) addressLocked(cid, id string) bool {
	return d.findAddressLocked(cid, id) != nil
}

// findAddressLocked is the company cid's address with id, or nil.
// Callers hold d.mu.
func (d *directory) findAddressLocked(cid, id string) *CompanyAddress {
	for _, a := range d.addresses[cid] {
		if a.ID == id {
			return a
		}
	}
	return nil
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

// AddAddress registers an address against a company. A create under an
// id the company already has updates that address and answers as a create
// would; the same body again answers with the stored address.
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
	if existing := d.findAddressLocked(c.ID, p.ID); p.ID != "" && existing != nil {
		// A client retrying a create whose answer it lost sends the same body
		// again: that gets the stored address. A client re-running its
		// boarding chain after the address was edited sends the new data
		// under the same id: that updates the address, which is the only
		// way to change one that is not the registered office.
		isOffice := c.Address != nil && c.Address.ID == existing.ID
		if isOffice && existing.Type != p.Type {
			// The registered office stays one: a different type under its
			// id would make the company's inline address something else.
			d.mu.Unlock()
			return nil, fmt.Errorf("%w: address %q is company %s's registered office, of type %q; it cannot become type %q",
				ErrDuplicate, existing.ID, c.ID, existing.Type, p.Type)
		}
		if existing.Type == p.Type && existing.Address == p.Address.normalized() && sameExtra(existing.Extra, p.Extra) {
			cp := *existing
			d.mu.Unlock()
			return &cp, nil
		}
		updated := *existing
		updated.Type = p.Type
		updated.Address = p.Address.normalized()
		updated.Extra = p.Extra
		d.replaceAddressLocked(updated)
		if isOffice {
			// The registered office is mirrored on the company: replace the
			// company too, so GET company and GET .../addresses agree.
			mirrored := *c
			office := updated
			mirrored.Address = &office
			mirrored.UpdatedAt = now()
			d.companies[mirrored.ID] = &mirrored
			if mirrored.ExternalRef != "" {
				d.companiesByRef[refKey(d.own, mirrored.ExternalRef)] = &mirrored
			}
		}
		d.mu.Unlock()
		d.changed()
		return &updated, nil
	}
	a := &CompanyAddress{
		ID:        p.ID,
		CompanyID: c.ID,
		Type:      p.Type,
		Address:   p.Address.normalized(),
		Extra:     p.Extra,
		CreatedAt: now(),
	}
	if a.ID == "" {
		a.ID = d.newID("adr")
	}
	d.addresses[c.ID] = append(d.addresses[c.ID], a)
	cp := *a
	d.mu.Unlock()
	d.changed()
	return &cp, nil
}

// sameExtra reports whether two addresses carry the same extra fields, no
// fields and an empty set being the same.
func sameExtra(a, b map[string]any) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
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
	addressKnown := p.AddressID == "" || d.addressLocked(cid, p.AddressID)
	d.mu.Unlock()

	p = p.normalized()
	if err := p.Validate(waived); err != nil {
		return nil, err
	}
	if !addressKnown {
		return nil, fmt.Errorf("%w: address_id %q is not an address of company %s; POST .../addresses first",
			ErrInvalid, p.AddressID, cid)
	}
	var address *Address
	if p.Address != nil {
		normalized := p.Address.normalized()
		address = &normalized
	}
	entity := p.EntityType
	if entity == "" {
		entity = EntityNaturalPerson
	}
	ts := now()
	person := &Person{
		ID:                      d.newID("per"),
		CompanyID:               cid,
		ExternalRef:             p.ExternalRef,
		EntityType:              entity,
		FirstName:               p.FirstName,
		MiddleName:              p.MiddleName,
		LastName:                p.LastName,
		DateOfBirth:             p.DateOfBirth,
		Nationality:             p.Nationality,
		Email:                   p.Email,
		Phone:                   p.Phone,
		TelephoneNumber:         p.TelephoneNumber,
		PositionsWithinBusiness: p.PositionsWithinBusiness,
		CountriesOfCitizenship:  p.CountriesOfCitizenship,
		LegalName:               p.LegalName,
		RegisteredCompanyNo:     p.RegisteredCompanyNo,
		Role:                    p.Role,
		Roles:                   p.Roles,
		OwnershipPercentage:     p.OwnershipPercentage,
		IsPEP:                   p.IsPEP,
		DocumentType:            p.DocumentType,
		DocumentNumber:          p.DocumentNumber,
		DocumentCountry:         p.DocumentCountry,
		Address:                 address,
		AddressID:               p.AddressID,
		CallbackURL:             p.CallbackURL,
		SanctionsStatus:         SanctionsPass,
		PepSanctionsStatus:      SanctionsPass,
		Extra:                   p.Extra,
		CreatedAt:               ts,
		UpdatedAt:               ts,
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
	var unknownDocs, otherCompanyDocs, emptyDocs, unknownAddresses, unknownPeople []string
	for _, entry := range p.Documents {
		doc, ok := d.documents[entry.Document]
		switch {
		case !ok || doc.Env != d.own:
			unknownDocs = append(unknownDocs, entry.Document)
		case doc.CompanyID != "" && doc.CompanyID != cid:
			// An upload slot is opened for one company; a POST /uploads
			// receipt names none and serves any.
			otherCompanyDocs = append(otherCompanyDocs, entry.Document)
		case doc.AwaitingBytes:
			emptyDocs = append(emptyDocs, entry.Document)
		}
	}
	for _, id := range []string{p.RegisteredOfficeAddressID, p.BusinessAddressID} {
		if id != "" && !d.addressLocked(cid, id) {
			unknownAddresses = append(unknownAddresses, id)
		}
	}
	for _, id := range p.OwnershipStructure.personIDs() {
		if person, ok := d.people[id]; id != "" && (!ok || person.CompanyID != cid) {
			unknownPeople = append(unknownPeople, id)
		}
	}
	d.mu.Unlock()

	if err := p.Validate(waived); err != nil {
		return nil, err
	}
	switch {
	case len(unknownDocs) > 0:
		return nil, fmt.Errorf("%w: documents %s were never uploaded; POST /oversight/v1/uploads first",
			ErrInvalid, strings.Join(unknownDocs, ", "))
	case len(otherCompanyDocs) > 0:
		return nil, fmt.Errorf("%w: documents %s were uploaded for another company, not company %s",
			ErrInvalid, strings.Join(otherCompanyDocs, ", "), cid)
	case len(emptyDocs) > 0:
		return nil, fmt.Errorf("%w: documents %s have an upload slot but no file; PUT the bytes to the slot's url first",
			ErrInvalid, strings.Join(emptyDocs, ", "))
	case len(unknownAddresses) > 0:
		return nil, fmt.Errorf("%w: addresses %s are not addresses of company %s",
			ErrInvalid, strings.Join(unknownAddresses, ", "), cid)
	case len(unknownPeople) > 0:
		return nil, fmt.Errorf("%w: ownership_structure names %s, which are not people of company %s",
			ErrInvalid, strings.Join(unknownPeople, ", "), cid)
	}
	prof := &ExtendedProfile{
		ID:                        d.newID("ext"),
		CompanyID:                 cid,
		CompanyType:               p.CompanyType,
		RegisteredCompanyNo:       p.RegisteredCompanyNo,
		NaceCodes:                 p.NaceCodes,
		RegisteredOfficeAddressID: p.RegisteredOfficeAddressID,
		BusinessAddressID:         p.BusinessAddressID,
		TaxNumbers:                p.TaxNumbers,
		VatNumber:                 p.VatNumber,
		CompanyWebsite:            p.CompanyWebsite,
		BusinessActivities:        p.BusinessActivities,
		Compliance:                p.Compliance,
		OwnershipStructure:        p.OwnershipStructure,
		Documents:                 p.Documents,
		IncorporationDate:         p.IncorporationDate,
		LegalForm:                 p.LegalForm,
		Website:                   p.Website,
		Financial:                 p.Financial,
		Payments:                  p.Payments,
		Extra:                     p.Extra,
		CreatedAt:                 now(),
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

// AddUploadSlot opens an upload slot for one of this view's companies: a
// document record awaiting its bytes, with the secret token its presigned
// URL carries. The bytes arrive through FillUpload.
func (d *Directory) AddUploadSlot(companyID string, p UploadSlotParams, token string, maxBytes int64) (*Document, error) {
	if err := p.Validate(maxBytes); err != nil {
		return nil, err
	}
	d.mu.Lock()
	c, ok := d.companyLocked(companyID)
	if !ok {
		d.mu.Unlock()
		return nil, fmt.Errorf("%w: company %q", ErrNotFound, companyID)
	}
	doc := &Document{
		ID:            d.newID("doc"),
		Env:           d.own,
		FileName:      p.Filename,
		ContentType:   p.ContentType,
		Size:          p.ByteSize,
		CompanyID:     c.ID,
		Checksum:      p.Checksum,
		AwaitingBytes: true,
		UploadToken:   token,
		CreatedAt:     now(),
	}
	d.documents[doc.ID] = doc
	if doc.UploadToken != "" {
		d.uploadsByToken[doc.UploadToken] = doc.ID
	}
	cp := *doc
	d.mu.Unlock()
	d.changed()
	return &cp, nil
}

// ErrUploadRejected is a PUT to an upload slot whose bytes are not the ones
// the slot declared. Maps to 400, as storage answers a bad digest.
var ErrUploadRejected = errors.New("b4b: upload rejected")

// FillUpload takes the bytes PUT to the slot token names. Their length must
// be the slot's byte_size and their MD5 its checksum. Any environment's
// slot: the token is the credential, as a presigned URL's signature is.
// A second PUT of the same bytes is accepted again.
func (d *Directory) FillUpload(token string, size int64, md5Base64, sha256Hex string) (*Document, error) {
	d.mu.Lock()
	doc := d.documents[d.uploadsByToken[token]]
	if token == "" || doc == nil {
		d.mu.Unlock()
		return nil, fmt.Errorf("%w: no upload slot for this url", ErrNotFound)
	}
	if size != doc.Size {
		d.mu.Unlock()
		return nil, fmt.Errorf("%w: %d bytes sent, the slot declared byte_size %d", ErrUploadRejected, size, doc.Size)
	}
	if md5Base64 != doc.Checksum {
		d.mu.Unlock()
		return nil, fmt.Errorf("%w: the bytes' MD5 is %s, the slot declared checksum %s", ErrUploadRejected, md5Base64, doc.Checksum)
	}
	filled := *doc
	filled.AwaitingBytes = false
	filled.SHA256 = sha256Hex
	d.documents[filled.ID] = &filled
	cp := filled.vendor()
	d.mu.Unlock()
	d.changed()
	return cp, nil
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
