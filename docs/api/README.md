# APIs in the lab

Every API this lab answers on: which ones exist, who calls which, and where
each is documented route by route. The same material is in the console, on
**Docs → API reference** (`http://127.0.0.1:8090/#docs/api`), which reads
the lab API's route list from the lab API itself.

## The short answer

There is one API over the lab: the **lab API**, `cmd/labapi`, on `:8097`
under `/v1` ([lab-api.md](lab-api.md)). The console page, scripts, test
suites and agents all call it the same way, for everything lab-level:
merchants, the clock, settlement runs, what happened, each service's record
and health, and each service's `/sim/*` hooks through one passthrough.

- **The console** (`cmd/console`, `:8090`) is a page. Its server serves the
  page and forwards `/v1` to the lab API, so the page calls its own origin;
  it keeps no state and holds no credential.
- **Each service has two ports.** Its vendor port carries the vendor's
  own API, behind the vendor's own login; your platform calls it directly,
  exactly as it would call the real vendor, and that traffic never goes
  through the lab API. Its **control port** (the vendor port plus 1000)
  carries the lab's hooks, `/sim/*`, with no vendor login; only the lab
  API calls it. The lab API holds no vendor credentials: those are the
  platform's.
- **The e2e suite** (`make e2e`) plays the platform. It calls each
  vendor's surface with that vendor's login, as your platform does, and
  steers and reads the lab through the lab API with `LAB_API_TOKEN`, as an
  agent does. It never calls a control port or `/internal/*`, so the same
  run works against a lab on your machine or in the cloud.

The lab API is a client of the services, never in the platform's way: no
vendor traffic goes through it, and every route has a service endpoint
behind it or is the lab API's own state (the merchant registry and the
plugin registrations). The control ports are not published, so from
outside the lab's network the lab API is the one way to a service's
hooks.

What the lab API is still to gain is in
[docs/lab-api/plan.md](../lab-api/plan.md).

