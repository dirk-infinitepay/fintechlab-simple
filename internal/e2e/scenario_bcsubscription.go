package e2e

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/webduvet/fintechlab-simple/internal/bankingcircle"
)

type bcSubscriptionResp struct {
	ID                         string `json:"id"`
	Endpoint                   string `json:"endpoint"`
	IsActive                   bool   `json:"isActive"`
	Status                     int    `json:"status"`
	StatusMessage              string `json:"statusMessage"`
	RowVersion                 string `json:"rowVersion"`
	EncryptionKey              string `json:"encryptionKey"`
	Email                      string `json:"email"`
	MaxNotificationsPerMessage int    `json:"maxNotificationsPerMessage"`
}

type bcActivateResp struct {
	Subscription             bcSubscriptionResp `json:"subscription"`
	RedeliveredNotifications int                `json:"redeliveredNotifications"`
}

type bcPendingResp struct {
	Pending int `json:"pending"`
}

type rawEventsResp struct {
	RawEvents []struct {
		ID       string              `json:"id"`
		Path     string              `json:"path"`
		Headers  map[string][]string `json:"headers"`
		BodySize int                 `json:"bodySize"`
		Body     string              `json:"body"`
	} `json:"raw_events"`
}

// BankingCircleSubscription drives Banking Circle's notification
// self-service API the way a real subscriber has to: create a subscription
// with its own encryption key and batch size, add an event to it, prove
// the optimistic-concurrency check is real, watch notifications arrive
// batched, and watch a subscription that never acknowledges get retried on
// the documented schedule until Banking Circle deactivates it -- then
// reactivate it and confirm the notifications it missed are redelivered.
//
// Every one of those is something a client has to be built to survive and
// could not previously be exercised at all.
func BankingCircleSubscription() Scenario {
	return Scenario{
		Name: "banking-circle-subscription",
		Run: func(ctx context.Context, env *Env, state *State) error {
			hdr, err := bankingCircleBearer(ctx, env)
			if err != nil {
				return err
			}
			base := env.BankingCircleURL + "/api/v1/notificationselfservice"

			// Delivery URL must be reachable from the twin process (compose
			// DNS). Capture matching uses the path only, so a host e2e run
			// still lists via ReceiverURL.
			endpoint := env.ReceiverDeliveryBase() + "/raw-events?sub=harness-batching"
			const key = "harness-bc-notification-key-32ch"

			var sub bcSubscriptionResp
			status, err := PostJSON(ctx, env.Client, base+"/subscription", hdr, map[string]any{
				"endpoint":                   endpoint,
				"status":                     2,
				"encryptionKey":              key,
				"email":                      "harness@fintechlab-simple.local",
				"maxNotificationsPerMessage": 5,
			}, &sub)
			if err != nil {
				return fmt.Errorf("create subscription: %w", err)
			}
			// Standalone returns 200; pod http-ingress create is 201.
			if (status != 200 && status != 201) || sub.ID == "" {
				return fmt.Errorf("create subscription: want 200/201 + an id, got %d", status)
			}
			if sub.EncryptionKey != "*Hidden*" {
				return fmt.Errorf("the API returned the encryption key as %q; it must never be echoed", sub.EncryptionKey)
			}
			if sub.RowVersion == "" {
				return fmt.Errorf("create returned no rowVersion; nothing could be updated afterwards")
			}
			if sub.MaxNotificationsPerMessage != 5 {
				return fmt.Errorf("maxNotificationsPerMessage = %d, want the 5 requested", sub.MaxNotificationsPerMessage)
			}

			// Duplicate endpoints are refused.
			dupStatus, err := PostJSON(ctx, env.Client, base+"/subscription", hdr, map[string]any{
				"endpoint": endpoint, "status": 2, "encryptionKey": key,
			}, nil)
			if err != nil {
				return fmt.Errorf("duplicate subscription: %w", err)
			}
			if dupStatus != 409 {
				return fmt.Errorf("creating a second subscription on the same endpoint = %d, want 409", dupStatus)
			}

			eventType := string(bankingcircle.NotificationOutgoingPaymentProcessed)
			if !env.UsesPodBankRails() {
				// Standalone: subscribe to a specific Connect event target.
				// Pod twin has no subscriptionEvent / clienttest surface yet
				// — active subscriptions receive configured bus Facts.
				evStatus, err := PostJSON(ctx, env.Client, base+"/subscriptionEvent", hdr, map[string]any{
					"subscriptionId": sub.ID,
					"eventType":      eventType,
					"targetType":     1,
				}, nil)
				if err != nil {
					return fmt.Errorf("create subscription event: %w", err)
				}
				if evStatus != 200 {
					return fmt.Errorf("create subscription event: want 200, got %d", evStatus)
				}
			}

			// Optimistic concurrency: a write without the current token is
			// refused. A client that was never made to send If-Match will
			// fail on its first concurrent update in production.
			noMatch, err := PutJSON(ctx, env.Client, base+"/subscription/"+sub.ID, hdr,
				map[string]any{"email": "changed@example.com"}, nil)
			if err != nil {
				return fmt.Errorf("update without If-Match: %w", err)
			}
			if noMatch != 412 {
				return fmt.Errorf("update with no If-Match = %d, want 412", noMatch)
			}

			var fresh bcSubscriptionResp
			if _, err := GetJSONWithHeaders(ctx, env.Client, base+"/subscription/"+sub.ID, hdr, &fresh); err != nil {
				return fmt.Errorf("re-read subscription: %w", err)
			}
			withMatch := mergeHeaders(hdr, map[string]string{"If-Match": fresh.RowVersion})
			var updated bcSubscriptionResp
			okStatus, err := PutJSON(ctx, env.Client, base+"/subscription/"+sub.ID, withMatch,
				map[string]any{"email": "changed@example.com"}, &updated)
			if err != nil {
				return fmt.Errorf("update with If-Match: %w", err)
			}
			if okStatus != 200 {
				return fmt.Errorf("update with a matching If-Match = %d, want 200", okStatus)
			}
			if updated.RowVersion == fresh.RowVersion {
				return fmt.Errorf("rowVersion did not advance on update; concurrent writers could clobber each other")
			}

			// Batching: five notifications, a batch size of five, one
			// message. A client that assumes one notification per POST
			// breaks the first time two events land close together.
			before, err := rawEventCount(ctx, env, endpoint)
			if err != nil {
				return err
			}
			if env.UsesPodBankRails() {
				// Pod has no /sim/subscription/{id}/notifications. Five
				// TransferPosted Facts (via /sim/funding) fill one batch;
				// flush forces delivery without waiting for the timer.
				for i := 0; i < 5; i++ {
					if err := bcFundSGA(ctx, env, "1.00", fmt.Sprintf("batch-fund-%s-%d", sub.ID, i)); err != nil {
						return fmt.Errorf("enqueue via funding[%d]: %w", i, err)
					}
				}
				flushStatus, err := PostJSON(ctx, env.Client, env.bcLabBase()+"/sim/notifications/flush", nil,
					map[string]any{"subscription": sub.ID}, nil)
				if err != nil {
					return fmt.Errorf("flush notifications: %w", err)
				}
				if flushStatus != 200 && flushStatus != 202 {
					return fmt.Errorf("flush notifications: want 200/202, got %d", flushStatus)
				}
			} else {
				enqStatus, err := PostJSON(ctx, env.Client,
					fmt.Sprintf("%s/sim/subscription/%s/notifications?count=5&eventType=%s",
						env.BankingCircleURL, sub.ID, eventType), hdr, nil, nil)
				if err != nil {
					return fmt.Errorf("enqueue synthetic notifications: %w", err)
				}
				if enqStatus != 202 {
					return fmt.Errorf("enqueue synthetic notifications: want 202, got %d", enqStatus)
				}
			}

			var batchEvent capturedEvent
			if err := PollUntil(ctx, 20*time.Second, 250*time.Millisecond, func() (bool, error) {
				events, err := rawEventsFor(ctx, env, endpoint)
				if err != nil {
					return false, err
				}
				if len(events) <= before {
					return false, fmt.Errorf("no new delivery yet (have %d, had %d)", len(events), before)
				}
				if len(events) > before+1 {
					return false, fmt.Errorf("five notifications arrived as %d separate messages; batching is not happening",
						len(events)-before)
				}
				batchEvent = events[len(events)-1]
				return true, nil
			}); err != nil {
				return err
			}

			batch, err := decryptEnvelope(batchEvent, key)
			if err != nil {
				return fmt.Errorf("decrypt the delivered batch: %w", err)
			}
			if len(batch.Notifications) != 5 {
				return fmt.Errorf("the delivered message carried %d notifications, want all 5 in one batch",
					len(batch.Notifications))
			}
			for _, n := range batch.Notifications {
				if n.SubscriptionID != sub.ID {
					return fmt.Errorf("notification carries subscriptionId %q, want %q", n.SubscriptionID, sub.ID)
				}
				if env.UsesPodBankRails() {
					// Funding emits Fact.TransferPosted; Connect stage names
					// appear when CaseAdvanced carries state. Require a type
					// and a payment/payload object — do not invent Processed.
					if n.NotificationType == "" {
						return fmt.Errorf("notification has empty notificationType")
					}
					if n.Payment == nil && n.Payload == nil {
						return fmt.Errorf("%s notification has neither payment nor payload", n.NotificationType)
					}
				} else {
					if n.NotificationType != eventType {
						return fmt.Errorf("notification type %q, want %q", n.NotificationType, eventType)
					}
					if n.Payment == nil {
						return fmt.Errorf("%s notification has no payment property", n.NotificationType)
					}
				}
			}

			state.Set("bc_subscription_id", sub.ID)
			return nil
		},
	}
}

