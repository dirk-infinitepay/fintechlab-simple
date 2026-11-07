package main

import (
	"net/http"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
)

// The console's API, described once.
//
// Every route below is registered by routes() and listed by GET /api, so the
// index cannot name a route that does not exist, and a route cannot exist
// without a line saying what it is for and which service it reaches. The
// long form, with bodies and answers, is docs/api/console-api.md; a test
// holds that document to exactly this table.
//
// The console stays a client of the other services (docs/console.md): most
// routes are one call to one service's own API, and Reaches names it, so a
// script or an agent can tell when it could have gone to the service itself.

// route is one console API route.
type route struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	Group   string `json:"group"`
	Summary string `json:"summary"`
	// Reaches is the service and endpoint the route calls, or what it reads
	// instead ("console state only").
	Reaches string `json:"reaches"`

	handler http.HandlerFunc
}

// routeGroup orders the index and the Docs view's API page.
type routeGroup struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Note  string `json:"note"`
}

var apiGroups = []routeGroup{
	{"lab", "Lab", "What is running and what it has done: health, the services, the run diagram, the stand-ins."},
	{"clock", "Clock", "The lab's business clock, fronted from the clock service."},
	{"config", "Configuration", "What the lab is set to, and the one setting the console can change."},
	{"merchants", "Merchants", "The console's merchant registry, and putting its merchants on the vendors."},
	{"banks", "Banks", "Accounts and payouts at the business bank and Banking Circle."},
	{"actions", "Vendor actions", "Buttons that make a vendor do now what it would otherwise do on its schedule."},
	{"plugins", "Plugins", "The platform under test: registration, its buttons and its settlement files."},
	{"connect", "Connect", "What a platform needs to point at this lab: addresses, credentials, key files."},
}

// apiDoc is where the long form of this table lives, relative to the repo.
const apiDoc = "docs/api/console-api.md"

