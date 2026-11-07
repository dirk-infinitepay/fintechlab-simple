package main

import (
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

	"github.com/ProtonMail/go-crypto/openpgp"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/settlement"
	"github.com/webduvet/fintechlab-simple/internal/waitfor"
	"github.com/webduvet/fintechlab-simple/internal/wlsftp"
	"github.com/webduvet/fintechlab-simple/internal/worldline"
)

// The platform's side of the Worldline settlement channel: connect to the
// acquirer's SFTP server, download whatever is new, decrypt it, and act on
// it.
//
// This is the code path a real-host swap has to work through, so it is the
// one the lab exercises. Nothing here reads a shared directory or a
// simulator-only endpoint: point WORLDLINE_SFTP_HOST at real Worldline,
// supply the real credentials and PGP key, and the same code runs.

// pulledFile records one settlement file this service has taken delivery
// of, for the /worldline/files view.
type pulledFile struct {
	Name       string    `json:"name"`
	Slot       string    `json:"slot"`
	PulledAt   time.Time `json:"pulled_at"`
	Bytes      int       `json:"bytes"`
	ArchivedAt string    `json:"archived_at"`
	// Processed distinguishes the morning file, which drives payouts,
	// from the afternoon confirmation, which is retained and nothing
	// more. Paying out twice because a confirmation arrived is the
	// mistake this field exists to make visible.
	Processed    bool     `json:"processed"`
	Settlements  []string `json:"settlement_ids,omitempty"`
	PayoutsByMID int      `json:"payouts_by_mid,omitempty"`
	Error        string   `json:"error,omitempty"`
}

type worldlinePuller struct {
	cfg        wlsftp.ClientConfig
	keyring    openpgp.EntityList
	archiveDir string
	interval   time.Duration
	app        *app

	// pullMu serializes whole pull passes. Without it two overlapping
	// pulls -- the background loop and a forced one, or two forced ones --
	// each connect, each list the same file, and each process it. That
	// pays every merchant in the file twice.
	pullMu sync.Mutex

	mu     sync.Mutex
	seen   map[string]bool
	pulled []pulledFile
	// notBefore is when the stand-in was last reset: a file on the server
	// older than that is not taken (predatesReset). Zero until a reset.
	notBefore time.Time
}

// newWorldlinePuller builds the puller from the environment, or returns
// nil when no Worldline host is configured -- running the platform side
// without an acquirer attached is a legitimate configuration (unit tests,
// a partial compose) and must not be fatal.
func newWorldlinePuller(a *app) *worldlinePuller {
	host := env("WORLDLINE_SFTP_HOST", "")
	if host == "" {
		log.Printf("settlement: WORLDLINE_SFTP_HOST unset -- no settlement files will be pulled")
		return nil
	}
	archiveDir := env("WORLDLINE_ARCHIVE_DIR", "/data/worldline")
	if err := os.MkdirAll(archiveDir, 0o777); err != nil {
		log.Printf("settlement: create worldline archive dir %s: %v", archiveDir, err)
	}

	p := &worldlinePuller{
		cfg: wlsftp.ClientConfig{
			Host:           host,
			Port:           env("WORLDLINE_SFTP_PORT", "2222"),
			User:           env("WORLDLINE_SFTP_USER", "infinitepay"),
			Password:       env("WORLDLINE_SFTP_PASSWORD", ""),
			PrivateKeyPath: env("WORLDLINE_SFTP_PRIVATE_KEY_PATH", ""),
			HostKeyPath:    env("WORLDLINE_SFTP_KNOWN_HOST_PATH", ""),
			Dir:            env("WORLDLINE_SFTP_DIR", wlsftp.DirDownload),
		},
		archiveDir: archiveDir,
		interval:   envDuration("WORLDLINE_PULL_INTERVAL", 10*time.Second),
		app:        a,
		seen:       map[string]bool{},
	}

	// The PGP key that decrypts what the acquirer sends. In this lab both
	// halves come from the same generated keypair over a shared volume; a
	// real deployment sets this to the platform's own private key and
	// hands Worldline the matching public one.
	privPath := env("WORLDLINE_PGP_PRIVATE_KEY_PATH", "/keys/wlsftp-keys/worldline_private.asc")
	pubPath := env("WORLDLINE_PGP_PUBLIC_KEY_PATH", "/keys/wlsftp-keys/worldline_public.asc")
	// worldline writes this pair on its first start, and on a fresh volume
	// that start is concurrent with this one. Without the wait, the first
	// boot of a pod (or of compose with keys/ deleted) finds nothing, tries
	// to generate a pair into a read-only mount, and runs with no key to
	// decrypt anything until it is restarted.
	if err := waitfor.Files(30*time.Second, pubPath, privPath); err != nil {
		log.Printf("settlement: %v; generating a keypair of its own instead", err)
	}
	entity, err := wlsftp.LoadOrGenerateKeypair(pubPath, privPath, "infinitepay", "fintechlab-simple", "platform@fintechlab-simple.local")
	if err != nil {
		log.Printf("settlement: load PGP keypair for settlement-file decryption: %v", err)
	} else {
		p.keyring = openpgp.EntityList{entity}
	}

	// Anything already archived was taken delivery of on an earlier run;
	// re-processing it would pay a merchant twice.
	if entries, err := os.ReadDir(archiveDir); err == nil {
		for _, e := range entries {
			p.seen[e.Name()] = true
		}
	}
	return p
}

