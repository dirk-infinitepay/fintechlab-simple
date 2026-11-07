package e2e

import (
	"context"
	"fmt"
	"time"
)

type aciSimulateResp struct {
	ID string `json:"id"`
}

// aciSentRecord mirrors cmd/aci's sentRecord JSON shape -- only the
// fields this scenario asserts on are decoded.
type aciSentRecord struct {
	Notification struct {
		Payload struct {
			MerchantTransactionID string `json:"merchantTransactionId"`
			PluginType            string `json:"pluginType"`
			PresentationAmount    string `json:"presentationAmount"`
			PresentationCurrency  string `json:"presentationCurrency"`
		} `json:"payload"`
	} `json:"notification"`
	CiphertextHex  string `json:"ciphertext_hex"`
	IVHex          string `json:"iv_hex"`
	TagHex         string `json:"tag_hex"`
	DeliveryStatus string `json:"delivery_status"`
}

// AciPaymentNotification confirms the ACI card-gateway webhook mock
// (docs/ARCHITECTURE-phase3-corrections.md section 2): triggering "a card
// payment just happened" (the lab API's POST /sim/payments on aci) produces a genuinely AES-256-GCM-encrypted
// notification -- non-empty ciphertext/IV/tag, carrying the exact
// merchantTransactionId/pluginType this scenario asked for -- and actually
// attempts delivery, not just accepts the trigger. internal/aci's own
// known-answer test already proves the algorithm is byte-interoperable
// with buddy's real decryptor (ACI's own published reference vector); this
// scenario instead proves the mock's HTTP surface genuinely runs that
// encryption end to end and records what happened, the same "observable
// side effect" bar every other scenario in this lab holds itself to.
// Delivery target defaults (compose.yml) to receiver's raw capture sink
// (section 6) so this settles fast and deterministically within the lab
// network -- a real buddy settle-aci-webhook is the intended eventual
// consumer (docs/local-domains.md), not exercised here.
func AciPaymentNotification() Scenario {
	return Scenario{
		Name: "aci-payment-notification",
		Run: func(ctx context.Context, env *Env, state *State) error {
			txID := fmt.Sprintf("harness-tx-%d", time.Now().UnixNano())
			req := map[string]string{
				"merchant_transaction_id": txID,
				"payment_type":            "RX",
				"payment_brand":           "VISA",
				"presentation_amount":     "42.50",
				"presentation_currency":   "EUR",
				"plugin_type":             "SHOPFY",
				"source":                  "OPP",
				"result_code":             "000.300.100",
				"result_description":      "Risk check successful",
			}
			var sim aciSimulateResp
			status, err := env.Lab.Post(ctx, Sim("aci", "payments"), req, &sim)
			if err != nil {
				return fmt.Errorf("aci POST /sim/payments: %w", err)
			}
			if status != 202 {
				return fmt.Errorf("aci POST /sim/payments: status %d, want 202", status)
			}
			if sim.ID == "" {
				return fmt.Errorf("aci POST /sim/payments: empty id in response")
			}

			var rec aciSentRecord
			if err := PollUntil(ctx, 10*time.Second, 100*time.Millisecond, func() (bool, error) {
				if _, err := env.Lab.Get(ctx, Sim("aci", "sent/"+sim.ID), &rec); err != nil {
					return false, err
				}
				if rec.DeliveryStatus == "" || rec.DeliveryStatus == "pending" {
					return false, nil
				}
				return true, nil
			}); err != nil {
				return fmt.Errorf("waiting for delivery attempt to settle: %w", err)
			}

			if rec.Notification.Payload.MerchantTransactionID != txID {
				return fmt.Errorf("merchantTransactionId = %q, want %q", rec.Notification.Payload.MerchantTransactionID, txID)
			}
			if rec.Notification.Payload.PluginType != "SHOPFY" {
				return fmt.Errorf("pluginType = %q, want SHOPFY", rec.Notification.Payload.PluginType)
			}
			if rec.CiphertextHex == "" || rec.IVHex == "" || rec.TagHex == "" {
				return fmt.Errorf("expected non-empty encrypted material, got ciphertext=%d iv=%d tag=%d hex chars", len(rec.CiphertextHex), len(rec.IVHex), len(rec.TagHex))
			}
			if rec.DeliveryStatus != "delivered" {
				return fmt.Errorf("delivery_status = %q, want delivered to ACI_WEBHOOK_TARGET_URL", rec.DeliveryStatus)
			}
			return nil
		},
	}
}
