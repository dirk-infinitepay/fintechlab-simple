# APIs in the lab

Every API this lab answers on: which ones exist, who calls which, and where
each is documented route by route. The same material is in the console, on
**Docs → API reference** (`http://127.0.0.1:8090/#docs/api`), which reads
the console's route list from the console itself.

## The short answer

There is no single gateway in front of the lab. Each service answers on its
own address, and the console is one client among several:

- **Each service's own API** is the real API layer. Your platform calls the
  vendor surfaces exactly as it would call the real vendors. Tests, agents
  and the console drive and observe the simulations through each service's
  `/sim/*` hooks.
- **The console's `/api/*`** (`:8090`) is the backend of the console page.
  It is also the most convenient lab-level API for a script or an agent: one
  address for merchants, the clock, settlement runs and what happened.
  Behind it, most routes are one call to one service's own API, and the
  route list says which ([console-api.md](console-api.md)).
- **The harness** (`make harness`) calls the services directly. It does not
  know the console exists, and the console being down never stops it.

Nothing in the lab can be reached only through the console
([console.md](../console.md)): every console route has a service endpoint
behind it, or is the console's own state (its merchant registry and the
plugin registrations).

A single lab API in front of everything, at `/v1` in a container of its
own, is designed but not built: [docs/lab-api/](../lab-api/README.md).
Until it exists, the console's `/api` is the lab-level API.

## Who calls what

```
                        ┌───────────────────────────────────┐
  console page ────────▶│ console :8090                     │
  (your browser)        │   GET /api      the route index   │
                        │   /api/*        lab-level calls   │
  scripts, agents ─────▶│   own state: merchants, plugins   │◀──── POST /api/plugins ─┐
        │  lab-level    └────────────────┬──────────────────┘ ──── buttons, files ──▶ │
        │                                │ calls the services'                       │
        │                                ▼ own APIs                          your platform
        │  vendor-level ┌───────────────────────────────────┐                (a plugin)
        └──────────────▶│ services, one process each        │◀──── vendor traffic ─────┤
                        │   vendor surface   :808x, :2222   │───── webhooks ─────────▶ │
  harness ─────────────▶│   /internal/*      plumbing       │                          │
  (make harness)        │   /sim/*           lab control    │                          │
                        │ clock :8096        GET /clock     │◀──── follows the clock ──┘
                        └───────────────────────────────────┘
```

| Caller | Calls | Never calls |
| --- | --- | --- |
| Console page | the console's `/api/*`, nothing else | a service directly |
| Console server | each service's vendor surface, `/internal/*` and `/sim/*`; a plugin's `base_url` | — |
| Scripts and agents | the console's `/api/*` for lab-level work; a service's `/sim/*` or vendor surface for vendor-level work | — |
| Harness | services' vendor surfaces and `/sim/*` | the console |
| Your platform | vendor surfaces; the console's `POST /api/plugins`; the clock's `GET /clock` | `/sim/*` or `/internal/*` from production code: the real vendor has neither. Its test harness may, like any script |
| One service, of another | `/internal/*` plumbing (B4B → Banking Circle's bridge, Worldline's wire → Banking Circle, Banking Circle → the business bank), webhooks and callbacks | — |

## The three surfaces of a service

| Surface | What it is | Auth | Exists at the real vendor? |
| --- | --- | --- | --- |
| **Vendor surface** | the API the vendor publishes: Worldline's SFTP and SFT channel, B4B's Oversight API, Banking Circle's Connect API, ACI's webhooks, the verification vendors | the vendor's own: SFTP keys, RS512 JWT, mTLS + Basic→Bearer, API keys | yes, and the shape matches it |
| **Lab plumbing** (`/internal/*`) | stand-ins for something with no API: money arriving by wire, a payout passed to the beneficiary's bank, B4B's bridge into Banking Circle | none, as a rule | no |
| **Lab control** (`/sim/*`) | what the lab uses to drive and observe a simulation: activity logs, seeding, running a cycle now, forcing an outcome, pausing a delivery | mixed today; see each service | no |

