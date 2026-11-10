package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"

	"github.com/webduvet/fintechlab-simple/internal/allowlist"
	"github.com/webduvet/fintechlab-simple/internal/bankingcircle"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

// Environments (docs/lab-api/environments.md, the sim's side in
// docs/lab-api/sim-control.md §8). Each platform using the lab is one
// environment, and to this bank one customer, known by its Banking Circle
// login. The lab API pushes the customers to the control port
// (PUT /sim/customers/{env}); a login no customer has is the default
// environment's, as every login was before there were environments.
//
// Every account, payment, subscription and email has an owner. A customer
// sees and changes only its own on the vendor surface, its notifications go
// only to its own subscriptions, and its payouts debit its own
// safeguarding accounts. The lab's hooks act in the environment the call
// names (scopeOf).

// newCustomers is this bank's customer store: a customer arriving opens its
// two safeguarding accounts, one leaving takes everything it made with it.
func (a *app) newCustomers() *tenant.Customers {
	c := tenant.NewCustomers()
	c.OnPut = a.customerArrived
	c.OnDelete = a.customerLeft
	return c
}

// customerArrived opens a customer's safeguarding accounts, EUR and GBP
// (tenant.SafeguardingAccount), at zero. A customer replaced keeps the ones
// it has, money and all.
func (a *app) customerArrived(c tenant.Customer, created bool) error {
	holder := firstNonEmpty(c.Name, c.Env)
	ids := make([]string, 0, len(tenant.Currencies))
	for _, ccy := range tenant.Currencies {
		acc, err := a.ledger.OpenSafeguarding(c.Env, ccy, holder+" Safeguarding Account "+ccy)
		if err != nil {
			return err
		}
		ids = append(ids, acc.ID+" ("+ccy+")")
	}
	login := "no Banking Circle login"
	if c.BankingCircle != nil {
		login = "login " + c.BankingCircle.Username
	}
	verb := "replaced"
	if created {
		verb = "added"
	}
	log.Printf("banking-circle: customer %s %s (%s); safeguarding accounts %s", c.Env, verb, login, strings.Join(ids, ", "))
	return nil
}

// customerLeft clears everything a customer made, as a reset of scope all
// of its environment would, and its safeguarding accounts and tokens with
// it: DELETE /sim/customers/{env}. Its tokens go first, so nothing it sends
// from here on lands.
func (a *app) customerLeft(env string) error {
	tokens := a.tokens.drop(env)
	var traps, steps int
	if a.traps != nil {
		traps, steps = a.traps.ResetIn(env)
	}
	cleared := a.bank.Forget(env, func(cleared map[string]int) {
		cleared["activity"] = int(a.payLog.ClearIn(env) + a.notifLog.ClearIn(env) + a.reportLog.ClearIn(env))
	})
	cleared["tokens"], cleared["traps"], cleared["trap_steps"] = tokens, traps, steps
	log.Printf("banking-circle: customer %s deleted — cleared %v", env, cleared)
	return nil
}

// customerFor is the environment a Banking Circle login belongs to: the
// customer whose login user and pass are. A customer's username with
// another password is refused (false), as a bad login is. Any other login
// is the default environment's.
func (a *app) customerFor(user, pass string) (string, bool) {
	hasUser := func(c tenant.Customer) bool { return c.BankingCircle != nil && c.BankingCircle.Username == user }
	if c, ok := a.customers.Find(func(c tenant.Customer) bool {
		return hasUser(c) && subtle.ConstantTimeCompare([]byte(c.BankingCircle.Password), []byte(pass)) == 1
	}); ok {
		return c.Env, true
	}
	if _, ok := a.customers.Find(hasUser); ok {
		return "", false
	}
	return tenant.Default, true
}

// knownEnv reports whether env is one this bank serves: the default, or a
// customer's.
func (a *app) knownEnv(env string) bool {
	if env = tenant.Normalize(env); env == tenant.Default {
		return true
	}
	_, ok := a.customers.Get(env)
	return ok
}

// scopeOf is the environment a call acts in. On LISTEN it is the caller's
// own, which requireBearer learnt from its token. On the control port and
// the bridge it is the one the call names (tenant.FromRequest: ?env=, then
// the Lab-Environment header, else the default; "*" for every one).
func scopeOf(r *http.Request) string {
	if env, ok := tenant.Named(r.Context()); ok {
		return tenant.Normalize(env)
	}
	return tenant.FromRequest(r)
}

// names reports whether the call names an environment at all.
func names(r *http.Request) bool {
	if _, ok := tenant.Named(r.Context()); ok {
		return true
	}
	return r.URL.Query().Get(tenant.Param) != "" || strings.TrimSpace(r.Header.Get(tenant.Header)) != ""
}

