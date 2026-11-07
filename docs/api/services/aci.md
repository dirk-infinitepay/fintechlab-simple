# ACI — API

ACI is the online card-payment gateway, on the vendor side of the line
([catalogue](../../catalogue.md)), and it is a sender: a platform does not
call it, it calls the platform. `POST /sim/payments` on its control port
stands for "a card payment just happened": ACI builds its notification payload,
encrypts it with AES-256-GCM exactly as ACI does, and delivers it to
`ACI_WEBHOOK_TARGET_URL` with retries. Real-protocol-accurate: the cipher,
the hex encodings, the header names, both body modes and the payload's
field casing, known-answer-tested against ACI's own published reference
vector (`internal/aci/crypto_test.go`). Simplified: only `PAYMENT`
notifications carrying the fields the trigger supplies; no
merchant-onboarding REST client and no SFTP+PGP file channel (both real ACI
surfaces, out of scope).

## At a glance

| | |
| --- | --- |
| Listens | HTTP `:8087` (`LISTEN`). Compose service `aci`; from the host `http://127.0.0.1:8087`.<br>`:9087`, plain HTTP: the **control port** (`SIM_CONTROL_LISTEN`), every `/sim/*` route below and its own `GET /health`. Not published by compose; the lab API reaches it at `http://aci:9087`.<br>Outbound: one POST per notification to `ACI_WEBHOOK_TARGET_URL` |
| Auth | None on either port. The control port takes calls from anything that can reach it, or only from `SIM_CONTROL_ALLOW` (addresses or CIDRs) when that is set. The outbound webhook's only authentication is its encryption: a listener that decrypts it with the shared key knows ACI sent it, and a tag mismatch is the auth failure |
| A platform points here with | Nothing: the platform is the target. Point `ACI_WEBHOOK_TARGET_URL` at the platform's listener (and allow its host in `WEBHOOK_ALLOWLIST`); the platform decrypts with `ACI_WEBHOOK_SECRET`, which the lab API's connect kit hands out (`GET :8097/v1/connect/env/aci`). Lab peers find ACI with `ACI_URL` (harness) and `CONSOLE_URL_ACI` (lab API) |
| Activity logs | None. ACI serves no `GET /sim/activity`, and its catalogue entry names no activity path, so the console draws no activity panel for it. `GET /sim/sent` is its record of what it built and how each delivery went |
| Configuration | The important variables are in the table below. Everything is in memory: a restart forgets every record |
| Design record | [ARCHITECTURE-phase3-corrections.md](../../ARCHITECTURE-phase3-corrections.md) §2 (the webhook contract and its sources), §6 (the receiver's raw capture sink it delivers to); [plugins.md](../../plugins.md#stand-ins) and [console.md](../../console.md#stand-ins-disconnect-and-hide) for the delivery switch |

| Variable | Default | Effect |
| --- | --- | --- |
| `ACI_WEBHOOK_SECRET` | `aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666aaaa7777bbbb8888` | AES-256 key as 64 hex characters. Not hex, or not 32 bytes decoded, is fatal at start |
| `ACI_WEBHOOK_TARGET_URL` | `http://localhost:3013/api/v1/webhook` (compose `https://receiver:8443/raw-events`) | Where every notification is delivered |
| `ACI_WEBHOOK_BODY_MODE` | `raw` | `raw`: the hex ciphertext as `text/plain`. `json`: `{"encryptedBody": "<hex>"}` as `application/json`. Anything else is fatal at start |
| `WEBHOOK_ALLOWLIST` | `localhost,127.0.0.1` (compose adds `receiver`, `receiver:8443`) | Hosts the target may name. A target outside it fails every attempt |
| `RETRY_BACKOFF`, `MAX_ATTEMPTS` | `1s,2s,4s`, `4` | Wait before attempts 2, 3, 4 (the last value repeats beyond that), and how many attempts in all |
| `CA_FILE` | `keys/certs/ca.pem` (compose `/keys/certs/ca.pem`) | Extra trust root for an HTTPS target. Unreadable is logged, not fatal |
| `ACI_WEBHOOK_CONNECTED` | `true` | Starting state of the delivery switch: `false`, `0`, `off` or `no` start it disconnected |
| `SIM_CONTROL_LISTEN`, `SIM_CONTROL_ALLOW` | `LISTEN`'s port plus 1000 (`:9087`), empty (any caller) | The control port, and who may call it |
| `LAB_CLOCK_URL` | unset (compose `http://clock:8096/clock`) | The payload's `timestamp` and the record's `created_at` follow the lab clock |

## Vendor surface

What a real platform calls: nothing. ACI calls the platform, and the
webhook below is the vendor-shaped surface, the same as ACI's.

**The webhook it sends.** `POST {ACI_WEBHOOK_TARGET_URL}`, an 8 s timeout
per attempt.

- Headers: `X-Initialization-Vector` (12 bytes, hex), `X-Authentication-Tag`
  (16 bytes, hex), `Content-Type` (`text/plain` or `application/json` by
  body mode).
- Body: the AES-256-GCM ciphertext, hex, with the tag split off: raw, or
  `{"encryptedBody": "<hex>"}`. A listener tells them apart by a leading
  `{`. The key is `ACI_WEBHOOK_SECRET` hex-decoded; the IV is random per
  notification.
- Plaintext:

  ```json
  {"type": "PAYMENT",
   "payload": {"id": "5f0c9e2d7a1b4c3e8d6f0a9b2c1d3e4f", "merchantTransactionId": "harness-tx-1727254800000000000",
               "paymentType": "RX", "paymentBrand": "VISA", "presentationAmount": "42.50",
               "presentationCurrency": "EUR", "pluginType": "SHOPFY", "source": "OPP",
               "shortId": "4a1b2c3d4e5f", "timestamp": "2026-09-25 09:00:00",
               "result": {"code": "000.300.100", "description": "Risk check successful"}}}
  ```

  `id` is 32 random hex characters, `shortId` 12; `timestamp` is the lab
  clock in UTC as `YYYY-MM-DD HH:MM:SS`. A field the trigger left empty is
  omitted, and `result` appears only when a code or description was given.
  `channelName`, `paymentMethod`, `ndc`, `resultDetails` and `card` are
  part of the payload type but never set.
- Delivery: any 2xx is delivered. Anything else, including a `4xx`, a
  refused connection or an allowlist refusal, is retried on `RETRY_BACKOFF`
  until `MAX_ATTEMPTS`, then the record is `failed`.

## Lab control

Not part of ACI. These are how a test, an agent or the lab API makes a card
payment happen and sees what was sent. They are on the control port,
`:9087` ([the control contract](../README.md#the-control-contract)). The
same `/sim/*` paths still answer on `:8087`, and so do
`POST /internal/simulate-payment` and `GET /internal/sent[/{id}]`, until
phase 4 of [the plan](../../lab-api/plan.md) retires them.

### `GET /health`

Liveness, on both ports. `:8087` answers `{"status": "ok", "service":
"aci"}`; the control port answers `{"status": "ok", "service": "aci",
"surface": "control"}`, and that is the one the lab API's monitor probes.

### `POST /sim/payments`

"A card payment just happened": builds, encrypts and sends one
notification.

- Body (this lab's own snake_case trigger, not ACI's wire shape):

  ```json
  {"merchant_transaction_id": "harness-tx-1727254800000000000", "payment_type": "RX", "payment_brand": "VISA",
   "presentation_amount": "42.50", "presentation_currency": "EUR", "plugin_type": "SHOPFY",
   "source": "OPP", "result_code": "000.300.100", "result_description": "Risk check successful"}
  ```

  Only `merchant_transaction_id` is required; the rest are passed through
  as strings, unvalidated. Decoded strictly: an unknown field is a `400`.
- Answers `202` `{"id": "aci_evt_7c2e9a1f0b3d"}` as soon as the
  notification is encrypted, before any delivery. `400`
  `{"error": "merchant_transaction_id required", "status": 400}` or for a
  body that does not decode; `500` if marshalling or encryption fails.
- Side effects: a record in `GET /sim/sent`, `delivery_status`
  `pending`, and delivery in the background. With the delivery switch off
  (`POST /sim/connected`) nothing is sent: the record is stored `held`,
  with `delivery_error` saying why, and it is never sent later.

### `POST /internal/simulate-payment`

`POST /sim/payments` on `:8087`, until phase 4: the same handler, so the
same body, answers and side effects. The harness calls this one.

### `GET /sim/sent`

Every notification built since start, oldest first.

- Answers `200` `{"result": [{…record…}]}`, each as
  `GET /sim/sent/{id}` returns it.

### `GET /internal/sent`

`GET /sim/sent` on `:8087`, until phase 4. The same answer.

### `GET /sim/sent/{id}`

One notification: the plaintext, the wire material and how delivery went.

- Answers `200`:

  ```json
  {"id": "aci_evt_7c2e9a1f0b3d",
   "notification": {"type": "PAYMENT", "payload": {"merchantTransactionId": "harness-tx-1727254800000000000", "…": "…"}},
   "ciphertext_hex": "9b1e…", "iv_hex": "3f9a0c1b7e2d4a6f8c5b1e09", "tag_hex": "d41c7a2e9b0f3c6d8e1a5b7c9f0e2d4a",
   "target_url": "https://receiver:8443/raw-events", "body_mode": "raw",
   "created_at": "2026-09-25T09:00:00Z", "delivery_status": "delivered",
   "http_status": 200, "delivered_at": "2026-09-25T09:00:00.412Z"}
  ```

  `delivery_status` is `pending`, `delivered`, `failed` or `held`.
  `http_status` (the last HTTP answer; absent when none came),
  `delivery_error` and `delivered_at` appear when they apply.
  `delivered_at` is the wall clock, `created_at` the lab clock. `404`
  `{"error": "notification not found", "status": 404}`.

### `GET /internal/sent/{id}`

`GET /sim/sent/{id}` on `:8087`, until phase 4. The same answers. The
harness polls this one.

### `GET /sim/connected`

The delivery switch: whether simulated payments are delivered to
`ACI_WEBHOOK_TARGET_URL`.

- Answers `200` `{"connected": true, "what": "delivers webhooks to ACI_WEBHOOK_TARGET_URL", "target": "https://receiver:8443/raw-events"}`,
  plus `reason` and `changed_at` once it has been set.

### `POST /sim/connected`

Turns delivery off or on. It stops only the automatic delivery: a request
to `POST /sim/payments` still answers `202` and is recorded `held`.

- Body: `{"connected": false, "reason": "a platform listens itself"}`;
  `reason` is optional (default `"set over /sim/connected"`). At most 4 KiB.
- Answers `200` with the state as `GET /sim/connected` returns it. `400`
  `{"error": "want {\"connected\": true|false}", "status": 400}` when
  `connected` is missing or the body is not JSON.
- Side effects: turning it back on does not send the records held while
  it was off.

### `GET /sim/info`

Who this is and what it can do, as every service answers it
([the control contract](../README.md#the-control-contract)):
`"service": "aci"`, `"kind": "vendor"`, `"vendor_listeners": [":8087"]`,
`"control_listener": ":9087"`, `"reset_scopes": ["flow", "all"]`.

### `GET /sim/logs`

The process's own log lines, as every service answers it
([the control contract](../README.md#the-control-contract)): every request
on either port but these reads and the health probes, every delivery
attempt, and every delivery a reset dropped.

### `POST /sim/reset`

Empties the sent log. Both scopes do the same: ACI holds nothing a platform
set up once.

- **Clears**: every record in `GET /sim/sent` (`sent`), and every
  delivery in flight, first attempts and retries alike
  (`deliveries_in_flight`). A delivery waiting out its backoff makes no
  further attempt; the reset waits for one already on the wire, so when it
  answers nothing it dropped is still being sent.
- **Keeps**: the delivery switch (`GET /sim/connected`), on or off as it
  was. It is live wiring, not something a run made.

```json
{"service": "aci", "scope": "flow", "cleared": {"sent": 4, "deliveries_in_flight": 1},
 "kept": ["connected_switch"], "at": "2026-11-07T12:00:00Z"}
```

## Called by

- **The harness**: `aci-payment-notification`
  (`internal/harness/scenario_aci.go`) posts
  `POST /internal/simulate-payment` on `:8087`, polls
  `GET /internal/sent/{id}` until the delivery settles, and requires
  `delivered` with non-empty ciphertext, IV and tag and the payload fields
  it sent.
- **The lab API** (`cmd/labapi`), on the control port only
  (`http://aci:9087`): `GET /health` (health monitor, behind
  `GET /v1/lab` and `POST /v1/services/{id}/probe`). `POST /sim/reset`
  (`POST /v1/lab/reset`, `POST /v1/services/aci/reset`) and
  `GET /sim/logs` (`GET /v1/logs`, `GET /v1/services/aci/logs`).
  `GET /sim/connected` (`GET /v1/stand-ins`) and `POST /sim/connected`
  (`POST /v1/stand-ins/{id}` for the `receiver` stand-in), which count
  only while the target is the receiver: ACI pointed at a real platform is
  that platform's wiring, not the stand-in's.
- **Nobody in the repo** calls `POST /sim/payments` or `GET /sim/sent`;
  they are for operators and agents.
- **The platform under test** never calls ACI. It is the target: ACI
  posts each notification to its listener at `ACI_WEBHOOK_TARGET_URL`
  (compose: the receiver's `POST /raw-events`, a raw capture that stores
  the body without decrypting it).

Every route above is held to the source by `internal/console/apidocs_test.go`: a route added, moved or removed without this page following fails `go test ./internal/console`.
