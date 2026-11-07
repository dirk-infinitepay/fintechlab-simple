package bankingcircle

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Notification delivery: batching, the documented retry schedule, and
// auto-deactivation.
//
// Three things here that the previous implementation did not have, each of
// which changes what a client has to be written to handle:
//
//  1. Notifications are batched. maxNotificationsPerMessage is not a hint
//     -- it changes the wire shape, and a client that assumes one
//     notification per POST breaks the first time two events land close
//     together.
//  2. Retries follow Banking Circle's own eleven-step schedule, ending in
//     the subscription being deactivated. A client has to survive that,
//     and cannot discover it if the simulator gives up after four tries
//     and stays subscribed.
//  3. Undelivered notifications are retained across deactivation and
//     redelivered on reactivation, while events that occur while
//     deactivated are not backfilled. That distinction is the difference
//     between a correct recovery procedure and a lost day of payments.

// RetryStep is one row of the retry schedule.
type RetryStep struct {
	// After is the delay before this attempt, measured from the previous
	// one. It reads from JSON as a duration string ("15s", "2h") rather
	// than a nanosecond count, because this is a file meant to be edited
	// by hand.
	After Duration `json:"after"`
	// Email is the notification sent alongside this attempt: "", "warning"
	// or "deactivation".
	Email string `json:"email,omitempty"`
}

// DeliveryConfig is the tunable behaviour of the notification pipe.
//
// It is a file rather than a pile of environment variables because the
// interesting part is a table. The real schedule ends 48 hours after the
// first failure; nobody is going to sit through that to see what a
// deactivation looks like, so TimeScale divides every delay by a constant
// and the whole two-day story plays out in seconds -- with the shape and
// the step count intact, which is what is actually being tested.
type DeliveryConfig struct {
	// Schedule is the retry table. Attempt 1 is the initial delivery;
	// Schedule[i] is the wait before retry i+1.
	Schedule []RetryStep `json:"retry_schedule"`

	// TimeScale divides every delay in Schedule. 1 means real time; 3600
	// turns an hour into a second. Values <= 0 are treated as 1.
	TimeScale float64 `json:"time_scale"`

	// DeactivateAfterRetries is how many failed retries deactivate the
	// subscription. The retry guide documents 11 while the OpenAPI text
	// says 10; 11 is the schedule of record here, and the discrepancy is
	// noted rather than silently resolved.
	DeactivateAfterRetries int `json:"deactivate_after_retries"`

	// BatchFlushInterval is how long a partly-filled batch waits for more
	// notifications before going out anyway. Without it a subscription
	// with maxNotificationsPerMessage=1000 would never deliver anything
	// until a thousand payments happened.
	BatchFlushInterval Duration `json:"batch_flush_interval"`

	// DefaultMaxNotificationsPerMessage applies when a subscription does
	// not set its own.
	DefaultMaxNotificationsPerMessage int `json:"default_max_notifications_per_message"`

	// RequestTimeout bounds one delivery attempt.
	RequestTimeout Duration `json:"request_timeout"`
}

// Duration is a time.Duration that reads from JSON as a string ("1s"),
// because a config file full of nanosecond integers is a config file
// nobody edits correctly.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("bankingcircle: %q is not a duration: %w", s, err)
		}
		*d = Duration(parsed)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("bankingcircle: duration must be a string like \"1s\"")
	}
	*d = Duration(time.Duration(n) * time.Second)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// DefaultDeliveryConfig is Banking Circle's documented retry schedule:
// eleven retries, ending 48 hours after the tenth, with a warning email at
// the seventh and tenth and a deactivation email at the eleventh.
//
// TimeScale defaults to 1 -- real time. Set it in the config file; the
// shipped file uses a compressed scale so the lab is usable.
func DefaultDeliveryConfig() DeliveryConfig {
	return DeliveryConfig{
		Schedule: []RetryStep{
			{After: Duration(15 * time.Second)},
			{After: Duration(30 * time.Second)},
			{After: Duration(1 * time.Minute)},
			{After: Duration(10 * time.Minute)},
			{After: Duration(30 * time.Minute)},
			{After: Duration(1 * time.Hour)},
			{After: Duration(2 * time.Hour), Email: "warning"},
			{After: Duration(6 * time.Hour)},
			{After: Duration(12 * time.Hour)},
			{After: Duration(24 * time.Hour), Email: "warning"},
			{After: Duration(48 * time.Hour), Email: "deactivation"},
		},
		TimeScale:                         1,
		DeactivateAfterRetries:            11,
		BatchFlushInterval:                Duration(2 * time.Second),
		DefaultMaxNotificationsPerMessage: DefaultNotificationsPerMessage,
		RequestTimeout:                    Duration(10 * time.Second),
	}
}

