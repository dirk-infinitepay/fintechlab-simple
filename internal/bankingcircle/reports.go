package bankingcircle

import (
	"sort"
	"strconv"
	"strings"
)

// The Connect "reports" API: the two reads a reconciliation sweep makes.
//
// These are the observation points that answer "did the money actually
// book", and they are deliberately derived from the same payment records
// the webhooks are derived from. A mock whose report disagreed with its own
// notifications would be worse than no mock: it would teach a client to
// trust a reconciliation that cannot be trusted.
//
// # What each report means
//
// The intraday reconciliation report carries no status field. A row's
// existence *is* the statement that a booking happened — so only booked and
// processed payments appear, and a reversal appears as a second, negative
// booking on the same payment rather than as an absence. What tells booked
// and processed apart is `processedTimestamp`: null while the payment is
// only booked (the real report's pendingProcessing), set once it is
// processed.
//
// The rejection report lists what could not be processed on the requested
// transaction date, with a status: rejected, missing funding, or still
// pending processing. Pending payments that have booked appear in both
// reports, as they do at the bank; every other payment instructed on a
// date appears in exactly one, which is the property a sweep relies on.

// ReconciliationRow is one booking line. Field names are the wire contract;
// the pointer-ish `*string` shapes exist because the real report sends null
// for fields that do not apply, and a client distinguishing "zero" from
// "not applicable" must keep working against this.
type ReconciliationRow struct {
	PaymentID                  *string  `json:"paymentId"`
	Account                    *string  `json:"account"`
	AccountCurrency            *string  `json:"accountCurrency"`
	DebitAmount                *float64 `json:"debitAmount"`
	CreditAmount               *float64 `json:"creditAmount"`
	CreditDebitIndicator       *string  `json:"creditDebitIndicator"`
	TransactionAmount          float64  `json:"transactionAmount"`
	TransactionAmountCurrency  *string  `json:"transactionAmountCurrency"`
	ValueDate                  *string  `json:"valueDate"`
	ReportDate                 *string  `json:"reportDate"`
	Return                     *bool    `json:"return"`
	LatestStatusChangedTimestp *string  `json:"latestStatusChangedTimestamp"`
	ProcessedTimestamp         *string  `json:"processedTimestamp"`
	StatusReasonCode           *string  `json:"statusReasonCode"`
	StatusReasonDescription    *string  `json:"statusReasonDescription"`
	PaymentReferenceNumber     *string  `json:"paymentReferenceNumber"`
	UserReferenceNumber        *string  `json:"userReferenceNumber"`
	ClientOrderID              *string  `json:"clientOrderId"`
	PaymentDetails1            *string  `json:"paymentDetails1"`
	PaymentDetails2            *string  `json:"paymentDetails2"`
	PaymentDetails3            *string  `json:"paymentDetails3"`
	PaymentDetails4            *string  `json:"paymentDetails4"`
	AdditionalRemittanceInfo1  *string  `json:"additionalRemittanceInformation1"`
	AdditionalRemittanceInfo2  *string  `json:"additionalRemittanceInformation2"`
	AdditionalRemittanceInfo3  *string  `json:"additionalRemittanceInformation3"`
}

// RejectionRow is one payment that could not be processed on the
// transaction date. Every property Banking Circle's reference lists is sent,
// in its spelling — pIdChanneluser and pTxndate here, unlike the
// reconciliation report's pIdChannelUser and pTxnDate. It carries no
// paymentId; paymentReferenceNumber, the bank's own reference, is the
// handle back to the payment.
type RejectionRow struct {
	// PIdChanneluser (the requesting API user) and CustomerID are null: the
	// lab models neither users behind its tokens nor customer ids.
	PIdChanneluser         *string `json:"pIdChanneluser"`
	PTxndate               *string `json:"pTxndate"`
	ReportDate             *string `json:"reportDate"`
	CustomerID             *string `json:"customerId"`
	Account                *string `json:"account"`
	AccountCurrency        *string `json:"accountCurrency"`
	ValueDate              *string `json:"valueDate"`
	PaymentAmount          float64 `json:"paymentAmount"`
	PaymentCurrency        *string `json:"paymentCurrency"`
	TransferCurrency       *string `json:"transferCurrency"`
	DestinationIban        *string `json:"destinationIban"`
	PaymentReferenceNumber *string `json:"paymentReferenceNumber"`
	UserReferenceNumber    *string `json:"userReferenceNumber"`
	FileReferenceNumber    *string `json:"fileReferenceNumber"`
	SourceType             *string `json:"sourceType"`
	Status                 *string `json:"status"`
	StatusReason           *string `json:"statusReason"`
}

