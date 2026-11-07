# Payment API (demo stand-in) — API

A generic payment facade: one `POST /payments` with an `Idempotency-Key`
moves money between two accounts at the business bank and enqueues a
signed webhook at the notifier, because a created payment must produce a
webhook rather than a disconnected ping. [catalogue.md](../../catalogue.md)
files it as **scaffolding**, a demo stand-in: a generic shape, not
B4B-shaped and not B4B (the real payout rail is `cmd/b4b`). Only
`make demo-payment` and the harness's `payment-api-to-webhook` call it.
Nothing on the settle path does, and nothing should depend on it.

## At a glance

| | |
| --- | --- |
| Listens | `:8080`, plain HTTP. Compose service `payment-api` (`http://payment-api:8080`); host `http://127.0.0.1:8080` |
| Auth | None |
| Found with | `PAYMENT_API_URL` (`http://127.0.0.1:8080`) in the harness and `scripts/demo-payment.sh`.<br>`CONSOLE_URL_PAYMENT_API` on the console |
| Activity logs | `payments` (*Payments received*): every `POST /payments`, op `payment.create`. The summary gives amount, debtor and creditor, then the outcome: accepted with the webhook's state, replayed for a seen key, or the refusal's own error. Detail: `payment`, `idempotency_key` |
| Configuration | `BANK_URL` (`http://bank:8081`): where the ledger move goes.<br>`NOTIFIER_URL` (`http://notifier:8082`): where the webhook is enqueued.<br>`LISTEN` (`:8080`).<br>Payments are in memory. It does not follow the lab clock: `createdAt` is wall-clock time |
| Design record | [scenarios/payment-to-webhook.md](../../scenarios/payment-to-webhook.md), [services/webhooks.md](../../services/webhooks.md), [services/fake-bank.md](../../services/fake-bank.md) |

## Service surface

Errors are `{"error": "<message>", "status": <code>}`, except the `409`
below. Bodies are decoded strictly: an unknown field is a `400`.

### `POST /payments`

Creates a payment: the ledger move, then the webhook.

Header: `Idempotency-Key`, optional, trimmed.

Body:

```json
{"debtorAccountId": "acc_alice", "creditorIban": "GB00SIM0000000000003",
 "amount": "25.00", "currency": "EUR", "reference": "invoice-42"}
```

`debtorAccountId` (a bank account id), `creditorIban` and `amount` are
required; `currency` defaults to `EUR`.

Answers `201`:

```json
{"id": "pay_9f8e7d6c5b4a", "status": "accepted", "debtorAccountId": "acc_alice",
 "creditorIban": "GB00SIM0000000000003", "amount": "25.00", "currency": "EUR",
 "reference": "invoice-42", "idempotencyKey": "tutorial-1", "notification": "enqueued",
 "createdAt": "2026-11-07T10:00:00Z"}
```

`reference` and `idempotencyKey` are omitted when empty.

- `200` with the stored payment when the `Idempotency-Key` was seen
  before. The body is not compared, and a rejected payment replays as the
  same rejected record.
- `201` with `notification` `enqueue_failed: <error>` when the bank moved
  the money but the notifier refused or could not be reached: the money
  moved, and no webhook will follow.
- `409` when the bank refused the transfer or could not be reached. The
  body is not the error shape; `error` carries the bank's raw answer:

  ```json
  {"error": "ledger rejected transfer: {\"error\":\"insufficient funds\",\"status\":409}\n",
   "payment": {"id": "pay_9f8e7d6c5b4a", "status": "rejected", "notification": "not_enqueued", "…": "…"}}
  ```

- `400`: `debtorAccountId, creditorIban, amount required`, malformed JSON
  or an unknown field. Checked before the key is looked up; nothing is
  stored.

Side effects:

1. `POST {BANK_URL}/transfers` with `{"fromAccountId", "toIban", "amount",
   "currency", "reference", "paymentId"}`. A refusal stores the payment as
   `rejected`, under its key.
2. `POST {NOTIFIER_URL}/enqueue` with `{"type": "payment.accepted",
   "paymentId": "pay_…", "data": <the payment>}`; `data` is the payment as
   it stood before this call, so its `notification` reads `pending`.

The key check and the store are not one critical section: two concurrent
first requests with the same key both move money.

### `GET /payments/{id}`

One payment.

Path: `id`, `pay_…`.

Answers `200` with the payment object above.

- `404`: `payment not found`.

### `GET /payments`

Every payment, in no particular order.

Answers `200` with `{"payments": [...]}`, `[]` when there are none.

## Lab control

### `GET /health`

Liveness. The console's monitor probes it.

Answers `200` with `{"status": "ok", "service": "payment-api"}`.

### `GET /sim/activity`

The `payments` log.

Query: `limit` (default 100; the log keeps its last 256 events).

Answers `200`:

```json
{"logs": [{"name": "payments", "title": "Payments received", "note": "Every POST /payments: …",
           "total": 2, "kept": 2, "last": {"…": "…"},
           "events": [{"seq": 2, "at": "2026-11-07T10:00:00Z", "op": "payment.create", "peer": "10.89.0.1",
                       "summary": "payment 25.00 EUR acc_alice → GB00SIM0000000000003 — replayed: this Idempotency-Key was seen before",
                       "status": "ok", "detail": {"payment": "pay_9f8e7d6c5b4a", "idempotency_key": "tutorial-1", "status": "OK"}}]}]}
```

## Called by

- **by hand**: `make demo-payment` (`scripts/demo-payment.sh`) posts
  `POST /payments` with a fresh key, reads `GET /payments/{id}`, then polls
  the receiver for the webhook. [payment-to-webhook.md](../../scenarios/payment-to-webhook.md)
  is the same with `curl`.
- **harness**: `payment-api-to-webhook` posts `POST /payments` twice with
  one key, expecting `201` then `200` with the same id, and checks one
  debit, one credit and the webhook at the receiver.
- **console**: `GET /api/services/payment-api/activity` proxies
  `GET /sim/activity`; `GET /api/overview` and
  `POST /api/services/payment-api/probe` probe `GET /health`.
- **the platform**: never. No vendor calls it either.

Routes checked against the source at commit db5e695.
