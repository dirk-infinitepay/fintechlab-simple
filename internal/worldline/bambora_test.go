package worldline

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func sampleBamboraLedger() []Transaction {
	return []Transaction{
		{MID: "GB00SIM0000000000003", Currency: "EUR", AmountCents: 10000, Date: "2026-09-03"},
		{MID: "GB00SIM0000000000003", Currency: "EUR", AmountCents: 5000, Date: "2026-09-03"},
		// debit leg: must not become a TXER row.
		{MID: "GB00SIM0000000000003", Currency: "EUR", AmountCents: -3000, Date: "2026-09-03"},
		// different merchant: must not leak into this file.
		{MID: "GB00SIM0000000000099", Currency: "EUR", AmountCents: 99999, Date: "2026-09-03"},
		// outside the requested date range: must not count.
		{MID: "GB00SIM0000000000003", Currency: "EUR", AmountCents: 2000, Date: "2026-08-01"},
	}
}

func TestBamboraFilenameFormat(t *testing.T) {
	at := time.Date(2026, 9, 4, 13, 5, 9, 0, time.UTC) // 15:05:09 in Paris, CEST
	got := BamboraFilename("Worldline_Settlement", at, "ER", "eur")
	want := "20260904150509_Worldline_Settlement_ER_EUR.csv"
	if got != want {
		t.Fatalf("BamboraFilename = %q, want %q", got, want)
	}
}

func TestBamboraFilenameAfternoonFile(t *testing.T) {
	at := time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC)
	got := BamboraFilename("Worldline_Settlement", at, "AR", "USD")
	want := "20260904200000_Worldline_Settlement_AR_USD.csv"
	if got != want {
		t.Fatalf("BamboraFilename = %q, want %q", got, want)
	}
}

