# iban.com — API

Simulates iban.com: bank-account validity and name match for accounts outside the UK. One process, its own sim (`cmd/ibancom`, sharing
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
| Listens | `:8094`, plain HTTP: the vendor routes. Compose service `iban-com` (`http://iban-com:8094` on the compose network); host `http://127.0.0.1:8094`.<br>`:9094`, plain HTTP: the **control port** (`SIM_CONTROL_LISTEN`), every `/sim/*` route below and its own `GET /health`. Not published by compose; the lab API reaches it at `http://iban-com:9094` |
| Auth | an `x-api-key` header. Any non-empty credential is accepted: an environment's own login makes the call that environment's, and any other is the default environment's ([Environments](#environments)). A customer's username with another password is `401`. `/health` and `/sim/*`: none. The control port has none: it takes calls from anything that can reach it, or only from `SIM_CONTROL_ALLOW` (addresses or CIDRs) when that is set |
| A platform points here with | `IBAN_COM_BASE_URL` (`http://127.0.0.1:8094/iban/verify`, the full endpoint), `IBAN_COM_API_KEY`. The lab API hands these out at `GET :8097/v1/connect/env/iban-com` |
| Activity logs | `GET /sim/activity`: the request journal, every vendor request and its answer, and the traps log |
| Configuration | `LISTEN` (`:8094`).<br>`SIM_CONTROL_LISTEN` (`LISTEN`'s port plus 1000, `:9094`) and `SIM_CONTROL_ALLOW` (empty: any caller): the control port.<br>Nothing else: no lab clock, no state file. The outcomes, per environment, every issued token and the customers live in memory and reset on restart; the lab API pushes the customers again |
| Design record | [integration/verification-vendor-edge.md](../../integration/verification-vendor-edge.md), the package comments in `cmd/ibancom` and `internal/vendorsim`, [catalogue.md](../../catalogue.md) |

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

What the platform's iban.com client calls. Errors this service writes
have one shape, `{"error": "<message>", "status": <code>}`. Request bodies
are decoded loosely: malformed JSON is read as an empty object rather than
refused, and only the fields named per route are read.

| Check | Default | Moves |
| --- | --- | --- |
| `bank-account` | `pass` | the account check |

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

## Lab control

Hooks a test, agent or operator uses to observe or steer the sim. They do
not exist at the real vendor. They are on the control port, `:9094`
([the control contract](../README.md#the-control-contract)); the same
`/sim/*` paths still answer on `:8094` until phase 4 of
[the plan](../../lab-api/plan.md) retires them.

### `GET /health`

Liveness, on both ports. `:8094` answers `{"status": "ok", "service":
"iban-com"}`; the control port answers `{"status": "ok", "service":
"iban-com", "surface": "control"}`, and that is the one the lab API
probes.

### `GET /sim/outcome`

What each check currently answers, in the environment the call names
(`?env=` or `Lab-Environment`; the default without either).

Answers `200`:

```json
{"env": "default", "outcomes": {"bank-account": "pass"}}
```

With `?env=*`, every environment's, the default's included. `404`
`no environment <env> at iban-com` for one this service was never given.

### `POST /sim/outcome`

Flips one check in the environment the call names (`?env=` or
`Lab-Environment`; the default without either). It applies to every later
call made with that environment's login until it is set back, the
environment is reset ([`POST /sim/reset`](#post-simreset)), or the process
restarts. Another environment's answers do not move.

Body, decoded strictly (an unknown field is a 400): `{"check": "bank-account", "outcome": "mismatch"}`.

| `check` | `outcome` |
| --- | --- |
| `bank-account` | `pass`, `mismatch`, `invalid` |

Answers `200` with the environment's every outcome, as `GET /sim/outcome`
does. `400` for an unknown check or outcome, which changes nothing:
`{"error":"check \"bank-account\" cannot be \"maybe\" (want: pass, mismatch, invalid)","status":400}`;
`400` `an outcome is set in one environment, not in all of them` for
`env=*`; `404` for an environment this service was never given.

### `GET /sim/traps/hooks`

The catalogue: one hook, `http.request`, on every vendor request
([traps.md §9.7](../../lab-api/traps.md#97-every-sim-httprequest)), with
the `respond` effect ([§4.3](../../lab-api/traps.md#43-respond-answering-a-vendor-shaped-request)).

```json
{"service": "iban-com",
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

Answers `200` with `{"service": "iban-com", "traps": [<trap>, …]}`.

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

The next IBAN.com check is an outage:

```json
{"hook": "http.request", "match": {"method": "POST", "path": "/iban/verify"},
 "effect": {"respond": {"status": 503, "body": {"error": "Service Unavailable"}}}}
```

Answers `201` with the trap, `effect` as armed. `400` with the reason: a
hook or match field iban-com does not have, an effect that is not
exactly one of the three, a status outside 100 to 599, or a delay that is
not a duration.

### `GET /sim/traps/{id}`

One trap. Each request it caught is in `caught`, with what was done to it:

```json
{"id": "trp_0001", "…": "as armed", "state": "spent",
 "caught": [{"subject": "POST /iban/verify", "at": "2026-11-12T09:40:00Z",
             "applied": [{"to": "respond 503", "at": "2026-11-12T09:40:00Z"}]}],
 "pending_steps": []}
```

`404` for an unknown id.

### `DELETE /sim/traps/{id}`

Disarms one trap. Answers `200` with `{"trap": <trap>, "cancelled": []}`,
`404` for an unknown id.

### `DELETE /sim/traps`

Disarms every trap. Answers `200` with `{"service": "iban-com",
"disarmed": 1, "cancelled": []}`.

### `GET /sim/activity`

Two logs: `requests`, the journal, and `traps`, every trap armed, every
request one caught and what was done to it. A request a trap answers is
journalled with the trap's answer; one a trap drops (`close`) is `bad`,
`connection dropped`.

The request journal, newest first: every request the platform made to a
vendor on `:8094`, and the status it was answered with. Query: `limit`
(positive integer, default 100); `env` (the default when left out, `*` for
every environment's); `404` for an environment this service was never
given. A request is the environment its credentials name, a
login included; one the vendor refused is the default's.

Answers `200`:

```json
{"logs": [{"name": "requests", "title": "Vendor requests",
           "note": "Every request the platform made to iban.com, and the status it was answered with.",
           "total": 1, "kept": 1,
           "last": {"seq": 1, "at": "2026-11-12T09:40:00Z", "op": "iban-com", "peer": "10.89.0.7",
                    "summary": "POST /iban/verify", "status": "ok", "detail": {"status": "OK"}},
           "events": [{"seq": 1, "…": "as last"}]}]}
```

`op` is `iban-com`. A `4xx` answer is `warn` and carries the vendor's error in the
summary, and so is iban.com's refusal, which it
answers `200` with an `error` field. The journal keeps the first 64 KiB of a request body; the
vendor reads it all.

### `GET /sim/info`

Who this is and what it can do, as every service answers it
([the control contract](../README.md#the-control-contract)):
`"service": "iban-com"`, `"kind": "vendor"`,
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
- **`flow` keeps**: nothing else: iban.com issues no tokens.
- **`all`** is first start: the same as `flow`.

`cleared` counts `outcomes` that were flipped, journal events it held (`activity`, at most 256 per environment), and the `traps` disarmed (with `trap_steps`, always 0 here), and with `all`, `tokens` (always 0 here). Another environment's state is left
alone.

```json
{"service": "iban-com", "scope": "flow", "env": "default", "cleared": {"activity": 5, "outcomes": 1, "trap_steps": 0, "traps": 1},
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

- **The platform** under test: its own iban.com client, through the
  variables in the table above. Nothing in the lab calls the vendor routes.
- **lab API** (`cmd/labapi`): only the control port, `http://iban-com:9094`.
  `GET /v1/lab` and `POST /v1/services/iban-com/probe` probe its
  `GET /health`. `POST /v1/lab/reset` and `POST /v1/services/iban-com/reset`
  call `POST /sim/reset`; `GET /v1/logs` and `GET /v1/services/iban-com/logs`
  read `GET /sim/logs`; every other `/sim/*` route is reached through
  `/v1/services/iban-com/sim/*`.
