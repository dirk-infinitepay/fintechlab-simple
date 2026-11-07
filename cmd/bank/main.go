// Bank is a tiny in-memory ledger with obviously fake IBANs (GB00SIM...).
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/httputilx"
	"github.com/webduvet/fintechlab-simple/internal/labclock"
	"github.com/webduvet/fintechlab-simple/internal/logring"
	"github.com/webduvet/fintechlab-simple/internal/money"
	"github.com/webduvet/fintechlab-simple/internal/simctl"
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
	cents    int64
}

type entry struct {
	ID        string `json:"id"`
	AccountID string `json:"accountId"`
	PaymentID string `json:"paymentId,omitempty"`
	Amount    string `json:"amount"`
	Currency  string `json:"currency"`
	Ref       string `json:"reference,omitempty"`
	At        string `json:"at"`
}

type store struct {
	mu      sync.Mutex
	accts   map[string]*account
	byIBAN  map[string]*account
	entries []entry
	seq     int
}

// newStore seeds the two accounts the settle path ends in, and nothing
// else that matters.
//
// The programme has one account per currency — that is where
// INFINITE_SETTLEMENT lands, and it is separate from every merchant's, so
// "did the platform get its share" is a balance rather than an inference.
// Merchant accounts are not seeded: a beneficiary bank does not know an
// IBAN until money arrives for it, and opening them on demand is both more
// honest and less coordination between this service and the console.
func newStore() *store {
	s := &store{accts: map[string]*account{}, byIBAN: map[string]*account{}}
	s.mustSeed("acc_infinite_eur", "GB00SIMINF00000001", "InfinitePay Ltd", "EUR", 0, kindInfinite)
	s.mustSeed("acc_infinite_gbp", "GB00SIMINF00000002", "InfinitePay Ltd", "GBP", 0, kindInfinite)

	// Two funded demo accounts, so `make demo-payment` and the payment-api
	// scenario have somewhere to move money from without a settlement run.
	s.mustSeed("acc_alice", "GB00SIM0000000000001", "Alice Simulator", "EUR", 1_000_000, kindDemo)
	s.mustSeed("acc_bob", "GB00SIM0000000000002", "Bob Simulator", "EUR", 50_000, kindDemo)
	s.mustSeed("acc_merchant", "GB00SIM0000000000003", "Merchant Simulator", "EUR", 0, kindDemo)
	return s
}

const (
	kindInfinite = "infinite"
	kindMerchant = "merchant"
	kindDemo     = "demo"
)

func (s *store) mustSeed(id, iban, holder, ccy string, cents int64, kind string) {
	a := &account{ID: id, IBAN: iban, Holder: holder, Currency: ccy, cents: cents,
		Balance: money.Format(cents), Kind: kind, OpenedBy: "seed"}
	s.accts[id] = a
	s.byIBAN[iban] = a
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
// which is also the only moment it is true.
func (s *store) credit(w http.ResponseWriter, r *http.Request) {
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
	a, existed := s.byIBAN[req.IBAN]
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
			Currency: ccy, Kind: kind, OpenedBy: req.Reference,
		}
		s.accts[a.ID] = a
		s.byIBAN[a.IBAN] = a
	}
	if a.Currency != ccy {
		httputilx.Error(w, 422, fmt.Sprintf("account %s is %s, credit is %s", a.IBAN, a.Currency, ccy))
		return
	}
	a.cents += cents
	a.Balance = money.Format(a.cents)
	s.seq++
	s.entries = append(s.entries, entry{
		ID: fmt.Sprintf("ent_%d", s.seq), AccountID: a.ID, Amount: money.Format(cents),
		Currency: ccy, Ref: req.Reference, At: labclock.Now().Format(time.RFC3339),
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
		Reset: func(context.Context, simctl.ResetRequest) (simctl.ResetResult, error) {
			cleared := s.reset()
			cleared["activity"] = int(credits.Clear() + ledger.Clear())
			return simctl.ResetResult{Cleared: cleared, Kept: []string{"seeded_accounts"}}, nil
		},
	}
}

// reset puts the ledger back as it was on first start: every account that
// was not seeded goes (opened by a payout or by hand), the seeded ones are
// back at their seed balance, and the ledger is empty. It says how many
// accounts it closed, how many seeded balances it moved back, and how many
// credits and transfers the ledger held.
func (s *store) reset() map[string]int {
	seed := newStore()
	s.mu.Lock()
	defer s.mu.Unlock()
	opened, balances := 0, 0
	for id, a := range s.accts {
		switch sa, seeded := seed.accts[id]; {
		case !seeded:
			opened++
		case a.cents != sa.cents:
			balances++
		}
	}
	credits, legs := 0, 0
	for _, e := range s.entries {
		if strings.HasPrefix(e.ID, "ent_") {
			credits++
		} else {
			legs++
		}
	}
	s.accts, s.byIBAN, s.entries, s.seq = seed.accts, seed.byIBAN, nil, 0
	return map[string]int{"accounts": opened, "balances": balances, "credits": credits, "transfers": legs / 2}
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
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*account, 0, len(s.accts))
	for _, a := range s.accts {
		out = append(out, a)
	}
	httputilx.WriteJSON(w, 200, map[string]any{"accounts": out})
}

func (s *store) getAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accts[id]
	if !ok {
		if x, ok2 := s.byIBAN[id]; ok2 {
			a, ok = x, true
		}
	}
	if !ok {
		httputilx.Error(w, 404, "account not found")
		return
	}
	httputilx.WriteJSON(w, 200, a)
}

type createReq struct {
	Holder         string `json:"holder"`
	Currency       string `json:"currency"`
	OpeningBalance string `json:"openingBalance"`
}

func (s *store) createAccount(w http.ResponseWriter, r *http.Request) {
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
	s.seq++
	iban := fakeIBAN(4 + s.seq)
	id := "acc_" + shortID()
	a := &account{ID: id, IBAN: iban, Holder: req.Holder, Currency: req.Currency, cents: cents, Balance: money.Format(cents)}
	s.accts[id] = a
	s.byIBAN[iban] = a
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

func (s *store) transfer(w http.ResponseWriter, r *http.Request) {
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
	from, ok := s.accts[req.FromAccountID]
	if !ok {
		httputilx.Error(w, 404, "debtor account not found")
		return
	}
	var to *account
	if req.ToAccountID != "" {
		to = s.accts[req.ToAccountID]
	} else if req.ToIBAN != "" {
		to = s.byIBAN[req.ToIBAN]
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
	debit := entry{ID: "le_" + shortID(), AccountID: from.ID, PaymentID: req.PaymentID, Amount: money.Format(-cents), Currency: ccy, Ref: req.Reference, At: now}
	credit := entry{ID: "le_" + shortID(), AccountID: to.ID, PaymentID: req.PaymentID, Amount: money.Format(cents), Currency: ccy, Ref: req.Reference, At: now}
	s.entries = append(s.entries, debit, credit)
	httputilx.WriteJSON(w, 201, map[string]any{
		"from":    from,
		"to":      to,
		"entries": []entry{debit, credit},
	})
}

func (s *store) listLedger(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	httputilx.WriteJSON(w, 200, map[string]any{"entries": s.entries})
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
