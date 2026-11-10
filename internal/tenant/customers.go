package tenant

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
)

// Customer is one environment as a vendor knows it: the logins its platform
// will present. The lab API sends the same Customer to every sim that
// supports environments (PUT /sim/customers/{env}) and each reads its own
// part; none carries a secret a vendor would not hold itself — B4B gets the
// public half of the signing key, never the private one.
type Customer struct {
	Env  string `json:"env"`
	Name string `json:"name,omitempty"`

	BankingCircle *Login    `json:"banking_circle,omitempty"`
	B4B           *B4BKey   `json:"b4b,omitempty"`
	Worldline     *SFTP     `json:"worldline,omitempty"`
	Verification  *Login    `json:"verification,omitempty"`
	Verify        *APIKey   `json:"verify,omitempty"`
	ACI           *Delivery `json:"aci,omitempty"`

	// WebhookHosts are the platform's own callback hosts, which this
	// environment's subscriptions and callbacks may reach on top of the
	// sim's WEBHOOK_ALLOWLIST.
	WebhookHosts []string `json:"webhook_hosts,omitempty"`
}

// Login is a username and password (Banking Circle's Basic credential,
// verification's vendor logins, where the password doubles as API key).
type Login struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// B4BKey is the key an environment signs its B4B JWTs with: its id (kid)
// and its public half.
type B4BKey struct {
	KeyID        string `json:"key_id"`
	PublicKeyPEM string `json:"public_key_pem"`
	// CallbackURL is where B4B calls this environment back when a record
	// names no callback_url of its own; empty, it calls back only records
	// that name one.
	CallbackURL string `json:"callback_url,omitempty"`
}

// SFTP is an environment's Worldline SFTP user and what its settlement
// files are called.
type SFTP struct {
	Username             string `json:"username"`
	Password             string `json:"password"`
	SettlementIdentifier string `json:"settlement_identifier,omitempty"`
}

// APIKey is a single shared secret.
type APIKey struct {
	Key string `json:"key"`
}

// Delivery is where a vendor that only sends delivers this environment's
// webhooks.
type Delivery struct {
	WebhookURL string `json:"webhook_url,omitempty"`
}

