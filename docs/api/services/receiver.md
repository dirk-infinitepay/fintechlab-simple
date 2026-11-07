# Webhook receiver (platform stand-in) — API

A stand-in for the platform's own webhook listener: an HTTPS endpoint that
verifies the lab's HMAC scheme and stores what it accepts (the notifier's
demo webhooks), plus a raw, unvalidated capture sink for wire formats it
holds no key for, which is where Banking Circle's AES-256-GCM batches and
ACI's notifications land. [catalogue.md](../../catalogue.md) files it as
**scaffolding**, a platform stand-in. Point the vendors' webhook URLs at
your own listener instead. A test may read its captures, but nothing
should treat a `2xx` from it as proof a message was understood:
`/raw-events` accepts every body, and holds no keys to check one.

## At a glance

| | |
| --- | --- |
| Listens | `:8443`, HTTPS only, with a server certificate from the lab CA (`keys/certs/receiver.pem`; SANs `receiver`, `localhost`, `127.0.0.1`, `receiver.local`, `receiver.fintech-sim-lab.test`). Compose service `receiver` (`https://receiver:8443`); host `https://127.0.0.1:8443` (`curl --cacert keys/certs/ca.pem`).<br>`:9443`, **plain HTTP**, not TLS: the **control port** (`SIM_CONTROL_LISTEN`), every `/sim/*` route below and its own `GET /health`. Not published by compose; the lab API reaches it at `http://receiver:9443` |
| Auth | TLS server authentication only; no client certificate is asked for. `POST /webhooks` requires the lab HMAC; every other route is open. The control port has none: it takes calls from anything that can reach it, or only from `SIM_CONTROL_ALLOW` (addresses or CIDRs) when that is set |
| Found with | notifier `WEBHOOK_URL` (`https://receiver:8443/webhooks`).<br>banking-circle `WEBHOOK_URL` (`https://receiver:8443/raw-events`), its seeded subscription's endpoint.<br>aci `ACI_WEBHOOK_TARGET_URL` (`https://receiver:8443/raw-events`).<br>harness `RECEIVER_URL` (where it reads, `https://127.0.0.1:8443`) and `RECEIVER_DELIVERY_URL` (where it tells a vendor to post; defaults to `https://receiver:8443` when `RECEIVER_URL` is loopback).<br>`scripts/demo-payment.sh` `RECEIVER_URL`; console `CONSOLE_URL_RECEIVER`.<br>Senders allowlist it as `receiver` and `receiver:8443` |
| Activity logs | None. There is no `GET /sim/activity`, and `GET /v1/services/receiver/activity` on the lab API answers `501`. Its two stores are the record: `GET /events` and `GET /raw-events`. Each request line is logged on stdout |
| Configuration | `LISTEN` (`:8443`).<br>`SIM_CONTROL_LISTEN` (`LISTEN`'s port plus 1000, `:9443`) and `SIM_CONTROL_ALLOW` (empty: any caller): the control port.<br>`TLS_CERT` (`/keys/certs/receiver.pem`), `TLS_KEY` (`/keys/certs/receiver-key.pem`): waited for up to 30 s at start, then the service exits.<br>`HMAC_SECRET` (`sim-hmac-dev-only`): must equal the notifier's.<br>The 10-minute timestamp window and the 1 MiB body caps are fixed in code. Both stores are in memory, unbounded and never evicted; a restart or [`POST /sim/reset`](#post-simreset) empties them.<br>It has no lab switch of its own: it is disconnected at the senders (Banking Circle's subscriptions to it deactivated, aci's `/sim/connected`) |
| Design record | [ARCHITECTURE-phase3-corrections.md §6](../../ARCHITECTURE-phase3-corrections.md#6-receiver-a-raw-capture-endpoint-closes-a-documented-gap) (the raw sink; it predates retained bodies), [services/webhooks.md](../../services/webhooks.md), [security/ca-and-tls.md](../../security/ca-and-tls.md), [plugins.md](../../plugins.md#stand-ins), [console.md](../../console.md#stand-ins-disconnect-and-hide) |

## Service surface

The listener a vendor or the notifier delivers to. Errors are
`{"error": "<message>", "status": <code>}`. The receiver follows no lab
clock: `receivedAt` and the HMAC window are wall-clock time.

### `POST /webhooks`

The HMAC-verified endpoint, for the notifier's signed webhooks.

Headers:

- `X-Sim-Signature`: `sha256=` and the hex HMAC-SHA256, keyed with
  `HMAC_SECRET`, over `<X-Sim-Timestamp>.<body>`.
- `X-Sim-Timestamp`: Unix seconds, within 10 minutes of the receiver's
  clock.
- `X-Sim-Event-Id`: optional.

Body: any JSON, of which the first 1 MiB is read (anything longer is cut,
and then fails the HMAC). The notifier's:

```json
{"id": "evt_1a2b3c4d5e6f", "type": "payment.accepted", "paymentId": "pay_9f8e7d6c5b4a",
 "createdAt": "2026-11-07T10:00:00Z", "data": {"id": "pay_9f8e7d6c5b4a", "status": "accepted", "…": "…"}}
```

Answers `200`:

```json
{"status": "stored", "id": "evt_1a2b3c4d5e6f"}
```

- `401`: `hmac verification failed`, for a missing `sha256=` prefix, a
  signature mismatch, or a timestamp that is not a number or is out of the
  window.
- `400`: `read body`.

Side effects: stores an event whose id is the body's `id`, else
`X-Sim-Event-Id`, else `recv_<12 hex>`, with the body's `type` and
`paymentId` when present and the three signature headers. There is no
deduplication: a redelivered id is stored again. The body is kept as raw
JSON, so a signed body that is not JSON (or is empty) is accepted and then
breaks `GET /events`, which answers `200` with an empty body from then on.

### `POST /raw-events`

The unvalidated capture sink: any content type, any body, any query
string. No signature, HMAC or decryption of any kind.

Answers `200` with `{"status": "received"}`, always; the capture's id is
not returned.

- `400`: `read body`.

Side effects: stores a capture with:

- `id`: `raw_<12 hex>`; `receivedAt`: wall-clock RFC 3339.
- `path`: the request URI including its query, such as
  `/raw-events?sub=harness-batching`. That is how two subscriptions
  pointed here are told apart.
- `headers`: every request header under its canonical name, values as
  lists. Banking Circle's decryption material arrives as `Nonce`,
  `Authenticationtag` and `Checksum`.
- `bodySize` and `body`, the bytes base64-encoded. Above 1 MiB the body is
  dropped and `bodySize` stops at 1048577.

## Lab control

The `/sim/*` routes are on the control port, `:9443`, over plain HTTP
([the control contract](../README.md#the-control-contract)); the same
paths still answer over HTTPS on `:8443` until phase 4 of
[the plan](../../lab-api/plan.md) retires them. `GET /events` and
`GET /raw-events` are on `:8443` only, where the harness and
`make demo-payment` read them.

### `GET /health`

Liveness, on both ports. `:8443` answers over HTTPS with
`{"status": "ok", "service": "receiver"}`; the control port answers over
plain HTTP with `{"status": "ok", "service": "receiver", "surface":
"control"}`, and that is the one the lab API's monitor probes.

### `GET /events`

Every event `POST /webhooks` accepted, in arrival order.

Answers `200`:

```json
{"events": [{"id": "evt_1a2b3c4d5e6f", "receivedAt": "2026-11-07T10:00:01Z",
             "type": "payment.accepted", "paymentId": "pay_9f8e7d6c5b4a",
             "headers": {"X-Sim-Signature": "sha256=…", "X-Sim-Timestamp": "1794045601", "X-Sim-Event-Id": "evt_1a2b3c4d5e6f"},
             "body": {"id": "evt_1a2b3c4d5e6f", "type": "payment.accepted", "…": "…"}}]}
```

`events` is `null`, not `[]`, before the first. `type` and `paymentId` are
omitted when empty. `body` is the JSON as received. No filter: callers
look for their own `paymentId`.

### `GET /raw-events`

Every capture `POST /raw-events` took, in arrival order.

Answers `200`:

```json
{"raw_events": [{"id": "raw_8c2f1e09ab34", "receivedAt": "2026-11-07T10:00:02Z",
                 "path": "/raw-events?sub=harness-batching",
                 "headers": {"Content-Type": ["application/json"], "Nonce": ["…"],
                             "Authenticationtag": ["…"], "Checksum": ["…"]},
                 "bodySize": 1184, "body": "eyJ…"}]}
```

`raw_events` is `null` before the first. No filter: callers match on
`path`.

### `GET /sim/info`

Who this is and what it can do, as every service answers it
([the control contract](../README.md#the-control-contract)):
`"service": "receiver"`, `"kind": "stand-in"`,
`"reset_scopes": ["flow", "all"]`.

### `GET /sim/logs`

The process's own log lines, as every service answers it
([the control contract](../README.md#the-control-contract)): one line per
request, each stored event and capture, and each HMAC refusal with its
reason.

### `POST /sim/reset`

Empties both stores. Both scopes do the same: everything here is what a
run delivered.

- **Clears**: every event `POST /webhooks` stored and every capture
  `POST /raw-events` took. `GET /events` and `GET /raw-events` answer
  `null` again.
- **Keeps**: nothing.

```json
{"service": "receiver", "scope": "flow", "cleared": {"events": 2, "raw_events": 14},
 "kept": [], "at": "2026-11-07T12:00:00Z"}
```

## Called by

- **notifier**: `POST /webhooks`, once per delivery attempt.
- **banking-circle**: `POST /raw-events`, for its seeded subscription
  (`WEBHOOK_URL`) and for any subscription a client registers with an
  endpoint here.
- **aci**: `POST /raw-events` (`ACI_WEBHOOK_TARGET_URL`), while aci's own
  lab switch is on.
- **harness**: `payment-api-to-webhook` polls `GET /events` for a
  `payment.accepted` with its payment id; `banking-circle-subscription`
  and `banking-circle-retry-deactivation` register Banking Circle
  subscriptions at `{RECEIVER_DELIVERY_URL}/raw-events?sub=…`, read
  `GET /raw-events` filtered by `path`, and decrypt the bodies with the key
  they chose; `aci-payment-notification` needs this sink to answer `2xx`,
  since it asserts ACI's own delivery status.
- **lab API**: only the control port, `http://receiver:9443`.
  `GET /v1/lab` and `POST /v1/services/receiver/probe` probe its
  `GET /health`. `POST /v1/lab/reset` and
  `POST /v1/services/receiver/reset` call `POST /sim/reset`; `GET /v1/logs`
  and `GET /v1/services/receiver/logs` read `GET /sim/logs`. It never reads
  `GET /events` or `GET /raw-events`. `GET /v1/stand-ins` and
  `POST /v1/stand-ins/receiver` never call the receiver: they read and
  switch Banking Circle's subscriptions to it and aci's `/sim/connected`.
  `GET /v1/flow` counts its *notification batches* from Banking Circle's
  log, not from here.
- **by hand**: `make demo-payment` (`scripts/demo-payment.sh`) polls
  `GET /events`; [payment-to-webhook.md](../../scenarios/payment-to-webhook.md)
  and [settlement-to-payout.md](../../scenarios/settlement-to-payout.md)
  read `GET /events` and `GET /raw-events`.
- **the platform**: never. A platform brings its own listener and points
  the vendors at it.

Routes checked against the source at commit db5e695.
