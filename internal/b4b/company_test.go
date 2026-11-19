package b4b

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/labclock"
)

func gbCompany() CompanyParams {
	return CompanyParams{
		ExternalRef: "mer_0001",
		LegalName:   "Harness Merchant Ltd",
		TradingName: "Harness",
		CompanyType: "kyb",
		Address: &AddressParams{Address: Address{
			Line1:    "1 Simulation Way",
			City:     "London",
			Postcode: "EC1A 1AA",
			Country:  "GB",
		}},
	}
}

func TestCreateCompanyRequiresLegalNameAndAddress(t *testing.T) {
	d := NewDirectory(nil)
	cases := map[string]CompanyParams{
		"no legal name": {Address: gbCompany().Address},
		"no address":    {LegalName: "Acme Ltd"},
		"address with no country": {LegalName: "Acme Ltd", Address: &AddressParams{Address: Address{
			Line1: "1 Simulation Way", City: "London", Postcode: "EC1A 1AA",
		}}},
		"three-letter country": {LegalName: "Acme Ltd", Address: &AddressParams{Address: Address{
			Line1: "1 Simulation Way", City: "London", Postcode: "EC1A 1AA", Country: "GBR",
		}}},
		"five-digit mcc": func() CompanyParams {
			p := gbCompany()
			p.MerchantCategoryCode = "59999"
			return p
		}(),
	}
	for name, p := range cases {
		if _, err := d.CreateCompany(p); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

// A repeated external_ref is the signal a reconciler reads as "I already
// sent this". Creating a second company under it would make that signal
// unavailable and leave two companies nobody can choose between.
func TestCreateCompanyRefusesDuplicateExternalRef(t *testing.T) {
	d := NewDirectory(nil)
	first, err := d.CreateCompany(gbCompany())
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err = d.CreateCompany(gbCompany())
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("second create err = %v, want ErrDuplicate", err)
	}
	if !strings.Contains(err.Error(), first.ID) {
		t.Errorf("duplicate error should name the existing company %s, got %q", first.ID, err)
	}
	if got := d.Companies(); len(got) != 1 {
		t.Errorf("companies = %d, want 1", len(got))
	}
}

// The registered office arrives inline on the create. A client that then
// reads the addresses back and finds none would reasonably conclude it had
// to post it again.
func TestCreateCompanyMaterializesRegisteredOffice(t *testing.T) {
	d := NewDirectory(nil)
	c, err := d.CreateCompany(gbCompany())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	addrs, err := d.Addresses(c.ID)
	if err != nil {
		t.Fatalf("addresses: %v", err)
	}
	if len(addrs) != 1 || addrs[0].Type != defaultInlineAddressType || addrs[0].City != "London" {
		t.Fatalf("addresses = %+v, want one registered London address", addrs)
	}
	if c.Address == nil || c.Address.ID != addrs[0].ID {
		t.Fatalf("the company's address %+v should carry the stored address's id %s", c.Address, addrs[0].ID)
	}
}

// Resolving by the caller's own reference matters because that is the only
// identifier a client has before it has stored the one B4B minted.
func TestCompanyResolvesByExternalRef(t *testing.T) {
	d := NewDirectory(nil)
	c, err := d.CreateCompany(gbCompany())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	byRef, err := d.Company("mer_0001")
	if err != nil {
		t.Fatalf("by external_ref: %v", err)
	}
	if byRef.ID != c.ID {
		t.Fatalf("by external_ref = %s, want %s", byRef.ID, c.ID)
	}
	if _, err := d.Company("cmp_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown company err = %v, want ErrNotFound", err)
	}
}

