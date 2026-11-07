package main

// The boarding channel: answer an enrolment batch with a receipt.
//
// The platform drops INFINITEENROLL_<NN>.<YYYYMMDD>.pgp into to_WLNORDIC
// and then polls from_WLNORDIC for the matching .ack.pgp. Until that
// receipt exists a submerchant has no MID, and without a MID nothing later
// in the settle path can key a payout to it — so the upload alone boards
// nobody.
//
// The answer is not immediate, on purpose. A real acquirer takes minutes to
// hours, and "we uploaded and nothing has come back yet" is a state the
// platform has to hold correctly. A lab that answered on the same tick
// would let that code path go untested. The delay is configurable and
// POST /sim/enrolment/process answers everything waiting, so a scenario
// never has to sit through it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/wlsftp"
	"github.com/webduvet/fintechlab-simple/internal/worldline"
)

type enrolmentDesk struct {
	root string
	keys *wlsftp.PGPKeys
	// delay is how long the acquirer appears to take. Zero answers on the
	// next sweep, which is what a test wants and not what a client should
	// be built against.
	delay time.Duration
	// sweep is how often the inbox is checked.
	sweep time.Duration

	mu sync.Mutex
	// seen records the batches already answered, so a receipt is written
	// once. Re-answering would hand the platform a second identity for a
	// submerchant it has already boarded.
	seen map[string]time.Time
	// rejects maps a pf_account_id to a reason code, armed by an operator
	// before uploading. Empty means everything is approved.
	rejects map[string]string

	// guard drops an answer a reset lands in the middle of (reset.go).
	guard *runGuard
}

func newEnrolmentDesk(root string, keys *wlsftp.PGPKeys, delay, sweep time.Duration) *enrolmentDesk {
	if sweep <= 0 {
		sweep = 5 * time.Second
	}
	return &enrolmentDesk{
		root: root, keys: keys, delay: delay, sweep: sweep,
		seen: map[string]time.Time{}, rejects: map[string]string{},
	}
}

func (d *enrolmentDesk) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /sim/enrolment", d.list)
	mux.HandleFunc("POST /sim/enrolment/process", d.processNow)
	mux.HandleFunc("POST /sim/enrolment/reject", d.armRejection)
}

// watch sweeps the inbox until the process ends.
func (d *enrolmentDesk) watch() {
	for {
		if n, err := d.process(false); err != nil {
			log.Printf("worldline: enrolment sweep: %v", err)
		} else if n > 0 {
			log.Printf("worldline: answered %d enrolment batch(es)", n)
		}
		time.Sleep(d.sweep)
	}
}

// pending lists batches that have arrived and not yet been answered.
func (d *enrolmentDesk) pending() ([]string, error) {
	dir := filepath.Join(d.root, wlsftp.DirToWLNordic)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var out []string
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, _, ok := worldline.ParseEnrolmentFilename(e.Name()); !ok {
			continue
		}
		if _, done := d.seen[e.Name()]; done {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// process answers every batch that is due. When now is true the configured
// delay is ignored, which is the operator seam. A reset that lands while it
// runs drops the rest: no receipt is written after it.
func (d *enrolmentDesk) process(now bool) (int, error) {
	started := d.guard.start()
	names, err := d.pending()
	if err != nil {
		return 0, err
	}
	answered := 0
	for _, name := range names {
		path := filepath.Join(d.root, wlsftp.DirToWLNordic, name)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if !now && time.Since(info.ModTime()) < d.delay {
			continue
		}
		if err := d.answer(name, path, started); err != nil {
			if errors.Is(err, errReset) {
				log.Printf("worldline: enrolment %s not answered: %v", name, err)
				break
			}
			// Log and carry on: one malformed batch must not stop the
			// others from being answered.
			log.Printf("worldline: enrolment %s: %v", name, err)
			d.guard.apply(started, func() {
				d.mu.Lock()
				d.seen[name] = time.Now().UTC()
				d.mu.Unlock()
			})
			continue
		}
		answered++
	}
	return answered, nil
}

// errReset is work a reset dropped.
var errReset = errors.New("worldline was reset while it was in flight; dropped")

// answer decrypts one batch and writes its receipt, unless a reset happened
// since started (errReset).
func (d *enrolmentDesk) answer(name, path string, started uint64) error {
	if d.guard.moved(started) {
		return errReset
	}
	fileID, sequenceDate, ok := worldline.ParseEnrolmentFilename(name)
	if !ok {
		return fmt.Errorf("not an enrolment filename")
	}
	ciphertext, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	plaintext, err := d.decrypt(ciphertext)
	if err != nil {
		return fmt.Errorf("decrypt: %w", err)
	}
	batch, err := worldline.ParseEnrolment(plaintext)
	if err != nil {
		return err
	}

	d.mu.Lock()
	rejects := make(map[string]string, len(d.rejects))
	for k, v := range d.rejects {
		rejects[k] = v
	}
	d.mu.Unlock()

	receipt := worldline.BuildReceipt(batch, worldline.ReceiptOptions{
		FileID: fileID, SequenceDate: sequenceDate, RejectCodes: rejects,
	})
	body, err := worldline.MarshalReceipt(receipt)
	if err != nil {
		return err
	}

	// The receipt goes back encrypted to the platform, in the directory it
	// polls, under the name it derived from its own upload.
	outName := worldline.ReceiptFilename(fileID, sequenceDate)
	// Always encrypted, exactly as the settlement file is. The platform
	// decrypts a receipt with its own private key, so writing plaintext
	// would produce a file its client cannot open — and a lab that only
	// encrypted when a recipient happened to be configured would pass
	// locally and fail the moment one was.
	//
	// With no recipient configured this self-encrypts to the acquirer's
	// own key, which is the same fallback the settlement file uses: still
	// a real OpenPGP message, still signed, and openable by anyone holding
	// the key this lab generated.
	if d.keys == nil || d.keys.Own == nil {
		return fmt.Errorf("no key to encrypt the receipt with")
	}
	sealed, err := d.keys.EncryptAndSign(body)
	if err != nil {
		return fmt.Errorf("encrypt receipt: %w", err)
	}
	// The receipt and the record of having answered land together, before a
	// reset or not at all.
	var werr error
	if !d.guard.apply(started, func() { werr = d.deliver(name, outName, sealed) }) {
		return errReset
	}
	if werr != nil {
		return werr
	}
	log.Printf("worldline: %s -> %s (%d approved, %d rejected)",
		name, outName, receipt.Summary.ApprovedCount, receipt.Summary.RejectedCount)
	return nil
}

// deliver writes a receipt into from_WLNORDIC/ and records the batch as
// answered.
func (d *enrolmentDesk) deliver(batch, outName string, sealed []byte) error {
	outDir := filepath.Join(d.root, wlsftp.DirFromWLNordic)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	// Written to a temporary name and renamed, so a poller never collects
	// a half-written receipt.
	tmp := filepath.Join(outDir, outName+".partial")
	if err := os.WriteFile(tmp, sealed, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(outDir, outName)); err != nil {
		return err
	}
	d.mu.Lock()
	d.seen[batch] = time.Now().UTC()
	d.mu.Unlock()
	return nil
}

// clearRejections disarms every rejection, for a reset, and returns how
// many were armed.
func (d *enrolmentDesk) clearRejections() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := len(d.rejects)
	d.rejects = map[string]string{}
	return n
}

// forgetAnswered forgets which batches were answered, for a reset of
// everything, and returns how many it had answered. A batch still in
// to_WLNORDIC/ is answered again on the next sweep.
func (d *enrolmentDesk) forgetAnswered() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := len(d.seen)
	d.seen = map[string]time.Time{}
	return n
}

