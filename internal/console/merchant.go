package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The hierarchy the platform actually sells through: a distributor signs
// partners, a partner signs customers (merchants), and a merchant trades
// from one or more outlets. The outlet is the unit that matters to the
// money: Worldline calls it a submerchant and identifies it by MID, and
// the platform pays out per MID, not per merchant.
//
// Nothing else in this lab needed a merchant record before -- MIDs were
// string literals in tests. That works for a scenario and not at all for a
// demo, where the first question is "who am I paying?".

// Address is a postal address. Deliberately fake and obviously so.
type Address struct {
	Line1    string `json:"line1"`
	Line2    string `json:"line2,omitempty"`
	City     string `json:"city"`
	PostCode string `json:"post_code"`
	Country  string `json:"country"` // ISO 3166-1 alpha-2
}

func (a Address) String() string {
	parts := []string{a.Line1, a.Line2, a.City, a.PostCode, a.Country}
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ", ")
}

// Distributor is the top of the hierarchy.
type Distributor struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Country   string    `json:"country"`
	CreatedAt time.Time `json:"created_at"`
}

// Partner sits under a distributor and signs merchants.
type Partner struct {
	ID            string    `json:"id"`
	DistributorID string    `json:"distributor_id"`
	Name          string    `json:"name"`
	Country       string    `json:"country"`
	CreatedAt     time.Time `json:"created_at"`
}

// Outlet is one trading location -- Worldline's submerchant, keyed by MID.
// AccountNumber/FinancialInstitution are where its share of a settlement is
// paid, and are exactly the fields B4B's creditor-consistency check
// compares a payment against.
type Outlet struct {
	ID                   string  `json:"id"`
	MID                  string  `json:"mid"`
	Name                 string  `json:"name"`
	Address              Address `json:"address"`
	TerminalID           string  `json:"terminal_id"`
	MCC                  string  `json:"mcc"`
	AccountName          string  `json:"account_name"`
	AccountNumber        string  `json:"account_number"`
	FinancialInstitution string  `json:"financial_institution"`
	// BeneficiaryID is set once the outlet has been provisioned at B4B.
	// Empty means "registered here but not known to the payout rail yet",
	// which is a real state worth being able to see.
	BeneficiaryID   string `json:"beneficiary_id,omitempty"`
	SanctionsStatus string `json:"sanctions_status,omitempty"`
}

// Merchant is the customer.
type Merchant struct {
	ID          string  `json:"id"`
	PartnerID   string  `json:"partner_id,omitempty"`
	LegalName   string  `json:"legal_name"`
	TradingName string  `json:"trading_name,omitempty"`
	Country     string  `json:"country"`
	Currency    string  `json:"currency"`
	MCC         string  `json:"mcc"`
	Address     Address `json:"address"`
	Email       string  `json:"email"`
	Status      string  `json:"status"` // draft | active | suspended
	// Batch is the batch it was made in, when it was (batch.go).
	Batch     string    `json:"batch,omitempty"`
	Outlets   []Outlet  `json:"outlets"`
	CreatedAt time.Time `json:"created_at"`
}

// Registry holds the whole hierarchy, persisted to one JSON file. A file
// rather than a database because the lab's whole promise is that a restart
// is cheap and the state is greppable; a file rather than memory only
// because a merchant you created yesterday should still be there when you
// come back to the demo.
type Registry struct {
	mu   sync.RWMutex
	path string
	seq  int

	distributors map[string]*Distributor
	partners     map[string]*Partner
	merchants    map[string]*Merchant
	batches      map[string]*Batch
}

type registryFile struct {
	Seq          int            `json:"seq"`
	Distributors []*Distributor `json:"distributors"`
	Partners     []*Partner     `json:"partners"`
	Merchants    []*Merchant    `json:"merchants"`
	Batches      []*Batch       `json:"batches,omitempty"`
}

var (
	// ErrNotFound is returned for an unknown id.
	ErrNotFound = errors.New("console: not found")
	// ErrInvalid is returned for input the caller can fix. Kept distinct
	// from every other failure so the HTTP layer can answer 400 rather
	// than 502: "bad gateway" for a blank name would send an operator
	// looking at container logs for a form-validation problem.
	ErrInvalid = errors.New("console: invalid input")
)

// NewRegistry loads the registry at path, seeding a default distributor and
// partner if the file does not exist yet. An empty path keeps it in memory
// only, which is what tests want.
func NewRegistry(path string) (*Registry, error) {
	r := &Registry{
		path:         path,
		distributors: map[string]*Distributor{},
		partners:     map[string]*Partner{},
		merchants:    map[string]*Merchant{},
		batches:      map[string]*Batch{},
	}
	if path != "" {
		if err := r.load(); err != nil {
			return nil, err
		}
	}
	if len(r.distributors) == 0 {
		d := r.addDistributorLocked("Simulator Distribution Ltd", "GB")
		r.addPartnerLocked(d.ID, "Northwind Partner Services", "NL")
	}
	return r, r.save()
}

