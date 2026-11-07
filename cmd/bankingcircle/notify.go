package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/webduvet/fintechlab-simple/internal/bankingcircle"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labclock"
)

// A notification's "payment" object comes in two shapes, both copied from
// Banking Circle's payload examples
// (https://docs.bankingcircleconnect.com/docs/payload-examples).
//
// The booked events (OutgoingPaymentBooked, IncomingPaymentBooked) are the
// short one, bookedPayment: ids, dates, a flat signed amount, and the
// remittance. Every other payment event is notificationPayment, which
// carries the payment's status and the one side of it that is ours:
// debtorInformation for money leaving our account, with
// creditorInformation null, and creditorInformation for money arriving,
// with debtorInformation null. Amounts are JSON numbers throughout.

// notificationPayment is the "payment" object of a status-carrying event:
// OutgoingPaymentProcessed, OutgoingPaymentRejected, MissingFunding,
// Reversed, IncomingPaymentProcessed.
type notificationPayment struct {
	PaymentID            string `json:"paymentId"`
	TransactionReference string `json:"transactionReference"`
	// Status is the payment's status (Processed, Rejected, Reversed,
	// MissingFunding), not the notification's type.
	Status string `json:"status"`
	// Return is true on an incoming return payment and null otherwise.
	Return *bool `json:"return"`
	// Exactly one side is set; the other is sent as null.
	DebtorInformation   *notificationParty    `json:"debtorInformation"`
	CreditorInformation *notificationParty    `json:"creditorInformation"`
	Transfer            *notificationTransfer `json:"transfer,omitempty"`
}

// bookedPayment is the "payment" object of OutgoingPaymentBooked and
// IncomingPaymentBooked. The amount is signed by the effect on the balance
// ("the amount will correspond with the effect on the balance"): a payout's
// booking is negative, money in is positive, and so is a reversal's
// booking, which puts the payout's money back.
type bookedPayment struct {
	PaymentID            string                `json:"paymentId"`
	TransactionReference string                `json:"transactionReference"`
	ValueDate            string                `json:"valueDate"`
	TransactionDate      string                `json:"transactionDate"`
	Amount               json.Number           `json:"amount"`
	Currency             string                `json:"currency"`
	Transfer             *notificationTransfer `json:"transfer,omitempty"`
}

type notificationTransfer struct {
	Amount                *notificationMoney      `json:"amount,omitempty"`
	RemittanceInformation *notificationRemittance `json:"remittanceInformation,omitempty"`
}

// notificationMoney is the vendor's {currency, amount} object.
type notificationMoney struct {
	Currency string      `json:"currency"`
	Amount   json.Number `json:"amount"`
}

type notificationInstruction struct {
	Amount *notificationMoney `json:"amount,omitempty"`
}

// notificationParty is our side of a payment: the account it left
// (debtorInformation, with debitAmount and the instruction's amount) or
// the account it arrived on (creditorInformation, with creditAmount).
type notificationParty struct {
	AccountID    string                   `json:"accountId"`
	DebitAmount  *notificationMoney       `json:"debitAmount,omitempty"`
	CreditAmount *notificationMoney       `json:"creditAmount,omitempty"`
	Instruction  *notificationInstruction `json:"instruction,omitempty"`
}

// notificationRemittance sends all four lines, null when unused, as the
// vendor example does.
type notificationRemittance struct {
	Line1 *string `json:"line1"`
	Line2 *string `json:"line2"`
	Line3 *string `json:"line3"`
	Line4 *string `json:"line4"`
}

func remittanceOf(lines []string) *notificationRemittance {
	if len(lines) == 0 {
		return nil
	}
	at := func(i int) *string {
		if i < len(lines) && lines[i] != "" {
			return &lines[i]
		}
		return nil
	}
	return &notificationRemittance{Line1: at(0), Line2: at(1), Line3: at(2), Line4: at(3)}
}

