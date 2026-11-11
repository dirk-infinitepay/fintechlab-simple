package worldline

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ParsedFile is a settlement file read back off the wire -- what a
// platform gets after downloading and PGP-decrypting one.
//
// This parser exists on the vendor side of the lab on purpose: it is the
// executable statement of what the generator promises, and a round-trip
// test (generate -> render -> parse) is what stops the two drifting. A
// platform is free to use it as a reference and just as free to write its
// own, which is the real situation.
type ParsedFile struct {
	Meta        BamboraMeta
	Batches     []BamboraBatch
	Chargebacks []BamboraChargeback
}

// PayoutsByMID totals the settlement amounts in f grouped by the real
// merchant key for TXER rows, ADDITIONAL_REF_2.
//
// The grouping key matters and is easy to get wrong: BAMBORA_MID is an
// acquiring-side batch id that can span several merchants, and
// SUBMERCHANT_ID is the CB rows' key, not TXER's. Getting this wrong pays
// the wrong merchant, which is why it is a named function with a test
// rather than an inline loop at the call site.
func (f *ParsedFile) PayoutsByMID() map[string]int64 {
	out := map[string]int64{}
	for _, b := range f.Batches {
		for _, t := range b.Transactions {
			out[t.AdditionalRef2] += t.SettlementAmount
		}
	}
	return out
}

// ParseBamboraCSV reads the multi-section settlement format back into a
// ParsedFile. Section headers repeat before their own rows, so each row is
// dispatched on its RECORD_TYPE rather than on position: RECORD_TYPE is
// the second column of a Settlement row (it follows VERSION_NUMBER) and
// the first column of every other section's rows.
//
// Unknown record types are skipped rather than rejected. Real vendor files
// gain columns and sections over time, and a consumer that dies on the
// first unrecognized row is a consumer that dies the day the vendor ships
// a change -- the same "parsers should be flexible" instruction Banking
// Circle gives for its own payloads.
func ParseBamboraCSV(r io.Reader) (*ParsedFile, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // sections have different widths
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("worldline: read settlement csv: %w", err)
	}

	// A file this lab wrote before it matched the real format (ST/BT record
	// types) carries amounts in minor units; the real format carries major
	// units. The unit is decided once, from the file's own settlement row.
	amounts := majorUnitAmounts
	for _, row := range rows {
		if len(row) > 1 && row[1] == legacyBamboraRecordTypeSettlement {
			amounts = minorUnitAmounts
			break
		}
	}

	f := &ParsedFile{}
	var batch *BamboraBatch
	for i, row := range rows {
		switch recordTypeOf(row) {
		case bamboraRecordTypeSettlement:
			m, err := parseMetaRow(row, amounts)
			if err != nil {
				return nil, fmt.Errorf("worldline: settlement row %d: %w", i+1, err)
			}
			f.Meta = m
		case bamboraRecordTypeBatch:
			b, err := parseBatchRow(row, amounts)
			if err != nil {
				return nil, fmt.Errorf("worldline: batch row %d: %w", i+1, err)
			}
			f.Batches = append(f.Batches, b)
			batch = &f.Batches[len(f.Batches)-1]
		case bamboraRecordTypeTXER:
			t, err := parseTXERRow(row, amounts)
			if err != nil {
				return nil, fmt.Errorf("worldline: TXER row %d: %w", i+1, err)
			}
			if batch == nil {
				return nil, fmt.Errorf("worldline: TXER row %d appears before any batch row", i+1)
			}
			batch.Transactions = append(batch.Transactions, t)
		case bamboraRecordTypeCB:
			c, err := parseCBRow(row, amounts)
			if err != nil {
				return nil, fmt.Errorf("worldline: CB row %d: %w", i+1, err)
			}
			f.Chargebacks = append(f.Chargebacks, c)
		}
	}
	return f, nil
}

// ParseSettlementFile is ParseBamboraCSV for bytes that must be a
// settlement file, as a lab hook taking one from anywhere wants them.
// ParseBamboraCSV is lenient by design and reads text that is no
// settlement file at all as an empty one; this refuses a file with no
// Settlement row, or whose Settlement row names no currency, with the
// reason.
func ParseSettlementFile(r io.Reader) (*ParsedFile, error) {
	f, err := ParseBamboraCSV(r)
	if err != nil {
		return nil, err
	}
	if f.Meta == (BamboraMeta{}) {
		return nil, fmt.Errorf("worldline: not a settlement file: it has no %q row", bamboraRecordTypeSettlement)
	}
	if f.Meta.SettlementCurrency == "" {
		return nil, fmt.Errorf("worldline: not a settlement file: its %s row names no SETTLEMENT_CURRENCY", bamboraRecordTypeSettlement)
	}
	return f, nil
}