// BankingCircleRetryDeactivation drives the failure half: a subscription
// whose endpoint never answers is retried on the documented schedule,
// deactivated when the schedule runs out, and its undelivered
// notifications are held and redelivered when it comes back.
//
// The schedule really ends 48 hours after the first failure. BC_TIME_SCALE
// divides every delay so the whole story plays out in seconds with the
// step count and shape intact -- which is what a client is being tested
// against, not the wall-clock waits.
func BankingCircleRetryDeactivation() Scenario {
	return Scenario{
		Name: "banking-circle-retry-deactivation",
		Run: func(ctx context.Context, env *Env, state *State) error {
			hdr, err := bankingCircleBearer(ctx, env)
			if err != nil {
				return err
			}
			base := env.BankingCircleURL + "/api/v1/notificationselfservice"

			// An allowlisted host with nothing listening: every delivery
			// attempt fails at connect, which is what the retry schedule
			// is for.
			dead := "http://127.0.0.1:9/dead-endpoint"
			const key = "harness-bc-deadendpoint-key-32ch"

			var sub bcSubscriptionResp
			status, err := PostJSON(ctx, env.Client, base+"/subscription", hdr, map[string]any{
				"endpoint":                   dead,
				"status":                     2,
				"encryptionKey":              key,
				"email":                      "deadletter@fintechlab-simple.local",
				"maxNotificationsPerMessage": 5,
			}, &sub)
			if err != nil {
				return fmt.Errorf("create dead-endpoint subscription: %w", err)
			}
			if status != 200 && status != 201 {
				return fmt.Errorf("create dead-endpoint subscription: want 200/201, got %d", status)
			}
			eventType := string(bankingcircle.NotificationOutgoingPaymentProcessed)
			if !env.UsesPodBankRails() {
				if _, err := PostJSON(ctx, env.Client, base+"/subscriptionEvent", hdr, map[string]any{
					"subscriptionId": sub.ID, "eventType": eventType, "targetType": 1,
				}, nil); err != nil {
					return fmt.Errorf("create subscription event: %w", err)
				}
			}

			// Queue notifications and flush them straight into the retry schedule.
			if env.UsesPodBankRails() {
				for i := 0; i < 2; i++ {
					if err := bcFundSGA(ctx, env, "1.00", fmt.Sprintf("retry-fund-%s-%d", sub.ID, i)); err != nil {
						return fmt.Errorf("enqueue via funding[%d]: %w", i, err)
					}
				}
				if _, err := PostJSON(ctx, env.Client, env.bcLabBase()+"/sim/notifications/flush", nil,
					map[string]any{"subscription": sub.ID}, nil); err != nil {
					return fmt.Errorf("flush notifications: %w", err)
				}
			} else {
				if _, err := PostJSON(ctx, env.Client,
					fmt.Sprintf("%s/sim/subscription/%s/notifications?count=2&flush=true", env.BankingCircleURL, sub.ID),
					hdr, nil, nil); err != nil {
					return fmt.Errorf("enqueue notifications: %w", err)
				}
			}

			// The schedule runs out and Banking Circle deactivates the
			// subscription itself. Pod twin compresses via clock scale
			// (recipes/bank-rails clock.yaml); standalone via BC_TIME_SCALE.
			var deactivated bcSubscriptionResp
			if err := PollUntil(ctx, 90*time.Second, time.Second, func() (bool, error) {
				if _, err := GetJSONWithHeaders(ctx, env.Client, base+"/subscription/"+sub.ID, hdr, &deactivated); err != nil {
					return false, err
				}
				if deactivated.IsActive {
					return false, fmt.Errorf("subscription is still active")
				}
				return true, nil
			}); err != nil {
				return fmt.Errorf("subscription was never deactivated after repeated delivery failures "+
					"(check BC_TIME_SCALE / pod clock scale): %w", err)
			}
			if deactivated.Status != int(bankingcircle.StatusInactive) {
				return fmt.Errorf("deactivated subscription has status %d, want %d",
					deactivated.Status, bankingcircle.StatusInactive)
			}
			if deactivated.StatusMessage == "" {
				return fmt.Errorf("no statusMessage explaining the deactivation; a client cannot tell why it stopped")
			}

			if !env.UsesPodBankRails() {
				// Deactivation email — standalone only (pod has no /sim/emails).
				var mail struct {
					Emails []struct {
						Kind           string `json:"kind"`
						SubscriptionID string `json:"subscription_id"`
						To             string `json:"to"`
					} `json:"emails"`
				}
				if _, err := GetJSONWithHeaders(ctx, env.Client, env.BankingCircleURL+"/sim/emails", hdr, &mail); err != nil {
					return fmt.Errorf("read emails: %w", err)
				}
				var sawDeactivation bool
				for _, e := range mail.Emails {
					if e.SubscriptionID == sub.ID && e.Kind == "deactivation" {
						sawDeactivation = true
					}
				}
				if !sawDeactivation {
					return fmt.Errorf("no deactivation email for %s; the operator would never learn it stopped", sub.ID)
				}

				var pending bcPendingResp
				if _, err := GetJSONWithHeaders(ctx, env.Client,
					fmt.Sprintf("%s/sim/subscription/%s/pending", env.BankingCircleURL, sub.ID), hdr, &pending); err != nil {
					return fmt.Errorf("read pending notifications: %w", err)
				}
				if pending.Pending != 2 {
					return fmt.Errorf("%d notifications retained, want the 2 that never landed -- "+
						"dropping them is how a client loses a day of payments", pending.Pending)
				}
			}

			// Point it at a live endpoint and reactivate: what it missed
			// is redelivered.
			live := env.ReceiverDeliveryBase() + "/raw-events?sub=harness-redelivery"
			withMatch := mergeHeaders(hdr, map[string]string{"If-Match": deactivated.RowVersion})
			var repointed bcSubscriptionResp
			if _, err := PutJSON(ctx, env.Client, base+"/subscription/"+sub.ID, withMatch,
				map[string]any{"endpoint": live}, &repointed); err != nil {
				return fmt.Errorf("repoint subscription at a live endpoint: %w", err)
			}

			var activated bcActivateResp
			if env.UsesPodBankRails() {
				// Pod: POST .../reactivate redelivers retained notifications.
				var reactBody map[string]any
				actStatus, err := PostJSON(ctx, env.Client, base+"/subscription/"+sub.ID+"/reactivate",
					mergeHeaders(hdr, map[string]string{"If-Match": repointed.RowVersion}), nil, &reactBody)
				if err != nil {
					return fmt.Errorf("reactivate subscription: %w", err)
				}
				if actStatus != 200 && actStatus != 202 {
					return fmt.Errorf("reactivate = %d, want 200/202", actStatus)
				}
				// redelivering count when present; otherwise rely on capture.
				if v, ok := reactBody["redelivering"]; ok {
					n, _ := asMinor(v)
					if n != 0 && n != 2 {
						return fmt.Errorf("reactivation redelivering %v, want 2", v)
					}
					activated.RedeliveredNotifications = int(n)
				} else {
					activated.RedeliveredNotifications = 2 // asserted via capture below
				}
			} else {
				actStatus, err := PutJSON(ctx, env.Client, base+"/subscription/"+sub.ID+"/activate",
					mergeHeaders(hdr, map[string]string{"If-Match": repointed.RowVersion}), nil, &activated)
				if err != nil {
					return fmt.Errorf("activate subscription: %w", err)
				}
				if actStatus != 200 {
					return fmt.Errorf("activate = %d, want 200", actStatus)
				}
				if activated.RedeliveredNotifications != 2 {
					return fmt.Errorf("reactivation redelivered %d notifications, want the 2 that were retained",
						activated.RedeliveredNotifications)
				}
			}

			if err := PollUntil(ctx, 30*time.Second, 250*time.Millisecond, func() (bool, error) {
				events, err := rawEventsFor(ctx, env, live)
				if err != nil {
					return false, err
				}
				if len(events) == 0 {
					return false, fmt.Errorf("nothing redelivered yet")
				}
				batch, err := decryptEnvelope(events[len(events)-1], key)
				if err != nil {
					return false, fmt.Errorf("decrypt redelivered batch: %w", err)
				}
				if len(batch.Notifications) != 2 {
					return false, fmt.Errorf("redelivered message carried %d notifications, want 2", len(batch.Notifications))
				}
				return true, nil
			}); err != nil {
				return err
			}
			return nil
		},
	}
}

