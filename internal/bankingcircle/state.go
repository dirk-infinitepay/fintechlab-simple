package bankingcircle

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/money"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// Persistence for everything Banking Circle is asked to remember.
//
// The bank used to be in memory, and a restart in the middle of a run took
// the run with it: the money a lump sum had landed on a safeguarding
// account, the subscription the platform had made at start-up, the payouts
// still walking their phases and the notifications nobody had been told
// yet. A platform that had done everything right was left holding an id
// the bank no longer knew and waiting for a webhook that would never come.
//
// One file, whole-state, write-then-rename -- B4B's shape (internal/b4b's
// state.go), so the lab's state stays greppable and a restart cheap. What
// is not written, as in B4B: the traps and the steps they had scheduled
// (docs/lab-api/traps.md §6), the activity logs, and the runs a reset
// moves -- a restart starts a new one.

// State is the on-disk file. Every list is flat and sorted, so the file
// diffs cleanly and reads in a sensible order.
//
// Records carry their owner as "env", left out for the default
// environment's, as everywhere else in this package.
type State struct {
	// Accounts is the ledger: balances, the day each one's beginOfDay
	// amount belongs to, and what a reset puts each back to.
	Accounts []*AccountRecord `json:"accounts,omitempty"`
	// Payments is every payment, in whatever state it had reached, and
	// LastReference the last paymentReferenceNumber handed out, so none is
	// handed out twice.
	Payments      []*PaymentRecord `json:"payments,omitempty"`
	LastReference int64            `json:"last_reference,omitempty"`
	// Subscriptions is the store as it stands; Seed is what a reset of
	// scope all puts back -- the subscriptions start-up seeded, on the
	// first start. A file without a seed is a first start (FirstStart).
	Subscriptions *SubscriptionSet `json:"subscriptions,omitempty"`
	Seed          *SubscriptionSet `json:"seed,omitempty"`
	// Notifications is every notification not yet delivered, by the batch
	// it was in and why it was waiting; Paused the subscriptions whose
	// delivery the lab has paused.
	Notifications []*HeldBatch `json:"notifications,omitempty"`
	Paused        []string     `json:"paused,omitempty"`
	// Emails is what the retry schedule mailed: the subscriber's inbox, not
	// the bank's log, so a restart does not empty it.
	Emails []Email `json:"emails,omitempty"`
	// Tokens are the bearer tokens issued and not yet expired, and
	// Customers the environments the bank serves (the parts of each it
	// reads); both are cmd/bankingcircle's, kept here so the bank is one
	// file.
	Tokens    []Token           `json:"tokens,omitempty"`
	Customers []tenant.Customer `json:"customers,omitempty"`
}

// FirstStart reports whether s is a first start's: no file, an empty one,
// or one this bank never wrote (every file it writes has a seed).
func (s State) FirstStart() bool { return s.Seed == nil }

// AccountRecord is one account as the file holds it: the account as the
// API shows it, plus the bookkeeping behind its balance read.
type AccountRecord struct {
	Account
	// OpenDay and OpenBalance are the balance the account opened OpenDay
	// with: beginOfDayAmount, on that day. Empty before its first movement.
	OpenDay     string `json:"open_day,omitempty"`
	OpenBalance string `json:"open_balance,omitempty"`
	// SeedBalance is what a reset puts the balance back to. Empty for an
	// account a payout opened, which a reset of scope all drops.
	SeedBalance string `json:"seed_balance,omitempty"`
}

// PaymentRecord is one payment as the file holds it.
type PaymentRecord struct {
	Payment
	// CaughtBy is the trap that caught the payout, whose path it walks
	// instead of the bank's own. Traps do not survive a restart, so a
	// payout one caught is left where it stopped.
	CaughtBy string `json:"caught_by,omitempty"`
	// MarkedBy is the trap whose step made a return payment, the mark its
	// notifications carry into the lab's log (Payment.Trap).
	MarkedBy string `json:"marked_by,omitempty"`
}

// SubscriptionRecord is one subscription as the file holds it: the wire
// shape, plus what the wire never shows.
type SubscriptionRecord struct {
	Subscription
	Key     string `json:"encryption_key"`
	Owner   string `json:"env,omitempty"`
	Created string `json:"created_at,omitempty"`
	Updated string `json:"updated_at,omitempty"`
}

