package b4b

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Persistence for everything B4B is asked to remember.
//
// The payout half of this mock was in-memory and that was defensible while
// a payment lived for two seconds. Boarding is not like that: a company,
// its people and its extended profile are the result of a chain a client
// walked once, and a restart that forgets them turns every later run into
// "board it again first". Worse, the extended profile is create-once, so a
// forgotten one silently *un*-breaks a client that should have been told it
// already sent one.
//
// One file, whole-state, write-then-rename -- the same shape the console's
// registry uses. Not a database: the lab's promise is that its state is
// greppable and a restart is cheap.

// State is the on-disk file. Every field is a flat slice rather than a map
// so the file diffs cleanly and reads in a sensible order.
//
// Companies, documents, beneficiaries and payments carry their owner as
// "env" (env.go), left out for the default environment's: a file written
// before environments loads as entirely the default's, and a lab that only
// ever had the default writes the file it always did. Addresses, people,
// extended profiles and vIBANs belong to their company's environment.
type State struct {
	Companies     []*Company         `json:"companies,omitempty"`
	Addresses     []*CompanyAddress  `json:"addresses,omitempty"`
	People        []*Person          `json:"people,omitempty"`
	Extended      []*ExtendedProfile `json:"extended_profiles,omitempty"`
	Documents     []*Document        `json:"documents,omitempty"`
	Vibans        []*Viban           `json:"vibans,omitempty"`
	Beneficiaries []*Beneficiary     `json:"beneficiaries,omitempty"`
	Payments      []*Payment         `json:"payments,omitempty"`
}

// LoadState reads path. A missing file is not an error -- it is a first
// start -- and returns an empty State.
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
		return s, fmt.Errorf("b4b: read state %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("b4b: parse state %s: %w", path, err)
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
	// Write-then-rename: a half-written state file that fails to parse on
	// the next start would lose every boarded company in it.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Snapshot copies the directory out for persistence.
func (d *Directory) Snapshot(s *State) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.companies {
		cp := *c
		s.Companies = append(s.Companies, &cp)
	}
	for _, list := range d.addresses {
		for _, a := range list {
			cp := *a
			s.Addresses = append(s.Addresses, &cp)
		}
	}
	for _, p := range d.people {
		cp := *p
		s.People = append(s.People, &cp)
	}
	for _, e := range d.extended {
		cp := *e
		s.Extended = append(s.Extended, &cp)
	}
	for _, doc := range d.documents {
		cp := *doc
		s.Documents = append(s.Documents, &cp)
	}
	for _, list := range d.vibans {
		for _, v := range list {
			cp := *v
			s.Vibans = append(s.Vibans, &cp)
		}
	}
	sortByCreated(s.Companies, func(c *Company) string { return c.CreatedAt + c.ID })
	sortByCreated(s.Addresses, func(a *CompanyAddress) string { return a.CreatedAt + a.ID })
	sortByCreated(s.People, func(p *Person) string { return p.CreatedAt + p.ID })
	sortByCreated(s.Extended, func(e *ExtendedProfile) string { return e.CreatedAt + e.ID })
	sortByCreated(s.Documents, func(doc *Document) string { return doc.CreatedAt + doc.ID })
	sortByCreated(s.Vibans, func(v *Viban) string { return v.CreatedAt + v.ID })
}

// Restore loads a snapshot back in, replacing whatever is there, each
// record into its own environment. Called once at startup, before the
// service is listening.
func (d *Directory) Restore(s State) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range s.Companies {
		c.Env = owner(c.Env)
		d.companies[c.ID] = c
		if c.ExternalRef != "" {
			d.companiesByRef[refKey(c.Env, c.ExternalRef)] = c
		}
	}
	for _, a := range s.Addresses {
		d.addresses[a.CompanyID] = append(d.addresses[a.CompanyID], a)
	}
	for _, p := range s.People {
		d.people[p.ID] = p
		d.peopleByCompany[p.CompanyID] = append(d.peopleByCompany[p.CompanyID], p.ID)
	}
	for _, e := range s.Extended {
		d.extended[e.CompanyID] = e
	}
	for _, doc := range s.Documents {
		doc.Env = owner(doc.Env)
		d.documents[doc.ID] = doc
	}
	for _, v := range s.Vibans {
		d.vibans[v.CompanyID] = append(d.vibans[v.CompanyID], v)
	}
}

// Snapshot copies registered beneficiaries out. Auto-vivified ones are not
// in the store and so are not written: they are derived from their id and
// come back identically on the next read.
func (s *BeneficiaryStore) Snapshot(st *State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.byID {
		cp := *b
		st.Beneficiaries = append(st.Beneficiaries, &cp)
	}
	sortByCreated(st.Beneficiaries, func(b *Beneficiary) string { return b.ID + "\x00" + b.Env })
}

// Restore loads beneficiaries back in, each into its own environment.
func (s *BeneficiaryStore) Restore(st State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range st.Beneficiaries {
		b.Env = owner(b.Env)
		s.byID[refKey(b.Env, b.ID)] = b
		if b.ExternalRef != "" {
			s.byRef[refKey(b.Env, b.ExternalRef)] = b
		}
	}
}

// Snapshot copies every payment out, in whatever state it had reached.
func (e *Engine) Snapshot(s *State) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range e.payments {
		cp := *p
		s.Payments = append(s.Payments, &cp)
	}
	sortByCreated(s.Payments, func(p *Payment) string { return p.CreatedAt + p.ID })
}

// Restore loads payments back in and resumes the ones that had not reached
// a terminal state, from where they stopped rather than from the beginning.
//
// A payment left mid-lifecycle by a restart would otherwise sit at
// B4BTMPending forever, and the client waiting on its callback would wait
// forever with it -- a failure mode that looks exactly like a lost webhook
// and is not one. Resuming re-fires only the transitions that had not
// happened yet, so a client never sees a state it has already been told
// about twice in a row because of a restart.
//
// A payment a trap had caught is not resumed: its walk was the trap's
// path, and traps -- with the steps they had scheduled -- live in memory and
// went with the process (docs/lab-api/traps.md §6). Walking it the default
// way instead would be a path nobody asked for. The unfinished ones are
// returned, as copies, for the caller to decide (cmd/b4b offers them to the
// standing traps it arms at start); anything it does not take on stays
// where it stopped.
//
// Only what the file holds resumes: a reset writes the file without the
// payments it dropped, so a restart never brings one back. Each payment
// resumes under its own environment's gate.
func (e *Engine) Restore(s State) (held []Payment) {
	byEnv := map[string][]*Payment{}
	var envs []string
	for _, p := range s.Payments {
		p.Env = owner(p.Env)
		if _, ok := byEnv[p.Env]; !ok {
			envs = append(envs, p.Env)
		}
		byEnv[p.Env] = append(byEnv[p.Env], p)
	}
	for _, env := range envs {
		resumable := make([]*Payment, 0, len(byEnv[env]))
		started := e.GateFor(env).Begin(func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			for _, p := range byEnv[env] {
				e.payments[p.ID] = p
				switch {
				case finished(p.State):
				case p.Trap != "":
					held = append(held, *p)
				default:
					resumable = append(resumable, p)
				}
			}
			e.walking.Add(int64(len(resumable)))
		})
		for _, p := range resumable {
			go e.process(p, started)
		}
	}
	return held
}

func shortHex() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func sortByCreated[T any](items []T, key func(T) string) {
	sort.SliceStable(items, func(i, j int) bool { return key(items[i]) < key(items[j]) })
}
