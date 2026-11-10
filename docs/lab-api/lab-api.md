# The lab API: `/v1`

The `lab-api` container ([architecture.md §1](architecture.md#1-what-runs)).
It is what the console UI, the runner, test frameworks and agents call.

## 1. Principles

**[PROPOSED]**

- **Resource-shaped JSON over HTTP** [DECIDED 2026-10-20: HTTP], under `/v1`.
  A breaking change is `/v2`; adding a field or a route is not.
- **One contract file.** `docs/lab-api/openapi.yaml` (OpenAPI 3.1) is
  written in phase 1, once this prose has settled, and served at
  `GET /v1/openapi.json`. Tests check the handlers against it. It is what
  an agent or a code generator reads.
- **Everything the console does, and nothing only the console does.** Every
  button in the UI is a `/v1` call, which is today's console rule
  ([console.md](../console.md)) moved one container over.
- **No platform by name** (`CLAUDE.md`). The platform arrives as a plugin
  (§12), and nothing here knows which one.
- **Errors** are `{"error": "…", "status": n}`, as everywhere in the lab.
  A sim's answer passed through (§5) keeps the sim's own status and body.
- **Long work answers `202`** and says where the outcome will appear (a
  log, a trap's `caught`, a plugin's runs), as the runner's `/sim/run` and
  `/sim/sweep` already do. There is no operations resource in v1; the
  streams (§9) are how to watch.
- **Cursors, not pages.** Lists that grow (logs, activity, events) take
  `since` (a cursor from the previous answer) and `limit`.

## 2. Index

`GET /v1`: the version, the build, the lab's services with their links,
and links to every top-level resource. An agent starting cold reads this
first, then `GET /v1/openapi.json`.

## 3. The lab

| Route | |
| --- | --- |
| `GET /v1/lab` | at a glance: services and health, the clock, the registered platform, trap counts (today's `/api/overview`, extended) |
| `POST /v1/lab/reset` | reset every sim (§4) |

## 4. Reset

**[DECIDED 2026-10-20: reset. Shape PROPOSED]**

```http
POST /v1/lab/reset
{"scope": "flow"}
```

```json
{"scope": "flow", "at": "2026-10-20T09:20:00Z", "ok": true,
 "services": [
   {"service": "worldline", "status": 200, "cleared": {"transactions": 120, "files": 2}},
   {"service": "b4b", "status": 200, "cleared": {"payments": 6}},
   {"service": "banking-circle", "status": 200, "cleared": {"payments": 13, "traps": 1}},
   {"service": "clock", "status": 200}
 ]}
```

- **Scopes** are the sims' own: `flow` (the default) or `all`
  ([sim-control.md §6](sim-control.md#6-post-simreset)). `flow` on the lab
  also keeps `lab-api`'s own merchant registry; `all` clears it. Plugin
  registrations and stand-in switches are never reset: they are live
  wiring, not run state.
- **A pre-flight check comes first.** If the clock is held (a settlement
  run is in flight), the answer is `409` with the holder's reason and
  nothing is reset. `{"force": true}` overrides.
- **Order:** producers before consumers, so nothing new arrives at a sim
  that is already clean: `worldline`, `aci`, `b4b`, `banking-circle`,
  `bank`, `verify`, `verification`, the stand-ins, then `clock`.
- **Partial failure is reported, not hidden.** `ok` is false if any sim
  failed, and each sim's own answer is in `services`.
- `{"services": ["banking-circle", "b4b"]}` resets just those.
- **The platform's side is not the lab's to reset** unless the plugin
  offers it ([OPEN 11](open.md), [runner.md §3](runner.md#3-the-plugin-contract)).

## 5. Services, and the passthrough to each sim's control surface

| Route | |
| --- | --- |
| `GET /v1/services` | every service: catalogue entry, live health, and its `/sim/info` |
| `GET /v1/services/{id}` | one |
| `POST /v1/services/{id}/probe` | probe health now |
| `GET /v1/services/{id}/activity` | its activity logs (today's `/api/services/{id}/activity`) |
| `GET /v1/services/{id}/logs` | its log lines ([sim-control.md §5](sim-control.md#5-get-simlogs)) |
| `POST /v1/services/{id}/reset` | its reset |
| `ANY /v1/services/{id}/sim/{path…}` | **passthrough:** forwarded to `{control listener}/sim/{path…}`, method, query and body as sent; the sim's status and body come back as they are |

The passthrough is how every sim-specific control is reached through one
address, traps included:

```sh
curl -s -XPOST localhost:8097/v1/services/banking-circle/sim/traps -d '{
  "hook": "payment.outgoing",
  "match": {"creditor.iban": "GB00SIM0000000000777"},
  "effect": {"path": [{"to": "OutgoingPaymentProcessed"}, {"to": "Reversed", "after": "10s"}]}}'
curl -s localhost:8097/v1/services/banking-circle/sim/traps/trp_0001
curl -s -XPOST localhost:8097/v1/services/worldline/sim/settlement-cycle/run -d '{"slot":"morning"}'
```

A sim that gains a hook needs no `lab-api` change. Each sim's control
surface is documented by the sim itself (`GET /sim/traps/hooks`,
[sim-control.md](sim-control.md)). `lab-api` adds the control token and
nothing else.

## 6. Traps across the lab

| Route | |
| --- | --- |
| `GET /v1/traps?state=armed` | every sim's traps, each with `service`, newest first |
| `GET /v1/traps/hooks` | every sim's hooks catalogue, each with `service` |
| `DELETE /v1/traps` | disarm every trap in the lab |

You arm a trap at the sim, through the passthrough (§5). There is no lab-wide
`POST`: a trap belongs to the sim that applies it
([architecture.md §3](architecture.md#3-the-drop-in-rule)).

## 7. Logs across the lab

**[DECIDED 2026-10-20: read logs. Shape PROPOSED]**

```http
GET /v1/logs?services=banking-circle,b4b&q=bcp_7f3a&since=<cursor>&limit=200
```

```json
{"lines": [
   {"service": "b4b", "seq": 911, "at": "2026-10-20T09:14:01.870Z", "text": "b4b: banking circle bridge call for payment … ok, bcPaymentId=bcp_7f3a…"},
   {"service": "banking-circle", "seq": 1841, "at": "2026-10-20T09:14:02.114Z", "text": "…"}
 ],
 "next": "<cursor>",
 "dropped": {"banking-circle": 0, "b4b": 0}}
```

Lines from several sims are merged by `at`. The cursor is opaque and holds
one `seq` per sim. `services` omitted means every sim. A sim that does not
answer is listed under `unreachable` and does not stop the rest.

## 8. The clock

| Route | Forwards to |
| --- | --- |
| `GET /v1/clock` | `GET clock:8096/clock` |
| `POST /v1/clock` | `POST /clock`: `{"at"}`, `{"advance"}` or `{"mode"}` |
| `POST /v1/clock/holds` | `POST /clock/holds` |
| `DELETE /v1/clock/holds/{holder}` | `DELETE /clock/holds/{holder}` |

The shapes are the clock service's own ([plugins.md](../plugins.md#the-clock)).
`lab-api` adds one refusal the console makes today: no move while a
platform registered on the wall clock is live.

## 9. Streams (SSE)

**[PROPOSED, phase 2]**

| Route | Events |
| --- | --- |
| `GET /v1/events?services=…` | `activity`: each new activity event from any sim, as `{service, log, event}`; `trap`: each trap catch and step |
| `GET /v1/logs/stream?services=…&q=…` | `log`: each new log line |

Each event's `id` is the cursor, so a client that reconnects with
`Last-Event-ID` misses nothing the rings still hold. `lab-api` builds these
by reading each sim's `seq` cursors once a second; nothing is pushed by a
sim ([architecture.md §5](architecture.md#5-the-back-link)).

## 10. What moves over from the console

**[PROPOSED]** Every route the console serves today keeps its shape and
moves under `/v1`. The console's `/api/*` becomes a proxy to these until
[phase 4](plan.md#phase-4--retire-the-aliases).

| Today (`:8090`) | `/v1` |
| --- | --- |
| `GET /api/overview` | `GET /v1/lab` |
| `POST /api/services/{id}/probe`, `GET /api/services/{id}/activity` | `/v1/services/{id}/probe`, `/v1/services/{id}/activity` |
| `GET /api/flow`, `GET /api/flow/hops/{id}` | `/v1/flow`, `/v1/flow/hops/{id}` |
| `GET, POST /api/clock` | `/v1/clock` (§8) |
| `GET, POST /api/merchants`, `DELETE /api/merchants/{id}`, `POST /api/merchants/{id}/outlets`, `/status`, `/provision`, `/trading` | `/v1/merchants…`, the same |
| `POST /api/merchants/batches`, `DELETE …/{id}`, `POST …/{id}/provision`, `…/trading` | `/v1/merchants/batches…`, the same |
| `POST /api/outlets/{mid}/sanctions`, `POST /api/partners` | `/v1/outlets/{mid}/sanctions`, `/v1/partners` |
| `GET /api/banks`, `POST /api/banks/{id}/accounts` | `/v1/banks…` |
| `GET /api/banking-circle/subscriptions`, `POST …/{id}/test`, `/pause`, `/resume` | `/v1/banking-circle/subscriptions…`, the same |
| `GET /api/banking-circle/payouts`, `POST …/{id}/return`, `/reverse` | `/v1/banking-circle/payouts…`, the same |
| `POST /api/actions/worldline-cycle`, `/settlement-pull`, `/fund-sga` | `/v1/actions/…`, the same; also reachable by passthrough |
| `GET /api/config`, `PUT /api/config/worldline-channel` | `/v1/config…` |
| `GET /api/connect`, `/env`, `/env/{service}`, `/files/{service}/{name}` | `/v1/connect…` |
| `GET /api/stand-ins`, `POST /api/stand-ins/{id}` | `/v1/stand-ins…` |
| `GET, POST /api/plugins`, `DELETE /api/plugins/{id}`, and every `/api/plugins/{id}/…` | `/v1/plugins…`, the same (§12) |

New in v1: `/v1/lab/reset`, `/v1/services/{id}/logs`,
`/v1/services/{id}/reset`, `/v1/services/{id}/sim/…`, `/v1/traps…`,
`/v1/logs`, `/v1/openapi.json`; later `/v1/events` and `/v1/logs/stream`.

Two things today's runbook sends an agent around the console for, which
`/v1` covers: Banking Circle's outcome queue (`curl :8095/internal/…`) is
now `/v1/services/banking-circle/sim/payments/outcomes` or a trap; and
verification's `/sim/outcome` (`:8089`) is
`/v1/services/verification/sim/outcome`.

## 11. Agents

**[PROPOSED]**

- **The OpenAPI file is the contract** (§1). Every operation has a
  one-sentence summary written for someone who has not read this
  directory, because that is what a tool-calling agent sees.
- **An MCP endpoint**, `/v1/mcp` (streamable HTTP), in phase 3. A thin layer
  over the same handlers, with a curated set of tools rather than one per
  route: read the lab, move the clock, reset, read logs, list hooks, arm
  and read traps, create merchants, run a plugin action. It is never a
  second implementation of anything.
- `agent.runbooks.md` is rewritten against `/v1` once phase 2 lands.

## 12. Plugins

Unchanged in shape: [plugins.md](../plugins.md) moves its paths from
`/api/plugins` to `/v1/plugins`, and `lab-api` keeps everything the console
does today: registration, renewal, forwarding of actions, run and files to
the platform's `base_url`, and the stand-ins wishes. Additions are in
[runner.md §3](runner.md#3-the-plugin-contract).

## 13. Not in v1

**[PROPOSED]**

- **Composite calls.** A composite is one `lab-api` call that runs several
  of the above in a sequence on the caller's behalf. "Prepare a business
  morning" would move the clock, create fifty merchants, register them at
  B4B, seed their trading, run Worldline's morning cycle and fund the
  safeguarding accounts, then answer once. It saves a client six calls,
  but it puts a scenario's choices inside the lab. Proposed: none in v1.
  A test, an agent or a runner helper makes the six calls itself
  ([OPEN 10](open.md)).
- **Container control** [DECIDED 2026-10-20]: no restart, no
  `podman logs`, no socket. Logs and reset are in-process (§4, §7).
- **Environments**: several platforms on one lab, each seeing only its own
  data. Designed in [environments.md](environments.md) and built in
  [plan.md, phase 5](plan.md#phase-5--environments).