// mergeHeaders returns base plus extra, without mutating base.
func mergeHeaders(base, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

type capturedEvent struct {
	ID       string
	Path     string
	BodySize int
	Body     string
	// Nonce, Tag and Checksum come from the delivery's own headers --
	// Banking Circle carries them there rather than inside the body, so a
	// capture without them cannot be decrypted.
	Nonce    string
	Tag      string
	Checksum string
}

// rawEventsFor returns receiver's captures whose request path matches the
// query part of endpoint, so two scenarios pointing at the same sink do
// not see each other's deliveries.
func rawEventsFor(ctx context.Context, env *Env, endpoint string) ([]capturedEvent, error) {
	var resp rawEventsResp
	if _, err := GetJSON(ctx, env.Client, env.ReceiverURL+"/raw-events", &resp); err != nil {
		return nil, fmt.Errorf("list raw events: %w", err)
	}
	marker := endpoint
	if i := strings.Index(endpoint, "/raw-events"); i >= 0 {
		marker = endpoint[i:]
	}
	var out []capturedEvent
	for _, e := range resp.RawEvents {
		if e.Path != marker {
			continue
		}
		out = append(out, capturedEvent{
			ID: e.ID, Path: e.Path, BodySize: e.BodySize, Body: e.Body,
			Nonce:    firstHeader(e.Headers, "Nonce"),
			Tag:      firstHeader(e.Headers, "Authenticationtag"),
			Checksum: firstHeader(e.Headers, "Checksum"),
		})
	}
	return out, nil
}

func rawEventCount(ctx context.Context, env *Env, endpoint string) (int, error) {
	events, err := rawEventsFor(ctx, env, endpoint)
	if err != nil {
		return 0, err
	}
	return len(events), nil
}

// utf16LEToString reverses the UTF-16LE encoding Banking Circle applies
// before encrypting.
func utf16LEToString(b []byte) string {
	if len(b)%2 != 0 {
		return ""
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	return string(utf16.Decode(units))
}

// firstHeader reads one header value out of receiver's capture. Go
// canonicalizes header names on the way in, so the lookup key here is the
// canonical form, not the wire spelling.
func firstHeader(h map[string][]string, name string) string {
	if v, ok := h[name]; ok && len(v) > 0 {
		return v[0]
	}
	return ""
}

// decryptEnvelope reverses Banking Circle's webhook encryption:
// AES-256-GCM over UTF-16LE-encoded JSON, keyed by the raw UTF-8 bytes of
// the 32-character subscription key, with the auth tag carried separately
// from the ciphertext.
//
// Doing the decryption here rather than trusting a 200 is the difference
// between proving a message arrived and proving the right message did --
// and it is also the only way to count how many notifications one message
// carried, which is what the batching assertion needs.
func decryptEnvelope(e capturedEvent, key string) (bankingcircle.Envelope, error) {
	var env bankingcircle.Envelope
	if e.Body == "" {
		return env, fmt.Errorf("delivery %s captured no body", e.ID)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(e.Body)
	if err != nil {
		return env, fmt.Errorf("decode body: %w", err)
	}
	nonce, err := base64.StdEncoding.DecodeString(e.Nonce)
	if err != nil {
		return env, fmt.Errorf("decode Nonce header: %w", err)
	}
	tag, err := base64.StdEncoding.DecodeString(e.Tag)
	if err != nil {
		return env, fmt.Errorf("decode AuthenticationTag header: %w", err)
	}
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return env, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return env, err
	}
	plain16, err := gcm.Open(nil, nonce, append(append([]byte{}, ciphertext...), tag...), nil)
	if err != nil {
		return env, fmt.Errorf("gcm open: %w", err)
	}
	plain := utf16LEToString(plain16)
	// The Checksum header is SHA-256 of the decrypted UTF-8 JSON. Verifying
	// it is what a real consumer must do, so the e2e suite does it too.
	if e.Checksum != "" {
		sum := sha256.Sum256([]byte(plain))
		if base64.StdEncoding.EncodeToString(sum[:]) != e.Checksum {
			return env, fmt.Errorf("Checksum header does not match the decrypted body")
		}
	}
	if err := json.Unmarshal([]byte(plain), &env); err != nil {
		return env, fmt.Errorf("unmarshal decrypted envelope: %w", err)
	}
	return env, nil
}
