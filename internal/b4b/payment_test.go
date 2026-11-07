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

// A payment a trap caught is accepted and then moved only by Hop: one
// state at a time, each a transition like the default walk's, a failure
// keeping the reason it was given.
func TestEngineHopTakesAPaymentOneStateAtATime(t *testing.T) {
	events := make(chan Payment, 8)
	engine := NewEngine(time.Hour, func(p *Payment, _ uint64) { events <- *p })
	started := engine.Accept(&Payment{ID: "b4bp_trapped", BeneficiaryID: "MID001", Trap: "trp_0001"})
	if ev := <-events; ev.State != StateAccepted {
		t.Fatalf("first transition = %s", ev.State)
	}
	if engine.Walking() != 0 {
		t.Fatal("Accept started the default walk")
	}

	if err := engine.Hop("b4bp_trapped", started, StateTMPending, "trp_0001", ""); err == nil {
		t.Fatal("a hop to a state that does not directly follow was taken")
	}
	for _, st := range []PaymentState{StateSanctionsPending, StateFailed} {
		if err := engine.Hop("b4bp_trapped", started, st, "trp_0001", "sanctions hit"); err != nil {
			t.Fatalf("hop to %s: %v", st, err)
		}
		if ev := <-events; ev.State != st || ev.Trap != "trp_0001" {
			t.Fatalf("transition = %s by %q, want %s by trp_0001", ev.State, ev.Trap, st)
		}
	}
	got, _ := engine.Get("b4bp_trapped")
	if got.State != StateFailed || got.FailureReason != "sanctions hit" {
		t.Fatalf("after the hops: %s, reason %q", got.State, got.FailureReason)
	}
	if err := engine.Hop("b4bp_trapped", started, StateTMApproved, "trp_0001", ""); err == nil {
		t.Fatal("a failed payment moved on")
	}
	if err := engine.Hop("nope", started, StateSanctionsPending, "trp_0001", ""); err != ErrPaymentNotFound {
		t.Fatalf("hop on an unknown payment: %v", err)
	}
}

func TestHopsWalksTheStatesInBetween(t *testing.T) {
	for _, c := range []struct {
		from, to PaymentState
		want     []PaymentState
	}{
		{StateAccepted, StateTMApproved, []PaymentState{StateSanctionsPending, StateSanctionsApproved, StateTMPending, StateTMApproved}},
		{StateAccepted, StateTMPending, []PaymentState{StateSanctionsPending, StateSanctionsApproved, StateTMPending}},
		{StateAccepted, StateFailed, []PaymentState{StateSanctionsPending, StateFailed}},
		{StateSanctionsApproved, StateFailed, []PaymentState{StateFailed}},
		{StateTMPending, StateTMPending, []PaymentState{}},
		{StateTMPending, StateSanctionsPending, nil},
		{StateFailed, StateTMApproved, nil},
		{PaymentState("nonsense"), StateTMApproved, nil},
	} {
		got := Hops(c.from, c.to)
		if (got == nil) != (c.want == nil) || strings.Join(states(got), ",") != strings.Join(states(c.want), ",") {
			t.Errorf("Hops(%s, %s) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
	for _, s := range []PaymentState{StateTMApproved, StateFailed, PaymentState("nonsense")} {
		if !finished(s) {
			t.Errorf("%s is not finished", s)
		}
	}
	if finished(StateTMPending) {
		t.Error("TMPending is finished")
	}
}

func states(s []PaymentState) []string {
	out := make([]string, len(s))
	for i, st := range s {
		out[i] = string(st)
	}
	return out
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
