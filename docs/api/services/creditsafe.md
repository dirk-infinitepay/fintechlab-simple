# Creditsafe — API

Simulates Creditsafe Connect: UK bank-account verification, company search and reports, and monitoring portfolios. One process, its own sim (`cmd/creditsafe`, sharing
the lab side with the other verification vendors through
`internal/vendorsim`). It sits on the **vendor** side of the line in
[catalogue.md](../../catalogue.md): a real vendor the platform calls
directly. What is real: the paths, the auth handshake, and the response
fields the platform's own client reads. What is simplified: the judgement
(every check passes until flipped with `POST /sim/outcome`), credentials
(any non-empty value is accepted, and an environment's own login names that
environment), token lifetime (never enforced), and everything a client does
not read, which is minimal, fixed or echoed. Nothing is persisted.

## At a glance

| | |
| --- | --- |
| Listens | `:8091`, plain HTTP: the vendor routes. Compose service `creditsafe` (`http://creditsafe:8091` on the compose network); host `http://127.0.0.1:8091`.<br>`:9091`, plain HTTP: the **control port** (`SIM_CONTROL_LISTEN`), every `/sim/*` route below and its own `GET /health`. Not published by compose; the lab API reaches it at `http://creditsafe:9091` |
| Auth | `POST /creditsafe/authenticate` (username and password) issues a bearer token for every other route. Any non-empty credential is accepted: an environment's own login makes the call that environment's, and any other is the default environment's ([Environments](#environments)). A customer's username with another password is `401`. `/health` and `/sim/*`: none. The control port has none: it takes calls from anything that can reach it, or only from `SIM_CONTROL_ALLOW` (addresses or CIDRs) when that is set |
| A platform points here with | `CREDITSAFE_API_URL` (`http://127.0.0.1:8091/creditsafe`), `CREDITSAFE_USERNAME`, `CREDITSAFE_PASSWORD`. The lab API hands these out at `GET :8097/v1/connect/env/creditsafe` |
| Activity logs | `GET /sim/activity`: the request journal, every vendor request and its answer, and the traps log |
| Configuration | `LISTEN` (`:8091`).<br>`SIM_CONTROL_LISTEN` (`LISTEN`'s port plus 1000, `:9091`) and `SIM_CONTROL_ALLOW` (empty: any caller): the control port.<br>Nothing else: no lab clock, no state file. The outcomes, per environment, every issued token and the customers live in memory and reset on restart; the lab API pushes the customers again |
| Design record | [integration/verification-vendor-edge.md](../../integration/verification-vendor-edge.md), the package comments in `cmd/creditsafe` and `internal/vendorsim`, [catalogue.md](../../catalogue.md) |

## Environments

Each environment the lab creates
([lab-api/environments.md](../../lab-api/environments.md)) is a customer
here, pushed by the lab API with
[`PUT /sim/customers/{env}`](#put-simcustomersenv) and known by its
`verification` login: `username` and `password` are the customer's.

- A call made with a customer's login is that environment's, and so is
  every token issued to it. Any other login is the default environment's.
  A customer's username with another password is refused, `401`, as the
  vendor would refuse it.
- Each environment has its own outcomes. A flip in one never moves
  another's answers.
- The `/sim/*` hooks act in the environment the call names: `?env=`, else
  the `Lab-Environment` header, else the default. `*` reads every
  environment; a flip names one. An environment this service was never
  given is `404`.
- A reset names one environment, or every one with `*`. Deleting a
  customer takes its outcomes and tokens with it.

## Vendor surface

What the platform's Creditsafe client calls. Errors this service writes
have one shape, `{"error": "<message>", "status": <code>}`. Request bodies
are decoded loosely: malformed JSON is read as an empty object rather than
refused, and only the fields named per route are read.

| Check | Default | Moves |
| --- | --- | --- |
| `bank-account` | `pass` | the UK bank verification |
| `company` | `active` | the company report |

Company data and UK bank verification. Authenticate once, then send
`Authorization: Bearer <token>` on every other Creditsafe route. A missing
header or one without the `Bearer ` prefix is `401` `bearer token required`;
a token this process did not issue (or issued before a restart) is `401`
`unknown or expired token`.

### `POST /creditsafe/authenticate`

Exchanges a username and password for a token.

Body: `{"username": "sim", "password": "sim-verification-dev-only"}`. Any
non-empty pair: a customer's login is its environment's, any other the
default's.

Answers `200` with `{"token": "cs_1794039000123456789"}`. `401`
`{"error":"username and password required","status":401}` when either is
missing or empty; `401` `invalid username or password` for a customer's
username with another password.

Side effects: the token is remembered until restart and never expires.
Every call it opens is the environment the login was.

### `POST /creditsafe/localSolutions/GB/bankVerification/search`

UK bank-account check: does this account number and sort code exist, and
does the name match.

Body, as the platform's client sends it:

```json
{"accountNumber": "12345678", "sortCode": "00-00-00", "name": "Southwind Coffee Ltd"}
```

Only `accountNumber` and `sortCode` are read, to echo back. The whole body
is returned under `request`.

Answers `200`:

```json
{
  "supplierResponse": {"result": true, "nameMatchResult": "Full", "accountTypeResult": true, "accountNumber": "12345678", "sortCode": "00-00-00"},
  "request": {"accountNumber": "12345678", "sortCode": "00-00-00", "name": "Southwind Coffee Ltd"}
}
```

`bank-account` outcome: `pass` gives `true`/`Full`/`true`; `mismatch` (a
real account in another name) gives `true`/`No Match`/`true`; `invalid`
gives `false`/`No Match`/`false`, as `result`/`nameMatchResult`/`accountTypeResult`.

### `GET /creditsafe/companies`

Company search.

Query: `countries` (for example `GB`), `regNo`.

Answers `200` with exactly one hit built from the query, whatever it was
(empty parameters included):

```json
{"totalSize": 1, "companies": [{"id": "GB-0-12345678", "country": "GB", "regNo": "12345678"}]}
```

### `GET /creditsafe/companies/{id}`

Company report.

Answers `200`. Every report carries a clean credit picture:

```json
{"companyId": "GB-0-12345678", "report": {
  "companySummary": {"companyNumber": "GB-0-12345678", "companyStatus": {"status": "Active"},
                     "creditRating": {"commonValue": "A", "commonDescription": "Very Low Risk"}},
  "creditScore": {"currentCreditRating": {"commonValue": "A", "commonDescription": "Very Low Risk", "pod": 0.5}},
  "negativeInformation": {"ccjSummary": {"numberOfExact": 0, "numberOfPossible": 0, "numberOfSatisfied": 0}}}}
```

`companyNumber` echoes the path id, not the registration number. `company`
outcome `dissolved` gives Creditsafe's common status for a company that has
stopped trading, `{"status": "NonActive", "providerStatus": "Dissolved"}`.

A company registered with
[`POST /sim/creditsafe/companies`](#post-simcreditsafecompanies) in the
caller's environment also carries what was registered, where Creditsafe
puts it: `companySummary.businessName`, `companySummary.companyRegistrationNumber`,
`companySummary.country`; `companyIdentification.basicInformation` with
`registeredCompanyName`, `companyRegistrationDate` and
`contactAddress.simpleValue`; and each person, as registered, under
`directors.currentDirectors`, `shareCapitalStructure.shareHolders` and
`additionalInformation.personsWithSignificantControl.activePSC`.

### `GET /creditsafe/monitoring/portfolios`

Lists monitoring portfolios.

Answers `200` with one fixed portfolio,
`{"portfolios": [{"id": "sim-portfolio-1", "name": "Simulated portfolio"}]}`.
Portfolios created with the `POST` below are not listed.

### `POST /creditsafe/monitoring/portfolios`

Creates a monitoring portfolio.

Body: `{"name": "Infinite merchants", "isDefault": false}`.

Answers `200` with `{"portfolioId": 1, "name": "Infinite merchants", "isDefault": false}`.
`portfolioId` is an integer counter from 1 per environment (the list above
uses a string id). Nothing is stored.

### `POST /creditsafe/monitoring/portfolios/{portfolioId}/companies`

Adds a company to a portfolio. Body and `portfolioId` are ignored; nothing
is stored.

Answers `201` with `{"message": "Company added to portfolio"}`.

### `GET /creditsafe/monitoring/portfolios/{portfolioId}/companies`

Answers `200` with `{"data": [], "totalCount": 0}`, always.

### `PUT /creditsafe/monitoring/portfolios/{portfolioId}/eventRules/{countryCode}`

Sets a portfolio's event rules. Body ignored.

Answers `204` with no body.

### `GET /creditsafe/monitoring/notificationEvents`

The monitoring poll. Query parameters are ignored.

Answers `200` with `{"data": [], "totalCount": 0}`, always: an invented
event would send the platform chasing a company that did not change. No
outcome moves it.

## Lab control

Hooks a test, agent or operator uses to observe or steer the sim. They do
not exist at the real vendor. They are on the control port, `:9091`
([the control contract](../README.md#the-control-contract)); the same
`/sim/*` paths still answer on `:8091` until phase 4 of
[the plan](../../lab-api/plan.md) retires them.

### `GET /health`

Liveness, on both ports. `:8091` answers `{"status": "ok", "service":
"creditsafe"}`; the control port answers `{"status": "ok", "service":
"creditsafe", "surface": "control"}`, and that is the one the lab API
probes.

### `GET /sim/outcome`

What each check currently answers, in the environment the call names
(`?env=` or `Lab-Environment`; the default without either).

Answers `200`:

```json
{"env": "default", "outcomes": {"bank-account": "pass", "company": "active"}}
```

With `?env=*`, every environment's, the default's included. `404`
`no environment <env> at creditsafe` for one this service was never given.

### `POST /sim/outcome`

Flips one check in the environment the call names (`?env=` or
`Lab-Environment`; the default without either). It applies to every later
call made with that environment's login until it is set back, the
environment is reset ([`POST /sim/reset`](#post-simreset)), or the process
restarts. Another environment's answers do not move.

Body, decoded strictly (an unknown field is a 400): `{"check": "company", "outcome": "dissolved"}`.

| `check` | `outcome` |
| --- | --- |
| `bank-account` | `pass`, `mismatch`, `invalid` |
| `company` | `active`, `dissolved` |

Answers `200` with the environment's every outcome, as `GET /sim/outcome`
does. `400` for an unknown check or outcome, which changes nothing:
`{"error":"check \"bank-account\" cannot be \"maybe\" (want: pass, mismatch, invalid)","status":400}`;
`400` `an outcome is set in one environment, not in all of them` for
`env=*`; `404` for an environment this service was never given.

### `POST /sim/creditsafe/companies`

Registers a company in the environment the call names (`?env=` or
`Lab-Environment`; the default without either), so its Creditsafe report
([`GET /creditsafe/companies/{id}`](#get-creditsafecompaniesid)) carries
these details. Registering the same `country` and `regNo` again replaces
them.

Body, decoded strictly (an unknown field is a 400):

```json
{"country": "GB", "regNo": "29210000", "businessName": "Southwind Coffee Ltd",
 "registrationDate": "2015-04-01T00:00:00Z", "address": "1 Test Street London SW1A 1AA",
 "directors": [{"name": "Ada Lovelace"}],
 "shareholders": [{"name": "Ada Lovelace", "percentSharesHeld": 100, "shareholderType": "Person"}],
 "personsWithSignificantControl": [{"name": "Ada Lovelace", "countryOfResidence": "United Kingdom"}]}
```

Each person needs a `name`; any other field is Creditsafe's own and is
served as given.

Answers `200` with `{"env": "default", "id": "GB-0-29210000", "company": {…}}`.
`400` without `country` or `regNo`, for a person with no `name`, or for
`env=*`; `404` for an
environment this service was never given.

### `GET /sim/traps/hooks`

The catalogue: one hook, `http.request`, on every vendor request
([traps.md §9.7](../../lab-api/traps.md#97-every-sim-httprequest)), with
the `respond` effect ([§4.3](../../lab-api/traps.md#43-respond-answering-a-vendor-shaped-request)).

```json
{"service": "creditsafe",
 "hooks": [{"hook": "http.request",
            "about": "Any request on the vendor surface. …",
            "subject": "request",
            "match": [{"field": "method", "type": "string", "about": "GET, POST, …"},
                      {"field": "path", "type": "path", "about": "the request path, without the query; * stands for one segment"}],
            "effect": "respond"}]}
```

A request is the environment its credentials name: its bearer token's or
its `x-api-key`'s. A login sent in the body (`/creditsafe/authenticate`,
`/lexisnexis/oauth/token`, IVI) is the default environment's. `/health`
and the `/sim/*` paths are never caught.

### `GET /sim/traps`

The traps, newest first. Query: `state` (`armed`, `spent`, `expired` or
`disarmed`) filters; `400` for anything else.

Answers `200` with `{"service": "creditsafe", "traps": [<trap>, …]}`.

### `POST /sim/traps`

Arms a trap. Body: `hook` (`http.request`), `match` (`{}` takes every
request), `effect`, and optionally `label`, `times` (`1` when left out,
`null` for every request until disarmed) and `expires_at`. An unknown field
is refused. The effect is one of:

| Effect | |
| --- | --- |
| `{"respond": {"status": 503, "body": …, "headers": {…}}}` | answered with this instead; the request has no effect. A string `body` is sent as it is (`text/plain` unless a header says otherwise), so a reply can be unreadable; anything else is sent as JSON |
| `{"delay": "8s"}` | handled normally, after the wait: for client timeouts |
| `{"close": true}` | the connection is dropped with no answer |

The next Creditsafe company report is an outage:

```json
{"hook": "http.request", "match": {"method": "GET", "path": "/creditsafe/companies/*"},
 "effect": {"respond": {"status": 503, "body": {"error": "Service Unavailable"}}}}
```

Answers `201` with the trap, `effect` as armed. `400` with the reason: a
hook or match field creditsafe does not have, an effect that is not
exactly one of the three, a status outside 100 to 599, or a delay that is
not a duration.

### `GET /sim/traps/{id}`

One trap. Each request it caught is in `caught`, with what was done to it:

```json
{"id": "trp_0001", "…": "as armed", "state": "spent",
 "caught": [{"subject": "GET /creditsafe/companies/GB-0-29210000", "at": "2026-11-12T09:40:00Z",
             "applied": [{"to": "respond 503", "at": "2026-11-12T09:40:00Z"}]}],
 "pending_steps": []}
```

`404` for an unknown id.

### `DELETE /sim/traps/{id}`

Disarms one trap. Answers `200` with `{"trap": <trap>, "cancelled": []}`,
`404` for an unknown id.

### `DELETE /sim/traps`

Disarms every trap. Answers `200` with `{"service": "creditsafe",
"disarmed": 1, "cancelled": []}`.

### `GET /sim/activity`

Two logs: `requests`, the journal, and `traps`, every trap armed, every
request one caught and what was done to it. A request a trap answers is
journalled with the trap's answer; one a trap drops (`close`) is `bad`,
`connection dropped`.

The request journal, newest first: every request the platform made to a
vendor on `:8091`, and the status it was answered with. Query: `limit`
(positive integer, default 100); `env` (the default when left out, `*` for
every environment's); `404` for an environment this service was never
given. A request is the environment its credentials name, a
login included; one the vendor refused is the default's.

Answers `200`:

```json
{"logs": [{"name": "requests", "title": "Vendor requests",
           "note": "Every request the platform made to Creditsafe, and the status it was answered with.",
           "total": 1, "kept": 1,
           "last": {"seq": 1, "at": "2026-11-12T09:40:00Z", "op": "creditsafe", "peer": "10.89.0.7",
                    "summary": "GET /creditsafe/companies/GB-0-29210000", "status": "ok", "detail": {"status": "OK"}},
           "events": [{"seq": 1, "…": "as last"}]}]}
```

`op` is `creditsafe`. A `4xx` answer is `warn` and carries the vendor's error in the
summary. The journal keeps the first 64 KiB of a request body; the
vendor reads it all.

### `GET /sim/info`

Who this is and what it can do, as every service answers it
([the control contract](../README.md#the-control-contract)):
`"service": "creditsafe"`, `"kind": "vendor"`,
`"reset_scopes": ["flow", "all"]`, `"hooks": ["http.request"]`,
`"traps": {"armed": 0, "pending_steps": 0}`, `"environments": true` and
`"customers"`, how many there are.

### `GET /sim/logs`

The process's own log lines, as every service answers it
([the control contract](../README.md#the-control-contract)): one line per
request, and every flip of an outcome.

### `POST /sim/reset`

Puts every outcome back to its default, in the environment the reset
names (`"env"` in the body, `?env=` or `Lab-Environment`; the default
without any), or in every one for `*`.

- **`flow` clears**: the outcomes, the registered companies, every trap and the request journal.
- **`flow` keeps**: every bearer token issued, so a platform holding one is not refused mid-session; and the portfolio id counter.
- **`all`** is first start: the outcomes, every token issued in the environment (a held one is `401` `unknown or expired token` until the client authenticates again), and its portfolio ids count from 1 again.

`cleared` counts `outcomes` that were flipped, `companies` that were registered, journal events it held (`activity`, at most 256 per environment), and the `traps` disarmed (with `trap_steps`, always 0 here), and with `all`, `tokens` and `portfolios` created. Another environment's state is left
alone.

```json
{"service": "creditsafe", "scope": "flow", "env": "default", "cleared": {"activity": 5, "companies": 1, "outcomes": 2, "trap_steps": 0, "traps": 1},
 "kept": ["tokens", "portfolios"], "at": "2026-11-07T12:00:00Z"}
```

### `GET /sim/customers`

The environments this service serves, the default first
([sim-control.md §8](../../lab-api/sim-control.md#8-environments)).

```json
{"customers": [
  {"env": "default", "created_at": "0001-01-01T00:00:00Z", "last_seen": "2026-11-10T09:00:00Z"},
  {"env": "sbx", "name": "sbx", "created_at": "2026-11-10T08:00:00Z", "last_seen": null}]}
```

`last_seen` is the last vendor call made with the environment's login, a
token request included; `null` until the first. The default has no
`created_at` of its own: it is the zero time.

### `PUT /sim/customers/{env}`

Adds or replaces one customer. The lab API pushes every customer again
when the service restarts, because they live in memory.

Body: a customer, of which this service reads `verification`:

```json
{"env": "sbx", "name": "sbx", "verification": {"username": "sbx-verification", "password": "…"}}
```

`env` may be left out; given, it must be the path's.

Answers `200` `{"env": "sbx", "created": true}`, `false` when it replaced
one; replacing keeps the environment's outcomes and tokens. `400` for a
body that is not a customer, an `env` that is not the path's, or an id
that cannot name an environment (`default`, `*`, or anything but
lower-case letters, digits and dashes, starting with a letter, at most
40).

### `DELETE /sim/customers/{env}`

Removes one customer and what it had here: its outcomes, every token
issued to it, its traps and its journal. Its login is then a stranger's, the default's.

Answers `200` `{"env": "sbx", "deleted": true}`; `404`
`no environment sbx here`.

## Called by

- **The platform** under test: its own Creditsafe client, through the
  variables in the table above. Nothing in the lab calls the vendor routes.
- **lab API** (`cmd/labapi`): only the control port, `http://creditsafe:9091`.
  `GET /v1/lab` and `POST /v1/services/creditsafe/probe` probe its
  `GET /health`. `POST /v1/lab/reset` and `POST /v1/services/creditsafe/reset`
  call `POST /sim/reset`; `GET /v1/logs` and `GET /v1/services/creditsafe/logs`
  read `GET /sim/logs`; every other `/sim/*` route is reached through
  `/v1/services/creditsafe/sim/*`.
