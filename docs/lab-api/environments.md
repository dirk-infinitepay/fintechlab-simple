# Environments: one lab, several platforms

**Built, 2026-11-10.** §2 is the owner's model **[DECIDED 2026-11-10]**;
the rest was proposed here and built as written, with the differences
below. [OPEN 17–25](open.md) were taken as proposed. It replaces the sketch
that was [plan.md's phase 5](plan.md#phase-5--environments).

Where the build differs from the draft:

- **The default environment's merchant registry stays `registry.json`.**
  Every other environment's registry is in the lab DB (`internal/labdb`).
- **There is no *All environments* entry in the console's selector** (§10).
- **A vendor that does not keep environments apart is never asked for
  another environment's records.** The lab API's transport answers `409`
  for it, so no view can pass the default's data off as an environment's.
- **The SFTP user in a kit is `<id>-sftp`**, so an environment named after
  one of the default's SFTP logins cannot take it over.
- **The e2e suite keeps its scenarios in the default environment.** They
  test the stand-ins, which are the default's. A scenario of its own,
  `environments-keep-platforms-apart`, makes two throwaway environments and
  checks every vendor between them.

Where it lives: `internal/tenant` (the sims' side), `internal/labdb` (the
database), `cmd/labapi/auth.go`, `environments.go` and `registries.go`
(tokens, scope, customers, the sync), `cmd/console/web` (the selector and
the Environments view), and each sim's own package.

## 1. Why

The lab has started serving more than one platform. The cloud lab
([deploy-cloud.md](../deploy-cloud.md)) serves the sbx deployment, and it
also serves a developer's runner over a tunnel. Every simulation, though,
has exactly one customer, and whoever calls it *is* that customer:

| Vendor | Today | So two platforms on one lab |
| --- | --- | --- |
| Banking Circle | Any non-empty Basic credential gets a token (`cmd/bankingcircle/main.go:511`), and the token records no one. The client certificate is checked against the CA, but nothing reads who it belongs to. The two safeguarding accounts are constants (`internal/bankingcircle/accountid.go:31`), and every payout debits the one for its currency. A subscription with no targets gets every notification. | fund and drain the same safeguarding account, and receive each other's notifications |
| B4B | One JWT key id (`B4B_JWT_KEY_ID`). Any valid JWT lists every company (`cmd/b4b/companies.go:38`), and `external_ref` is unique across the whole store. | see each other's merchants, and collide when both board the same one |
| Worldline | One SFTP password for any username, and one root (`internal/wlsftp/server.go:56`). One file per currency covers every MID in the store (`internal/worldline/cycle.go:111`). | both pull the same files, and both pay the same outlets |
| verification, verify | One outcome per vendor, or one API key, for everyone | override each other's outcomes |
| ACI | One webhook target | cannot both receive |
| The lab API | One merchant registry, one `POST /v1/lab/reset`, one console | reset each other's runs, and read each other's data |

Nothing below the vendors records an owner either: an activity event, a
trap or a reset request has no field to put one in (`internal/activity`,
`internal/trap`, `internal/simctl`).

The way a platform plugs in today is a **plugin registration**
([plugins.md](../plugins.md)), which doesn't fit a deployed platform:

- It answers "is a process alive?". The registration has to be renewed
  every 10 s against a 30 s TTL, and the lab API forgets it on every restart.
- A laptop runner can keep that loop going. A deployed stack can't do it
  without a process kept running only for that.
- A platform is really tied to the lab by its vendor credentials. sbx shows
  this: it was fully working against the cloud lab before anything was
  registered.

## 2. The model

**[DECIDED 2026-11-10, the owner]**

- An **environment** is one platform deployment using the lab: sbx, a
  developer's laptop, an e2e run. It is created in the console or through
  the lab API with a **name** and a **config**; an empty config means the
  defaults. Creating it returns its **token**, which the lab keeps.
- The **lab DB** is SQLite in the `lab-api` container, on its volume.
- Each environment is its vendors' **customer**:
  - for Banking Circle and B4B, its own companies with their own
    credentials (username, password, company id), two or three of them
    pre-created from config and more added in the console;
  - for Worldline, its own SFTP location, `{env}/…`.
- Every call to the lab API carries a token. An environment's token sees
  and changes only that environment, so nothing mixes.
- The console has an **environment selector** in its header.
- **There is no heartbeat.** A registered environment is green, and grey
  only while nothing has ever been seen from it.

## 3. Two identities, kept apart

| Where | Who it identifies | How |
| --- | --- | --- |
| The lab API, `/v1` | whoever steers the lab: a person, the console, an agent, a platform's runner | `Authorization: Bearer`: the **admin token** (today's `LAB_API_TOKEN`) or an **environment token** |
| The vendor surfaces | the platform, as the vendor's customer | the vendor's own login: Banking Circle's Basic credential, B4B's JWT `kid`, the SFTP username, verification's API key or username |

