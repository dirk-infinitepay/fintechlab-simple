package worldline

// Submerchant enrolment and its receipt.
//
// The acquirer's boarding channel is a pair of files over SFTP, and the
// second one is the point: the platform drops an enrolment batch into
// to_WLNORDIC, and the acquirer answers in from_WLNORDIC with a receipt
// that either approves each submerchant — assigning the MID it will be
// settled under — or rejects it with a reason code.
//
// Without the receipt an upload is a write to a directory, which proves
// nothing. The MID is what the rest of the settle path keys on, so a lab
// that accepted enrolments and never answered would leave every merchant
// permanently unboarded.
//
// Both files are XML under PGP. This package knows only the shapes; the
// crypto and the directories belong to the caller.

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/labclock"
)

// EnrolmentFilename is INFINITEENROLL_<NN>.<YYYYMMDD>.pgp, and the receipt
// is the same name with .ack before the suffix. The platform derives the
// receipt name it expects from the batch it sent, so these must agree
// exactly or the file is invisible to it.
var EnrolmentFilename = regexp.MustCompile(`^INFINITEENROLL_(\d{2})\.(\d{8})\.pgp$`)

// ParseEnrolmentFilename returns the file id and sequence date, and whether
// the name is one the platform would have sent.
func ParseEnrolmentFilename(name string) (fileID int, sequenceDate string, ok bool) {
	m := EnrolmentFilename.FindStringSubmatch(name)
	if m == nil {
		return 0, "", false
	}
	id, err := strconv.Atoi(m[1])
	if err != nil || id < 1 || id > 99 {
		return 0, "", false
	}
	if _, err := time.Parse("20060102", m[2]); err != nil {
		return 0, "", false
	}
	return id, m[2], true
}

// ReceiptFilename is the name the platform will look for, derived from the
// batch rather than invented: it polls for exactly this string.
func ReceiptFilename(fileID int, sequenceDate string) string {
	return fmt.Sprintf("INFINITEENROLL_%02d.%s.ack.pgp", fileID, sequenceDate)
}

// --- the enrolment batch (inbound) --------------------------------------

// Enrolment is the uploaded batch. Only the fields the receipt has to
// answer are modelled; a real batch carries the whole trading identity of
// each outlet and this deliberately ignores it.
type Enrolment struct {
	XMLName      xml.Name      `xml:"enrollment"`
	Submerchants []Submerchant `xml:"submerchant"`
}

// Submerchant is one outlet applying to be boarded. pf_account_id is the
// platform's own reference and the only handle the receipt correlates on.
type Submerchant struct {
	PFAccountID string `xml:"pf_account_id"`
	LegalName   string `xml:"legal_name"`
	DBAName     string `xml:"dba_name"`
	ProxyMID    string `xml:"proxy_mid"`
}

// ParseEnrolment reads an uploaded batch.
//
// The batch declares ISO-8859-1, as the acquirer's schema requires, and
// Go's decoder refuses any declared encoding it was not given a reader
// for — so without the CharsetReader below this would reject every real
// file with an error about the declaration rather than about the content.
func ParseEnrolment(b []byte) (*Enrolment, error) {
	var e Enrolment
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.CharsetReader = charsetReader
	if err := dec.Decode(&e); err != nil {
		return nil, fmt.Errorf("worldline: enrolment xml: %w", err)
	}
	if len(e.Submerchants) == 0 {
		return nil, fmt.Errorf("worldline: enrolment carries no submerchants")
	}
	return &e, nil
}

// charsetReader decodes the encodings the acquirer's files declare.
//
// ISO-8859-1 is a byte-for-byte map onto the first 256 code points, so the
// conversion is a widening rather than a table lookup. UTF-8 and ASCII pass
// through. Anything else is refused by name: silently treating an unknown
// encoding as Latin-1 would corrupt accented trading names in a way that
// only shows up on somebody's statement.
func charsetReader(label string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "iso-8859-1", "latin1", "latin-1", "iso8859-1", "windows-1252":
		raw, err := io.ReadAll(input)
		if err != nil {
			return nil, err
		}
		out := make([]rune, len(raw))
		for i, b := range raw {
			out[i] = rune(b)
		}
		return strings.NewReader(string(out)), nil
	case "utf-8", "utf8", "us-ascii", "ascii", "":
		return input, nil
	}
	return nil, fmt.Errorf("worldline: unsupported encoding %q", label)
}

// --- the receipt (outbound) ---------------------------------------------

// Receipt is the acquirer's answer.
type Receipt struct {
	XMLName    xml.Name    `xml:"enrollment_receipt"`
	Summary    Summary     `xml:"summary"`
	Approvals  []Approval  `xml:"approvals>approval"`
	Rejections []Rejection `xml:"rejections>rejection"`
}

// Summary restates the batch this answers and how it went. The counts are
// redundant with the lists and are sent anyway, because the real file does
// and a client may be checking one against the other.
type Summary struct {
	FileID        string `xml:"fileid"`
	FileDate      string `xml:"filedate"`
	FileTime      string `xml:"filetime"`
	ApprovedCount int    `xml:"approved_count"`
	RejectedCount int    `xml:"rejected_count"`
}

