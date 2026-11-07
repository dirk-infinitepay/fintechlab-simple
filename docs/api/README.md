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
and health, and each service's `/sim/*` hooks through one passthrough that
adds the credentials a service asks for.

- **The console** (`cmd/console`, `:8090`) is a page. Its server serves the
  page and forwards `/v1` to the lab API, so the page calls its own origin;
  it keeps no state and holds no credential.
- **Each service's own API** is behind the lab API. Your platform calls the
  vendor surfaces directly, exactly as it would call the real vendors; that
  traffic never goes through the lab API.
- **The harness** (`make harness`) calls the services directly. It needs
  neither the lab API nor the console, and neither being down stops it.

The lab API is a client of the services, never in their way: nothing in
the lab can be reached only through it, and every route has a service
endpoint behind it or is the lab API's own state (the merchant registry and
the plugin registrations).

What the lab API is still to gain — a reset of the whole lab, every
service's log lines, traps that make a vendor answer a future request the
way a test needs, and a control port per service — is the rest of phase 1
in [docs/lab-api/plan.md](../lab-api/plan.md).

## Who calls what

```
  console page ───▶ console :8090 ──(forwards /v1)──┐
  (your browser)    page, no state                  │
                                                    ▼
  scripts, agents ────────────────────────▶┌──────────────────────────────────┐
                                            │ lab API :8097                    │◀── POST /v1/plugins ──┐
                                            │   GET /v1          the index     │─── buttons, files ──▶ │
                                            │   /v1/*            the lab       │                       │
                                            │   /v1/services/{id}/sim/*  every │                your platform
                                            │                    service's hooks│                (a plugin)
                                            │   own state: merchants, plugins  │                       │
                                            └────────────────┬─────────────────┘                       │
                                                             │ calls the services' own APIs            │
                                                             ▼                                         │
                                            ┌──────────────────────────────────┐                       │
  harness ─────────────────────────────────▶│ services, one process each       │◀── vendor traffic ────┤
  (make harness)                            │   vendor surface   :808x, :2222  │─── webhooks ────────▶ │
                                            │   /internal/*      plumbing      │                       │
                                            │   /sim/*           lab control   │                       │
                                            │ clock :8096        GET /clock    │◀── follows the clock ─┘
                                            └──────────────────────────────────┘
```

| Caller | Calls | Never calls |
| --- | --- | --- |
| Console page | the lab API's `/v1`, through the console's origin | a service directly |
| Console server | the lab API, forwarding what the page sends | anything else |
| Scripts and agents | the lab API for everything lab-level; a vendor surface when testing an integration with that vendor | — |
| Lab API | each service's vendor surface, `/internal/*` and `/sim/*`; a plugin's `base_url` | — |
| Harness | services' vendor surfaces and `/sim/*` | the lab API, the console |
| Your platform | vendor surfaces; the lab API's `POST /v1/plugins`; the clock's `GET /clock` | `/sim/*` or `/internal/*` from production code: the real vendor has neither. Its test harness may use the lab API, like any script |
| One service, of another | `/internal/*` plumbing (B4B → Banking Circle's bridge, Worldline's wire → Banking Circle, Banking Circle → the business bank), webhooks and callbacks | — |

## The three surfaces of a service

| Surface | What it is | Auth | Exists at the real vendor? |
| --- | --- | --- | --- |
| **Vendor surface** | the API the vendor publishes: Worldline's SFTP and SFT channel, B4B's Oversight API, Banking Circle's Connect API, ACI's webhooks, the verification vendors | the vendor's own: SFTP keys, RS512 JWT, mTLS + Basic→Bearer, API keys | yes, and the shape matches it |
| **Lab plumbing** (`/internal/*`) | stand-ins for something with no API: money arriving by wire, a payout passed to the beneficiary's bank, B4B's bridge into Banking Circle | none, as a rule | no |
| **Lab control** (`/sim/*`) | what the lab uses to drive and observe a simulation: activity logs, seeding, running a cycle now, forcing an outcome, pausing a delivery | mixed today; the lab API's passthrough adds what each needs | no |

The swap rule follows from the table: pointing a platform at the real
vendor replaces the vendor surface. Nothing the platform calls may be
`/internal/*` or `/sim/*`.

## Which to call

| To | Call | How |
| --- | --- | --- |
| Drive the lab from a script or an agent: merchants, the clock, a run, what happened | the lab API | `GET /v1` first, then `/v1/*` |
| Make a vendor misbehave, or read what it saw | the lab API's passthrough | `/v1/services/{id}/sim/*`; the credentials are added for you |
| Test your integration with one vendor | that vendor's surface | exactly the calls your platform makes to the real one |
| Put your platform's card in the console | the lab API | `POST /v1/plugins` ([plugins.md](../plugins.md)) |
| Keep your platform's business dates on the lab's | the clock | `GET /clock` every second ([plugins.md](../plugins.md#the-clock)) |
| Prove every hop is connected | `make harness` | it calls the services directly |

[agent.runbooks.md](../../agent.runbooks.md) is the worked how-to: every
step of a settlement run as one call.

## Starting cold, as an agent

1. `GET http://127.0.0.1:8097/v1`: every route with its group, a one-line
   summary and what it reaches.
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

- The lab API registers its routes from one table, `cmd/labapi/routes.go`.
  `GET /v1` serves that table, and the Docs view draws it.
  `cmd/labapi/routes_test.go` fails if a route has no summary, if the index
  and the routes served differ (aliases included), or if
  [lab-api.md](lab-api.md) does not document exactly those routes.
- `internal/console/apidocs_test.go` fails if a service in the catalogue
  has no reference under `services/`, if a reference and the routes that
  service's source registers differ, or if a card lists a route the service
  does not serve.

To add a lab API route: add a row to the table in `routes.go`, add its
section to `lab-api.md`, and run `go test ./cmd/labapi`. To add a service
route: add its section to the service's reference (and, if a person would
want it on the service's card, an `Endpoint` in
`internal/console/catalogue.go`), and run `go test ./internal/console`. A
`/sim` hook a service gains needs no lab API change: the passthrough
reaches it.
