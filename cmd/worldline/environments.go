package main

// Environments: one acquirer, several platforms (docs/lab-api/environments.md
// §7, docs/lab-api/sim-control.md §8).
//
// Each environment the lab creates is one more Worldline customer: an SFTP
// user with a password, and a jail of its own under WORLDLINE_SFTP_ENV_ROOT,
// outside the default's root so the default's user cannot see into it. What
// the default has, each environment has again for itself: its transactions,
// its cycle runs and their lump sums, its published files, its enrolment
// desk, and the guard a reset of it moves. The default environment is the
// acquirer as it always was, on WORLDLINE_SFTP_ROOT, and keeps its own
// login, identifier and safeguarding accounts.

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
	"github.com/webduvet/fintechlab-simple/internal/wlsftp"
	"github.com/webduvet/fintechlab-simple/internal/worldline"
)

// envSim is one environment's acquirer.
type envSim struct {
	env   string
	guard *runGuard
	acq   *settlementApp
	desk  *enrolmentDesk
}

// environments is what sim keeps for the environments besides the default.
type environments struct {
	customers *tenant.Customers
	// root is where each environment's jail is made, one folder per
	// environment: never inside the default's root.
	root string

	mu   sync.Mutex
	envs map[string]*envSim
}

// serveEnvironments makes s serve customers besides the default, each
// jailed in a folder of its own under envRoot. Call it once, before the
// control port is built.
func (s *sim) serveEnvironments(envRoot string) {
	s.tenants = environments{customers: tenant.NewCustomers(), root: envRoot, envs: map[string]*envSim{}}
	s.tenants.customers.OnPut = s.putCustomer
	s.tenants.customers.OnDelete = s.deleteCustomer
}

// defaultEnvRoot is WORLDLINE_SFTP_ENV_ROOT's default: a sibling of the
// default's root, so no jail is inside another.
func defaultEnvRoot(root string) string {
	return filepath.Clean(root) + "-environments"
}

// checkEnvRoot refuses an environment root that is the default's root or
// inside it, or that holds it: either way one environment's user could
// see another's files.
func checkEnvRoot(root, envRoot string) error {
	abs := func(p string) string {
		a, err := filepath.Abs(p)
		if err != nil {
			return filepath.Clean(p)
		}
		return a
	}
	r, e := abs(root), abs(envRoot)
	if within(r, e) || within(e, r) {
		return fmt.Errorf("WORLDLINE_SFTP_ENV_ROOT %s and WORLDLINE_SFTP_ROOT %s overlap: each environment's jail must be outside the default's, and the default's outside every environment's", envRoot, root)
	}
	return nil
}

// within reports whether p is dir or below it.
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// jailOf is env's SFTP root.
func (s *sim) jailOf(env string) (string, error) {
	if err := tenant.Valid(env); err != nil {
		return "", err
	}
	if s.tenants.root == "" {
		return "", errors.New("worldline serves no environments: WORLDLINE_SFTP_ENV_ROOT is unset")
	}
	return filepath.Join(s.tenants.root, env), nil
}

