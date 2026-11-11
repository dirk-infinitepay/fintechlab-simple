package main

import (
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"github.com/webduvet/fintechlab-simple/internal/httputilx"
)

// The lab API as an OpenAPI 3.1 document: what a tool-calling agent or a
// code generator reads (docs/lab-api/lab-api.md §1, §11).
//
// It is generated from the route table in routes.go — every path, method,
// summary and group — and from opSpecs below, which say what each route
// takes: its query parameters and its request body. So the document cannot
// list a route that is not served or miss one that is, and a test fails
// when a route that takes input has no entry here, or an entry names a
// route that does not exist. GET /v1/openapi.json serves it;
// docs/api/openapi.yaml is the same document, checked in, and a test holds
// the two equal (go test ./cmd/labapi -run OpenAPI -update rewrites it).
//
// The console's old /api aliases are left out: they are the same routes,
// kept for plugins until phase 4, and a new client should not learn them.

// openAPIDoc is where the checked-in copy lives, relative to the repo.
const openAPIDoc = "docs/api/openapi.yaml"

// param is one query parameter.
type param struct {
	Name     string
	About    string
	Type     string // "string" (default), "integer", "boolean"
	Enum     []string
	Required bool
}

// opSpec is what one route takes, beyond its path.
type opSpec struct {
	Query []param
	// Body is the JSON schema of the request body; nil means none.
	Body map[string]any
	// BodyRequired: the route refuses an empty body (strict decoding; send
	// {} for all defaults).
	BodyRequired bool
	// BodyMedia is the body's media type when it is not JSON.
	BodyMedia string
	// Returns is the answer's media type when it is not JSON.
	Returns string
	// Status is the success status when it is not 200.
	Status int
	// Verbatim: the answer is the upstream's own, status and body.
	Verbatim bool
	// Bodyless marks a route that changes something but takes no body, so
	// the test can tell it apart from one nobody annotated.
	Bodyless bool
}

// --- schema helpers ------------------------------------------------------

func str(about string) map[string]any { return map[string]any{"type": "string", "description": about} }
func integer(about string) map[string]any {
	return map[string]any{"type": "integer", "description": about}
}
func boolean(about string) map[string]any {
	return map[string]any{"type": "boolean", "description": about}
}
func enum(about string, values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values, "description": about}
}
func list(about string, items map[string]any) map[string]any {
	return map[string]any{"type": "array", "items": items, "description": about}
}
func anyObject(about string) map[string]any {
	return map[string]any{"type": "object", "additionalProperties": true, "description": about}
}

// obj is an object with these properties; required names the ones it must
// have. Strict: an unknown field is refused, as the lab decodes.
func obj(props map[string]any, required ...string) map[string]any {
	o := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

var (
	scopeBody = obj(map[string]any{
		"scope": enum("flow (default) clears what a run makes and keeps what a platform set up once; all puts it back as on first start", "flow", "all"),
		"force": boolean("reset even when the sim says it is not safe now (409): the clock while a run holds it"),
	})
	tradeBody = obj(map[string]any{
		"count":        integer("how many card payments per outlet; default 5"),
		"amount_cents": integer("each payment's amount in cents; default varies per payment"),
		"currency":     str("ISO 4217; default the merchant's"),
		"date":         str("YYYY-MM-DD, the day traded; default yesterday on the wall clock — pass it when the lab clock is moved"),
	})
	logQuery = []param{
		{Name: "since", About: "a previous answer's next: lines after it; omitted, the newest limit lines"},
		{Name: "limit", Type: "integer", About: "1 to 1000, default 200"},
		{Name: "q", About: "case-insensitive substring filter"},
	}
	stepSchema = obj(map[string]any{
		"to":                 str("the next state; the hook's graph says which may follow which"),
		"after":              str("real time after the previous step (or the event): \"10s\", \"2h\", \"1d\""),
		"after_lab":          str("lab-clock time after the previous step instead: applies when the lab clock passes it"),
		"reason":             str("a parameter of the state, when the hook's graph lists it"),
		"reason_code":        str("a parameter of the state, when the hook's graph lists it"),
		"reason_description": str("a parameter of the state, when the hook's graph lists it"),
	}, "to")
)

// envConfigSchema is an environment's config (internal/labdb.Config).
var envConfigSchema = obj(map[string]any{
	"settlement_identifier": str("Worldline's settlement file name for this environment"),
	"webhook_hosts":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "the platform's callback hosts its subscriptions and callbacks may reach"},
	"aci_webhook_url":       str("where ACI delivers this environment's webhooks"),
	"poll_url":              str("the platform's Worldline poll, which POST /v1/platform/poll calls"),
	"poll_token":            str("sent to poll_url as a bearer; shown as (set), and kept when a change leaves it out"),
	"databases":             map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "the platform's databases by the seeder's names (platform, settle): postgres:// URLs, shown as (set)"},
	"b4b_callback_url":      str("where B4B calls back a record that names no callback_url"),
	"clock":                 str("follows or wall: a seen wall environment blocks moving the lab clock"),
})

