// Bank is a tiny in-memory ledger with obviously fake IBANs (GB00SIM...).
//
// It stays one bank, the merchants' own, whichever platform's payout
// arrives. Each environment's accounts are kept apart all the same
// (internal/tenant): a credit is the environment's that Banking Circle says
// it is, in the Lab-Environment header, and opens or credits that
// environment's account for the IBAN, so the same merchant paid from two
// environments is two accounts and neither sees the other's money. Every
// route acts in the environment the call names (?env= or the header), the
// default's when it names none; the seeded accounts are the default's.
// The bank has no logins: its customers exist so the lab can name, reset
// and remove an environment here.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/logring"
	"github.com/webduvet/fintechlab-simple/internal/money"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
	"github.com/webduvet/fintechlab-simple/internal/tenant"
)

type account struct {
	ID       string `json:"id"`
	IBAN     string `json:"iban"`
	Holder   string `json:"holder"`
	Currency string `json:"currency"`
	Balance  string `json:"balance"`
	// Kind says whose money this is: "infinite" for the programme's own
	// account, "merchant" for a settled outlet, "demo" for the seeded
	// examples. The console groups by it, and a payout arriving into an
	// account nobody can name is a thing worth being able to see.
	Kind string `json:"kind"`
	// OpenedBy records how the account came to exist — "seed" or the
	// reference of the credit that auto-opened it. A beneficiary bank
	// opens an account the first time money arrives for an IBAN, and
	// which credit did that is the question asked afterwards.
	OpenedBy string `json:"opened_by,omitempty"`
	// Env is the environment the account is (internal/tenant). Empty is
	// the default's.
	Env   string `json:"env,omitempty"`
	cents int64
}

type entry struct {
	ID        string `json:"id"`
	AccountID string `json:"accountId"`
	PaymentID string `json:"paymentId,omitempty"`
	Amount    string `json:"amount"`
	Currency  string `json:"currency"`
	Ref       string `json:"reference,omitempty"`
	At        string `json:"at"`
	Env       string `json:"env,omitempty"`
}

// store is the bank: a book per environment.
type store struct {
	// customers are the environments besides the default. The bank has no
	// logins to check them by; it only needs to know which exist.
	customers *tenant.Customers

	mu    sync.Mutex
	books map[string]*book
}

// book is one environment's accounts and ledger.
type book struct {
	accts   map[string]*account
	byIBAN  map[string]*account
	entries []entry
	seq     int
}

func newBook() *book {
	return &book{accts: map[string]*account{}, byIBAN: map[string]*account{}}
}

// envTag is env as a record carries it: empty for the default.
func envTag(env string) string {
	if env = tenant.Normalize(env); env == tenant.Default {
		return ""
	}
	return env
}

// seedBook is the default environment's book on first start: the two
// accounts the settle path ends in, and nothing else that matters.
//
// The programme has one account per currency — that is where
// INFINITE_SETTLEMENT lands, and it is separate from every merchant's, so
// "did the platform get its share" is a balance rather than an inference.
// Merchant accounts are not seeded: a beneficiary bank does not know an
// IBAN until money arrives for it, and opening them on demand is both more
// honest and less coordination between this service and the console.
func seedBook() *book {
	b := newBook()
	b.mustSeed("acc_infinite_eur", "GB00SIMINF00000001", "InfinitePay Ltd", "EUR", 0, kindInfinite)
	b.mustSeed("acc_infinite_gbp", "GB00SIMINF00000002", "InfinitePay Ltd", "GBP", 0, kindInfinite)

	// Two funded demo accounts, so `make demo-payment` and the payment-api
	// scenario have somewhere to move money from without a settlement run.
	b.mustSeed("acc_alice", "GB00SIM0000000000001", "Alice Simulator", "EUR", 1_000_000, kindDemo)
	b.mustSeed("acc_bob", "GB00SIM0000000000002", "Bob Simulator", "EUR", 50_000, kindDemo)
	b.mustSeed("acc_merchant", "GB00SIM0000000000003", "Merchant Simulator", "EUR", 0, kindDemo)
	return b
}

// newStore is the bank on first start: the default's seeded book, and no
// other environment yet.
func newStore() *store {
	return &store{customers: tenant.NewCustomers(), books: map[string]*book{tenant.Default: seedBook()}}
}

