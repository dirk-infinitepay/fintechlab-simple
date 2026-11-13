// Package console backs the lab's control panel: the catalogue of
// simulated services, their live health, the sim banks, and the merchant
// registry a demo needs before any of the money flow means anything.
//
// It holds no vendor contract of its own. Everything real-shaped lives in
// the vendor services (worldline, b4b, banking-circle, aci); the console
// only reads and drives them over exactly the same HTTP surface a human
// with curl would use. Nothing here is on the critical path of a scenario,
// which is the point: if the console were the only way to make something
// happen, that thing would not be testable headlessly.
package console

import (
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Kind is which half of the lab a service belongs to. The distinction is
// the one docs/catalogue.md draws: a vendor simulation is the deliverable,
// scaffolding exists only so the e2e suite can prove a hop is connected.
type Kind string

const (
	KindVendor   Kind = "vendor"
	KindPlatform Kind = "platform"
	// KindVerification is the stand-in for the platform's own verification
	// service: the AML decision that gates a merchant before any money
	// exists. The vendors that service calls are vendors like any other.
	KindVerification Kind = "verification"
)

// Endpoint is one route worth showing a human. Not exhaustive -- the
// catalogue is a map, not a substitute for the OpenAPI the real vendors
// publish.
type Endpoint struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Note   string `json:"note,omitempty"`
}

// Service is one entry in the catalogue. BaseURL is where *this process*
// reaches it, which differs between a containerized console (compose
// service names) and one run from a shell (published 127.0.0.1 ports) --
// hence Browse, the URL that works from the operator's browser instead.
type Service struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    Kind   `json:"kind"`
	Summary string `json:"summary"`
	BaseURL string `json:"base_url"`
	Browse  string `json:"browse_url"`
	// Control is where the lab steers the service: its control port, every
	// /sim/* route and its own GET /health, plain HTTP with no vendor auth
	// (internal/simctl). Empty for a plugin, which has none.
	Control    string   `json:"control_url,omitempty"`
	HealthPath string   `json:"health_path"`
	Ports      []string `json:"ports"`
	Transport  string   `json:"transport"`
	Auth       string   `json:"auth"`
	SwapFor    string   `json:"swap_for"`
	Docs       string   `json:"docs,omitempty"`
	// Activity names the service's recent-history endpoint, when it keeps
	// one. Empty means the console draws no activity panel for it — which
	// is the honest rendering: "this service is not telling us" rather than
	// an empty list that reads as "nothing happened".
	Activity  string     `json:"activity_path,omitempty"`
	Endpoints []Endpoint `json:"endpoints,omitempty"`
	// Plugin is set on a platform that registered itself (plugins.go):
	// its buttons, its settlement files and how its registration stands.
	Plugin *PluginInfo `json:"plugin,omitempty"`
	// StandIn marks one of the lab's own services that plays the
	// platform's part until a platform plugs in (standins.go).
	StandIn *StandInInfo `json:"stand_in,omitempty"`
}

// Catalogue is the ordered list of the lab's own services, vendors first,
// and the platforms that registered themselves as plugins.
type Catalogue struct {
	Services []Service `json:"services"`

	mu      sync.RWMutex
	plugins []*registration
	now     func() time.Time // for tests; nil is time.Now

	standIns        map[string]*StandInInfo // live shown/hidden state, see standins.go
	appliedStandIns map[string]string       // plugin id -> the stand_ins last applied for it
}

// Get returns the service with the given id, the lab's own or a plugin.
func (c *Catalogue) Get(id string) (Service, bool) {
	for _, s := range c.Services {
		if s.ID == id {
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.overlayLocked(s), true
		}
	}
	for _, s := range c.Plugins() {
		if s.ID == id {
			return s, true
		}
	}
	return Service{}, false
}