// opSpecs annotates every route that takes something beyond its path.
var opSpecs = map[string]opSpec{
	"POST /v1/lab/reset": {Body: obj(map[string]any{
		"scope":    enum("flow (default) or all", "flow", "all"),
		"force":    boolean("reset even while the lab clock is held (a settlement run in flight)"),
		"services": list("only these services, in reset order; lab-api is the merchant registry", str("a service id")),
	})},
	"GET /v1/logs": {Query: append([]param{{Name: "services", About: "comma-separated service ids; omitted, every lab service"}}, logQuery...)},
	"POST /v1/stand-ins/{id}": {BodyRequired: true, Body: obj(map[string]any{
		"connected": boolean("connect or disconnect the wiring the stand-in owns"),
		"shown":     boolean("show or hide its card and its hops"),
	})},
	"GET /v1/services/{id}/activity": {Query: []param{{Name: "limit", Type: "integer", About: "how many events per log; default 100"}}},
	"POST /v1/services/{id}/probe":   {Bodyless: true},
	"POST /v1/environments": {BodyRequired: true, Status: 201, Body: obj(map[string]any{
		"name":   str("what the environment is called; its id is made from it unless given"),
		"id":     str("lower-case letters, digits and dashes; default the name's"),
		"config": envConfigSchema,
	})},
	"PATCH /v1/environments/{id}": {BodyRequired: true, Body: obj(map[string]any{
		"name":   str("a new name; the id does not change"),
		"config": envConfigSchema,
	})},
	"DELETE /v1/environments/{id}": {Bodyless: true},
	"POST /v1/settlement-files": {BodyRequired: true, Status: 201, BodyMedia: "application/octet-stream",
		Query: []param{{Name: "name", About: "what to keep it as", Required: true}}},
	"POST /v1/settlement-files/generate": {BodyRequired: true, Status: 201, Body: obj(map[string]any{
		"transactions": integer("shared between the currencies by outlet count, at least one per outlet"),
		"currency":     str("EUR or GBP; empty for a file per currency"),
		"seed":         integer("replays the transactions; null for new ones"),
	}, "transactions")},
	"POST /v1/settlement-files/cut": {BodyRequired: true, Status: 201, Body: obj(map[string]any{
		"currency": str("EUR or GBP"),
		"date":     str("YYYY-MM-DD, the trading day; default what the cycle would settle"),
		"slot":     str("ER (the morning file, default) or AR"),
	}, "currency")},
	"POST /v1/platform/seed": {BodyRequired: true, Body: obj(map[string]any{
		"merchants": integer("how many to add, 1 to 20000"),
		"mode":      str("add (default), reseed (remove every generated merchant first) or remove"),
		"files":     str("delete (default) or keep: the generated settlement files, on reseed or remove"),
		"seed":      integer("replays a population; null for a new one"),
	})},
	"GET /v1/settlement-files/preview": {Query: []param{{Name: "name", About: "the file's name", Required: true}, {Name: "lines", Type: "integer", About: "1 to 1000, default 100"}}},
	"DELETE /v1/settlement-files":      {Bodyless: true, Query: []param{{Name: "name", About: "the file's name", Required: true}}},
	"POST /v1/settlement-files/publish": {BodyRequired: true, Body: obj(map[string]any{
		"files": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "the files to publish, at most one per currency"},
		"wire":  boolean("wire each ER file's lump sum to the environment's safeguarding account, as Worldline's morning does; default true"),
	}, "files")},
	"POST /v1/platform/poll": {BodyRequired: true, Verbatim: true, Body: obj(map[string]any{
		"fileType": str("ER (the morning file) or AR (the afternoon's)"),
	}, "fileType")},
	"POST /v1/environments/{id}/token": {Bodyless: true},
	"GET /v1/services/{id}/logs":       {Query: logQuery, Verbatim: true},
	"POST /v1/services/{id}/reset":     {Body: scopeBody, Verbatim: true},
	"ANY /v1/services/{id}/sim/{path...}": {Verbatim: true,
		Body: anyObject("whatever the service's /sim route takes, sent on as it is; each service's reference lists its routes")},
	"GET /v1/traps":    {Query: []param{{Name: "state", About: "only traps in this state", Enum: []string{"armed", "spent", "expired", "disarmed"}}}},
	"DELETE /v1/traps": {Bodyless: true},
	"POST /v1/clock": {BodyRequired: true, Verbatim: true, Body: map[string]any{
		"description": "exactly one of at, advance or mode",
		"oneOf": []any{
			obj(map[string]any{"at": str("an RFC 3339 instant to pin the lab clock to")}, "at"),
			obj(map[string]any{"advance": str("how far to move it on: \"1h\", \"1d\"")}, "advance"),
			obj(map[string]any{"mode": enum("real follows the wall clock; auto-business-day the most recent business day", "real", "auto-business-day")}, "mode"),
		},
	}},
	"POST /v1/clock/holds": {BodyRequired: true, Verbatim: true, Body: obj(map[string]any{
		"holder":      str("who holds it: the name to release it by"),
		"reason":      str("why, shown to anyone whose move is refused"),
		"ttl_seconds": integer("how long the hold lasts unless renewed or released"),
	}, "holder")},
	"DELETE /v1/clock/holds/{holder}": {Bodyless: true, Verbatim: true},
	"PUT /v1/config/worldline-channel": {BodyRequired: true, Verbatim: true,
		Body: anyObject("Worldline's SFT file-exchange channel config, as GET /v1/config shows it under worldline_channel")},
	"POST /v1/merchants": {BodyRequired: true, Status: 201, Body: obj(map[string]any{
		"partner_id":   str("the partner it trades under; default the first"),
		"legal_name":   str("the merchant's legal name"),
		"trading_name": str("default the legal name"),
		"country":      str("ISO 3166-1 alpha-2"),
		"currency":     str("ISO 4217; default the country's"),
		"mcc":          str("merchant category code"),
		"email":        str("default one derived from the name, on a .test domain"),
		"address": obj(map[string]any{
			"line1": str(""), "line2": str(""), "city": str(""), "post_code": str(""), "country": str("ISO 3166-1 alpha-2"),
		}),
		"outlets": integer("how many outlets to open with; default 1"),
	}, "legal_name")},
	"DELETE /v1/merchants/{id}": {Bodyless: true},
	"POST /v1/merchants/batches": {BodyRequired: true, Status: 201, Body: obj(map[string]any{
		"count":       integer("how many merchants"),
		"max_outlets": integer("each gets 1 to this many outlets"),
		"country":     str("ISO 3166-1 alpha-2; \"\" mixes countries"),
		"seed":        integer("the generator's seed: the same seed, the same merchants"),
		"partner_id":  str("the partner they trade under; default the first"),
	}, "count")},
	"DELETE /v1/merchants/batches/{id}":                 {Bodyless: true},
	"POST /v1/merchants/batches/{id}/provision":         {Bodyless: true},
	"POST /v1/merchants/batches/{id}/trading":           {BodyRequired: true, Status: 201, Body: tradeBody},
	"POST /v1/merchants/{id}/outlets":                   {BodyRequired: true, Status: 201, Body: obj(map[string]any{"name": str("the outlet's name; default derived")})},
	"POST /v1/merchants/{id}/status":                    {BodyRequired: true, Body: obj(map[string]any{"status": enum("", "draft", "active", "suspended")}, "status")},
	"POST /v1/merchants/{id}/provision":                 {Bodyless: true},
	"POST /v1/merchants/{id}/trading":                   {BodyRequired: true, Status: 201, Body: tradeBody},
	"POST /v1/outlets/{mid}/sanctions":                  {BodyRequired: true, Body: obj(map[string]any{"sanctions_status": enum("the beneficiary's sanctions status at B4B", "pass", "review", "fail")}, "sanctions_status")},
	"POST /v1/partners":                                 {BodyRequired: true, Status: 201, Body: obj(map[string]any{"distributor_id": str("default the first"), "name": str(""), "country": str("ISO 3166-1 alpha-2")}, "name")},
	"POST /v1/banks/{id}/accounts":                      {BodyRequired: true, Status: 201, Body: obj(map[string]any{"holder": str(""), "currency": str("ISO 4217"), "opening_balance": str("a decimal, default 0.00")}, "holder")},
	"POST /v1/banking-circle/subscriptions/{id}/test":   {Bodyless: true},
	"POST /v1/banking-circle/subscriptions/{id}/pause":  {Bodyless: true},
	"POST /v1/banking-circle/subscriptions/{id}/resume": {Bodyless: true},
	"POST /v1/banking-circle/payouts/{id}/return":       {Bodyless: true},
	"POST /v1/banking-circle/payouts/{id}/reverse":      {Bodyless: true},
	"POST /v1/actions/fund-sga":                         {Bodyless: true, Query: []param{{Name: "amount", About: "a decimal per empty safeguarding account; default 1000000.00"}}},
	"POST /v1/actions/worldline-cycle": {Bodyless: true, Verbatim: true, Query: []param{
		{Name: "slot", About: "default morning", Enum: []string{"morning", "afternoon"}},
		{Name: "date", About: "YYYY-MM-DD, the business day the file covers; default the lab clock's"},
	}},
	"POST /v1/actions/settlement-pull": {Bodyless: true, Verbatim: true},
	"POST /v1/plugins": {BodyRequired: true, Body: map[string]any{
		"type": "object", "additionalProperties": true, "required": []string{"id", "name", "base_url", "health_path", "clock"},
		"description": "A platform's descriptor (docs/plugins.md): its card, buttons, logs and settlement files. Sent again every renew_seconds.",
		"properties": map[string]any{
			"id":            str("lower-case letters, digits, dashes; not a lab service's id"),
			"name":          str("the card's title"),
			"summary":       str(""),
			"base_url":      str("where the lab API reaches the platform"),
			"browse_url":    str("where a person's browser does"),
			"health_path":   str("a 2xx here is up"),
			"activity_path": str("logs in the lab's activity shape"),
			"clock":         enum("whether it follows the lab clock", "follows", "wall"),
			"start_hint":    str("shown while it is not running"),
			"actions":       list("buttons, at most eight", anyObject("{id, label, path, method, primary, confirm, note, fields}")),
			"settlement":    anyObject("settlement files: files_path, run_path, upload_path, preview_path, delete_path, reveal_path, status_path"),
			"stand_ins":     anyObject("how the lab's stand-ins should be while it is registered"),
		},
	}},
	"DELETE /v1/plugins/{id}":                {Bodyless: true, Status: 204},
	"POST /v1/plugins/{id}/actions/{action}": {Verbatim: true, Body: anyObject("the action's fields, if it declares any; forwarded to the platform")},
	"POST /v1/plugins/{id}/run":              {BodyRequired: true, Verbatim: true, Body: obj(map[string]any{"files": list("file names, one run per currency", str(""))}, "files")},
	"POST /v1/plugins/{id}/files":            {BodyRequired: true, Verbatim: true, BodyMedia: "application/octet-stream", Query: []param{{Name: "name", About: "the file's name", Required: true}}},
	"DELETE /v1/plugins/{id}/files":          {Bodyless: true, Verbatim: true, Query: []param{{Name: "name", About: "the file's name", Required: true}}},
	"GET /v1/plugins/{id}/files/preview":     {Verbatim: true, Query: []param{{Name: "name", About: "the file's name", Required: true}, {Name: "lines", Type: "integer", About: "1 to 1000, default 100"}}},
	"POST /v1/plugins/{id}/files/reveal":     {BodyRequired: true, Verbatim: true, Body: obj(map[string]any{"name": str("the file's name")}, "name")},
	"GET /v1/connect":                        {Query: []param{{Name: "host", About: "the host a platform reaches the lab at; default the request's"}}},
	"GET /v1/connect/env":                    {Returns: "text/plain", Query: []param{{Name: "host", About: "the host a platform reaches the lab at"}}},
	"GET /v1/connect/env/{service}":          {Returns: "text/plain", Query: []param{{Name: "host", About: "the host a platform reaches the lab at"}}},
	"GET /v1/connect/files/{service}/{name}": {Returns: "application/octet-stream"},
}