The swap rule follows from the table: pointing a platform at the real
vendor replaces the vendor surface. Nothing the platform calls may be
`/internal/*` or `/sim/*`.

## Which to call

| To | Call | How |
| --- | --- | --- |
| Drive the lab from a script or an agent: merchants, the clock, a run, what happened | the console | `GET /api` first, then `/api/*` |
| Test your integration with one vendor | that vendor's surface | exactly the calls your platform makes to the real one |
| Make a vendor misbehave, or read what it saw | that service's `/sim/*` | lab-only hooks; the console wraps the common ones |
| Put your platform's card in the console | the console | `POST /api/plugins` ([plugins.md](../plugins.md)) |
| Keep your platform's business dates on the lab's | the clock | `GET /clock` every second ([plugins.md](../plugins.md#the-clock)) |
| Prove every hop is connected | `make harness` | it calls the services directly |

[agent.runbooks.md](../../agent.runbooks.md) is the worked how-to: every
step of a settlement run as one call.

## Starting cold, as an agent

1. `GET http://127.0.0.1:8090/api`: every console route with its group, a
   one-line summary and what it reaches.
2. `GET http://127.0.0.1:8090/api/overview`: every service with its health,
   `browse_url` (where you reach it from the host), auth, and the endpoints
   worth knowing; plus the registered platform, if any.
3. The reference for whichever service you need, below.

## Conventions

These hold for the lab's own routes: the console, every `/sim/*` and
`/internal/*`, the clock and the stand-ins. A vendor surface answers in its
vendor's shape instead, errors included. Banking Circle's `400` body, for
example, is Banking Circle's.

- **JSON in and out.** Request bodies are decoded strictly where the code
  says so (`httputilx.ReadJSON` refuses unknown fields).
- **Errors** are `{"error": "…", "status": n}` (`httputilx.Error`), the
  message in the failing peer's own words where there is one. In the
  console, a service that refuses or cannot be reached comes back as `502`
  with the service's message; only the routes that forward a call as it is
  (a plugin's buttons and files, moving the clock, the Worldline cycle and
  channel, the settlement pull) pass the service's status and body through
  unchanged. Each route in
  [console-api.md](console-api.md) says which it does.
- **`501`, not `500`, for what the backend cannot do.** A console route that
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
  serves (design-system.md, contract 11).

## Reference

| Document | Service | Side | Listens |
| --- | --- | --- | --- |
| [console-api.md](console-api.md) | console | control panel | `:8090` |
| [services/worldline.md](services/worldline.md) | Worldline | vendor | `:8084` HTTP, `:2222` SFTP |
| [services/b4b.md](services/b4b.md) | B4B Payments | vendor | `:8086` |
| [services/banking-circle.md](services/banking-circle.md) | Banking Circle | vendor | `:8085` Connect API, `:8095` bridge |
| [services/aci.md](services/aci.md) | ACI | vendor | `:8087` |
| [services/bank.md](services/bank.md) | business bank | the far end | `:8081` |
| [services/verify.md](services/verify.md) | AML decision | verification | `:8088` |
| [services/verification.md](services/verification.md) | verification vendors | verification | `:8089` |
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

- The console registers its routes from one table,
  `cmd/console/routes.go`. `GET /api` serves that table, and the Docs view
  draws it. `cmd/console/routes_test.go` fails if a route has no summary,
  if the index and the routes served differ, or if
  [console-api.md](console-api.md) does not document exactly those routes.
- `internal/console/apidocs_test.go` fails if a service in the catalogue
  has no reference under `services/`, or if a reference and the routes
  that service's source registers differ.

To add a console route: add a row to the table in `routes.go`, add its
section to `console-api.md`, and run `go test ./cmd/console`. To add a
service route: add its section to the service's reference (and, if a
person would want it on the service's card, an `Endpoint` in
`internal/console/catalogue.go`), and run `go test ./internal/console`.