// SubscriptionSet is a subscription store's contents and its counters,
// which go on from where they were so no id or rowVersion is handed out
// twice.
type SubscriptionSet struct {
	Subscriptions  []*SubscriptionRecord `json:"subscriptions"`
	LastRowVersion uint64                `json:"last_row_version"`
	LastID         int                   `json:"last_id,omitempty"`
}

// Why a held batch was waiting.
const (
	// HeldBatching: on its subscription's queue, for the batch to fill or
	// the flush timer.
	HeldBatching = "batching"
	// HeldSending: on its way out -- being sent, or between retries. It is
	// sent again from its first attempt after a restart: the subscriber may
	// get it twice, as Banking Circle's at-least-once delivery allows, but
	// never not at all.
	HeldSending = "sending"
	// HeldRetained: kept after the retry schedule deactivated the
	// subscription, for its reactivation.
	HeldRetained = "retained"
	// HeldPaused: parked behind the lab's pause, for the resume.
	HeldPaused = "paused"
)

// HeldBatch is notifications for one subscription that had not been
// delivered, and why.
type HeldBatch struct {
	SubscriptionID string               `json:"subscription_id"`
	Env            string               `json:"env,omitempty"`
	Held           string               `json:"held"`
	Notifications  []StoredNotification `json:"notifications"`
}

// StoredNotification is a Notification as the file holds it: the payment
// detail kept as the exact JSON it would have been sent as, and the lab's
// trap mark, which the wire never carries.
type StoredNotification struct {
	EventID             string          `json:"eventId"`
	SubscriptionID      string          `json:"subscriptionId"`
	SubscriptionEventID string          `json:"subscriptionEventId"`
	NotificationType    string          `json:"notificationType"`
	Timestamp           string          `json:"timestamp"`
	Payment             json.RawMessage `json:"payment,omitempty"`
	Payload             json.RawMessage `json:"payload,omitempty"`
	Trap                string          `json:"trap,omitempty"`
}

// Token is one issued bearer token: whose it is and when it stops working.
type Token struct {
	Token   string    `json:"token"`
	Env     string    `json:"env,omitempty"`
	Expires time.Time `json:"expires"`
}

// LoadState reads path. A missing or empty file is not an error -- it is a
// first start -- and neither is an empty path, which keeps everything in
// memory.
func LoadState(path string) (State, error) {
	var s State
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("banking-circle: read state %s: %w", path, err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("banking-circle: parse state %s: %w", path, err)
	}
	return s, nil
}

// SaveState writes the whole state to path. An empty path is a no-op, which
// is what tests and a deliberately ephemeral run want.
func SaveState(path string, s State) error {
	if path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Write-then-rename, synced first: a half-written file that fails to
	// parse on the next start would lose every balance in it.
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// changed calls a store's OnChange, when it has one.
func changed(onChange func()) {
	if onChange != nil {
		onChange()
	}
}

// --- the ledger ------------------------------------------------------------

// Snapshot copies the ledger out.
func (l *Ledger) Snapshot(s *State) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.snapshotLocked(s)
}

func (l *Ledger) snapshotLocked(s *State) {
	for id, a := range l.accts {
		r := &AccountRecord{Account: *a}
		if a.openDay != "" {
			r.OpenDay, r.OpenBalance = a.openDay, money.Format(a.openCents)
		}
		if seed, ok := l.seeded[id]; ok {
			r.SeedBalance = money.Format(seed)
		}
		s.Accounts = append(s.Accounts, r)
	}
	sortBy(s.Accounts, func(r *AccountRecord) string { return r.ID })
}

// Restore loads accounts back in, each into its own environment, over any
// account of the same id (the two the ledger is built with included). Called
// once at start, before the bank serves anything.
func (l *Ledger) Restore(s State) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range s.Accounts {
		a := r.Account
		cents, err := money.Parse(a.Balance)
		if err != nil {
			return fmt.Errorf("account %s balance: %w", a.ID, err)
		}
		a.Env, a.cents, a.Balance = Owner(a.Env), cents, money.Format(cents)
		if r.OpenDay != "" {
			if a.openCents, err = money.Parse(r.OpenBalance); err != nil {
				return fmt.Errorf("account %s open balance: %w", a.ID, err)
			}
			a.openDay = r.OpenDay
		}
		if old, ok := l.accts[a.ID]; ok {
			delete(l.byVIBAN, old.VIBAN)
		}
		l.accts[a.ID] = &a
		l.byVIBAN[a.VIBAN] = &a
		delete(l.seeded, a.ID)
		if r.SeedBalance != "" {
			seed, err := money.Parse(r.SeedBalance)
			if err != nil {
				return fmt.Errorf("account %s seed balance: %w", a.ID, err)
			}
			l.seeded[a.ID] = seed
		}
	}
	return nil
}