// apiRoutes is the table. GET /api's handler is left nil: it describes the
// table, so routes() wires it where the table and the build are both known.
func (a *app) apiRoutes() []route {
	return []route{
		{"GET", "/health", "lab", "Whether the console process is up", "nothing — console state only", a.health},
		{"GET", "/api", "lab", "This index: every console route, its group and what it reaches", "nothing — console state only", nil},
		{"GET", "/api/overview", "lab", "Every service and registered plugin with its last health probe", "each service's health_path, probed in the background", a.overview},
		{"POST", "/api/services/{id}/probe", "lab", "Probe one service's health now instead of waiting for the monitor", "the service's GET health_path", a.probeService},
		{"GET", "/api/services/{id}/activity", "lab", "One service's activity logs, as the service reports them", "the service's GET /sim/activity (or plugin activity_path)", a.serviceActivity},
		{"GET", "/api/flow", "lab", "The settlement run as a sequence: participants, hops, counts, report", "each vendor's GET /sim/activity, the plugin's status_path", a.flow},
		{"GET", "/api/flow/hops/{id}", "lab", "The events behind one arrow of the run diagram", "same reads as GET /api/flow", a.flowHop},
		{"GET", "/api/stand-ins", "lab", "The stand-ins: shown or hidden, and how each is wired, read live", "settlement+aci /sim/connected, banking-circle subscriptions", a.listStandIns},
		{"POST", "/api/stand-ins/{id}", "lab", "Show, hide, connect or disconnect one stand-in", "settlement+aci POST /sim/connected; BC (de)activate", a.setStandIn},

		{"GET", "/api/clock", "clock", "The lab clock as it stands", "clock GET /clock", a.labClock},
		{"POST", "/api/clock", "clock", "Move the lab clock, or change its mode", "clock POST /clock", a.setLabClock},

		{"GET", "/api/config", "config", "The lab's settings: env knobs, BC delivery table, Worldline channel", "banking-circle /sim/delivery-config, worldline GET /config", a.getConfig},
		{"PUT", "/api/config/worldline-channel", "config", "Change Worldline's SFT file-exchange channel config", "worldline PUT /config", a.putWorldlineChannel},

		{"GET", "/api/merchants", "merchants", "The merchant registry: distributors, partners, merchants, outlets, batches", "nothing — console state only", a.listMerchants},
		{"POST", "/api/merchants", "merchants", "Create a merchant and its outlets in the registry", "nothing — console state only", a.createMerchant},
		{"DELETE", "/api/merchants/{id}", "merchants", "Delete a merchant from the registry; vendors keep what they had", "nothing — console state only", a.deleteMerchant},
		{"POST", "/api/merchants/batches", "merchants", "Generate a batch of merchants with outlets from a seed", "nothing — console state only", a.createBatch},
		{"DELETE", "/api/merchants/batches/{id}", "merchants", "Delete a batch and its merchants from the registry", "nothing — console state only", a.deleteBatch},
		{"POST", "/api/merchants/batches/{id}/provision", "merchants", "Register every outlet of a batch as a B4B beneficiary", "b4b POST /oversight/v1/beneficiaries, per outlet", a.provisionBatch},
		{"POST", "/api/merchants/batches/{id}/trading", "merchants", "Seed card payments at Worldline for every outlet of a batch", "worldline POST /sim/transactions, per merchant", a.tradeBatch},
		{"POST", "/api/merchants/{id}/outlets", "merchants", "Open another outlet (a new MID) for a merchant", "nothing — console state only", a.addOutlet},
		{"POST", "/api/merchants/{id}/status", "merchants", "Set a merchant's status: draft, active or suspended", "nothing — console state only", a.setMerchantStatus},
		{"POST", "/api/merchants/{id}/provision", "merchants", "Register a merchant's outlets as B4B beneficiaries", "b4b POST /oversight/v1/beneficiaries, per outlet", a.provisionMerchant},
		{"POST", "/api/merchants/{id}/trading", "merchants", "Seed card payments at Worldline for a merchant's outlets", "worldline POST /sim/transactions", a.seedTrading},
		{"POST", "/api/outlets/{mid}/sanctions", "merchants", "Set an outlet's sanctions status at B4B: pass, review or fail", "b4b PUT /sim/beneficiaries/{mid}/sanctions", a.setSanctions},
		{"POST", "/api/partners", "merchants", "Add a partner under a distributor", "nothing — console state only", a.createPartner},

		{"GET", "/api/banks", "banks", "Both banks with their accounts and balances", "bank GET /accounts; banking-circle GET /accounts", a.listBanks},
		{"POST", "/api/banks/{id}/accounts", "banks", "Open an account at the business bank (Banking Circle: 501)", "bank POST /accounts", a.openAccount},
		{"GET", "/api/banking-circle/subscriptions", "banks", "Banking Circle's notification subscriptions, with what is waiting", "banking-circle subscription list and /sim pending reads", a.bcSubscriptions},
		{"POST", "/api/banking-circle/subscriptions/{id}/test", "banks", "Send a subscription a test notification and report what it did", "banking-circle POST clienttest/{id}, GET /sim/activity", a.bcSubscriptionTest},
		{"POST", "/api/banking-circle/subscriptions/{id}/pause", "banks", "Hold a subscription's deliveries", "banking-circle POST /sim/subscription/{id}/pause", a.bcSubscriptionPause},
		{"POST", "/api/banking-circle/subscriptions/{id}/resume", "banks", "Release a paused subscription's deliveries", "banking-circle POST /sim/subscription/{id}/resume", a.bcSubscriptionPause},
		{"GET", "/api/banking-circle/payouts", "banks", "Banking Circle's outgoing payouts, and which can be returned or reversed", "banking-circle GET /payments", a.bcPayouts},
		{"POST", "/api/banking-circle/payouts/{id}/return", "banks", "Return a processed payout, as the beneficiary's bank would", "banking-circle POST /sim/payments/{id}/return", a.bcPayoutAction},
		{"POST", "/api/banking-circle/payouts/{id}/reverse", "banks", "Reverse a processed payout, as the scheme would", "banking-circle POST /sim/payments/{id}/reverse", a.bcPayoutAction},
		{"POST", "/api/actions/fund-sga", "banks", "Fund the empty safeguarding accounts, as Worldline's wire would", "banking-circle bridge GET balances, POST incoming-payments", a.fundSGA},

		{"POST", "/api/actions/worldline-cycle", "actions", "Cut and publish Worldline's settlement files now", "worldline POST /sim/settlement-cycle/run", a.runWorldlineCycle},
		{"POST", "/api/actions/settlement-pull", "actions", "Make the settlement stand-in pull Worldline's SFTP now", "settlement POST /worldline/pull", a.settlementPull},

		{"GET", "/api/plugins", "plugins", "The registered platforms and how each registration stands", "nothing — console state only", a.listPlugins},
		{"POST", "/api/plugins", "plugins", "Register or renew a platform's card (docs/plugins.md)", "console state; then health_path and stand-in wiring", a.registerPlugin},
		{"DELETE", "/api/plugins/{id}", "plugins", "Unregister a platform on a clean shutdown", "nothing — console state only", a.unregisterPlugin},
		{"POST", "/api/plugins/{id}/actions/{action}", "plugins", "Press one of a platform's declared buttons", "the plugin's base_url + the action's path", a.pluginAction},
		{"GET", "/api/plugins/{id}/files", "plugins", "A settling platform's settlement files", "the plugin's base_url + files_path", a.pluginFiles},
		{"POST", "/api/plugins/{id}/run", "plugins", "Run settlement files on the platform", "the plugin's base_url + run_path", a.pluginRun},
		{"POST", "/api/plugins/{id}/files", "plugins", "Upload a settlement file to the platform", "the plugin's base_url + upload_path", a.pluginUpload},
		{"DELETE", "/api/plugins/{id}/files", "plugins", "Delete a settlement file on the platform", "the plugin's base_url + delete_path (or upload_path)", a.pluginDeleteFile},
		{"GET", "/api/plugins/{id}/files/preview", "plugins", "The first lines of one settlement file", "the plugin's base_url + preview_path", a.pluginPreview},
		{"POST", "/api/plugins/{id}/files/reveal", "plugins", "Open a settlement file's folder on the platform's machine", "the plugin's base_url + reveal_path", a.pluginReveal},

		{"GET", "/api/connect", "connect", "What each vendor asks of a platform that connects: names, no values", "nothing — console state and the keys directory", a.connect},
		{"GET", "/api/connect/env", "connect", "Download fintechlab.env: every vendor's variables with values", "nothing — console state and the keys directory", a.connectEnv},
		{"GET", "/api/connect/env/{service}", "connect", "Download one vendor's .env", "nothing — console state and the keys directory", a.connectEnv},
		{"GET", "/api/connect/files/{service}/{name}", "connect", "Download one key or certificate file", "nothing — the keys directory", a.connectFile},
	}
}

func (a *app) health(w http.ResponseWriter, r *http.Request) {
	httputilx.WriteJSON(w, 200, map[string]string{"status": "ok", "service": "console"})
}

// apiIndex answers GET /api: the table above without its handlers. An agent
// starting cold reads this first; GET /api/overview then says where every
// service lives and which of its own endpoints are worth calling directly.
func apiIndex(table []route, build string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, map[string]any{
			"service":  "console",
			"build":    build,
			"docs":     apiDoc,
			"services": "GET /api/overview lists every service: base_url, browse_url, auth and endpoints; docs/api/services/<id>.md has each in full",
			"groups":   apiGroups,
			"routes":   table,
		})
	}
}
