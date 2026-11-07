package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
)

// The lab API, described once.
//
// Every route below is registered by routes() and listed by GET /v1, so the
// index cannot name a route that does not exist, and a route cannot exist
// without a line saying what it is for and which service it reaches. The
// long form, with bodies and answers, is docs/api/lab-api.md; a test holds
// that document to exactly this table.
//
// The lab API stays a client of the services (docs/api/README.md): most
// routes are one call to one service's own API, and Reaches names it, so a
// script or an agent can tell when it could have gone to the service itself.

// route is one lab API route.
type route struct {
	// Method is an HTTP method, or ANY for a route that takes every one
	// (the passthrough).
	Method string `json:"method"`
	Path   string `json:"path"`
	// Alias is the console's old path for the same route, served until
	// every plugin registers at /v1 (docs/lab-api/plan.md, phase 4).
	Alias   string `json:"alias,omitempty"`
	Group   string `json:"group"`
	Summary string `json:"summary"`
	// Reaches is the service and endpoint the route calls, or what it reads
	// instead ("the lab API's own state").
	Reaches string `json:"reaches"`

	handler http.HandlerFunc
}

// patterns is what the mux registers for a route: its path and its alias,
// each with its method, or bare for ANY.
func (rt route) patterns() []string {
	var out []string
	for _, p := range []string{rt.Path, rt.Alias} {
		if p == "" {
			continue
		}
		if rt.Method == "ANY" {
			out = append(out, p)
		} else {
			out = append(out, rt.Method+" "+p)
		}
	}
	return out
}

// routeGroup orders the index and the Docs view's API page.
type routeGroup struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Note  string `json:"note"`
}

var apiGroups = []routeGroup{
	{"lab", "Lab", "What is running and what it has done: the index, the run diagram, the stand-ins."},
	{"services", "Services", "Each service of the lab: its record and health, its logs, and its /sim/* hooks through one address."},
	{"clock", "Clock", "The lab's business clock, fronted from the clock service."},
	{"config", "Configuration", "What the lab is set to, and the one setting the lab API can change."},
	{"merchants", "Merchants", "The lab's merchant registry, and putting its merchants on the vendors."},
	{"banks", "Banks", "Accounts and payouts at the business bank and Banking Circle."},
	{"actions", "Vendor actions", "Make a vendor do now what it would otherwise do on its schedule."},
	{"plugins", "Plugins", "The platform under test: registration, its buttons and its settlement files."},
	{"connect", "Connect", "What a platform needs to point at this lab: addresses, credentials, key files."},
}

// apiDoc is where the long form of this table lives, relative to the repo.
const apiDoc = "docs/api/lab-api.md"