// --- the payments ------------------------------------------------------------

// Snapshot copies every payment out, in whatever state it had reached.
func (e *Engine) Snapshot(s *State) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.snapshotLocked(s)
}

func (e *Engine) snapshotLocked(s *State) {
	for _, p := range e.payments {
		s.Payments = append(s.Payments, &PaymentRecord{Payment: *p, CaughtBy: p.caught, MarkedBy: p.trap})
	}
	sortBy(s.Payments, func(r *PaymentRecord) string { return r.CreatedAt + r.ID })
	s.LastReference = e.refSeq
}

// Restore loads payments back in and resumes the ones that had not reached
// the end of the bank's own walk, from where they stopped: a payout still
// OutgoingPaymentBooked is processed, money in still IncomingPaymentBooked
// is processed, each after the engine's delay as if it had just arrived.
// Every notification already fired was fired before the restart, so only
// the ones still to come fire now.
//
// A payout a trap had caught is not resumed: its walk was the trap's, and
// traps -- with the steps they had scheduled -- live in memory and went with
// the process (docs/lab-api/traps.md §6). Processing it the bank's own way
// instead would be a path nobody asked for, so it stays where it stopped.
//
// Only what the file holds resumes: a reset writes the file without the
// payments it dropped, so a restart never brings one back. Each resumes in
// the run now, so a reset after the restart drops it as it would any other.
func (e *Engine) Restore(s State) {
	e.mu.Lock()
	started := e.run.Current()
	var outgoing, incoming []*Payment
	for _, r := range s.Payments {
		p := new(Payment)
		*p = r.Payment
		p.Env, p.epoch, p.caught, p.trap = Owner(p.Env), started, r.CaughtBy, r.MarkedBy
		e.payments[p.ID] = p
		switch {
		case p.State == NotificationOutgoingPaymentBooked && p.caught == "":
			outgoing = append(outgoing, p)
		case p.State == NotificationIncomingPaymentBooked:
			incoming = append(incoming, p)
		}
	}
	if s.LastReference > e.refSeq {
		e.refSeq = s.LastReference
	}
	e.wg.Add(len(outgoing) + len(incoming))
	e.mu.Unlock()
	for _, p := range outgoing {
		go e.process(p)
	}
	for _, p := range incoming {
		go e.processIncoming(p)
	}
}

// --- the subscriptions -------------------------------------------------------

// Snapshot copies the subscriptions out, with the counters behind their ids
// and rowVersions.
func (s *SubscriptionStore) Snapshot(st *State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshotLocked(st)
}

func (s *SubscriptionStore) snapshotLocked(st *State) {
	st.Subscriptions = subscriptionState{subs: s.subs, rowSeq: s.rowSeq, idSeq: s.idSeq}.set()
}

// Restore puts back the subscriptions s holds, replacing the store's, and
// the counters with them. A state with no subscriptions recorded (a first
// start's) leaves the store as it is.
func (s *SubscriptionStore) Restore(st State) {
	if st.Subscriptions == nil {
		return
	}
	in := st.Subscriptions.state()
	s.mu.Lock()
	defer s.mu.Unlock()
	subs, events := copySubscriptions(in.subs)
	s.subs, s.events = subs, events
	s.rowSeq, s.idSeq = in.rowSeq, in.idSeq
}

// set is st as the file holds it: deep copies, sorted by id.
func (st subscriptionState) set() *SubscriptionSet {
	subs, _ := copySubscriptions(st.subs)
	out := &SubscriptionSet{Subscriptions: []*SubscriptionRecord{}, LastRowVersion: st.rowSeq, LastID: st.idSeq}
	for _, sub := range subs {
		out.Subscriptions = append(out.Subscriptions, &SubscriptionRecord{
			Subscription: *sub, Key: sub.EncryptionKey, Owner: sub.Env, Created: sub.CreatedAt, Updated: sub.UpdatedAt,
		})
	}
	sortBy(out.Subscriptions, func(r *SubscriptionRecord) string { return r.ID })
	return out
}

