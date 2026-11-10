package worldline

import (
	"strings"
	"testing"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/labclock"
)

const sampleBatch = `<?xml version="1.0" encoding="ISO-8859-1"?>
<enrollment>
  <submerchant>
    <pf_account_id>PF-0001</pf_account_id>
    <legal_name>Southwind Coffee Ltd</legal_name>
    <dba_name>Southwind Coffee</dba_name>
  </submerchant>
  <submerchant>
    <pf_account_id>PF-0002</pf_account_id>
    <legal_name>Northgate Books Ltd</legal_name>
    <dba_name>Northgate Books</dba_name>
  </submerchant>
</enrollment>`

// TestFilenamesMatchWhatThePlatformExpects: the platform derives the
// receipt name from the batch it sent and polls for exactly that string.
// A name that differs by one character is a receipt nobody collects.
func TestFilenamesMatchWhatThePlatformExpects(t *testing.T) {
	id, date, ok := ParseEnrolmentFilename("INFINITEENROLL_07.20260918.pgp")
	if !ok || id != 7 || date != "20260918" {
		t.Fatalf("parse = %d %q %v", id, date, ok)
	}
	if got := ReceiptFilename(id, date); got != "INFINITEENROLL_07.20260918.ack.pgp" {
		t.Errorf("receipt name = %q", got)
	}

	for _, bad := range []string{
		"INFINITEENROLL_7.20260918.pgp",  // file id must be two digits
		"INFINITEENROLL_00.20260918.pgp", // and at least 01
		"INFINITEENROLL_07.20261318.pgp", // month 13 is not a date
		"INFINITEENROLL_07.20260918.xml", // must be .pgp
		"settlement_20260918.pgp",        // not an enrolment at all
	} {
		if _, _, ok := ParseEnrolmentFilename(bad); ok {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// TestEveryApplicationIsAnsweredExactlyOnce is the property the receipt
// exists for. A submerchant in neither list is one the platform will wait
// on forever; in both, one it cannot resolve.
func TestEveryApplicationIsAnsweredExactlyOnce(t *testing.T) {
	e, err := ParseEnrolment([]byte(sampleBatch))
	if err != nil {
		t.Fatal(err)
	}
	r := BuildReceipt(e, ReceiptOptions{FileID: 7, SequenceDate: "20260918",
		RejectCodes: map[string]string{"PF-0002": "09"}})

	seen := map[string]int{}
	for _, a := range r.Approvals {
		seen[a.PFAccountID]++
	}
	for _, x := range r.Rejections {
		seen[x.PFAccountID]++
	}
	for _, s := range e.Submerchants {
		if seen[s.PFAccountID] != 1 {
			t.Errorf("%s answered %d times, want exactly 1", s.PFAccountID, seen[s.PFAccountID])
		}
	}
	if r.Summary.ApprovedCount != 1 || r.Summary.RejectedCount != 1 {
		t.Errorf("summary = %d approved / %d rejected", r.Summary.ApprovedCount, r.Summary.RejectedCount)
	}
}

// TestApprovalCarriesAMID: the MID is what every later settlement line is
// keyed by, so an approval without one boards nobody.
func TestApprovalCarriesAMID(t *testing.T) {
	e, _ := ParseEnrolment([]byte(sampleBatch))
	r := BuildReceipt(e, ReceiptOptions{FileID: 1, SequenceDate: "20260918"})
	if len(r.Approvals) != 2 {
		t.Fatalf("approvals = %d, want 2", len(r.Approvals))
	}
	for _, a := range r.Approvals {
		if a.ProxyMID == "" || a.VirtualAccountID == "" {
			t.Errorf("approval %+v is missing an identifier", a)
		}
		if len(a.ProxyMID) != 8 {
			t.Errorf("proxy MID %q is not eight digits", a.ProxyMID)
		}
	}
	if r.Approvals[0].ProxyMID == r.Approvals[1].ProxyMID {
		t.Error("two submerchants were boarded to the same MID")
	}
}

// TestMIDsAreStable: a rerun, a screenshot and a test fixture should agree,
// and an operator who re-uploads after a mistake must not get a second
// identity for the same outlet.
func TestMIDsAreStable(t *testing.T) {
	e, _ := ParseEnrolment([]byte(sampleBatch))
	first := BuildReceipt(e, ReceiptOptions{FileID: 1, SequenceDate: "20260918"})
	second := BuildReceipt(e, ReceiptOptions{FileID: 2, SequenceDate: "20260919"})
	if first.Approvals[0].ProxyMID != second.Approvals[0].ProxyMID {
		t.Errorf("the same submerchant boarded to %q then %q",
			first.Approvals[0].ProxyMID, second.Approvals[0].ProxyMID)
	}
}

// TestUnknownReasonCodeIsNotSent: the platform maps a code to a
// description, so a code it has never heard of leaves it reporting an error
// it cannot explain.
func TestUnknownReasonCodeIsNotSent(t *testing.T) {
	e, _ := ParseEnrolment([]byte(sampleBatch))
	r := BuildReceipt(e, ReceiptOptions{FileID: 1, SequenceDate: "20260918",
		RejectCodes: map[string]string{"PF-0001": "99"}})
	if len(r.Rejections) != 1 {
		t.Fatalf("rejections = %d", len(r.Rejections))
	}
	if _, known := RejectionReasons[r.Rejections[0].ReasonCode]; !known {
		t.Errorf("sent reason code %q, which the platform cannot describe", r.Rejections[0].ReasonCode)
	}
}

// TestReceiptRoundTrips: what this writes must parse back to what it meant,
// with the element names the platform's parser reads.
func TestReceiptRoundTrips(t *testing.T) {
	e, _ := ParseEnrolment([]byte(sampleBatch))
	r := BuildReceipt(e, ReceiptOptions{FileID: 7, SequenceDate: "20260918",
		At: time.Date(2026, 9, 18, 16, 47, 35, 0, time.UTC)})
	raw, err := MarshalReceipt(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), `<?xml version="1.0" encoding="ISO-8859-1"?>`) {
		t.Error("missing the declaration the acquirer sends")
	}
	for _, want := range []string{
		"<enrollment_receipt>", "<summary>", "<fileid>07</fileid>",
		"<filedate>20260918</filedate>", "<filetime>164735</filetime>",
		"<approved_count>2</approved_count>", "<approvals>", "<approval>",
		"<pf_account_id>PF-0001</pf_account_id>", "<proxy_mid>", "<virtual_account_id>",
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("receipt is missing %s", want)
		}
	}

	back, err := ParseReceipt(raw)
	if err != nil {
		t.Fatalf("does not parse back: %v", err)
	}
	if back.Summary.ApprovedCount != 2 || len(back.Approvals) != 2 {
		t.Errorf("round trip lost content: %+v", back.Summary)
	}
}

// TestBatchWithNoSubmerchantsIsRefused: an empty batch is a mistake worth
// reporting rather than an empty receipt that looks like success.
func TestBatchWithNoSubmerchantsIsRefused(t *testing.T) {
	if _, err := ParseEnrolment([]byte(`<enrollment></enrollment>`)); err == nil {
		t.Error("an empty batch was accepted")
	}
	if _, err := ParseEnrolment([]byte(`not xml`)); err == nil {
		t.Error("a non-XML batch was accepted")
	}
}

// TestReceiptIsTimedOnTheRunnersClock: a receipt built without an explicit
// time is timed on the platform's clock.
func TestReceiptIsTimedOnTheRunnersClock(t *testing.T) {
	labclock.Set(90 * time.Minute)
	defer labclock.Set(0)
	e, err := ParseEnrolment([]byte(sampleBatch))
	if err != nil {
		t.Fatal(err)
	}
	before := labclock.Now().Format("150405")
	r := BuildReceipt(e, ReceiptOptions{FileID: 1, SequenceDate: "20260918"})
	after := labclock.Now().Format("150405")
	if r.Summary.FileTime < before || r.Summary.FileTime > after {
		t.Errorf("filetime %s, want the runner's %s–%s", r.Summary.FileTime, before, after)
	}
}

// Two platforms boarding the same pf_account_id get different MIDs: the
// default's are what they always were, every other environment's are
// eight digits from a range the default's never reach, apart per
// environment and the same every time.
func TestEachEnvironmentBoardsWithMIDsOfItsOwn(t *testing.T) {
	e, err := ParseEnrolment([]byte(sampleBatch))
	if err != nil {
		t.Fatal(err)
	}
	board := func(env string) []Approval {
		t.Helper()
		return BuildReceipt(e, ReceiptOptions{FileID: 1, SequenceDate: "20260918", Env: env}).Approvals
	}
	def := board("")
	if again := board("default"); again[0] != def[0] || def[0].ProxyMID != ProxyMIDFor("6001", "PF-0001") {
		t.Fatalf("the default's boarding changed: %+v, %+v", def, again)
	}
	seen := map[string]string{}
	for _, a := range def {
		seen[a.ProxyMID] = "default"
	}
	for _, env := range []string{"sbx", "dev", "e2e-run-1"} {
		got := board(env)
		for i, a := range got {
			if len(a.ProxyMID) != 8 || !strings.HasPrefix(a.ProxyMID, "6") || strings.HasPrefix(a.ProxyMID, "60") {
				t.Errorf("%s boarded %s as %q: want eight digits from 61 to 69", env, a.PFAccountID, a.ProxyMID)
			}
			if other, dup := seen[a.ProxyMID]; dup {
				t.Errorf("%s and %s share MID %s", env, other, a.ProxyMID)
			}
			seen[a.ProxyMID] = env
			if a.VirtualAccountID == def[i].VirtualAccountID {
				t.Errorf("%s shares the default's virtual account for %s", env, a.PFAccountID)
			}
			if a.ProxyMID != EnvProxyMID(env, a.PFAccountID) || board(env)[i] != a {
				t.Errorf("%s's MID for %s is not stable", env, a.PFAccountID)
			}
		}
	}
}