// bookLocked is env's book, made on first use. s.mu is held.
func (s *store) bookLocked(env string) *book {
	env = tenant.Normalize(env)
	b := s.books[env]
	if b == nil {
		b = newBook()
		s.books[env] = b
	}
	return b
}

// booksLocked is every book in scope, the default's first, then by
// environment. s.mu is held.
func (s *store) booksLocked(scope string) []*book {
	envs := make([]string, 0, len(s.books))
	for env := range s.books {
		if tenant.InScope(scope, env) {
			envs = append(envs, env)
		}
	}
	sort.Slice(envs, func(i, j int) bool {
		if envs[i] == tenant.Default || envs[j] == tenant.Default {
			return envs[i] == tenant.Default
		}
		return envs[i] < envs[j]
	})
	out := make([]*book, 0, len(envs))
	for _, env := range envs {
		out = append(out, s.books[env])
	}
	return out
}

// scope is the environment a call names, when the bank serves it: one
// environment, or every one ("*") for a read. A call that changes
// something names one. The environment goes into the call's context, for
// its activity event.
func (s *store) scope(w http.ResponseWriter, r *http.Request, changes bool) (string, bool) {
	env := tenant.FromRequest(r)
	switch {
	case env == tenant.All && changes:
		httputilx.Error(w, 400, "money moves in one environment, not in all of them: name one")
		return "", false
	case env == tenant.Default || env == tenant.All:
	default:
		if _, ok := s.customers.Get(env); !ok {
			httputilx.Error(w, 404, "no environment "+env+" at the bank")
			return "", false
		}
	}
	tenant.Set(r.Context(), env)
	return env, true
}

const (
	kindInfinite = "infinite"
	kindMerchant = "merchant"
	kindDemo     = "demo"
)

func (b *book) mustSeed(id, iban, holder, ccy string, cents int64, kind string) {
	a := &account{ID: id, IBAN: iban, Holder: holder, Currency: ccy, cents: cents,
		Balance: money.Format(cents), Kind: kind, OpenedBy: "seed"}
	b.accts[id] = a
	b.byIBAN[iban] = a
}

// creditReq is a payment arriving over a rail. The sending bank names the
// beneficiary by IBAN, because that is all it has — it does not know this
// bank's internal account ids, and a real one would not.
type creditReq struct {
	IBAN string `json:"iban"`
	// AccountNumber and FinancialInstitution name the account where there is
	// no IBAN — a UK account number and sort code. The account is then
	// keyed "<institution> <number>", in the field an IBAN would be in:
	// it is the identifier the rail named, whichever shape it had.
	AccountNumber        string `json:"account_number"`
	FinancialInstitution string `json:"financial_institution"`
	Holder               string `json:"holder"`
	Amount               string `json:"amount"`
	Currency             string `json:"currency"`
	Reference            string `json:"reference"`
	Kind                 string `json:"kind"`
}

