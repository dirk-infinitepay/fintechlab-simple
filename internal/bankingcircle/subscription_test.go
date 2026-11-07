package bankingcircle

import (
	"errors"
	"testing"
)

func newStore(t *testing.T) *SubscriptionStore {
	t.Helper()
	return NewSubscriptionStore(nil)
}

const testKey = "0123456789abcdef0123456789abcdef"

func TestCreateValidatesTheDocumentedFieldRules(t *testing.T) {
	s := newStore(t)
	cases := map[string]CreateParams{
		"no endpoint":     {EncryptionKey: testKey, Status: StatusActive},
		"short key":       {Endpoint: "https://a.test/h", EncryptionKey: "short", Status: StatusActive},
		"long key":        {Endpoint: "https://a.test/h", EncryptionKey: testKey + "x", Status: StatusActive},
		"status retired":  {Endpoint: "https://a.test/h", EncryptionKey: testKey, Status: StatusRetired},
		"batch too small": {Endpoint: "https://a.test/h", EncryptionKey: testKey, Status: StatusActive, MaxNotificationsPerMessage: 4},
		"batch too large": {Endpoint: "https://a.test/h", EncryptionKey: testKey, Status: StatusActive, MaxNotificationsPerMessage: 1001},
	}
	for name, p := range cases {
		if _, err := s.Create(p); err == nil {
			t.Errorf("Create(%s) succeeded, want an error", name)
		}
	}
}