// decrypt opens a batch. The platform encrypts to the acquirer's public key
// and signs with its own; this lab holds the acquirer's private half.
//
// An unencrypted batch is accepted when it parses as XML, because a
// scenario driving this by hand should not have to hold a keyring to test
// the boarding logic. That leniency is the lab's, not the vendor's.
func (d *enrolmentDesk) decrypt(ciphertext []byte) ([]byte, error) {
	if looksLikeXML(ciphertext) {
		return ciphertext, nil
	}
	if d.keys == nil || d.keys.Own == nil {
		return nil, fmt.Errorf("no key to decrypt with")
	}
	return wlsftp.Decrypt(ciphertext, d.keys.Keyring())
}

func looksLikeXML(b []byte) bool {
	return strings.HasPrefix(strings.TrimSpace(string(b)), "<")
}

// --- the operator seam --------------------------------------------------

func (d *enrolmentDesk) list(w http.ResponseWriter, r *http.Request) {
	names, err := d.pending()
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	d.mu.Lock()
	answered := make([]string, 0, len(d.seen))
	for k := range d.seen {
		answered = append(answered, k)
	}
	rejects := make(map[string]string, len(d.rejects))
	for k, v := range d.rejects {
		rejects[k] = v
	}
	d.mu.Unlock()
	sort.Strings(answered)
	httputilx.WriteJSON(w, 200, map[string]any{
		"waiting":  names,
		"answered": answered,
		"rejects":  rejects,
		"delay":    d.delay.String(),
	})
}

func (d *enrolmentDesk) processNow(w http.ResponseWriter, r *http.Request) {
	n, err := d.process(true)
	if err != nil {
		httputilx.Error(w, 500, err.Error())
		return
	}
	httputilx.WriteJSON(w, 200, map[string]any{"answered": n})
}

type rejectReq struct {
	PFAccountID string `json:"pf_account_id"`
	ReasonCode  string `json:"reason_code"`
}

// armRejection tells the desk to refuse one submerchant on the next batch.
//
// Armed beforehand rather than applied afterwards, because that is the only
// order in which it is true: the acquirer decides when it reads the file,
// and a receipt already written cannot be changed.
func (d *enrolmentDesk) armRejection(w http.ResponseWriter, r *http.Request) {
	var req rejectReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	if req.PFAccountID == "" {
		httputilx.Error(w, 400, "pf_account_id required")
		return
	}
	if _, known := worldline.RejectionReasons[req.ReasonCode]; !known {
		httputilx.Error(w, 400, fmt.Sprintf(
			"reason_code %q is not one the platform can describe", req.ReasonCode))
		return
	}
	d.mu.Lock()
	d.rejects[req.PFAccountID] = req.ReasonCode
	d.mu.Unlock()
	httputilx.WriteJSON(w, 200, map[string]any{
		"pf_account_id": req.PFAccountID,
		"reason_code":   req.ReasonCode,
		"reason":        worldline.RejectionReasons[req.ReasonCode],
	})
}