func (r *Registry) load() error {
	b, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("console: read registry: %w", err)
	}
	var f registryFile
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("console: parse registry %s: %w", r.path, err)
	}
	r.seq = f.Seq
	for _, d := range f.Distributors {
		r.distributors[d.ID] = d
	}
	for _, p := range f.Partners {
		r.partners[p.ID] = p
	}
	for _, m := range f.Merchants {
		r.merchants[m.ID] = m
	}
	for _, b := range f.Batches {
		r.batches[b.ID] = b
	}
	return nil
}

// save writes the whole registry out. Called with r.mu already held by the
// mutating methods, or on a quiet registry at construction.
func (r *Registry) save() error {
	if r.path == "" {
		return nil
	}
	f := registryFile{Seq: r.seq}
	for _, d := range r.distributors {
		f.Distributors = append(f.Distributors, d)
	}
	for _, p := range r.partners {
		f.Partners = append(f.Partners, p)
	}
	for _, m := range r.merchants {
		f.Merchants = append(f.Merchants, m)
	}
	for _, b := range r.batches {
		f.Batches = append(f.Batches, b)
	}
	sort.Slice(f.Distributors, func(i, j int) bool { return f.Distributors[i].ID < f.Distributors[j].ID })
	sort.Slice(f.Partners, func(i, j int) bool { return f.Partners[i].ID < f.Partners[j].ID })
	sort.Slice(f.Merchants, func(i, j int) bool { return f.Merchants[i].ID < f.Merchants[j].ID })
	sort.Slice(f.Batches, func(i, j int) bool { return f.Batches[i].ID < f.Batches[j].ID })
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	// Write-then-rename: a half-written registry that fails to parse on
	// the next start would lose every merchant in it.
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

func (r *Registry) next(prefix string) string {
	r.seq++
	return fmt.Sprintf("%s_%04d", prefix, r.seq)
}

// --- distributors and partners ---------------------------------------

func (r *Registry) addDistributorLocked(name, country string) *Distributor {
	d := &Distributor{ID: r.next("dst"), Name: name, Country: normCountry(country), CreatedAt: time.Now().UTC()}
	r.distributors[d.ID] = d
	return d
}

func (r *Registry) addPartnerLocked(distributorID, name, country string) *Partner {
	p := &Partner{ID: r.next("prt"), DistributorID: distributorID, Name: name, Country: normCountry(country), CreatedAt: time.Now().UTC()}
	r.partners[p.ID] = p
	return p
}

// AddDistributor creates a distributor.
func (r *Registry) AddDistributor(name, country string) (*Distributor, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalid)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.addDistributorLocked(name, country)
	return d, r.save()
}

// AddPartner creates a partner under an existing distributor.
func (r *Registry) AddPartner(distributorID, name, country string) (*Partner, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalid)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if distributorID == "" {
		for id := range r.distributors {
			distributorID = id
			break
		}
	}
	if _, ok := r.distributors[distributorID]; !ok {
		return nil, fmt.Errorf("%w: distributor %s", ErrNotFound, distributorID)
	}
	p := r.addPartnerLocked(distributorID, name, country)
	return p, r.save()
}