// apiRoutes is the table. GET /v1's handler is left nil: it describes the
// table, so routes() wires it where the table is known.
func (a *app) apiRoutes() []route {
	return []route{
		{"GET", "/health", "", "lab", "Whether the lab API process is up", "nothing — the lab API's own state", a.health},
		{"GET", "/v1", "/api", "lab", "This index: every route, its alias, its group and what it reaches", "nothing — the lab API's own state", nil},
		{"GET", "/v1/lab", "/api/overview", "lab", "Every service and registered plugin with its last health probe", "each service's health_path, probed in the background", a.overview},
		{"GET", "/v1/flow", "/api/flow", "lab", "The settlement run as a sequence: participants, hops, counts, report", "each vendor's GET /sim/activity, the plugin's status_path", a.flow},
		{"GET", "/v1/flow/hops/{id}", "/api/flow/hops/{id}", "lab", "The events behind one arrow of the run diagram", "same reads as GET /v1/flow", a.flowHop},
		{"POST", "/v1/lab/reset", "", "lab", "Reset every service in order, producers first and the clock last", "every service's POST /sim/reset; the merchant registry on all", a.labReset},
		{"GET", "/v1/logs", "", "lab", "Every service's log lines, merged by time, behind one cursor", "every service's GET /sim/logs", a.labLogs},
		{"GET", "/v1/stand-ins", "/api/stand-ins", "lab", "The stand-ins: shown or hidden, and how each is wired, read live", "settlement+aci /sim/connected, banking-circle subscriptions", a.listStandIns},
		{"POST", "/v1/stand-ins/{id}", "/api/stand-ins/{id}", "lab", "Show, hide, connect or disconnect one stand-in", "settlement+aci POST /sim/connected; BC (de)activate", a.setStandIn},

		{"GET", "/v1/services", "", "services", "Every service: its catalogue entry and its last health probe", "each service's health_path, probed in the background", a.listServices},
		{"GET", "/v1/services/{id}", "", "services", "One service: its catalogue entry and its last health probe", "the service's health_path, probed in the background", a.getService},
		{"POST", "/v1/services/{id}/probe", "/api/services/{id}/probe", "services", "Probe one service's health now instead of waiting for the monitor", "the service's GET health_path", a.probeService},
		{"GET", "/v1/services/{id}/activity", "/api/services/{id}/activity", "services", "One service's activity logs, as the service reports them", "the service's GET /sim/activity (or plugin activity_path)", a.serviceActivity},
		{"GET", "/v1/services/{id}/logs", "", "services", "One service's own log lines", "the service's GET /sim/logs", a.serviceLogs},
		{"POST", "/v1/services/{id}/reset", "", "services", "Reset one service: flow or all", "the service's POST /sim/reset", a.serviceReset},
		{"ANY", "/v1/services/{id}/sim/{path...}", "", "services", "Any of a service's /sim/* lab hooks, with the credentials it needs", "the service's /sim/{path}: method, query and body as sent", a.simPassthrough},

		{"GET", "/v1/clock", "/api/clock", "clock", "The lab clock as it stands", "clock GET /clock", a.labClock},
		{"POST", "/v1/clock", "/api/clock", "clock", "Move the lab clock, or change its mode", "clock POST /clock", a.setLabClock},
		{"POST", "/v1/clock/holds", "", "clock", "Hold the lab clock while a run is in flight: moves are refused", "clock POST /clock/holds", a.clockHold},
		{"DELETE", "/v1/clock/holds/{holder}", "", "clock", "Release a hold on the lab clock", "clock DELETE /clock/holds/{holder}", a.clockRelease},

		{"GET", "/v1/config", "/api/config", "config", "The lab's settings: env knobs, BC delivery table, Worldline channel", "banking-circle /sim/delivery-config, worldline GET /config", a.getConfig},
		{"PUT", "/v1/config/worldline-channel", "/api/config/worldline-channel", "config", "Change Worldline's SFT file-exchange channel config", "worldline PUT /config", a.putWorldlineChannel},

		{"GET", "/v1/merchants", "/api/merchants", "merchants", "The merchant registry: distributors, partners, merchants, outlets, batches", "nothing — the lab API's own state", a.listMerchants},
		{"POST", "/v1/merchants", "/api/merchants", "merchants", "Create a merchant and its outlets in the registry", "nothing — the lab API's own state", a.createMerchant},
		{"DELETE", "/v1/merchants/{id}", "/api/merchants/{id}", "merchants", "Delete a merchant from the registry; vendors keep what they had", "nothing — the lab API's own state", a.deleteMerchant},
		{"POST", "/v1/merchants/batches", "/api/merchants/batches", "merchants", "Generate a batch of merchants with outlets from a seed", "nothing — the lab API's own state", a.createBatch},
		{"DELETE", "/v1/merchants/batches/{id}", "/api/merchants/batches/{id}", "merchants", "Delete a batch and its merchants from the registry", "nothing — the lab API's own state", a.deleteBatch},
		{"POST", "/v1/merchants/batches/{id}/provision", "/api/merchants/batches/{id}/provision", "merchants", "Register every outlet of a batch as a B4B beneficiary", "b4b POST /oversight/v1/beneficiaries, per outlet", a.provisionBatch},
		{"POST", "/v1/merchants/batches/{id}/trading", "/api/merchants/batches/{id}/trading", "merchants", "Seed card payments at Worldline for every outlet of a batch", "worldline POST /sim/transactions, per merchant", a.tradeBatch},
		{"POST", "/v1/merchants/{id}/outlets", "/api/merchants/{id}/outlets", "merchants", "Open another outlet (a new MID) for a merchant", "nothing — the lab API's own state", a.addOutlet},
		{"POST", "/v1/merchants/{id}/status", "/api/merchants/{id}/status", "merchants", "Set a merchant's status: draft, active or suspended", "nothing — the lab API's own state", a.setMerchantStatus},
		{"POST", "/v1/merchants/{id}/provision", "/api/merchants/{id}/provision", "merchants", "Register a merchant's outlets as B4B beneficiaries", "b4b POST /oversight/v1/beneficiaries, per outlet", a.provisionMerchant},
		{"POST", "/v1/merchants/{id}/trading", "/api/merchants/{id}/trading", "merchants", "Seed card payments at Worldline for a merchant's outlets", "worldline POST /sim/transactions", a.seedTrading},
		{"POST", "/v1/outlets/{mid}/sanctions", "/api/outlets/{mid}/sanctions", "merchants", "Set an outlet's sanctions status at B4B: pass, review or fail", "b4b PUT /sim/beneficiaries/{mid}/sanctions", a.setSanctions},
		{"POST", "/v1/partners", "/api/partners", "merchants", "Add a partner under a distributor", "nothing — the lab API's own state", a.createPartner},

		{"GET", "/v1/banks", "/api/banks", "banks", "Both banks with their accounts and balances", "bank GET /accounts; banking-circle GET /accounts", a.listBanks},
		{"POST", "/v1/banks/{id}/accounts", "/api/banks/{id}/accounts", "banks", "Open an account at the business bank (Banking Circle: 501)", "bank POST /accounts", a.openAccount},
		{"GET", "/v1/banking-circle/subscriptions", "/api/banking-circle/subscriptions", "banks", "Banking Circle's notification subscriptions, with what is waiting", "banking-circle subscription list and /sim pending reads", a.bcSubscriptions},
		{"POST", "/v1/banking-circle/subscriptions/{id}/test", "/api/banking-circle/subscriptions/{id}/test", "banks", "Send a subscription a test notification and report what it did", "banking-circle POST clienttest/{id}, GET /sim/activity", a.bcSubscriptionTest},
		{"POST", "/v1/banking-circle/subscriptions/{id}/pause", "/api/banking-circle/subscriptions/{id}/pause", "banks", "Hold a subscription's deliveries", "banking-circle POST /sim/subscription/{id}/pause", a.bcSubscriptionPause},
		{"POST", "/v1/banking-circle/subscriptions/{id}/resume", "/api/banking-circle/subscriptions/{id}/resume", "banks", "Release a paused subscription's deliveries", "banking-circle POST /sim/subscription/{id}/resume", a.bcSubscriptionPause},
		{"GET", "/v1/banking-circle/payouts", "/api/banking-circle/payouts", "banks", "Banking Circle's outgoing payouts, and which can be returned or reversed", "banking-circle GET /payments", a.bcPayouts},
		{"POST", "/v1/banking-circle/payouts/{id}/return", "/api/banking-circle/payouts/{id}/return", "banks", "Return a processed payout, as the beneficiary's bank would", "banking-circle POST /sim/payments/{id}/return", a.bcPayoutAction},
		{"POST", "/v1/banking-circle/payouts/{id}/reverse", "/api/banking-circle/payouts/{id}/reverse", "banks", "Reverse a processed payout, as the scheme would", "banking-circle POST /sim/payments/{id}/reverse", a.bcPayoutAction},
		{"POST", "/v1/actions/fund-sga", "/api/actions/fund-sga", "banks", "Fund the empty safeguarding accounts, as Worldline's wire would", "banking-circle bridge GET balances, POST incoming-payments", a.fundSGA},

		{"POST", "/v1/actions/worldline-cycle", "/api/actions/worldline-cycle", "actions", "Cut and publish Worldline's settlement files now", "worldline POST /sim/settlement-cycle/run", a.runWorldlineCycle},
		{"POST", "/v1/actions/settlement-pull", "/api/actions/settlement-pull", "actions", "Make the settlement stand-in pull Worldline's SFTP now", "settlement POST /worldline/pull", a.settlementPull},

		{"GET", "/v1/plugins", "/api/plugins", "plugins", "The registered platforms and how each registration stands", "nothing — the lab API's own state", a.listPlugins},
		{"POST", "/v1/plugins", "/api/plugins", "plugins", "Register or renew a platform's card (docs/plugins.md)", "the lab API's own state; then health_path and stand-in wiring", a.registerPlugin},
		{"DELETE", "/v1/plugins/{id}", "/api/plugins/{id}", "plugins", "Unregister a platform on a clean shutdown", "nothing — the lab API's own state", a.unregisterPlugin},
		{"POST", "/v1/plugins/{id}/actions/{action}", "/api/plugins/{id}/actions/{action}", "plugins", "Press one of a platform's declared buttons", "the plugin's base_url + the action's path", a.pluginAction},
		{"GET", "/v1/plugins/{id}/files", "/api/plugins/{id}/files", "plugins", "A settling platform's settlement files", "the plugin's base_url + files_path", a.pluginFiles},
		{"POST", "/v1/plugins/{id}/run", "/api/plugins/{id}/run", "plugins", "Run settlement files on the platform", "the plugin's base_url + run_path", a.pluginRun},
		{"POST", "/v1/plugins/{id}/files", "/api/plugins/{id}/files", "plugins", "Upload a settlement file to the platform", "the plugin's base_url + upload_path", a.pluginUpload},
		{"DELETE", "/v1/plugins/{id}/files", "/api/plugins/{id}/files", "plugins", "Delete a settlement file on the platform", "the plugin's base_url + delete_path (or upload_path)", a.pluginDeleteFile},
		{"GET", "/v1/plugins/{id}/files/preview", "/api/plugins/{id}/files/preview", "plugins", "The first lines of one settlement file", "the plugin's base_url + preview_path", a.pluginPreview},
		{"POST", "/v1/plugins/{id}/files/reveal", "/api/plugins/{id}/files/reveal", "plugins", "Open a settlement file's folder on the platform's machine", "the plugin's base_url + reveal_path", a.pluginReveal},

		{"GET", "/v1/connect", "/api/connect", "connect", "What each vendor asks of a platform that connects: names, no values", "nothing — the lab API's own state and the keys directory", a.connect},
		{"GET", "/v1/connect/env", "/api/connect/env", "connect", "Download fintechlab.env: every vendor's variables with values", "nothing — the lab API's own state and the keys directory", a.connectEnv},
		{"GET", "/v1/connect/env/{service}", "/api/connect/env/{service}", "connect", "Download one vendor's .env", "nothing — the lab API's own state and the keys directory", a.connectEnv},
		{"GET", "/v1/connect/files/{service}/{name}", "/api/connect/files/{service}/{name}", "connect", "Download one key or certificate file", "nothing — the keys directory", a.connectFile},
	}
}

func (a *app) health(w http.ResponseWriter, r *http.Request) {
	httputilx.WriteJSON(w, 200, map[string]string{"status": "ok", "service": "lab-api"})
}

// apiIndex answers GET /v1: the table above without its handlers. An agent
// starting cold reads this first; GET /v1/services then says where every
// service lives and which of its own endpoints are worth calling directly.
//
// build is a hash of the table, so it changes exactly when the API does.
func apiIndex(table []route) http.HandlerFunc {
	h := sha256.New()
	for _, rt := range table {
		fmt.Fprintf(h, "%s %s %s %s %s %s\n", rt.Method, rt.Path, rt.Alias, rt.Group, rt.Summary, rt.Reaches)
	}
	build := hex.EncodeToString(h.Sum(nil))[:12]
	return func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, map[string]any{
			"service":  "lab-api",
			"version":  "v1",
			"build":    build,
			"docs":     apiDoc,
			"services": "GET /v1/services lists every service: base_url, browse_url, auth and endpoints; docs/api/services/<id>.md has each in full",
			"groups":   apiGroups,
			"routes":   table,
		})
	}
}
