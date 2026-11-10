package verify

import (
	"testing"
	"time"
)

func TestTriggerDefaultPositiveDecision(t *testing.T) {
	engine := NewEngine(10*time.Millisecond, nil)

	got := engine.Trigger(42)

	if got.MerchantApplicationID != 42 {
		t.Fatalf("MerchantApplicationID = %d, want 42", got.MerchantApplicationID)
	}
	if got.OverallDecision != "APPROVED" {
		t.Fatalf("OverallDecision = %q, want APPROVED", got.OverallDecision)
	}
	if got.AmlDecision != "APPROVED" {
		t.Fatalf("AmlDecision = %q, want APPROVED", got.AmlDecision)
	}
	if got.AmlCompanyDecision != "APPROVED" {
		t.Fatalf("AmlCompanyDecision = %q, want APPROVED", got.AmlCompanyDecision)
	}
	if got.RiskDecision != "LOW" {
		t.Fatalf("RiskDecision = %q, want LOW", got.RiskDecision)
	}
	if got.RiskCompanyDecision != "LOW" {
		t.Fatalf("RiskCompanyDecision = %q, want LOW", got.RiskCompanyDecision)
	}
	if got.RiskScore != 5 {
		t.Fatalf("RiskScore = %d, want 5", got.RiskScore)
	}
	if got.RiskClassification != "LOW" {
		t.Fatalf("RiskClassification = %q, want LOW", got.RiskClassification)
	}
	if got.ManualOverrideDecision != "" {
		t.Fatalf("ManualOverrideDecision = %q, want empty", got.ManualOverrideDecision)
	}
}

func TestTriggerForceDeclineDecision(t *testing.T) {
	engine := NewEngine(10*time.Millisecond, map[int]bool{99: true})

	got := engine.Trigger(99)

	if got.OverallDecision != "DECLINE" {
		t.Fatalf("OverallDecision = %q, want DECLINE", got.OverallDecision)
	}
	if got.AmlDecision != "DECLINED" {
		t.Fatalf("AmlDecision = %q, want DECLINED", got.AmlDecision)
	}
	if got.AmlCompanyDecision != "DECLINED" {
		t.Fatalf("AmlCompanyDecision = %q, want DECLINED", got.AmlCompanyDecision)
	}
	if got.RiskDecision != "HIGH" {
		t.Fatalf("RiskDecision = %q, want HIGH", got.RiskDecision)
	}
	if got.RiskCompanyDecision != "HIGH" {
		t.Fatalf("RiskCompanyDecision = %q, want HIGH", got.RiskCompanyDecision)
	}
	if got.RiskScore != 95 {
		t.Fatalf("RiskScore = %d, want 95", got.RiskScore)
	}
	if got.RiskClassification != "HIGH" {
		t.Fatalf("RiskClassification = %q, want HIGH", got.RiskClassification)
	}
}

func TestGetBeforeDelayElapsedReturnsFalse(t *testing.T) {
	engine := NewEngine(50*time.Millisecond, nil)
	engine.Trigger(7)

	if _, ok := engine.Get(7); ok {
		t.Fatal("Get before delay elapsed = true, want false")
	}
}