- **A platform never sends a lab token to a vendor.** The vendor knows the
  environment from its own login, as the real vendor would know its
  customer. This keeps the drop-in rule
  ([architecture.md §3](architecture.md#3-the-drop-in-rule)).
- **A sim tags what it creates with the customer it came from,** and the
  customer carries the environment's id.
- **The lab API keeps each environment's kit** (the credentials its
  platform writes into its `.env`), as it already serves today's connect
  kit: to hand out, never to use. It still calls no vendor surface
  ([CLAUDE.md](../../CLAUDE.md#apis)).
- **The admin token** creates and deletes environments, and reaches any
  environment by naming it (§6).

## 4. The default environment, and what changes for today's users

- **The lab starts with one environment, `default`, which owns everything
  that is fixed today:**
  - Banking Circle's safeguarding accounts `…000000000978` and
    `…000000000826`;
  - B4B's `b4b-mock-1` key;
  - the SFTP user `infinitepay` and its password;
  - the receiver's seeded Banking Circle subscription;
  - the merchant registry as it stands;
  - the **stand-ins**.
- **The stand-ins play `default`'s platform.** The settlement stand-in pulls
  only `default`'s files and pays from `default`'s accounts. Switching the
  stand-ins off is then only needed when a platform shares `default`
  ([§9](#9-plugins-and-the-stand-ins)).
- **Nothing breaks for today's users:**
  - With no `LAB_API_TOKEN` set (the local `make up`), every call is
    `default`, as today.
  - `LAB_API_TOKEN` becomes the admin token. A call made with it that names
    no environment means `default` ([OPEN 24](open.md)).
  - buddy's runner and the e2e suite keep working untouched until they opt
    in.

## 5. Creating an environment

```http
POST /v1/environments            (admin token)
{"name": "sbx", "config": {}}
```

```json
{"id": "sbx", "name": "sbx", "token": "lenv_…", "created_at": "…",
 "kit": "/v1/connect/env", "state": "never-seen"}
```

The lab creates the environment's customers at every vendor that supports
environments, writes them to the lab DB, and pushes each sim its own
(§7). Its kit, from `GET /v1/connect/env` with its token, is the same `.env`
the console hands out today, filled with the environment's own values:

| Vendor | The customer | In the kit |
| --- | --- | --- |
| Banking Circle | a client, with a username and password | `BC_API_M2M_USERNAME`, its password; `BC_SAFEGUARDING_ACCOUNT_ID_EUR` and `_GBP`, the environment's own (UUIDv5 of the environment and the currency, so a recreated environment keeps them; `default` keeps 978/826); the client certificate, shared ([OPEN 20](open.md)) |
| B4B | a key id and an RS512 key pair the lab makes, plus one company per currency | `B4B_JWT_KEY_ID`, `B4B_JWT_PRIVATE_KEY`, the company ids and debtor vIBANs |
| Worldline | an SFTP user and password, jailed at `{root}/{env}` | `WORLDLINE_SFTP_USERNAME`, its password, the host key fingerprint; the PGP keys, shared ([OPEN 21](open.md)) |
| verification, verify | an API key or username of its own | the keys |

`config` starts empty. The knobs it is expected to grow are the ones a
deployment sets lab-wide today. sbx's are in its `deploy/aws/lab.env`:

- `settlement_identifier`: Worldline's file name, `Infinite_Payment` for
  sbx. Today it is `LAB_SET`.
- `webhook_hosts`: the platform's callback hosts, which the environment's
  subscriptions and callbacks may reach. Today it is `LAB_WEBHOOK_ALLOW`,
  shared by everyone.
- `aci_webhook_url`: ACI's delivery target for this environment.
- `clock`: `follows` or `wall` (§8).

Other routes:

- `GET /v1/environments`: the admin sees every environment, and an
  environment token sees its own.
- `GET`, `PATCH` and `DELETE /v1/environments/{id}`. Deleting clears the
  environment's records at every vendor, as an `all` reset would, removes
  its customers, and drops its token.
- `POST /v1/environments/{id}/token`: rotate the token.

### Status: green from traffic, not from a heartbeat

- **Each sim records when it last saw each customer** on its vendor
  surface, and answers with it in `GET /sim/customers`. The lab API puts
  these together into `last_seen`, per vendor and overall.
- **The state follows from it:** `never-seen` (grey) until the first vendor
  call, then `seen` (green), shown with "last seen 3 h ago".
- **It never turns red with age.** A deployed platform may be quiet for a
  day and still be fine.

## 6. Scoping the lab API

An environment token reaches only that environment. The admin token picks
one with `Lab-Environment: <id>`, which is the console's header selector,
and `default` is assumed without it.

| Environment-scoped | Lab-wide, admin only | Readable with any token |
| --- | --- | --- |
| merchants and batches, provisioning, trading | `/v1/environments` | `GET /v1`, `GET /v1/openapi.json` |
| `/v1/banking-circle/*`, `/v1/actions/fund-sga`, `/v1/actions/worldline-cycle` | `/v1/config`, `PUT /v1/config/worldline-channel` | `GET /v1/services`, health, probes |
| `/v1/services/{id}/activity`, `/v1/flow` (the Dashboard) | moving and holding the clock | `GET /v1/clock` |
| `/v1/traps`, `/v1/lab/reset`, `/v1/services/{id}/reset` | `/v1/stand-ins` (they are `default`'s) | |
| `/v1/connect*` (the environment's kit) | `/v1/logs`, `/v1/services/{id}/logs` ([OPEN 18](open.md)) | |
| `/v1/plugins` (owned by the environment that registered them) | `/v1/actions/settlement-pull` (the stand-in) | |
| `/v1/services/{id}/sim/*`, only to sims that support environments | the passthrough to sims that do not yet | |

The rules behind the table:

- **The scope reaches the sims through their control ports.** The lab API
  adds `env=<id>` to every `/sim/*` call it makes on an environment's
  behalf: a query parameter on reads, and a field on resets
  (`internal/simctl` refuses unknown fields today, so the field is added
  there) and on traps.
- **A sim says it supports environments in `GET /sim/info`**
  (`"environments": true`).
- **An environment token is refused, not quietly widened, at a sim that
  does not support them yet,** so a half-built lab never lets one
  environment change another's data. The console marks such a vendor
  *shared by every environment*.
- **`POST /v1/lab/reset` with an environment token resets that environment
  only.** The admin resets one environment by naming it, or every
  environment with `Lab-Environment: *`.

## 7. What each sim learns

The pattern is the same everywhere:

- **Customers are pushed to the sim on its control port**, with
  `PUT /sim/customers/{env}` and the credentials it should accept. The lab
  DB is their record, because Banking Circle's and Worldline's state is in
  memory. When a sim's `/sim/info` epoch changes (it restarted), the lab API
  pushes its customers again, so a platform's credentials survive a lab
  restart.
- **Each record gets an owner:** an account, payout, subscription, company,
  beneficiary, payment, transaction or file. The sim sets it from the
  customer the vendor call authenticated as.
- **Fan-out goes only to the owner's listeners.** That covers
  notifications, callbacks and webhooks.
- **`/sim/*` reads filter by `env`, and resets and traps act within it.**
  `internal/activity` events gain an `env` field. There is a ring per
  environment, so one busy environment cannot push out another's history.
  `internal/trap`'s `Spec` gains `Env`, and an event from another
  environment never matches.
- **Calls between sims carry the environment.** The Banking Circle bridge
  on `:8095` is how B4B pays out, how Worldline wires its lump sum and how
  settlement reads balances; each of those calls says whose it is.

Per vendor:

- **Banking Circle.**
  - `authorize` looks the credential up among its customers, and the token
    records which customer it was issued to.
  - Each customer has its own safeguarding accounts. Creditor accounts are
    keyed by the customer and the beneficiary, because today's UUIDv5 of the
    beneficiary alone would give two environments one account
    (`internal/bankingcircle/accountid.go:36`).
  - A payout debits the payer's own safeguarding account.
  - Subscriptions and their notifications are the owner's.
  - The intraday report covers only the owner's accounts.
  - `POST /sim/payments/outcomes` arms traps with an empty match
    (`cmd/bankingcircle/traps.go:263`). Scoped, it catches the environment's
    next payout, not anyone's.
- **B4B.**
  - A key id per customer, verified against that customer's public key.
  - Companies, beneficiaries and payments are owned. Lists show only the
    caller's own, and `external_ref` is unique per customer.
  - Callbacks go to the environment's hosts.
  - `state.json` gains the owner, and records without one load as
    `default`'s.
- **Worldline.**
  - SFTP checks the username and its password, and jails each user at
    `{root}/{env}`.
  - Transactions, cycle runs, lump sums and enrolment batches are owned.
  - The cycle writes one file per currency *per environment*, named with
    that environment's identifier, into its folder, and wires the lump sum
    to its safeguarding account.
  - `ProxyMIDFor("6001", pf_account_id)` takes the environment into account
    (`internal/worldline/enrolment.go:240`), so two environments onboarding
    the same account get different MIDs.
- **verification and verify.**
  - Outcomes and forced declines are per customer.
  - Verify accepts one internal API key per environment.
- **ACI.** A target per environment, and a sent log per environment.
- **The business bank.** Credits are tagged with the environment the payout
  came from, and its account list filters by it. It stays one bank, because
  that is what the merchant's own bank is.
- **notifier, payment-api, receiver, settlement.** These are stand-ins and
  demo services. They stay `default`'s.

## 8. The clock

There is still one clock per lab. The cloud profile has none, so every
environment there runs on wall time, which is right. With the local
profile, a laptop usually has one environment.

The plugin's `clock` field moves into the environment's `config`: an
environment set to `wall` that has been seen blocks moves, as a live
`"wall"` plugin does today ([plugins.md](../plugins.md#the-clock)). Who may
move a shared clock is [OPEN 19](open.md).

## 9. Plugins and the stand-ins

- **A plugin is optional, and only for a platform that brings buttons, a
  files table or logs.** buddy's runner is the example. It registers with
  its environment's token, and its card shows under that environment.
- **A lapsed plugin greys out its buttons, not its environment.** The
  environment's state still comes from vendor traffic (§5).
- **A descriptor's `stand_ins` applies only to `default`,** the one
  environment the stand-ins serve. An environment of its own never has to
  switch them off.

## 10. The console

- **The environment selector is in
  [design-system.md](../design-system.md#environment-selector--whose-lab-you-are-looking-at)**,
  as a component every operator console can use. It shows each
  environment's name and its status dot. There is no *All environments*
  entry: every view is one environment's, and the lab-wide reads are the
  API's (`Lab-Environment: *`).
- **The Environments page.** It lists the environments with their last-seen
  status. *Create* takes a name and a config, and shows the token and the
  kit's download. There are also rotate-token and delete, with a confirm
  for delete.
- **Every view follows the selector.** The Dashboard, the vendor views and
  their activity, merchants, and traps.
- **A vendor not yet split says so on its card:** *shared by every
  environment*.

## 11. Alternatives not taken

- **A lab per environment** (a pod per platform). It needs no vendor change,
  but in the cloud it is a host per platform, and the console cannot show
  two side by side. It stays possible for CI, where one throwaway pod per
  run costs nothing to keep apart.
- **Naming the environment on vendor calls** (a header or a parameter that
  only the lab understands). The real vendors have no such thing, so it
  breaks the drop-in rule. A platform would also have to carry lab-only
  code on its vendor clients.
- **Keeping the plugin heartbeat as the sign of life.** §1 says why it does
  not fit a deployed platform. Vendor traffic is the better sign: it shows
  the platform actually working, not just running.

## 12. What this means for sbx

- **This week, nothing changes.** sbx uses `default`'s values: the
  `…978`/`…826` safeguarding accounts, `b4b-mock-1`, `infinitepay`. The
  stand-ins are switched off by hand on the cloud lab, and set to start off
  in `deploy/aws/lab.env`, except the receiver's Banking Circle
  subscription, which has no start-up switch.
- **Once Banking Circle, B4B and Worldline are split, sbx gets an
  environment of its own:**
  - its kit goes into AWS-Cloud as before: config with
    `scripts/deploy.sh sbx up`, secrets with `scripts/app-secrets.sh sbx
    apply`;
  - its `LAB_SET` and `LAB_WEBHOOK_ALLOW` entries become its environment
    config;
  - a developer's runner keeps `default`, with the stand-ins, on the same
    lab.