// ReconciliationQuery is the filter the paged report accepts.
type ReconciliationQuery struct {
	FromTransactionDate string
	ToTransactionDate   string
	FromCreatedAt       string
	ToCreatedAt         string
	AccountIDs          []string
	PageNumber          int
	PageSize            int
	// IBAN names an account on the report. See AccountIBAN.
	IBAN AccountIBAN
}

// AccountIBAN resolves an account id to the account's IBAN. Both reports'
// `account` is the IBAN ("IBAN of your account"), not the id: the id is
// only what the AccountId filter matches on. An account it cannot resolve
// is reported as null, never as its id.
type AccountIBAN func(accountID string) string

func (f AccountIBAN) of(accountID string) *string {
	if f == nil {
		return nil
	}
	return str(f(accountID))
}

// RejectionQuery is the filter the rejection report accepts, with the
// reference's meanings:
//   - IncludeReceived: "Include payments in pending processing";
//   - IncludeMissingFunds: "Include payments with insufficient funds";
//   - ExcludeBooked: "Exclude booked payments" (default false).
//
// IncludeReversals is not here: it means "Include only Direct Debit and SEPA
// Direct Debit reversals", and the lab has no direct debits to reverse.
type RejectionQuery struct {
	TransactionDate     string
	IncludeReceived     bool
	IncludeMissingFunds bool
	ExcludeBooked       bool
	// ReportDate is the day the report is generated, YYYY-MM-DD.
	ReportDate string
	// IBAN names an account on the report. See AccountIBAN.
	IBAN AccountIBAN
}

// booked reports whether a state means value moved on an account.
func booked(s NotificationType) bool {
	switch s {
	case NotificationOutgoingPaymentBooked, NotificationOutgoingPaymentProcessed,
		NotificationIncomingPaymentBooked, NotificationIncomingPaymentProcessed,
		NotificationReversed:
		return true
	}
	return false
}

// processed reports whether a booked state is also processed. A reversal
// only happens to a processed payment, so its original row keeps the
// timestamp.
func processed(s NotificationType) bool {
	switch s {
	case NotificationOutgoingPaymentProcessed, NotificationIncomingPaymentProcessed,
		NotificationReversed:
		return true
	}
	return false
}

// incoming reports whether the booking credits one of our accounts.
func incoming(s NotificationType) bool {
	return s == NotificationIncomingPaymentBooked || s == NotificationIncomingPaymentProcessed
}

