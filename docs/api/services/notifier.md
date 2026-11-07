# Notifier (demo stand-in) — API

A webhook worker of the platform's own shape: it signs what it is given
with HMAC-SHA256 and POSTs it to one subscription URL over HTTPS, with
retries, a hard destination allowlist, and a subscription that is marked
failed once a delivery runs out of retries.
[catalogue.md](../../catalogue.md) files it as **scaffolding**, a demo
stand-in: a generic shape, not any vendor's. Only payment-api enqueues
anything, for `make demo-payment` and the e2e suite's first scenario.
Nothing on the settle path calls it, and nothing should depend on it.

## At a glance

| | |
| --- | --- |
| Listens | `:8082`, plain HTTP: the notifier's own routes. Compose service `notifier` (`http://notifier:8082`); host `http://127.0.0.1:8082`. Delivers out over HTTPS to its subscription URL.<br>`:9082`, plain HTTP: the **control port** (`SIM_CONTROL_LISTEN`), every `/sim/*` route below and its own `GET /health`. Not published by compose; the lab API reaches it at `http://notifier:9082` |
| Auth | None inbound. The control port takes calls from anything that can reach it, or only from `SIM_CONTROL_ALLOW` (addresses or CIDRs) when that is set. Outbound, each delivery carries the lab HMAC (`X-Sim-Signature`) keyed with `HMAC_SECRET`, and TLS is verified against `CA_FILE` |
| Found with | `NOTIFIER_URL` (`http://notifier:8082`) on payment-api; the e2e suite sets it too but no scenario uses it.<br>`CONSOLE_URL_NOTIFIER` on the console |
| Activity logs | `deliveries` (*Webhooks delivered*): one event per delivery attempt, op `delivery`, `ok` on a `2xx` and `warn` on a failed attempt; then `delivery.failed` (`bad`) when the retries run out and the subscription is marked failed. Enqueues are not logged, only the attempts |
| Configuration | `WEBHOOK_URL` (`https://receiver:8443/webhooks`): the default subscription. It must pass the allowlist or the service does not start.<br>`WEBHOOK_ALLOWLIST` (`receiver,receiver:8443,localhost,127.0.0.1`): hostnames, `host:port` pairs or CIDRs; `http` and `https` only, no userinfo; empty denies everything.<br>`HMAC_SECRET` (`sim-hmac-dev-only`): must equal the receiver's.<br>`CA_FILE` (`/keys/certs/ca.pem`): trusted for HTTPS; waited for 30 s, then system roots.<br>`RETRY_BACKOFF` (`1s,2s,4s`): the wait before attempts 2, 3, 4…, the last value repeating.<br>`MAX_ATTEMPTS` (`4`).<br>`LISTEN` (`:8082`).<br>`SIM_CONTROL_LISTEN` (`LISTEN`'s port plus 1000, `:9082`) and `SIM_CONTROL_ALLOW` (empty: any caller): the control port.<br>Everything is in memory. It does not follow the lab clock: `createdAt` and the signing timestamp are wall-clock time |
| Design record | [services/webhooks.md](../../services/webhooks.md), [security/allowlists.md](../../security/allowlists.md), [scenarios/payment-to-webhook.md](../../scenarios/payment-to-webhook.md) |

## Service surface

Errors are `{"error": "<message>", "status": <code>}`. Bodies are decoded
strictly: an unknown field is a `400`.

### `POST /enqueue`

Queues one webhook for the current subscription and starts delivering it
in the background.

Body:

```json
{"type": "payment.accepted", "paymentId": "pay_9f8e7d6c5b4a",
 "data": {"id": "pay_9f8e7d6c5b4a", "status": "accepted", "amount": "25.00"}}
```

`type` defaults to `payment.accepted`; `data` is any JSON and defaults to
`{}`.

Answers `202`:

```json
{"eventId": "evt_1a2b3c4d5e6f", "state": "pending"}
```

