package b4b

import (
	"strings"
	"testing"
	"time"
)

func TestEngineFullProgression(t *testing.T) {
	events := make(chan Payment, 8)
	engine := NewEngine(10*time.Millisecond, func(p *Payment, _ uint64) { events <- *p })

	p := &Payment{
		ID:                 "b4bp_test1",
		BeneficiaryID:      "ben_merchant1",
		CallbackURL:        "http://settlement:8083/internal/b4b-webhook",
		Amount:             Amount{Amount: "100.00", Currency: "EUR"},
		CurrencyOfTransfer: "EUR",
	}
	engine.Submit(p)

	want := []PaymentState{
		StateAccepted,
		StateSanctionsPending,
		StateSanctionsApproved,
		StateTMPending,
		StateTMApproved,
	}
	for _, w := range want {
		select {
		case ev := <-events:
			if ev.State != w {
				t.Fatalf("state = %s, want %s", ev.State, w)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s", w)
		}
	}

	got, err := engine.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateTMApproved {
		t.Fatalf("final state = %s, want %s", got.State, StateTMApproved)
	}
}

func TestEngineForceFailureDivertsToFailed(t *testing.T) {
	events := make(chan Payment, 8)
	engine := NewEngine(10*time.Millisecond, func(p *Payment, _ uint64) { events <- *p })

	p := &Payment{
		ID:            "b4bp_test2",
		BeneficiaryID: "ben_sanctioned",
		CallbackURL:   "http://settlement:8083/internal/b4b-webhook",
		Amount:        Amount{Amount: "50.00", Currency: "EUR"},
		ForceFail:     true,
	}
	engine.Submit(p)

	want := []PaymentState{
		StateAccepted,
		StateSanctionsPending,
		StateSanctionsApproved,
		StateTMPending,
		StateFailed,
	}
	for _, w := range want {
		select {
		case ev := <-events:
			if ev.State != w {
				t.Fatalf("state = %s, want %s", ev.State, w)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s", w)
		}
	}

	got, err := engine.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateFailed {
		t.Fatalf("final state = %s, want %s", got.State, StateFailed)
	}

	// Never a TMApproved event for a force-failed payment.
	select {
	case ev := <-events:
		t.Fatalf("unexpected extra transition: %s", ev.State)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestEngineGetUnknownPayment(t *testing.T) {
	engine := NewEngine(time.Millisecond, nil)
	if _, err := engine.Get("does-not-exist"); err != ErrPaymentNotFound {
		t.Fatalf("err = %v, want ErrPaymentNotFound", err)
	}
}

func TestEngineList(t *testing.T) {
	engine := NewEngine(time.Millisecond, nil)
	engine.Submit(&Payment{ID: "b4bp_a"})
	engine.Submit(&Payment{ID: "b4bp_b"})
	// Submit fires synchronously before returning, so both are already
	// present as B4BAccepted regardless of the background goroutines.
	list := engine.List()
	if len(list) != 2 {
		t.Fatalf("List() len = %d, want 2", len(list))
	}
	var ids []string
	for _, p := range list {
		ids = append(ids, p.ID)
	}
	joined := strings.Join(ids, ",")
	if !strings.Contains(joined, "b4bp_a") || !strings.Contains(joined, "b4bp_b") {
		t.Fatalf("List() ids = %v, missing an entry", ids)
	}
}