// LoadDeliveryConfig reads a config file, falling back to the documented
// defaults for anything it does not set. A missing file is not an error --
// the defaults are the real schedule, so running without a file gives you
// real Banking Circle behaviour, just slowly.
func LoadDeliveryConfig(path string) (DeliveryConfig, error) {
	cfg := DefaultDeliveryConfig()
	if path == "" {
		return cfg, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("bankingcircle: read %s: %w", path, err)
	}
	// Decode over the defaults so a file setting only time_scale keeps the
	// real schedule rather than silently ending up with an empty one.
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return DefaultDeliveryConfig(), fmt.Errorf("bankingcircle: parse %s: %w", path, err)
	}
	if cfg.TimeScale <= 0 {
		cfg.TimeScale = 1
	}
	if cfg.DeactivateAfterRetries <= 0 || cfg.DeactivateAfterRetries > len(cfg.Schedule) {
		cfg.DeactivateAfterRetries = len(cfg.Schedule)
	}
	if cfg.BatchFlushInterval <= 0 {
		cfg.BatchFlushInterval = Duration(2 * time.Second)
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = Duration(10 * time.Second)
	}
	return cfg, nil
}

// Wait returns the scaled delay before retry number n (1-based), and
// whether such a retry exists at all.
func (c DeliveryConfig) Wait(n int) (time.Duration, bool) {
	if n < 1 || n > len(c.Schedule) || n > c.DeactivateAfterRetries {
		return 0, false
	}
	scale := c.TimeScale
	if scale <= 0 {
		scale = 1
	}
	return time.Duration(float64(c.Schedule[n-1].After) / scale), true
}

// EmailFor returns the email kind to send alongside retry n, or "".
func (c DeliveryConfig) EmailFor(n int) string {
	if n < 1 || n > len(c.Schedule) {
		return ""
	}
	return c.Schedule[n-1].Email
}

// Envelope is a decrypted notification message body.
type Envelope struct {
	Notifications []Notification `json:"notifications"`
}

// Notification is one event inside an envelope.
//
// The payment-versus-payload split is real and load-bearing: most payment
// events nest their detail under "payment", but PaymentStatus and
// AgencyBankingWhitelistResult use "payload" instead. A parser written
// against only one of them silently drops the other.
type Notification struct {
	EventID             string `json:"eventId"`
	SubscriptionID      string `json:"subscriptionId"`
	SubscriptionEventID string `json:"subscriptionEventId"`
	NotificationType    string `json:"notificationType"`
	Timestamp           string `json:"timestamp"`
	Payment             any    `json:"payment,omitempty"`
	Payload             any    `json:"payload,omitempty"`
	// Trap is the lab's trap whose step made the state this notifies
	// (Payment.Trap), for the notifications activity log. It is never
	// marshalled, so it never reaches a subscriber.
	Trap string `json:"-"`
}

// UsesPayloadProperty reports whether an event type nests its detail under
// "payload" rather than "payment".
func UsesPayloadProperty(eventType string) bool {
	switch NotificationType(eventType) {
	case NotificationPaymentStatus, NotificationAgencyBankingWhitelistResult:
		return true
	}
	return false
}

// Email is one message the delivery pipe would have sent to a
// subscription's configured address. Kept in memory and exposed, rather
// than actually mailed: a warning that nobody can see is not a simulation
// of a warning.
type Email struct {
	To             string    `json:"to"`
	Kind           string    `json:"kind"`
	SubscriptionID string    `json:"subscription_id"`
	Endpoint       string    `json:"endpoint"`
	Subject        string    `json:"subject"`
	Attempt        int       `json:"attempt"`
	At             time.Time `json:"at"`
}

// MailBox collects the emails the pipe emits.
type MailBox struct {
	mu   sync.Mutex
	sent []Email
}

// Send records one email.
func (m *MailBox) Send(e Email) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.At = time.Now().UTC()
	m.sent = append(m.sent, e)
	if len(m.sent) > 500 {
		m.sent = m.sent[len(m.sent)-500:]
	}
}

// clearLocked forgets every email and says how many there were. Caller
// holds m.mu.
func (m *MailBox) clearLocked() int {
	n := len(m.sent)
	m.sent = nil
	return n
}

// All returns every email sent so far.
func (m *MailBox) All() []Email {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Email, len(m.sent))
	copy(out, m.sent)
	return out
}
