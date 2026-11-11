package worldline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func seededStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore()
	for _, tx := range []Transaction{
		{MID: "MID-001", Currency: "EUR", AmountCents: 10000, Date: "2026-09-03"},
		{MID: "MID-001", Currency: "EUR", AmountCents: 5000, Date: "2026-09-03"},
		{MID: "MID-001", Currency: "GBP", AmountCents: 2500, Date: "2026-09-03"},
		{MID: "MID-002", Currency: "EUR", AmountCents: 7000, Date: "2026-09-03"},
		{MID: "MID-001", Currency: "EUR", AmountCents: 999, Date: "2026-08-01"},
	} {
		if _, err := s.Add(tx); err != nil {
			t.Fatalf("Add(%v): %v", tx, err)
		}
	}
	return s
}

func TestStoreRejectsIncompleteTransactions(t *testing.T) {
	s := NewStore()
	for name, tx := range map[string]Transaction{
		"no mid":      {Currency: "EUR", AmountCents: 100, Date: "2026-09-03"},
		"no currency": {MID: "MID-001", AmountCents: 100, Date: "2026-09-03"},
		"zero amount": {MID: "MID-001", Currency: "EUR", Date: "2026-09-03"},
		"bad date":    {MID: "MID-001", Currency: "EUR", AmountCents: 100, Date: "03/09/2026"},
	} {
		if _, err := s.Add(tx); err == nil {
			t.Errorf("Add(%s) succeeded, want an error", name)
		}
	}
	if got := len(s.All()); got != 0 {
		t.Fatalf("store holds %d transactions after only-invalid adds, want 0", got)
	}
}

func TestStoreGroupsByMIDAndCurrencyWithinRange(t *testing.T) {
	s := seededStore(t)

	mids := s.MIDsInRange("2026-09-01", "2026-09-03")
	if len(mids) != 2 || mids[0] != "MID-001" || mids[1] != "MID-002" {
		t.Fatalf("MIDsInRange = %v, want [MID-001 MID-002]", mids)
	}
	// The August transaction is outside the range: its MID must still
	// appear (it also traded in range) but its money must not.
	ccys := s.CurrenciesInRange("MID-001", "2026-09-01", "2026-09-03")
	if len(ccys) != 2 || ccys[0] != "EUR" || ccys[1] != "GBP" {
		t.Fatalf("CurrenciesInRange = %v, want [EUR GBP]", ccys)
	}
	if got := len(s.Range("MID-001", "2026-09-01", "2026-09-03")); got != 3 {
		t.Fatalf("Range returned %d transactions, want 3 (the August one is out of range)", got)
	}
}

func TestCutProducesOneFilePerCurrencyCoveringEveryMID(t *testing.T) {
	at := time.Date(2026, 9, 4, 8, 42, 0, 0, time.UTC)
	files, err := Cut(seededStore(t), CycleConfig{Identifier: "Worldline_Settlement"},
		SlotMorning, "2026-09-01", "2026-09-03", at)
	if err != nil {
		t.Fatalf("Cut: %v", err)
	}
	// Two currencies traded, so two files -- not one per merchant. The
	// filename pattern carries no merchant, so a per-merchant cut would
	// collide and silently overwrite.
	if len(files) != 2 {
		t.Fatalf("Cut produced %d files, want 2 (one per currency)", len(files))
	}
	byCcy := map[string]CutFile{}
	for _, f := range files {
		if _, dup := byCcy[f.Currency]; dup {
			t.Fatalf("two files cut for currency %s", f.Currency)
		}
		byCcy[f.Currency] = f
	}
	eur := byCcy["EUR"]
	// MID-001's 15000 plus MID-002's 7000; the out-of-range 999 is
	// excluded.
	if eur.AmountCents != 22000 {
		t.Fatalf("EUR file settles %d, want 22000 across both submerchants", eur.AmountCents)
	}
	if eur.Items != 3 {
		t.Fatalf("EUR file has %d items, want 3", eur.Items)
	}
	if len(eur.MIDs) != 2 || eur.MIDs[0] != "MID-001" || eur.MIDs[1] != "MID-002" {
		t.Fatalf("EUR file covers %v, want both MID-001 and MID-002", eur.MIDs)
	}
	if got := byCcy["GBP"].AmountCents; got != 2500 {
		t.Fatalf("GBP file settles %d, want 2500", got)
	}
	if !strings.Contains(eur.Filename, "_ER_EUR.csv") {
		t.Fatalf("filename %q does not carry the morning slot and currency", eur.Filename)
	}
	if strings.Contains(eur.Filename, "MID-") {
		t.Fatalf("filename %q names a merchant; a settlement file is per currency", eur.Filename)
	}
	if eur.Filename == byCcy["GBP"].Filename {
		t.Fatal("the two currencies' files share a filename: one would overwrite the other")
	}
}