func TestGenerateBamboraMultiSectionStructure(t *testing.T) {
	f := GenerateBambora(sampleBamboraLedger(), "GB00SIM0000000000003", "2026-09-01", "2026-09-03", BamboraOptions{})

	if f.Meta.NumberOfItems != 2 {
		t.Fatalf("Meta.NumberOfItems = %d, want 2", f.Meta.NumberOfItems)
	}
	if f.Meta.SettlementAmount != 15000 {
		t.Fatalf("Meta.SettlementAmount = %d, want 15000", f.Meta.SettlementAmount)
	}
	if f.Meta.SettlementCurrency != "EUR" {
		t.Fatalf("Meta.SettlementCurrency = %q, want EUR", f.Meta.SettlementCurrency)
	}
	if f.Meta.ToAccount != "GB00SIM0000000000003" {
		t.Fatalf("Meta.ToAccount = %q, want the merchant IBAN", f.Meta.ToAccount)
	}
	if len(f.Batches) != 1 {
		t.Fatalf("expected exactly 1 batch, got %d", len(f.Batches))
	}
	batch := f.Batches[0]
	if batch.NumberOfTrans != 2 || len(batch.Transactions) != 2 {
		t.Fatalf("expected 2 transactions in the batch, got NumberOfTrans=%d len=%d", batch.NumberOfTrans, len(batch.Transactions))
	}
	if batch.NetAmount != 15000 || batch.SettlementAmount != 15000 {
		t.Fatalf("batch amounts = net:%d settlement:%d, want 15000 each", batch.NetAmount, batch.SettlementAmount)
	}
	if len(f.Chargebacks) != 0 {
		t.Fatalf("expected no chargebacks (no data source in this lab), got %d", len(f.Chargebacks))
	}

	var buf bytes.Buffer
	if err := f.WriteCSV(&buf); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	// Settlement header + row (2), Batch header + row (2), TXER header + 2
	// rows (3), CB header only, no rows (1) = 8 lines.
	if len(lines) != 8 {
		t.Fatalf("expected 8 lines (settlement x2, batch x2, txer x3, cb header x1), got %d:\n%s", len(lines), buf.String())
	}
	wantMetaHeader := `"VERSION_NUMBER","RECORD_TYPE","SETTLEMENT_AMOUNT","SETTLEMENT_CURRENCY","VALUE_DATE","NUMBER_OF_ITEMS","TO_ACCOUNT","PAYMENT_REFERENCE"`
	if lines[0] != wantMetaHeader {
		t.Fatalf("line 0 = %q, want %q", lines[0], wantMetaHeader)
	}
	if !strings.HasPrefix(lines[1], `"V2.10","Settlement","150.00","EUR",`) {
		t.Fatalf("line 1 (settlement row) = %q", lines[1])
	}
	wantBatchHeader := `"RECORD_TYPE","BAMBORA_MID","BATCH_REF","PAYREF_EXTENDED","NET_AMOUNT","BATCH_CURRENCY","SETTLEMENT_AMOUNT","SETTLEMENT_CURRENCY","NUMBER_OF_TRANS"`
	if lines[2] != wantBatchHeader {
		t.Fatalf("line 2 = %q, want %q", lines[2], wantBatchHeader)
	}
	if !strings.HasPrefix(lines[3], `"Batch",`) || !strings.Contains(lines[3], `"150.00"`) {
		t.Fatalf("line 3 (batch row) = %q, want RECORD_TYPE Batch and a decimal amount", lines[3])
	}
	wantTXERHeader := `"RECORD_TYPE","BAMBORA_MID","SUBMERCHANT_ID","BATCH_REF","TRANSACTION_REF","BAMBORA_REF","ADDITIONAL_REF_1","ADDITIONAL_REF_2","TRANSACTION_TYPE","TRANSACTION_AMOUNT","TRANSACTION_CURRENCY","FX_RATE","SETTLEMENT_AMOUNT","SETTLEMENT_CURRENCY","CARD_SCHEME_NAME","CARD_USAGE","CARD_CATEGORY","INTERCHANGE_DOMAIN","COUNTRY_MERCHANT","COUNTRY_ISSUER","MCC","ECOM_SECURITY_LEVEL","ADDITIONAL_REF_3","CARD_NUMBER_TRUNCATED","TRANSACTION_DATE","TRANSACTION_TIME","CASHBACK_AMOUNT","PAYREF_EXTENDED","TERMINALID"`
	if lines[4] != wantTXERHeader {
		t.Fatalf("line 4 = %q, want %q", lines[4], wantTXERHeader)
	}
	if !strings.HasPrefix(lines[5], `"TXER",`) || !strings.HasPrefix(lines[6], `"TXER",`) {
		t.Fatalf("lines 5-6 (txer rows) = %q, %q", lines[5], lines[6])
	}
	wantCBHeader := `"RECORD_TYPE","BAMBORA_MID","SUBMERCHANT_ID","ORIGINAL_TRANSACTION_REF","BAMBORA_REF","DISPUTE_TRANSACTION_AMOUNT","DISPUTE_CURRENCY","DISPUTE_SETTLEMENT_AMOUNT","DISPUTE_SETTLEMENT_CURRENCY","DISPUTE_REASON_CODE","DISPUTE_REGISTRATION_DATE","DISPUTE_FEE","DISPUTE_FEE_CURRENCY","ORIGINAL_TRANSACTION_ADDITIONAL_REF_1"`
	if lines[7] != wantCBHeader {
		t.Fatalf("line 7 = %q, want %q", lines[7], wantCBHeader)
	}
}

// TestGenerateBamboraMerchantKeyIsAdditionalRef2 is a regression test for
// the exact mistake docs/ARCHITECTURE-vendor-corrections.md section 1
// calls out: the real merchant grouping key for TXER rows is
// ADDITIONAL_REF_2, not BAMBORA_MID (one BAMBORA_MID can span multiple
// merchants in the real file) and not SUBMERCHANT_ID either (that column
// keys CB rows, not TXER). A naive implementation might alias
// ADDITIONAL_REF_2 to the synthesized BAMBORA_MID/SUBMERCHANT_ID values,
// or vice versa -- this asserts that never happens.
func TestGenerateBamboraMerchantKeyIsAdditionalRef2(t *testing.T) {
	const merchantID = "GB00SIM0000000000003"
	f := GenerateBambora(sampleBamboraLedger(), merchantID, "2026-09-01", "2026-09-03", BamboraOptions{})

	if len(f.Batches) != 1 || len(f.Batches[0].Transactions) == 0 {
		t.Fatalf("expected at least one TXER row, got %+v", f.Batches)
	}
	batch := f.Batches[0]
	for _, txn := range batch.Transactions {
		if txn.AdditionalRef2 != merchantID {
			t.Fatalf("TXER.AdditionalRef2 = %q, want the real merchant id %q (this is the grouping key, not BAMBORA_MID)", txn.AdditionalRef2, merchantID)
		}
		if txn.BamboraMID == merchantID {
			t.Fatalf("TXER.BamboraMID must NOT equal the merchant id %q -- BAMBORA_MID only keys batches and can span multiple merchants, it must never be used as (or confused with) the merchant key", merchantID)
		}
		if txn.SubmerchantID == merchantID {
			t.Fatalf("TXER.SubmerchantID must NOT equal the merchant id %q -- SUBMERCHANT_ID is the CB rows' merchant key, not TXER's", merchantID)
		}
	}
	// The batch's own BAMBORA_MID must match every one of its TXER rows'
	// BAMBORA_MID (it's a real, consistent batch identifier) while still
	// staying distinct from the merchant id.
	if batch.BamboraMID == "" {
		t.Fatal("batch.BamboraMID must not be empty")
	}
	for _, txn := range batch.Transactions {
		if txn.BamboraMID != batch.BamboraMID {
			t.Fatalf("TXER.BamboraMID = %q, want it to match its batch's BamboraMID %q", txn.BamboraMID, batch.BamboraMID)
		}
	}
}