// DefaultCatalogue describes the lab as compose runs it. Each BaseURL can
// be overridden with CONSOLE_URL_<ID>, upper-cased with dashes as
// underscores (CONSOLE_URL_BANKING_CIRCLE), which is what the compose file
// sets so the console reaches peers by service name.
//
// A single source of truth was tempting here -- generating this from
// compose.yml -- but compose is not always what is running (a service may
// be run from a shell, or pointed at a real vendor host), so the catalogue
// describes the *shape* and the environment says where it currently lives.
func DefaultCatalogue() *Catalogue {
	shown := standInsShown()
	svcs := []Service{
		{
			ID: "worldline", Name: "Worldline", Kind: KindVendor,
			Summary:    "The acquirer. Holds acquired card transactions per submerchant (MID), cuts the daily Bambora settlement file in two slots, publishes it PGP-encrypted over real SSH/SFTP, and wires the lump sum to the safeguarding account.",
			BaseURL:    "http://127.0.0.1:8084",
			HealthPath: "/health",
			Activity:   "/sim/activity",
			Ports:      []string{"8084/http", "2222/ssh"},
			Transport:  "HTTP + real SSH/SFTP with PGP",
			Auth:       "SFTP password or public key; optional host-key pinning",
			SwapFor:    "Point WORLDLINE_SFTP_HOST and the credentials at real Worldline.",
			Docs:       "docs/ARCHITECTURE-worldline-acquirer.md",
			Endpoints: []Endpoint{
				{"POST", "/sim/transactions", "seed acquired card transactions"},
				{"GET", "/sim/transactions", "what the acquirer holds"},
				{"POST", "/sim/settlement-cycle/run?slot=morning|afternoon", "cut and publish a file now"},
				{"GET", "/sim/settlement-cycle/runs", "cycle history"},
				{"GET", "/sim/enrolment", "enrolment batches seen and answered"},
				{"POST", "/sim/enrolment/process", "answer waiting enrolment batches now"},
				{"POST", "/sim/enrolment/reject", "arm a rejection for one submerchant on the next batch"},
				{"GET", "/sim/activity", "lab-only: SFTP and file activity"},
				{"GET|PUT", "/config", "SFT file-exchange channel config"},
				{"GET|POST", "/inbound/onboarding/{merchant_id}", "list / upload onboarding files"},
				{"GET|POST", "/inbound/corrections/{merchant_id}", "list / upload correction files"},
				{"GET", "/outbound/{merchant_id}/daily|financial", "list the files waiting"},
				{"GET", "/outbound/{merchant_id}/{category}/{filename}", "download one (archives it)"},
				{"GET", "/archive/{year}/{month}", "files already retrieved"},
			},
		},
		{
			ID: "b4b", Name: "B4B Payments", Kind: KindVendor,
			Summary:    "Oversight API, both halves: boarding a company (people, documents, the create-once extended profile, vIBANs) and paying it (beneficiary registration with pass/review/fail sanctions, the gates, the payout lifecycle and its callbacks, and the bridge into Banking Circle once approved).",
			BaseURL:    "http://127.0.0.1:8086",
			HealthPath: "/health",
			Activity:   "/sim/activity",
			Ports:      []string{"8086/http"},
			Transport:  "HTTP",
			Auth:       "Bearer RS512 JWT (aud b4b-payments)",
			SwapFor:    "Point B4B_URL at the real Oversight API.",
			Docs:       "docs/ARCHITECTURE-b4b-oversight.md",
			Endpoints: []Endpoint{
				{"POST", "/sim/traps", "arm a trap on payment.create: hang or fail a future payment (control port)"},
				{"GET", "/sim/traps/hooks", "what can be trapped: fields and the payment's lifecycle (control port)"},
				{"GET|POST", "/oversight/v1/companies", "list / board a company (422 on a repeated external_ref)"},
				{"GET", "/oversight/v1/companies/{id}", "by id or external_ref"},
				{"GET|POST", "/oversight/v1/companies/{id}/addresses", ""},
				{"GET|POST", "/oversight/v1/companies/{id}/people", "natural persons and legal entities"},
				{"GET", "/oversight/v1/companies/{id}/people/{personID}", ""},
				{"POST", "/oversight/v1/uploads", "documents: accepted, measured, discarded"},
				{"GET", "/oversight/v1/uploads/{id}", "an upload's record"},
				{"POST", "/oversight/v1/companies/{id}/extended", "the regulatory profile -- once, ever"},
				{"GET", "/oversight/v1/companies/{id}/extended[/{extendedID}]", "read this before re-posting one"},
				{"GET|POST", "/oversight/v1/companies/{id}/vibans", "stable per currency"},
				{"POST", "/oversight/v1/beneficiaries", "register a payee"},
				{"GET", "/oversight/v1/beneficiaries/{id}", ""},
				{"POST", "/oversight/v1/payments", "pay out (all gates apply)"},
				{"GET", "/oversight/v1/payments/{id}", "recovery read for a missed callback"},
				{"PUT", "/sim/beneficiaries/{id}/sanctions", "lab-only: move a sanctions status"},
				{"PUT", "/sim/beneficiaries/{id}/status", "lab-only: active/disabled"},
				{"PUT", "/sim/people/{id}/sanctions", "lab-only: sanctions and PEP move separately"},
				{"PUT", "/sim/companies/{id}/sanctions", "lab-only"},
				{"GET", "/sim/activity", "lab-only: payouts received, callbacks sent"},
			},
		},
		{
			ID: "banking-circle", Name: "Banking Circle", Kind: KindVendor,
			Summary:    "Connect API: the safeguarding-account ledger, the full notification self-service surface (subscriptions, If-Match, per-subscription keys, batching, event targets), the eleven-step retry schedule ending in auto-deactivation, and the reconciliation reads: the intraday report, the rejection report and a payment's status.",
			BaseURL:    "https://127.0.0.1:8085",
			HealthPath: "/health",
			Activity:   "/sim/activity",
			Ports:      []string{"8085/https+mtls", "8095/http (lab bridge)"},
			Transport:  "HTTPS, mTLS per BC_MTLS",
			Auth:       "Basic -> Bearer exchange",
			SwapFor:    "Point at the real Connect API.",
			Docs:       "docs/ARCHITECTURE-banking-circle-webhooks.md",
			Endpoints: []Endpoint{
				{"POST", "/sim/traps", "arm a trap on payment.outgoing: decide how a future payout ends (control port)"},
				{"GET", "/sim/traps/hooks", "what can be trapped: fields and the payout's state graph (control port)"},
				// The Connect API, as the real bank serves it (mTLS, Bearer).
				{"GET", "/api/v1/authorizations/authorize", "Basic -> Bearer"},
				{"GET", "/api/v1/accounts/{id}/balances", "safeguarding balance; financialDate is the lab clock's date"},
				{"GET|POST", "/api/v1/notificationselfservice/subscription", "list / create webhook subscriptions"},
				{"GET|PUT|DELETE", "/api/v1/notificationselfservice/subscription/{id}", "one subscription (PUT and DELETE need If-Match)"},
				{"PUT", "/api/v1/notificationselfservice/subscription/{id}/activate|deactivate", "switch a subscription on or off"},
				{"POST", "/api/v1/notificationselfservice/subscriptionEvent", "add an event type to a subscription"},
				{"PUT", "/api/v1/notificationselfservice/subscriptionEvent/{id}/targets", "scope an event to accounts"},
				{"DELETE", "/api/v1/notificationselfservice/subscriptionEvent/{id}", ""},
				{"POST", "/api/v1/notificationselfservice/clienttest/{id}", "flush a test notification"},
				{"GET", "/api/v1/reports/intraday-reconciliation-paged-report", "bookings for a business day; 6 required params, PropertiesIncluded"},
				{"GET", "/api/v1/reports/rejection-report", "rejected, missing-funding and pending payments; TransactionDate required"},
				{"GET", "/api/v1/payments/singles/{id}/status", "one payment's PaymentStatus; 404 if unknown"},
				// Lab-only, same listener and token.
				{"GET", "/sim/emails", "warning + deactivation emails"},
				{"GET", "/sim/delivery-config", "the retry schedule it is actually running"},
				{"GET", "/sim/subscription/{id}/pending", "retained and queued notifications"},
				{"POST", "/sim/subscription/{id}/pause|resume", "hold / release one subscription's deliveries"},
				{"POST", "/sim/subscription/{id}/notifications", "enqueue synthetic notifications"},
				{"POST", "/sim/payments/{id}/return", "the beneficiary's bank sends a processed payout back"},
				{"POST", "/sim/payments/{id}/reverse", "the scheme reverses a processed payout"},
				{"GET", "/sim/activity", "payments received and notifications sent"},
				{"GET", "/payments", "introspection: every payment the bank holds"},
				{"GET", "/accounts", "introspection: every account and its balance"},
				{"GET", "/reconciliation?date=YYYY-MM-DD[&account_id=]", "introspection: the payments of one day, optionally one account"},
				// Lab bridge, plain HTTP on :8095, no auth.
				{"POST", ":8095 /internal/payments", "B4B's hand-off: book a payout"},
				{"POST", ":8095 /internal/incoming-payments", "money in: a Worldline lump sum lands"},
				{"POST", ":8095 /internal/payments/{id}/return|reverse", "the same two test hooks as /sim/payments"},
				{"GET|POST", ":8095 /internal/payments/outcomes", "queue Rejected / Pending for the next payouts"},
				{"GET", ":8095 /internal/accounts/{id}/balances", "balances without mTLS"},
			},
		},
		{
			ID: "aci", Name: "ACI", Kind: KindVendor,
			Summary:    "Online card-payment gateway. A sender, not a callee: it emits an AES-256-GCM encrypted webhook that stands in for \"a card payment just happened\".",
			BaseURL:    "http://127.0.0.1:8087",
			HealthPath: "/health",
			Ports:      []string{"8087/http"},
			Transport:  "HTTP out (webhook)",
			Auth:       "shared webhook secret (hex, 32 raw bytes)",
			SwapFor:    "It is the sender -- point ACI_WEBHOOK_TARGET_URL at your own listener.",
			Endpoints: []Endpoint{
				{"POST", "/sim/payments", "emit a card-payment webhook (control port)"},
				{"GET", "/sim/sent", "what it delivered (control port)"},
				{"GET", "/sim/sent/{id}", "one delivery (control port)"},
				{"GET|POST", "/sim/connected", "lab switch: whether it delivers webhooks at all"},
			},
		},
		{
			// The far end of every payout. Banking Circle credits a
			// processed payout here by IBAN (BANK_CREDIT_URL), and the
			// account opens on first sight, as a beneficiary bank's would.
			ID: "bank", Name: "Business bank", Kind: KindVendor,
			Summary:    "The merchants' own bank, where a payout lands: Banking Circle credits every processed payout here by IBAN, and the outlet's account opens the first time money arrives for it. Also the in-memory ledger make demo-payment moves money in. Fake GB00SIM… IBANs throughout.",
			BaseURL:    "http://127.0.0.1:8081",
			HealthPath: "/health",
			Activity:   "/sim/activity",
			Ports:      []string{"8081/http"},
			Transport:  "HTTP",
			Auth:       "none (lab rail; Banking Circle's allowlist governs who may credit it)",
			SwapFor:    "Nothing to swap: a merchant's real bank is not something a platform integrates with. Banking Circle stops crediting it when BANK_CREDIT_URL is empty.",
			Endpoints: []Endpoint{
				{"POST", "/internal/credit", "a payout arriving over the rail, by IBAN; opens the account on first sight"},
				{"GET|POST", "/accounts", "accounts, and open one"},
				{"GET", "/accounts/{id}", "by id or IBAN"},
				{"POST", "/transfers", "move money between two accounts (payment-api's call)"},
				{"GET", "/ledger", "every entry"},
				{"GET", "/sim/activity", "credits received, transfers"},
			},
		},
		{
			ID: "verify", Name: "AML decision", Kind: KindVerification,
			Summary:    "Stub verification/compliance service gating a payout. Always approves unless an id is force-declined.",
			BaseURL:    "http://127.0.0.1:8088",
			HealthPath: "/api/v1/verification/health",
			Ports:      []string{"8088/http"},
			Transport:  "HTTP",
			Auth:       "x-internal-api-key",
			SwapFor:    "A real verification vendor.",
			Endpoints: []Endpoint{
				{"POST", "/api/v1/verification/verify/aml-decision", "trigger"},
				{"GET", "/api/v1/verification/verify/data/verify-decision/{id}", "read the decision"},
			},
		},
		{
			ID: "creditsafe", Name: "Creditsafe", Kind: KindVendor,
			Summary:    "UK bank-account verification, company search and reports, and monitoring portfolios. Every check passes by default; bank-account and company outcomes can be flipped, and a test registers a company so its report carries what the platform compares.",
			BaseURL:    "http://127.0.0.1:8091",
			HealthPath: "/health",
			Activity:   "/sim/activity",
			Ports:      []string{"8091/http"},
			Transport:  "HTTP",
			Auth:       "username/password, then bearer",
			SwapFor:    "Point CREDITSAFE_API_URL at Creditsafe Connect.",
			Endpoints: []Endpoint{
				{"POST", "/creditsafe/authenticate", "username/password -> token"},
				{"POST", "/creditsafe/localSolutions/GB/bankVerification/search", "UK bank account + name match"},
				{"GET", "/creditsafe/companies", "company search"},
				{"GET", "/creditsafe/companies/{id}", "company report"},
				{"GET", "/creditsafe/monitoring/portfolios", "monitoring portfolios"},
				{"GET", "/creditsafe/monitoring/notificationEvents", "monitoring poll (nothing to report)"},
				{"GET", "/sim/outcome", "what each check currently answers"},
				{"POST", "/sim/outcome", "lab-only: flip one check to a failure"},
				{"POST", "/sim/creditsafe/companies", "lab-only: register a company for its report"},
			},
		},
		{
			ID: "kyc6", Name: "KYC6", Kind: KindVendor,
			Summary:    "Sanctions, PEP and adverse-media screening, profile lookups and monitoring worklists. Every screen is clear by default; the screening outcome flips to a current-sanctions hit.",
			BaseURL:    "http://127.0.0.1:8092",
			HealthPath: "/health",
			Activity:   "/sim/activity",
			Ports:      []string{"8092/http"},
			Transport:  "HTTP",
			Auth:       "x-api-key",
			SwapFor:    "Point KYC6_API_BASE_URL and KYC6_MONITOR_API_BASE_URL at KYC6.",
			Endpoints: []Endpoint{
				{"POST", "/kyc6/individuals", "screen a person"},
				{"POST", "/kyc6/businesses", "screen a business"},
				{"GET", "/kyc6/businesses/{resourceId}", "profile lookup"},
				{"GET", "/kyc6/lifecycle/ongoing-monitoring/{alertId}", "ongoing-monitoring alert"},
				{"GET", "/sim/outcome", "what each check currently answers"},
				{"POST", "/sim/outcome", "lab-only: flip one check to a failure"},
			},
		},
		{
			ID: "lexisnexis", Name: "LexisNexis", Kind: KindVendor,
			Summary:    "Identity: OAuth, then IDU identity reports and IVI attribute queries. Every person passes by default; the identity outcome flips to refer or fail.",
			BaseURL:    "http://127.0.0.1:8093",
			HealthPath: "/health",
			Activity:   "/sim/activity",
			Ports:      []string{"8093/http"},
			Transport:  "HTTP",
			Auth:       "OAuth client credentials, then bearer (IDU); key in the body (IVI)",
			SwapFor:    "Point the LEXISNEXIS_* URLs at LexisNexis.",
			Endpoints: []Endpoint{
				{"POST", "/lexisnexis/oauth/token", "OAuth"},
				{"POST", "/lexisnexis/idu/v1/reports", "IDU: identity report (bearer)"},
				{"POST", "/lexisnexis/ivi/api/attribute-query", "IVI: identity verification attribute query"},
				{"GET", "/sim/outcome", "what each check currently answers"},
				{"POST", "/sim/outcome", "lab-only: flip one check to a failure"},
			},
		},
		{
			ID: "iban-com", Name: "iban.com", Kind: KindVendor,
			Summary:    "Bank-account validity and name match for accounts outside the UK. Every account is valid with a matching name by default; the bank-account outcome flips to a mismatch or an invalid account.",
			BaseURL:    "http://127.0.0.1:8094",
			HealthPath: "/health",
			Activity:   "/sim/activity",
			Ports:      []string{"8094/http"},
			Transport:  "HTTP",
			Auth:       "x-api-key",
			SwapFor:    "Point IBAN_COM_BASE_URL at iban.com.",
			Endpoints: []Endpoint{
				{"POST", "/iban/verify", "IBAN validity and name match"},
				{"GET", "/sim/outcome", "what each check currently answers"},
				{"POST", "/sim/outcome", "lab-only: flip one check to a failure"},
			},
		},
		{
			ID: "clock", Name: "Lab clock", Kind: KindPlatform,
			Summary:    "The lab's business clock: one offset from the real time that every vendor follows, and that a registered platform follows too. Put the lab on a Sunday, then on Monday with the same money.",
			BaseURL:    "http://127.0.0.1:8096",
			HealthPath: "/health",
			Activity:   "/sim/activity",
			Ports:      []string{"8096/http"},
			Transport:  "HTTP",
			Auth:       "none (lab)",
			SwapFor:    "Nothing — real vendors are on the real clock. A platform deployed where its clock cannot be moved registers with clock \"wall\", and the lab then stays on the real time too.",
			Docs:       "docs/plugins.md",
			Endpoints: []Endpoint{
				{"GET", "/clock", "the clock as it stands; offset_ms is what a follower reads"},
				{"POST", "/clock", `move it: {"at": "<RFC 3339>"}, {"advance": "1d"}, {"mode": "real"} or {"mode": "auto-business-day"}`},
				{"POST", "/clock/holds", `{"holder", "reason", "ttl_seconds"} — refuse moves while a run is in flight`},
				{"DELETE", "/clock/holds/{holder}", "release it"},
				{"GET", "/sim/activity", "every move and hold"},
			},
		},
		{
			ID: "settlement", Name: "Settlement (platform stand-in)", Kind: KindPlatform,
			StandIn: &StandInInfo{
				Shown:     shown,
				Plays:     "your settle pipeline: pulls Worldline's file on a timer and pays each outlet through B4B at the daily cutoff. Wanted only when no platform is plugged in — otherwise its payouts at B4B are payouts nobody made.",
				StartsOff: "SETTLEMENT_CONNECTED=false on the settlement service",
			},
			Summary:    "Scaffolding for your platform: pulls Worldline's file over real SFTP+PGP, decrypts, parses, splits it per MID and pays each outlet through B4B.",
			BaseURL:    "http://127.0.0.1:8083",
			HealthPath: "/health",
			Ports:      []string{"8083/http"},
			Transport:  "HTTP",
			Auth:       "none (lab)",
			SwapFor:    "Replace it with your own platform.",
			Endpoints: []Endpoint{
				{"GET", "/worldline/files", "what it has taken delivery of"},
				{"POST", "/worldline/pull", "pull now"},
				{"GET", "/reports/settlement", ""},
				{"POST", "/reports/settlement/generate", "build reports for a date range (default yesterday)"},
				{"GET", "/reports/settlement/{id}", ""},
				{"GET", "/reports/settlement/{id}/payout", "the payout it submitted to B4B"},
				{"POST", "/reports/settlement/{id}/transition", "move the settlement state machine"},
				{"POST", "/reports/settlement/{id}/retry-payout", "re-submit a payout stuck in a gate"},
				{"POST", "/internal/b4b-webhook", "B4B's payment-status callback"},
				{"GET", "/sftp/out[/{merchant_id}[/{filename}]]", "report files written for SFTP"},
				{"GET", "/sftp/staging", "files staged for delivery"},
				{"GET|POST", "/sim/connected", "lab switch: whether it pulls files"},
			},
		},
		{
			ID: "receiver", Name: "Webhook receiver (platform stand-in)", Kind: KindPlatform,
			StandIn: &StandInInfo{
				Shown:     shown,
				Plays:     "your webhook listener: Banking Circle's seeded subscription and ACI deliver here. Wanted only when nothing of yours subscribes — otherwise it is a second listener taking the same batches.",
				StartsOff: "ACI_WEBHOOK_CONNECTED=false on aci; the Banking Circle subscription is switched live through the bank's own API",
			},
			Summary:    "Stand-in for your own listener: verifies the HMAC scheme, and captures raw bodies for wire formats it holds no key for.",
			BaseURL:    "https://127.0.0.1:8443",
			HealthPath: "/health",
			Ports:      []string{"8443/https"},
			Transport:  "HTTPS",
			Auth:       "HMAC on /webhooks; none on /raw-events",
			SwapFor:    "Point the vendors' webhook URLs at your own listener.",
			Endpoints: []Endpoint{
				{"POST", "/webhooks", "HMAC-signed webhooks: verified, then stored"},
				{"GET", "/events", "verified webhooks"},
				{"POST", "/raw-events", "capture any body unverified (Banking Circle, ACI)"},
				{"GET", "/raw-events", "raw captures (base64 bodies)"},
			},
		},
		{
			// Not on the settle path: payment-api, the business bank's
			// transfers and the notifier make the lab's oldest scenario,
			// `make demo-payment` — payment, ledger move, signed webhook,
			// stored event — which the e2e suite still runs.
			ID: "payment-api", Name: "Payment API (demo stand-in)", Kind: KindPlatform,
			StandIn: &StandInInfo{
				Shown: shown,
				Plays: "a payment facade of the platform's own: POST /payments with an Idempotency-Key moves money between two accounts at the business bank and enqueues a signed webhook at the notifier. Nothing in the settle path calls it; make demo-payment and the e2e suite's first scenario do.",
			},
			Summary: "Generic payment facade with Idempotency-Key: one POST /payments moves money at the business bank and enqueues a signed webhook at the notifier. Only make demo-payment and the e2e suite call it — the settle path never does.",
			BaseURL: "http://127.0.0.1:8080", HealthPath: "/health", Activity: "/sim/activity",
			Ports: []string{"8080/http"}, Transport: "HTTP", Auth: "none (lab)",
			SwapFor: "-- generic shape, not a vendor.",
			Endpoints: []Endpoint{
				{"POST", "/payments", "Idempotency-Key header; moves the ledger, then enqueues the webhook"},
				{"GET", "/payments", ""}, {"GET", "/payments/{id}", ""},
				{"GET", "/sim/activity", "payments received"},
			},
		},
		{
			ID: "notifier", Name: "Notifier (demo stand-in)", Kind: KindPlatform,
			StandIn: &StandInInfo{
				Shown: shown,
				Plays: "a webhook worker of the platform's own: HMAC-signs what payment-api enqueues and delivers it to the receiver, with retries and a destination allowlist. Nothing in the settle path calls it; make demo-payment does.",
			},
			Summary: "Signed webhook worker: HMAC-signs what payment-api enqueues and POSTs it to the receiver, with retries and an allowlist. Only make demo-payment and the e2e suite call it — the settle path never does.",
			BaseURL: "http://127.0.0.1:8082", HealthPath: "/health", Activity: "/sim/activity",
			Ports: []string{"8082/http"}, Transport: "HTTP", Auth: "HMAC out",
			SwapFor: "-- generic shape, not a vendor.",
			Endpoints: []Endpoint{
				{"POST", "/enqueue", "what payment-api calls"},
				{"GET|POST", "/subscriptions", "read / replace the subscription (POST clears a failed one)"}, {"GET", "/deliveries", ""},
				{"GET", "/sim/activity", "deliveries and their attempts"},
			},
		},
	}
	// CONSOLE_URL_<ID>=off: this deployment does not run that service at
	// all -- the cloud profile runs no clock (docs/deploy-cloud.md). It is
	// left out rather than listed as down: a card that is red forever says
	// something is broken, and nothing is.
	kept := svcs[:0]
	for _, s := range svcs {
		v := os.Getenv(envKeyFor(s.ID))
		if v == ServiceOff {
			continue
		}
		if v != "" {
			s.BaseURL = v
		}
		s.Browse = browseURL(s)
		s.Control = os.Getenv(controlEnvKeyFor(s.ID))
		if s.Control == "" {
			s.Control = ControlURL(s.BaseURL)
		}
		kept = append(kept, s)
	}
	return &Catalogue{Services: kept}
}