// pathParamAbout says what a path parameter is, by the segment before it.
var pathParamAbout = map[string]string{
	"services/id":      "a lab service: worldline, b4b, banking-circle, aci, bank, verify, verification, clock, settlement, receiver, payment-api, notifier",
	"sim/path":         "the rest of the service's /sim path; it may hold slashes",
	"hops/id":          "a hop id, as GET /v1/flow lists them",
	"stand-ins/id":     "a stand-in: settlement, receiver, payment-api, notifier",
	"holds/holder":     "the holder named when the hold was taken",
	"merchants/id":     "a merchant id",
	"batches/id":       "a batch id",
	"outlets/mid":      "an outlet's MID",
	"banks/id":         "core-ledger or banking-circle",
	"subscriptions/id": "a Banking Circle subscription id",
	"payouts/id":       "a Banking Circle payment id",
	"plugins/id":       "a registered platform's id",
	"actions/action":   "one of the platform's declared action ids",
	"env/service":      "a vendor id",
	"files/service":    "a vendor id",
	"service/name":     "a file name, as GET /v1/connect lists it",
}

// --- the document --------------------------------------------------------

type oaDoc struct {
	OpenAPI      string                            `json:"openapi" yaml:"openapi"`
	Info         oaInfo                            `json:"info" yaml:"info"`
	Servers      []oaServer                        `json:"servers" yaml:"servers"`
	Tags         []oaTag                           `json:"tags" yaml:"tags"`
	Paths        map[string]map[string]oaOperation `json:"paths" yaml:"paths"`
	Components   oaComponents                      `json:"components" yaml:"components"`
	ExternalDocs oaExternal                        `json:"externalDocs" yaml:"externalDocs"`
}