// This is the non-GB boarding failure, reproduced: the same payload that
// boards a GB merchant with the waiver set is refused without it.
func TestPersonIdentityDocumentRequiredUnlessWaived(t *testing.T) {
	person := PersonParams{FirstName: "Ada", LastName: "Lovelace", DateOfBirth: "1815-12-10"}

	d := NewDirectory(nil)
	strict, err := d.CreateCompany(gbCompany())
	if err != nil {
		t.Fatalf("create strict company: %v", err)
	}
	_, err = d.AddPerson(strict.ID, person)
	if !errors.Is(err, ErrRequirementUnmet) {
		t.Fatalf("person with no identity document: err = %v, want ErrRequirementUnmet", err)
	}
	for _, field := range []string{"document_type", "document_number", "document_country"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error should name the missing field %s, got %q", field, err)
		}
	}

	waivedParams := gbCompany()
	waivedParams.ExternalRef = "mer_0002"
	waivedParams.PersonDocumentsWaived = true
	waived, err := d.CreateCompany(waivedParams)
	if err != nil {
		t.Fatalf("create waived company: %v", err)
	}
	if _, err := d.AddPerson(waived.ID, person); err != nil {
		t.Fatalf("same person against a waived company: %v", err)
	}

	documented := person
	documented.DocumentType, documented.DocumentNumber, documented.DocumentCountry = "passport", "P123456", "IE"
	if _, err := d.AddPerson(strict.ID, documented); err != nil {
		t.Fatalf("person with an identity document: %v", err)
	}
}

func TestPersonLegalEntityNeedsLegalNameNotDocuments(t *testing.T) {
	d := NewDirectory(nil)
	c, err := d.CreateCompany(gbCompany())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := d.AddPerson(c.ID, PersonParams{EntityType: EntityLegalEntity}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("legal entity with no legal_name: err = %v, want ErrInvalid", err)
	}
	entity, err := d.AddPerson(c.ID, PersonParams{EntityType: EntityLegalEntity, LegalName: "Holdco Ltd"})
	if err != nil {
		t.Fatalf("legal entity: %v", err)
	}
	if entity.SanctionsStatus != SanctionsPass || entity.PepSanctionsStatus != SanctionsPass {
		t.Fatalf("a new entity should screen clean, got %+v", entity)
	}
	if _, err := d.AddPerson(c.ID, PersonParams{EntityType: "robot", LegalName: "X"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown entity_type: err = %v, want ErrInvalid", err)
	}
}

// The two screening statuses move independently, because a PEP flag with no
// sanctions hit is exactly the case a client reading only the first gets
// wrong.
func TestSetPersonSanctionsMovesEachStatusIndependently(t *testing.T) {
	d := NewDirectory(nil)
	p := gbCompany()
	p.PersonDocumentsWaived = true
	c, _ := d.CreateCompany(p)
	person, err := d.AddPerson(c.ID, PersonParams{FirstName: "Ada", LastName: "Lovelace", DateOfBirth: "1815-12-10"})
	if err != nil {
		t.Fatalf("add person: %v", err)
	}
	got, changed, err := d.SetPersonSanctions(person.ID, "", SanctionsReview)
	if err != nil || !changed {
		t.Fatalf("set pep only: err=%v changed=%t", err, changed)
	}
	if got.PepSanctionsStatus != SanctionsReview || got.SanctionsStatus != SanctionsPass {
		t.Fatalf("pep-only move changed the wrong field: %+v", got)
	}
	if _, _, err := d.SetPersonSanctions(person.ID, "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty move: err = %v, want ErrInvalid", err)
	}
	if _, _, err := d.SetPersonSanctions(person.ID, "CLEAR", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("uppercase status: err = %v, want ErrInvalid", err)
	}
}