// paymentDetail is the "payment" object for p's current state, in the shape
// the vendor's example for that event type has.
func paymentDetail(p *bankingcircle.Payment) any {
	money := &notificationMoney{Currency: p.Currency, Amount: json.Number(p.Amount)}
	switch p.State {
	case bankingcircle.NotificationOutgoingPaymentBooked, bankingcircle.NotificationIncomingPaymentBooked:
		amount := p.Amount
		if p.State == bankingcircle.NotificationOutgoingPaymentBooked && p.ReversedAt == "" {
			amount = "-" + amount
		}
		detail := &bookedPayment{
			PaymentID:            p.ID,
			TransactionReference: p.ReferenceNumber,
			ValueDate:            bookingDate(p.UpdatedAt),
			TransactionDate:      bookingDate(p.CreatedAt),
			Amount:               json.Number(amount),
			Currency:             p.Currency,
		}
		if rem := remittanceOf(p.Remittance); rem != nil {
			detail.Transfer = &notificationTransfer{RemittanceInformation: rem}
		}
		return detail
	}

	detail := &notificationPayment{
		PaymentID:            p.ID,
		TransactionReference: p.ReferenceNumber,
		Status:               bankingcircle.PaymentStatus(p.State),
	}
	if p.Return {
		detail.Return = &p.Return
	}
	if p.State == bankingcircle.NotificationIncomingPaymentProcessed {
		detail.CreditorInformation = &notificationParty{AccountID: p.ToAccountID, CreditAmount: money}
		detail.Transfer = &notificationTransfer{Amount: money}
	} else {
		// Nothing was transferred on a rejection or missing funding, so
		// only a processed or reversed payout carries transfer.amount.
		detail.DebtorInformation = &notificationParty{
			AccountID:   p.FromAccountID,
			DebitAmount: money,
			Instruction: &notificationInstruction{Amount: money},
		}
		if p.State == bankingcircle.NotificationOutgoingPaymentProcessed || p.State == bankingcircle.NotificationReversed {
			detail.Transfer = &notificationTransfer{Amount: money}
		}
	}
	if rem := remittanceOf(p.Remittance); rem != nil {
		if detail.Transfer == nil {
			detail.Transfer = &notificationTransfer{}
		}
		detail.Transfer.RemittanceInformation = rem
	}
	return detail
}

// bookingDate renders the business day a timestamp books on the way the
// booked examples do: 2024-07-26T00:00:00.
func bookingDate(ts string) string {
	if d := bankingcircle.BusinessDate(ts); d != "" {
		return d + "T00:00:00"
	}
	return ""
}

// onTransition fires on every notification the engine produces.
//
// It routes by subscribed event type and target rather than fanning out to
// every active subscription. That is the behaviour a client is written
// against: it asks for IncomingPaymentProcessed on one account and expects
// not to be handed every outgoing payment in the system. Fanning out to
// everyone made the target and event-type configuration decorative, so a
// client with a broken subscription looked like a working one.
//
// It never blocks the engine: the dispatcher batches and delivers on its
// own goroutines, so a down or slow subscriber never stalls payments.
func (a *app) onTransition(p *bankingcircle.Payment) {
	eventType := string(p.State)

	// A payout that leaves the safeguarding account has to arrive
	// somewhere. Notifying a subscriber that a payment processed is not
	// the same statement as the beneficiary's bank holding the money, and
	// a lab that only did the first teaches that the two are one thing.
	if p.State == bankingcircle.NotificationOutgoingPaymentProcessed {
		go a.creditBeneficiaryBank(*p)
	}

	// The accounts this payment touches are the targets a subscription may
	// have scoped itself to.
	recipients := a.subs.Recipients(eventType, p.ToAccountID, p.FromAccountID)
	if len(recipients) == 0 {
		log.Printf("banking-circle: %s for payment %s matched no active subscription", eventType, p.ID)
		return
	}
	// transactionReference is the bank's own reference for the payment
	// (010F10…, the report's paymentReferenceNumber), as in Banking Circle's
	// webhook examples — never a reference the sender chose; that travels in
	// the remittance information.
	detail := paymentDetail(p)
	for _, rec := range recipients {
		// From the payment's run: if a reset has cleared the payment, the
		// dispatcher drops this rather than queue it.
		n := newNotification(rec, eventType, detail)
		n.Trap = p.Trap() // the lab's mark, for the notifications log; never sent
		a.dispatch.EnqueueFrom(p.Epoch(), rec.Subscription, n)
	}
}

