# Sim control: the contract every simulation serves

The back link. Each simulation serves it on its control listener
([architecture.md §4](architecture.md#4-the-control-listener)).
`lab-api` is its main caller, but it is a plain HTTP surface, and a sim run
alone answers it the same way. The vendor surface never changes for it
([architecture.md §3](architecture.md#3-the-drop-in-rule)).

## 1. Who serves it

**[PROPOSED]**

Every service in `compose.yml` except `ca` (a one-shot script) and
`console`:

- **vendors**: `worldline`, `banking-circle`, `b4b`, `aci`, `verify`,
  `verification`
- **lab**: `clock`
- **stand-ins**: `bank`, `settlement`, `receiver`, `payment-api`,
  `notifier`

The generic parts (the log ring, the trap matcher and scheduler, the reset
plumbing) go in shared `internal/` packages, as `internal/activity` already
does. What a vendor's hooks mean, which states it can reach and what a
reset clears stay in that vendor's own package. A simulation's behaviour
lives in the vendor's package (`CLAUDE.md`).

## 2. Conventions

**[PROPOSED]**

- JSON in and out. Lab-owned shapes are `snake_case`, like the activity and
  plugin shapes today. Vendor shapes are never reused here.
- Errors are `{"error": "…", "status": 400}`, which is what
  `httputilx.Error` writes now. A refusal says why, in words an operator
  can act on.
- Unknown request fields are refused (`400`), as `httputilx.ReadJSON`
  already does. A typo in a trap must not arm a different trap.
- Times are RFC 3339 UTC. `at` is the wall clock. `lab_at` is the lab clock,
  given wherever the two can differ.
- `401` without the control token when `SIM_CONTROL_TOKEN` is set.

## 3. Endpoints

| Route | Required | What |
| --- | --- | --- |
| `GET /sim/info` | every sim | who this is, what it simulates, what it can do (§4) |
| `GET /sim/activity` | every sim that keeps a log | unchanged: the ring of structured events (`internal/activity`) |
| `GET /sim/logs` | every sim | the process's own log lines (§5) |
| `POST /sim/reset` | every sim | put the sim back to a known state (§6) |
| `GET /sim/traps/hooks` | sims with hooks | the trap catalogue ([traps.md §8](traps.md#8-endpoints)) |
| `GET, POST, DELETE /sim/traps` | sims with hooks | list, arm, disarm all |
| `GET, DELETE /sim/traps/{id}` | sims with hooks | one trap |
| vendor-specific `/sim/*` | — | the hooks that exist today, moved here (§7) |

## 4. `GET /sim/info`

**[PROPOSED]**

```json
{
  "service": "banking-circle",
  "kind": "vendor",
  "simulates": "Banking Circle Connect",
  "build": "3f2a91c",
  "started_at": "2026-10-20T08:00:01Z",
  "vendor_listeners": [":8085", ":8095"],
  "clock": {"follows": "http://clock:8096/clock", "offset_ms": -225000000},
  "hooks": ["payment.outgoing", "payment.incoming", "notification.deliver", "http.request"],
  "reset_scopes": ["flow", "all"],
  "traps": {"armed": 2, "pending_steps": 1}
}
```

`lab-api` builds its service list from this rather than from a hard-coded
catalogue, so a sim that adds a hook needs no `lab-api` release.

## 5. `GET /sim/logs`

**[DECIDED 2026-10-20: read logs; shape PROPOSED]**

The process's own log output: what `podman logs` shows today, kept in a
ring of the last `SIM_LOG_RING` lines (default 2000). Every sim logs
through the standard `log` package, so this is one shared tee on
`log.SetOutput`. Nothing else changes, and stdout carries the same lines
as before.

```http
GET /sim/logs?since=1840&limit=200&q=bcp_7f3a
```

```json
{
  "service": "banking-circle",
  "lines": [
    {"seq": 1841, "at": "2026-10-20T09:14:02.114Z", "text": "banking-circle: next 1 outgoing payment(s) will end [Rejected]"}
  ],
  "next": 1842,
  "dropped": 0
}
```

| Parameter | |
| --- | --- |
| `since` | lines after this `seq`; omitted = the newest `limit` |
| `limit` | 1–1000, default 200 |
| `q` | substring filter, case-insensitive |

`dropped` counts lines between `since` and the oldest line still held. It
is non-zero when the reader fell behind the ring, so a gap is never silent.
There are no levels, because the services log without them today. Like
activity, this is a window onto a run, not a record of one.

The stream form, `GET /sim/logs/stream` (Server-Sent Events, one event per
line), comes in [phase 2](plan.md#phase-2--streams-and-the-console-on-v1).

## 6. `POST /sim/reset`

**[DECIDED 2026-10-20: reset; shape PROPOSED]**

```http
POST /sim/reset
{"scope": "flow"}
```

```json
{"service": "banking-circle", "scope": "flow",
 "cleared": {"payments": 42, "traps": 1, "pending_notifications": 0},
 "kept": ["subscriptions", "accounts"],
 "at": "2026-10-20T09:20:00Z"}
```

**Two scopes:**

- **`flow`** (the default) clears what a run makes: payments, files,
  callbacks, notifications in flight, activity, traps. It keeps what a
  platform set up once and would otherwise have to set up again: Banking
  Circle subscriptions, B4B boarding, Worldline enrolment. That is the
  difference from a lab restart, which today loses Banking Circle's
  subscriptions and forces a runner restart
  ([agent.runbooks.md](../../agent.runbooks.md#lifecycle)).
- **`all`** puts the sim back as it was on first start.

**Rules:**

- Synchronous: the answer comes when the reset is done.
- In-flight work is cancelled, not raced. Each engine carries an epoch
  that reset bumps. A goroutine still sleeping out `PROCESSING_DELAY`
  checks the epoch before it applies a transition, and drops it if the
  epoch has moved. A reset must never be followed by a stray
  `OutgoingPaymentProcessed` for a payment that no longer exists.
- Traps are disarmed and their pending steps cancelled.
- Activity is cleared, so the Dashboard's diagram starts from zero. Logs
  are **kept**, with one line marking the reset, because the log is where
  you look for what happened just before it ([OPEN 12](open.md)).
- `409` with the reason when the sim cannot reset safely right now: for
  example the clock while a hold is in place (a settlement run is in
  flight). `{"force": true}` overrides.

**What each scope clears** ([OPEN 1](open.md): edit this table):

| Sim | `flow` clears | `flow` keeps | `all` also clears |
| --- | --- | --- | --- |
| `banking-circle` | payments (outgoing, incoming, returns), balances back to zero, queued/paused/retrying notifications, emails, the outcome queue, traps, activity | subscriptions and their state; accounts; issued tokens | subscriptions back to the one seeded; tokens ([OPEN 13](open.md)) |
| `b4b` | payments, callbacks in flight, traps, activity | companies, addresses, people, extended profiles, documents, vIBANs, beneficiaries, and their sanctions and status settings | everything in `state.json` |
| `worldline` | acquired transactions, cycle runs, published settlement files and their archive, lump sums, traps, activity | enrolment receipts (issued MIDs), the SFT channel config | enrolment state, the SFT channel tree back to its seed |
| `aci` | sent log, retries in flight, traps, activity | the connected switch | — |
| `verify` | traps | — | — |
| `verification` | traps; outcomes back to defaults | — | — |
| `clock` | holds (refused while held, without `force`), the change log; back to `CLOCK_START` | calendars | — |
| `bank` | transfers, accounts opened by payouts, seeded balances back to their seed | the seeded accounts | — |
| `settlement` | settlement state, archive, reports | its connected switch | — |
| `receiver` | stored events and raw captures | — | — |
| `payment-api`, `notifier` | payments, deliveries | — | — |

A sim run alone resets exactly the same way.

## 7. Today's control hooks, moved

**[PROPOSED]**

These routes exist today. They move to the control listener under `/sim/`,
and the old paths stay as aliases until
[phase 4](plan.md#phase-4--retire-the-aliases). Where a trap can do the
same job, the hook stays as a shortcut that arms one
([traps.md §9](traps.md#9-hooks-by-vendor)).

| Sim | Today | On the control listener |
| --- | --- | --- |
| banking-circle | `POST :8095/internal/payments/outcomes`, `GET` the same | `POST /sim/payments/outcomes`: shortcut, arms catch-next traps |
| banking-circle | `POST :8095/internal/payments/{id}/return`, `/reverse`; `POST :8085/sim/payments/{id}/return`, `/reverse` | `POST /sim/payments/{id}/return`, `/reverse` |
| banking-circle | `:8085/sim/subscription/{id}/pause`, `/resume`, `/pending`, `/notifications`; `/sim/emails`; `/sim/delivery-config` | the same paths; no bearer token needed |
| banking-circle | `GET :8085/payments`, `/accounts`, `/reconciliation` (debug reads) | `GET /sim/payments`, `/sim/accounts` |
| b4b | `PUT /sim/beneficiaries/{id}/sanctions`, `/status`; `/sim/companies/{id}/sanctions`; `/sim/people/{id}/sanctions` (JWT) | the same paths; no JWT |
| b4b | env `B4B_FORCE_FAILURE_BENEFICIARY_IDS` | kept; each id becomes a standing trap at start-up |
| worldline | `/sim/transactions`, `/sim/settlement-cycle/run`, `/runs`, `/sim/enrolment`, `/process`, `/reject` | the same paths; `/sim/enrolment/reject` arms a trap |
| aci | `POST /internal/simulate-payment`, `GET /internal/sent` | `POST /sim/payments`, `GET /sim/sent` |
| verify | env `VERIFY_FORCE_DECLINE_IDS` | kept; standing traps at start-up |
| verification | `GET, POST /sim/outcome` | the same path |
| settlement, aci | `GET, POST /sim/connected` (the stand-in switch) | the same path |

A trigger (do something now: run a cycle, return a payout, simulate a card
payment) stays a plain route. A trap (decide something about a future
event) goes through `/sim/traps`. A setter (put a standing value in place:
sanctions, a verification outcome, a subscription paused) stays a plain
route too. [traps.md §1](traps.md#1-what-a-trap-is) explains the line
between them.

## 8. Environments

**[DECIDED 2026-11-10, built]** — the sim side of
[environments.md](environments.md). A sim that serves more than one
platform keeps a customer per environment (`internal/tenant`) and gives
`simctl.Config` its `Customers`; one that does not is the default
environment's alone and refuses a reset of any other.

| Route | |
| --- | --- |
| `GET /sim/customers` | every customer, the default first, with `created_at` and `last_seen` (the last vendor call it made) |
| `PUT /sim/customers/{env}` | add or replace one: a `tenant.Customer`, the logins its platform will present. The lab API pushes every customer again when the sim restarts |
| `DELETE /sim/customers/{env}` | remove one, and everything it made |

The rules every such sim keeps:

1. **The vendor's own login says whose a call is.** The vendor's auth
   finds the customer (Banking Circle's Basic credential, B4B's `kid`,
   the SFTP user), puts its environment in the request's context
   (`tenant.With`), and records it as seen. A login no customer has is the
   default environment's, as every login was before. A platform never
   sends a lab token to a vendor.
2. **Every record has an owner**, set from that context: an account, a
   payment, a subscription, a company, a file. A record from before
   environments is the default's.
3. **The vendor surface shows a caller only its own**, and fans out only
   to its own listeners: notifications, callbacks, webhooks. A
   destination is allowed if the sim's `WEBHOOK_ALLOWLIST` or the
   customer's `webhook_hosts` names it.
4. **Lab hooks act in the environment the call names**:
   `tenant.FromRequest` (`?env=`, else the `Lab-Environment` header, else
   the default). Reads filter with `tenant.InScope`; `*` reads every
   environment; a hook that creates something creates it in the named
   environment and refuses `*`.
5. **Activity events and trap events carry `env`.** `Log.Watch` records
   whoever the auth found; an event recorded by hand sets `Env` itself.
6. **A reset names an environment** (`env` in the body, or the query): it
   clears that environment's records only, `*` clears everything.
7. **Calls between sims say whose they are**, in a `Lab-Environment`
   header: B4B paying out over Banking Circle's bridge, Banking Circle
   crediting the business bank.