func TestGenerateBamboraDeterministicOutput(t *testing.T) {
	f1 := GenerateBambora(sampleBamboraLedger(), "GB00SIM0000000000003", "2026-09-01", "2026-09-03", BamboraOptions{})
	f2 := GenerateBambora(sampleBamboraLedger(), "GB00SIM0000000000003", "2026-09-01", "2026-09-03", BamboraOptions{})

	var buf1, buf2 bytes.Buffer
	if err := f1.WriteCSV(&buf1); err != nil {
		t.Fatal(err)
	}
	if err := f2.WriteCSV(&buf2); err != nil {
		t.Fatal(err)
	}
	if buf1.String() != buf2.String() {
		t.Fatalf("GenerateBambora is not deterministic: two runs on identical input produced different output:\n--- run 1 ---\n%s\n--- run 2 ---\n%s", buf1.String(), buf2.String())
	}
	if buf1.Len() == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestGenerateBamboraEmptyLedgerStillWritesHeaders(t *testing.T) {
	f := GenerateBambora(nil, "GB00SIM0000000000003", "2026-09-01", "2026-09-03", BamboraOptions{})
	if len(f.Batches) != 0 {
		t.Fatalf("expected no batches for an empty ledger, got %d", len(f.Batches))
	}
	if f.Meta.NumberOfItems != 0 {
		t.Fatalf("Meta.NumberOfItems = %d, want 0", f.Meta.NumberOfItems)
	}

	var buf bytes.Buffer
	if err := f.WriteCSV(&buf); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	// Settlement header + row, CB header only: no batches at all.
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines (settlement header+row, cb header), got %d:\n%s", len(lines), buf.String())
	}
}

func TestBamboraOptionsDefaults(t *testing.T) {
	f := GenerateBambora(sampleBamboraLedger(), "GB00SIM0000000000003", "2026-09-01", "2026-09-03", BamboraOptions{})
	if f.Meta.VersionNumber != "V2.10" {
		t.Fatalf("VersionNumber default = %q, want %q", f.Meta.VersionNumber, "V2.10")
	}
	if f.Meta.ValueDate != "2026-09-03" {
		t.Fatalf("ValueDate default = %q, want toDate", f.Meta.ValueDate)
	}
	if f.Meta.PaymentReference == "" {
		t.Fatal("expected a non-empty default PaymentReference")
	}

	f2 := GenerateBambora(sampleBamboraLedger(), "GB00SIM0000000000003", "2026-09-01", "2026-09-03", BamboraOptions{
		ValueDate:        "2026-09-05",
		PaymentReference: "custom-ref",
		VersionNumber:    "2",
	})
	if f2.Meta.ValueDate != "2026-09-05" || f2.Meta.PaymentReference != "custom-ref" || f2.Meta.VersionNumber != "2" {
		t.Fatalf("options overrides not applied: %+v", f2.Meta)
	}
}