// identifierPattern is what a customer's settlement identifier may be: it
// is a segment of a filename the cycle writes, so nothing that could leave
// the folder or split the name's own underscores ambiguously at its ends.
var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_-]{0,62}[A-Za-z0-9])?$`)

// identifierOf is the settlement identifier c's files are named with: its
// own, else the lab's WORLDLINE_SETTLEMENT_IDENTIFIER.
func (s *sim) identifierOf(c tenant.Customer) (string, error) {
	if c.Worldline == nil || c.Worldline.SettlementIdentifier == "" {
		return s.acq.config().Identifier, nil
	}
	id := c.Worldline.SettlementIdentifier
	if !identifierPattern.MatchString(id) {
		return "", fmt.Errorf("worldline: settlement_identifier %q: want letters, digits, _ and -, starting and ending with a letter or digit, at most 64", id)
	}
	return id, nil
}

// putCustomer is the customer store's OnPut: the environment's jail, with
// the default's folders, and its acquirer. A customer put again keeps
// everything it made and takes its new identifier.
func (s *sim) putCustomer(c tenant.Customer, _ bool) error {
	root, err := s.jailOf(c.Env)
	if err != nil {
		return err
	}
	ident, err := s.identifierOf(c)
	if err != nil {
		return err
	}
	if err := wlsftp.EnsureLayout(root); err != nil {
		return err
	}
	t := &s.tenants
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.envs[c.Env]; ok {
		e.acq.setIdentifier(ident)
		return nil
	}
	t.envs[c.Env] = s.newEnv(c.Env, root, ident)
	return nil
}

// newEnv is env's acquirer, built as the default's is with the parts that
// are the lab's shared: the schedule, the PGP keys, the Banking Circle
// bridge. Its money goes to env's own safeguarding accounts.
func (s *sim) newEnv(env, root, ident string) *envSim {
	guard := &runGuard{}
	cfg := s.acq.config()
	cfg.Identifier = ident
	sga := map[string]string{}
	for _, ccy := range tenant.Currencies {
		sga[ccy] = tenant.SafeguardingAccount(env, ccy)
	}
	acq := &settlementApp{
		env: env, store: worldline.NewStore(), cfg: cfg, root: root, keys: s.acq.keys,
		sgaURL: s.acq.sgaURL, sgaAccount: sga, httpClient: s.acq.httpClient, guard: guard,
	}
	desk := newEnrolmentDesk(root, s.desk.keys, s.desk.delay, s.desk.sweep)
	desk.env, desk.guard = env, guard
	return &envSim{env: env, guard: guard, acq: acq, desk: desk}
}

// deleteCustomer is the customer store's OnDelete: everything env made goes,
// as an all reset would take it, and then its jail with whatever the
// platform left in it. Work of env's still in flight is dropped and none
// starts again.
func (s *sim) deleteCustomer(env string) error {
	root, err := s.jailOf(env)
	if err != nil {
		return err
	}
	t := &s.tenants
	t.mu.Lock()
	e, ok := t.envs[env]
	delete(t.envs, env)
	t.mu.Unlock()
	if ok {
		e.guard.retire(func() {
			e.acq.clearRuns()
			e.acq.store.Clear()
			e.desk.clearRejections()
			e.desk.forgetAnswered()
		})
	}
	s.sftpLog.ClearIn(env)
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("worldline: remove %s's SFTP folder: %w", env, err)
	}
	return nil
}

// at is env's acquirer, the default's included.
func (s *sim) at(env string) (*envSim, bool) {
	env = tenant.Normalize(env)
	if env == tenant.Default {
		return &envSim{env: tenant.Default, guard: s.guard, acq: s.acq, desk: s.desk}, true
	}
	t := &s.tenants
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.envs[env]
	return e, ok
}

// in is every acquirer scope names, the default first: one, or with "*"
// every one.
func (s *sim) in(scope string) []*envSim {
	scope = tenant.Normalize(scope)
	if scope != tenant.All {
		if e, ok := s.at(scope); ok {
			return []*envSim{e}
		}
		return nil
	}
	out := []*envSim{}
	for _, env := range s.tenants.customers.Envs() {
		if e, ok := s.at(env); ok {
			out = append(out, e)
		}
	}
	return out
}

// scope is the environments a /sim call acts in (sim-control.md §8, rule
// 4): what ?env= or the Lab-Environment header names, the default's when
// neither does, every one's for "*". It answers 404 itself for an
// environment worldline has no customer for.
func (s *sim) scope(w http.ResponseWriter, r *http.Request) (string, []*envSim, bool) {
	scope := tenant.FromRequest(r)
	envs := s.in(scope)
	if len(envs) == 0 {
		httputilx.Error(w, 404, "no environment "+scope+" at worldline")
		return scope, nil, false
	}
	return scope, envs, true
}

// one is the single environment a hook that makes something makes it in:
// "*" names no one, and is refused.
func (s *sim) one(w http.ResponseWriter, r *http.Request) (*envSim, bool) {
	scope, envs, ok := s.scope(w, r)
	if !ok {
		return nil, false
	}
	if scope == tenant.All {
		httputilx.Error(w, 400, `name one environment (?env=): this makes something, and "*" is every environment`)
		return nil, false
	}
	return envs[0], true
}

// sftpAccounts is the SFTP server's Accounts: the customers whose Worldline
// login has user as its username, each jailed in its environment's folder.
// One whose acquirer is not set up (its jail could not be made) is still
// named, so its username is refused rather than let in as the default.
func (s *sim) sftpAccounts(user string) []wlsftp.Account {
	if user == "" {
		return nil
	}
	var out []wlsftp.Account
	for _, c := range s.tenants.customers.List() {
		if c.Worldline == nil || c.Worldline.Username != user {
			continue
		}
		acct := wlsftp.Account{User: user, Tag: c.Env}
		if e, ok := s.at(c.Env); ok {
			acct.Password, acct.Root = c.Worldline.Password, e.acq.root
		}
		out = append(out, acct)
	}
	return out
}

// owner is how a record names env: empty for the default, which is how
// every record read before there were environments.
func owner(env string) string {
	if env = tenant.Normalize(env); env != tenant.Default {
		return env
	}
	return ""
}