// Create-once, with no update endpoint. A boarding chain that re-runs after
// a later step failed hits this, and a client that treats it as fatal never
// finishes boarding anybody.
func TestExtendedProfileCanOnlyBeCreatedOnce(t *testing.T) {
	d := NewDirectory(nil)
	p := gbCompany()
	p.CompanyDocumentsWaived = true
	c, _ := d.CreateCompany(p)

	first, err := d.CreateExtended(c.ID, ExtendedParams{RegisteredCompanyNo: "12345678"})
	if err != nil {
		t.Fatalf("first extended: %v", err)
	}
	_, err = d.CreateExtended(c.ID, ExtendedParams{RegisteredCompanyNo: "12345678"})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("second extended: err = %v, want ErrDuplicate", err)
	}
	if !strings.Contains(err.Error(), first.ID) {
		t.Errorf("the refusal should name the profile that exists (%s), so a client can read it: %q", first.ID, err)
	}
	got, err := d.Extended(c.ID)
	if err != nil || got.ID != first.ID {
		t.Fatalf("read back = %+v, %v; want %s", got, err, first.ID)
	}
}

func TestExtendedProfileDocumentRules(t *testing.T) {
	d := NewDirectory(nil)
	c, _ := d.CreateCompany(gbCompany()) // no waiver

	if _, err := d.CreateExtended(c.ID, ExtendedParams{RegisteredCompanyNo: "12345678"}); !errors.Is(err, ErrRequirementUnmet) {
		t.Fatalf("unwaived profile with no documents: err = %v, want ErrRequirementUnmet", err)
	}
	// A document id nobody uploaded is a caller error, not a silent
	// dangling reference: the two-step upload flow only means anything if
	// the second step checks the first happened.
	_, err := d.CreateExtended(c.ID, ExtendedParams{RegisteredCompanyNo: "12345678", Documents: []ExtendedDocument{{Document: "doc_never"}}})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "doc_never") {
		t.Fatalf("unknown document id: err = %v, want ErrInvalid naming doc_never", err)
	}

	doc := d.AddDocument(DocumentParams{DocumentType: "business_registry_extract", FileName: "extract.pdf", Size: 12})
	// registered_company_no is optional, as the gateway's client has it.
	if _, err := d.CreateExtended(c.ID, ExtendedParams{Documents: []ExtendedDocument{{Document: doc.ID}}}); err != nil {
		t.Fatalf("profile with an uploaded document and no registered_company_no: %v", err)
	}
}

// Provisioning, not an append-only log: a retry must not leave a company
// with two accounts in the same currency.
func TestVibanIsStablePerCurrency(t *testing.T) {
	d := NewDirectory(nil)
	c, _ := d.CreateCompany(gbCompany())

	gbp, err := d.AddViban(c.ID, "gbp")
	if err != nil {
		t.Fatalf("add viban: %v", err)
	}
	if gbp.Currency != "GBP" {
		t.Errorf("currency = %q, want GBP (normalized)", gbp.Currency)
	}
	if !strings.HasPrefix(gbp.IBAN, "GB00SIM") {
		t.Errorf("iban %q must be obviously fake (GB00SIM...)", gbp.IBAN)
	}
	again, _ := d.AddViban(c.ID, "GBP")
	if again.ID != gbp.ID || again.IBAN != gbp.IBAN {
		t.Fatalf("re-registering GBP minted a second account: %s vs %s", again.ID, gbp.ID)
	}
	eur, _ := d.AddViban(c.ID, "EUR")
	if eur.IBAN == gbp.IBAN {
		t.Fatalf("EUR and GBP share an account number %q", eur.IBAN)
	}
	if _, err := d.AddViban(c.ID, "POUNDS"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad currency: err = %v, want ErrInvalid", err)
	}
	list, _ := d.Vibans(c.ID)
	if len(list) != 2 {
		t.Fatalf("vibans = %d, want 2", len(list))
	}
}