// run polls the acquirer forever. Every error is logged and retried on the
// next tick rather than killing the loop: an acquirer's SFTP host being
// briefly unreachable is normal operations, not a reason for the platform
// to stop.
func (p *worldlinePuller) run() {
	for {
		// Disconnected, the timer stops pulling; a forced pull still runs.
		if !p.app.wiring.Connected() {
			time.Sleep(p.interval)
			continue
		}
		if n, err := p.pullOnce(); err != nil {
			log.Printf("settlement: worldline pull: %v", err)
		} else if n > 0 {
			log.Printf("settlement: worldline pull took delivery of %d file(s)", n)
		}
		time.Sleep(p.interval)
	}
}

// pullOnce connects, downloads everything not yet seen, and returns how
// many files it took delivery of.
//
// Only one pull runs at a time, and a file is claimed before it is
// downloaded rather than after it is processed. Both matter: taking
// delivery of a settlement file means paying merchants, and a file
// processed twice pays them twice. Checking "have I seen this?" and
// recording "yes" on either side of a multi-second download and payout
// run is a window wide enough to drive a second pull through, which is
// exactly what a caller polling for the file does.
func (p *worldlinePuller) pullOnce() (int, error) {
	p.pullMu.Lock()
	defer p.pullMu.Unlock()
	// A reset that lands while this pull runs drops the rest of it: no file
	// archived, no record made and no payout sent after it (reset.go).
	started := p.app.guard.start()

	c, err := wlsftp.Dial(p.cfg)
	if err != nil {
		return 0, err
	}
	defer c.Close()

	files, err := c.List()
	if err != nil {
		return 0, err
	}
	var n int
	for _, f := range files {
		if p.app.guard.moved(started) {
			log.Printf("settlement: worldline pull dropped by a reset after taking %d file(s)", n)
			break
		}
		if p.predatesReset(f) || !p.claim(f.Name) {
			continue
		}
		rec, ok := p.take(c, f.Name, started)
		if ok {
			ok = p.app.guard.apply(started, func() {
				p.mu.Lock()
				p.pulled = append(p.pulled, rec)
				p.mu.Unlock()
			})
		}
		if !ok {
			log.Printf("settlement: %s: %v", f.Name, errReset)
			break
		}
		n++
	}
	return n, nil
}

// predatesReset reports whether f was on the server before the last reset,
// and marks it seen so it is never taken. The reset emptied the archive
// that would otherwise say it had been taken already, and taking it again
// would pay its merchants a second time. A file Worldline publishes after
// the reset — even under the same name — is newer, and taken.
func (p *worldlinePuller) predatesReset(f wlsftp.RemoteFile) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.notBefore.IsZero() || p.seen[f.Name] || !f.ModTime.Before(p.notBefore) {
		return false
	}
	p.seen[f.Name] = true
	log.Printf("settlement: %s was on Worldline's server before the reset; not taken", f.Name)
	return true
}

// claim marks a filename as taken and reports whether this caller is the
// one that took it. The check and the mark happen under one lock, so two
// callers cannot both believe they claimed the same file.
func (p *worldlinePuller) claim(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.seen[name] {
		return false
	}
	p.seen[name] = true
	return true
}

// take downloads, decrypts, archives, and (for a morning file) processes
// one settlement file. It reports false when a reset since started dropped
// the rest of it.
func (p *worldlinePuller) take(c *wlsftp.Client, name string, started uint64) (pulledFile, bool) {
	rec := pulledFile{Name: name, Slot: worldline.FilenameSlot(name), PulledAt: labclock.Now()}

	plaintext, err := c.Download(name, p.keyring)
	if err != nil {
		rec.Error = err.Error()
		log.Printf("settlement: download %s: %v", name, err)
		return rec, true
	}
	rec.Bytes = len(plaintext)

	// Archive first, process second. The file is the audit record; if
	// processing panics or the process dies mid-payout, the bytes that
	// caused it are already on disk.
	archived := filepath.Join(p.archiveDir, name)
	var werr error
	if !p.app.guard.apply(started, func() { werr = os.WriteFile(archived, plaintext, 0o644) }) {
		return rec, false
	}
	if werr != nil {
		log.Printf("settlement: archive %s: %v", name, werr)
	} else {
		rec.ArchivedAt = archived
	}

	switch rec.Slot {
	case worldline.SlotAfternoon:
		// The confirmation file is retained for audit and deliberately
		// not reprocessed -- it confirms the settlement the morning file
		// already described.
		log.Printf("settlement: retained afternoon confirmation file %s (%d bytes), not reprocessed", name, len(plaintext))
	case worldline.SlotMorning:
		ids, payouts, err := p.process(name, plaintext, started)
		if errors.Is(err, errReset) {
			return rec, false
		}
		if err != nil {
			rec.Error = err.Error()
			log.Printf("settlement: process %s: %v", name, err)
			return rec, true
		}
		rec.Processed = true
		rec.Settlements = ids
		rec.PayoutsByMID = payouts
	default:
		log.Printf("settlement: %s carries no recognizable settlement slot, archived only", name)
	}
	return rec, true
}