func TestCutRoundTripsThroughTheParser(t *testing.T) {
	at := time.Date(2026, 9, 4, 8, 42, 0, 0, time.UTC)
	files, err := Cut(seededStore(t), CycleConfig{Identifier: "Worldline_Settlement"},
		SlotMorning, "2026-09-01", "2026-09-03", at)
	if err != nil {
		t.Fatalf("Cut: %v", err)
	}
	var eur CutFile
	for _, f := range files {
		if f.Currency == "EUR" {
			eur = f
		}
	}
	parsed, err := ParseBamboraCSV(strings.NewReader(string(eur.Bytes)))
	if err != nil {
		t.Fatalf("ParseBamboraCSV: %v", err)
	}
	if parsed.Meta.SettlementAmount != eur.AmountCents {
		t.Fatalf("parsed settlement amount %d != generated %d", parsed.Meta.SettlementAmount, eur.AmountCents)
	}
	if parsed.Meta.NumberOfItems != eur.Items {
		t.Fatalf("parsed item count %d != generated %d", parsed.Meta.NumberOfItems, eur.Items)
	}
	// One batch per submerchant, both in the one file.
	if len(parsed.Batches) != 2 {
		t.Fatalf("parsed %d batches, want one per submerchant", len(parsed.Batches))
	}
	// The per-outlet split the platform pays against comes from
	// ADDITIONAL_REF_2, never BAMBORA_MID.
	payouts := parsed.PayoutsByMID()
	if len(payouts) != 2 || payouts["MID-001"] != 15000 || payouts["MID-002"] != 7000 {
		t.Fatalf("PayoutsByMID = %v, want {MID-001: 15000, MID-002: 7000}", payouts)
	}
	var sum int64
	for _, v := range payouts {
		sum += v
	}
	if sum != parsed.Meta.SettlementAmount {
		t.Fatalf("per-MID totals sum to %d but the file settles %d -- the lump sum would not reconcile",
			sum, parsed.Meta.SettlementAmount)
	}
}