func TestDirectoryOnChangeFiresForEveryMutation(t *testing.T) {
	d := NewDirectory(nil)
	changes := 0
	d.OnChange = func() { changes++ }

	c, err := d.CreateCompany(gbCompany())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := d.AddAddress(c.ID, AddressParams{Type: "trading", Address: gbCompany().Address.Address}); err != nil {
		t.Fatalf("add address: %v", err)
	}
	d.AddDocument(DocumentParams{FileName: "x.pdf"})
	if _, err := d.AddViban(c.ID, "EUR"); err != nil {
		t.Fatalf("add viban: %v", err)
	}
	if changes != 4 {
		t.Fatalf("OnChange fired %d times, want 4 (company, address, document, viban)", changes)
	}
	// A refused create must not fire it: persisting after a no-op write is
	// how a state file grows entries nothing asked for.
	before := changes
	if _, err := d.CreateCompany(gbCompany()); err == nil {
		t.Fatal("expected the duplicate create to fail")
	}
	if changes != before {
		t.Fatalf("OnChange fired on a refused create")
	}
}

func TestUnknownCompanyIsNotFoundEverywhere(t *testing.T) {
	d := NewDirectory(nil)
	if _, err := d.AddAddress("cmp_nope", AddressParams{Type: "trading", Address: gbCompany().Address.Address}); !errors.Is(err, ErrNotFound) {
		t.Errorf("AddAddress err = %v, want ErrNotFound", err)
	}
	if _, err := d.AddPerson("cmp_nope", PersonParams{FirstName: "A", LastName: "B", DateOfBirth: "2000-01-01"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("AddPerson err = %v, want ErrNotFound", err)
	}
	if _, err := d.CreateExtended("cmp_nope", ExtendedParams{RegisteredCompanyNo: "1"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("CreateExtended err = %v, want ErrNotFound", err)
	}
	if _, err := d.AddViban("cmp_nope", "EUR"); !errors.Is(err, ErrNotFound) {
		t.Errorf("AddViban err = %v, want ErrNotFound", err)
	}
	if _, err := d.Person("per_nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Person err = %v, want ErrNotFound", err)
	}
	if _, err := d.Document("doc_nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Document err = %v, want ErrNotFound", err)
	}
}

// TestRecordsAreStampedOnTheRunnersClock: with the platform's clock moved,
// a company is created on the platform's day, not the wall clock's.
func TestRecordsAreStampedOnTheRunnersClock(t *testing.T) {
	labclock.Set(72 * time.Hour)
	defer labclock.Set(0)
	c, err := NewDirectory(nil).CreateCompany(gbCompany())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.CreatedAt[:10], labclock.Now().Format("2006-01-02"); got != want {
		t.Errorf("createdAt %s, want the runner's day %s", c.CreatedAt, want)
	}
}

// The lab's first address spellings still board a company, and the answer
// carries both spellings: a client written against either keeps working.
func TestAddressTakesTheEarlierSpellingsAndAnswersTheGateways(t *testing.T) {
	d := NewDirectory(nil)
	p := gbCompany()
	p.Address = &AddressParams{Address: Address{AddressLine1: "1 Old Way", AddressLine2: "Unit 2", PostalCode: "EC1A 1AA", City: "London", Country: "GB"}}
	c, err := d.CreateCompany(p)
	if err != nil {
		t.Fatalf("create with address_line_1/postal_code: %v", err)
	}
	got := c.Address.Address
	if got.Line1 != "1 Old Way" || got.Line2 != "Unit 2" || got.Postcode != "EC1A 1AA" {
		t.Fatalf("address = %+v, want the earlier spellings folded into line_1, line_2, postcode", got)
	}
	if got.AddressLine1 != "1 Old Way" || got.AddressLine2 != "Unit 2" || got.PostalCode != "EC1A 1AA" {
		t.Fatalf("address = %+v, want the earlier spellings answered too", got)
	}
	both := Address{Line1: "gateway", AddressLine1: "lab", City: "London", Country: "GB"}.normalized()
	if both.Line1 != "gateway" {
		t.Fatalf("with both spellings sent, line_1 = %q, want the gateway's", both.Line1)
	}
}

