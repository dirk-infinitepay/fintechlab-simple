package worldline

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the image has no zoneinfo, and file names are stamped in CET
)

// This file implements the real Worldline/Bambora European settlement file
// format -- a multi-section, always-quoted CSV, distinct from the
// North-American-style Report/ToCSV/ToXML API the rest of this package
// models. See docs/ARCHITECTURE-vendor-corrections.md section 1 (and its
// "Addendum: resolved integration gaps" for anything that overrides it).
//
// Section identification: this lab's simulated bank ledger has no
// card-scheme/MCC/interchange/chargeback data (see Entry's doc comment in
// report.go for why), so GenerateBambora synthesizes deterministic fake
// card metadata per transaction -- seeded from the transaction's own data
// (merchant id, date, running index), never time.Now or math/rand, so the
// same ledger input always produces byte-identical output. Chargebacks are
// always empty for the same honest-reflection reason Generate hard-codes
// its own chargeback fields to 0: there is no source data for them.

// BamboraMeta is the file-level "Settlement" section: exactly one row per
// file, RECORD_TYPE "Settlement".
//
// Amounts are held in minor units here and written to the file in major
// units with two decimals ("141.22"), as the Worldline sample buddy parses
// (apps/settle-integration-testing/support/fixtures/worldline-sample.csv).
type BamboraMeta struct {
	VersionNumber      string
	SettlementAmount   int64 // minor units
	SettlementCurrency string
	ValueDate          string // YYYY-MM-DD
	NumberOfItems      int
	ToAccount          string
	PaymentReference   string
}

// BamboraBatch is one "Batch" section row (RECORD_TYPE "Batch"); a file may
// repeat this section, each followed by its own TXER header + rows.
type BamboraBatch struct {
	BamboraMID         string // acquiring-side batch id; NOT a merchant key, see ADDITIONAL_REF_2 below
	BatchRef           string
	PayrefExtended     string
	NetAmount          int64 // minor units
	BatchCurrency      string
	SettlementAmount   int64 // minor units
	SettlementCurrency string
	NumberOfTrans      int
	Transactions       []BamboraTransaction
}

// BamboraTransaction is one "TXER" row within a batch.
//
// Non-obvious real semantics (see ARCHITECTURE-vendor-corrections.md
// section 1, verified against buddy's own parser + unit tests): the real
// merchant grouping key for TXER rows is AdditionalRef2, NOT BamboraMID
// (one BamboraMID can span multiple merchants) and NOT SubmerchantID
// (that column is the CB rows' merchant key, not TXER's). GenerateBambora
// always sets AdditionalRef2 to the real merchant id it was called with,
// and never uses BamboraMID/SubmerchantID to carry or derive that identity.
type BamboraTransaction struct {
	BamboraMID          string
	SubmerchantID       string // synthesized acquiring-side id; NOT the merchant key for TXER rows
	BatchRef            string
	TransactionRef      string
	BamboraRef          string
	AdditionalRef1      string
	AdditionalRef2      string // the real merchant grouping key for TXER rows -- always the merchant id
	TransactionType     string
	TransactionAmount   int64 // minor units
	TransactionCurrency string
	FXRate              string
	SettlementAmount    int64 // minor units
	SettlementCurrency  string
	CardSchemeName      string
	CardUsage           string
	CardCategory        string
	InterchangeDomain   string
	CountryMerchant     string
	CountryIssuer       string
	MCC                 string
	EcomSecurityLevel   string
	AdditionalRef3      string
	CardNumberTruncated string
	TransactionDate     string
	TransactionTime     string
	CashbackAmount      int64 // minor units
	PayrefExtended      string
	TerminalID          string
}

