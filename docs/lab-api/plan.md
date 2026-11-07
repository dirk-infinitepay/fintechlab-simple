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

## Phase 5 — parallel environments (deferred)

**[DECIDED 2026-10-20: in the plan, deferred.]** The lab is one
long-running tenant for local development until this phase starts.

The goal: several isolated runs side by side, each with its own copy of
the platform and its settle DB, created through the API
(`POST /v1/environments`, `DELETE /v1/environments/{id}`).

What it takes, as far as is known now:

| Piece | Today | Per environment |
| --- | --- | --- |
| Platform + settle DB | one, the developer's | one copy each: the platform's job; each registers its own plugin |
| B4B | already multi-company | likely enough to give each environment its own test companies (the owner's view); the JWT key is shared |
| Banking Circle | one set of safeguarding accounts (`BC_SAFEGUARDING_ACCOUNT_ID_*`), shared subscriptions | its own safeguarding accounts and subscriptions; reports filtered to its accounts. Separate companies may be most of it |
| Worldline | one identifier, one SFTP user and root, one transaction store: the naive one | its own SFTP user, root and store |
| Clock | one offset for the whole lab | **the hard part.** Either environments share one clock that none may move, or every vendor stamps time per environment |
| Traps, logs, activity | per sim | tagged with the environment, and filtered by it |

The alternative to weigh when the phase starts: one whole lab per
environment (a pod per test run). That needs no vendor changes, at the
cost of more containers.