// A boarding chain re-run after an address was edited sends the new data
// under the address's id: that updates the address, and the registered
// office's mirror on the company with it. The same body again answers the
// stored address.
func TestAnAddressCreateUnderAKnownIDUpdatesIt(t *testing.T) {
	d := NewDirectory(nil)
	p := gbCompany()
	p.Address.ID = "office-1"
	c, err := d.CreateCompany(p)
	if err != nil {
		t.Fatal(err)
	}
	business := AddressParams{ID: "biz-1", Type: "business", Address: Address{Line1: "2 High Street", City: "Leeds", Country: "GB"}}
	first, err := d.AddAddress(c.ID, business)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := d.AddAddress(c.ID, business); err != nil || again.Line1 != "2 High Street" {
		t.Fatalf("identical retry = %+v, %v; want the stored address", again, err)
	}

	business.Line1 = "4 High Street"
	updated, err := d.AddAddress(c.ID, business)
	if err != nil {
		t.Fatalf("a changed body under a known id: %v", err)
	}
	if updated.ID != "biz-1" || updated.Line1 != "4 High Street" || updated.CreatedAt != first.CreatedAt {
		t.Fatalf("updated = %+v, want biz-1 at 4 High Street, created when first posted", updated)
	}

	office := AddressParams{ID: "office-1", Type: defaultInlineAddressType, Address: Address{Line1: "9 New Way", City: "London", Country: "GB"}}
	if _, err := d.AddAddress(c.ID, office); err != nil {
		t.Fatalf("a changed registered office under its id: %v", err)
	}
	stored, _ := d.Addresses(c.ID)
	lines := map[string]string{}
	for _, a := range stored {
		lines[a.ID] = a.Line1
	}
	if len(stored) != 2 || lines["biz-1"] != "4 High Street" || lines["office-1"] != "9 New Way" {
		t.Fatalf("addresses = %v, want both updated in place", lines)
	}
	read, _ := d.Company(c.ID)
	if read.Address.ID != "office-1" || read.Address.Line1 != "9 New Way" {
		t.Fatalf("company address = %+v, want the updated registered office", read.Address)
	}
}

