// Package tenant is the lab's environments as every simulation sees them
// (docs/lab-api/environments.md). One platform using the lab is one
// environment; at each vendor it is one customer, known by the vendor's own
// login — a Banking Circle client, a B4B signing key, an SFTP user — never
// by a lab token, which a platform does not send to a vendor.
//
// A sim tags what each customer makes with the customer's environment, and
// its lab hooks (/sim/*, on the control port) read and change one
// environment at a time: the lab API names it in ?env= or the
// Lab-Environment header. A call that names none is the default
// environment's, which owns everything the lab had before there were
// environments, so a single-tenant lab behaves exactly as it did.
package tenant

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/webduvet/fintechlab-simple/internal/uuidx"
)

const (
	// Default is the environment that owns everything fixed: the lab's
	// built-in credentials, its two safeguarding accounts, the stand-ins.
	Default = "default"
	// All names every environment at once: a lab-wide read or reset.
	All = "*"
	// Header is how the lab API names the environment on a control-port
	// call (and how the admin token picks one on the lab API).
	Header = "Lab-Environment"
	// Param is the same, as a query parameter, for links and downloads.
	Param = "env"
)

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// Valid reports why id cannot name a new environment, or nil. Default and
// All are the lab's own and cannot be created.
func Valid(id string) error {
	switch {
	case id == Default:
		return fmt.Errorf("%q is the lab's own environment", id)
	case id == All:
		return fmt.Errorf("%q means every environment", id)
	case !idPattern.MatchString(id):
		return fmt.Errorf("an environment id is lower-case letters, digits and dashes, starting with a letter, at most 40 (got %q)", id)
	}
	return nil
}

// Normalize is env with the empty string read as Default.
func Normalize(env string) string {
	env = strings.TrimSpace(env)
	if env == "" {
		return Default
	}
	return env
}

// FromRequest is the environment a control-port call names: ?env=, then
// the Lab-Environment header, then Default.
func FromRequest(r *http.Request) string {
	if v := r.URL.Query().Get(Param); v != "" {
		return Normalize(v)
	}
	return Normalize(r.Header.Get(Header))
}

// InScope reports whether a record owned by owner is in what scope names:
// scope All takes every owner, anything else only its own.
func InScope(scope, owner string) bool {
	scope = Normalize(scope)
	return scope == All || scope == Normalize(owner)
}

// The request's environment travels in its context, in a slot an inner
// handler can fill after an outer one has handed the context down: the
// vendor's auth learns who the caller is, and the activity log that wraps
// it reads the answer once the handler returns.
type slotKey struct{}

type slot struct {
	mu  sync.Mutex
	env string
}

// Bind gives ctx a slot for the environment, keeping one it already has.
func Bind(ctx context.Context) context.Context {
	if _, ok := ctx.Value(slotKey{}).(*slot); ok {
		return ctx
	}
	return context.WithValue(ctx, slotKey{}, &slot{})
}

// With is Bind and Set: ctx, carrying env.
func With(ctx context.Context, env string) context.Context {
	ctx = Bind(ctx)
	Set(ctx, env)
	return ctx
}

// Set fills ctx's slot, when it has one.
func Set(ctx context.Context, env string) {
	if s, ok := ctx.Value(slotKey{}).(*slot); ok {
		s.mu.Lock()
		s.env = env
		s.mu.Unlock()
	}
}

// Of is the environment in ctx's slot: Default when there is none.
func Of(ctx context.Context) string {
	if s, ok := ctx.Value(slotKey{}).(*slot); ok {
		s.mu.Lock()
		defer s.mu.Unlock()
		return Normalize(s.env)
	}
	return Default
}

// Named reports whether ctx carries an environment someone set, as opposed
// to falling back to Default: the lab API adds ?env= only then.
func Named(ctx context.Context) (string, bool) {
	if s, ok := ctx.Value(slotKey{}).(*slot); ok {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.env != "" {
			return s.env, true
		}
	}
	return "", false
}

// The default environment's safeguarding accounts: what the lab has always
// had. 978 and 826 are EUR's and GBP's ISO 4217 numeric codes.
const (
	DefaultSafeguardingEUR = "00000000-0000-4000-8000-000000000978"
	DefaultSafeguardingGBP = "00000000-0000-4000-8000-000000000826"
)

// namespaceSafeguarding is the UUIDv5 namespace an environment's
// safeguarding accounts are derived in, apart from every other derived id.
const namespaceSafeguarding = "5f1b0000-0000-4000-8000-5f1b5a6a0000"

// Currencies are the currencies an environment holds a safeguarding
// account in.
var Currencies = []string{"EUR", "GBP"}

// SafeguardingAccount is env's Banking Circle safeguarding account for
// currency: Default's are the lab's two fixed ones, every other
// environment's is derived from its id, so a recreated environment gets the
// same ids back and the lab API, Banking Circle and Worldline agree on them
// without asking each other.
func SafeguardingAccount(env, currency string) string {
	env, currency = Normalize(env), strings.ToUpper(currency)
	if env == Default {
		switch currency {
		case "EUR":
			return DefaultSafeguardingEUR
		case "GBP":
			return DefaultSafeguardingGBP
		}
	}
	return uuidx.Derive(namespaceSafeguarding, env+"/"+currency)
}