// ParseSettlementFile reads what Cut wrote, with the MIDs Cut named, and
// refuses what is not a settlement file where ParseBamboraCSV would read it
// as an empty one.
func TestParseSettlementFileRefusesWhatIsNotOne(t *testing.T) {
	files, err := Cut(seededStore(t), CycleConfig{Identifier: "Worldline_Settlement"},
		SlotMorning, "2026-09-03", "2026-09-03", time.Date(2026, 9, 4, 8, 42, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		parsed, err := ParseSettlementFile(strings.NewReader(string(f.Bytes)))
		if err != nil {
			t.Fatalf("%s: %v", f.Filename, err)
		}
		if got, want := strings.Join(parsed.MIDs(), ","), strings.Join(f.MIDs, ","); got != want {
			t.Errorf("%s: MIDs() = %s, want the cut's %s", f.Filename, got, want)
		}
	}

	headerOnly := `"VERSION_NUMBER","RECORD_TYPE","SETTLEMENT_AMOUNT","SETTLEMENT_CURRENCY","VALUE_DATE","NUMBER_OF_ITEMS","TO_ACCOUNT","PAYMENT_REFERENCE"` + "\n"
	noCurrency := headerOnly + `"V2.10","Settlement","1.00","","2026-09-03","1","ACC","REF"` + "\n"
	for name, body := range map[string]string{
		"plain text":        "hello, this is not a settlement file\n",
		"empty":             "",
		"headers only":      headerOnly,
		"no currency":       noCurrency,
		"a third decimal":   strings.Replace(noCurrency, `"1.00","",`, `"1.005","EUR",`, 1),
		"an unclosed quote": `"V2.10,"Settlement"`,
	} {
		if _, err := ParseSettlementFile(strings.NewReader(body)); err == nil {
			t.Errorf("%s: parsed as a settlement file", name)
		}
	}
	if _, err := ParseBamboraCSV(strings.NewReader("hello\n")); err != nil {
		t.Errorf("ParseBamboraCSV is lenient and stays so: %v", err)
	}
}

func TestCutFromFixturesServesBytesVerbatim(t *testing.T) {
	dir := t.TempDir()
	morning := "20260904084200_Worldline_Settlement_ER_EUR.csv"
	afternoon := "20260904154200_Worldline_Settlement_AR_EUR.csv"
	content := []byte("\"VERSION_NUMBER\",\"RECORD_TYPE\"\n\"1\",\"ST\"\n")
	for _, n := range []string{morning, afternoon} {
		if err := os.WriteFile(filepath.Join(dir, n), content, 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	cfg := CycleConfig{Identifier: "ignored", Source: FileSourceFixture, FixtureDir: dir}

	files, err := Cut(NewStore(), cfg, SlotMorning, "2026-09-03", "2026-09-03", time.Now())
	if err != nil {
		t.Fatalf("Cut: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("morning cut returned %d files, want only the ER one", len(files))
	}
	// The fixture's own name survives: a real example file's filename is
	// part of what a consumer's validator is being tested against.
	if files[0].Filename != morning {
		t.Fatalf("filename = %q, want the fixture's own name %q", files[0].Filename, morning)
	}
	if string(files[0].Bytes) != string(content) {
		t.Fatalf("fixture bytes were rewritten:\n%q", files[0].Bytes)
	}

	files, err = Cut(NewStore(), cfg, SlotAfternoon, "2026-09-03", "2026-09-03", time.Now())
	if err != nil {
		t.Fatalf("Cut afternoon: %v", err)
	}
	if len(files) != 1 || files[0].Filename != afternoon {
		t.Fatalf("afternoon cut = %v, want only %q", files, afternoon)
	}
}

func TestFilenameSlot(t *testing.T) {
	cases := map[string]string{
		"20260904084200_Worldline_Settlement_ER_EUR.csv":     SlotMorning,
		"20260904154200_Worldline_Settlement_AR_EUR.csv.pgp": SlotAfternoon,
		"20260904154200_Worldline_Settlement_XX_EUR.csv":     "",
		"nonsense.csv": "",
	}
	for name, want := range cases {
		if got := FilenameSlot(name); got != want {
			t.Errorf("FilenameSlot(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestSlotName(t *testing.T) {
	for in, want := range map[string]string{
		"morning": SlotMorning, "ER": SlotMorning, "": SlotMorning,
		"afternoon": SlotAfternoon, "confirmation": SlotAfternoon, "ar": SlotAfternoon,
	} {
		got, err := SlotName(in)
		if err != nil || got != want {
			t.Errorf("SlotName(%q) = %q, %v; want %q, nil", in, got, err, want)
		}
	}
	if _, err := SlotName("evening"); err == nil {
		t.Error("SlotName(evening) succeeded, want an error")
	}
}

func TestNextFirePicksWhicheverSlotComesFirst(t *testing.T) {
	cfg := CycleConfig{MorningAt: "08:30", AfternoonAt: "15:30"}
	// Before both: the morning file is next.
	at, slot := cfg.NextFire(time.Date(2026, 9, 4, 6, 0, 0, 0, time.UTC))
	if slot != SlotMorning || at.Hour() != 8 {
		t.Fatalf("at 06:00 next fire = %v %s, want the same day's morning slot", at, slot)
	}
	// Between them: the afternoon confirmation is next.
	at, slot = cfg.NextFire(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	if slot != SlotAfternoon || at.Hour() != 15 {
		t.Fatalf("at 12:00 next fire = %v %s, want the same day's afternoon slot", at, slot)
	}
	// After both: tomorrow morning.
	at, slot = cfg.NextFire(time.Date(2026, 9, 4, 20, 0, 0, 0, time.UTC))
	if slot != SlotMorning || at.Day() != 5 {
		t.Fatalf("at 20:00 next fire = %v %s, want tomorrow's morning slot", at, slot)
	}
}

func TestCoverageIsThePreviousDay(t *testing.T) {
	cfg := CycleConfig{}
	from, to := cfg.CoverageFor(time.Date(2026, 9, 4, 8, 30, 0, 0, time.UTC))
	if from != "2026-09-03" || to != "2026-09-03" {
		t.Fatalf("CoverageFor = %s..%s, want 2026-09-03 twice (Worldline settles T+1)", from, to)
	}
}

func TestMorningJitterIsDeterministicAndWithinWindow(t *testing.T) {
	cfg := CycleConfig{MorningAt: "08:00", AfternoonAt: "15:30", MorningJitter: 2 * time.Hour}
	now := time.Date(2026, 9, 4, 6, 0, 0, 0, time.UTC)
	first, _ := cfg.NextFire(now)
	second, _ := cfg.NextFire(now)
	if !first.Equal(second) {
		t.Fatalf("jitter is not deterministic: %v vs %v", first, second)
	}
	base := time.Date(2026, 9, 4, 8, 0, 0, 0, time.UTC)
	if first.Before(base) || !first.Before(base.Add(2*time.Hour)) {
		t.Fatalf("morning fire %v is outside the 08:00-10:00 window", first)
	}
}

func TestFilenameTimestampComesFromDeliveryTimeNotCoverage(t *testing.T) {
	store := seededStore(t)
	cfg := CycleConfig{Identifier: "Worldline_Settlement"}

	// Same coverage window, two different delivery times. The filenames
	// must differ: a real acquirer delivering the same day's settlement
	// twice does not reuse a filename, and a consumer that tracks
	// already-seen files by name would ignore the second delivery
	// entirely if it did.
	first, err := Cut(store, cfg, SlotMorning, "2026-09-01", "2026-09-03",
		time.Date(2026, 9, 4, 8, 42, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Cut: %v", err)
	}
	second, err := Cut(store, cfg, SlotMorning, "2026-09-01", "2026-09-03",
		time.Date(2026, 9, 4, 9, 15, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Cut: %v", err)
	}
	if len(first) == 0 || len(first) != len(second) {
		t.Fatalf("cut %d then %d files, want the same non-zero count", len(first), len(second))
	}
	for i := range first {
		if first[i].Filename == second[i].Filename {
			t.Fatalf("both deliveries produced %q; the timestamp must come from the delivery time", first[i].Filename)
		}
		// The content is the same settlement, though.
		if string(first[i].Bytes) != string(second[i].Bytes) {
			t.Fatalf("the same coverage window produced different content across two deliveries")
		}
	}
}
