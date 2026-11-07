package bankingcircle

import (
	"errors"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/labclock"
)

func TestLedgerMoveSuccess(t *testing.T) {
	l := NewLedger()
	l.mustSeed("acc_from", "VBTEST0000000001", "From", "EUR", 50_000_000)
	l.mustSeed("acc_to", "VBTEST0000000002", "To", "EUR", 0)

	from, to, err := l.Move("acc_from", "acc_to", 12345)
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if from.Balance != "499876.55" {
		t.Fatalf("from balance = %s", from.Balance)
	}
	if to.Balance != "123.45" {
		t.Fatalf("to balance = %s", to.Balance)
	}
	// confirm the ledger's own copy was updated, not just the returned copy
	got, err := l.Get("acc_to")
	if err != nil {
		t.Fatal(err)
	}
	if got.Balance != "123.45" {
		t.Fatalf("stored balance = %s", got.Balance)
	}
}

func TestLedgerMoveInsufficientFunds(t *testing.T) {
	l := NewLedger()
	// The EUR safeguarding account opens at zero -- the whole point of the
	// safeguarding-account model -- so paying out of it is always an
	// insufficient-funds error until it has been credited.
	_, _, err := l.Move(SGAAccountEUR, SGAAccountGBP, 100)
	if err != ErrInsufficientFunds {
		t.Fatalf("err = %v, want ErrInsufficientFunds", err)
	}
	// balances must be unchanged on a rejected move
	acc, _ := l.Get(SGAAccountEUR)
	if acc.Balance != "0.00" {
		t.Fatalf("balance mutated on failed move: %s", acc.Balance)
	}
}

func TestLedgerMoveSameAccountRejected(t *testing.T) {
	l := NewLedger()
	_, _, err := l.Move(SGAAccountEUR, SGAAccountEUR, 100)
	if err != ErrSameAccount {
		t.Fatalf("err = %v, want ErrSameAccount", err)
	}
}

func TestLedgerMoveUnknownAccount(t *testing.T) {
	l := NewLedger()
	_, _, err := l.Move(SGAAccountEUR, "bc_acc_nope", 100)
	if err != ErrAccountNotFound {
		t.Fatalf("err = %v, want ErrAccountNotFound", err)
	}
}

func TestLedgerGetByVIBAN(t *testing.T) {
	l := NewLedger()
	acc, err := l.Get("BE00SIMSGA00000001")
	if err != nil {
		t.Fatal(err)
	}
	if acc.ID != SGAAccountEUR {
		t.Fatalf("id = %s", acc.ID)
	}
}

func TestLedgerSeedShape(t *testing.T) {
	l := NewLedger()
	eur, err := l.Get(SGAAccountEUR)
	if err != nil {
		t.Fatal(err)
	}
	if eur.VIBAN != "BE00SIMSGA00000001" || eur.Holder != "Infinite Safeguarding Account EUR" ||
		eur.Currency != "EUR" || eur.Balance != "0.00" {
		t.Fatalf("EUR SGA seed = %+v", eur)
	}
	gbp, err := l.Get(SGAAccountGBP)
	if err != nil {
		t.Fatal(err)
	}
	if gbp.VIBAN != "GB00SIMSGA00000001" || gbp.Holder != "Infinite Safeguarding Account GBP" ||
		gbp.Currency != "GBP" || gbp.Balance != "0.00" {
		t.Fatalf("GBP SGA seed = %+v", gbp)
	}
	if len(l.List()) != 2 {
		t.Fatalf("seeded account count = %d, want 2", len(l.List()))
	}
}

func TestLedgerCreditSuccess(t *testing.T) {
	l := NewLedger()
	acc, err := l.Credit(SGAAccountEUR, 1_250_000)
	if err != nil {
		t.Fatalf("Credit: %v", err)
	}
	if acc.Balance != "12500.00" {
		t.Fatalf("returned balance = %s", acc.Balance)
	}
	// confirm the ledger's own copy was updated, not just the returned copy
	got, err := l.Get(SGAAccountEUR)
	if err != nil {
		t.Fatal(err)
	}
	if got.Balance != "12500.00" {
		t.Fatalf("stored balance = %s", got.Balance)
	}

	// a second credit accumulates rather than replacing
	acc, err = l.Credit(SGAAccountEUR, 100)
	if err != nil {
		t.Fatalf("second Credit: %v", err)
	}
	if acc.Balance != "12501.00" {
		t.Fatalf("accumulated balance = %s", acc.Balance)
	}
}

func TestLedgerCreditUnknownAccount(t *testing.T) {
	l := NewLedger()
	_, err := l.Credit("bc_acc_nope", 100)
	if err != ErrAccountNotFound {
		t.Fatalf("err = %v, want ErrAccountNotFound", err)
	}
}

func TestLedgerGetOrCreateVivifiesDeterministically(t *testing.T) {
	l := NewLedger()
	newMerchant := AccountIDFor("new_merchant")

	if _, err := l.Get(newMerchant); err != ErrAccountNotFound {
		t.Fatalf("account should not exist yet: err = %v", err)
	}

	got, err := l.GetOrCreate(newMerchant, "EUR")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if got.ID != newMerchant || got.Currency != "EUR" || got.Balance != "0.00" {
		t.Fatalf("vivified account = %+v", got)
	}
	if got.VIBAN == "" {
		t.Fatalf("vivified account missing a VIBAN")
	}

	// same id found on a second call -- not re-vivified, currency argument
	// ignored once the account already exists.
	again, err := l.GetOrCreate(newMerchant, "GBP")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if again.VIBAN != got.VIBAN || again.Currency != "EUR" {
		t.Fatalf("GetOrCreate re-vivified an existing account: %+v", again)
	}

	// deterministic: a fresh ledger vivifying the same id gets the same VIBAN
	other, err := NewLedger().GetOrCreate(newMerchant, "EUR")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if other.VIBAN != got.VIBAN {
		t.Fatalf("VIBAN not deterministic: %s vs %s", other.VIBAN, got.VIBAN)
	}

	// findable afterward, by ID and by its vivified VIBAN
	found, err := l.Get(newMerchant)
	if err != nil || found.VIBAN != got.VIBAN {
		t.Fatalf("Get by id after GetOrCreate = %+v, %v", found, err)
	}
	byVIBAN, err := l.Get(got.VIBAN)
	if err != nil || byVIBAN.ID != newMerchant {
		t.Fatalf("Get by VIBAN after GetOrCreate = %+v, %v", byVIBAN, err)
	}
}