// state is the set back as a store's contents.
func (set *SubscriptionSet) state() subscriptionState {
	st := subscriptionState{subs: map[string]*Subscription{}, rowSeq: set.LastRowVersion, idSeq: set.LastID}
	for _, r := range set.Subscriptions {
		sub := r.Subscription
		sub.EncryptionKey, sub.Env, sub.CreatedAt, sub.UpdatedAt = r.Key, Owner(r.Owner), r.Created, r.Updated
		for _, e := range sub.Events {
			if e.SubscriptionID == "" {
				e.SubscriptionID = sub.ID
			}
		}
		st.subs[sub.ID] = &sub
	}
	return st
}

// --- the notifications -------------------------------------------------------

// Snapshot copies out every notification not yet delivered and the pauses.
func (d *Dispatcher) Snapshot(s *State) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.snapshotLocked(s)
}

func (d *Dispatcher) snapshotLocked(s *State) {
	held := func(id, env, why string, batch []Notification) {
		if len(batch) > 0 {
			s.Notifications = append(s.Notifications, &HeldBatch{SubscriptionID: id, Env: Owner(env), Held: why, Notifications: storeNotifications(batch)})
		}
	}
	for id, q := range d.queues {
		held(id, q.env, HeldBatching, q.items)
	}
	for f := range d.inflight {
		held(f.sub, f.env, HeldSending, f.batch)
	}
	for id, batch := range d.pending {
		held(id, d.owners[id], HeldRetained, batch)
	}
	for id, batch := range d.queued {
		held(id, d.owners[id], HeldPaused, batch)
	}
	sortBy(s.Notifications, func(b *HeldBatch) string {
		first := b.Notifications[0]
		return b.SubscriptionID + "\x00" + b.Held + "\x00" + first.Timestamp + "\x00" + first.EventID
	})
	for id, paused := range d.paused {
		if paused {
			s.Paused = append(s.Paused, id)
		}
	}
	sort.Strings(s.Paused)
}

// Restore puts back the notifications s holds and the pauses, and sends
// what was on its way: every batch that was mid-delivery goes out again from
// its first attempt, and every queue is batched again and flushed on its
// timer. What was retained waits for its subscription's reactivation, and
// what was parked waits behind its pause. A notification for a subscription
// the store no longer has is dropped, as it would have been at its flush.
// Call it after the subscriptions are restored.
func (d *Dispatcher) Restore(s State) {
	subs := map[string]*Subscription{}
	lookup := func(id string) *Subscription {
		if sub, ok := subs[id]; ok {
			return sub
		}
		sub, err := d.subs.Get(id)
		if err != nil {
			d.logf("banking-circle: restore: subscription %s is gone; dropping what was held for it", id)
			sub = nil
		}
		subs[id] = sub
		return sub
	}
	type launch struct {
		sub   *Subscription
		batch []Notification
		f     *flight
	}
	var launches []launch
	var batching []*HeldBatch
	for _, id := range s.Paused {
		lookup(id)
	}
	for _, b := range s.Notifications {
		lookup(b.SubscriptionID)
	}

	d.mu.Lock()
	started := d.run.Current()
	for _, id := range s.Paused {
		if sub := subs[id]; sub != nil {
			d.paused[id] = true
			d.owners[id] = sub.Env
		}
	}
	for _, b := range s.Notifications {
		sub := subs[b.SubscriptionID]
		if sub == nil {
			continue
		}
		batch := restoreNotifications(b.Notifications)
		switch b.Held {
		case HeldSending:
			launches = append(launches, launch{sub: sub, batch: batch, f: d.launchLocked(sub.ID, sub.Env, batch)})
		case HeldRetained:
			d.owners[sub.ID] = sub.Env
			d.pending[sub.ID] = append(d.pending[sub.ID], batch...)
		case HeldPaused:
			d.owners[sub.ID] = sub.Env
			d.queued[sub.ID] = append(d.queued[sub.ID], batch...)
		default:
			batching = append(batching, b)
		}
	}
	d.mu.Unlock()

	for _, l := range launches {
		d.deliverLaunched(l.sub, l.batch, started, l.f)
	}
	for _, b := range batching {
		sub := subs[b.SubscriptionID]
		for _, n := range restoreNotifications(b.Notifications) {
			d.EnqueueFrom(started, sub, n)
		}
	}
}

