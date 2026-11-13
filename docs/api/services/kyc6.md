# KYC6 — API

Simulates Acuris KYC6: sanctions, PEP and adverse-media screening, profile lookups and monitoring worklists. One process, its own sim (`cmd/kyc6`, sharing
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
| Listens | `:8092`, plain HTTP: the vendor routes. Compose service `kyc6` (`http://kyc6:8092` on the compose network); host `http://127.0.0.1:8092`.<br>`:9092`, plain HTTP: the **control port** (`SIM_CONTROL_LISTEN`), every `/sim/*` route below and its own `GET /health`. Not published by compose; the lab API reaches it at `http://kyc6:9092` |
| Auth | an `x-api-key` header on every route. Any non-empty credential is accepted: an environment's own login makes the call that environment's, and any other is the default environment's ([Environments](#environments)). A customer's username with another password is `401`. `/health` and `/sim/*`: none. The control port has none: it takes calls from anything that can reach it, or only from `SIM_CONTROL_ALLOW` (addresses or CIDRs) when that is set |
| A platform points here with | `KYC6_API_BASE_URL` and `KYC6_MONITOR_API_BASE_URL` (both `http://127.0.0.1:8092/kyc6`), `KYC6_API_KEY`. The lab API hands these out at `GET :8097/v1/connect/env/kyc6` |
| Activity logs | `GET /sim/activity`: the request journal, every vendor request and its answer, and the traps log |
| Configuration | `LISTEN` (`:8092`).<br>`SIM_CONTROL_LISTEN` (`LISTEN`'s port plus 1000, `:9092`) and `SIM_CONTROL_ALLOW` (empty: any caller): the control port.<br>Nothing else: no lab clock, no state file. The outcomes, per environment, every issued token and the customers live in memory and reset on restart; the lab API pushes the customers again |
| Design record | [integration/verification-vendor-edge.md](../../integration/verification-vendor-edge.md), the package comments in `cmd/kyc6` and `internal/vendorsim`, [catalogue.md](../../catalogue.md) |

## Environments

Each environment the lab creates
([lab-api/environments.md](../../lab-api/environments.md)) is a customer
here, pushed by the lab API with
[`PUT /sim/customers/{env}`](#put-simcustomersenv) and known by its
`verification` login: the customer's `password` is the `x-api-key`.

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

What the platform's KYC6 client calls. Errors this service writes
have one shape, `{"error": "<message>", "status": <code>}`. Request bodies
are decoded loosely: malformed JSON is read as an empty object rather than
refused, and only the fields named per route are read.

| Check | Default | Moves |
| --- | --- | --- |
| `screening` | `clear` | every screen |

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
{"results": {"matchCount": 1, "matches": [{"resourceId": "sim-match-1", "name": "Ada Lovelace", "score": 0.92, "datasets": ["SAN-CURRENT"], "matchTypes": ["name"]}]}}
```

`SAN-CURRENT` is KYC6's dataset key for a current sanctions listing.

### `POST /kyc6/businesses`

Screens a business. Same handler, body and answers as
`POST /kyc6/individuals`.

### `GET /kyc6/individuals/{resourceId}`

A match's profile. Answers `200` with
`{"resourceId": "<the path id>", "datasets": ["SAN-CURRENT"]}` for any id.

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

## Lab control

Hooks a test, agent or operator uses to observe or steer the sim. They do
not exist at the real vendor. They are on the control port, `:9092`
([the control contract](../README.md#the-control-contract)); the same
`/sim/*` paths still answer on `:8092` until phase 4 of
[the plan](../../lab-api/plan.md) retires them.

### `GET /health`

Liveness, on both ports. `:8092` answers `{"status": "ok", "service":
"kyc6"}`; the control port answers `{"status": "ok", "service":
"kyc6", "surface": "control"}`, and that is the one the lab API
probes.

### `GET /sim/outcome`

What each check currently answers, in the environment the call names
(`?env=` or `Lab-Environment`; the default without either).

Answers `200`:

```json
{"env": "default", "outcomes": {"screening": "clear"}}
```

With `?env=*`, every environment's, the default's included. `404`
`no environment <env> at kyc6` for one this service was never given.

### `POST /sim/outcome`

Flips one check in the environment the call names (`?env=` or
`Lab-Environment`; the default without either). It applies to every later
call made with that environment's login until it is set back, the
environment is reset ([`POST /sim/reset`](#post-simreset)), or the process
restarts. Another environment's answers do not move.

Body, decoded strictly (an unknown field is a 400): `{"check": "screening", "outcome": "hit"}`.

| `check` | `outcome` |
| --- | --- |
| `screening` | `clear`, `hit` |

Answers `200` with the environment's every outcome, as `GET /sim/outcome`
does. `400` for an unknown check or outcome, which changes nothing:
`{"error":"check \"screening\" cannot be \"maybe\" (want: clear, hit)","status":400}`;
`400` `an outcome is set in one environment, not in all of them` for
`env=*`; `404` for an environment this service was never given.

### `GET /sim/traps/hooks`

The catalogue: one hook, `http.request`, on every vendor request
([traps.md §9.7](../../lab-api/traps.md#97-every-sim-httprequest)), with
the `respond` effect ([§4.3](../../lab-api/traps.md#43-respond-answering-a-vendor-shaped-request)).

```json
{"service": "kyc6",
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

Answers `200` with `{"service": "kyc6", "traps": [<trap>, …]}`.

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

The next KYC6 business screen is an outage:

```json
{"hook": "http.request", "match": {"method": "POST", "path": "/kyc6/businesses"},
 "effect": {"respond": {"status": 503, "body": {"error": "Service Unavailable"}}}}
```

Answers `201` with the trap, `effect` as armed. `400` with the reason: a
hook or match field kyc6 does not have, an effect that is not
exactly one of the three, a status outside 100 to 599, or a delay that is
not a duration.

### `GET /sim/traps/{id}`

One trap. Each request it caught is in `caught`, with what was done to it:

```json
{"id": "trp_0001", "…": "as armed", "state": "spent",
 "caught": [{"subject": "POST /kyc6/businesses", "at": "2026-11-12T09:40:00Z",
             "applied": [{"to": "respond 503", "at": "2026-11-12T09:40:00Z"}]}],
 "pending_steps": []}
```

`404` for an unknown id.

### `DELETE /sim/traps/{id}`

Disarms one trap. Answers `200` with `{"trap": <trap>, "cancelled": []}`,
`404` for an unknown id.

### `DELETE /sim/traps`

Disarms every trap. Answers `200` with `{"service": "kyc6",
"disarmed": 1, "cancelled": []}`.

### `GET /sim/activity`

Two logs: `requests`, the journal, and `traps`, every trap armed, every
request one caught and what was done to it. A request a trap answers is
journalled with the trap's answer; one a trap drops (`close`) is `bad`,
`connection dropped`.

The request journal, newest first: every request the platform made to a
vendor on `:8092`, and the status it was answered with. Query: `limit`
(positive integer, default 100); `env` (the default when left out, `*` for
every environment's); `404` for an environment this service was never
given. A request is the environment its credentials name, a
login included; one the vendor refused is the default's.

Answers `200`:

```json
{"logs": [{"name": "requests", "title": "Vendor requests",
           "note": "Every request the platform made to KYC6, and the status it was answered with.",
           "total": 1, "kept": 1,
           "last": {"seq": 1, "at": "2026-11-12T09:40:00Z", "op": "kyc6", "peer": "10.89.0.7",
                    "summary": "POST /kyc6/businesses", "status": "ok", "detail": {"status": "OK"}},
           "events": [{"seq": 1, "…": "as last"}]}]}
```

`op` is `kyc6`. A `4xx` answer is `warn` and carries the vendor's error in the
summary. The journal keeps the first 64 KiB of a request body; the
vendor reads it all.

### `GET /sim/info`

Who this is and what it can do, as every service answers it
([the control contract](../README.md#the-control-contract)):
`"service": "kyc6"`, `"kind": "vendor"`,
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

- **`flow` clears**: the outcomes, every trap and the request journal.
- **`flow` keeps**: nothing else: KYC6 issues no tokens.
- **`all`** is first start: the same as `flow`.

`cleared` counts `outcomes` that were flipped, journal events it held (`activity`, at most 256 per environment), and the `traps` disarmed (with `trap_steps`, always 0 here), and with `all`, `tokens` (always 0 here). Another environment's state is left
alone.

```json
{"service": "kyc6", "scope": "flow", "env": "default", "cleared": {"activity": 5, "outcomes": 1, "trap_steps": 0, "traps": 1},
 "kept": ["tokens"], "at": "2026-11-07T12:00:00Z"}
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

- **The platform** under test: its own KYC6 client, through the
  variables in the table above. Nothing in the lab calls the vendor routes.
- **lab API** (`cmd/labapi`): only the control port, `http://kyc6:9092`.
  `GET /v1/lab` and `POST /v1/services/kyc6/probe` probe its
  `GET /health`. `POST /v1/lab/reset` and `POST /v1/services/kyc6/reset`
  call `POST /sim/reset`; `GET /v1/logs` and `GET /v1/services/kyc6/logs`
  read `GET /sim/logs`; every other `/sim/*` route is reached through
  `/v1/services/kyc6/sim/*`.
