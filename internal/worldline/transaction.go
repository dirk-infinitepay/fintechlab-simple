package worldline

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Transaction is one card transaction Worldline acquired on a
// submerchant's behalf -- the acquirer's own record, not the platform's.
//
// This is the ownership inversion the rest of this package exists for.
// Previously the platform generated the settlement file it was supposed to
// be *receiving*, and handed it to a passive file server over a shared
// bind mount, which meant the one code path a real-host swap would
// exercise (pull it over SFTP, decrypt it, parse it) was never run. Here
// Worldline holds the transactions, builds the file from them, and the
// platform side has no way to see it except over the wire.
//
// MID is Worldline's submerchant identifier -- the per-outlet key the
// platform pays out against. One merchant may have several.
type Transaction struct {
	ID          string `json:"id"`
	MID         string `json:"mid"`
	Currency    string `json:"currency"`
	AmountCents int64  `json:"amount_cents"`
	Date        string `json:"date"` // YYYY-MM-DD, the transaction (not settlement) date

	// Env is the environment whose platform the submerchant belongs to
	// (internal/tenant): empty for the default's. The acquirer sets it from
	// the lab hook's scope; a submitted value is ignored.
	Env string `json:"env,omitempty"`

	// Card metadata a real acquirer knows and the platform does not. Each
	// is optional on submission: GenerateBambora synthesizes anything left
	// empty, deterministically from the transaction's own identity, so an
	// identical seed always produces a byte-identical file.
	Type            string `json:"type,omitempty"`
	CardSchemeName  string `json:"card_scheme_name,omitempty"`
	CardUsage       string `json:"card_usage,omitempty"`
	CardCategory    string `json:"card_category,omitempty"`
	MCC             string `json:"mcc,omitempty"`
	CountryMerchant string `json:"country_merchant,omitempty"`
	CountryIssuer   string `json:"country_issuer,omitempty"`
	TerminalID      string `json:"terminal_id,omitempty"`
}

// Validate reports whether t carries the minimum a settlement file row
// needs. Card metadata is deliberately not required (see Transaction).
func (t Transaction) Validate() error {
	switch {
	case strings.TrimSpace(t.MID) == "":
		return errors.New("mid is required")
	case strings.TrimSpace(t.Currency) == "":
		return errors.New("currency is required")
	case t.AmountCents == 0:
		return errors.New("amount_cents must be non-zero")
	}
	if _, err := time.Parse("2006-01-02", t.Date); err != nil {
		return fmt.Errorf("date %q is not YYYY-MM-DD", t.Date)
	}
	return nil
}

// Store holds the transactions Worldline has acquired but not yet
// settled, plus everything it has settled since start-up. In-memory and
// process-lifetime only: this is a simulator, and a restart is how you
// reset it.
type Store struct {
	mu   sync.Mutex
	txns []Transaction
	seq  int
}

// NewStore returns an empty Store.
func NewStore() *Store { return &Store{} }

// Add records one acquired transaction, assigning an id if the caller did
// not supply one, and returns the stored copy.
func (s *Store) Add(t Transaction) (Transaction, error) {
	if err := t.Validate(); err != nil {
		return Transaction{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.ID == "" {
		s.seq++
		t.ID = fmt.Sprintf("wltxn_%06d", s.seq)
	}
	t.Currency = strings.ToUpper(t.Currency)
	s.txns = append(s.txns, t)
	return t, nil
}

// All returns a snapshot of every stored transaction.
func (s *Store) All() []Transaction {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Transaction, len(s.txns))
	copy(out, s.txns)
	return out
}

// Range returns the transactions for mid dated within [fromDate, toDate]
// inclusive, compared lexically as "YYYY-MM-DD" (the same comparison the
// settlement report generator has always used). An empty mid matches all.
func (s *Store) Range(mid, fromDate, toDate string) []Transaction {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Transaction
	for _, t := range s.txns {
		if mid != "" && t.MID != mid {
			continue
		}
		if t.Date < fromDate || t.Date > toDate {
			continue
		}
		out = append(out, t)
	}
	return out
}

// MIDsInRange lists, sorted, every submerchant with at least one
// transaction in [fromDate, toDate]. The settlement cycle walks this to
// decide which files to cut -- Worldline settles per submerchant, and only
// for submerchants that actually traded.
func (s *Store) MIDsInRange(fromDate, toDate string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for _, t := range s.txns {
		if t.Date < fromDate || t.Date > toDate {
			continue
		}
		seen[t.MID] = true
	}
	out := make([]string, 0, len(seen))
	for mid := range seen {
		out = append(out, mid)
	}
	sort.Strings(out)
	return out
}

// CurrenciesInRange lists, sorted, the currencies mid traded in over
// [fromDate, toDate]. A settlement file is per submerchant *and* per
// currency: Worldline settles each currency separately, and the filename
// itself carries the currency (see BamboraFilename).
func (s *Store) CurrenciesInRange(mid, fromDate, toDate string) []string {
	seen := map[string]bool{}
	for _, t := range s.Range(mid, fromDate, toDate) {
		seen[t.Currency] = true
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
