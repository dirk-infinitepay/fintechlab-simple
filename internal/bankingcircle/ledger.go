// Package bankingcircle models Banking Circle as a settlement bank: an
// account ledger with virtual IBANs (VIBANs), a payout lifecycle state
// machine, and a reconciliation query. See
// docs/ARCHITECTURE-banking-circle.md for the reference model this is
// grounded in and what is deliberately out of scope, and
// docs/ARCHITECTURE-phase3-corrections.md section 1 for the
// safeguarding-account (SGA) correction this file implements.
package bankingcircle

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/money"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// Account is a Banking Circle client sub-account, identified by a virtual
// IBAN (VIBAN) — a distinct identifier space from the fake bank's IBANs, so
// the two ledgers are never mistaken for one another.
type Account struct {
	ID       string `json:"id"`
	VIBAN    string `json:"viban"`
	Holder   string `json:"holder"`
	Currency string `json:"currency"`
	Balance  string `json:"balance"` // decimal string, see internal/money
	// Env is the environment whose account this is (internal/tenant): ""
	// for the default's, which holds every account the lab had before
	// there were environments. Owner says how it is set.
	Env   string `json:"env,omitempty"`
	cents int64
	// The balance at the start of openDay, taken at the account's first
	// movement that day: what Banking Circle reports as beginOfDayAmount,
	// with the day's net movement as intraDayAmount. A platform reads its
	// balance as the two summed (buddy's SGA balance check does), so
	// reporting the whole balance in both counted every euro twice.
	openDay   string
	openCents int64
}

// FinancialDay is the date a balance belongs to, on the lab clock.
func FinancialDay(t time.Time) string { return t.Format("2006-01-02") }

// DayAmounts are the account's beginOfDay and intraDay amounts, in cents,
// on day: the balance it opened the day with, and what has moved since.
// They always sum to the balance.
func (a Account) DayAmounts(day string) (open, intraday int64) {
	if a.openDay != day {
		return a.cents, 0
	}
	return a.openCents, a.cents - a.openCents
}

// opening records the balance at the start of today, before the first
// movement of the day changes it. Caller holds the ledger lock.
func (a *Account) opening() {
	day := FinancialDay(labclock.Now())
	if a.openDay != day {
		a.openDay, a.openCents = day, a.cents
	}
}

// Ledger is a tiny in-memory account store, same shape as cmd/bank's store.
type Ledger struct {
	mu      sync.Mutex
	accts   map[string]*Account
	byVIBAN map[string]*Account
	// seeded is every account the ledger was built with, and the balance
	// it was seeded at: what a reset puts back. Accounts opened later by a
	// payout (GetOrCreate) are not in it.
	seeded map[string]int64
}

var (
	ErrAccountNotFound   = errors.New("bankingcircle: account not found")
	ErrInsufficientFunds = errors.New("bankingcircle: insufficient funds")
	ErrSameAccount       = errors.New("bankingcircle: cannot move to the same account")
	// ErrAccountNotYours is an account another environment holds: a
	// customer never moves money on, or reads, another's account.
	ErrAccountNotYours = errors.New("bankingcircle: the account is another customer's")
	// ErrInvalidAccountID is returned for an account id that is not a
	// UUID. Real Banking Circle identifies accounts by UUID and so does
	// every service in front of it, so accepting anything else here
	// would let a client hold a configuration that works only against
	// this simulator.
	ErrInvalidAccountID = errors.New("bankingcircle: account id must be a UUID")
)

// NewLedger seeds the two safeguarding accounts (SGA) this lab needs, one
// per currency, both starting at zero. Real Banking Circle's SGA starts at
// whatever Worldline has actually transferred — buddy runs an hourly
// balance check against it precisely because it can be insufficient
// (docs/ARCHITECTURE-phase3-corrections.md section 1). Pre-funding it, as
// this ledger's predecessor did, would skip the "did the money actually
// land" step the real flow starts with. Credited only via Credit, driven by
// the Engine.CreditIncoming "Worldline lump sum landed" simulation.
func NewLedger() *Ledger {
	l := &Ledger{accts: map[string]*Account{}, byVIBAN: map[string]*Account{}, seeded: map[string]int64{}}
	l.mustSeed(SGAAccountEUR, "BE00SIMSGA00000001", "Infinite Safeguarding Account EUR", "EUR", 0)
	l.mustSeed(SGAAccountGBP, "GB00SIMSGA00000001", "Infinite Safeguarding Account GBP", "GBP", 0)
	return l
}

func (l *Ledger) mustSeed(id, viban, holder, ccy string, cents int64) {
	l.seedLocked("", id, viban, holder, ccy, cents)
}

func (l *Ledger) seedLocked(owner, id, viban, holder, ccy string, cents int64) {
	a := &Account{ID: id, VIBAN: viban, Holder: holder, Currency: ccy, Env: owner, cents: cents, Balance: money.Format(cents)}
	l.accts[id] = a
	l.byVIBAN[viban] = a
	l.seeded[id] = cents
}