// credit implements POST /internal/credit: money arriving from outside.
//
// It auto-opens on first sight of an IBAN. That is what a beneficiary bank
// does, and it means no coordination is needed between whoever creates a
// merchant and this service — the account exists because a payout arrived,
// which is also the only moment it is true. The credit and the account are
// the environment's the sender names (Lab-Environment): Banking Circle
// says whose payout it was.
func (s *store) credit(w http.ResponseWriter, r *http.Request) {
	env, ok := s.scope(w, r, true)
	if !ok {
		return
	}
	var req creditReq
	if err := httputilx.ReadJSON(r, &req); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	if req.IBAN == "" && req.AccountNumber != "" {
		req.IBAN = strings.TrimSpace(req.FinancialInstitution + " " + req.AccountNumber)
	}
	if req.IBAN == "" {
		httputilx.Error(w, 400, "iban or account_number required: a rail names the beneficiary's account")
		return
	}
	cents, err := money.Parse(req.Amount)
	if err != nil {
		httputilx.Error(w, 400, "amount: "+err.Error())
		return
	}
	if cents <= 0 {
		httputilx.Error(w, 400, "amount must be positive")
		return
	}
	ccy := req.Currency
	if ccy == "" {
		ccy = "EUR"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.bookLocked(env)
	a, existed := b.byIBAN[req.IBAN]
	if !existed {
		kind := req.Kind
		if kind == "" {
			kind = kindMerchant
		}
		holder := req.Holder
		if holder == "" {
			holder = "Beneficiary " + req.IBAN
		}
		a = &account{
			ID: "acc_" + strings.ToLower(strings.ReplaceAll(req.IBAN, " ", "_")), IBAN: req.IBAN, Holder: holder,
			Currency: ccy, Kind: kind, OpenedBy: req.Reference, Env: envTag(env),
		}
		b.accts[a.ID] = a
		b.byIBAN[a.IBAN] = a
	}
	if a.Currency != ccy {
		httputilx.Error(w, 422, fmt.Sprintf("account %s is %s, credit is %s", a.IBAN, a.Currency, ccy))
		return
	}
	a.cents += cents
	a.Balance = money.Format(a.cents)
	b.seq++
	b.entries = append(b.entries, entry{
		ID: fmt.Sprintf("ent_%d", b.seq), AccountID: a.ID, Amount: money.Format(cents),
		Currency: ccy, Ref: req.Reference, At: labclock.Now().Format(time.RFC3339), Env: envTag(env),
	})
	httputilx.WriteJSON(w, 200, map[string]any{
		"account": a, "opened": !existed, "credited": money.Format(cents),
	})
}

// The two conversations worth watching at the far end of a payout: money
// arriving over the rail, and the ledger's own moves (make demo-payment's
// transfers, accounts opened by hand). The console draws a panel for each,
// and the Dashboard's last arrow counts the first.
func newLogs() (credits, ledger *activity.Log) {
	credits = activity.New("credits", "Payouts credited",
		"Every payout Banking Circle passed on, by IBAN, and the account it landed in. An account opens the first time money arrives for it.")
	ledger = activity.New("ledger", "Accounts and transfers",
		"Accounts opened and transfers between them — make demo-payment's ledger move, through payment-api.")
	return credits, ledger
}

// routes is the bank's HTTP surface, watched by its logs.
func routes(s *store, credits, ledger *activity.Log) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, map[string]string{"status": "ok", "service": "bank"})
	})
	mux.HandleFunc("GET /accounts", s.listAccounts)
	mux.HandleFunc("GET /accounts/{id}", s.getAccount)
	mux.HandleFunc("POST /accounts", ledger.Watch("account.open", summarizeOpen, s.createAccount))
	mux.HandleFunc("POST /transfers", ledger.Watch("transfer", summarizeTransfer, s.transfer))
	mux.HandleFunc("GET /ledger", s.listLedger)

	// The rail hop: a payout leaving the settlement bank has to arrive
	// somewhere, and this is where. No auth — it is a lab seam, and the
	// allowlist on the sending side is what governs who may call it.
	mux.HandleFunc("POST /internal/credit", credits.Watch("credit", summarizeCredit, s.credit))
	return mux
}

// simRoutes are the lab's hooks on the control port: the activity logs,
// and the accounts the lab API lists and opens. GET and POST /sim/accounts
// are GET and POST /accounts, the same handlers answering the same, so the
// lab never has to call the ledger's own API to steer the bank.
func simRoutes(mux *http.ServeMux, s *store, credits, ledger *activity.Log) {
	mux.HandleFunc("GET /sim/activity", activity.Handler(credits, ledger))
	mux.HandleFunc("GET /sim/accounts", s.listAccounts)
	mux.HandleFunc("POST /sim/accounts", ledger.Watch("account.open", summarizeOpen, s.createAccount))
}

// surfaces builds the bank's two listeners: its own API on addr, and the
// control surface on addr's port plus 1000 (internal/simctl), with every
// /sim route. The old /sim paths still answer on addr, passed through to
// the control surface, until phase 4 of docs/lab-api/plan.md retires them.
func surfaces(s *store, credits, ledger *activity.Log, addr string) (*http.ServeMux, *simctl.Control, error) {
	// A customer removed takes its accounts, its ledger and its activity
	// with it.
	s.customers.OnDelete = func(env string) error {
		s.drop(env)
		credits.ClearIn(env)
		ledger.ClearIn(env)
		return nil
	}
	mux := routes(s, credits, ledger)
	ctl, err := simctl.NewControl(control(s, credits, ledger, addr), simctl.ControlAddr(addr))
	if err != nil {
		return nil, nil, err
	}
	simRoutes(ctl.Mux, s, credits, ledger)
	mux.Handle("/sim/", ctl.Mux)
	return mux, ctl, nil
}

