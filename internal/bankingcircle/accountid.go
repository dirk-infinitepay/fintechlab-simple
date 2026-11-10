package bankingcircle

import (
	"strings"

	"github.com/webduvet/fintechlab-simple/internal/tenant"
	"github.com/webduvet/fintechlab-simple/internal/uuidx"
)

// Account identifiers.
//
// Banking Circle identifies an account by a UUID, and so does this
// simulation. That is not cosmetic. A client reads an account id out of
// its own configuration and puts it in a URL path, and the services in
// front of Banking Circle validate that path segment -- buddy's
// apps/banking-circle answers 400 "Validation failed (uuid is expected)"
// before the request ever leaves the platform. A lab that named its
// safeguarding account "bc_acc_sga_eur" therefore could not be reached
// through the platform's own Banking Circle service at all; it could only
// be reached by bypassing it, which meant the one code path the lab exists
// to exercise was the one path never taken.
//
// The ids below are obviously synthetic on sight -- a run of zeroes ending
// in the currency's ISO 4217 numeric code -- so nobody mistakes one for a
// real Banking Circle account, while still being structurally valid UUIDs
// that every validator in the chain accepts.

const (
	// SGAAccountEUR and SGAAccountGBP are the default environment's
	// safeguarding accounts, one per currency. 978 and 826 are EUR's and
	// GBP's ISO 4217 numeric codes, which is what makes these readable
	// rather than arbitrary. They are the lab's own (internal/tenant), so
	// the lab API and Worldline name the same two; every other
	// environment's are tenant.SafeguardingAccount's, derived from its id.
	SGAAccountEUR = tenant.DefaultSafeguardingEUR
	SGAAccountGBP = tenant.DefaultSafeguardingGBP
)

// NamespaceAccount is the UUIDv5 namespace for derived account ids.
//
// Separate from any other derived identifier's namespace so that a
// beneficiary and an account built from the same string can never collide.
const NamespaceAccount = "5f1b0000-0000-4000-8000-5f1b1abc0de0"

// AccountIDFor returns the Banking Circle account id for a stable external
// key -- a beneficiary id, a merchant IBAN, whatever the naming party
// happens to hold.
//
// In reality Banking Circle assigns account ids and a client stores the
// one it was given; nothing invents them. The lab has no onboarding call
// to hand them out, so the parties derive the same id from the same key
// instead. The effect a client sees is identical -- a UUID it did not
// choose, stable across restarts -- and, unlike a random id, two
// independent processes reach it without talking to each other, which is
// what lets B4B bridge a payout to the same account the e2e suite then
// inspects.
//
// A key that is already a UUID is returned unchanged: when the platform
// has a real account id, that is the account, and re-deriving would send
// the money somewhere else.
func AccountIDFor(key string) string {
	k := strings.TrimSpace(key)
	if uuidx.Valid(k) {
		return k
	}
	return uuidx.Derive(NamespaceAccount, k)
}

// AccountIDIn is AccountIDFor in one environment (internal/tenant): the
// account a customer's payout to key lands on. The default environment's is
// AccountIDFor's, so every id the lab handed out before there were
// environments stays where it was. Every other environment's is derived
// from the environment and that id, so two platforms paying the same
// beneficiary never share an account, and neither ever lands on one the
// default could name. AccountIDIn(env, key) and
// AccountIDIn(env, AccountIDFor(key)) are the same account.
func AccountIDIn(env, key string) string {
	id := AccountIDFor(key)
	if env = tenant.Normalize(env); env == tenant.Default {
		return id
	}
	return uuidx.Derive(NamespaceAccount, env+"/"+id)
}

// Owner is how a record names the environment that owns it: "" for the
// default's, so a record reads as it did before there were environments,
// and the environment's id otherwise.
func Owner(env string) string {
	if env = tenant.Normalize(env); env != tenant.Default {
		return env
	}
	return ""
}

// ValidAccountID reports whether id is shaped like a Banking Circle
// account id.
func ValidAccountID(id string) bool { return uuidx.Valid(id) }