// BamboraChargeback is one "CB" row, trailing, file-wide (one section for
// the whole file, not per-batch). Its merchant grouping key is
// SubmerchantID, not AdditionalRef1/2 -- the opposite of TXER rows, see
// BamboraTransaction's doc comment. GenerateBambora never populates any
// chargebacks: this simulated bank ledger only ever records successful
// transfers (see Entry's doc comment in report.go), so there is no data
// source for a dispute -- the same honest-reflection choice Generate makes
// for its own chargeback fields.
type BamboraChargeback struct {
	BamboraMID                        string
	SubmerchantID                     string // the real merchant grouping key for CB rows
	OriginalTransactionRef            string
	BamboraRef                        string
	DisputeTransactionAmount          int64 // minor units; negative = chargeback, positive = reversal
	DisputeCurrency                   string
	DisputeSettlementAmount           int64 // minor units
	DisputeSettlementCurrency         string
	DisputeReasonCode                 string
	DisputeRegistrationDate           string
	DisputeFee                        int64 // minor units
	DisputeFeeCurrency                string
	OriginalTransactionAdditionalRef1 string
}

// BamboraFile is the full multi-section document GenerateBambora builds:
// one Meta row, zero or more Batches (each with its own Transactions), and
// a trailing (currently always empty, see BamboraChargeback) Chargebacks
// section.
type BamboraFile struct {
	Meta        BamboraMeta
	Batches     []BamboraBatch
	Chargebacks []BamboraChargeback
}

// BamboraOptions carries file-level fields GenerateBambora cannot derive
// from ledger entries alone. Every field is optional; zero values fall
// back to a sensible default documented per field.
type BamboraOptions struct {
	// ValueDate is the Settlement section's VALUE_DATE (YYYY-MM-DD).
	// Defaults to toDate.
	ValueDate string
	// PaymentReference is the Settlement section's PAYMENT_REFERENCE.
	// Defaults to a deterministic reference built from merchantID and toDate.
	PaymentReference string
	// VersionNumber is the Settlement section's VERSION_NUMBER. Defaults to "V2.10".
	VersionNumber string
	// ToAccount is the Settlement section's TO_ACCOUNT: the account the
	// lump sum is paid into. For a whole-file cut that is the platform's
	// settlement account, not any one merchant's.
	ToAccount string
	// Currency restricts the file to transactions in that currency. A
	// real settlement file is per submerchant *and* per currency -- the
	// filename itself carries it (see BamboraFilename). Empty means "take
	// every currency and label the file with whichever came first", which
	// is only correct when the caller already knows there is just one.
	Currency string
}

// bamboraCardSchemes, bamboraInterchangeDomains, etc. are small closed sets
// of plausible-but-obviously-simulated card metadata values, in the same
// "obviously fake" spirit as this lab's "GB00SIM..." IBANs. GenerateBambora
// picks deterministically from each set per transaction.
var (
	bamboraCardSchemes        = []string{"Visa", "Mastercard", "AMEX"}
	bamboraCardSchemeBINs     = []string{"411111", "555555", "371449"}
	bamboraCardUsages         = []string{"Debit", "Credit"}
	bamboraCardCategories     = []string{"Consumer", "Commercial"}
	bamboraInterchangeDomains = []string{"Domestic", "Intraregional", "Interregional"}
	bamboraCountries          = []string{"GB", "DE", "FR", "NL", "IE"}
	bamboraMCCs               = []string{"5411", "5812", "5999", "7995"}
	// ECOM_SECURITY_LEVEL is a numeric electronic-commerce indicator in the
	// real file, not a protocol name; these are the values the sample carries.
	bamboraEcomSecurityLevels = []string{"0", "1", "2", "5", "6", "7"}
)

// bamboraVersion is the file format version the real Worldline file carries.
const bamboraVersion = "V2.10"

// settles reports whether t becomes a TXER row: every sale, and a refund,
// which Worldline settles as a negative row. Any other negative entry (a
// debit leg of this lab's ledger) is not something the acquirer settles.
func settles(t Transaction) bool {
	if t.AmountCents > 0 {
		return true
	}
	return t.AmountCents < 0 && strings.EqualFold(t.Type, "Refund")
}

