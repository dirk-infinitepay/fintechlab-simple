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
  *Built 2026-11-12:* `http.request` and the `respond` effect on
  verification, so Verify's suite can run vendor outages, errors and
  unreadable replies. The other sims can offer it the same way.
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

## Phase 6 — the platform card is the environment's

**[DECIDED 2026-11-10, the owner]**: every environment has a platform
card, drawn by the lab from the environment itself rather than registered
by a runner, and its buttons are lab API calls that the console, a script
and an agent make alike. Running settlement replicates the real morning:
Worldline drops the file in the platform's SFTP folder, and the platform's
own poll picks it up. The trigger lives where that poll lives, the gateway.
**Status, 2026-11-10: building.**

### How a deployed platform settles today (lightyear, sbx)

1. **The gateway polls Worldline**
   (`apps/gateway/src/modules/worldline-downloader`). It runs hourly in a
   morning window (08–10 GMT+2, the ER file) and an afternoon one (14–16,
   AR), each with a cut-off run that alerts on a missing currency. It lists
   and downloads through acquirer-fts (SFTP), and writes each new file to
   the settle-incoming bucket, `worldline/` (or `worldline-invalid/`).
2. **The write starts the run.** An `s3:ObjectCreated` notification on
   `worldline/` invokes settle-processing (`processing-s3.lambda.ts`). The
   11:00 UTC rule is a separate batch check, not a trigger.
3. In sbx the poll was off: `WORLDLINE_DOWNLOADER_CRON_ENABLED` was the
   literal `false` in the gateway's `deploy.yaml`.

So *run now* is *poll now*: settlement already starts when a file lands in
S3, and the waiting is the poll window.

### The card

For the environment the console shows, with or without a runner:

- **Its settlement files**, kept by the lab per environment on its data
  volume (`<lab data>/files/<env>/`, beside `lab.db`): generated from the
  environment's own merchants and trading, or uploaded from anywhere.
  Each is listed with its currency, MIDs and total, read with Worldline's
  own parser; a file the parser refuses is listed with why.
- **Run**: tick one or two files, at most one per currency (a platform
  settles two currencies at once, never two files of one), and the lab has
  Worldline publish them: encrypted and signed as its own, in the
  environment's SFTP download folder, with the morning lump sum wired to the
  environment's safeguarding account when asked. The platform's next poll
  picks them up.
- **Poll now**: the lab calls the poll the environment's config names, and
  shows the platform's answer. The run starts at once instead of at the
  next poll.
- A registered card (buddy's runner) still adds its own buttons, run log
  and stages, as phase 5.6 has it; the environment's card does not need one.

### The lab API

| Route | |
| --- | --- |
| `GET /v1/settlement-files` | the environment's files: name, bytes, source (`generated`, `uploaded`), currency, MIDs, total, or the parser's `problem` |
| `POST /v1/settlement-files?name=` | upload one (raw bytes, `CONSOLE_UPLOAD_MAX_MB`) |
| `POST /v1/settlement-files/generate` | `{"currency", "date", "slot"}`: what Worldline would cut from the environment's trading, kept as a file, not published |
| `GET /v1/settlement-files/preview?name=&lines=` | its first lines |
| `DELETE /v1/settlement-files?name=` | delete one |
| `POST /v1/settlement-files/publish` | `{"files": [...], "wire": true}`: Worldline publishes them in the environment's SFTP folder; `409` for two files of one currency |
| `POST /v1/platform/poll` | `{"fileType": "ER"\|"AR"}`: `POST` to the environment's `poll_url` with `Authorization: Bearer <poll_token>`; the platform's answer, verbatim |

The environment's config gains `poll_url` and `poll_token`. The default
environment's config can now be set too (`PATCH /v1/environments/default`,
`config` only, kept in the lab DB), so a lab with no other environment has
a card that polls.

### Worldline

Two lab hooks on its control port, in the environment the call names:

- `POST /sim/settlement-files/render` `{"currency", "date", "slot"}`: the
  file the cycle would cut from that environment's transactions, as plain
  text with its name, currency, MIDs and total; `404` when nothing settles.
- `POST /sim/settlement-files/publish?name=&wire=` with the plain text: the
  file parsed (`400` if it is not a settlement file), encrypted and signed,
  and put in the environment's download folder under the cycle's own naming
  (`<name>.pgp`). With `wire=true` (an ER file), the lump sum (the file's
  total) is wired to the environment's safeguarding account, as the
  morning cycle does.