// Status is one customer as GET /sim/customers reports it.
type Status struct {
	Env       string     `json:"env"`
	Name      string     `json:"name,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	LastSeen  *time.Time `json:"last_seen"`
}

// ErrUnknown is a customer the sim does not have.
var ErrUnknown = errors.New("no such environment here")

// Customers is a sim's customers, one per environment the lab created. The
// default environment is not among them: it is the sim as it always was,
// and is only tracked for when it was last seen.
//
// The lab API's database is their record, and it pushes them again when a
// sim restarts, so a sim may keep them in memory.
type Customers struct {
	// OnPut and OnDelete are the sim's own work when a customer arrives or
	// goes: opening its accounts, making its SFTP folder, clearing what it
	// made. Both are called without the store's lock held.
	OnPut    func(c Customer, created bool) error
	OnDelete func(env string) error

	mu          sync.Mutex
	m           map[string]*entry
	defaultSeen time.Time
}

type entry struct {
	c        Customer
	created  time.Time
	lastSeen time.Time
}

// NewCustomers is an empty store.
func NewCustomers() *Customers { return &Customers{m: map[string]*entry{}} }

// Put adds or replaces env's customer, and reports whether it is new.
func (s *Customers) Put(c Customer) (bool, error) {
	if err := Valid(c.Env); err != nil {
		return false, err
	}
	s.mu.Lock()
	e, ok := s.m[c.Env]
	if ok {
		e.c = c
	} else {
		s.m[c.Env] = &entry{c: c, created: time.Now().UTC()}
	}
	s.mu.Unlock()
	// A customer the sim could not set up stays known, so its login is
	// refused rather than taken for the default's; the lab API checks a
	// customer before it pushes one, and a corrected one replaces it.
	if s.OnPut != nil {
		if err := s.OnPut(c, !ok); err != nil {
			return !ok, err
		}
	}
	return !ok, nil
}

// Get is env's customer.
func (s *Customers) Get(env string) (Customer, bool) {
	if s == nil {
		return Customer{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[env]
	if !ok {
		return Customer{}, false
	}
	return e.c, true
}

// Delete removes env's customer, after the sim's OnDelete has cleared
// what it made.
func (s *Customers) Delete(env string) error {
	s.mu.Lock()
	_, ok := s.m[env]
	s.mu.Unlock()
	if !ok {
		return ErrUnknown
	}
	if s.OnDelete != nil {
		if err := s.OnDelete(env); err != nil {
			return err
		}
	}
	s.mu.Lock()
	delete(s.m, env)
	s.mu.Unlock()
	return nil
}

// Find is the first customer match accepts, in no particular order: the
// vendor's auth looking a login up.
func (s *Customers) Find(match func(Customer) bool) (Customer, bool) {
	if s == nil {
		return Customer{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.m {
		if match(e.c) {
			return e.c, true
		}
	}
	return Customer{}, false
}

// List is every customer, by environment id.
func (s *Customers) List() []Customer {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Customer, 0, len(s.m))
	for _, e := range s.m {
		out = append(out, e.c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Env < out[j].Env })
	return out
}

// Envs is every environment this sim serves, Default first.
func (s *Customers) Envs() []string {
	out := []string{Default}
	for _, c := range s.List() {
		out = append(out, c.Env)
	}
	return out
}

// Seen records that env just called the vendor surface.
func (s *Customers) Seen(env string) {
	if s == nil {
		return
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if Normalize(env) == Default {
		s.defaultSeen = now
		return
	}
	if e, ok := s.m[env]; ok {
		e.lastSeen = now
	}
}

// Status is every customer and when it was last seen, Default first.
func (s *Customers) Status() []Status {
	if s == nil {
		return []Status{{Env: Default}}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Status{{Env: Default, LastSeen: when(s.defaultSeen)}}
	envs := make([]string, 0, len(s.m))
	for env := range s.m {
		envs = append(envs, env)
	}
	sort.Strings(envs)
	for _, env := range envs {
		e := s.m[env]
		out = append(out, Status{Env: env, Name: e.c.Name, CreatedAt: e.created, LastSeen: when(e.lastSeen)})
	}
	return out
}

func when(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// Routes serves the customer hooks on a sim's control port:
//
//	GET    /sim/customers        every customer and when it was last seen
//	PUT    /sim/customers/{env}  add or replace one (a Customer)
//	DELETE /sim/customers/{env}  remove one, clearing what it made
func (s *Customers) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /sim/customers", func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, map[string]any{"customers": s.Status()})
	})
	mux.HandleFunc("PUT /sim/customers/{env}", func(w http.ResponseWriter, r *http.Request) {
		env := r.PathValue("env")
		raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			httputilx.Error(w, 400, err.Error())
			return
		}
		var c Customer
		if err := json.Unmarshal(raw, &c); err != nil {
			httputilx.Error(w, 400, "want a customer: "+err.Error())
			return
		}
		if c.Env == "" {
			c.Env = env
		}
		if c.Env != env {
			httputilx.Error(w, 400, "the body names "+c.Env+", the path "+env)
			return
		}
		created, err := s.Put(c)
		if err != nil {
			httputilx.Error(w, 400, err.Error())
			return
		}
		httputilx.WriteJSON(w, 200, map[string]any{"env": env, "created": created})
	})
	mux.HandleFunc("DELETE /sim/customers/{env}", func(w http.ResponseWriter, r *http.Request) {
		env := r.PathValue("env")
		switch err := s.Delete(env); {
		case errors.Is(err, ErrUnknown):
			httputilx.Error(w, 404, "no environment "+env+" here")
		case err != nil:
			httputilx.Error(w, 500, err.Error())
		default:
			httputilx.WriteJSON(w, 200, map[string]any{"env": env, "deleted": true})
		}
	})
}
