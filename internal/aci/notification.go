// Package aci implements ACI's real card-gateway webhook contract: the
// AES-256-GCM payload encryption (crypto.go) and the notification wire shape
// (this file), per docs/ARCHITECTURE-phase3-corrections.md section 2; plus
// the mock's own record of what it sent and how a reset cancels deliveries
// in flight (sent.go).
// Source of truth: buddy's own
// apps/settle-aci-webhook/src/webhook/aci-decryption.service.ts:39-101
// (AciWebhookNotificationPayload). This lab models only the fields that
// section 2 lists as in scope — ACI's real interface carries several more
// risk/card sub-fields buddy's decoder tolerates via an index signature, but
// this harness's own trigger (POST /internal/simulate-payment) never
// produces them, so they are not modeled here.
package aci

// Notification is ACI's `{ type, payload }` webhook envelope. `Type` is the
// category ACI sends (PAYMENT/REGISTRATION/SCHEDULE/RISK); this lab's own
// trigger only ever produces PAYMENT.
type Notification struct {
	Type    string   `json:"type,omitempty"`
	Payload *Payload `json:"payload,omitempty"`
}

// Payload is the transaction detail carried inside a Notification. Every
// JSON tag matches ACI's real wire casing verbatim (aci-decryption.service.ts:42-54).
type Payload struct {
	ID                    string         `json:"id,omitempty"`
	MerchantTransactionID string         `json:"merchantTransactionId,omitempty"`
	PaymentType           string         `json:"paymentType,omitempty"`
	PaymentBrand          string         `json:"paymentBrand,omitempty"`
	PresentationAmount    string         `json:"presentationAmount,omitempty"`
	PresentationCurrency  string         `json:"presentationCurrency,omitempty"`
	PluginType            string         `json:"pluginType,omitempty"`
	Source                string         `json:"source,omitempty"`
	ShortID               string         `json:"shortId,omitempty"`
	Timestamp             string         `json:"timestamp,omitempty"`
	ChannelName           string         `json:"channelName,omitempty"`
	PaymentMethod         string         `json:"paymentMethod,omitempty"`
	NDC                   string         `json:"ndc,omitempty"`
	Result                *Result        `json:"result,omitempty"`
	ResultDetails         *ResultDetails `json:"resultDetails,omitempty"`
	Card                  *Card          `json:"card,omitempty"`
}

// Result is the top-level outcome code (aci-decryption.service.ts:55).
type Result struct {
	Code        string `json:"code,omitempty"`
	Description string `json:"description,omitempty"`
}

// ResultDetails carries ACI's risk-engine detail. RiskOrderId/TransactionId
// are capitalized, action is not — ACI's real mixed casing verbatim
// (aci-decryption.service.ts:57-59), not a typo, do not "fix" it.
type ResultDetails struct {
	RiskOrderID   string `json:"RiskOrderId,omitempty"`
	TransactionID string `json:"TransactionId,omitempty"`
	Action        string `json:"action,omitempty"`
}

// Card is the tokenized card detail (aci-decryption.service.ts:81-88).
type Card struct {
	Bin         string `json:"bin,omitempty"`
	Last4Digits string `json:"last4Digits,omitempty"`
	Holder      string `json:"holder,omitempty"`
	ExpiryMonth string `json:"expiryMonth,omitempty"`
	ExpiryYear  string `json:"expiryYear,omitempty"`
	Type        string `json:"type,omitempty"`
	Country     string `json:"country,omitempty"`
}
