# Verification vendors — API

Simulates four third-party checks the platform runs before it will move a
merchant's money: **Creditsafe** (company data and UK bank-account
verification), **iban.com** (IBAN validity and name match), **KYC6**
(sanctions, PEP and adverse-media screening) and **LexisNexis** (OAuth, then
IDU identity reports and IVI attribute queries). One process, one path prefix
per vendor, no state shared between them. It sits on the **verification**
side of the line in [catalogue.md](../../catalogue.md): real vendors the
platform calls directly. What is real: the paths after each prefix, the auth
handshakes (tokens are issued and checked), and the response fields the
platform's own clients read. What is simplified: the judgement (every check
passes until flipped with `POST /sim/outcome`), credentials (any non-empty
value is accepted, and an environment's own login names that environment),
token lifetime (never enforced), and everything a client does not read,
which is minimal, fixed or echoed. Nothing is persisted.

## At a glance

| | |
| --- | --- |
| Listens | `:8089`, plain HTTP: the vendor routes. Compose service `verification` (`http://verification:8089` on the compose network); host `http://127.0.0.1:8089`.<br>`:9089`, plain HTTP: the **control port** (`SIM_CONTROL_LISTEN`), every `/sim/*` route below and its own `GET /health`. Not published by compose; the lab API reaches it at `http://verification:9089` |
| Auth | Creditsafe: `POST /creditsafe/authenticate` (username and password) issues a bearer token for its other routes. LexisNexis: `POST /lexisnexis/oauth/token` (client id and secret) issues a bearer token for IDU; IVI carries `org_id`, `api_key` and `policy` in the body. iban.com and KYC6: an `x-api-key` header. Any non-empty credential is accepted: an environment's own login makes the call that environment's, and any other is the default environment's ([Environments](#environments)). A customer's username with another password is `401`. Bearer tokens come from one shared pool, so a token from either issuer opens either vendor's bearer routes, in the environment it was issued to. `/health` and `/sim/*`: none. The control port has none: it takes calls from anything that can reach it, or only from `SIM_CONTROL_ALLOW` (addresses or CIDRs) when that is set |
| A platform points here with | `CREDITSAFE_API_URL` (`http://127.0.0.1:8089/creditsafe`), `CREDITSAFE_USERNAME`, `CREDITSAFE_PASSWORD`; `IBAN_COM_BASE_URL` (`…/iban/verify`, the full endpoint), `IBAN_COM_API_KEY`; `KYC6_API_BASE_URL` and `KYC6_MONITOR_API_BASE_URL` (both `…/kyc6`), `KYC6_API_KEY`; `LEXISNEXIS_AUTH_URL` (`…/lexisnexis`), `LEXISNEXIS_IDU_BASE_URL` (`…/lexisnexis/idu`), `LEXISNEXIS_IVI_BASE_URL` (`…/lexisnexis/ivi`), `LEXISNEXIS_CLIENT_ID`, `LEXISNEXIS_CLIENT_SECRET`, `LEXISNEXIS_IVI_API_KEY`. The lab API hands these out at `GET :8097/v1/connect/env/verification` |
| Activity logs | None. There is no `GET /sim/activity`, so the console card shows no panels. Each request line is logged on stdout |
| Configuration | `LISTEN` (`:8089`).<br>`SIM_CONTROL_LISTEN` (`LISTEN`'s port plus 1000, `:9089`) and `SIM_CONTROL_ALLOW` (empty: any caller): the control port.<br>Nothing else: no lab clock, no state file. The outcomes, per environment, every issued token and the customers live in memory and reset on restart; the lab API pushes the customers again. Outcomes change only through `POST /sim/outcome`, and go back to their defaults with `POST /sim/reset` |
| Design record | [integration/verification-vendor-edge.md](../../integration/verification-vendor-edge.md), the package comment in `cmd/verification/main.go`, [catalogue.md](../../catalogue.md) |

## Environments

Each environment the lab creates
([lab-api/environments.md](../../lab-api/environments.md)) is a customer
here, pushed by the lab API with
[`PUT /sim/customers/{env}`](#put-simcustomersenv) and known by its
`verification` login:

| The customer's | is |
| --- | --- |
| `username` | Creditsafe's `username`, LexisNexis's `client_id` |
| `password` | Creditsafe's `password`, LexisNexis's `client_secret`, the KYC6 and iban.com `x-api-key`, LexisNexis IVI's `api_key` |

- A call made with a customer's login is that environment's, and so is
  every token issued to it. Any other login is the default environment's,
  as every login was before there were environments. A customer's
  username with another password is refused, `401`, as the vendor would
  refuse it.
- Each environment has its own outcomes and its own Creditsafe portfolio
  ids. A flip in one never moves another's answers.
- The `/sim/*` hooks act in the environment the call names: `?env=`, else
  the `Lab-Environment` header, else the default. `*` reads every
  environment; a flip names one. An environment this service was never
  given is `404`.
- A reset names one environment, or every one with `*`. Deleting a
  customer takes its outcomes and tokens with it.

## Vendor surface

What the platform's Creditsafe, iban.com, KYC6 and LexisNexis clients call.
Each vendor sits under its own prefix, the platform's base-URL variable
includes that prefix, and the path after it is the vendor's.

Errors this service writes have one shape,
`{"error": "<message>", "status": <code>}`, except iban.com's missing-IBAN
answer (below). Request bodies are decoded loosely: malformed JSON is read
as an empty object rather than refused, and only the fields named per route
are read.

The four outcome switches (see [`POST /sim/outcome`](#post-simoutcome))
decide what the checks answer:

| Check | Default | Moves |
| --- | --- | --- |
| `bank-account` | `pass` | Creditsafe bank verification, iban.com |
| `company` | `active` | Creditsafe company report |
| `screening` | `clear` | KYC6 screens |
| `identity` | `pass` | LexisNexis IDU and IVI |

### Creditsafe

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

### iban.com

The base URL is the endpoint. Auth is an `x-api-key` header, checked inside
the handler.

### `POST /iban/verify`

IBAN validity, BIC and name match.

Header: `x-api-key` (any non-empty value; `401`
`{"error":"x-api-key header required","status":401}` without it).

Body: `{"IBAN": "GB00SIMMERCH0000001", "name": "Southwind Coffee Ltd"}` (the
key is upper-case `IBAN`).

Answers `200`:

```json
{"query": {"IBAN": "GB00SIMMERCH0000001", "name": "Southwind Coffee Ltd", "success": true}, "result": {"valid": true, "name_match": "yes", "bic": "SIMBGB2L"}}
```

`bank-account` outcome: `mismatch` gives `valid: true`, `name_match: "no"`;
`invalid` gives `valid: false`, `name_match: "no"`. No checksum is computed:
under `pass` any string is valid. The BIC is fixed. A body with no `IBAN` is
answered `200` with `{"error": "IBAN is required"}` (no `status` field).

### KYC6

Sanctions, PEP and adverse-media screening, and ongoing monitoring. Every
route needs an `x-api-key` header (any non-empty value; `401`
`{"error":"x-api-key header required","status":401}` without it).

### `POST /kyc6/individuals`

Screens a person.

Body: only `name` is read, and only to fill a hit.
`{"name": "Ada Lovelace"}`.

Answers `200`; clear means zero matches:

```json
{"results": {"matchCount": 0, "matches": []}}
```

With the `screening` outcome at `hit`:

```json
{"results": {"matchCount": 1, "matches": [{"resourceId": "sim-match-1", "name": "Ada Lovelace", "score": 0.92, "datasets": ["SANCTIONS"], "matchTypes": ["name"]}]}}
```

### `POST /kyc6/businesses`

Screens a business. Same handler, body and answers as
`POST /kyc6/individuals`.

### `GET /kyc6/individuals/{resourceId}`

A match's profile. Answers `200` with
`{"resourceId": "<the path id>", "datasets": ["SANCTIONS"]}` for any id.

### `GET /kyc6/businesses/{resourceId}`

Same as `GET /kyc6/individuals/{resourceId}`.

### `PUT /kyc6/worklists/{worklistId}`

Creates a monitoring worklist.

Body: `{"name": "Infinite merchants"}`.

Answers `200` with `{"worklistId": "<the path id>", "name": "Infinite merchants"}`.
Nothing is stored.

### `GET /kyc6/lifecycle/ongoing-monitoring/{alertId}`

An ongoing-monitoring alert. Answers `200` with
`{"alertId": "<the path id>", "status": "closed", "matches": []}`, always.

### LexisNexis

OAuth first, then the identity report (IDU) with the bearer token. The IVI
attribute query carries its credentials in the body instead.

### `POST /lexisnexis/oauth/token`

Issues an access token.

Body, JSON: `{"client_id": "sim", "client_secret": "sim-verification-dev-only"}`.
Only a JSON body is decoded, so a form-encoded request is refused like a
missing credential.

Answers `200`:

```json
{"access_token": "ln_1794039000123456789", "expires_in": 3600, "token_type": "Bearer"}
```

`401` `{"error":"client_id and client_secret required","status":401}` when
either is missing or empty; `401` `invalid client_id or client_secret` for a
customer's client id with another secret. A customer's pair is its
environment's, any other the default's, and so is the token. `expires_in`
is advisory: the token never expires here.

### `POST /lexisnexis/idu/v1/reports`

Identity report. Bearer token required (same `401`s as Creditsafe).

Body: only `reference` is read. `{"reference": "ref-1"}`.

Answers `200`:

```json
{"data": {"id": "sim-idu-ref-1", "status": "PASS", "assessment": {"result": "PASS", "score": 850}, "context": {"pass_threshold": 700}, "attributes": {"dob_count": 1}}}
```

`identity` outcome: `refer` gives `REFER` with score 500, `fail` gives
`FAIL` with score 100, in both `status` and `assessment.result`.

### `POST /lexisnexis/ivi/api/attribute-query`

Identity verification attribute query. No header auth.

Body: `org_id`, `api_key` and `policy` are required (any non-empty);
`reference` is echoed. A customer's `api_key` (its login's password) makes
the query that environment's.

```json
{"org_id": "sim", "api_key": "sim-verification-dev-only", "policy": "default", "reference": "ref-1"}
```

Answers `200`:

```json
{"request_id": "sim-ivi-ref-1", "request_result": {"review_status": "PASS", "policy_score": 850}, "integration_hub_results": [{"Products": [{"ProductStatus": "PASS"}]}]}
```

`401` `{"error":"org_id, api_key and policy required","status":401}` when
any is missing. `identity` outcome as for IDU (`REFER`/500, `FAIL`/100).

## Lab control

Hooks a test, agent or operator uses to observe or steer the sim. They do
not exist at the real vendors. They are on the control port, `:9089`
([the control contract](../README.md#the-control-contract)); the same
`/sim/*` paths still answer on `:8089` until phase 4 of
[the plan](../../lab-api/plan.md) retires them.

### `GET /health`

Liveness, on both ports. `:8089` answers `{"status": "ok", "service":
"verification"}`; the control port answers `{"status": "ok", "service":
"verification", "surface": "control"}`, and that is the one the lab API
probes.

### `GET /sim/outcome`

What each check currently answers, in the environment the call names
(`?env=` or `Lab-Environment`; the default without either).

Answers `200`:

```json
{"env": "default", "outcomes": {"bank-account": "pass", "company": "active", "identity": "pass", "screening": "clear"}}
```

With `?env=*`, every environment's, the default's included:

```json
{"env": "*", "environments": {"default": {"bank-account": "pass", "…": "…"}, "sbx": {"screening": "hit", "…": "…"}}}
```

`404` `no environment <env> at verification` for one this service was
never given.

### `POST /sim/outcome`

Flips one check in the environment the call names (`?env=` or
`Lab-Environment`; the default without either). It applies to every later
call made with that environment's login until it is set back, the
environment is reset ([`POST /sim/reset`](#post-simreset)), or the process
restarts. Another environment's answers do not move.

Body, decoded strictly (an unknown field is a 400):
`{"check": "screening", "outcome": "hit"}`.

| `check` | `outcome` |
| --- | --- |
| `bank-account` | `pass`, `mismatch`, `invalid` |
| `screening` | `clear`, `hit` |
| `identity` | `pass`, `refer`, `fail` |
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

### `GET /sim/info`

Who this is and what it can do, as every service answers it
([the control contract](../README.md#the-control-contract)):
`"service": "verification"`, `"kind": "vendor"`,
`"reset_scopes": ["flow", "all"]`, `"environments": true` and
`"customers"`, how many there are.

### `GET /sim/logs`

The process's own log lines, as every service answers it
([the control contract](../README.md#the-control-contract)): one line per
request, and every flip of an outcome.

### `POST /sim/reset`

Puts every outcome back to its default, in the environment the reset
names (`"env"` in the body, `?env=` or `Lab-Environment`; the default
without any), or in every one for `*`. A flipped check is a run's trap; a
token is something the platform obtained once and caches, so `flow` keeps
it.

- **`flow` clears**: the outcomes, back to the defaults in the table under
  [Vendor surface](#vendor-surface), and the registered Creditsafe
  companies.
- **`flow` keeps**: every bearer token issued, so a platform holding one
  is not refused mid-session; and the Creditsafe portfolio id counter.
- **`all`** is first start: the outcomes, every token issued in the
  environment (a held one is `401` `unknown or expired token` until the
  client authenticates again), and its portfolio ids count from 1 again.

`cleared` counts `outcomes` that were flipped and `companies` that were
registered, and with `all`, `tokens`
and `portfolios` created. Another environment's outcomes and tokens are
left alone.

```json
{"service": "verification", "scope": "flow", "env": "default", "cleared": {"companies": 1, "outcomes": 2},
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

Removes one customer and what it had here: its outcomes and every token
issued to it. Its login is then a stranger's, the default's.

Answers `200` `{"env": "sbx", "deleted": true}`; `404`
`no environment sbx here`.

## Called by

- **The platform** under test: its own Creditsafe, iban.com, KYC6 and
  LexisNexis clients, through the variables in the table above. Nothing in
  the lab calls the vendor routes.
- **lab API** (`cmd/labapi`): only the control port,
  `http://verification:9089`. `GET /v1/lab` and
  `POST /v1/services/verification/probe` probe its `GET /health`.
  `POST /v1/lab/reset` and `POST /v1/services/verification/reset` call
  `POST /sim/reset`; `GET /v1/logs` and
  `GET /v1/services/verification/logs` read `GET /sim/logs`. No dedicated
  route calls `/sim/outcome`; flip outcomes through the passthrough,
  `/v1/services/verification/sim/outcome`, which names the caller's
  environment. The lab API pushes each environment's customer with
  `PUT /sim/customers/{env}` and reads `GET /sim/customers` for its
  status.
  `GET /v1/connect/env/verification` hands out the variables without
  calling this service.
- **e2e suite**: no scenario calls this service.
- **An operator or agent**: `GET` and `POST /sim/outcome` to arm a failure,
  through the lab API or on the control port.

Every route above is held to the source by `internal/console/apidocs_test.go`: a route added, moved or removed without this page following fails `go test ./internal/console`.
