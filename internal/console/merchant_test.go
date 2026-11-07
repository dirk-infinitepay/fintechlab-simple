package console

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNewRegistrySeedsAHierarchy(t *testing.T) {
	r, err := NewRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Distributors()) != 1 {
		t.Fatalf("distributors = %d, want 1", len(r.Distributors()))
	}
	p := r.Partners()
	if len(p) != 1 || p[0].DistributorID != r.Distributors()[0].ID {
		t.Fatalf("partner not hung off the seeded distributor: %+v", p)
	}
}

func TestAddMerchantFillsInEverythingItWasNotTold(t *testing.T) {
	r, _ := NewRegistry("")
	m, err := r.AddMerchant(NewMerchantParams{LegalName: "Quiet Coffee Ltd"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Country == "" || m.Currency == "" || m.MCC == "" || m.Email == "" {
		t.Fatalf("blank derived fields: %+v", m)
	}
	if m.Address.Line1 == "" || m.Address.City == "" || m.Address.Country != m.Country {
		t.Fatalf("address not generated in the merchant's own country: %+v", m.Address)
	}
	// A merchant that trades nowhere cannot be paid, so "create" opens one
	// outlet rather than none.
	if len(m.Outlets) != 1 {
		t.Fatalf("outlets = %d, want 1", len(m.Outlets))
	}
	o := m.Outlets[0]
	if o.MID == "" || o.AccountNumber == "" || o.FinancialInstitution == "" {
		t.Fatalf("outlet missing payout identity: %+v", o)
	}
	if o.BeneficiaryID != "" {
		t.Fatal("a new outlet must not claim to be known to the payout rail")
	}
}

func TestAddMerchantRequiresANameAndBoundsOutlets(t *testing.T) {
	r, _ := NewRegistry("")
	if _, err := r.AddMerchant(NewMerchantParams{}); err == nil {
		t.Fatal("a nameless merchant was accepted")
	}
	if _, err := r.AddMerchant(NewMerchantParams{LegalName: "X", Outlets: 999}); err == nil {
		t.Fatal("999 outlets was accepted")
	}
}

func TestOutletMIDsAreUniqueAcrossMerchants(t *testing.T) {
	r, _ := NewRegistry("")
	seen := map[string]string{}
	for _, name := range []string{"A Ltd", "B Ltd", "C Ltd"} {
		m, err := r.AddMerchant(NewMerchantParams{LegalName: name, Outlets: 4})
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range m.Outlets {
			// A collision here pays two different outlets into one
			// account, which is the exact failure the per-MID split
			// exists to prevent.
			if prev, dup := seen[o.MID]; dup {
				t.Fatalf("MID %s issued twice: %s and %s", o.MID, prev, o.ID)
			}
			seen[o.MID] = o.ID
		}
	}
}

func TestExplicitFieldsWin(t *testing.T) {
	r, _ := NewRegistry("")
	m, err := r.AddMerchant(NewMerchantParams{
		LegalName: "Given Ltd", Country: "pl", Currency: "eur", MCC: "1234",
		Address: &Address{Line1: "1 Real Street", City: "Krakow", PostCode: "30-001"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.Country != "PL" || m.Currency != "EUR" || m.MCC != "1234" {
		t.Fatalf("supplied fields were overwritten: %+v", m)
	}
	if m.Address.Line1 != "1 Real Street" || m.Address.Country != "PL" {
		t.Fatalf("supplied address not kept and completed: %+v", m.Address)
	}
}

func TestRegistrySurvivesAReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "registry.json")
	r, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	m, err := r.AddMerchant(NewMerchantParams{LegalName: "Persisted Ltd", Outlets: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetOutletBeneficiary(m.Outlets[0].MID, "ben_1", "pass"); err != nil {
		t.Fatal(err)
	}

	again, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := again.Merchant(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Outlets) != 2 || got.Outlets[0].BeneficiaryID != "ben_1" {
		t.Fatalf("reloaded merchant lost state: %+v", got)
	}
	// Ids must not restart, or a second session issues MIDs that collide
	// with the first session's.
	next, err := again.AddMerchant(NewMerchantParams{LegalName: "Later Ltd"})
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == m.ID {
		t.Fatalf("id sequence restarted: %s reused", next.ID)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("registry file not written: %v", err)
	}
}

func TestUnknownIDsReportNotFound(t *testing.T) {
	r, _ := NewRegistry("")
	if _, err := r.Merchant("mer_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Merchant err = %v, want ErrNotFound", err)
	}
	if _, _, err := r.AddOutlet("mer_nope", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AddOutlet err = %v, want ErrNotFound", err)
	}
	if err := r.SetOutletBeneficiary("WL000000000000", "b", "pass"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetOutletBeneficiary err = %v, want ErrNotFound", err)
	}
	if _, err := r.SetStatus("mer_nope", "active"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetStatus err = %v, want ErrNotFound", err)
	}
	if _, err := r.AddMerchant(NewMerchantParams{LegalName: "X", PartnerID: "prt_nope"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("AddMerchant with a bogus partner err = %v, want ErrNotFound", err)
	}
}

func TestSetStatusRejectsInventedStates(t *testing.T) {
	r, _ := NewRegistry("")
	m, _ := r.AddMerchant(NewMerchantParams{LegalName: "Statused Ltd"})
	if _, err := r.SetStatus(m.ID, "frozen"); err == nil {
		t.Fatal("an invented status was accepted")
	}
	if _, err := r.SetStatus(m.ID, "suspended"); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Merchant(m.ID)
	if got.Status != "suspended" {
		t.Fatalf("status = %q", got.Status)
	}
}

func TestClearLeavesOnlyWhatAFirstStartSeeds(t *testing.T) {
	r, err := NewRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	first := r.Partners()
	if _, err := r.AddMerchant(NewMerchantParams{LegalName: "Acme Ltd", Country: "GB", Outlets: 2}); err != nil {
		t.Fatal(err)
	}
	m, _, err := r.Clear()
	if err != nil || m != 1 {
		t.Fatalf("cleared %d merchants, %v", m, err)
	}
	if len(r.Merchants()) != 0 || len(r.Partners()) != len(first) || r.Partners()[0].ID != first[0].ID {
		t.Errorf("after clear: %d merchants, partners %v; want none and %v", len(r.Merchants()), r.Partners(), first)
	}
}