// A PATCH tells a field sent as "" (cleared) from one not sent (kept).
func TestAPatchedAddressClearsAFieldSentEmpty(t *testing.T) {
	d := NewDirectory(nil)
	p := gbCompany()
	p.Address.Line2 = "Unit 2"
	p.Address.Region = "Greater London"
	c, err := d.CreateCompany(p)
	if err != nil {
		t.Fatal(err)
	}
	empty := ""
	got, err := d.UpdateCompany(c.ID, CompanyPatch{Address: &AddressPatch{Line2: &empty}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Address.Line2 != "" || got.Address.AddressLine2 != "" {
		t.Fatalf("address = %+v, want line_2 cleared in both spellings", got.Address.Address)
	}
	if got.Address.Region != "Greater London" || got.Address.Line1 != "1 Simulation Way" {
		t.Fatalf("address = %+v, want the fields not sent kept", got.Address.Address)
	}
	list, _ := d.Addresses(c.ID)
	if len(list) != 1 || list[0].Line2 != "" {
		t.Fatalf("addresses = %+v, want the registered office's record cleared too", list)
	}
}

// An upload slot is opened for one company; another company's profile
// cannot name it.
func TestExtendedProfileRefusesAnotherCompanysUploadSlot(t *testing.T) {
	d := NewDirectory(nil)
	mine, _ := d.CreateCompany(gbCompany())
	other := gbCompany()
	other.ExternalRef = "mer_0002"
	theirs, _ := d.CreateCompany(other)

	slot, err := d.AddUploadSlot(theirs.ID, UploadSlotParams{Filename: "x.pdf", ByteSize: 3, Checksum: "rL0Y20zC+Fzt72VPzMSk2A==", ContentType: "application/pdf"}, "tok-1", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.FillUpload("tok-1", 3, "rL0Y20zC+Fzt72VPzMSk2A==", "x"); err != nil {
		t.Fatal(err)
	}
	_, err = d.CreateExtended(mine.ID, ExtendedParams{Documents: []ExtendedDocument{{Document: slot.ID}}})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "another company") {
		t.Fatalf("err = %v, want ErrInvalid naming another company's upload", err)
	}
	if _, err := d.CreateExtended(theirs.ID, ExtendedParams{Documents: []ExtendedDocument{{Document: slot.ID}}}); err != nil {
		t.Fatalf("the slot's own company: %v", err)
	}
}

// The registered office's id cannot be reposted as another type: the
// company's inline address would stop being its registered office.
func TestTheRegisteredOfficeKeepsItsTypeOnARepost(t *testing.T) {
	d := NewDirectory(nil)
	p := gbCompany()
	p.Address.ID = "office-1"
	c, err := d.CreateCompany(p)
	if err != nil {
		t.Fatal(err)
	}
	trading := AddressParams{ID: "office-1", Type: "trading", Address: Address{Line1: "9 New Way", City: "London", Country: "GB"}}
	if _, err := d.AddAddress(c.ID, trading); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("err = %v, want ErrDuplicate for the registered office reposted as trading", err)
	}
	read, _ := d.Company(c.ID)
	if read.Address.Type != defaultInlineAddressType || read.Address.Line1 != "1 Simulation Way" {
		t.Fatalf("company address = %+v, want the registered office unchanged", read.Address)
	}
	list, _ := d.Addresses(c.ID)
	if len(list) != 1 || list[0].Type != defaultInlineAddressType {
		t.Fatalf("addresses = %+v, want the one registered office", list)
	}
}

// A repost that changes only extra updates the address's extra.
func TestAnAddressRepostChangingOnlyExtraUpdatesIt(t *testing.T) {
	d := NewDirectory(nil)
	c, _ := d.CreateCompany(gbCompany())
	business := AddressParams{ID: "biz-1", Type: "business", Address: Address{Line1: "2 High Street", City: "Leeds", Country: "GB"},
		Extra: map[string]any{"label": "front"}}
	if _, err := d.AddAddress(c.ID, business); err != nil {
		t.Fatal(err)
	}
	business.Extra = map[string]any{"label": "back"}
	got, err := d.AddAddress(c.ID, business)
	if err != nil {
		t.Fatal(err)
	}
	if got.Extra["label"] != "back" {
		t.Fatalf("answer extra = %v, want the new label", got.Extra)
	}
	list, _ := d.Addresses(c.ID)
	for _, a := range list {
		if a.ID == "biz-1" && a.Extra["label"] != "back" {
			t.Fatalf("stored extra = %v, want the new label", a.Extra)
		}
	}
}

// The gateway's name is a legal entity's name. A natural person is named by
// first_name and last_name, so a name sent with one is not a legal_name.
func TestPersonNameIsALegalNameOnlyForALegalEntity(t *testing.T) {
	d := NewDirectory(nil)
	params := gbCompany()
	params.PersonDocumentsWaived = true
	c, err := d.CreateCompany(params)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	natural, err := d.AddPerson(c.ID, PersonParams{
		Type: EntityNaturalPerson, Name: "Jane Doe",
		FirstName: "Jane", LastName: "Doe", DateOfBirth: "1990-01-01",
	})
	if err != nil {
		t.Fatalf("natural person: %v", err)
	}
	if natural.LegalName != "" {
		t.Errorf("a natural person got legal_name %q", natural.LegalName)
	}
	entity, err := d.AddPerson(c.ID, PersonParams{Type: EntityLegalEntity, Name: "Holdco Ltd"})
	if err != nil {
		t.Fatalf("legal entity: %v", err)
	}
	if entity.LegalName != "Holdco Ltd" {
		t.Errorf("a legal entity's legal_name = %q, want Holdco Ltd", entity.LegalName)
	}
}