func main() {
	logring.Install()
	addr := env("LISTEN", ":8081")
	labclock.FollowEnv(context.Background(), "bank")
	credits, ledger := newLogs()
	mux, ctl, err := surfaces(newStore(), credits, ledger, addr)
	if err != nil {
		log.Fatalf("bank: %v", err)
	}
	ctl.Start()
	log.Printf("bank listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, logReq(mux)))
}

// control is the bank's part of the sim control contract. Both scopes do
// the same: nothing here is set up once by a platform — a platform never
// calls the bank — so "what a run made" is everything that was not seeded.
func control(s *store, credits, ledger *activity.Log, addr string) simctl.Config {
	return simctl.Config{
		Service: "bank", Kind: "vendor", Simulates: "the merchants' business bank",
		Listeners: []string{addr},
		Scopes:    []string{simctl.ScopeFlow, simctl.ScopeAll},
		Reset: func(_ context.Context, req simctl.ResetRequest) (simctl.ResetResult, error) {
			cleared := s.reset(req.Env)
			cleared["activity"] = int(credits.ClearIn(req.Env) + ledger.ClearIn(req.Env))
			return simctl.ResetResult{Cleared: cleared, Kept: []string{"seeded_accounts"}}, nil
		},
		Customers: s.customers,
	}
}

// reset puts scope's books (one environment's, or every one's for "*")
// back as they were on first start: every account that was not seeded goes
// (opened by a payout or by hand), the default's seeded ones are back at
// their seed balance, and the ledger is empty. It says how many accounts it
// closed, how many seeded balances it moved back, and how many credits and
// transfers the ledgers held.
func (s *store) reset(scope string) map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	opened, balances, credits, legs := 0, 0, 0, 0
	for env, b := range s.books {
		if !tenant.InScope(scope, env) {
			continue
		}
		seed := newBook()
		if env == tenant.Default {
			seed = seedBook()
		}
		for id, a := range b.accts {
			switch sa, seeded := seed.accts[id]; {
			case !seeded:
				opened++
			case a.cents != sa.cents:
				balances++
			}
		}
		for _, e := range b.entries {
			if strings.HasPrefix(e.ID, "ent_") {
				credits++
			} else {
				legs++
			}
		}
		s.books[env] = seed
	}
	return map[string]int{"accounts": opened, "balances": balances, "credits": credits, "transfers": legs / 2}
}

// drop forgets env's book.
func (s *store) drop(env string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.books, env)
}

// summarizeCredit: "credited 733.34 EUR to GB00SIM… (Acme Ltd) — account opened".
func summarizeCredit(c *activity.Call) (string, map[string]string) {
	detail := map[string]string{
		"iban":      c.JSONField("iban"),
		"reference": c.JSONField("reference"),
	}
	if c.Status >= 400 {
		return fmt.Sprintf("credit of %s %s to %s refused", c.JSONField("amount"), c.JSONField("currency"), c.JSONField("iban")), detail
	}
	holder := c.RespField("account", "holder")
	sum := fmt.Sprintf("credited %s %s to %s", c.RespField("credited"), c.RespField("account", "currency"), c.JSONField("iban"))
	if holder != "" {
		sum += " (" + holder + ")"
	}
	if c.RespField("opened") == "true" {
		sum += " — account opened"
		detail["opened"] = "true"
	}
	detail["balance"] = c.RespField("account", "balance")
	detail["account"] = c.RespField("account", "id")
	return sum, detail
}

func summarizeTransfer(c *activity.Call) (string, map[string]string) {
	to := c.JSONField("toIban")
	if to == "" {
		to = c.JSONField("toAccountId")
	}
	sum := fmt.Sprintf("transfer %s %s from %s to %s", c.JSONField("amount"), c.JSONField("currency"), c.JSONField("fromAccountId"), to)
	return sum, map[string]string{"payment": c.JSONField("paymentId"), "reference": c.JSONField("reference")}
}

func summarizeOpen(c *activity.Call) (string, map[string]string) {
	return fmt.Sprintf("opened %s for %s", c.RespField("iban"), c.JSONField("holder")),
		map[string]string{"account": c.RespField("id"), "currency": c.RespField("currency")}
}

func (s *store) listAccounts(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.scope(w, r, false)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []*account{}
	for _, b := range s.booksLocked(scope) {
		for _, a := range b.accts {
			out = append(out, a)
		}
	}
	httputilx.WriteJSON(w, 200, map[string]any{"accounts": out})
}

func (s *store) getAccount(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.scope(w, r, false)
	if !ok {
		return
	}
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.booksLocked(scope) {
		a, ok := b.accts[id]
		if !ok {
			a, ok = b.byIBAN[id]
		}
		if ok {
			httputilx.WriteJSON(w, 200, a)
			return
		}
	}
	httputilx.Error(w, 404, "account not found")
}