// newNotification builds one envelope entry, putting the detail under the
// property the event type actually uses.
func newNotification(rec bankingcircle.Recipient, eventType string, detail any) bankingcircle.Notification {
	n := bankingcircle.Notification{
		EventID:             "evt_" + shortID(),
		SubscriptionID:      rec.Subscription.ID,
		SubscriptionEventID: rec.Event.ID,
		NotificationType:    eventType,
		Timestamp:           labclock.Now().Format(time.RFC3339),
	}
	if bankingcircle.UsesPayloadProperty(eventType) {
		n.Payload = detail
	} else {
		n.Payment = detail
	}
	return n
}

// clientTest implements POST /api/v1/notificationselfservice/clienttest/{id}
// -- a real sandbox feature: it sends a synthetic notification through the
// exact same encrypted delivery pipe every other notification uses, so a
// developer can confirm their endpoint is reachable without waiting for a
// real payment.
//
// It flushes immediately rather than waiting for the batch timer, because
// the whole point is an answer now.
func (a *app) clientTest(w http.ResponseWriter, r *http.Request) {
	sub, err := a.subs.Get(r.PathValue("subscriptionId"))
	if err != nil {
		writeSubscriptionError(w, err)
		return
	}
	eventType := string(bankingcircle.NotificationPaymentStatus)
	// A clienttest fires against the subscription itself, so it is not
	// filtered by event type or target the way a real event is -- it is a
	// reachability check, not an event.
	rec := bankingcircle.Recipient{Subscription: sub, Event: syntheticEvent(sub, eventType)}
	detail := map[string]any{
		"EndToEndId":        "clienttest_" + shortID(),
		"TransactionStatus": "PendingProcessing",
	}
	a.dispatch.Enqueue(sub, newNotification(rec, eventType, detail))
	a.dispatch.Flush(sub.ID)
	httputilx.WriteJSON(w, 200, map[string]any{
		"status":         "sent",
		"subscriptionId": sub.ID,
		"eventType":      eventType,
	})
}

// syntheticEvent returns the subscription's own event for eventType if it
// has one, so a clienttest carries a real subscriptionEventId, and a
// stand-in otherwise.
func syntheticEvent(sub *bankingcircle.Subscription, eventType string) *bankingcircle.SubscriptionEvent {
	for _, e := range sub.Events {
		if e.EventType == eventType {
			return e
		}
	}
	return &bankingcircle.SubscriptionEvent{ID: "subev_clienttest", EventType: eventType, SubscriptionID: sub.ID}
}

// sendEncrypted is the dispatcher's Send: marshal the envelope, encrypt it
// with this subscription's own key, and POST it. ctx is the run's: a reset
// aborts the POST.
//
// The key is per subscription, not per server. A client that changed its
// encryption key with a PUT and kept decrypting successfully was being
// told a lie about its own configuration.
func (a *app) sendEncrypted(ctx context.Context, sub *bankingcircle.Subscription, env bankingcircle.Envelope) error {
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal notification batch: %w", err)
	}
	key := []byte(sub.EncryptionKey)
	if len(key) != bankingcircle.EncryptionKeyLength {
		return fmt.Errorf("subscription %s has a %d-character encryption key, need %d",
			sub.ID, len(key), bankingcircle.EncryptionKeyLength)
	}
	ciphertext, nonce, tag, checksum, err := encryptNotification(body, key)
	if err != nil {
		return fmt.Errorf("encrypt notification: %w", err)
	}
	status, err := a.postNotification(ctx, sub.Endpoint, ciphertext, nonce, tag, checksum, sub.Version)
	a.recordNotification(sub, env, status, err)
	return err
}

// encryptNotification implements Banking Circle's real webhook encryption,
// exactly:
//  1. checksum = SHA-256 of the original JSON string's UTF-8 bytes.
//  2. UTF-16LE-encode that JSON string.
//  3. AES-256-GCM-encrypt the UTF-16LE bytes with a random 12-byte nonce
//     and key = the raw UTF-8 bytes of the 32-character configured key
//     (NOT base64-decoded).
//  4. Split Seal's output into ciphertext and the trailing 16-byte auth
//     tag -- real BC's wire format keeps them separate, unlike Go's GCM
//     default.
func encryptNotification(jsonBody, key []byte) (ciphertext, nonce, tag, checksum []byte, err error) {
	sum := sha256.Sum256(jsonBody)
	checksum = sum[:]

	utf16le := utf16LEBytes(string(jsonBody))

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, nil, nil, err
	}
	sealed := gcm.Seal(nil, nonce, utf16le, nil)
	tagLen := gcm.Overhead()
	ciphertext = sealed[:len(sealed)-tagLen]
	tag = sealed[len(sealed)-tagLen:]
	return ciphertext, nonce, tag, checksum, nil
}