type oaInfo struct {
	Title       string `json:"title" yaml:"title"`
	Version     string `json:"version" yaml:"version"`
	Description string `json:"description" yaml:"description"`
}

type oaServer struct {
	URL         string `json:"url" yaml:"url"`
	Description string `json:"description" yaml:"description"`
}

type oaTag struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description" yaml:"description"`
}

type oaExternal struct {
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	URL         string `json:"url" yaml:"url"`
}

type oaOperation struct {
	OperationID  string                `json:"operationId" yaml:"operationId"`
	Summary      string                `json:"summary" yaml:"summary"`
	Description  string                `json:"description" yaml:"description"`
	Tags         []string              `json:"tags" yaml:"tags"`
	Parameters   []oaParameter         `json:"parameters,omitempty" yaml:"parameters,omitempty"`
	RequestBody  *oaRequestBody        `json:"requestBody,omitempty" yaml:"requestBody,omitempty"`
	Responses    map[string]oaResponse `json:"responses" yaml:"responses"`
	ExternalDocs oaExternal            `json:"externalDocs" yaml:"externalDocs"`
}

type oaParameter struct {
	Name        string         `json:"name" yaml:"name"`
	In          string         `json:"in" yaml:"in"`
	Required    bool           `json:"required" yaml:"required"`
	Description string         `json:"description,omitempty" yaml:"description,omitempty"`
	Schema      map[string]any `json:"schema" yaml:"schema"`
}

