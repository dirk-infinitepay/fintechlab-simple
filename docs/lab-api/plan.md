# Plan

**[PROPOSED]**, except where a phase says otherwise. Each phase ends with
the lab working end to end, and `make e2e` green.

## Phase 1 — the control contract, lab-api, first traps

**Status, 2026-11-07.** Built:

- the `lab-api` block below, apart from `/v1/traps` and `openapi.yaml`, and
  its wiring into `compose.yml` and the pod;
- `internal/logring` and `internal/simctl` (info, logs, reset with scopes,
  the 409 and `force`, the epoch, the control listener); every sim serves
  its `/sim/*` routes, `/sim/info`, `/sim/logs` and `/sim/reset` among
  them, on its control port, with the old paths kept as aliases;
- `POST /v1/lab/reset`, `GET /v1/logs`, `GET /v1/services/{id}/logs` and
  `POST /v1/services/{id}/reset`;
- `internal/trap` (store, matching, the path validated against a hook's
  graph, steps in real or lab time, cancelling on disarm and reset,
  standing traps, the `traps` activity log, the `/sim/traps` routes);
  Banking Circle's `payment.outgoing`, with the outcome queue rebuilt as a
  shortcut over it; B4B's `payment.create`, with
  `B4B_FORCE_FAILURE_BENEFICIARY_IDS` as standing traps; `/v1/traps`,
  `/v1/traps/hooks` and `DELETE /v1/traps`;
- the e2e scenarios for the trap paths and for a reset mid-payout
  (`E2E_ONLY` runs a few);
- `openapi.yaml`, generated from the route table and an annotation of
  what each route takes, served at `GET /v1/openapi.json` and checked in
  as [docs/api/openapi.yaml](../api/openapi.yaml) (next to the reference,
  not here), held to the routes by tests and valid against the OpenAPI 3.1
  meta-schema.

Choices made on the way:

- **The old `/api` paths are aliases in the lab API's route table**, not a
  path-mapping proxy in the console. The console forwards `/v1` and `/api`
  untouched, so it holds no knowledge of the API, and phase 4 retires the
  aliases by deleting one column.
- **The console page calls `/v1` already**, pulled forward from phase 2:
  every route kept its shape, so it was a rename, and it is what makes the
  console and an agent call the lab the same way. [OPEN 3](open.md) is
  answered by the proposal: a separate `console` container, so `lab-api`
  stays headless.