// Approval boards one submerchant. ProxyMID is the identifier every later
// settlement line is keyed by, so this field is the whole reason the
// channel exists. VirtualAccountID is the acquirer's own reference.
type Approval struct {
	PFAccountID      string `xml:"pf_account_id"`
	ProxyMID         string `xml:"proxy_mid"`
	VirtualAccountID string `xml:"virtual_account_id"`
}

// Rejection refuses one submerchant. ReasonCode is a two-digit code from
// the acquirer's published list; the platform maps it to a description.
type Rejection struct {
	PFAccountID string `xml:"pf_account_id"`
	ReasonCode  string `xml:"reason_code"`
}

// RejectionReasons are the codes the platform knows how to describe. A code
// outside this set would leave it reporting an error it cannot explain, so
// a simulation should only ever send one of these.
var RejectionReasons = map[string]string{
	"01": "Invalid registration number",
	"02": "Invalid proxy MID",
	"03": "Outlet data incomplete",
	"04": "Invalid outlet count",
	"05": "Invalid registered address country",
	"06": "Invalid trading name",
	"07": "Invalid trading address line 1",
	"08": "Invalid trading address town/city",
	"09": "Invalid or not permitted MCC",
	"10": "Invalid country of origin",
	"11": "Invalid outlet phone number",
	"12": "Invalid website",
	"13": "Invalid SIRET",
}

// ReceiptOptions decide what the acquirer says about a batch.
type ReceiptOptions struct {
	FileID       int
	SequenceDate string
	At           time.Time
	// RejectCodes maps a pf_account_id to the reason it was refused.
	// Anything not named here is approved, so the happy path is the
	// default and a refusal has to be asked for.
	RejectCodes map[string]string
	// MIDPrefix begins every assigned proxy MID. Obviously fake, per
	// docs/principles.md.
	MIDPrefix string
}

// BuildReceipt answers an enrolment batch.
//
// MIDs are derived from the submerchant's own pf_account_id rather than a
// counter or a random source, so the same batch always boards to the same
// MID: a rerun, a screenshot and a test fixture agree, and an operator who
// re-uploads after a mistake does not get a second identity for the same
// outlet.
func BuildReceipt(e *Enrolment, opt ReceiptOptions) *Receipt {
	if opt.At.IsZero() {
		opt.At = labclock.Now()
	}
	if opt.MIDPrefix == "" {
		opt.MIDPrefix = "6001"
	}
	r := &Receipt{
		Summary: Summary{
			FileID:   fmt.Sprintf("%02d", opt.FileID),
			FileDate: opt.SequenceDate,
			FileTime: opt.At.UTC().Format("150405"),
		},
	}
	for _, s := range e.Submerchants {
		id := strings.TrimSpace(s.PFAccountID)
		if id == "" {
			// No handle to answer on. Saying nothing is better than
			// inventing a reference the platform cannot match.
			continue
		}
		if code, refused := opt.RejectCodes[id]; refused {
			if _, known := RejectionReasons[code]; !known {
				code = "03"
			}
			r.Rejections = append(r.Rejections, Rejection{PFAccountID: id, ReasonCode: code})
			continue
		}
		r.Approvals = append(r.Approvals, Approval{
			PFAccountID:      id,
			ProxyMID:         ProxyMIDFor(opt.MIDPrefix, id),
			VirtualAccountID: virtualAccountFor(id),
		})
	}
	r.Summary.ApprovedCount = len(r.Approvals)
	r.Summary.RejectedCount = len(r.Rejections)
	return r
}

// ProxyMIDFor derives a stable eight-digit MID from a platform reference.
func ProxyMIDFor(prefix, pfAccountID string) string {
	return prefix + fmt.Sprintf("%04d", checksum(pfAccountID)%10000)
}

func virtualAccountFor(pfAccountID string) string {
	return fmt.Sprintf("%08d", checksum("va:"+pfAccountID)%100000000)
}

// checksum is a small deterministic hash. Not cryptographic and not meant
// to be: it exists so the same reference yields the same MID every time.
func checksum(s string) int {
	h := 17
	for _, c := range s {
		h = h*31 + int(c)
	}
	if h < 0 {
		h = -h
	}
	return h
}

// ParseReceipt reads a receipt back.
//
// Exported because anything reading one — a test, an e2e assertion, an
// operator tool — needs the same charset handling the declaration demands,
// and plain xml.Unmarshal refuses the file this package just wrote.
func ParseReceipt(b []byte) (*Receipt, error) {
	var r Receipt
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.CharsetReader = charsetReader
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("worldline: receipt xml: %w", err)
	}
	return &r, nil
}

// MarshalReceipt renders the receipt as the acquirer sends it.
//
// The declaration says ISO-8859-1 because the real file does, and a client
// that honours the declaration would mis-decode anything outside ASCII if
// this claimed one encoding and wrote another. Everything this lab puts in
// a receipt is ASCII, so the two agree.
func MarshalReceipt(r *Receipt) ([]byte, error) {
	body, err := xml.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("worldline: receipt xml: %w", err)
	}
	return append([]byte(`<?xml version="1.0" encoding="ISO-8859-1"?>`+"\n"), append(body, '\n')...), nil
}
