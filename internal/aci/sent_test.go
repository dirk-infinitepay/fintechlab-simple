package aci

import (
	"testing"
	"time"
)

func TestResetEmptiesTheLogAndRefusesDeliveriesPutBeforeIt(t *testing.T) {
	s := NewSentLog()
	before := s.Put(&Record{ID: "aci_evt_1", DeliveryStatus: "pending"})
	s.Put(&Record{ID: "aci_evt_2", DeliveryStatus: "held"})

	sent, deliveries := s.Reset()
	if sent != 2 || deliveries != 0 {
		t.Fatalf("Reset() = %d sent, %d deliveries; want 2, 0", sent, deliveries)
	}
	if len(s.List()) != 0 {
		t.Errorf("%d record(s) after a reset", len(s.List()))
	}
	if _, ok := s.Get("aci_evt_1"); ok {
		t.Error("a record is still readable after a reset")
	}
	if s.Do(before, func() { t.Error("an attempt ran for a record the reset removed") }) {
		t.Error("Do ran a delivery put before the reset")
	}
	// Updating a record the reset removed does not bring it back.
	s.Update("aci_evt_1", func(r *Record) { r.DeliveryStatus = "delivered" })
	if len(s.List()) != 0 {
		t.Error("an update brought back a record the reset removed")
	}

	after := s.Put(&Record{ID: "aci_evt_3"})
	if !s.Do(after, func() {}) {
		t.Error("Do refused a delivery put after the reset")
	}
}

func TestResetCountsTheDeliveriesInFlight(t *testing.T) {
	s := NewSentLog()
	release := make(chan struct{})
	s.Go(s.Put(&Record{ID: "aci_evt_1"}), func(uint64) { <-release })

	if _, deliveries := s.Reset(); deliveries != 1 {
		t.Fatalf("deliveries in flight = %d, want 1", deliveries)
	}
	// A second reset does not count the delivery the first one dropped.
	if _, deliveries := s.Reset(); deliveries != 0 {
		t.Fatalf("a second reset counted %d delivery(ies), want 0", deliveries)
	}
	if s.InFlight() != 1 {
		t.Fatalf("InFlight() = %d, want the dropped delivery until it returns", s.InFlight())
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for s.InFlight() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the delivery never finished")
		}
		time.Sleep(time.Millisecond)
	}
}

// A reset waits for an attempt already on the wire, so when it answers no
// webhook it dropped is still being sent.
func TestResetWaitsForAnAttemptOnTheWire(t *testing.T) {
	s := NewSentLog()
	started := s.Put(&Record{ID: "aci_evt_1"})
	inAttempt, release := make(chan struct{}), make(chan struct{})
	go s.Do(started, func() {
		close(inAttempt)
		<-release
	})
	<-inAttempt

	done := make(chan struct{})
	go func() {
		s.Reset()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("the reset answered while an attempt was still on the wire")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-done
	if s.Do(started, func() {}) {
		t.Error("a delivery put before the reset made another attempt after it")
	}
}

func TestADeliveryWhoseRecordAResetRemovedIsNotStarted(t *testing.T) {
	s := NewSentLog()
	before := s.Put(&Record{ID: "aci_evt_1"})
	s.Reset()
	s.Go(before, func(uint64) { t.Error("a delivery for a removed record was started") })
	if s.InFlight() != 0 {
		t.Fatalf("InFlight() = %d, want 0", s.InFlight())
	}
}