type oaRequestBody struct {
	Required bool                   `json:"required" yaml:"required"`
	Content  map[string]oaMediaType `json:"content" yaml:"content"`
}

type oaMediaType struct {
	Schema map[string]any `json:"schema" yaml:"schema"`
}

type oaResponse struct {
	Description string                 `json:"description" yaml:"description"`
	Content     map[string]oaMediaType `json:"content,omitempty" yaml:"content,omitempty"`
}

type oaComponents struct {
	Schemas         map[string]any `json:"schemas" yaml:"schemas"`
	SecuritySchemes map[string]any `json:"securitySchemes" yaml:"securitySchemes"`
}

var (
	pathParam = regexp.MustCompile(`\{([a-zA-Z_]+)(\.\.\.)?\}`)
	errorRef  = map[string]any{"$ref": "#/components/schemas/Error"}
)

// openAPI builds the document from the route table and opSpecs.
func openAPI(table []route) oaDoc {
	doc := oaDoc{
		OpenAPI: "3.1.0",
		Info: oaInfo{
			Title:   "fintechlab lab API",
			Version: "v1",
			Description: "The one API over the lab: merchants, the clock, settlement runs and what happened, every simulation's " +
				"record, health, logs and reset, and its lab hooks (/sim/*) through one passthrough. The lab API reaches each " +
				"simulation on its control port and holds no vendor credentials; a platform calls the vendors directly. " +
				"Errors are {\"error\", \"status\"}. When LAB_API_TOKEN is set, every call but GET /health carries " +
				"Authorization: Bearer <it>. The console's old /api paths still answer, as aliases, and are not listed.",
		},
		Servers: []oaServer{
			{URL: "http://127.0.0.1:8097", Description: "the lab API"},
			{URL: "http://127.0.0.1:8090", Description: "through the console, which forwards /v1"},
		},
		Paths: map[string]map[string]oaOperation{},
		Components: oaComponents{
			Schemas: map[string]any{
				"Error": obj(map[string]any{
					"error":  str("what went wrong, in the failing peer's own words where there is one"),
					"status": integer("the HTTP status"),
				}, "error"),
			},
			SecuritySchemes: map[string]any{
				"labToken": map[string]any{"type": "http", "scheme": "bearer",
					"description": "LAB_API_TOKEN, when the lab sets one; unset, no auth"},
			},
		},
		ExternalDocs: oaExternal{Description: "the reference, with examples", URL: apiDoc},
	}
	for _, g := range apiGroups {
		doc.Tags = append(doc.Tags, oaTag{Name: g.ID, Description: g.Note})
	}
	for _, rt := range table {
		methods := []string{strings.ToLower(rt.Method)}
		if rt.Method == "ANY" {
			methods = []string{"get", "post", "put", "delete"}
		}
		path := pathParam.ReplaceAllString(rt.Path, "{$1}")
		for _, m := range methods {
			op := operation(rt, m)
			if doc.Paths[path] == nil {
				doc.Paths[path] = map[string]oaOperation{}
			}
			doc.Paths[path][m] = op
		}
	}
	return doc
}