// buddySampleExcerpt is the head of buddy's Worldline sample
// (apps/settle-integration-testing/support/fixtures/worldline-sample.csv) --
// the format this lab renders and buddy parses. Synthetic data only.
const buddySampleExcerpt = `VERSION_NUMBER,RECORD_TYPE,SETTLEMENT_AMOUNT,SETTLEMENT_CURRENCY,VALUE_DATE,NUMBER_OF_ITEMS,TO_ACCOUNT,PAYMENT_REFERENCE
V2.10,Settlement,6660.27,EUR,2024-05-01,0,acc124,PaymentReferenceXYZ
RECORD_TYPE,BAMBORA_MID,BATCH_REF,PAYREF_EXTENDED,NET_AMOUNT,BATCH_CURRENCY,SETTLEMENT_AMOUNT,SETTLEMENT_CURRENCY,NUMBER_OF_TRANS
Batch,64567892,0,,20637,EUR,20637,EUR,7
RECORD_TYPE,BAMBORA_MID,SUBMERCHANT_ID,BATCH_REF,TRANSACTION_REF,BAMBORA_REF,ADDITIONAL_REF_1,ADDITIONAL_REF_2,TRANSACTION_TYPE,TRANSACTION_AMOUNT,TRANSACTION_CURRENCY,FX_RATE,SETTLEMENT_AMOUNT,SETTLEMENT_CURRENCY,CARD_SCHEME_NAME,CARD_USAGE,CARD_CATEGORY,INTERCHANGE_DOMAIN,COUNTRY_MERCHANT,COUNTRY_ISSUER,MCC,ECOM_SECURITY_LEVEL,ADDITIONAL_REF_3,CARD_NUMBER_TRUNCATED,TRANSACTION_DATE,TRANSACTION_TIME,CASHBACK_AMOUNT,PAYREF_EXTENDED,TERMINALID
TXER,64567892,submten,0,240430000505,x,Order123,5,Sale with cash back,917.00,EUR,1,917.00,EUR,Visa,Debit,Consumer,Intraregional,528,250,8398,5,,411111xxxxxx1111,2024-04-30,8.0:28,0.00,,6528154
TXER,64567892,submten,0,240430000506,x,Order123,5,Sale,0.5,EUR,1,0.5,EUR,Mastercard,Credit,Commercial,Intraregional,528,250,8398,6,,511111xxxxxx1111,2024-04-30,8.0000000,0.00,,6528154
RECORD_TYPE,BAMBORA_MID,SUBMERCHANT_ID,ORIGINAL_TRANSACTION_REF,BAMBORA_REF,DISPUTE_TRANSACTION_AMOUNT,DISPUTE_CURRENCY,DISPUTE_SETTLEMENT_AMOUNT,DISPUTE_SETTLEMENT_CURRENCY,DISPUTE_REASON_CODE,DISPUTE_REGISTRATION_DATE,DISPUTE_FEE,DISPUTE_FEE_CURRENCY,ORIGINAL_TRANSACTION_ADDITIONAL_REF_1
CB,64567891,1,101812863316,V2024122-911111,99.00,EUR,-99.00,EUR,13.5,05-01-24,0.00,EUR,Order123
`

func TestParseBuddySampleReadsMajorUnits(t *testing.T) {
	f, err := ParseBamboraCSV(strings.NewReader(buddySampleExcerpt))
	if err != nil {
		t.Fatal(err)
	}
	if f.Meta.SettlementAmount != 666027 || f.Meta.VersionNumber != "V2.10" {
		t.Fatalf("meta = %+v, want 666027 minor units and V2.10", f.Meta)
	}
	if len(f.Batches) != 1 || f.Batches[0].NetAmount != 2063700 {
		t.Fatalf("batches = %+v, want one batch of 2063700 minor units (a whole-unit 20637)", f.Batches)
	}
	txns := f.Batches[0].Transactions
	if len(txns) != 2 || txns[0].SettlementAmount != 91700 || txns[1].SettlementAmount != 50 {
		t.Fatalf("transactions = %+v, want 91700 and 50 minor units", txns)
	}
	if len(f.Chargebacks) != 1 || f.Chargebacks[0].DisputeSettlementAmount != -9900 {
		t.Fatalf("chargebacks = %+v, want one of -9900 minor units", f.Chargebacks)
	}
}