// TestLedgerRejectsANonUUIDAccountID. Auto-vivification is the one door
// into this ledger that opens for an id nobody has seen before, so it is
// also the one place a typo can mint a funded, perfectly balanced account
// belonging to nobody. Real Banking Circle identifies accounts by UUID and
// every service in front of it validates that before the request is even
// forwarded, so anything else has to be refused here too -- otherwise a
// client can hold a configuration that works against this simulator and
// against nothing else.
func TestLedgerRejectsANonUUIDAccountID(t *testing.T) {
	l := NewLedger()
	for _, id := range []string{
		"bc_acc_sga_eur",                       // the lab's own previous scheme
		"",                                     // nothing at all
		"00000000-0000-4000-8000-00000000097",  // one digit short
		"00000000-0000-4000-8000-000000000ZZZ", // not hex
		"00000000-0000-4000-8000-000000000978 ",
		"00000000000040008000000000000978", // unhyphenated
	} {
		if _, err := l.GetOrCreate(id, "EUR"); !errors.Is(err, ErrInvalidAccountID) {
			t.Errorf("GetOrCreate(%q) error = %v, want ErrInvalidAccountID", id, err)
		}
	}
}

// TestSGAAccountsAreSeededAsUUIDs, because their ids travel through the
// platform's own config into a URL path that is validated as a UUID two
// services before it reaches this one.
func TestSGAAccountsAreSeededAsUUIDs(t *testing.T) {
	l := NewLedger()
	for _, tc := range []struct{ id, currency string }{
		{SGAAccountEUR, "EUR"},
		{SGAAccountGBP, "GBP"},
	} {
		if !ValidAccountID(tc.id) {
			t.Errorf("%s is not a valid account id", tc.id)
		}
		acc, err := l.Get(tc.id)
		if err != nil {
			t.Fatalf("Get(%s): %v", tc.id, err)
		}
		if acc.Currency != tc.currency || acc.Balance != "0.00" {
			t.Errorf("seeded SGA = %+v, want %s at 0.00", acc, tc.currency)
		}
	}
}

// TestAccountIDForIsStableAndPassesThrough. Two processes that never talk
// to each other -- B4B naming the creditor of a payout, and the e2e suite
// checking that merchant's balance afterwards -- have to land on the same
// account from the same merchant key, or the money is provably somewhere
// but not findable.
func TestAccountIDForIsStableAndPassesThrough(t *testing.T) {
	a, b := AccountIDFor("GB00SIM00000000000042"), AccountIDFor("GB00SIM00000000000042")
	if a != b {
		t.Errorf("AccountIDFor is not stable: %s vs %s", a, b)
	}
	if !ValidAccountID(a) {
		t.Errorf("AccountIDFor produced %q, which is not a UUID", a)
	}
	if other := AccountIDFor("GB00SIM00000000000043"); other == a {
		t.Errorf("two merchants derived the same account id: %s", a)
	}
	// An id that is already an account id is that account, not the seed of
	// a different one -- the platform knowing a real account id is the
	// normal case, and re-deriving would pay someone else.
	if got := AccountIDFor(SGAAccountEUR); got != SGAAccountEUR {
		t.Errorf("AccountIDFor(%s) = %s, want it unchanged", SGAAccountEUR, got)
	}
}

// TestBalancesSplitIntoTheDaysOpeningAndItsMovement: Banking Circle reports a
// balance as beginOfDayAmount plus intraDayAmount, and a platform sums them.
// The lab used to put the whole balance in both, so a safeguarding account
// funded with X read as 2X and a balance check passed on half the money.
func TestBalancesSplitIntoTheDaysOpeningAndItsMovement(t *testing.T) {
	defer labclock.Set(0)
	l := NewLedger()
	acct, err := l.GetOrCreate("00000000-0000-4000-8000-0000000005a1", "EUR")
	if err != nil {
		t.Fatal(err)
	}
	day := func() string { return FinancialDay(labclock.Now()) }
	check := func(wantOpen, wantMoved int64) {
		t.Helper()
		a, err := l.Get(acct.ID)
		if err != nil {
			t.Fatal(err)
		}
		open, moved := a.DayAmounts(day())
		if open != wantOpen || moved != wantMoved || open+moved != a.cents {
			t.Fatalf("open=%d moved=%d balance=%d, want open=%d moved=%d", open, moved, a.cents, wantOpen, wantMoved)
		}
	}
	check(0, 0)
	if _, err := l.Credit(acct.ID, 959594374); err != nil {
		t.Fatal(err)
	}
	check(0, 959594374) // funded today: all of it moved today
	if _, err := l.Credit(acct.ID, 100); err != nil {
		t.Fatal(err)
	}
	check(0, 959594474)
	labclock.Set(24 * time.Hour) // the next day: yesterday's balance is the opening
	check(959594474, 0)
	if _, err := l.Credit(acct.ID, 50); err != nil {
		t.Fatal(err)
	}
	check(959594474, 50)
}