// process parses a morning settlement file and turns each submerchant's
// total into a settlement record and a payout.
//
// The per-MID grouping comes from the file, not from the platform's own
// ledger: MID is the outlet Worldline settled, and it is the unit the
// platform pays out against.
//
// Each MID's record, and its payout, lands only if no reset came since
// started; the first one that would land after a reset ends it with
// errReset.
func (p *worldlinePuller) process(name string, plaintext []byte, started uint64) (ids []string, payouts int, err error) {
	parsed, err := worldline.ParseBamboraCSV(strings.NewReader(string(plaintext)))
	if err != nil {
		return nil, 0, err
	}
	byMID := parsed.PayoutsByMID()
	payouts = len(byMID)

	currency := parsed.Meta.SettlementCurrency
	valueDate := parsed.Meta.ValueDate
	settlementDate := valueDate
	if ts, perr := time.Parse("2006-01-02", valueDate); perr == nil {
		settlementDate = ts.AddDate(0, 0, 1).Format("2006-01-02")
	}

	mids := make([]string, 0, len(byMID))
	for mid := range byMID {
		mids = append(mids, mid)
	}
	sort.Strings(mids) // deterministic ordering: a file must always process the same way

	for _, mid := range mids {
		amount := byMID[mid]
		if amount <= 0 {
			continue
		}
		report := reportFromFile(parsed, mid, amount, currency, valueDate, settlementDate)
		rec := &settlement.SettlementRecord{
			ID:               "set_" + shortID(),
			MerchantID:       mid,
			TransactionsDate: valueDate,
			Currency:         currency,
			State:            settlement.StateScheduled,
			SettlementDate:   settlementDate,
		}
		var created, settled bool
		if !p.app.guard.apply(started, func() { created, settled = p.app.settleRecord(rec, report, "settlement file processing failed") }) {
			return ids, payouts, errReset
		}
		if created {
			ids = append(ids, rec.ID)
		}
		if !settled {
			continue
		}
		if fresh, gerr := p.app.store.Get(rec.ID); gerr == nil {
			p.app.submitPayout(fresh, started)
		}
	}
	return ids, payouts, nil
}

// reportFromFile builds the settlement report for one MID out of what the
// file actually says. Fees, declines, chargebacks and reserves stay zero:
// the acquirer's file carries no chargeback rows in this lab, and fee
// schedules live in the platform's own product configuration, which is
// deliberately not modelled here.
func reportFromFile(parsed *worldline.ParsedFile, mid string, amountCents int64, currency, valueDate, settlementDate string) *settlement.Report {
	var count int
	for _, b := range parsed.Batches {
		for _, t := range b.Transactions {
			if t.AdditionalRef2 == mid {
				count++
			}
		}
	}
	return &settlement.Report{
		MerchantID:               mid,
		TransactionsDate:         valueDate,
		Currency:                 currency,
		SettlementState:          string(settlement.StateScheduled),
		SettlementDate:           settlementDate,
		ApprovedTransactionCount: count,
		SaleAmountTotal:          amountCents,
		SettlementNetAmount:      amountCents,
	}
}

func (p *worldlinePuller) history() []pulledFile {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]pulledFile, len(p.pulled))
	copy(out, p.pulled)
	return out
}

// routes exposes the pull state, and a way to force a pull rather than
// waiting for the next tick -- which is what a test wants right after it
// has told the acquirer to cut a file.
func (p *worldlinePuller) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /worldline/files", func(w http.ResponseWriter, _ *http.Request) {
		files := p.history()
		httputilx.WriteJSON(w, 200, map[string]any{"count": len(files), "files": files})
	})
	mux.HandleFunc("POST /worldline/pull", func(w http.ResponseWriter, _ *http.Request) {
		n, err := p.pullOnce()
		if err != nil {
			httputilx.Error(w, 502, fmt.Sprintf("pull from %s:%s failed: %v", p.cfg.Host, p.cfg.Port, err))
			return
		}
		httputilx.WriteJSON(w, 200, map[string]any{"pulled": n, "files": p.history()})
	})
}

// envDuration reads a duration from the environment, falling back to def
// when unset or unparseable -- a typo in a poll interval must not stop the
// service booting, same as everywhere else in this lab.
func envDuration(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("settlement: %s=%q is not a duration, using %s", k, v, def)
		return def
	}
	return d
}