// IntradayReconciliation returns the booking lines for a date range.
//
// Both date bounds are inclusive and compared on the date prefix, because
// that is how a caller sweeping "yesterday" reasons about them — and a
// half-open range would silently drop the last payment of the day, which is
// exactly the bug a reconciliation report exists to catch.
func IntradayReconciliation(payments []*Payment, q ReconciliationQuery) []ReconciliationRow {
	rows := make([]ReconciliationRow, 0)
	for _, p := range payments {
		if !booked(p.State) {
			continue
		}
		if !withinDates(p.CreatedAt, q.FromCreatedAt, q.ToCreatedAt) {
			continue
		}
		account := p.FromAccountID
		if incoming(p.State) {
			account = p.ToAccountID
		}
		if len(q.AccountIDs) > 0 && !containsFold(q.AccountIDs, account) {
			continue
		}
		for _, row := range bookingRows(p, q.IBAN.of(account)) {
			if withinDates(derefStr(row.ReportDate), q.FromTransactionDate, q.ToTransactionDate) {
				rows = append(rows, row)
			}
		}
	}
	// Paging over an unordered set is not paging. The caller's payment
	// store hands these back in map order, which differs between calls, so
	// without a total order page 2 can repeat a row from page 1 and drop
	// one entirely — a sweep walking pages would silently lose a payment.
	// Booking time first, then id, then a payment's booking before its
	// reversal, so the order is total.
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		at, bt := derefStr(a.ProcessedTimestamp), derefStr(b.ProcessedTimestamp)
		if at != bt {
			return at < bt
		}
		if ai, bi := derefStr(a.PaymentID), derefStr(b.PaymentID); ai != bi {
			return ai < bi
		}
		return !isReversalRow(a) && isReversalRow(b)
	})
	return page(rows, q.PageNumber, q.PageSize)
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// bookingRows is every line a payment puts on the report. A report lists
// bookings, and a booking is never rewritten, so a reversed payment keeps
// the row it booked with and gains a second one for the reversal.
func bookingRows(p *Payment, account *string) []ReconciliationRow {
	if p.State != NotificationReversed {
		return []ReconciliationRow{reconciliationRow(p, account, p.UpdatedAt)}
	}
	original := reconciliationRow(p, account, firstNonEmpty(p.ProcessedAt, p.CreatedAt))
	return []ReconciliationRow{original, reversalRow(p, original)}
}

func reconciliationRow(p *Payment, account *string, bookedAt string) ReconciliationRow {
	amount := amountFloat(p.Amount)
	// reportDate and valueDate in the reference example's form,
	// 2025-01-31T00:00:00+00:00 (the schema says "format": "date", the
	// example sends midnight UTC).
	date := bcDate(BusinessDate(bookedAt))

	row := ReconciliationRow{
		PaymentID:                  str(p.ID),
		Account:                    account,
		AccountCurrency:            str(p.Currency),
		TransactionAmount:          amount,
		TransactionAmountCurrency:  str(p.Currency),
		ValueDate:                  date,
		ReportDate:                 date,
		LatestStatusChangedTimestp: str(p.UpdatedAt),
		PaymentReferenceNumber:     str(p.ReferenceNumber),
	}
	// paymentDetails1-4 are remittance information lines 1-4.
	details := []**string{&row.PaymentDetails1, &row.PaymentDetails2, &row.PaymentDetails3, &row.PaymentDetails4}
	for i, line := range p.Remittance {
		if i < len(details) {
			*details[i] = str(line)
		}
	}
	// `return` is true on an incoming return payment and null on everything
	// else ("true if the payment is an incoming return payment, otherwise
	// null"). A return also carries the docs' examples of what
	// additionalRemittanceInformation1-3 may hold for one: the return
	// indicator /RETN/, the reason /AC04/Closed Account Number, and the
	// returned payment's reference /MREF/010F10xxxx012345.
	if p.Return {
		isReturn := true
		row.Return = &isReturn
		row.AdditionalRemittanceInfo1 = str("/RETN/")
		if p.ReturnReasonCode != "" {
			row.AdditionalRemittanceInfo2 = str("/" + p.ReturnReasonCode + "/" + p.ReturnReasonDescription)
		}
		if p.ReturnedReference != "" {
			row.AdditionalRemittanceInfo3 = str("/MREF/" + p.ReturnedReference)
		}
	}
	if processed(p.State) {
		row.ProcessedTimestamp = str(firstNonEmpty(p.ProcessedAt, bookedAt))
	}
	if incoming(p.State) {
		row.CreditAmount = &amount
		row.CreditDebitIndicator = str("CRDT")
	} else {
		row.DebitAmount = &amount
		row.CreditDebitIndicator = str("DBIT")
	}
	// paymentReferenceNumber is the bank's own reference (set above), and
	// clientOrderId stays null: the docs fill it only for FX trades executed
	// via the FX API, which this lab does not simulate.
	row.UserReferenceNumber = userReference(p)
	return row
}