// ServiceOff, as a CONSOLE_URL_<ID> value, leaves that service out of the
// catalogue: the deployment does not run it.
const ServiceOff = "off"

// controlEnvKeyFor is CONSOLE_CONTROL_URL_<ID>, the override for a
// service's control address.
func controlEnvKeyFor(id string) string {
	return "CONSOLE_CONTROL_URL_" + strings.ToUpper(strings.ReplaceAll(id, "-", "_"))
}

// ControlURL is a service's control address by the lab's convention: the
// same host, plain HTTP, and the port plus 1000 (https://banking-circle:8085
// → http://banking-circle:9085). "" when the base URL has no port to add to.
func ControlURL(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return ""
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port <= 0 || port+1000 > 65535 {
		return ""
	}
	return "http://" + net.JoinHostPort(u.Hostname(), strconv.Itoa(port+1000))
}

// envKeyFor is the per-service base-URL override, e.g. banking-circle ->
// CONSOLE_URL_BANKING_CIRCLE.
func envKeyFor(id string) string {
	return "CONSOLE_URL_" + strings.ToUpper(strings.ReplaceAll(id, "-", "_"))
}

// browseURL is the link handed to the operator's browser. Inside compose
// the console reaches peers as http://bank:8081, which means nothing on
// the host, so the published port is reconstructed from the catalogue's
// own port list. CONSOLE_BROWSE_HOST names the host those ports are
// published on (127.0.0.1 unless the lab runs somewhere else).
func browseURL(s Service) string {
	host := os.Getenv("CONSOLE_BROWSE_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	scheme := "http"
	port := ""
	for _, p := range s.Ports {
		name, kind, _ := strings.Cut(p, "/")
		if strings.HasPrefix(kind, "http") {
			port = name
			if strings.HasPrefix(kind, "https") {
				scheme = "https"
			}
			break
		}
	}
	if port == "" {
		return ""
	}
	return scheme + "://" + host + ":" + port
}