type createReq struct {
	Holder         string `json:"holder"`
	Currency       string `json:"currency"`
	OpeningBalance string `json:"openingBalance"`
}

func (s *store) createAccount(w http.ResponseWriter, r *http.Request) {
	env, ok := s.scope(w, r, true)
	if !ok {
		return
	}
	var req createReq
	if err := httputilx.ReadJSON(r, &req); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	if req.Holder == "" {
		httputilx.Error(w, 400, "holder required")
		return
	}
	if req.Currency == "" {
		req.Currency = "EUR"
	}
	cents := int64(0)
	if req.OpeningBalance != "" {
		var err error
		cents, err = money.Parse(req.OpeningBalance)
		if err != nil {
			httputilx.Error(w, 400, err.Error())
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.bookLocked(env)
	b.seq++
	iban := fakeIBAN(4 + b.seq)
	id := "acc_" + shortID()
	a := &account{ID: id, IBAN: iban, Holder: req.Holder, Currency: req.Currency, cents: cents, Balance: money.Format(cents), Env: envTag(env)}
	b.accts[id] = a
	b.byIBAN[iban] = a
	httputilx.WriteJSON(w, 201, a)
}

type transferReq struct {
	FromAccountID string `json:"fromAccountId"`
	ToAccountID   string `json:"toAccountId"`
	ToIBAN        string `json:"toIban"`
	Amount        string `json:"amount"`
	Currency      string `json:"currency"`
	Reference     string `json:"reference"`
	PaymentID     string `json:"paymentId"`
}

// transfer moves money between two accounts of the environment the call
// names.
func (s *store) transfer(w http.ResponseWriter, r *http.Request) {
	env, ok := s.scope(w, r, true)
	if !ok {
		return
	}
	var req transferReq
	if err := httputilx.ReadJSON(r, &req); err != nil {
		httputilx.Error(w, 400, err.Error())
		return
	}
	cents, err := money.Parse(req.Amount)
	if err != nil || cents <= 0 {
		httputilx.Error(w, 400, "amount must be a positive decimal")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.bookLocked(env)
	from, ok := b.accts[req.FromAccountID]
	if !ok {
		httputilx.Error(w, 404, "debtor account not found")
		return
	}
	var to *account
	if req.ToAccountID != "" {
		to = b.accts[req.ToAccountID]
	} else if req.ToIBAN != "" {
		to = b.byIBAN[req.ToIBAN]
	}
	if to == nil {
		httputilx.Error(w, 404, "creditor account not found")
		return
	}
	if from.ID == to.ID {
		httputilx.Error(w, 400, "cannot transfer to the same account")
		return
	}
	ccy := req.Currency
	if ccy == "" {
		ccy = from.Currency
	}
	if from.Currency != ccy || to.Currency != ccy {
		httputilx.Error(w, 409, "currency mismatch (sim is single-ccy per account)")
		return
	}
	if from.cents < cents {
		httputilx.Error(w, 409, "insufficient funds")
		return
	}
	from.cents -= cents
	to.cents += cents
	from.Balance = money.Format(from.cents)
	to.Balance = money.Format(to.cents)
	now := labclock.Now().Format(time.RFC3339)
	debit := entry{ID: "le_" + shortID(), AccountID: from.ID, PaymentID: req.PaymentID, Amount: money.Format(-cents), Currency: ccy, Ref: req.Reference, At: now, Env: envTag(env)}
	credit := entry{ID: "le_" + shortID(), AccountID: to.ID, PaymentID: req.PaymentID, Amount: money.Format(cents), Currency: ccy, Ref: req.Reference, At: now, Env: envTag(env)}
	b.entries = append(b.entries, debit, credit)
	httputilx.WriteJSON(w, 201, map[string]any{
		"from":    from,
		"to":      to,
		"entries": []entry{debit, credit},
	})
}

func (s *store) listLedger(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.scope(w, r, false)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []entry // null until the first entry, as it always was
	for _, b := range s.booksLocked(scope) {
		out = append(out, b.entries...)
	}
	httputilx.WriteJSON(w, 200, map[string]any{"entries": out})
}

func fakeIBAN(n int) string {
	return "GB00SIM" + strings.Repeat("0", 12-len(itoa(n))) + itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func shortID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func logReq(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