func storeNotifications(batch []Notification) []StoredNotification {
	out := make([]StoredNotification, 0, len(batch))
	for _, n := range batch {
		out = append(out, StoredNotification{
			EventID: n.EventID, SubscriptionID: n.SubscriptionID, SubscriptionEventID: n.SubscriptionEventID,
			NotificationType: n.NotificationType, Timestamp: n.Timestamp,
			Payment: rawJSON(n.Payment), Payload: rawJSON(n.Payload), Trap: n.Trap,
		})
	}
	return out
}

func restoreNotifications(stored []StoredNotification) []Notification {
	out := make([]Notification, 0, len(stored))
	for _, s := range stored {
		n := Notification{
			EventID: s.EventID, SubscriptionID: s.SubscriptionID, SubscriptionEventID: s.SubscriptionEventID,
			NotificationType: s.NotificationType, Timestamp: s.Timestamp, Trap: s.Trap,
		}
		// The detail goes out as the bytes it was stored as: a JSON number
		// read back into an interface would lose its trailing zero.
		if len(s.Payment) > 0 {
			n.Payment = s.Payment
		}
		if len(s.Payload) > 0 {
			n.Payload = s.Payload
		}
		out = append(out, n)
	}
	return out
}

// rawJSON is v as the JSON it is sent as, nil for nothing.
func rawJSON(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// --- the mailbox -------------------------------------------------------------

// Snapshot copies the emails out.
func (m *MailBox) Snapshot(s *State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshotLocked(s)
}

func (m *MailBox) snapshotLocked(s *State) {
	s.Emails = append([]Email(nil), m.sent...)
}

// Restore puts the emails back, oldest first.
func (m *MailBox) Restore(s State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append([]Email(nil), s.Emails...)
	for i := range m.sent {
		m.sent[i].Env = Owner(m.sent[i].Env)
	}
}

// --- the bank as one ---------------------------------------------------------

// Snapshot copies the whole bank out at one instant: it holds every lock a
// reset holds, in the same order, so a payout's state and the money it moved
// are written together or not at all, and the file never shows a payment
// that settled with its balance still unmoved -- which a restart would
// settle again. The seed goes with it.
func (b *Bank) Snapshot(s *State) {
	b.engine.mu.Lock()
	defer b.engine.mu.Unlock()
	b.ledger.mu.Lock()
	defer b.ledger.mu.Unlock()
	b.disp.mu.Lock()
	defer b.disp.mu.Unlock()
	b.mail.mu.Lock()
	defer b.mail.mu.Unlock()
	b.subs.mu.Lock()
	defer b.subs.mu.Unlock()
	b.engine.snapshotLocked(s)
	b.ledger.snapshotLocked(s)
	b.disp.snapshotLocked(s)
	b.mail.snapshotLocked(s)
	b.subs.snapshotLocked(s)
	s.Seed = b.seed.set()
}

// Restore loads a snapshot back in and resumes the work it was in the middle
// of: the ledger, the subscriptions and the seed a reset of scope all puts
// back, the emails, the notifications (sent on), and the payments (walked
// on). Call it once, after NewBank and before anything is served.
func (b *Bank) Restore(s State) error {
	if err := b.ledger.Restore(s); err != nil {
		return fmt.Errorf("banking-circle: restore: %w", err)
	}
	b.subs.Restore(s)
	if s.Seed != nil {
		b.seed = s.Seed.state()
	}
	b.mail.Restore(s)
	b.disp.Restore(s)
	b.engine.Restore(s)
	return nil
}

// OnChange gives every part of the bank f, called after each change to what
// Snapshot copies. f must not block and must not call into the bank: it is
// called with the changed part's lock held.
func (b *Bank) OnChange(f func()) {
	b.ledger.OnChange, b.engine.OnChange, b.disp.OnChange, b.mail.OnChange, b.subs.OnChange = f, f, f, f, f
}

// Stop ends the bank's work for good, for a process about to exit: what
// sleeps out a delay or a retry wakes, a send or credit on the wire is
// aborted, the effects under way finish, and nothing starts again -- a
// reset included. What was unfinished stays as it was, for a Snapshot to
// write and Restore to pick up: a payout still booked, a batch still to
// send. It never returns control of the bank; call nothing that does work
// after it.
func (b *Bank) Stop() {
	b.stop.Do(func() {
		b.mu.Lock() // no reset now or later
		b.engine.run.stop(tenant.All)
		b.engine.run.hold(tenant.All)
	})
}

func sortBy[T any](items []T, key func(T) string) {
	sort.SliceStable(items, func(i, j int) bool { return key(items[i]) < key(items[j]) })
}