func TestCreateHidesTheEncryptionKeyAndRejectsDuplicateEndpoints(t *testing.T) {
	s := newStore(t)
	sub, err := s.Create(CreateParams{Endpoint: "https://a.test/h", EncryptionKey: testKey, Status: StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	if sub.MaxNotificationsPerMessage != DefaultNotificationsPerMessage {
		t.Fatalf("default batch size = %d, want %d", sub.MaxNotificationsPerMessage, DefaultNotificationsPerMessage)
	}
	// The key is kept for encryption but never rendered.
	pub := sub.Public()
	if pub.EncryptionKey != hiddenKey {
		t.Fatalf("Public() rendered the key as %q, want %q", pub.EncryptionKey, hiddenKey)
	}
	if sub.EncryptionKey != testKey {
		t.Fatal("the stored key was mangled; notifications would be encrypted with the wrong key")
	}

	// Subscriptions cannot share an endpoint URL.
	if _, err := s.Create(CreateParams{Endpoint: "https://a.test/h", EncryptionKey: testKey, Status: StatusActive}); !errors.Is(err, ErrDuplicateEndpoint) {
		t.Fatalf("duplicate endpoint: err = %v, want ErrDuplicateEndpoint", err)
	}
}

func TestMutationsRequireAMatchingRowVersion(t *testing.T) {
	s := newStore(t)
	sub, err := s.Create(CreateParams{Endpoint: "https://a.test/h", EncryptionKey: testKey, Status: StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	original := sub.RowVersion

	// No token at all is a rejection, not a bypass: the point of the check
	// is that a caller who has not read the state cannot overwrite it.
	if _, err := s.Update(sub.ID, "", UpdateParams{}); !errors.Is(err, ErrRowVersionMismatch) {
		t.Fatalf("update with no If-Match: err = %v, want ErrRowVersionMismatch", err)
	}
	if _, err := s.Update(sub.ID, "nonsense", UpdateParams{}); !errors.Is(err, ErrRowVersionMismatch) {
		t.Fatalf("update with a wrong If-Match: err = %v, want ErrRowVersionMismatch", err)
	}

	email := "new@example.com"
	updated, err := s.Update(sub.ID, original, UpdateParams{Email: &email})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Email != email {
		t.Fatalf("email = %q, want %q", updated.Email, email)
	}
	if updated.RowVersion == original {
		t.Fatal("rowVersion did not advance; a concurrent writer could clobber this update")
	}
	// The consumed token is now stale everywhere, not just on Update.
	if _, err := s.SetActive(sub.ID, original, false); !errors.Is(err, ErrRowVersionMismatch) {
		t.Fatalf("SetActive with a consumed token: err = %v, want ErrRowVersionMismatch", err)
	}
	if err := s.Delete(sub.ID, original); !errors.Is(err, ErrRowVersionMismatch) {
		t.Fatalf("Delete with a consumed token: err = %v, want ErrRowVersionMismatch", err)
	}
}

// TestOverrideActiveSkipsIfMatchButStillMovesTheRowVersion: the lab's
// switch needs no token, and a client holding the old one is still told
// its view is stale.
func TestOverrideActiveSkipsIfMatchButStillMovesTheRowVersion(t *testing.T) {
	s := newStore(t)
	sub, err := s.Create(CreateParams{Endpoint: "https://a.test/h", EncryptionKey: testKey, Status: StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddEvent(sub.ID, "PaymentStatus", TargetCompany, nil); err != nil {
		t.Fatal(err)
	}
	held, eventHeld := sub.RowVersion, sub.Events[0].RowVersion

	off, err := s.OverrideActive(sub.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if off.IsActive || off.Status != StatusInactive || off.StatusMessage != "Subscription is inactive" || off.Events[0].IsActive {
		t.Fatalf("after OverrideActive(false): %+v", off.Public())
	}
	if off.RowVersion == held || off.Events[0].RowVersion == eventHeld {
		t.Fatal("rowVersions did not move")
	}
	if _, err := s.SetActive(sub.ID, held, true); !errors.Is(err, ErrRowVersionMismatch) {
		t.Fatalf("SetActive with the token from before the override: err = %v, want ErrRowVersionMismatch", err)
	}
	on, err := s.OverrideActive(sub.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !on.IsActive || on.Status != StatusActive || !on.Events[0].IsActive {
		t.Fatalf("after OverrideActive(true): %+v", on.Public())
	}
	if _, err := s.OverrideActive("sub_nope", true); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("OverrideActive of an unknown id: err = %v, want ErrSubscriptionNotFound", err)
	}
}

func TestEventsRouteByTypeAndTarget(t *testing.T) {
	s := newStore(t)
	// Two subscribers wanting different things.
	payments, err := s.Create(CreateParams{Endpoint: "https://payments.test/h", EncryptionKey: testKey, Status: StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	cases, err := s.Create(CreateParams{Endpoint: "https://cases.test/h", EncryptionKey: testKey, Status: StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddEvent(payments.ID, "OutgoingPaymentProcessed", TargetAccount, []string{"acc_1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddEvent(cases.ID, "CaseEvents", TargetCompany, nil); err != nil {
		t.Fatal(err)
	}

	// The right event on the right account reaches exactly one subscriber.
	got := s.Recipients("OutgoingPaymentProcessed", "acc_1")
	if len(got) != 1 || got[0].Subscription.ID != payments.ID {
		t.Fatalf("Recipients for acc_1 = %+v, want just the payments subscriber", got)
	}
	if got[0].Event.EventType != "OutgoingPaymentProcessed" {
		t.Fatalf("matched event %q", got[0].Event.EventType)
	}

	// A different account is not this subscriber's business. Fanning out
	// to every active subscription regardless -- which is what the old
	// model did -- made the target configuration decorative.
	if got := s.Recipients("OutgoingPaymentProcessed", "acc_999"); len(got) != 0 {
		t.Fatalf("an event on acc_999 reached %d subscriber(s) that never asked for it", len(got))
	}
	// An event type nobody subscribed to goes nowhere.
	if got := s.Recipients("MissingFunding", "acc_1"); len(got) != 0 {
		t.Fatalf("MissingFunding reached %d subscriber(s) without a matching event", len(got))
	}
	// A subscription-wide event (no targets) matches any target.
	if got := s.Recipients("CaseEvents", "anything"); len(got) != 1 || got[0].Subscription.ID != cases.ID {
		t.Fatalf("Recipients for CaseEvents = %+v, want the cases subscriber", got)
	}
}

func TestDeactivatedSubscriptionsReceiveNothing(t *testing.T) {
	s := newStore(t)
	sub, err := s.Create(CreateParams{Endpoint: "https://a.test/h", EncryptionKey: testKey, Status: StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddEvent(sub.ID, "OutgoingPaymentProcessed", TargetCompany, nil); err != nil {
		t.Fatal(err)
	}
	if len(s.Recipients("OutgoingPaymentProcessed", "acc_1")) != 1 {
		t.Fatal("an active subscription did not match its own event")
	}

	fresh, _ := s.Get(sub.ID)
	if _, err := s.SetActive(sub.ID, fresh.RowVersion, false); err != nil {
		t.Fatal(err)
	}
	if got := s.Recipients("OutgoingPaymentProcessed", "acc_1"); len(got) != 0 {
		t.Fatalf("a deactivated subscription still matched %d event(s)", len(got))
	}
}

func TestCreateInactiveSubscriptionIsNotDeliveredTo(t *testing.T) {
	s := newStore(t)
	sub, err := s.Create(CreateParams{Endpoint: "https://a.test/h", EncryptionKey: testKey, Status: StatusInactive})
	if err != nil {
		t.Fatal(err)
	}
	if sub.IsActive {
		t.Fatal("a subscription created with status 1 (inactive) came back active")
	}
	if _, err := s.AddEvent(sub.ID, "OutgoingPaymentProcessed", TargetCompany, nil); err != nil {
		t.Fatal(err)
	}
	if got := s.Recipients("OutgoingPaymentProcessed", "acc_1"); len(got) != 0 {
		t.Fatalf("an inactive subscription matched %d event(s)", len(got))
	}
}

func TestDuplicateEventTypeOnOneSubscriptionIsRejected(t *testing.T) {
	s := newStore(t)
	sub, err := s.Create(CreateParams{Endpoint: "https://a.test/h", EncryptionKey: testKey, Status: StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddEvent(sub.ID, "OutgoingPaymentProcessed", TargetCompany, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddEvent(sub.ID, "OutgoingPaymentProcessed", TargetCompany, nil); !errors.Is(err, ErrDuplicateEvent) {
		t.Fatalf("duplicate eventType: err = %v, want ErrDuplicateEvent", err)
	}
}

func TestReplaceTargetsSwapsTheWholeList(t *testing.T) {
	s := newStore(t)
	sub, err := s.Create(CreateParams{Endpoint: "https://a.test/h", EncryptionKey: testKey, Status: StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := s.AddEvent(sub.ID, "OutgoingPaymentProcessed", TargetAccount, []string{"acc_1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceTargets(ev.ID, "wrong", TargetAccount, []string{"acc_2"}); !errors.Is(err, ErrRowVersionMismatch) {
		t.Fatalf("ReplaceTargets with a bad If-Match: err = %v, want ErrRowVersionMismatch", err)
	}
	if _, err := s.ReplaceTargets(ev.ID, ev.RowVersion, TargetAccount, nil); err == nil {
		t.Fatal("ReplaceTargets with an empty list succeeded; targetIds requires at least one id")
	}
	replaced, err := s.ReplaceTargets(ev.ID, ev.RowVersion, TargetAccount, []string{"acc_2", "acc_3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(replaced.Targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(replaced.Targets))
	}
	// Replaced, not appended: acc_1 must be gone.
	if len(s.Recipients("OutgoingPaymentProcessed", "acc_1")) != 0 {
		t.Fatal("the old target is still receiving events after being replaced")
	}
	if len(s.Recipients("OutgoingPaymentProcessed", "acc_3")) != 1 {
		t.Fatal("a new target is not receiving events")
	}
}

func TestDeleteEventStopsDelivery(t *testing.T) {
	s := newStore(t)
	sub, err := s.Create(CreateParams{Endpoint: "https://a.test/h", EncryptionKey: testKey, Status: StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := s.AddEvent(sub.ID, "OutgoingPaymentProcessed", TargetCompany, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteEvent(ev.ID, ev.RowVersion); err != nil {
		t.Fatal(err)
	}
	if got := s.Recipients("OutgoingPaymentProcessed", "acc_1"); len(got) != 0 {
		t.Fatalf("a deleted event still matched %d time(s)", len(got))
	}
	if err := s.DeleteEvent(ev.ID, ev.RowVersion); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Fatalf("double delete: err = %v, want ErrSubscriptionNotFound", err)
	}
}

func TestListPages(t *testing.T) {
	s := newStore(t)
	for i := range 7 {
		if _, err := s.Create(CreateParams{
			Endpoint:      "https://a.test/h" + string(rune('a'+i)),
			EncryptionKey: testKey,
			Status:        StatusActive,
		}); err != nil {
			t.Fatal(err)
		}
	}
	page1, total := s.List(1, 3)
	if total != 7 || len(page1) != 3 {
		t.Fatalf("page 1 = %d items of %d total, want 3 of 7", len(page1), total)
	}
	page3, _ := s.List(3, 3)
	if len(page3) != 1 {
		t.Fatalf("page 3 = %d items, want the 1 left over", len(page3))
	}
	if got, _ := s.List(4, 3); len(got) != 0 {
		t.Fatalf("page 4 = %d items, want none", len(got))
	}
	// Pages must not overlap.
	if page1[0].ID == page3[0].ID {
		t.Fatal("paging returned the same subscription on two pages")
	}
}

func TestUsesPayloadProperty(t *testing.T) {
	// PaymentStatus and AgencyBankingWhitelistResult nest their detail
	// under "payload"; everything else uses "payment". A parser written
	// against only one of them silently drops the other.
	if !UsesPayloadProperty(string(NotificationPaymentStatus)) {
		t.Error("PaymentStatus should use the payload property")
	}
	if !UsesPayloadProperty(string(NotificationAgencyBankingWhitelistResult)) {
		t.Error("AgencyBankingWhitelistResult should use the payload property")
	}
	if UsesPayloadProperty(string(NotificationOutgoingPaymentProcessed)) {
		t.Error("OutgoingPaymentProcessed should use the payment property")
	}
}
