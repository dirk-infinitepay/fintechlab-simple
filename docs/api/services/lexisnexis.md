# LexisNexis — API

Simulates LexisNexis Risk Solutions: OAuth, then IDU identity reports and IVI attribute queries. One process, its own sim (`cmd/lexisnexis`, sharing
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
| Listens | `:8093`, plain HTTP: the vendor routes. Compose service `lexisnexis` (`http://lexisnexis:8093` on the compose network); host `http://127.0.0.1:8093`.<br>`:9093`, plain HTTP: the **control port** (`SIM_CONTROL_LISTEN`), every `/sim/*` route below and its own `GET /health`. Not published by compose; the lab API reaches it at `http://lexisnexis:9093` |
| Auth | `POST /lexisnexis/oauth/token` (client id and secret) issues a bearer token for IDU; IVI carries `org_id`, `api_key` and `policy` in the body. Any non-empty credential is accepted: an environment's own login makes the call that environment's, and any other is the default environment's ([Environments](#environments)). A customer's username with another password is `401`. `/health` and `/sim/*`: none. The control port has none: it takes calls from anything that can reach it, or only from `SIM_CONTROL_ALLOW` (addresses or CIDRs) when that is set |
| A platform points here with | `LEXISNEXIS_AUTH_URL` (`http://127.0.0.1:8093/lexisnexis`), `LEXISNEXIS_IDU_BASE_URL` (`…/lexisnexis/idu`), `LEXISNEXIS_IVI_BASE_URL` (`…/lexisnexis/ivi`), `LEXISNEXIS_CLIENT_ID`, `LEXISNEXIS_CLIENT_SECRET`, `LEXISNEXIS_IVI_API_KEY`. The lab API hands these out at `GET :8097/v1/connect/env/lexisnexis` |
| Activity logs | `GET /sim/activity`: the request journal, every vendor request and its answer, and the traps log |
| Configuration | `LISTEN` (`:8093`).<br>`SIM_CONTROL_LISTEN` (`LISTEN`'s port plus 1000, `:9093`) and `SIM_CONTROL_ALLOW` (empty: any caller): the control port.<br>Nothing else: no lab clock, no state file. The outcomes, per environment, every issued token and the customers live in memory and reset on restart; the lab API pushes the customers again |
| Design record | [integration/verification-vendor-edge.md](../../integration/verification-vendor-edge.md), the package comments in `cmd/lexisnexis` and `internal/vendorsim`, [catalogue.md](../../catalogue.md) |

## Environments

Each environment the lab creates
([lab-api/environments.md](../../lab-api/environments.md)) is a customer
here, pushed by the lab API with
[`PUT /sim/customers/{env}`](#put-simcustomersenv) and known by its
`verification` login: `username` is the `client_id`; `password` is the `client_secret` and IVI's `api_key`.

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

What the platform's LexisNexis client calls. Errors this service writes
have one shape, `{"error": "<message>", "status": <code>}`. Request bodies
are decoded loosely: malformed JSON is read as an empty object rather than
refused, and only the fields named per route are read.

| Check | Default | Moves |
| --- | --- | --- |
| `identity` | `pass` | IDU and IVI |

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

Identity report. Bearer token required: `401` `bearer token required` without one, `401` `unknown or expired token` for one this sim did not issue.

Body: only `reference` is read. `{"reference": "ref-1"}`.

Answers `200`:

```json
{"data": {"id": "sim-idu-ref-1", "status": "PASS", "assessment": {"result": "PASS", "score": 850}, "context": {"pass_threshold": 700},
          "attributes": {"dob_count": 1, "address_verified": true, "sanction": false, "pep": false, "insolvency": false,
                         "ccj": false, "death_ddri": false, "death_gro": false, "death_halo": false}}}
```

`attributes` is a clean person: the address is verified and no sanction,
PEP, insolvency, CCJ or death register flags them. The `identity` outcome
leaves them as they are.

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
not exist at the real vendor. They are on the control port, `:9093`
([the control contract](../README.md#the-control-contract)); the same
`/sim/*` paths still answer on `:8093` until phase 4 of
[the plan](../../lab-api/plan.md) retires them.

### `GET /health`

Liveness, on both ports. `:8093` answers `{"status": "ok", "service":
"lexisnexis"}`; the control port answers `{"status": "ok", "service":
"lexisnexis", "surface": "control"}`, and that is the one the lab API
probes.

### `GET /sim/outcome`

What each check currently answers, in the environment the call names
(`?env=` or `Lab-Environment`; the default without either).

Answers `200`:

```json
{"env": "default", "outcomes": {"identity": "pass"}}
```

With `?env=*`, every environment's, the default's included. `404`
`no environment <env> at lexisnexis` for one this service was never given.

### `POST /sim/outcome`

Flips one check in the environment the call names (`?env=` or
`Lab-Environment`; the default without either). It applies to every later
call made with that environment's login until it is set back, the
environment is reset ([`POST /sim/reset`](#post-simreset)), or the process
restarts. Another environment's answers do not move.

Body, decoded strictly (an unknown field is a 400): `{"check": "identity", "outcome": "refer"}`.

| `check` | `outcome` |
| --- | --- |
| `identity` | `pass`, `refer`, `fail` |

Answers `200` with the environment's every outcome, as `GET /sim/outcome`
does. `400` for an unknown check or outcome, which changes nothing:
`{"error":"check \"identity\" cannot be \"maybe\" (want: pass, refer, fail)","status":400}`;
`400` `an outcome is set in one environment, not in all of them` for
`env=*`; `404` for an environment this service was never given.

### `GET /sim/traps/hooks`

The catalogue: one hook, `http.request`, on every vendor request
([traps.md §9.7](../../lab-api/traps.md#97-every-sim-httprequest)), with
the `respond` effect ([§4.3](../../lab-api/traps.md#43-respond-answering-a-vendor-shaped-request)).

```json
{"service": "lexisnexis",
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

Answers `200` with `{"service": "lexisnexis", "traps": [<trap>, …]}`.

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

The next LexisNexis identity report is an outage:

```json
{"hook": "http.request", "match": {"method": "POST", "path": "/lexisnexis/idu/v1/reports"},
 "effect": {"respond": {"status": 503, "body": {"error": "Service Unavailable"}}}}
```

Answers `201` with the trap, `effect` as armed. `400` with the reason: a
hook or match field lexisnexis does not have, an effect that is not
exactly one of the three, a status outside 100 to 599, or a delay that is
not a duration.

### `GET /sim/traps/{id}`

One trap. Each request it caught is in `caught`, with what was done to it:

```json
{"id": "trp_0001", "…": "as armed", "state": "spent",
 "caught": [{"subject": "POST /lexisnexis/idu/v1/reports", "at": "2026-11-12T09:40:00Z",
             "applied": [{"to": "respond 503", "at": "2026-11-12T09:40:00Z"}]}],
 "pending_steps": []}
```

`404` for an unknown id.

### `DELETE /sim/traps/{id}`

Disarms one trap. Answers `200` with `{"trap": <trap>, "cancelled": []}`,
`404` for an unknown id.

### `DELETE /sim/traps`

Disarms every trap. Answers `200` with `{"service": "lexisnexis",
"disarmed": 1, "cancelled": []}`.

### `GET /sim/activity`

Two logs: `requests`, the journal, and `traps`, every trap armed, every
request one caught and what was done to it. A request a trap answers is
journalled with the trap's answer; one a trap drops (`close`) is `bad`,
`connection dropped`.

The request journal, newest first: every request the platform made to a
vendor on `:8093`, and the status it was answered with. Query: `limit`
(positive integer, default 100); `env` (the default when left out, `*` for
every environment's); `404` for an environment this service was never
given. A request is the environment its credentials name, a
login included; one the vendor refused is the default's.

Answers `200`:

```json
{"logs": [{"name": "requests", "title": "Vendor requests",
           "note": "Every request the platform made to LexisNexis, and the status it was answered with.",
           "total": 1, "kept": 1,
           "last": {"seq": 1, "at": "2026-11-12T09:40:00Z", "op": "lexisnexis", "peer": "10.89.0.7",
                    "summary": "POST /lexisnexis/idu/v1/reports", "status": "ok", "detail": {"status": "OK"}},
           "events": [{"seq": 1, "…": "as last"}]}]}
```

`op` is `lexisnexis`. A `4xx` answer is `warn` and carries the vendor's error in the
summary. The journal keeps the first 64 KiB of a request body; the
vendor reads it all.

### `GET /sim/info`

Who this is and what it can do, as every service answers it
([the control contract](../README.md#the-control-contract)):
`"service": "lexisnexis"`, `"kind": "vendor"`,
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
- **`flow` keeps**: every bearer token issued, so a platform holding one is not refused mid-session.
- **`all`** is first start: the outcomes and every token issued in the environment (a held one is `401` `unknown or expired token` until the client asks for another).

`cleared` counts `outcomes` that were flipped, journal events it held (`activity`, at most 256 per environment), and the `traps` disarmed (with `trap_steps`, always 0 here), and with `all`, `tokens`. Another environment's state is left
alone.

```json
{"service": "lexisnexis", "scope": "flow", "env": "default", "cleared": {"activity": 5, "outcomes": 1, "trap_steps": 0, "traps": 1},
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

- **The platform** under test: its own LexisNexis client, through the
  variables in the table above. Nothing in the lab calls the vendor routes.
- **lab API** (`cmd/labapi`): only the control port, `http://lexisnexis:9093`.
  `GET /v1/lab` and `POST /v1/services/lexisnexis/probe` probe its
  `GET /health`. `POST /v1/lab/reset` and `POST /v1/services/lexisnexis/reset`
  call `POST /sim/reset`; `GET /v1/logs` and `GET /v1/services/lexisnexis/logs`
  read `GET /sim/logs`; every other `/sim/*` route is reached through
  `/v1/services/lexisnexis/sim/*`.