// bamboraSeed derives a deterministic 64-bit seed from parts (never
// time.Now/math/rand): same parts always produce the same seed, so
// GenerateBambora's output is byte-identical for byte-identical input.
func bamboraSeed(parts ...string) uint64 {
	h := sha256.New()
	for _, p := range parts {
		io.WriteString(h, p)
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return binary.BigEndian.Uint64(sum[:8])
}

// GenerateBambora scans ledger for merchantID's credit entries within
// [fromDate, toDate] (inclusive, "YYYY-MM-DD" lexical compare, mirroring
// Generate) and produces the real Worldline/Bambora settlement file for
// them: one Meta row summarizing the whole file, and (if there is at least
// one matching entry) a single Batch containing one TXER row per entry.
// Card-scheme/MCC/interchange metadata that this lab's ledger has no
// source for is synthesized deterministically per entry (see
// BamboraTransaction's doc comment for the non-obvious merchant-key
// semantics this synthesis must respect).
func GenerateBambora(txns []Transaction, merchantID, fromDate, toDate string, opts BamboraOptions) *BamboraFile {
	f := &BamboraFile{}

	valueDate := opts.ValueDate
	if valueDate == "" {
		valueDate = toDate
	}
	version := opts.VersionNumber
	if version == "" {
		version = bamboraVersion
	}
	paymentRef := opts.PaymentReference
	if paymentRef == "" {
		paymentRef = fmt.Sprintf("WORLDLINE SETTLEMENT %s %s", merchantID, toDate)
	}

	currency := strings.ToUpper(opts.Currency)
	var rows []BamboraTransaction
	var total int64
	idx := 0
	for _, t := range txns {
		if t.MID != merchantID {
			continue
		}
		if t.Date < fromDate || t.Date > toDate {
			continue
		}
		if !settles(t) {
			continue
		}
		if opts.Currency != "" && !strings.EqualFold(t.Currency, opts.Currency) {
			continue
		}
		if currency == "" {
			currency = strings.ToUpper(t.Currency)
		}
		rows = append(rows, bamboraTransaction(merchantID, t, idx))
		total += t.AmountCents
		idx++
	}

	f.Meta = BamboraMeta{
		VersionNumber:      version,
		SettlementAmount:   total,
		SettlementCurrency: currency,
		ValueDate:          valueDate,
		NumberOfItems:      len(rows),
		ToAccount:          merchantID,
		PaymentReference:   paymentRef,
	}

	if len(rows) > 0 {
		batchMID := fmt.Sprintf("MID%08X", uint32(bamboraSeed("batch-mid", merchantID, fromDate, toDate)))
		batchRef := fmt.Sprintf("BATCH%08X", uint32(bamboraSeed("batch-ref", merchantID, fromDate, toDate)))
		for i := range rows {
			rows[i].BamboraMID = batchMID
			rows[i].BatchRef = batchRef
			rows[i].PayrefExtended = paymentRef
		}
		f.Batches = []BamboraBatch{{
			BamboraMID:         batchMID,
			BatchRef:           batchRef,
			PayrefExtended:     paymentRef,
			NetAmount:          total,
			BatchCurrency:      currency,
			SettlementAmount:   total,
			SettlementCurrency: currency,
			NumberOfTrans:      len(rows),
			Transactions:       rows,
		}}
	}

	return f
}

// GenerateSettlementFile builds the settlement file Worldline actually
// delivers: one file per currency covering *every* submerchant that
// traded, with one Batch section per MID.
//
// The filename pattern carries a timestamp, the contract identifier, the
// slot and the currency -- and no merchant. That is not an omission: a
// real settlement file is per receiving platform, not per merchant, which
// is exactly why the row-level ADDITIONAL_REF_2 grouping key exists and
// why the platform has to split the file itself to find what each outlet
// is owed. Cutting one file per merchant would collide on that filename
// and quietly overwrite all but the last.
//
// GenerateBambora remains the single-merchant view of the same machinery,
// used where one merchant's rows are what is wanted.
func GenerateSettlementFile(txns []Transaction, currency, fromDate, toDate string, opts BamboraOptions) *BamboraFile {
	currency = strings.ToUpper(currency)
	opts.Currency = currency

	// Deterministic MID ordering: the same input must always render the
	// same bytes, and map iteration order would break that.
	seen := map[string]bool{}
	var mids []string
	for _, t := range txns {
		if t.Date < fromDate || t.Date > toDate || !settles(t) {
			continue
		}
		if currency != "" && !strings.EqualFold(t.Currency, currency) {
			continue
		}
		if !seen[t.MID] {
			seen[t.MID] = true
			mids = append(mids, t.MID)
		}
	}
	sort.Strings(mids)

	valueDate := opts.ValueDate
	if valueDate == "" {
		valueDate = toDate
	}
	version := opts.VersionNumber
	if version == "" {
		version = bamboraVersion
	}
	toAccount := opts.ToAccount
	if toAccount == "" {
		toAccount = "SETTLEMENT_ACCOUNT"
	}
	paymentRef := opts.PaymentReference
	if paymentRef == "" {
		paymentRef = fmt.Sprintf("WORLDLINE SETTLEMENT %s %s", currency, toDate)
	}

	f := &BamboraFile{}
	var total int64
	var items int
	for _, mid := range mids {
		per := GenerateBambora(txns, mid, fromDate, toDate, BamboraOptions{
			ValueDate:        valueDate,
			VersionNumber:    version,
			Currency:         currency,
			PaymentReference: paymentRef,
		})
		f.Batches = append(f.Batches, per.Batches...)
		total += per.Meta.SettlementAmount
		items += per.Meta.NumberOfItems
	}

	f.Meta = BamboraMeta{
		VersionNumber:      version,
		SettlementAmount:   total,
		SettlementCurrency: currency,
		ValueDate:          valueDate,
		NumberOfItems:      items,
		// The account the lump sum lands in -- the platform's, not a
		// merchant's. The platform works out per-merchant shares from the
		// TXER rows.
		ToAccount:        toAccount,
		PaymentReference: paymentRef,
	}
	return f
}

// bamboraTransaction builds one TXER row for e, deterministically seeded
// from merchantID + e.Date + idx (idx breaks ties between same-day
// entries, guaranteeing distinct refs without any random/time input).
// BamboraMID, BatchRef, and PayrefExtended are filled in by the caller
// once the batch they belong to is known.
func bamboraTransaction(merchantID string, e Transaction, idx int) BamboraTransaction {
	seed := bamboraSeed("txn", merchantID, e.Date, strconv.Itoa(idx))

	schemeIdx := int(seed % uint64(len(bamboraCardSchemes)))
	usageIdx := int((seed / 3) % uint64(len(bamboraCardUsages)))
	categoryIdx := int((seed / 7) % uint64(len(bamboraCardCategories)))
	domainIdx := int((seed / 11) % uint64(len(bamboraInterchangeDomains)))
	merchantCountryIdx := int((seed / 13) % uint64(len(bamboraCountries)))
	issuerCountryIdx := int((seed / 17) % uint64(len(bamboraCountries)))
	mccIdx := int((seed / 19) % uint64(len(bamboraMCCs)))
	ecomIdx := int((seed / 23) % uint64(len(bamboraEcomSecurityLevels)))

	txType := "Sale"
	var cashback int64
	if seed%5 == 0 {
		txType = "Sale with Cash Back"
		cashback = int64(seed % 500)
	}
	if e.Type != "" {
		txType = e.Type
	}

	hh := (seed / 29) % 24
	mm := (seed / 31) % 60
	ss := (seed / 37) % 60

	last4 := seed % 10000

	return BamboraTransaction{
		SubmerchantID:       fmt.Sprintf("SUB%08X", uint32(bamboraSeed("submerchant", merchantID, e.Date, strconv.Itoa(idx)))),
		TransactionRef:      fmt.Sprintf("TXN%s%06d", strings.ToUpper(strconv.FormatUint(seed&0xFFFFFF, 16)), idx),
		BamboraRef:          fmt.Sprintf("BREF%016X", seed),
		AdditionalRef1:      fmt.Sprintf("AUTH%06d", seed%1000000),
		AdditionalRef2:      merchantID, // the real merchant grouping key for TXER rows
		TransactionType:     txType,
		TransactionAmount:   e.AmountCents,
		TransactionCurrency: e.Currency,
		FXRate:              "1", // this lab never models cross-currency settlement
		SettlementAmount:    e.AmountCents,
		SettlementCurrency:  e.Currency,
		CardSchemeName:      pick(e.CardSchemeName, bamboraCardSchemes[schemeIdx]),
		CardUsage:           pick(e.CardUsage, bamboraCardUsages[usageIdx]),
		CardCategory:        pick(e.CardCategory, bamboraCardCategories[categoryIdx]),
		InterchangeDomain:   bamboraInterchangeDomains[domainIdx],
		CountryMerchant:     pick(e.CountryMerchant, bamboraCountries[merchantCountryIdx]),
		CountryIssuer:       pick(e.CountryIssuer, bamboraCountries[issuerCountryIdx]),
		MCC:                 pick(e.MCC, bamboraMCCs[mccIdx]),
		EcomSecurityLevel:   bamboraEcomSecurityLevels[ecomIdx],
		AdditionalRef3:      fmt.Sprintf("%012d", seed%1000000000000),
		CardNumberTruncated: bamboraCardSchemeBINs[schemeIdx] + "XXXXXX" + fmt.Sprintf("%04d", last4),
		TransactionDate:     e.Date,
		TransactionTime:     fmt.Sprintf("%02d:%02d:%02d", hh, mm, ss),
		CashbackAmount:      cashback,
		TerminalID:          pick(e.TerminalID, fmt.Sprintf("TERM%06d", seed%1000000)),
	}
}

// pick returns supplied when the caller gave a value for a field, and the
// deterministically-synthesized fallback otherwise. A real acquirer knows
// the card scheme, MCC and terminal for every transaction; this lab lets a
// seeder state them where a scenario cares and invents plausible ones
// where it does not, without ever reaching for time.Now or math/rand.
func pick(supplied, synthesized string) string {
	if supplied != "" {
		return supplied
	}
	return synthesized
}

// bamboraMetaHeader, bamboraBatchHeader, bamboraTXERHeader, and
// bamboraCBHeader match the exact column order in
// docs/ARCHITECTURE-vendor-corrections.md section 1's fenced block.
var (
	bamboraMetaHeader = []string{
		"VERSION_NUMBER", "RECORD_TYPE", "SETTLEMENT_AMOUNT", "SETTLEMENT_CURRENCY",
		"VALUE_DATE", "NUMBER_OF_ITEMS", "TO_ACCOUNT", "PAYMENT_REFERENCE",
	}
	bamboraBatchHeader = []string{
		"RECORD_TYPE", "BAMBORA_MID", "BATCH_REF", "PAYREF_EXTENDED", "NET_AMOUNT",
		"BATCH_CURRENCY", "SETTLEMENT_AMOUNT", "SETTLEMENT_CURRENCY", "NUMBER_OF_TRANS",
	}
	bamboraTXERHeader = []string{
		"RECORD_TYPE", "BAMBORA_MID", "SUBMERCHANT_ID", "BATCH_REF", "TRANSACTION_REF",
		"BAMBORA_REF", "ADDITIONAL_REF_1", "ADDITIONAL_REF_2", "TRANSACTION_TYPE",
		"TRANSACTION_AMOUNT", "TRANSACTION_CURRENCY", "FX_RATE", "SETTLEMENT_AMOUNT",
		"SETTLEMENT_CURRENCY", "CARD_SCHEME_NAME", "CARD_USAGE", "CARD_CATEGORY",
		"INTERCHANGE_DOMAIN", "COUNTRY_MERCHANT", "COUNTRY_ISSUER", "MCC",
		"ECOM_SECURITY_LEVEL", "ADDITIONAL_REF_3", "CARD_NUMBER_TRUNCATED",
		"TRANSACTION_DATE", "TRANSACTION_TIME", "CASHBACK_AMOUNT", "PAYREF_EXTENDED",
		"TERMINALID",
	}
	bamboraCBHeader = []string{
		"RECORD_TYPE", "BAMBORA_MID", "SUBMERCHANT_ID", "ORIGINAL_TRANSACTION_REF",
		"BAMBORA_REF", "DISPUTE_TRANSACTION_AMOUNT", "DISPUTE_CURRENCY",
		"DISPUTE_SETTLEMENT_AMOUNT", "DISPUTE_SETTLEMENT_CURRENCY", "DISPUTE_REASON_CODE",
		"DISPUTE_REGISTRATION_DATE", "DISPUTE_FEE", "DISPUTE_FEE_CURRENCY",
		"ORIGINAL_TRANSACTION_ADDITIONAL_REF_1",
	}
)

// Record-type codes, as the real file carries them: the Settlement and Batch
// sections spell their names out. This lab used to write its own short codes
// ST/BT, which buddy's parser does not recognise; ParseBamboraCSV still
// reads them so an archived file from that era stays readable.
const (
	bamboraRecordTypeSettlement       = "Settlement"
	bamboraRecordTypeBatch            = "Batch"
	bamboraRecordTypeTXER             = "TXER"
	bamboraRecordTypeCB               = "CB"
	legacyBamboraRecordTypeSettlement = "ST"
	legacyBamboraRecordTypeBatch      = "BT"
)

// formatMajorUnits writes a minor-unit amount in major units with two
// decimals: 14122 -> "141.22", -1234 -> "-12.34", 0 -> "0.00". Integer
// arithmetic only; an amount never passes through a float.
func formatMajorUnits(minor int64) string {
	sign := ""
	if minor < 0 {
		sign = "-"
		minor = -minor
	}
	return fmt.Sprintf("%s%d.%02d", sign, minor/100, minor%100)
}

// quoteBamboraField wraps s in double quotes, doubling any embedded quote
// -- standard CSV escaping -- matching section 1's "Values are `\"quoted\"`."
func quoteBamboraField(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func writeBamboraRow(w io.Writer, fields []string) error {
	quoted := make([]string, len(fields))
	for i, f := range fields {
		quoted[i] = quoteBamboraField(f)
	}
	_, err := fmt.Fprintf(w, "%s\n", strings.Join(quoted, ","))
	return err
}

func bamboraMetaRow(m BamboraMeta) []string {
	return []string{
		m.VersionNumber,
		bamboraRecordTypeSettlement,
		formatMajorUnits(m.SettlementAmount),
		m.SettlementCurrency,
		m.ValueDate,
		strconv.Itoa(m.NumberOfItems),
		m.ToAccount,
		m.PaymentReference,
	}
}

func bamboraBatchRow(b BamboraBatch) []string {
	return []string{
		bamboraRecordTypeBatch,
		b.BamboraMID,
		b.BatchRef,
		b.PayrefExtended,
		formatMajorUnits(b.NetAmount),
		b.BatchCurrency,
		formatMajorUnits(b.SettlementAmount),
		b.SettlementCurrency,
		strconv.Itoa(b.NumberOfTrans),
	}
}

func bamboraTXERRow(t BamboraTransaction) []string {
	return []string{
		bamboraRecordTypeTXER,
		t.BamboraMID,
		t.SubmerchantID,
		t.BatchRef,
		t.TransactionRef,
		t.BamboraRef,
		t.AdditionalRef1,
		t.AdditionalRef2,
		t.TransactionType,
		formatMajorUnits(t.TransactionAmount),
		t.TransactionCurrency,
		t.FXRate,
		formatMajorUnits(t.SettlementAmount),
		t.SettlementCurrency,
		t.CardSchemeName,
		t.CardUsage,
		t.CardCategory,
		t.InterchangeDomain,
		t.CountryMerchant,
		t.CountryIssuer,
		t.MCC,
		t.EcomSecurityLevel,
		t.AdditionalRef3,
		t.CardNumberTruncated,
		t.TransactionDate,
		t.TransactionTime,
		formatMajorUnits(t.CashbackAmount),
		t.PayrefExtended,
		t.TerminalID,
	}
}

func bamboraCBRow(c BamboraChargeback) []string {
	return []string{
		bamboraRecordTypeCB,
		c.BamboraMID,
		c.SubmerchantID,
		c.OriginalTransactionRef,
		c.BamboraRef,
		formatMajorUnits(c.DisputeTransactionAmount),
		c.DisputeCurrency,
		formatMajorUnits(c.DisputeSettlementAmount),
		c.DisputeSettlementCurrency,
		c.DisputeReasonCode,
		c.DisputeRegistrationDate,
		formatMajorUnits(c.DisputeFee),
		c.DisputeFeeCurrency,
		c.OriginalTransactionAdditionalRef1,
	}
}

// WriteCSV renders f as the exact quoted multi-section format: the
// Settlement header + its one row, then for each batch its Batch header +
// row followed by that batch's TXER header + rows, then finally the CB
// header (always present, even with zero rows -- see BamboraChargeback's
// doc comment) + any chargeback rows. Each section's own header row
// repeats immediately before its data rows, matching section 1's observed
// real-file shape.
func (f *BamboraFile) WriteCSV(w io.Writer) error {
	if err := writeBamboraRow(w, bamboraMetaHeader); err != nil {
		return err
	}
	if err := writeBamboraRow(w, bamboraMetaRow(f.Meta)); err != nil {
		return err
	}
	for _, b := range f.Batches {
		if err := writeBamboraRow(w, bamboraBatchHeader); err != nil {
			return err
		}
		if err := writeBamboraRow(w, bamboraBatchRow(b)); err != nil {
			return err
		}
		if err := writeBamboraRow(w, bamboraTXERHeader); err != nil {
			return err
		}
		for _, t := range b.Transactions {
			if err := writeBamboraRow(w, bamboraTXERRow(t)); err != nil {
				return err
			}
		}
	}
	if err := writeBamboraRow(w, bamboraCBHeader); err != nil {
		return err
	}
	for _, c := range f.Chargebacks {
		if err := writeBamboraRow(w, bamboraCBRow(c)); err != nil {
			return err
		}
	}
	return nil
}

// BamboraFilename implements the real filename pattern from
// reconciliation-file-validator.ts:
// "{YYYYMMDDHHMMSS}_{identifier}_{ER|AR}_{CCY}.csv". fileType is "ER"
// ("Morning file") or "AR" ("Afternoon file") -- two delivery slots for
// the same content shape, not different report types. currency is
// upper-cased to match the documented 3-letter uppercase convention.
//
// The timestamp is Worldline's local time, CET/CEST: the zone of its cut-off
// ("00:00 CET"), and the zone the platform reads the file's day in (lightyear's
// gateway takes only files whose name starts with today's date in
// Europe/Paris). In UTC, a file cut late in the evening would read as the day
// before for an hour or two.
func BamboraFilename(identifier string, at time.Time, fileType, currency string) string {
	return fmt.Sprintf("%s_%s_%s_%s.csv", FileStamp(at), identifier, fileType, strings.ToUpper(currency))
}

// worldlineZone is the zone Worldline stamps its file names in.
var worldlineZone = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		panic("worldline: Europe/Paris: " + err.Error()) // time/tzdata is embedded
	}
	return loc
}()

// FileStamp is the timestamp a Worldline file name starts with, for at.
func FileStamp(at time.Time) string { return at.In(worldlineZone).Format("20060102150405") }
