# AML decision — API

Simulates the merchant-verification service that decides whether a
merchant application may proceed: keyed on an integer
`merchantApplicationId`, its decision is what advances a merchant to
`UNDERWRITING_APPROVED` (and can later pause settlements).
It is a **stand-in** ([catalogue.md](../../catalogue.md#which-half-is-which)):
it simulates the platform's own internal verification-service, not a third
party, and the lab API's connect kit says pointing a platform here
*replaces* that service. The console still shows it on the Verification
view, next to the KYC vendors, because that is where an operator flips a
check. What is real: the
`/api/v1/verification` path prefix, the `x-internal-api-key` guard with a
constant-time comparison, the trigger-then-read contract (202, then 404
until the decision is ready), and the decision's field names and enum
values. What is simplified: there is no judgement and no vendor call behind
it. Every decision is `APPROVED`/`LOW` unless the id is force-declined by
configuration, `manualOverrideDecision` is never set, and decisions live in
memory only.

## At a glance

| | |
| --- | --- |
| Listens | `:8088`, plain HTTP (the real service is plain HTTP in local development too): the platform's routes. Compose service `verify` (`http://verify:8088` on the compose network); host `http://127.0.0.1:8088`.<br>`:9088`, plain HTTP: the **control port** (`SIM_CONTROL_LISTEN`), every `/sim/*` route below and its own `GET /health`. Not published by compose; the lab API reaches it at `http://verify:9088` |
| Auth | `x-internal-api-key` header equal to `VERIFICATION_INTERNAL_API_KEY` (`sim-verify-key-dev-only`) on both `/verify/*` routes. The health route is public. The control port has none: it takes calls from anything that can reach it, or only from `SIM_CONTROL_ALLOW` (addresses or CIDRs) when that is set |
| A platform points here with | `VERIFICATION_SERVICE_API_URL` (`http://127.0.0.1:8088/api/v1/verification`) and `VERIFICATION_INTERNAL_API_KEY`. the lab API's connect kit (`GET :8097/v1/connect/env/verify`) ships both commented out, because they replace the platform's own service. The settlement stand-in uses `VERIFY_URL` (`http://verify:8088`, no prefix) and `VERIFY_INTERNAL_API_KEY` |
| Activity logs | None. There is no `GET /sim/activity`. Each request line is logged on stdout |
| Configuration | `LISTEN` (`:8088`).<br>`SIM_CONTROL_LISTEN` (`LISTEN`'s port plus 1000, `:9088`) and `SIM_CONTROL_ALLOW` (empty: any caller): the control port.<br>`VERIFICATION_INTERNAL_API_KEY` (`sim-verify-key-dev-only`): the key every `/verify/*` call must carry.<br>`VERIFY_PROCESSING_DELAY` (`100ms`): how long after a trigger the decision becomes readable. Settlement polls for about 500 ms and then carries on as if approved, so a longer delay makes its gate let everything through.<br>`VERIFY_FORCE_DECLINE_IDS` (empty): comma-separated integer `merchantApplicationId`s that decide `DECLINE`; non-integers are logged and skipped. Settlement derives the id from the merchant id (FNV-32a, read as a signed 32-bit integer and made positive: `fnvPositive` in `cmd/settlement/main.go`), so that number is the one to list.<br>`LAB_CLOCK_URL` (compose: `http://clock:8096/clock`): `completedAt` follows the [lab clock](../../plugins.md#the-clock) |
| Design record | [ARCHITECTURE-phase3-corrections.md](../../ARCHITECTURE-phase3-corrections.md) §3 (the service) and §5 (settlement's gate), [catalogue.md](../../catalogue.md) |

## Vendor surface

The routes the platform's own verification-service exposes, with the same
prefix and shapes. Errors have one shape:
`{"error": "<message>", "status": <code>}`. A missing or wrong key on either
`/verify/*` route is `401`
`{"error":"missing or invalid x-internal-api-key","status":401}`.

### `GET /api/v1/verification/health`

Liveness, public like the real service's health route. The lab API probes
the control port's `GET /health` instead.

Answers `200` with `{"status": "ok", "service": "verify"}`.

### `POST /api/v1/verification/verify/aml-decision`

Starts the AML decision for one merchant application.

Header: `x-internal-api-key`.

Body, decoded strictly (an unknown field is a 400):

```json
{"merchantApplicationId": 123}
```

Answers `202` with the decision it is about to record:

```json
{"merchantApplicationId": 123, "journeys": [{"journeyType": "AML", "overallDecision": "APPROVED", "riskScore": 5, "riskClassification": "LOW"}]}
```

A force-declined id answers `"overallDecision": "DECLINE"`, `riskScore` 95,
`riskClassification` `"HIGH"`.

- `400`: malformed JSON, an unknown field, a non-integer id
  (`json: cannot unmarshal string into Go struct field …`), or a missing or
  zero id (`merchantApplicationId required`).
- `401`: the key.

Side effects: after `VERIFY_PROCESSING_DELAY` the decision is recorded with
`completedAt` from the lab clock, and only then can it be read. Triggering
the same id again records a fresh decision. In memory only.

### `GET /api/v1/verification/verify/data/verify-decision/{merchantApplicationId}`

Reads a recorded decision. Callers poll this after the trigger.

Path: `merchantApplicationId`, an integer.

Header: `x-internal-api-key`.

Answers `200`:

```json
{
  "overallDecision": "APPROVED",
  "amlDecision": "APPROVED",
  "amlCompanyDecision": "APPROVED",
  "riskDecision": "LOW",
  "riskCompanyDecision": "LOW",
  "riskScore": 5,
  "riskClassification": "LOW",
  "completedAt": "2026-11-07T09:30:00Z"
}
```

A force-declined id reads `overallDecision` `"DECLINE"`, `amlDecision` and
`amlCompanyDecision` `"DECLINED"`, `riskDecision`, `riskCompanyDecision` and
`riskClassification` `"HIGH"`, `riskScore` 95. `manualOverrideDecision` is
always omitted.

- `400`: `merchantApplicationId must be an integer`.
- `401`: the key.
- `404`: `decision not found`, for an id never triggered or one whose
  processing delay has not yet elapsed.

## Lab control

The sim control contract, on the control port, `:9088` ([the control
contract](../README.md#the-control-contract)). No `x-internal-api-key`, and
not under the `/api/v1/verification` prefix: the real service has neither
route. The same `/sim/*` paths still answer on `:8088` until phase 4 of
[the plan](../../lab-api/plan.md) retires them.

### `GET /health`

The control port's liveness: `{"status": "ok", "service": "verify",
"surface": "control"}`. The lab API's monitor probes it; the service port's
own `GET /api/v1/verification/health` is the platform's.

### `GET /sim/info`

Who this is and what it can do, as every service answers it
([the control contract](../README.md#the-control-contract)):
`"service": "verify"`, `"kind": "stand-in"`, `"reset_scopes": ["flow", "all"]`.

### `GET /sim/logs`

The process's own log lines, as every service answers it
([the control contract](../README.md#the-control-contract)): one line per
request, and each skipped `VERIFY_FORCE_DECLINE_IDS` entry at start.

### `POST /sim/reset`

Forgets every decision. Both scopes do the same: verify has nothing a
platform set up once.

- **Clears**: every recorded decision, so a read is `404` again; and every
  trigger still sleeping out `VERIFY_PROCESSING_DELAY`, which is dropped,
  never recorded after the reset.
- **Keeps**: `VERIFY_FORCE_DECLINE_IDS`. It is configuration: a
  force-declined id still declines after a reset.

`cleared` counts recorded `decisions` and `pending` triggers.

```json
{"service": "verify", "scope": "flow", "cleared": {"decisions": 3, "pending": 1},
 "kept": ["force_decline_ids"], "at": "2026-11-07T12:00:00Z"}
```

## Called by

- **settlement** (scaffolding, `cmd/settlement`), as its merchant-verification
  gate before every B4B payout (`checkVerification`): `POST …/verify/aml-decision`
  with the derived id, then `GET …/verify-decision/{id}` every 50 ms, up to
  ten times. `APPROVE`, `APPROVED` or `PRE_APPROVE` lets the payout proceed;
  any other decision sets the record's payout state to
  `verification_declined`. An unreachable service, a non-200 answer or a
  decision that never becomes ready is treated as approved.
- **lab API** (`cmd/labapi`): only the control port,
  `http://verify:9088`. `GET /v1/lab` and `POST /v1/services/verify/probe`
  probe its `GET /health`. `POST /v1/lab/reset` and
  `POST /v1/services/verify/reset` call `POST /sim/reset`; `GET /v1/logs`
  and `GET /v1/services/verify/logs` read `GET /sim/logs`; the passthrough
  `/v1/services/verify/sim/*` forwards to `/sim/*`.
  `GET /v1/connect/env/verify` hands out the variables without calling this
  service.
- **harness**: no scenario calls it directly; `banking-circle-payout`
  (`internal/harness/scenario_bankingcircle.go`) passes through it via
  settlement's payout gate.
- **The platform** under test, optionally, in place of its own
  verification-service, through `VERIFICATION_SERVICE_API_URL` and
  `VERIFICATION_INTERNAL_API_KEY`.

Every route above is held to the source by `internal/console/apidocs_test.go`: a route added, moved or removed without this page following fails `go test ./internal/console`.