// Distributors returns every distributor, id-ordered.
func (r *Registry) Distributors() []Distributor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Distributor, 0, len(r.distributors))
	for _, d := range r.distributors {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Partners returns every partner, id-ordered.
func (r *Registry) Partners() []Partner {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Partner, 0, len(r.partners))
	for _, p := range r.partners {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// --- merchants --------------------------------------------------------

// NewMerchantParams is what the create form collects. Everything except a
// name is optional: the point of the console is that you can get a usable
// merchant in one click and fix the details later, the same way this lab's
// vendors auto-vivify what they are not told.
type NewMerchantParams struct {
	PartnerID   string   `json:"partner_id"`
	LegalName   string   `json:"legal_name"`
	TradingName string   `json:"trading_name"`
	Country     string   `json:"country"`
	Currency    string   `json:"currency"`
	MCC         string   `json:"mcc"`
	Email       string   `json:"email"`
	Address     *Address `json:"address"`
	// Outlets is how many outlets to open with. Zero means one -- a
	// merchant that trades nowhere cannot be paid, and is never what
	// somebody clicking "create" meant.
	Outlets int `json:"outlets"`
}

// AddMerchant creates a merchant and its opening outlets.
func (r *Registry) AddMerchant(p NewMerchantParams) (*Merchant, error) {
	if strings.TrimSpace(p.LegalName) == "" {
		return nil, fmt.Errorf("%w: legal_name is required", ErrInvalid)
	}
	if p.Outlets < 0 || p.Outlets > 20 {
		return nil, fmt.Errorf("%w: outlets must be between 0 and 20", ErrInvalid)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.PartnerID != "" {
		if _, ok := r.partners[p.PartnerID]; !ok {
			return nil, fmt.Errorf("%w: partner %s", ErrNotFound, p.PartnerID)
		}
	}
	id := r.next("mer")
	country := normCountry(p.Country)
	if country == "" {
		country = countryFor(id)
	}
	m := &Merchant{
		ID:          id,
		PartnerID:   p.PartnerID,
		LegalName:   strings.TrimSpace(p.LegalName),
		TradingName: strings.TrimSpace(p.TradingName),
		Country:     country,
		Currency:    defaultString(strings.ToUpper(strings.TrimSpace(p.Currency)), currencyFor(country)),
		MCC:         defaultString(strings.TrimSpace(p.MCC), mccFor(id)),
		Email:       defaultString(strings.TrimSpace(p.Email), emailFor(p.LegalName)),
		Status:      "active",
		CreatedAt:   time.Now().UTC(),
	}
	if p.Address != nil && p.Address.Line1 != "" {
		m.Address = *p.Address
		if m.Address.Country == "" {
			m.Address.Country = country
		}
	} else {
		m.Address = addressFor(id, country)
	}
	n := p.Outlets
	if n == 0 {
		n = 1
	}
	for i := 0; i < n; i++ {
		m.Outlets = append(m.Outlets, r.newOutletLocked(m, ""))
	}
	r.merchants[m.ID] = m
	return m, r.save()
}

// AddOutlet opens another outlet for an existing merchant.
func (r *Registry) AddOutlet(merchantID, name string) (*Merchant, *Outlet, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.merchants[merchantID]
	if !ok {
		return nil, nil, fmt.Errorf("%w: merchant %s", ErrNotFound, merchantID)
	}
	o := r.newOutletLocked(m, name)
	m.Outlets = append(m.Outlets, o)
	return m, &o, r.save()
}

func (r *Registry) newOutletLocked(m *Merchant, name string) Outlet {
	id := r.next("out")
	n := len(m.Outlets) + 1
	if name == "" {
		name = fmt.Sprintf("%s - outlet %d", displayName(m), n)
	}
	acct := fakeAccountNumber(id)
	return Outlet{
		ID:                   id,
		MID:                  midFor(id),
		Name:                 name,
		Address:              addressFor(id, m.Country),
		TerminalID:           terminalFor(id),
		MCC:                  m.MCC,
		AccountName:          defaultString(m.TradingName, m.LegalName),
		AccountNumber:        acct,
		FinancialInstitution: sortCodeFor(id),
	}
}

// SetOutletBeneficiary records the payout-rail identity an outlet was
// provisioned with. Returns ErrNotFound if the MID is unknown.
func (r *Registry) SetOutletBeneficiary(mid, beneficiaryID, sanctions string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.merchants {
		for i := range m.Outlets {
			if m.Outlets[i].MID == mid {
				m.Outlets[i].BeneficiaryID = beneficiaryID
				m.Outlets[i].SanctionsStatus = sanctions
				return r.save()
			}
		}
	}
	return fmt.Errorf("%w: mid %s", ErrNotFound, mid)
}

// SetStatus moves a merchant between draft, active and suspended.
func (r *Registry) SetStatus(merchantID, status string) (*Merchant, error) {
	switch status {
	case "draft", "active", "suspended":
	default:
		return nil, fmt.Errorf("%w: status %q must be draft, active or suspended", ErrInvalid, status)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.merchants[merchantID]
	if !ok {
		return nil, fmt.Errorf("%w: merchant %s", ErrNotFound, merchantID)
	}
	m.Status = status
	return m, r.save()
}

// DeleteMerchant removes a merchant. It does not unwind anything already
// provisioned at a vendor -- B4B has no beneficiary-delete endpoint, and
// pretending otherwise here would be a lie about what the rail supports.
func (r *Registry) DeleteMerchant(merchantID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.merchants[merchantID]; !ok {
		return fmt.Errorf("%w: merchant %s", ErrNotFound, merchantID)
	}
	delete(r.merchants, merchantID)
	return r.save()
}

// Clear puts the registry back as it was on first start — the seeded
// distributor and partner and nothing else — and returns how many merchants
// and batches it held. What the merchants were registered as at the
// vendors stays there: the vendors reset themselves.
func (r *Registry) Clear() (merchants, batches int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	merchants, batches = len(r.merchants), len(r.batches)
	r.seq = 0
	r.distributors = map[string]*Distributor{}
	r.partners = map[string]*Partner{}
	r.merchants = map[string]*Merchant{}
	r.batches = map[string]*Batch{}
	d := r.addDistributorLocked("Simulator Distribution Ltd", "GB")
	r.addPartnerLocked(d.ID, "Northwind Partner Services", "NL")
	return merchants, batches, r.save()
}

// Merchant returns one merchant by id.
func (r *Registry) Merchant(id string) (Merchant, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.merchants[id]
	if !ok {
		return Merchant{}, fmt.Errorf("%w: merchant %s", ErrNotFound, id)
	}
	return *m, nil
}

// Merchants returns every merchant, id-ordered.
func (r *Registry) Merchants() []Merchant {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Merchant, 0, len(r.merchants))
	for _, m := range r.merchants {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func displayName(m *Merchant) string {
	if m.TradingName != "" {
		return m.TradingName
	}
	return m.LegalName
}

func defaultString(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