### The platform (lightyear and AWS-Cloud)

- **The gateway's poll becomes config.** `WORLDLINE_DOWNLOADER_CRON_ENABLED:
  ${config?:WORLDLINE_DOWNLOADER_CRON_ENABLED}` (absent: off, as before), on
  in sbx's lab mode.
- **`POST /api/v1/worldline-downloader/poll` `{"fileType": "ER"|"AR"}`** on
  the gateway runs exactly the job the cron runs, now, whether the cron is
  on or not. It is `404` unless `WORLDLINE_DOWNLOADER_TRIGGER_TOKEN` is set
  (a secret, optional), and wants `Authorization: Bearer` that token, so a
  production gateway never offers it. The answer says what it found,
  downloaded and skipped, and which currencies are still missing.
- **The deploy tool gains optional secrets**, `secret?:<path>`, like
  `${config?:KEY}`: a parameter that does not exist is left out instead of
  failing the deploy, so one environment can carry the trigger token and
  the rest need not.
- **sbx**: the cron on and the token set in AWS-Cloud; the lab's `sbx`
  environment gets `poll_url` (the gateway's ALB, already reachable by the
  lab through its WAF) and `poll_token`.

### Done when

On the cloud lab, with `sbx` selected, an operator ticks an EUR and a GBP
file, presses *Run* and *Poll now*, and sbx's settle-processing starts a
run for each, seen in sbx's logs and in the vendors' arrows on the
Dashboard. The same works from `curl` with sbx's token.

### 6.1 Seeding the platform, from the lab

**[DECIDED 2026-11-11, the owner]**: the lab seeds an environment's
platform itself, with what buddy's runner did locally, so a lab in the
cloud, a CI run and a laptop all seed the same way and the local runner
stays lean. The seeding code stays the platform's own, kept in step with
its schemas, and runs as a container in the lab's pod.

- **What seeding is** (lightyear `infinite-local-runner`, now
  `libs/lab-seeders`): generated merchants into the platform database,
  boarded and active, with company details, outlets and MIDs (`7xxxxxxx`),
  B4B beneficiaries and bank details, and per outlet the service products
  from its country's offer, each with its pricing contract (the fees);
  the base seed first when missing; and a master account per outlet in the
  settle database. Then a Worldline ER file per currency paying every
  generated outlet. Everything generated is marked (`SIM…` accounts), so a
  reseed replaces only that.
- **The seeder** is an image built in lightyear (`apps/lab-seeder`),
  added to the pod with `LAB_SEEDER_IMAGE` (`make pod-up`, `lab.sh
  deploy`). Its contract is generic, so the lab still knows no platform:
  `GET /info` names the databases it needs, `POST /seed` and
  `POST /settlement-files` take them with the form's fields. It mounts the
  lab API's volume at the same path and streams files straight into the
  environment's folder, so a file of millions of transactions never passes
  through the lab.
- **The environment's config** names its databases (`databases`, by the
  seeder's names), never shown back. Uat and sit are not seeded: their
  merchants board the normal way.
- **Private databases** are reached through the environment's jump host
  (`tunnel`: host, user, PEM key and/or password, an optional host-key
  pin), registered with its databases and never shown back. The lab opens
  it for each seeder call and hands the seeder each database's URL at a
  forward on the lab API's `127.0.0.1`, so the seeder needs no SSH and its
  contract does not change; `POST /v1/platform/check` says whether the lab
  reaches each one. A cloud lab's address must be allowed on the jump
  host.
- **The card** gains *Seed merchants…* and *Create settlement file…*, the
  runner's forms; without a seeder it keeps *Cut from the lab's trading*.
- **In step with the schemas**: lightyear's agent notes say a schema change
  means running the seeder's integration test, which migrates scratch
  databases, seeds and generates, and is meant for CI.

Routes: `GET /v1/platform`, `POST /v1/platform/check`, `POST /v1/platform/seed`,
`POST /v1/settlement-files/generate` (the seeder's file),
`POST /v1/settlement-files/cut` (Worldline's, from the lab's trading).

**Still open (6.2): files of millions of transactions end to end.** They
are made and listed without being held in memory, but Worldline's publish
still reads a file whole (64 MiB); it should stream from the lab's volume.