// oneEnv is the one environment a hook that makes something acts in:
// scopeOf, refused for "*" (400) and for an environment this bank does not
// serve (404). It answers the refusal itself.
func (a *app) oneEnv(w http.ResponseWriter, r *http.Request, what string) (string, bool) {
	env := scopeOf(r)
	switch {
	case env == tenant.All:
		httputilx.Error(w, 400, what+" is one environment's: name it with ?env= or "+tenant.Header+", not \"*\"")
		return "", false
	case !a.knownEnv(env):
		httputilx.Error(w, 404, "no environment "+env+" at banking-circle")
		return "", false
	}
	return env, true
}

// allowed is the webhook destination rule for env: the sim's
// WEBHOOK_ALLOWLIST, or the customer's own webhook_hosts.
func (a *app) allowed(env, dest string) error {
	err := a.list.Allowed(dest)
	if err == nil {
		return nil
	}
	if c, ok := a.customers.Get(tenant.Normalize(env)); ok && len(c.WebhookHosts) > 0 {
		if own, perr := allowlist.Parse(strings.Join(c.WebhookHosts, ",")); perr == nil && own.Allowed(dest) == nil {
			return nil
		}
	}
	return err
}

// sgaAccountID is env's safeguarding account for currency: debited by its
// payouts, credited by its lump sums. The default's are the lab's two fixed
// ones; a customer's are opened when it arrives.
func (a *app) sgaAccountID(env, currency string) (string, error) {
	if !slices.Contains(tenant.Currencies, currency) {
		return "", fmt.Errorf("no safeguarding account configured for currency %q", currency)
	}
	id := tenant.SafeguardingAccount(env, currency)
	if _, err := a.ledger.GetIn(env, id); err != nil {
		return "", fmt.Errorf("environment %s has no %s safeguarding account", tenant.Normalize(env), currency)
	}
	return id, nil
}

// creditor is the account env's payout to accountId lands on, opened on
// first use. The default's is accountId itself, as it always was. Another
// environment's is accountId when that is already one of its own accounts,
// and otherwise bankingcircle.AccountIDIn(env, accountId): B4B derives the
// same id from the same beneficiary for everyone, and two platforms paying
// one beneficiary must not share an account.
func (a *app) creditor(env, accountID, currency string) (*bankingcircle.Account, error) {
	if !bankingcircle.ValidAccountID(accountID) {
		return nil, fmt.Errorf("%w: %q", bankingcircle.ErrInvalidAccountID, accountID)
	}
	id := accountID
	if tenant.Normalize(env) != tenant.Default {
		if _, err := a.ledger.GetIn(env, id); err != nil {
			id = bankingcircle.AccountIDIn(env, accountID)
		}
	}
	return a.ledger.GetOrCreateIn(env, id, currency)
}

// foreignSubscription reports whether id is a subscription outside the
// call's scope: answered as an unknown one is, 404.
func (a *app) foreignSubscription(r *http.Request, id string) bool {
	sub, err := a.subs.Get(id)
	return err == nil && !tenant.InScope(scopeOf(r), sub.Env)
}

// foreignEvent is foreignSubscription for a subscription event.
func (a *app) foreignEvent(r *http.Request, id string) bool {
	if _, err := a.subs.GetEvent(id); err != nil {
		return false
	}
	_, err := a.subs.GetEventIn(scopeOf(r), id)
	return err != nil
}

// asCaller serves a control route on LISTEN, behind the bearer (the old
// /sim paths, until phase 4 of docs/lab-api/plan.md retires them), as the
// token's own environment: whatever environment the call names is replaced
// by the caller's, a reset's body included, so a customer's login reaches
// its own records and nobody else's. What is the lab's alone is not served
// here: the customers, and for a customer's login, the process log.
func (a *app) asCaller(ctl http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		env := tenant.Of(r.Context())
		switch {
		case r.URL.Path == "/sim/customers" || strings.HasPrefix(r.URL.Path, "/sim/customers/"):
			httputilx.Error(w, 404, "the customers are the lab's: on the control port only")
			return
		case r.URL.Path == "/sim/logs" && env != tenant.Default:
			httputilx.Error(w, 403, "the process log is every environment's: on the control port only")
			return
		}
		r = r.Clone(r.Context())
		q := r.URL.Query()
		q.Set(tenant.Param, env)
		r.URL.RawQuery = q.Encode()
		r.Header.Set(tenant.Header, env)
		if r.Method == http.MethodPost && r.URL.Path == "/sim/reset" {
			body, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
			if err != nil {
				httputilx.Error(w, 400, err.Error())
				return
			}
			var fields map[string]json.RawMessage
			if len(bytes.TrimSpace(body)) == 0 || json.Unmarshal(body, &fields) == nil {
				if fields == nil {
					fields = map[string]json.RawMessage{}
				}
				fields["env"], _ = json.Marshal(env)
				body, _ = json.Marshal(fields)
			}
			r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		}
		ctl.ServeHTTP(w, r)
	}
}