func TestGeneratedFileMatchesBuddySampleVocabulary(t *testing.T) {
	f := GenerateSettlementFile(sampleBamboraLedger(), "EUR", "2026-09-01", "2026-09-03", BamboraOptions{})
	categories := map[string]bool{"Consumer": true, "Commercial": true}
	levels := map[string]bool{"0": true, "1": true, "2": true, "5": true, "6": true, "7": true}
	for _, b := range f.Batches {
		for _, txn := range b.Transactions {
			if !categories[txn.CardCategory] {
				t.Fatalf("CARD_CATEGORY %q is not Consumer/Commercial", txn.CardCategory)
			}
			if !levels[txn.EcomSecurityLevel] {
				t.Fatalf("ECOM_SECURITY_LEVEL %q is not a numeric indicator", txn.EcomSecurityLevel)
			}
		}
	}
}

func TestRefundBecomesANegativeRow(t *testing.T) {
	ledger := append(sampleBamboraLedger(), Transaction{
		MID: "GB00SIM0000000000003", Currency: "EUR", AmountCents: -1234, Date: "2026-09-03", Type: "Refund",
	})
	f := GenerateBambora(ledger, "GB00SIM0000000000003", "2026-09-01", "2026-09-03", BamboraOptions{})

	if f.Meta.NumberOfItems != 3 || f.Meta.SettlementAmount != 15000-1234 {
		t.Fatalf("meta = %+v, want 3 items netting to %d", f.Meta, 15000-1234)
	}
	var buf bytes.Buffer
	if err := f.WriteCSV(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"Refund","-12.34","EUR","1","-12.34"`) {
		t.Fatalf("expected a refund row with -12.34, got:\n%s", buf.String())
	}
}

func TestRenderParseRoundTripKeepsEveryCent(t *testing.T) {
	ledger := []Transaction{
		{MID: "5", Currency: "EUR", AmountCents: 1, Date: "2026-09-03"},
		{MID: "5", Currency: "EUR", AmountCents: 100, Date: "2026-09-03"},
		{MID: "5", Currency: "EUR", AmountCents: 1234567, Date: "2026-09-03"},
		{MID: "6", Currency: "EUR", AmountCents: -250, Date: "2026-09-03", Type: "Refund"},
	}
	f := GenerateSettlementFile(ledger, "EUR", "2026-09-03", "2026-09-03", BamboraOptions{})
	var buf bytes.Buffer
	if err := f.WriteCSV(&buf); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseBamboraCSV(&buf)
	if err != nil {
		t.Fatal(err)
	}
	payouts := parsed.PayoutsByMID()
	if payouts["5"] != 1234668 || payouts["6"] != -250 || parsed.Meta.SettlementAmount != 1234418 {
		t.Fatalf("payouts = %v, meta = %d; want 5:1234668 6:-250 total 1234418", payouts, parsed.Meta.SettlementAmount)
	}
}

func TestParseLegacyFileKeepsMinorUnits(t *testing.T) {
	legacy := `"VERSION_NUMBER","RECORD_TYPE","SETTLEMENT_AMOUNT","SETTLEMENT_CURRENCY","VALUE_DATE","NUMBER_OF_ITEMS","TO_ACCOUNT","PAYMENT_REFERENCE"
"1","ST","2923490","EUR","2026-09-29","1","ACC","REF"
"RECORD_TYPE","BAMBORA_MID","BATCH_REF","PAYREF_EXTENDED","NET_AMOUNT","BATCH_CURRENCY","SETTLEMENT_AMOUNT","SETTLEMENT_CURRENCY","NUMBER_OF_TRANS"
"BT","MID1","B1","REF","2923490","EUR","2923490","EUR","1"
`
	f, err := ParseBamboraCSV(strings.NewReader(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if f.Meta.SettlementAmount != 2923490 || len(f.Batches) != 1 || f.Batches[0].NetAmount != 2923490 {
		t.Fatalf("legacy amounts must stay minor units: meta %+v batches %+v", f.Meta, f.Batches)
	}
}

func TestParseRejectsAThirdDecimal(t *testing.T) {
	bad := strings.Replace(buddySampleExcerpt, "6660.27", "6660.275", 1)
	if _, err := ParseBamboraCSV(strings.NewReader(bad)); err == nil {
		t.Fatal("expected an error for an amount with three decimals")
	}
}

func TestAFileCutLateInTheEveningIsTomorrowsInParis(t *testing.T) {
	at := time.Date(2026, 11, 10, 23, 55, 0, 0, time.UTC) // 00:55 on the 11th in Paris, CET
	if got := FileStamp(at); got != "20261111005500" {
		t.Fatalf("FileStamp = %s, want the Paris date", got)
	}
}
