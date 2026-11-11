package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/bankingcircle"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// The bank's state on disk (BC_STATE_PATH, internal/bankingcircle's
// state.go): the ledger, the payments, the subscriptions, the
// notifications not yet delivered, the emails, the bearer tokens and the
// customers. A restart picks up where the last process stopped: a platform
// keeps its balance, its subscription and its token, and a payout walking
// its phases walks on.

// stateWriteDelay is how long a change waits for the ones right behind it
// before the file is written. The bank changes in bursts -- a payout is a
// booking, a notification queued, a batch sent, a ledger move, another
// notification -- and one write per burst is as good as one per change.
const stateWriteDelay = 250 * time.Millisecond

// stateFile writes the bank's state to path: on every change, coalesced by
// one writer (touch), and at once when something must be on disk before it
// answers (flush). A nil stateFile, or one with no path, writes nothing.
type stateFile struct {
	path  string
	snap  func() bankingcircle.State
	delay time.Duration

	// writeMu makes each write one snapshot-and-save, so writes land in the
	// order their snapshots were taken: a stale one can never overwrite a
	// reset's.
	writeMu sync.Mutex
	dirty   chan struct{} // a change not yet written
	quit    chan struct{}
	done    chan struct{}
	once    sync.Once
}

func newStateFile(path string, snap func() bankingcircle.State, delay time.Duration) *stateFile {
	return &stateFile{
		path: path, snap: snap, delay: delay,
		dirty: make(chan struct{}, 1), quit: make(chan struct{}), done: make(chan struct{}),
	}
}

// touch says something changed. It never blocks: a change already waiting
// to be written covers this one, since the write snapshots whatever is
// there when it happens.
func (f *stateFile) touch() {
	if f == nil || f.path == "" {
		return
	}
	select {
	case f.dirty <- struct{}{}:
	default:
	}
}

// flush writes the state now.
func (f *stateFile) flush() error {
	if f == nil || f.path == "" {
		return nil
	}
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	return bankingcircle.SaveState(f.path, f.snap())
}

// start runs the writer: every change is written within delay of the last
// one before it. A change that lands while a write is under way leaves the
// writer another write to do, so the last change is never left unwritten.
func (f *stateFile) start() {
	if f == nil || f.path == "" {
		return
	}
	go func() {
		defer close(f.done)
		for {
			select {
			case <-f.quit:
				return
			case <-f.dirty:
			}
			select {
			case <-f.quit:
				return
			case <-time.After(f.delay):
			}
			if err := f.flush(); err != nil {
				// Logged, not fatal: losing the ability to remember is bad,
				// but taking the bank down mid-run because a disk is full is
				// worse, and the state in memory is still right.
				log.Printf("banking-circle: persist state to %s: %v", f.path, err)
			}
		}
	}()
}

// close stops the writer and waits for a write under way to finish. What
// it had not written yet is the caller's final flush.
func (f *stateFile) close() {
	if f == nil || f.path == "" {
		return
	}
	f.once.Do(func() { close(f.quit) })
	select {
	case <-f.done:
	case <-time.After(10 * time.Second):
	}
}

// open is the bank's start-up from its state file: path's state is read,
// seed runs on a first start (no file, or an empty one) to make what a first
// start makes, and everything is restored -- the customers first, so a
// notification sent on at once is checked against its customer's own
// webhook hosts -- and the work it was in the middle of resumed. From here
// on every change is written to path. An empty path keeps everything in
// memory.
func (a *app) open(path string, seed func() error) error {
	state, err := bankingcircle.LoadState(path)
	if err != nil {
		return err
	}
	first := state.FirstStart()
	if first && seed != nil {
		if err := seed(); err != nil {
			return err
		}
	}
	// Taken after the seed, before any listener: the subscriptions as they
	// stand now are what a reset of scope all puts back -- unless the file
	// says what start-up seeded the first time, which Restore puts back.
	a.bank = bankingcircle.NewBank(a.engine, a.dispatch)
	a.state = newStateFile(path, a.snapshot, stateWriteDelay)
	a.bank.OnChange(a.state.touch)
	a.tokens.onChange = a.state.touch
	a.customers.OnChange = a.state.touch

	for _, c := range state.Customers {
		if _, err := a.customers.Put(c); err != nil {
			log.Printf("banking-circle: restore customer %s: %v", c.Env, err)
		}
	}
	a.tokens.restore(state.Tokens)
	if err := a.bank.Restore(state); err != nil {
		return err
	}
	// Written now, so a first start's seed is on disk before anything is
	// served, and a disk the bank cannot write to says so at start.
	if err := a.state.flush(); err != nil {
		log.Printf("banking-circle: state %s cannot be written, so a restart would lose what this one learns: %v", path, err)
	}
	a.state.start()
	if path != "" {
		verb := "restored"
		if first {
			verb = "first start"
		}
		log.Printf("banking-circle: state %s %s: %d account(s), %d payment(s), %d subscription(s), %d batch(es) of notifications held, %d token(s), %d customer(s)",
			path, verb, len(state.Accounts), len(state.Payments), subscriptionCount(state.Subscriptions),
			len(state.Notifications), len(state.Tokens), len(state.Customers))
	}
	return nil
}