// MIDs lists every merchant the file settles, by the TXER rows' grouping
// key ADDITIONAL_REF_2 (see PayoutsByMID), each once, in the order the
// file first names it: for a file Cut wrote, its CutFile.MIDs.
func (f *ParsedFile) MIDs() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, b := range f.Batches {
		for _, t := range b.Transactions {
			if !seen[t.AdditionalRef2] {
				seen[t.AdditionalRef2] = true
				out = append(out, t.AdditionalRef2)
			}
		}
	}
	return out
}

// recordTypeOf identifies a data row's section, or "" for a header row or
// anything unrecognized. A header row is distinguishable because its own
// RECORD_TYPE cell holds the literal string "RECORD_TYPE".
func recordTypeOf(row []string) string {
	if len(row) < 2 {
		return ""
	}
	if row[0] == "RECORD_TYPE" || row[1] == "RECORD_TYPE" {
		return ""
	}
	switch row[0] {
	case bamboraRecordTypeBatch, legacyBamboraRecordTypeBatch:
		return bamboraRecordTypeBatch
	case bamboraRecordTypeTXER, bamboraRecordTypeCB:
		return row[0]
	}
	if row[1] == bamboraRecordTypeSettlement || row[1] == legacyBamboraRecordTypeSettlement {
		return bamboraRecordTypeSettlement
	}
	return ""
}

// field returns row[i] or "" -- a short file that stops before an optional
// trailing column is readable, not an error.
func field(row []string, i int) string {
	if i < 0 || i >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[i])
}

// amountParser turns one amount cell into minor units.
type amountParser func(s, name string) (int64, error)

// majorUnitAmounts reads the real format's major units -- "141.22", "20637",
// "-99.00", "0.5" -- into minor units with integer arithmetic. More than two
// decimal places is an error rather than a rounding: a cent that silently
// appears or disappears is a merchant paid the wrong amount.
func majorUnitAmounts(s, name string) (int64, error) {
	negative := strings.HasPrefix(s, "-")
	digits := strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(digits, ".")
	frac = strings.TrimRight(frac, "0")
	if whole == "" || len(frac) > 2 || !allDigits(whole) || !allDigits(frac) {
		return 0, fmt.Errorf("%s = %q is not an amount with at most two decimals", name, s)
	}
	units, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s = %q is out of range", name, s)
	}
	cents := int64(0)
	if frac != "" {
		frac += strings.Repeat("0", 2-len(frac))
		cents, _ = strconv.ParseInt(frac, 10, 64)
	}
	minor := units*100 + cents
	if negative {
		minor = -minor
	}
	return minor, nil
}

// minorUnitAmounts reads this lab's pre-V2.10 files, which wrote integer
// minor units.
func minorUnitAmounts(s, name string) (int64, error) {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s = %q is not an integer", name, s)
	}
	return v, nil
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// amountField parses an amount cell. An empty cell is 0; a non-numeric one
// is an error, because silently reading a corrupt amount as zero is how a
// merchant gets underpaid.
func amountField(row []string, i int, name string, parse amountParser) (int64, error) {
	s := field(row, i)
	if s == "" {
		return 0, nil
	}
	return parse(s, name)
}

// intField parses a count. An empty cell is 0; a non-numeric one is an error.
func intField(row []string, i int, name string) (int64, error) {
	s := field(row, i)
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s = %q is not an integer", name, s)
	}
	return v, nil
}

func parseMetaRow(row []string, amounts amountParser) (BamboraMeta, error) {
	amount, err := amountField(row, 2, "SETTLEMENT_AMOUNT", amounts)
	if err != nil {
		return BamboraMeta{}, err
	}
	items, err := intField(row, 5, "NUMBER_OF_ITEMS")
	if err != nil {
		return BamboraMeta{}, err
	}
	return BamboraMeta{
		VersionNumber:      field(row, 0),
		SettlementAmount:   amount,
		SettlementCurrency: field(row, 3),
		ValueDate:          field(row, 4),
		NumberOfItems:      int(items),
		ToAccount:          field(row, 6),
		PaymentReference:   field(row, 7),
	}, nil
}