func operation(rt route, method string) oaOperation {
	spec := opSpecs[rt.Method+" "+rt.Path]
	op := oaOperation{
		OperationID:  operationID(method, rt.Path),
		Summary:      rt.Summary,
		Description:  "Reaches: " + rt.Reaches + ".",
		Tags:         []string{rt.Group},
		ExternalDocs: oaExternal{URL: apiDoc + "#" + anchor(rt.Method+" "+rt.Path)},
		Responses:    map[string]oaResponse{},
	}
	if spec.Verbatim {
		op.Description += " The answer is the upstream's own, status and body."
	}
	// Path parameters, described by the segment before each.
	segs := strings.Split(strings.Trim(rt.Path, "/"), "/")
	for i, seg := range segs {
		m := pathParam.FindStringSubmatch(seg)
		if m == nil {
			continue
		}
		about := ""
		if i > 0 {
			about = pathParamAbout[strings.Trim(segs[i-1], "{}")+"/"+m[1]]
		}
		op.Parameters = append(op.Parameters, oaParameter{Name: m[1], In: "path", Required: true, Description: about,
			Schema: map[string]any{"type": "string"}})
	}
	for _, q := range spec.Query {
		schema := map[string]any{"type": "string"}
		if q.Type != "" {
			schema["type"] = q.Type
		}
		if len(q.Enum) > 0 {
			schema["enum"] = q.Enum
		}
		op.Parameters = append(op.Parameters, oaParameter{Name: q.Name, In: "query", Required: q.Required, Description: q.About, Schema: schema})
	}
	if spec.Body != nil || spec.BodyMedia != "" {
		if !(rt.Method == "ANY" && (method == "get" || method == "delete")) {
			media, schema := "application/json", spec.Body
			if spec.BodyMedia != "" {
				media, schema = spec.BodyMedia, map[string]any{"type": "string", "format": "binary"}
			}
			op.RequestBody = &oaRequestBody{Required: spec.BodyRequired, Content: map[string]oaMediaType{media: {Schema: schema}}}
		}
	}
	status := "200"
	switch spec.Status {
	case 201:
		status = "201"
	case 204:
		status = "204"
	}
	ok := oaResponse{Description: "done"}
	switch {
	case status == "204":
	case spec.Returns != "":
		schema := map[string]any{"type": "string"}
		if spec.Returns == "application/octet-stream" {
			schema["format"] = "binary"
		}
		ok.Content = map[string]oaMediaType{spec.Returns: {Schema: schema}}
	default:
		ok.Content = map[string]oaMediaType{"application/json": {Schema: map[string]any{"type": "object"}}}
	}
	op.Responses[status] = ok
	if spec.Verbatim {
		op.Responses["default"] = oaResponse{Description: "the upstream's own answer, or the lab API's error when it cannot be reached",
			Content: map[string]oaMediaType{"application/json": {Schema: map[string]any{}}}}
	} else {
		op.Responses["default"] = oaResponse{Description: "an error: 400 the caller can fix, 404 unknown, 409 refused by state, 501 not here by design, 502 a peer failed",
			Content: map[string]oaMediaType{"application/json": {Schema: errorRef}}}
	}
	return op
}

