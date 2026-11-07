package worldline

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A reset deletes a published file only when its name proves the cycle cut
// it. The name has to be exactly the one Cut gives, under this identifier.
func TestIsCutFilenameAcceptsOnlyWhatCutNames(t *testing.T) {
	at := time.Date(2026, 9, 25, 8, 34, 12, 0, time.UTC)
	for name, want := range map[string]bool{
		BamboraFilename("Worldline_Settlement", at, SlotMorning, "eur"):          true,
		BamboraFilename("Worldline_Settlement", at, SlotAfternoon, "GBP"):        true,
		BamboraFilename("Someone_Else", at, SlotMorning, "EUR"):                  false,
		BamboraFilename("Worldline_Settlement", at, SlotMorning, "EUR") + ".pgp": false, // the cut name, not the published one
		"INFINITEENROLL_01.20260925.pgp":                                         false,
		"2026092508341_Worldline_Settlement_ER_EUR.csv":                          false, // 13 digits
		"20260925083412_Worldline_Settlement_XX_EUR.csv":                         false,
		"notes.csv": false,
	} {
		if got := IsCutFilename("Worldline_Settlement", name); got != want {
			t.Errorf("IsCutFilename(%q) = %v, want %v", name, got, want)
		}
	}
	if IsCutFilename("", "20260925083412__ER_EUR.csv") {
		t.Error("an empty identifier attributed a file")
	}
}

// In fixture mode a fixture's own name is published, so it is one of the
// cycle's names too; a file in the fixture directory that the cycle would
// never serve is not.
func TestCutNamesInFixtureModeKnowsTheFixtures(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"real_bank_ER_export.csv", "README.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ours, err := CycleConfig{Identifier: "Worldline_Settlement", Source: FileSourceFixture, FixtureDir: dir}.CutNames()
	if err != nil {
		t.Fatal(err)
	}
	if !ours("real_bank_ER_export.csv") {
		t.Error("a fixture the cycle publishes is not one of its names")
	}
	if ours("README.txt") {
		t.Error("a fixture the cycle never publishes was attributed to it")
	}
	if !ours("20260925083412_Worldline_Settlement_ER_EUR.csv") {
		t.Error("the generated pattern is not one of its names")
	}

	generate, err := CycleConfig{Identifier: "Worldline_Settlement", Source: FileSourceGenerate, FixtureDir: dir}.CutNames()
	if err != nil {
		t.Fatal(err)
	}
	if generate("real_bank_ER_export.csv") {
		t.Error("in generate mode a fixture's name was attributed to the cycle")
	}
}

func TestStoreClearForgetsTransactionsAndRestartsIDs(t *testing.T) {
	s := NewStore()
	for range 3 {
		if _, err := s.Add(Transaction{MID: "M1", Currency: "eur", AmountCents: 100, Date: "2026-09-24"}); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.Clear(); n != 3 {
		t.Fatalf("Clear = %d, want 3", n)
	}
	if got := s.All(); len(got) != 0 {
		t.Fatalf("after Clear the store holds %d transaction(s)", len(got))
	}
	tx, err := s.Add(Transaction{MID: "M1", Currency: "EUR", AmountCents: 100, Date: "2026-09-24"})
	if err != nil {
		t.Fatal(err)
	}
	if tx.ID != "wltxn_000001" {
		t.Fatalf("first id after Clear = %s, want wltxn_000001: a scenario run again should get the same ids", tx.ID)
	}
}

func TestIsReceiptFilename(t *testing.T) {
	for name, want := range map[string]bool{
		ReceiptFilename(1, "20260925"):               true,
		ReceiptFilename(42, "20260925") + ".partial": true,
		"INFINITEENROLL_01.20260925.pgp":             false, // the platform's batch, never ours
		"INFINITEENROLL_1.20260925.ack.pgp":          false,
		"other.ack.pgp":                              false,
	} {
		if got := IsReceiptFilename(name); got != want {
			t.Errorf("IsReceiptFilename(%q) = %v, want %v", name, got, want)
		}
	}
}