// reversalRow is the booking a scheme reversal adds. The docs describe it
// only through the amount: "In case of reversal of a debit, the amount will
// be given as a negative equivalent of the original amount debited" (and
// likewise for a credit). So it keeps the payment's side and references —
// a reversal's webhook matches the original's account and transaction
// reference too — and negates that amount. statusReasonDescription carries
// the reversal reason; the lab has no reason code to put in
// statusReasonCode, so that stays null.
func reversalRow(p *Payment, original ReconciliationRow) ReconciliationRow {
	row := original
	date := bcDate(BusinessDate(firstNonEmpty(p.ReversedAt, p.UpdatedAt)))
	row.ValueDate = date
	row.ReportDate = date
	row.ProcessedTimestamp = str(firstNonEmpty(p.ReversedAt, p.UpdatedAt))
	if original.DebitAmount != nil {
		negated := -*original.DebitAmount
		row.DebitAmount = &negated
	}
	if original.CreditAmount != nil {
		negated := -*original.CreditAmount
		row.CreditAmount = &negated
	}
	row.StatusReasonDescription = str(p.ReversalReason)
	return row
}

// isReversalRow tells a reversal booking from the one it reverses by its
// negative amount, the only thing the docs say distinguishes it.
func isReversalRow(r ReconciliationRow) bool {
	return (r.DebitAmount != nil && *r.DebitAmount < 0) || (r.CreditAmount != nil && *r.CreditAmount < 0)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Rejections returns the payments that could not be processed, instructed
// on the transaction date. By default the reference lists three kinds:
// missing funding, pending processing and rejected. Outgoing payments only:
// the report is about payments you instructed.
func Rejections(payments []*Payment, q RejectionQuery) []RejectionRow {
	rows := make([]RejectionRow, 0)
	for _, p := range payments {
		if BusinessDate(p.CreatedAt) != q.TransactionDate {
			continue
		}
		kind, ok := rejectionKindOf(p.State)
		if !ok {
			continue
		}
		switch {
		case kind == rejectionMissingFunds && !q.IncludeMissingFunds,
			(kind == rejectionPending || kind == rejectionPendingBooked) && !q.IncludeReceived,
			kind == rejectionPendingBooked && q.ExcludeBooked:
			continue
		}
		status, reason := rejectionLabels(kind, p)
		txnDate := bcDate(BusinessDate(p.CreatedAt))
		rows = append(rows, RejectionRow{
			PTxndate:               txnDate,
			ReportDate:             bcDate(q.ReportDate),
			Account:                q.IBAN.of(p.FromAccountID),
			AccountCurrency:        str(p.Currency),
			ValueDate:              txnDate,
			PaymentAmount:          amountFloat(p.Amount),
			PaymentCurrency:        str(p.Currency),
			TransferCurrency:       str(p.Currency),
			DestinationIban:        str(p.ToIBAN),
			PaymentReferenceNumber: str(p.ReferenceNumber),
			UserReferenceNumber:    userReference(p),
			FileReferenceNumber:    &empty,
			SourceType:             str("Single payment"),
			Status:                 &status,
			StatusReason:           &reason,
		})
	}
	// Same reason as the reconciliation report, even though this one is
	// not paged: a report whose row order changes between identical calls
	// is one nobody can diff.
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if derefStr(a.ValueDate) != derefStr(b.ValueDate) {
			return derefStr(a.ValueDate) < derefStr(b.ValueDate)
		}
		return derefStr(a.PaymentReferenceNumber) < derefStr(b.PaymentReferenceNumber)
	})
	return rows
}

// empty is the value the reference's example sends for a field that does
// not apply on a single payment (fileReferenceNumber: ""), where the
// reconciliation report would send null.
var empty = ""

type rejectionKind int