func utf16LEBytes(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, len(units)*2)
	for i, u := range units {
		binary.LittleEndian.PutUint16(out[i*2:], u)
	}
	return out
}

func (a *app) postNotification(ctx context.Context, dest string, ciphertext, nonce, tag, checksum []byte, version int) (int, error) {
	if err := a.list.Allowed(dest); err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dest, bytes.NewReader(ciphertext))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Nonce", base64.StdEncoding.EncodeToString(nonce))
	req.Header.Set("AuthenticationTag", base64.StdEncoding.EncodeToString(tag))
	req.Header.Set("Checksum", base64.StdEncoding.EncodeToString(checksum))
	req.Header.Set("SubscriptionVersion", strconv.Itoa(version))
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("notification: destination returned %s", resp.Status)
	}
	return resp.StatusCode, nil
}

// creditBeneficiaryBank posts an arriving payment to the receiving bank.
//
// Fire-and-forget on its own goroutine, and a failure is logged rather than
// retried: this hop stands in for an interbank rail, and modelling its
// retry semantics would be inventing a protocol rather than simulating one.
// What matters here is that the money is observable on the other side.
//
// Disabled when BANK_CREDIT_URL is empty, so a stack without a beneficiary
// bank behaves exactly as it did before.
//
// The POST goes through the payment's run: if a reset has cleared the
// payment by the time this goroutine gets to it, nothing is credited, and a
// reset that lands while the POST is on the wire aborts it.
func (a *app) creditBeneficiaryBank(p bankingcircle.Payment) {
	if a.bankCreditURL == "" {
		return
	}
	// The rail names the beneficiary by IBAN, or by account number and
	// sort code where there is none — how a UK merchant is paid. Without
	// either there is nothing for the receiving bank to open an account
	// against, and inventing an identifier here would put money into an
	// account nobody can reconcile.
	iban := p.ToIBAN
	if iban == "" && p.ToAccountNumber == "" {
		log.Printf("banking-circle: payment %s names no beneficiary account (IBAN or account number); not crediting the beneficiary bank", p.ID)
		return
	}
	beneficiary := iban
	if beneficiary == "" {
		beneficiary = strings.TrimSpace(p.ToFinancialInstitution + " " + p.ToAccountNumber)
	}
	if err := a.list.Allowed(a.bankCreditURL); err != nil {
		log.Printf("banking-circle: beneficiary bank %s not allowlisted: %v", a.bankCreditURL, err)
		return
	}
	body, err := json.Marshal(map[string]any{
		"iban":                  iban,
		"account_number":        p.ToAccountNumber,
		"financial_institution": p.ToFinancialInstitution,
		"holder":                p.ToHolder,
		"amount":                p.Amount,
		"currency":              p.Currency,
		"reference":             firstNonEmpty(p.Reference, p.SettlementID, p.ID),
	})
	if err != nil {
		log.Printf("banking-circle: credit payload for %s: %v", p.ID, err)
		return
	}
	a.engine.Run().Do(p.Epoch(), func(run context.Context) {
		ctx, cancel := context.WithTimeout(run, 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.bankCreditURL, bytes.NewReader(body))
		if err != nil {
			log.Printf("banking-circle: credit request for %s: %v", p.ID, err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := a.client.Do(req)
		if err != nil {
			log.Printf("banking-circle: crediting beneficiary bank for %s: %v", p.ID, err)
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			log.Printf("banking-circle: beneficiary bank returned %s for payment %s", resp.Status, p.ID)
			return
		}
		log.Printf("banking-circle: %s %s credited to %s at the beneficiary bank", p.Amount, p.Currency, beneficiary)
	})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