- `400`: malformed JSON or an unknown field.
- `403`: the allowlist's refusal of the subscription URL.
- `409`: `subscription is failed`, until `POST /subscriptions` replaces it
  or [`POST /sim/reset`](#post-simreset) makes it active again.

Side effects: delivers to the subscription URL, as it was at enqueue:

```json
{"createdAt": "2026-11-07T10:00:00Z", "data": {"…": "…"}, "id": "evt_1a2b3c4d5e6f",
 "paymentId": "pay_9f8e7d6c5b4a", "type": "payment.accepted"}
```

with `Content-Type: application/json`, `X-Sim-Signature: sha256=<hex
HMAC-SHA256 of "<timestamp>.<body>">`, `X-Sim-Timestamp: <unix seconds>`
and `X-Sim-Event-Id: evt_…`. A `2xx` ends it. Anything else (a `4xx`
included, a connection error, the allowlist refusing) is retried up to
`MAX_ATTEMPTS` with `RETRY_BACKOFF`. When the attempts run out the
delivery is `failed` and, if it still belongs to the current
subscription, the subscription is marked `failed`: its other deliveries
stop as `aborted` before their next attempt, and further enqueues are
`409`.

### `GET /subscriptions`

The subscription. There is always exactly one.

Answers `200`:

```json
{"subscriptions": [{"id": "sub_default", "url": "https://receiver:8443/webhooks", "status": "active"}]}
```

`status` is `active` or `failed`.

### `POST /subscriptions`

Replaces the subscription. The other way to clear a failed one is
[`POST /sim/reset`](#post-simreset).

Body:

```json
{"url": "https://receiver:8443/webhooks"}
```

Answers `201` with the new subscription, id `sub_<12 hex>`, status
`active`.

- `400`: malformed JSON or an unknown field.
- `403`: the allowlist's refusal, such as `allowlist: destination host
  "example.com" is not permitted`.

Side effects: deliveries already running keep going to the old URL.

### `GET /deliveries`

Every delivery, in enqueue order.

Answers `200`:

```json
{"deliveries": [{"id": "evt_1a2b3c4d5e6f", "subscriptionId": "sub_default", "type": "payment.accepted",
                 "paymentId": "pay_9f8e7d6c5b4a", "attempts": 1, "lastStatus": 200,
                 "state": "delivered", "createdAt": "2026-11-07T10:00:00Z", "updatedAt": "2026-11-07T10:00:00Z"}]}
```

`deliveries` is `null`, not `[]`, before the first. `state` is `pending`,
`delivered`, `failed` or `aborted`. `lastStatus` is `0` when no HTTP
answer came back; `lastError` is omitted when empty.

## Lab control

On the control port, `:9082` ([the control
contract](../README.md#the-control-contract)). The same `/sim/*` paths
still answer on `:8082` until phase 4 of [the plan](../../lab-api/plan.md)
retires them.

### `GET /health`

Liveness, on both ports. `:8082` answers `{"status": "ok", "service":
"notifier"}`; the control port answers `{"status": "ok", "service":
"notifier", "surface": "control"}`, and that is the one the lab API's
monitor probes.

### `GET /sim/activity`

The `deliveries` log.

Query: `limit` (default 100; the log keeps its last 256 events).

Answers `200`:

```json
{"logs": [{"name": "deliveries", "title": "Webhooks delivered", "note": "Each signed webhook payment-api enqueued, …",
           "total": 1, "kept": 1, "last": {"…": "…"},
           "events": [{"seq": 1, "at": "2026-11-07T10:00:00Z", "op": "delivery",
                       "peer": "https://receiver:8443/webhooks",
                       "summary": "payment.accepted for pay_9f8e7d6c5b4a delivered — HTTP 200 on attempt 1",
                       "status": "ok",
                       "detail": {"event": "evt_1a2b3c4d5e6f", "payment": "pay_9f8e7d6c5b4a", "endpoint": "https://receiver:8443/webhooks"}}]}]}
```

`peer` is the destination, not a caller.

### `GET /sim/info`

Who this is and what it can do, as every service answers it
([the control contract](../README.md#the-control-contract)):
`"service": "notifier"`, `"kind": "stand-in"`,
`"reset_scopes": ["flow", "all"]`.

### `GET /sim/logs`

The process's own log lines, as every service answers it
([the control contract](../README.md#the-control-contract)): one line per
request and per delivery attempt, and the subscription marked failed.

### `POST /sim/reset`

Forgets every delivery and stops those still running. The subscription is
what a platform set up once, so `flow` keeps it.

- **`flow` clears**: every delivery and the `deliveries` log. A delivery
  still running stops at its next step: asleep between attempts, it is
  woken and makes no further attempt; on the wire, its answer is not
  recorded. Either way it never marks the subscription failed afterwards.
  A subscription a run marked `failed` is `active` again, with the same id
  and URL: failed is what a delivery that ran out of retries made, and that
  delivery is gone.
- **`flow` keeps**: the subscription, its id and its URL, whether it is
  `WEBHOOK_URL`'s or one `POST /subscriptions` set.
- **`all`** is first start: as `flow`, and the subscription goes back to
  `sub_default` on `WEBHOOK_URL`.

`cleared` counts `deliveries` (of which `in_flight` were still running),
`activity` events, `failed_subscription` (1 when a failed subscription is
active again) and, with `all`, `subscription` (1 when one set with
`POST /subscriptions` gave way to `WEBHOOK_URL`'s).

```json
{"service": "notifier", "scope": "flow",
 "cleared": {"activity": 3, "deliveries": 2, "failed_subscription": 0, "in_flight": 1},
 "kept": ["subscription"], "at": "2026-11-07T12:00:00Z"}
```

## Called by

- **payment-api**: `POST /enqueue` after each payment the bank accepted.
- **lab API**: only the control port, `http://notifier:9082`.
  `GET /v1/services/notifier/activity` proxies `GET /sim/activity`;
  `GET /v1/lab` and `POST /v1/services/notifier/probe` probe its
  `GET /health`; `POST /v1/lab/reset` and
  `POST /v1/services/notifier/reset` call `POST /sim/reset`;
  `GET /v1/logs` and `GET /v1/services/notifier/logs` read
  `GET /sim/logs`.
- **e2e suite**: nothing directly. `payment-api-to-webhook` goes through it
  (payment-api → notifier → receiver) and checks the receiver.
- **by hand**: `make demo-payment`, through payment-api. Nothing in the repo
  calls `GET /subscriptions`, `POST /subscriptions` or `GET /deliveries`.
- **the platform**: never.

Every route above is held to the source by `internal/console/apidocs_test.go`: a route added, moved or removed without this page following fails `go test ./internal/console`.