- **No control token, ever** (the owner, 2026-11-07). The control port is
  trusted by network placement, with `SIM_CONTROL_ALLOW` to narrow it;
  authentication is only between a person, an agent or the console and the
  lab API ([architecture.md §4, §8](architecture.md#4-the-control-listener)).
  The lab API holds no vendor credentials, and reaches a sim only on its
  control port.
- **What each reset clears is in each service's reference**
  (`docs/api/services/<id>.md`, *Lab control*), which follows the table in
  [sim-control.md §6](sim-control.md#6-post-simreset) with these
  differences: verification and notifier keep their tokens and their
  subscription on `flow` (set up once, like Banking Circle's); Worldline's
  `flow` also clears armed enrolment rejections (a trap in all but name);
  settlement's `all` is its `flow`, and after a reset its puller skips
  Worldline files older than the reset, so a settlement reset alone never
  pays a file twice; the business bank's `flow` also drops accounts opened
  by hand. [OPEN 1](open.md) and [OPEN 12](open.md) are answered by the
  proposals. [OPEN 13](open.md) is answered by the owner: an `all` reset
  keeps Banking Circle's issued tokens, so a lab reset never logs the
  platform out.
- **Log rings leave out `GET /health` and `GET /sim/logs`**, so watching the
  lab does not fill them.

Traps took the proposals of [OPEN 5, 6, 7, 9](open.md): a trap takes one
event unless `times` says otherwise, `after` is real time and `after_lab`
the lab clock's, the vendor's own rule wins when its books cannot follow a
step (`trap.diverted`), and disarming cancels pending steps. B4B walks a
step to a state further along as one step per hop, so `pending_steps`
shows the next hop and a disarm stops at it. [OPEN 8](open.md) is not
decided: `MissingFunding` stays final.

**Phase 1 is done.** Follow-ups it leaves: four services each carry their
own reset guard (b4b's `Gate`, aci's `SentLog`, worldline's and
settlement's `runGuard`), and one in `internal/simctl` should replace them;
a walking graph's hops are expanded by b4b itself, where `internal/trap`
could do it for every sim.

**After phase 1, built 2026-11-07:** the e2e suite steers the lab through
the lab API alone (`LAB_API_URL`, `LAB_API_TOKEN`) and plays the platform
on the vendor surfaces; it calls no old `/sim` path, no control port and
no `/internal/*`, so nothing in it stands in phase 4's way. The control
ports stay unpublished ([OPEN 2](open.md), decided). Compose and the pod
hand `LAB_API_TOKEN` to `lab-api` and `console` alike: from the host's
environment, and in the pod from an optional ConfigMap given at play time.

Shared packages, generic infra like `internal/activity`:

- `internal/logring`: the tee on `log.SetOutput`, the ring, and the
  `GET /sim/logs` handler.
- `internal/simctl`: the control listener (`SIM_CONTROL_LISTEN`), the token
  (`SIM_CONTROL_TOKEN`), `GET /sim/info`, and the reset plumbing (scopes,
  epochs, `409`).
- `internal/trap`: the store, matching, the path validator against a graph,
  the step scheduler (real and lab time), and the `traps` activity log.

Every sim:

- serves `/sim/info`, `/sim/logs` and `/sim/reset` on its control listener;
- moves its existing control hooks there, with the old paths kept as
  aliases ([sim-control.md §7](sim-control.md#7-todays-control-hooks-moved)).

First traps, the owner's case:

- banking-circle `payment.outgoing`, with the outcome queue rebuilt as a
  shortcut over it;
- b4b `payment.create`, with `B4B_FORCE_FAILURE_BENEFICIARY_IDS` as standing
  traps.

`lab-api`:

- a new `cmd/labapi`. The console's handlers move there (the domain code in
  `internal/console` stays where it is, and is renamed only if that helps);
- `/v1` as in [lab-api.md](lab-api.md): everything from the console, plus
  the passthrough, `/v1/lab/reset`, `/v1/logs` and `/v1/traps`;
- `LAB_API_TOKEN`;
- `docs/lab-api/openapi.yaml`, with a test that the routes and the file
  agree;
- the console's Go server proxies `/api/*` to `/v1/*`, so the UI and every
  registered plugin keep working untouched.

Wiring and proof:

- `compose.yml` and the pod manifest gain `lab-api` and the control ports,
  and `deploy/pod/manifest_test.go` follows;
- the e2e suite gains a scenario for each trap path, and one for reset (a
  reset in the middle of a payout leaves no stray notification);
- [plugins.md](../plugins.md), [console.md](../console.md) and
  [agent.runbooks.md](../../agent.runbooks.md) say where things moved.

**Done when** an agent given only `http://<host>:8097` can reset the lab,
read every sim's logs, arm the owner's two Banking Circle traps, and see
them fire.

## Phase 2 — streams and the console on /v1

- SSE: `/v1/events`, `/v1/logs/stream`, `/sim/logs/stream`.
- Delivery traps: banking-circle `notification.deliver`, b4b
  `callback.deliver`.
- worldline `settlement.cycle` and `enrolment.batch`; verify `aml.decision`;
  verification `check`.
- The console UI calls `/v1` itself, and the console becomes a static page
  ([OPEN 3](open.md)). A *Traps* panel on each vendor card and the trap mark
  on activity rows go into [design-system.md](../design-system.md) first.
- The runner: `lab_url`, the token, `LabClient`, and `reset_path` /
  `logs_path` if [OPEN 11](open.md) says yes ([runner.md](runner.md)).
  *Built 2026-11-07:* `lab_url`, the token and a first `LabClient`; buddy's
  runner registers, follows the clock and lands its wires through the lab
  API. Phase 4's precondition below is met.
- The clock's long poll (`GET /clock?wait=`), if a second of lag is shown
  to matter.

## Phase 3 — more hooks, and agents

- banking-circle `payment.incoming` and `report.read`; aci
  `webhook.deliver`; `http.request` on every sim ([OPEN 15](open.md)).
- `/v1/mcp` ([lab-api.md §11](lab-api.md#11-agents)).
- `GET /sim/state`, if [OPEN 16](open.md) wants it.

## Phase 4 — retire the aliases

Only once the reference plugin (buddy's runner) is on `lab_url`:

- the console's `/api/*` proxy;
- Banking Circle's control hooks on the `:8095` bridge and `/sim/*` on
  `:8085`; B4B's JWT on `/sim/*`; ACI's `/internal/*`;
- `console_url` and `clock_url` in the runner.

## Phase 5 — environments

**[DECIDED 2026-11-10, the owner]:** one lab serves several platforms, each
one an *environment* with its own token and its own customers at the
vendors, and no heartbeat. The design is [environments.md](environments.md).
It replaces this phase's earlier sketch, which was deferred on 2026-10-20.
That sketch also had one fact wrong: Banking Circle's safeguarding accounts
are constants in the sim, not `BC_SAFEGUARDING_ACCOUNT_ID_*`, which only
Worldline and settlement read.

**Status, 2026-11-10: built**, all of 5.1–5.6, in one pass rather than
step by step. Where the build differs from the steps below is listed at
the top of [environments.md](environments.md). The e2e suite gained
`environments-keep-platforms-apart`, and its other scenarios stay in the
default environment.

Each step ends with the lab working end to end, `make e2e` green, and its
routes and documents changed together (`docs/api/lab-api.md`,
`docs/api/services/<id>.md`).

### 5.1 Environments in the lab API

- The lab DB: SQLite on the lab API's volume (a pure-Go driver, so the
  images stay static). It holds the environments, their tokens, their
  customers and kits, and the merchant registry moved in from
  `registry.json`.
- `/v1/environments` ([environments.md §5](environments.md#5-creating-an-environment)).
  Environment tokens, with the admin token chosen by `Lab-Environment`.
  `default` owns today's state.
- The scoping in [§6](environments.md#6-scoping-the-lab-api). A sim says it
  supports environments in `/sim/info`. Until one does, an environment
  token is refused at that sim, and the console says it is shared.
- The console: the selector in `design-system.md` first, then in the
  header, and the Environments page.

**Done when** an admin creates `sbx`, gets its token and kit, and the
console switches between `sbx` and `default`. The kit holds `default`'s
values until the vendors split, and the page says so.

### 5.2 Banking Circle

- Customers pushed with `PUT /sim/customers/{env}`, and pushed again when
  the sim's epoch changes.
- Tokens that record their customer, safeguarding accounts per
  environment, and payouts, subscriptions, notifications and reports that
  are the owner's.
- Calls on the `:8095` bridge say whose they are.
- `fund-sga`, traps, activity and reset scoped by environment.
- From here on, **the e2e suite runs in an environment of its own**,
  created for the run and deleted after it.

**Done when** two environments pay out at the same time, each sees only
its own payouts and notifications, and their safeguarding balances stay
apart.

### 5.3 B4B

- A key id and key pair per environment.
- Companies, beneficiaries and payments are owned, and `external_ref` is
  unique per customer.
- Callbacks go to the environment's hosts.
- `state.json` gains owners, and records without one are `default`'s.

### 5.4 Worldline

- SFTP users jailed at `{root}/{env}`.
- Transactions, cycles, lump sums and enrolment owned by an environment.
- One file per currency per environment, under its own
  `settlement_identifier`.
- MIDs that take the environment into account.

**Done when** sbx can have an environment of its own on the cloud lab
([environments.md §12](environments.md#12-what-this-means-for-sbx)).

### 5.5 The rest

- verification, verify and ACI per environment.
- The business bank tagged by environment.
- The stand-ins confined to `default`.

### 5.6 Plugins become optional

- Environment status comes from vendor traffic.
- A plugin registers under its environment's token, only for its buttons,
  files and logs.
- `stand_ins` applies only to `default`.