const (
	rejectionRejected rejectionKind = iota
	rejectionMissingFunds
	// rejectionPending has not been booked yet; rejectionPendingBooked has
	// (value already moved), which is what ExcludeBooked drops.
	rejectionPending
	rejectionPendingBooked
)

// rejectionKindOf reports whether a payment in state s belongs in the
// report, and as which kind. A reversal is not one of the report's default
// statuses, and IncludeReversals only covers direct debits, so an outgoing
// Reversed payment never appears.
func rejectionKindOf(s NotificationType) (rejectionKind, bool) {
	switch s {
	case NotificationOutgoingPaymentRejected:
		return rejectionRejected, true
	case NotificationMissingFunding:
		return rejectionMissingFunds, true
	case NotificationOutgoingPaymentBooked:
		return rejectionPendingBooked, true
	case NotificationPaymentRouting, NotificationOutgoingDirectDebitPendingProcessing:
		return rejectionPending, true
	}
	return 0, false
}

// rejectionLabels is the status and statusReason a row carries. The docs
// give "Rejected" for a rejection and "Insufficient Funds" for missing
// funding. For pending processing they disagree: the guide says the status
// is blank, while the API reference's example row is a pending payment
// (the parameter that includes those is IncludeReceived) with status
// "Received" and an empty statusReason. This follows the API reference,
// the wire contract. The reasons are descriptive text, the lab's own,
// except a rejection a trap gave a reason (Payment.RejectionReasonCode):
// that reads "<code> <description>", as a return's remittance does.
func rejectionLabels(kind rejectionKind, p *Payment) (status, reason string) {
	switch kind {
	case rejectionRejected:
		if given := strings.TrimSpace(p.RejectionReasonCode + " " + p.RejectionReasonDescription); given != "" {
			return "Rejected", given
		}
		return "Rejected", "Payment rejected by the beneficiary bank (simulated)"
	case rejectionMissingFunds:
		return "Insufficient Funds", "Insufficient funds on the debtor account (simulated)"
	}
	return "Received", ""
}

// bcDate renders a date the way both reports' examples do:
// 2024-12-30T00:00:00+00:00.
func bcDate(ts string) *string {
	if d := datePart(ts); d != "" {
		return str(d + "T00:00:00+00:00")
	}
	return nil
}

// userReference is userReferenceNumber: the debtor reference the payment
// instruction carried. The lab's payouts are created without one, so they
// stand in with the reference the caller sent the payment under
// (externalRef); whether the real bridge carries that as the debtor
// reference is not something this mock can confirm.
func userReference(p *Payment) *string {
	return str(firstNonEmpty(p.Reference, p.SettlementID))
}

// page applies PageNumber/PageSize. Page numbers are 1-based; a page past
// the end is empty rather than an error, because a sweep walking pages
// until it sees fewer rows than it asked for must be able to stop.
func page(rows []ReconciliationRow, number, size int) []ReconciliationRow {
	if size <= 0 {
		return rows
	}
	if number <= 0 {
		number = 1
	}
	start := (number - 1) * size
	if start >= len(rows) {
		return []ReconciliationRow{}
	}
	end := start + size
	if end > len(rows) {
		end = len(rows)
	}
	return rows[start:end]
}

// withinDates compares on the date prefix, inclusive at both ends. An empty
// bound does not filter.
func withinDates(timestamp, from, to string) bool {
	d := datePart(timestamp)
	if d == "" {
		return from == "" && to == ""
	}
	if from != "" && d < datePart(from) {
		return false
	}
	if to != "" && d > datePart(to) {
		return false
	}
	return true
}

func datePart(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

// amountFloat parses the decimal string these records carry. A malformed
// amount reports zero rather than failing the whole report: one bad row
// must not hide every good one from a sweep.
func amountFloat(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}

func containsFold(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(strings.TrimSpace(s), v) {
			return true
		}
	}
	return false
}

// str returns a pointer to s, or nil when s is empty — the report sends
// null for a field that does not apply, and a client may be reading that
// distinction.
func str(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