**Where authentication is.** Between a person, an agent or the console and
the lab API: `LAB_API_TOKEN`, a bearer token, when set — on the lab API and
the console alike, from the host's environment under compose
(`LAB_API_TOKEN=… make up`) and from a ConfigMap in the pod
([deploy-pod.md](../deploy-pod.md#asking-for-a-token)). Unset, the lab API
is open. Between your
platform and a vendor simulation: that vendor's own login, as with the real
vendor. Between the lab API and a simulation's control port: none — the
control port is trusted by where it is. Compose does not publish it, so
only the lab's own network reaches it, and `SIM_CONTROL_ALLOW` on a service
narrows that to the addresses listed.

## Who calls what

```
  console page ───▶ console :8090 ──(forwards /v1)──┐
  (your browser)    page, no state                  │
                                                    ▼
  scripts, agents ─────────────────────────▶┌──────────────────────────────────┐
  e2e suite (make e2e) ────────────────────▶│ lab API :8097                    │◀── POST /v1/plugins ──┐
                                            │   GET /v1          the index     │─── buttons, files ──▶ │
                                            │   /v1/*            the lab       │                       │
                                            │   /v1/services/{id}/sim/*  every │                your platform
                                            │                    service's hooks│                (a plugin)
                                            │   own state: merchants, plugins  │                       │
                                            └────────────────┬─────────────────┘                       │
                                                             │ control ports only, no vendor login     │
                                                             ▼                                         │
                                            ┌──────────────────────────────────┐                       │
  e2e suite, as the platform ──────────────▶│ services, one process each       │◀── vendor traffic ────┤
  (vendor surfaces only)                    │   vendor port :808x, :2222       │─── webhooks ────────▶ │
                                            │     the vendor's API, its login  │                       │
                                            │     /internal/*      plumbing    │                       │
                                            │   control port :908x  /sim/*     │                       │
                                            │ clock :8096        GET /clock    │◀── follows the clock ─┘
                                            └──────────────────────────────────┘
```

| Caller | Calls | Never calls |
| --- | --- | --- |
| Console page | the lab API's `/v1`, through the console's origin | a service directly |
| Console server | the lab API, forwarding what the page sends | anything else |
| Scripts and agents | the lab API for everything lab-level; a vendor surface when testing an integration with that vendor | — |
| Lab API | each service's control port; the clock's `GET /clock`; a plugin's `base_url` | a vendor surface: it holds no vendor credentials |
| The e2e suite | vendor surfaces, with each vendor's login, as the platform; the lab API for everything lab-level, with `LAB_API_TOKEN` | a control port, `/internal/*`, the console |
| Your platform | vendor surfaces; the lab API's `POST /v1/plugins`; the clock's `GET /clock` | `/sim/*` or `/internal/*` from production code: the real vendor has neither. Its test harness may use the lab API, like any script |
| One service, of another | `/internal/*` plumbing (B4B → Banking Circle's bridge, Worldline's wire → Banking Circle, Banking Circle → the business bank), webhooks and callbacks | — |

## The three surfaces of a service

| Surface | What it is | Auth | Exists at the real vendor? |
| --- | --- | --- | --- |
| **Vendor surface** | the API the vendor publishes: Worldline's SFTP and SFT channel, B4B's Oversight API, Banking Circle's Connect API, ACI's webhooks, the verification vendors | the vendor's own: SFTP keys, RS512 JWT, mTLS + Basic→Bearer, API keys | yes, and the shape matches it |
| **Lab plumbing** (`/internal/*`) | stand-ins for something with no API: money arriving by wire, a payout passed to the beneficiary's bank, B4B's bridge into Banking Circle | none, as a rule | no |
| **Lab control** (`/sim/*`) | what the lab uses to drive and observe a simulation: activity logs, seeding, running a cycle now, forcing an outcome, pausing a delivery. On the service's **control port**, the vendor port plus 1000, plain HTTP | none: trusted by network placement, optionally `SIM_CONTROL_ALLOW` | no |

The swap rule follows from the table: pointing a platform at the real
vendor replaces the vendor surface. Nothing the platform calls may be
`/internal/*` or `/sim/*`.

## The control contract

Every service serves its lab hooks on a **control port** of its own:
`SIM_CONTROL_LISTEN`, by default the vendor port plus 1000 (`:9085` for
Banking Circle's `:8085`), plain HTTP even where the vendor port is HTTPS,
with no vendor login. It answers `GET /health` (`"surface": "control"`) and
every `/sim/*` route, and logs each call as a request line. The old `/sim`
paths still answer on the vendor port, behind whatever login they had,
until phase 4 of [the plan](../lab-api/plan.md) retires them.

Three `/sim` routes every service answers the same way
(`internal/simctl`, designed in
[lab-api/sim-control.md](../lab-api/sim-control.md)). Each service's
reference says what its reset clears; the shapes are here.

**`GET /sim/info`** — who this is and what it can do:

```json
{"service": "banking-circle", "kind": "vendor", "simulates": "Banking Circle Connect",
 "build": "3f2a91c0d4e1", "started_at": "2026-11-07T08:00:01Z",
 "vendor_listeners": [":8085", ":8095"], "control_listener": ":9085",
 "clock": {"follows": "http://clock:8096/clock", "offset_ms": -225000000},
 "reset_scopes": ["flow", "all"], "hooks": [],
 "logs": {"held": 412, "capacity": 2000}}
```

`kind` is `vendor` (a third party's service, simulated), `stand-in` (a
service the platform team would provide, simulated because a test does not
need the real one or it does not exist yet) or `lab` (the clock;
[catalogue.md](../catalogue.md#which-half-is-which)). `build` is there when
the binary recorded its VCS revision; `clock` when the service follows the
lab clock. `hooks` lists the trap hooks it offers, and `traps` how many are
armed and how many steps are pending (see [Traps](#traps)).

**`GET /sim/logs?since=&limit=&q=`** — the process's own log lines, what
`podman logs` shows, kept in a ring of the last `SIM_LOG_RING` lines
(default 2000). Stdout is unchanged. The ring leaves out the request lines
for `GET /health` and `GET /sim/logs` — the lab API's probe and the reading
of the logs themselves — so watching the lab does not fill it.

```json
{"service": "banking-circle",
 "lines": [{"seq": 1841, "at": "2026-11-07T09:14:02.114Z", "text": "banking-circle: …"}],
 "next": 1841, "dropped": 0}
```

| Parameter | |
| --- | --- |
| `since` | lines after this `seq`; omitted, the newest `limit` lines |
| `limit` | 1–1000, default 200 |
| `q` | case-insensitive substring filter |

`next` is the cursor for the next read: pass it back as `since`. It moves
past lines a `q` filter skipped, so a filtered reader never rereads them.
`dropped` counts lines after `since` the ring no longer holds — the reader
fell behind, and the gap is said, not silent. The log is a window onto a
run, not a record of one; a reset does not clear it, it writes a line
marking the reset.

**`POST /sim/reset`** `{"scope": "flow"|"all", "force": false}` — put the
service back to a known state. The body is optional (`flow`), and decoded
strictly: a misspelt field is `400`, never a reset with another scope.

- **`flow`** clears what a run makes — payments, files, callbacks and
  notifications in flight, activity — and keeps what a platform set up once
  and would otherwise have to set up again: Banking Circle's
  subscriptions, B4B's boarding, Worldline's enrolment.
- **`all`** puts the service back as it was on first start.
- Work in flight is cancelled, not raced: a payment still sleeping out its
  processing delay when the reset lands is dropped, never completed.
- Activity logs are cleared, so the Dashboard's diagram starts from zero.

```json
{"service": "banking-circle", "scope": "flow",
 "cleared": {"payments": 42, "notifications": 3}, "kept": ["subscriptions", "accounts"],
 "at": "2026-11-07T09:20:00Z"}
```

`409` with the reason when the service cannot reset safely now (the clock
while a run holds it); `{"force": true}` resets anyway.

## Traps

A trap is an instruction armed in advance in one simulation: it decides
something about a future event at one of its **hooks** — which outcome it
takes, and when (designed in [lab-api/traps.md](../lab-api/traps.md)). A
trap only ever puts the vendor in a state the real vendor can be in, and it
is invisible on the vendor surface: what a trap caused carries
`detail.trap` in the simulation's own activity, nowhere a platform looks.

A simulation with hooks serves, on its control port (and through the lab
API as `/v1/services/{id}/sim/traps…`):

| Route | |
| --- | --- |
| `GET /sim/traps/hooks` | the catalogue: each hook, its subject, its match fields, its effect, its graph |
| `GET /sim/traps?state=` | the traps, newest first |
| `POST /sim/traps` | arm one: `201` with it, `400` with the reason |
| `GET /sim/traps/{id}` | one trap: what it caught, the steps applied and still pending |
| `DELETE /sim/traps/{id}` | disarm it and cancel its pending steps |
| `DELETE /sim/traps` | disarm every one |

```json
{"label": "merchant 7: processed, then reversed",
 "hook": "payment.outgoing",
 "match": {"creditor.iban": "GB00SIM0000000000777"},
 "times": 1,
 "effect": {"path": [{"to": "OutgoingPaymentProcessed"},
                     {"to": "Reversed", "after": "10s", "reason": "scheme"}]}}
```

- **Match**: every field given must match; `{}` matches every event. A
  value is a string (exact), `{"in": […]}`, `{"prefix": "…"}`, or for
  decimals `{"gte": "…", "lte": "…"}`. IBANs are compared upper case
  without spaces; decimals as numbers (`"10"` is `"10.00"`). An unknown
  field is a `400` naming the hook's fields.
- **Precedence**: an event is offered to the armed traps on its hook in
  the order they were armed; the first that matches takes it, and only
  that one. "The next two payouts are rejected" is two traps armed one
  after the other.
- **Times**: how many events a trap takes; `1` when left out, `null` for
  every one until disarmed. `expires_at` stops it taking anything after.
- **Path**: the states a caught event is taken to, each checked against
  the hook's graph when armed (`400` naming an edge that is not there). A
  step waits `after` (real time; the first step defaults to the vendor's
  normal delay) or `after_lab` (lab-clock time: move the clock and it
  applies). `[]` leaves the subject where it starts. Each step goes
  through the vendor's own engine — same books, notifications, reports and
  status answer as a transition no trap touched — and when the books
  cannot follow one, the vendor's own rule applies instead and the rest of
  the path is dropped (`trap.diverted`).
- **Lifecycle**: `armed` → `spent` (times reached), `expired`, or
  `disarmed`. A trap stays listed with what it caught until a reset.
  Disarming cancels its pending steps; a reset disarms and forgets every
  trap — except the **standing** ones a simulation arms from its own
  configuration, which it arms again.
- **Activity**: a `traps` log on the simulation (`trap.armed`,
  `trap.caught`, `trap.step`, `trap.diverted`, `trap.disarmed`), and a log
  line per catch and step.

Hooks so far: Banking Circle's `payment.outgoing` and B4B's
`payment.create` — each in its service's reference. `400` for a scope
the service does not honour. The lab API resets the whole lab in one call,
in order, with `POST /v1/lab/reset`, and reads every service's log lines
merged with `GET /v1/logs` ([lab-api.md](lab-api.md)).

## Which to call

| To | Call | How |
| --- | --- | --- |
| Drive the lab from a script or an agent: merchants, the clock, a run, what happened | the lab API | `GET /v1` first, then `/v1/*` |
| Make a vendor misbehave, or read what it saw | the lab API's passthrough | `/v1/services/{id}/sim/*`, on the service's control port |
| Test your integration with one vendor | that vendor's surface | exactly the calls your platform makes to the real one |
| Put your platform's card in the console | the lab API | `POST /v1/plugins` ([plugins.md](../plugins.md)) |
| Keep your platform's business dates on the lab's | the clock | `GET /clock` every second ([plugins.md](../plugins.md#the-clock)) |
| Prove every hop is connected | `make e2e` | it plays the platform on the vendor surfaces and steers the lab through the lab API |

[agent.runbooks.md](../../agent.runbooks.md) is the worked how-to: every
step of a settlement run as one call.

## Starting cold, as an agent

1. `GET http://127.0.0.1:8097/v1`: every route with its group, a one-line
   summary and what it reaches. A tool-calling agent or a code generator
   reads the same routes as OpenAPI 3.1 at `GET /v1/openapi.json`
   ([openapi.yaml](openapi.yaml)): parameters, request bodies, statuses.
2. `GET http://127.0.0.1:8097/v1/services`: every service with its health,
   `browse_url` (where you reach it from the host), auth, and the endpoints
   worth knowing; plus the registered platform, if any.
3. A service's hooks are in its reference below, under *Lab control*; call
   them as `/v1/services/{id}/sim/…`.

With `LAB_API_TOKEN` set on the lab, every call but `GET /health` carries
`Authorization: Bearer $LAB_API_TOKEN`.

## Conventions

These hold for the lab's own routes: the lab API, every `/sim/*` and
`/internal/*`, the clock and the stand-ins. A vendor surface answers in its
vendor's shape instead, errors included. Banking Circle's `400` body, for
example, is Banking Circle's.

- **JSON in and out.** Request bodies are decoded strictly where the code
  says so (`httputilx.ReadJSON` refuses unknown fields).
- **Errors** are `{"error": "…", "status": n}` (`httputilx.Error`), the
  message in the failing peer's own words where there is one. In the lab
  API, a service that refuses or cannot be reached comes back as `502` with
  the service's message; the passthrough, and the routes that forward a call
  as it is (a plugin's buttons and files, moving and holding the clock, the
  Worldline cycle and channel, the settlement pull), pass the service's
  status and body through unchanged. Each route in
  [lab-api.md](lab-api.md) says which it does.
- **`501`, not `500`, for what the backend cannot do.** A lab API route that
  asks a service for something it has no endpoint for (an account at a bank
  that cannot open one, the activity of a service that keeps no log) answers
  `501`, so a caller can tell "not possible here" from "failed".
- **Health** is `GET /health` on every HTTP service, `2xx` when up.
- **Activity logs** are `GET /sim/activity[?limit=N]` (default 100), in one
  shape everywhere (`internal/activity`):

  ```json
  {"logs": [{"name": "payments", "title": "…", "note": "…", "total": 412, "kept": 256,
             "last": {…},
             "events": [{"seq": 412, "at": "2026-11-07T09:00:00Z", "op": "payment.create",
                         "peer": "10.89.0.1", "summary": "…", "status": "ok|warn|bad",
                         "detail": {"…": "…"}, "steps": [ … ]}]}]}
  ```

  `at` is always the wall clock, even with the lab clock moved; `total`
  counts everything ever recorded and `kept` what is still in the ring, so
  a list shorter than `total` is a window, not the whole run.
- **The console's answers carry `X-Console-Build`**, a hash of the page it
  serves (design-system.md, contract 11), the lab API's answers forwarded
  through it included.

## Reference

| Document | Service | Side | Listens |
| --- | --- | --- | --- |
| [lab-api.md](lab-api.md) | lab API | lab | `:8097` (and `:8090/v1` through the console) |
| [openapi.yaml](openapi.yaml) | lab API, as OpenAPI 3.1 | lab | served at `GET /v1/openapi.json` |
| [services/worldline.md](services/worldline.md) | Worldline | vendor | `:8084` HTTP, `:2222` SFTP |
| [services/b4b.md](services/b4b.md) | B4B Payments | vendor | `:8086` |
| [services/banking-circle.md](services/banking-circle.md) | Banking Circle | vendor | `:8085` Connect API, `:8095` bridge |
| [services/aci.md](services/aci.md) | ACI | vendor | `:8087` |
| [services/bank.md](services/bank.md) | business bank | vendor (the far end) | `:8081` |
| [services/verify.md](services/verify.md) | AML decision | stand-in | `:8088` |
| [services/verification.md](services/verification.md) | verification vendors | vendor | `:8089` |
| [services/clock.md](services/clock.md) | lab clock | lab | `:8096` |
| [services/settlement.md](services/settlement.md) | settlement | platform stand-in | `:8083` |
| [services/receiver.md](services/receiver.md) | webhook receiver | platform stand-in | `:8443` HTTPS |
| [services/payment-api.md](services/payment-api.md) | payment API | demo stand-in | `:8080` |
| [services/notifier.md](services/notifier.md) | notifier | demo stand-in | `:8082` |
| [../plugins.md](../plugins.md) | the plugin contract | platform | what a platform serves on its `base_url` |

Each service reference is in the same order: what the service is, an *At a
glance* table, then the **vendor surface**, **lab plumbing** and **lab
control** routes, each route under its own heading, and finally who calls
it.

## Keeping this true

The documents are held to the code by tests, so a route cannot be added or
removed without its documentation following:

- The lab API registers its routes from one table, `cmd/labapi/routes.go`.
  `GET /v1` serves that table, and the Docs view draws it.
  `GET /v1/openapi.json` is generated from it and from what each route
  takes (`cmd/labapi/openapi.go`); `cmd/labapi/openapi_test.go` fails if a
  route that takes input is not annotated, or if
  [openapi.yaml](openapi.yaml) is not the generated document.
  `cmd/labapi/routes_test.go` fails if a route has no summary, if the index
  and the routes served differ (aliases included), or if
  [lab-api.md](lab-api.md) does not document exactly those routes.
- `internal/console/apidocs_test.go` fails if a service in the catalogue
  has no reference under `services/`, if a reference and the routes that
  service's source registers differ, or if a card lists a route the service
  does not serve.

To add a lab API route: add a row to the table in `routes.go`, say what it
takes in `openapi.go` if it takes anything, add its section to
`lab-api.md`, rewrite `openapi.yaml` (`go test ./cmd/labapi -run OpenAPI
-update`), and run `go test ./cmd/labapi`. To add a service
route: add its section to the service's reference (and, if a person would
want it on the service's card, an `Endpoint` in
`internal/console/catalogue.go`), and run `go test ./internal/console`. A
`/sim` hook a service gains needs no lab API change: the passthrough
reaches it.