func TestGetAfterDelayElapsedReturnsDecision(t *testing.T) {
	engine := NewEngine(10*time.Millisecond, nil)
	engine.Trigger(7)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if d, ok := engine.Get(7); ok {
			if d.MerchantApplicationID != 7 {
				t.Fatalf("MerchantApplicationID = %d, want 7", d.MerchantApplicationID)
			}
			if d.OverallDecision != "APPROVED" {
				t.Fatalf("OverallDecision = %q, want APPROVED", d.OverallDecision)
			}
			if d.CompletedAt == "" {
				t.Fatal("CompletedAt is empty, want an RFC3339 timestamp")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("decision was never recorded within the deadline")
}

func TestGetUnknownMerchantReturnsFalse(t *testing.T) {
	engine := NewEngine(10*time.Millisecond, nil)

	if _, ok := engine.Get(12345); ok {
		t.Fatal("Get for unknown id = true, want false")
	}
}

// TestResetForgetsRecordedDecisions: after a reset a decision read is a
// 404 again, as on first start.
func TestResetForgetsRecordedDecisions(t *testing.T) {
	engine := NewEngine(0, nil)
	engine.Trigger(7)
	engine.Trigger(8)
	engine.running.Wait()
	if _, ok := engine.Get(7); !ok {
		t.Fatal("decision 7 was never recorded")
	}

	decisions, pending := engine.Reset()
	if decisions != 2 || pending != 0 {
		t.Errorf("Reset = %d decisions, %d pending; want 2, 0", decisions, pending)
	}
	if _, ok := engine.Get(7); ok {
		t.Error("decision 7 survived the reset")
	}
	if decisions, pending := engine.Reset(); decisions != 0 || pending != 0 {
		t.Errorf("a second Reset = %d, %d; want nothing left to clear", decisions, pending)
	}
}

// TestResetDropsATriggerStillInItsDelay: a trigger sleeping out the
// processing delay when the reset lands is dropped, not recorded after it —
// and the reset wakes it, so it does not linger for the rest of the delay.
func TestResetDropsATriggerStillInItsDelay(t *testing.T) {
	engine := NewEngine(time.Hour, nil)
	engine.Trigger(7)

	decisions, pending := engine.Reset()
	if decisions != 0 || pending != 1 {
		t.Errorf("Reset = %d decisions, %d pending; want 0, 1", decisions, pending)
	}
	engine.running.Wait() // returns at once: the reset woke it
	if _, ok := engine.Get(7); ok {
		t.Fatal("a trigger from before the reset was recorded after it")
	}

	// Work started after the reset is the new run's, and counted as such.
	engine.Trigger(8)
	if _, pending := engine.Reset(); pending != 1 {
		t.Errorf("pending after a new trigger = %d, want 1", pending)
	}
	engine.running.Wait()
}

// TestResetKeepsTheForceDeclineSet: VERIFY_FORCE_DECLINE_IDS is
// configuration, not something a run made.
func TestResetKeepsTheForceDeclineSet(t *testing.T) {
	engine := NewEngine(0, map[int]bool{99: true})
	engine.Reset()
	if got := engine.Trigger(99); got.OverallDecision != "DECLINE" {
		t.Errorf("after a reset, 99 decides %q, want DECLINE", got.OverallDecision)
	}
	engine.running.Wait()
}

// TestEachEnvironmentHasItsOwnDecisions: the same merchantApplicationId in
// two environments is two decisions, and a reset of one drops only its own
// -- its triggers still in their delay included.
func TestEachEnvironmentHasItsOwnDecisions(t *testing.T) {
	engine := NewEngine(0, map[int]bool{7: true})
	engine.TriggerIn("sbx", 7)
	engine.Trigger(7)
	engine.running.Wait()

	if d, ok := engine.GetIn("sbx", 7); !ok || d.OverallDecision != "APPROVED" {
		t.Errorf("sbx's 7 = %+v, %v; want APPROVED: the force-decline set is the default's", d, ok)
	}
	if d, ok := engine.Get(7); !ok || d.OverallDecision != "DECLINE" {
		t.Errorf("the default's 7 = %+v, %v; want DECLINE", d, ok)
	}
	if _, ok := engine.GetIn("other", 7); ok {
		t.Error("an environment that triggered nothing reads a decision")
	}

	if decisions, pending := engine.ResetIn("sbx"); decisions != 1 || pending != 0 {
		t.Errorf("ResetIn(sbx) = %d, %d; want 1, 0", decisions, pending)
	}
	if _, ok := engine.GetIn("sbx", 7); ok {
		t.Error("sbx's decision survived its reset")
	}
	if _, ok := engine.Get(7); !ok {
		t.Error("the default's decision went with sbx's reset")
	}
}

// TestAResetOfOneEnvironmentLeavesTheOthersTriggersInFlight: only the reset
// environment's sleepers are woken and dropped.
func TestAResetOfOneEnvironmentLeavesTheOthersTriggersInFlight(t *testing.T) {
	engine := NewEngine(30*time.Millisecond, nil)
	engine.TriggerIn("sbx", 1)
	engine.Trigger(1)
	if _, pending := engine.ResetIn("sbx"); pending != 1 {
		t.Fatalf("ResetIn(sbx) pending = %d, want 1", pending)
	}
	engine.running.Wait()
	if _, ok := engine.GetIn("sbx", 1); ok {
		t.Error("sbx's trigger from before its reset was recorded after it")
	}
	if _, ok := engine.Get(1); !ok {
		t.Error("the default's trigger was dropped by sbx's reset")
	}
	if decisions, _ := engine.Forget("default"); decisions != 1 {
		t.Errorf("Forget(default) = %d decisions, want 1", decisions)
	}
}