// operationID is a stable name for an operation: the method and the path's
// words, parameters as By<Name> — getServicesByIdLogs.
func operationID(method, path string) string {
	var b strings.Builder
	b.WriteString(method)
	for _, seg := range strings.Split(strings.Trim(path, "/"), "/") {
		if seg == "v1" || seg == "" {
			continue
		}
		if m := pathParam.FindStringSubmatch(seg); m != nil {
			b.WriteString("By")
			b.WriteString(camel(m[1]))
			continue
		}
		b.WriteString(camel(seg))
	}
	if b.Len() == len(method) {
		b.WriteString("Index")
	}
	return b.String()
}

func camel(s string) string {
	out := []rune{}
	up := true
	for _, r := range s {
		if r == '-' || r == '_' || r == '.' {
			up = true
			continue
		}
		if up {
			out = append(out, unicode.ToUpper(r))
			up = false
		} else {
			out = append(out, r)
		}
	}
	return string(out)
}

// anchor is the heading's anchor in lab-api.md, as GitHub makes it: the
// heading "### `GET /v1/lab`" is #get-v1lab.
func anchor(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// openAPIHandler answers GET /v1/openapi.json.
func openAPIHandler(table []route) http.HandlerFunc {
	doc := openAPI(table)
	return func(w http.ResponseWriter, r *http.Request) {
		httputilx.WriteJSON(w, 200, doc)
	}
}