// OpenSafeguarding opens env's safeguarding account in currency
// (tenant.SafeguardingAccount), at zero, as a customer arriving gets them:
// seeded, so a reset puts it back to zero and keeps it, as it does the
// default's two. An account env already has is kept as it is, under holder.
// The default's are NewLedger's.
func (l *Ledger) OpenSafeguarding(env, currency, holder string) (*Account, error) {
	owner := Owner(env)
	id := tenant.SafeguardingAccount(env, currency)
	l.mu.Lock()
	defer l.mu.Unlock()
	if a, ok := l.accts[id]; ok {
		if a.Env != owner {
			return nil, fmt.Errorf("%w: %s", ErrAccountNotYours, id)
		}
		a.Holder = holder
		cp := *a
		return &cp, nil
	}
	prefix := "BE00SIMSGA"
	if strings.ToUpper(currency) == "GBP" {
		prefix = "GB00SIMSGA"
	}
	sum := sha256.Sum256([]byte(id))
	l.seedLocked(owner, id, prefix+strings.ToUpper(hex.EncodeToString(sum[:]))[:8], holder, strings.ToUpper(currency), 0)
	cp := *l.accts[id]
	return &cp, nil
}

// Get looks an account up by ID or VIBAN, whichever environment holds it.
func (l *Ledger) Get(id string) (*Account, error) { return l.GetIn(tenant.All, id) }

// GetIn looks one of scope's accounts up by ID or VIBAN: another
// environment's is ErrAccountNotFound, as an unknown one is.
func (l *Ledger) GetIn(scope, id string) (*Account, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.accts[id]
	if !ok {
		a, ok = l.byVIBAN[id]
	}
	if !ok || !tenant.InScope(scope, a.Env) {
		return nil, ErrAccountNotFound
	}
	cp := *a
	return &cp, nil
}

// GetOrCreate returns the account with the given id (looked up by ID only,
// not VIBAN), auto-vivifying a deterministic synthetic one on first read if
// it doesn't exist yet — same "seed on first read" philosophy as
// internal/b4b's beneficiary auto-vivification (LookupBeneficiary),
// hash-derived (not random) so the same id always yields the same account.
// B4B (docs/ARCHITECTURE-phase3-corrections.md section 1) derives a
// distinct account per beneficiary rather than sending a single known one,
// so the first payout to a merchant always names an account no call has
// created.
//
// The id must be a UUID. Auto-vivification is the one place this ledger
// accepts an identifier it has never seen, which makes it also the one
// place a typo becomes a real, funded, entirely fictional account that
// balances perfectly and belongs to nobody. Real Banking Circle rejects a
// malformed account id outright, and ErrInvalidAccountID is how that shows
// up here.
func (l *Ledger) GetOrCreate(id, currency string) (*Account, error) {
	return l.GetOrCreateIn(tenant.Default, id, currency)
}

// GetOrCreateIn is GetOrCreate for env: the account it opens is env's, and
// an account another environment holds is ErrAccountNotYours rather than
// handed over. Which id an environment's payout lands on is AccountIDIn's
// business, not this one's.
func (l *Ledger) GetOrCreateIn(env, id, currency string) (*Account, error) {
	if !ValidAccountID(id) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidAccountID, id)
	}
	owner := Owner(env)
	l.mu.Lock()
	defer l.mu.Unlock()
	if a, ok := l.accts[id]; ok {
		if a.Env != owner {
			return nil, fmt.Errorf("%w: %s", ErrAccountNotYours, id)
		}
		cp := *a
		return &cp, nil
	}
	sum := sha256.Sum256([]byte(id))
	seed := strings.ToUpper(hex.EncodeToString(sum[:]))
	a := &Account{
		ID:       id,
		VIBAN:    "XX00SIMGEN" + seed[:16],
		Holder:   "Auto-Vivified Creditor " + id,
		Currency: currency,
		Env:      owner,
		cents:    0,
		Balance:  money.Format(0),
	}
	l.accts[id] = a
	l.byVIBAN[a.VIBAN] = a
	cp := *a
	return &cp, nil
}

// List returns every account, every environment's.
func (l *Ledger) List() []*Account { return l.ListIn(tenant.All) }

// ListIn returns scope's accounts.
func (l *Ledger) ListIn(scope string) []*Account {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*Account, 0, len(l.accts))
	for _, a := range l.accts {
		if !tenant.InScope(scope, a.Env) {
			continue
		}
		cp := *a
		out = append(out, &cp)
	}
	return out
}

// Move debits fromID and credits toID by cents. Both must already exist as
// account IDs (not VIBANs — callers resolve VIBANs to IDs via Get first).
// Insufficient funds and same-account moves are typed errors the HTTP layer
// turns into 409/400, exactly like bank's /transfers.
func (l *Ledger) Move(fromID, toID string, cents int64) (from, to *Account, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if fromID == toID {
		return nil, nil, ErrSameAccount
	}
	f, ok := l.accts[fromID]
	if !ok {
		return nil, nil, ErrAccountNotFound
	}
	t, ok := l.accts[toID]
	if !ok {
		return nil, nil, ErrAccountNotFound
	}
	if f.cents < cents {
		return nil, nil, ErrInsufficientFunds
	}
	f.opening()
	t.opening()
	f.cents -= cents
	t.cents += cents
	f.Balance = money.Format(f.cents)
	t.Balance = money.Format(t.cents)
	fcp, tcp := *f, *t
	return &fcp, &tcp, nil
}

// Credit adds cents to toID's balance. Incoming payments (the SGA lump-sum
// landing) have no ledger "from" side in this model — the money arrives
// from outside any account this lab tracks — so unlike Move there is no
// debtor to check or debit.
func (l *Ledger) Credit(toID string, cents int64) (*Account, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.accts[toID]
	if !ok {
		return nil, ErrAccountNotFound
	}
	t.opening()
	t.cents += cents
	t.Balance = money.Format(t.cents)
	cp := *t
	return &cp, nil
}