func parseBatchRow(row []string, amounts amountParser) (BamboraBatch, error) {
	net, err := amountField(row, 4, "NET_AMOUNT", amounts)
	if err != nil {
		return BamboraBatch{}, err
	}
	settled, err := amountField(row, 6, "SETTLEMENT_AMOUNT", amounts)
	if err != nil {
		return BamboraBatch{}, err
	}
	count, err := intField(row, 8, "NUMBER_OF_TRANS")
	if err != nil {
		return BamboraBatch{}, err
	}
	return BamboraBatch{
		BamboraMID:         field(row, 1),
		BatchRef:           field(row, 2),
		PayrefExtended:     field(row, 3),
		NetAmount:          net,
		BatchCurrency:      field(row, 5),
		SettlementAmount:   settled,
		SettlementCurrency: field(row, 7),
		NumberOfTrans:      int(count),
	}, nil
}

func parseTXERRow(row []string, amounts amountParser) (BamboraTransaction, error) {
	amount, err := amountField(row, 9, "TRANSACTION_AMOUNT", amounts)
	if err != nil {
		return BamboraTransaction{}, err
	}
	settled, err := amountField(row, 12, "SETTLEMENT_AMOUNT", amounts)
	if err != nil {
		return BamboraTransaction{}, err
	}
	cashback, err := amountField(row, 26, "CASHBACK_AMOUNT", amounts)
	if err != nil {
		return BamboraTransaction{}, err
	}
	return BamboraTransaction{
		BamboraMID:          field(row, 1),
		SubmerchantID:       field(row, 2),
		BatchRef:            field(row, 3),
		TransactionRef:      field(row, 4),
		BamboraRef:          field(row, 5),
		AdditionalRef1:      field(row, 6),
		AdditionalRef2:      field(row, 7),
		TransactionType:     field(row, 8),
		TransactionAmount:   amount,
		TransactionCurrency: field(row, 10),
		FXRate:              field(row, 11),
		SettlementAmount:    settled,
		SettlementCurrency:  field(row, 13),
		CardSchemeName:      field(row, 14),
		CardUsage:           field(row, 15),
		CardCategory:        field(row, 16),
		InterchangeDomain:   field(row, 17),
		CountryMerchant:     field(row, 18),
		CountryIssuer:       field(row, 19),
		MCC:                 field(row, 20),
		EcomSecurityLevel:   field(row, 21),
		AdditionalRef3:      field(row, 22),
		CardNumberTruncated: field(row, 23),
		TransactionDate:     field(row, 24),
		TransactionTime:     field(row, 25),
		CashbackAmount:      cashback,
		PayrefExtended:      field(row, 27),
		TerminalID:          field(row, 28),
	}, nil
}

func parseCBRow(row []string, amounts amountParser) (BamboraChargeback, error) {
	amount, err := amountField(row, 5, "DISPUTE_TRANSACTION_AMOUNT", amounts)
	if err != nil {
		return BamboraChargeback{}, err
	}
	settled, err := amountField(row, 7, "DISPUTE_SETTLEMENT_AMOUNT", amounts)
	if err != nil {
		return BamboraChargeback{}, err
	}
	fee, err := amountField(row, 11, "DISPUTE_FEE", amounts)
	if err != nil {
		return BamboraChargeback{}, err
	}
	return BamboraChargeback{
		BamboraMID:                        field(row, 1),
		SubmerchantID:                     field(row, 2),
		OriginalTransactionRef:            field(row, 3),
		BamboraRef:                        field(row, 4),
		DisputeTransactionAmount:          amount,
		DisputeCurrency:                   field(row, 6),
		DisputeSettlementAmount:           settled,
		DisputeSettlementCurrency:         field(row, 8),
		DisputeReasonCode:                 field(row, 9),
		DisputeRegistrationDate:           field(row, 10),
		DisputeFee:                        fee,
		DisputeFeeCurrency:                field(row, 12),
		OriginalTransactionAdditionalRef1: field(row, 13),
	}, nil
}

// FilenameSlot extracts the slot code ("ER"/"AR") from a settlement
// filename, so a consumer can tell the morning file it must process from
// the afternoon confirmation it need only retain. Returns "" when the name
// does not follow the pattern.
func FilenameSlot(name string) string {
	name = strings.TrimSuffix(strings.TrimSuffix(name, ".pgp"), ".csv")
	parts := strings.Split(name, "_")
	// {timestamp}_{identifier...}_{slot}_{currency}: the identifier itself
	// may contain underscores, so the slot is found by position from the
	// end, not from the start.
	if len(parts) < 3 {
		return ""
	}
	switch slot := parts[len(parts)-2]; slot {
	case SlotMorning, SlotAfternoon:
		return slot
	}
	return ""
}