func subscriptionCount(set *bankingcircle.SubscriptionSet) int {
	if set == nil {
		return 0
	}
	return len(set.Subscriptions)
}

// snapshot is everything the file holds: the bank at one instant, then the
// tokens and the customers.
func (a *app) snapshot() bankingcircle.State {
	var s bankingcircle.State
	a.bank.Snapshot(&s)
	s.Tokens = a.tokens.snapshot()
	for _, c := range a.customers.List() {
		s.Customers = append(s.Customers, customerRecord(c))
	}
	return s
}

// customerRecord is the part of a customer this bank reads, the only part
// it writes down: its environment, its name, its Banking Circle login and
// its webhook hosts. Another vendor's credentials are not this bank's to
// keep. The lab API pushes the whole customer again when the bank restarts
// (its sync follows GET /sim/info's started_at), so nothing here goes
// stale.
func customerRecord(c tenant.Customer) tenant.Customer {
	out := tenant.Customer{Env: c.Env, Name: c.Name, WebhookHosts: c.WebhookHosts}
	if c.BankingCircle != nil {
		login := *c.BankingCircle
		out.BankingCircle = &login
	}
	return out
}

// persistNow writes the state file before the caller answers: a reset, or a
// customer deleted, must not come back with a restart.
func (a *app) persistNow() error {
	if err := a.state.flush(); err != nil {
		return fmt.Errorf("the state file was not written, so a restart would bring back what was cleared: %w", err)
	}
	return nil
}

// stop is the bank's last act before the process exits (SIGTERM): its work
// ends where it stands, and the state is written once more with nothing
// moving, so a payout still booked and a batch still to send are on disk to
// pick up after the restart.
func (a *app) stop() {
	a.bank.Stop()
	a.state.close()
	if err := a.state.flush(); err != nil {
		log.Printf("banking-circle: stopping: the state file was not written: %v", err)
		return
	}
	if a.state != nil && a.state.path != "" {
		log.Printf("banking-circle: stopping: state written to %s", a.state.path)
	}
}

// shutdown stops the listeners -- each finishes what it was answering, and
// takes nothing new -- and then the bank (stop).
func (a *app) shutdown(servers ...*http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Add(1)
		go func(srv *http.Server) {
			defer wg.Done()
			if err := srv.Shutdown(ctx); err != nil {
				log.Printf("banking-circle: stopping %s: %v", srv.Addr, err)
			}
		}(srv)
	}
	wg.Wait()
	a.stop()
}

// --- the tokens -------------------------------------------------------------

// snapshot is every token still valid, soonest to expire first.
func (s *tokenStore) snapshot() []bankingcircle.Token {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]bankingcircle.Token, 0, len(s.tokens))
	for tok, t := range s.tokens {
		if now.After(t.expires) {
			continue
		}
		out = append(out, bankingcircle.Token{Token: tok, Env: bankingcircle.Owner(t.env), Expires: t.expires})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Expires.Equal(out[j].Expires) {
			return out[i].Expires.Before(out[j].Expires)
		}
		return out[i].Token < out[j].Token
	})
	return out
}

// restore takes back the tokens that have not expired, each its own
// customer's: a platform's cached token keeps working across a restart, as
// it would across a real bank's deploy.
func (s *tokenStore) restore(tokens []bankingcircle.Token) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range tokens {
		if t.Token == "" || now.After(t.Expires) {
			continue
		}
		s.tokens[t.Token] = issuedToken{expires: t.Expires, env: tenant.Normalize(t.Env)}
	}
}
