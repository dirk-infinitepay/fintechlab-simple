# Banking Circle — API

Banking Circle is the settlement bank, on the vendor side of the line
([catalogue](../../catalogue.md)): it holds the platform's safeguarding
accounts (one per currency, starting empty), books the payouts B4B hands
it, credits the beneficiary's bank when one processes, and tells the
platform through batched, encrypted webhooks. Real-protocol-accurate: the
Connect API's Basic→Bearer exchange behind optional mTLS, the balance read,
the notification self-service API (`rowVersion` and `If-Match`,
per-subscription keys, batch sizes, event targets), the exact AES-256-GCM
webhook cipher and payload shapes, the eleven-step retry schedule ending in
deactivation, and the reconciliation reads (intraday report, rejection
report, single-payment status) including their `400` body. Bookings are
dated on the bank's business day (Europe/Paris, 19:00 cutoff, weekends to
Monday) on the lab clock. Simplified: any Basic credential is accepted, no
payment-creation endpoint (real Banking Circle has none; B4B bridges in),
no FX, bulk payments, direct debits or bank holidays, and everything is in
memory.

## At a glance

| | |
| --- | --- |
| Listens | `:8085` HTTPS (`LISTEN`), the Connect-shaped API; `:8095` plain HTTP (`INTERNAL_LISTEN`), the lab bridge. Compose service `banking-circle`; from the host `https://127.0.0.1:8085` and `http://127.0.0.1:8095` |
| Auth | `:8085`: TLS 1.2+ with a server certificate from the lab CA. A client certificate per `BC_MTLS` (`require`, the default and compose's; `optional`; `off`), verified against `CA_FILE`; without one under `require` the handshake fails and there is no HTTP answer. Then `GET /api/v1/authorizations/authorize` with any non-empty Basic credentials returns a bearer token, and every other route on `:8085` except `GET /health` needs `Authorization: Bearer <token>` (`401` `{"error": "bearer token required", "status": 401}` otherwise). `:8095`: none |
| A platform points here with | The console's connect kit (`GET :8090/api/connect/env/banking-circle`): `BC_AUTH_BASE_URL` and `BC_API_BASE_URL` (both `https://…:8085`), `BC_API_M2M_USERNAME`, `BC_API_M2M_PASSWORD`, `BC_API_MTLS_ENABLED`, `BC_API_CLIENT_CERT_PEM_BASE64`, `BC_API_CLIENT_KEY_PEM_BASE64`, `BC_SAFEGUARDING_ACCOUNT_ID_EUR`, `BC_SAFEGUARDING_ACCOUNT_ID_GBP`, `NODE_EXTRA_CA_CERTS`. Lab peers: `BANKING_CIRCLE_URL` on b4b (the bridge, `:8095`), `BC_INTERNAL_URL` on worldline and settlement (the bridge), `CONSOLE_URL_BANKING_CIRCLE` and `CONSOLE_BC_INTERNAL_URL` on the console, `BANKING_CIRCLE_URL` (`:8085`) and `BANKING_CIRCLE_INTERNAL_URL` (`:8095`) on the harness. The same name `BANKING_CIRCLE_URL` means the bridge on b4b and the Connect API in the harness |
| Activity logs | `GET /sim/activity` (on `:8085`, Bearer) keeps three logs. `payments` (*Payments received*): `payment.create` for each `POST /internal/payments`, `payment.incoming` for each `POST /internal/incoming-payments`, written from the caller's request so a refused one still shows its amount. `notifications` (*Notifications sent*): `notification` for each encrypted batch POST (every retry is its own line; red on a non-2xx or a refused connection), `notification.queued` for each batch parked behind a pause, `notification.pause` for each pause and resume. `reports` (*Reconciliation reads*): `report.intraday`, `report.rejection` and `payment.status`, amber when refused or not found |
| Configuration | The important variables are in the table below. Seeded at start: the safeguarding accounts `00000000-0000-4000-8000-000000000978` (EUR, vIBAN `BE00SIMSGA00000001`) and `00000000-0000-4000-8000-000000000826` (GBP, `GB00SIMSGA00000001`), both `0.00`; and one active subscription to `WEBHOOK_URL`, subscribed subscription-wide (target type `1`, no targets) to `OutgoingPaymentBooked`, `OutgoingPaymentProcessed`, `OutgoingPaymentRejected`, `MissingFunding`, `Reversed`, `IncomingPaymentProcessed`, `IncomingPaymentBooked` and `PaymentStatus`. A restart forgets tokens, subscriptions, payments and balances |
| Design record | [ARCHITECTURE-banking-circle-webhooks.md](../../ARCHITECTURE-banking-circle-webhooks.md) (subscriptions, batching, retries, the lab-only endpoints), [ARCHITECTURE-vendor-corrections.md](../../ARCHITECTURE-vendor-corrections.md) §3 and Addendum B, C, G and H (auth, webhook cipher, the bridge, two listeners, UUID account ids, the reconciliation reads), [ARCHITECTURE-phase3-corrections.md](../../ARCHITECTURE-phase3-corrections.md) §1 (the safeguarding-account model), [structure/banking-circle.md](../../structure/banking-circle.md). [ARCHITECTURE-banking-circle.md](../../ARCHITECTURE-banking-circle.md) is superseded |

| Variable | Default | Effect |
| --- | --- | --- |
| `BC_MTLS` | `require` | `off` (also `none`, `false`), `optional` (`verify-if-given`), `require` (`required`, `true`). Anything else is fatal at start. Bearer auth applies in every mode |
| `CA_FILE`, `TLS_CERT`, `TLS_KEY` | `/keys/certs/ca.pem`, `/keys/certs/banking-circle.pem`, `/keys/certs/banking-circle-key.pem` | Waited for up to 30 s at start. The CA is also trusted for outbound HTTPS |
| `TOKEN_TTL` | `1h` | Bearer token lifetime, on the wall clock |
| `PROCESSING_DELAY` | `2s` | Wait between a payment booking and processing, outgoing and incoming |
| `LAB_CLOCK_URL` | unset (compose `http://clock:8096/clock`) | Follow the lab clock for booking times, notification timestamps, balance dates and report dates |
| `WEBHOOK_URL` | `https://receiver:8443/webhooks` (compose `https://receiver:8443/raw-events`) | Endpoint of the seeded subscription. Must pass the allowlist or start-up fails |
| `WEBHOOK_ALLOWLIST` | `receiver,receiver:8443,localhost,127.0.0.1` (compose adds `bank`, `bank:8081`, `host.containers.internal`) | Hosts a subscription endpoint and `BANK_CREDIT_URL` may name |
| `BC_NOTIFICATION_KEY` | `sim-bc-notification-key-32-chars` | Exactly 32 characters or start-up fails. The seeded subscription's key, and the key of a subscription created without one |
| `BC_SEED_BATCH_SIZE`, `BC_SEED_SUBSCRIPTION_EMAIL` | `5`, `alerts@fintechlab-simple.local` | The seeded subscription's `maxNotificationsPerMessage` and `email` |
| `BC_DELIVERY_CONFIG` | `/config/banking-circle.json` | Retry schedule, email steps, `batch_flush_interval` (2 s), default batch size. A missing file means the documented schedule at real time |
| `BC_TIME_SCALE` | unset (compose `86400`) | Overrides the file's `time_scale` (3600): every retry delay is divided by it. At 86400 the whole schedule takes about four seconds |
| `BANK_CREDIT_URL` | empty (compose `http://bank:8081/internal/credit`) | Where a processed payout is credited. Empty turns that hop off |

## Vendor surface

What a real platform calls, on `:8085`: Banking Circle's Connect API, the
same paths and shapes. Bodies are decoded strictly: an unknown field is a
`400`.

**Webhooks it sends.** For each change of a payment's state, to every
active subscription with an active event of that `eventType` whose targets
are empty or include the payment's `toAccountId` or `fromAccountId` (a
target's `targetType` is stored but plays no part in matching). Queued per
subscription; a message goes out when the queue reaches the subscription's
`maxNotificationsPerMessage` or after `batch_flush_interval`.

- Request: `POST {endpoint}`, `Content-Type: application/octet-stream`.
  The body is the raw AES-256-GCM ciphertext of the JSON envelope encoded
  as UTF-16LE, keyed with the subscription's 32-character `encryptionKey`
  taken as UTF-8 bytes (not base64-decoded). Headers, all base64: `Nonce`
  (12 bytes), `AuthenticationTag` (16 bytes), `Checksum` (SHA-256 of the
  envelope's UTF-8 JSON). Plus `SubscriptionVersion`, the subscription's
  `version`.
- Any 2xx is delivered. Anything else, a refused connection or an 8 s
  timeout runs the retry schedule: 11 retries (15 s, 30 s, 1 m, 10 m, 30 m,
  1 h, 2 h, 6 h, 12 h, 24 h, 48 h, divided by the time scale), a warning
  email at retries 7 and 10, a deactivation email at 11 (`GET /sim/emails`).
  Then the subscription is deactivated with `statusMessage`
  `"Subscription deactivated after the final unsuccessful delivery retry"`
  and the batch is retained until `PUT …/activate`. Events while a
  subscription is inactive are not queued at all.
- Decrypted envelope, for a processed payout:

  ```json
  {"notifications": [{"eventId": "evt_5d0c2a91be47", "subscriptionId": "sub_8f3e1a0c2b6d",
    "subscriptionEventId": "subev_1a2b3c4d5e6f", "notificationType": "OutgoingPaymentProcessed",
    "timestamp": "2026-09-25T09:00:02Z",
    "payment": {"paymentId": "bcp_3e9a4c1d7b20", "transactionReference": "010F100000000001",
      "status": "Processed", "return": null,
      "debtorInformation": {"accountId": "00000000-0000-4000-8000-000000000978",
        "debitAmount": {"currency": "EUR", "amount": 150.00},
        "instruction": {"amount": {"currency": "EUR", "amount": 150.00}}},
      "creditorInformation": null,
      "transfer": {"amount": {"currency": "EUR", "amount": 150.00}}}}]}
  ```

  `OutgoingPaymentBooked` and `IncomingPaymentBooked` carry the short shape
  `{"paymentId", "transactionReference", "valueDate", "transactionDate", "amount", "currency", "transfer"}`,
  the amount signed by its effect on the balance (a payout's booking
  negative; money in, and a reversal's booking, positive). `IncomingPaymentProcessed`
  sets `creditorInformation: {"accountId", "creditAmount"}` instead and
  `return: true` on a returned payout. Remittance lines travel in
  `transfer.remittanceInformation` (`line1`–`line4`, null when unused).
  `PaymentStatus` nests its detail under `payload`, not `payment`.
  `transactionReference` is the bank's own `010F10…` reference.

### `GET /api/v1/authorizations/authorize`

Exchanges Basic credentials for a bearer token.

- Headers: `Authorization: Basic …`. Any non-empty username and password
  is accepted; nothing is checked against a store.
- Answers `200` `{"access_token": "bcat_6c1f…(48 hex)", "expires_in": 3600, "token_type": "bearer"}`.
  `401` `{"error": "basic auth required", "status": 401}` with
  `WWW-Authenticate: Basic realm="banking-circle"` when the header is
  missing or either part is empty.
- Side effects: the token lives in memory for `TOKEN_TTL`; a restart
  invalidates every token.

### `GET /api/v1/accounts/{accountId}/balances`

An account's balance, as the platform's safeguarding check reads it.

- Path: `accountId`, a UUID. Query: `pageNumber`, `pageSize` (echoed in
  `pageInfo`; there is only ever one entry).
- Answers `200`:

  ```json
  {"result": [{"type": "CurrentBalance", "currency": "EUR", "beginOfDayAmount": "0.00",
               "financialDate": "2026-09-25", "intraDayAmount": "1250.00",
               "lastTransactionTimestamp": "2026-09-25T09:12:03Z"}],
   "pageInfo": {"currentPage": 1, "pageSize": 20, "rowCount": 1}}
  ```

  The balance is `beginOfDayAmount` plus `intraDayAmount` (the day's
  movement). Amounts are decimal strings. `financialDate` is the lab
  clock's UTC calendar date, not the bank's business day, and
  `lastTransactionTimestamp` is the time of the read. `400`
  `{"error": "accountId must be a UUID", "status": 400}` (a vIBAN is
  refused here); `404` `{"error": "account not found", "status": 404}`.

### `GET /api/v1/notificationselfservice/subscription`

Lists subscriptions, ordered by id.

- Query: `PageNumber` (default 1), `PageSize` (default 50).
- Answers `200` `{"result": [{…subscription…}], "pageInfo": {"pageNumber": 1, "pageSize": 50, "totalItems": 1}, "totalItems": 1}`,
  each subscription as `GET …/subscription/{id}` returns it.

### `POST /api/v1/notificationselfservice/subscription`

Registers a webhook endpoint.

- Body:

  ```json
  {"endpoint": "https://receiver:8443/raw-events?sub=mine", "encryptionKey": "0123456789abcdef0123456789abcdef",
   "status": 2, "email": "ops@example.test", "maxNotificationsPerMessage": 5, "mtlsEnabled": false}
  ```

  `endpoint` is required and must pass `WEBHOOK_ALLOWLIST`. `encryptionKey`
  must be exactly 32 characters; omitted, it defaults to
  `BC_NOTIFICATION_KEY`. `status` is `2` (active, the default) or `1`
  (inactive). `maxNotificationsPerMessage` is 5–1000, default 1000. The old
  lab shape `{"url": "…"}` is still accepted as `endpoint`.
- Answers `200`:

  ```json
  {"id": "sub_8f3e1a0c2b6d", "endpoint": "https://receiver:8443/raw-events?sub=mine", "isActive": true,
   "mtlsEnabled": false, "status": 2, "statusMessage": "Subscription successfully created",
   "rowVersion": "AAAAAAAAACs=", "version": 1, "email": "ops@example.test",
   "maxNotificationsPerMessage": 5, "subscriptionEvents": [], "encryptionKey": "*Hidden*"}
  ```

  `400` `{"error": "…", "status": 400}` for an allowlist rejection, a key
  of the wrong length, another `status`, a batch size out of range or an
  unknown field. `409` when another subscription already uses the endpoint.
- Side effects: none until an event is added; a subscription with no
  events receives nothing but `clienttest`.

### `GET /api/v1/notificationselfservice/subscription/{id}`

One subscription. The key is always `"*Hidden*"`.

- Answers `200` with the subscription; `404` `{"error": "subscription not found", "status": 404}`.
- `subscriptionEvents` lists its events, each as
  `POST …/subscriptionEvent` returns it. `version` is always `1`.

### `PUT /api/v1/notificationselfservice/subscription/{id}`

Changes a subscription under optimistic concurrency.

- Headers: `If-Match: <rowVersion>` (quotes allowed). Missing or stale is
  `412`.
- Body: any of `endpoint`, `encryptionKey`, `email`,
  `maxNotificationsPerMessage`, `mtlsEnabled`; an omitted field is left
  alone. `status` is not accepted here (unknown field, `400`); use
  `/activate` and `/deactivate`.
- Answers `200` with the subscription and a new `rowVersion`. `400` for an
  allowlist rejection or a value out of range; `404`; `409` for an endpoint
  another subscription uses; `412`
  `{"error": "If-Match does not match the current rowVersion; re-read the subscription and retry", "status": 412}`.

### `PUT /api/v1/notificationselfservice/subscription/{id}/activate`

Reactivates a subscription and redelivers what it missed.

- Headers: `If-Match: <rowVersion>`.
- Answers `200` `{"subscription": {…, "isActive": true, "status": 2, "statusMessage": "Subscription is active"}, "redeliveredNotifications": 2}`.
  `404`; `412`.
- Side effects: the subscription and its events become active, with new
  `rowVersion`s. Notifications retained when the retry schedule gave up are
  queued again. Events that happened while it was inactive are not
  backfilled.

### `PUT /api/v1/notificationselfservice/subscription/{id}/deactivate`

Switches a subscription off.

- Headers: `If-Match: <rowVersion>`.
- Answers `200` with the subscription (`"isActive": false`, `"status": 1`,
  `"statusMessage": "Subscription is inactive"`). `404`; `412`.
- Side effects: the subscription and its events go inactive with new
  `rowVersion`s; nothing is matched to it until it is activated.

### `DELETE /api/v1/notificationselfservice/subscription/{id}`

Removes a subscription and its events. Irreversible.

- Headers: `If-Match: <rowVersion>`.
- Answers `200` with an empty body. `404`; `412`.
- Side effects: a batch already in its retry schedule is abandoned at the
  next retry; a queued batch is dropped at flush.

### `POST /api/v1/notificationselfservice/subscriptionEvent`

Adds an event type to a subscription, optionally scoped to targets.

- Body: `{"subscriptionId": "sub_8f3e1a0c2b6d", "eventType": "IncomingPaymentProcessed", "targetType": 0, "targetIds": ["00000000-0000-4000-8000-000000000978"]}`.
  `targetType` is required: `0` Account, `1` Company, `2` CompanyGroup.
  `targetIds` may be empty, which makes the event subscription-wide.
  `eventType` is not checked against Banking Circle's thirteen values.
- Answers `200`:

  ```json
  {"id": "subev_1a2b3c4d5e6f", "eventType": "IncomingPaymentProcessed", "isActive": true,
   "subscriptionId": "sub_8f3e1a0c2b6d", "status": 2, "rowVersion": "AAAAAAAAAC0=",
   "subscriptionEventTargetDetails": [{"id": "tgt_7e6d5c4b3a29", "targetId": "00000000-0000-4000-8000-000000000978", "targetType": 0}]}
  ```

  `subscriptionEventTargetDetails` is `null` with no targets. `400` when
  `targetType` or `eventType` is missing; `404` for an unknown
  subscription; `409` when the subscription already has that `eventType`
  (case-insensitive). No `If-Match`.
- Side effects: the subscription's `rowVersion` changes, so a client
  holding the old one gets `412` on its next write.

### `PUT /api/v1/notificationselfservice/subscriptionEvent/{id}/targets`

Replaces every target on one event.

- Headers: `If-Match: <the event's rowVersion>`.
- Body: `{"targetType": 0, "targetIds": ["00000000-0000-4000-8000-000000000978"]}`;
  at least one id.
- Answers `200` `{"id": "subev_…", "eventType": "…", "subscriptionId": "sub_…", "status": "Active", "rowVersion": "AAAAAAAAAC4=", "subscriptionEventTargetDetails": [{…}]}`.
  `status` is a word here (`Active`, `Inactive`, `Retired`, `None`) where
  every other answer has the number: the real API's inconsistency,
  reproduced. `400` for a missing `targetType` or no ids; `404`; `412`.
- Side effects: the event's `rowVersion` changes; the subscription's does
  not.

### `DELETE /api/v1/notificationselfservice/subscriptionEvent/{id}`

Removes one event from its subscription.

- Headers: `If-Match: <the event's rowVersion>`.
- Answers `200` with an empty body. `404`; `412`.
- Side effects: the subscription's `rowVersion` changes.

### `POST /api/v1/notificationselfservice/clienttest/{subscriptionId}`

Sends a test notification now, through the same encrypted pipe.

- Body: none.
- Answers `200` `{"status": "sent", "subscriptionId": "sub_8f3e1a0c2b6d", "eventType": "PaymentStatus"}`;
  `404`. `sent` means queued and flushed, not delivered: the outcome is in
  the `notifications` activity log.
- Side effects: one `PaymentStatus` notification with
  `"payload": {"EndToEndId": "clienttest_…", "TransactionStatus": "PendingProcessing"}`
  is queued and the subscription's queue is flushed at once, taking
  anything else waiting with it. It ignores event types, targets and
  whether the subscription is active, but a paused subscription parks it.

### `GET /api/v1/reports/intraday-reconciliation-paged-report`

The bookings a reconciliation sweep reads: one row per booking, no status
field. A row's existence is the booking. Only booked, processed and
reversed payments appear; a rejection or missing funding never does.

- Query, required: `FromTransactionDate`, `ToTransactionDate`,
  `FromCreatedAt`, `ToCreatedAt` (`YYYY-MM-DD` or a date-time, with or
  without zone and fraction), `PageNumber` and `PageSize` (positive
  integers). Optional: `AccountId` (comma-separated account ids,
  case-insensitive), `PropertiesIncluded` or `PropertiesExcluded`
  (comma-separated property names; the deprecated repeated
  `IncludeProperties` and `ExcludeProperties` are read when the new ones
  are absent).
- Filtering: the transaction-date range applies to the row's `reportDate`
  (the business day it booked on), the created-at range to the payment's
  creation date (UTC calendar date). Both inclusive, compared on the date
  part. Rows are ordered by `processedTimestamp`, then `paymentId`, a
  booking before its reversal, and paged.
- Properties: with neither parameter, Banking Circle's default list
  applies, which leaves `paymentId`, `processedTimestamp`, `return`,
  `latestStatusChangedTimestamp`, `statusReasonCode`,
  `statusReasonDescription`, `clientOrderId` and
  `additionalRemittanceInformation1`–`3` null. `PropertiesIncluded` sends
  exactly the named ones; `PropertiesExcluded` sends all but the named
  ones (empty means all). If both are sent, `PropertiesIncluded` wins. An
  unselected property is sent as null, not dropped.
- Answers `200`, here with `PropertiesIncluded=PaymentId,ProcessedTimestamp,Account,DebitAmount,CreditDebitIndicator,ReportDate,PaymentReferenceNumber,UserReferenceNumber`:

  ```json
  {"reconciliations": [{"paymentId": "bcp_3e9a4c1d7b20", "account": "BE00SIMSGA00000001",
    "accountCurrency": null, "debitAmount": 150, "creditAmount": null, "creditDebitIndicator": "DBIT",
    "transactionAmount": null, "transactionAmountCurrency": null, "valueDate": null,
    "reportDate": "2026-09-25T00:00:00+00:00", "return": null, "latestStatusChangedTimestamp": null,
    "processedTimestamp": "2026-09-25T09:00:02Z", "statusReasonCode": null, "statusReasonDescription": null,
    "paymentReferenceNumber": "010F100000000001", "userReferenceNumber": "sttl_20260924_0001",
    "clientOrderId": null, "paymentDetails1": null, "paymentDetails2": null, "paymentDetails3": null,
    "paymentDetails4": null, "additionalRemittanceInformation1": null,
    "additionalRemittanceInformation2": null, "additionalRemittanceInformation3": null}]}
  ```

  `account` is the account's vIBAN; in this lab every row is on a
  safeguarding account (the debtor of a payout, the creditor of money in).
  Amounts are JSON numbers. A payout is `DBIT`, money in `CRDT`.
  `processedTimestamp` is null while a payment is only booked. A reversal
  adds a second row on the same `paymentId` with the amount negated, dated
  on the reversal, its reason in `statusReasonDescription`. A returned
  payout's money comes back as its own `CRDT` row with `return: true`,
  remittance in `paymentDetails1`–`4` and `/RETN/`, `/<code>/<text>`,
  `/MREF/<payout reference>` in `additionalRemittanceInformation1`–`3`.
  `userReferenceNumber` is the payout's `externalRef` (or the incoming
  payment's reference). A page past the end is an empty list.
- Errors: `400` with `Content-Type: application/problem+json`:

  ```json
  {"type": "https://tools.ietf.org/html/rfc7231#section-6.5.1", "title": "One or more validation errors occurred.",
   "status": 400, "extensions": {"traceId": "3f2a9c1e-…"},
   "errors": {"pageSize": ["A value for the 'PageSize' parameter or property was not provided."]}}
  ```

  A value that does not parse reads `"The value 'x' is not valid for PageNumber."`.
- Side effects: a `report.intraday` event in the `reports` log, amber when
  refused.

### `GET /api/v1/reports/rejection-report`

Outgoing payments instructed on a transaction date that could not be
processed.

- Query: `TransactionDate` (required, date or date-time; only the date part
  is used). Booleans, as `true`/`false` or `1`/`0`: `IncludeReceived`
  (default true; pending payments), `IncludeMissingFunds` (default true),
  `ExcludeBooked` (default false; drops pending payments that have booked,
  which in this lab is every pending one), `IncludeReversals` (accepted and
  ignored: it covers direct debits only).
- A payment belongs to the date its creation falls on as a business day.
  Rows: `OutgoingPaymentRejected` as `Rejected`, `MissingFunding` as
  `Insufficient Funds`, still `OutgoingPaymentBooked` as `Received`. A
  reversed or processed payout never appears; neither does money in.
- Answers `200`:

  ```json
  {"rejections": [{"pIdChanneluser": null, "pTxndate": "2026-09-25T00:00:00+00:00",
    "reportDate": "2026-09-25T00:00:00+00:00", "customerId": null, "account": "BE00SIMSGA00000001",
    "accountCurrency": "EUR", "valueDate": "2026-09-25T00:00:00+00:00", "paymentAmount": 150,
    "paymentCurrency": "EUR", "transferCurrency": "EUR", "destinationIban": "GB00SIM0000000000003",
    "paymentReferenceNumber": "010F100000000003", "userReferenceNumber": "sttl_20260924_0002",
    "fileReferenceNumber": "", "sourceType": "Single payment", "status": "Rejected",
    "statusReason": "Payment rejected by the beneficiary bank (simulated)"}]}
  ```

  No `paymentId`: `paymentReferenceNumber` is the handle back. A
  `Received` row has `statusReason` `""`; `Insufficient Funds` reads
  "Insufficient funds on the debtor account (simulated)". `reportDate` is
  today on the lab clock. Errors are the same `400` problem body as the
  intraday report.
- Side effects: a `report.rejection` event in the `reports` log.

### `GET /api/v1/payments/singles/{paymentId}/status`

One payment's status, the sweep's fallback for what the intraday report
does not show as processed.

- Answers `200` `{"status": "Processed"}`: `PendingProcessing` (booked,
  not finished), `Processed` (outgoing or incoming), `Rejected`,
  `MissingFunding`, `Reversed`. A returned payout stays `Processed`. `404`
  `{"error": "payment not found", "status": 404}`, which is also the answer
  for any payment made before the last restart.
- Side effects: a `payment.status` event in the `reports` log.

## Lab plumbing

On the bridge, `:8095`: plain HTTP, no auth, same-network trust. Real
Banking Circle has none of these. They stand in for B4B's hand-off, a wire
arriving on the safeguarding account, and a platform-internal balance
service. Bodies are decoded strictly.

### `POST /internal/payments`

B4B's hand-off: books a payout from the safeguarding account to the
beneficiary's account. Called by b4b when its payout reaches
`B4BTMApproved`.

- Body:

  ```json
  {"paymentId": "bcp_3e9a4c1d7b20", "accountId": "6f1c2b9e-4d0a-5c3b-8e7f-1a2b3c4d5e6f",
   "amount": "150.00", "currency": "EUR", "externalRef": "sttl_20260924_0001",
   "holder": "Outlet One Ltd", "iban": "GB00SIM0000000000003"}
  ```

  `paymentId` (the caller's) and `accountId` (a UUID; the creditor account,
  opened on first use with a `XX00SIMGEN…` vIBAN) are required. `amount` is
  a positive decimal with at most two places. `currency` is `EUR` (the
  default) or `GBP`. Instead of `iban`, `accountNumber` and
  `financialInstitution` name a UK account.
- Answers `202` with the payment, booked:

  ```json
  {"id": "bcp_3e9a4c1d7b20", "settlementId": "sttl_20260924_0001",
   "fromAccountId": "00000000-0000-4000-8000-000000000978", "toAccountId": "6f1c2b9e-4d0a-5c3b-8e7f-1a2b3c4d5e6f",
   "toIban": "GB00SIM0000000000003", "toHolder": "Outlet One Ltd", "amount": "150.00", "currency": "EUR",
   "paymentReferenceNumber": "010F100000000001", "state": "OutgoingPaymentBooked",
   "createdAt": "2026-09-25T09:00:00Z", "updatedAt": "2026-09-25T09:00:00Z"}
  ```

  `400` `{"error": "…", "status": 400}` for a missing `paymentId` or
  `accountId`, a bad amount, another currency
  (`no safeguarding account configured for currency "USD"`), an
  `accountId` that is not a UUID, or an unknown field.
- Side effects: `OutgoingPaymentBooked` is notified at once. After
  `PROCESSING_DELAY` the ledger moves the money and the payment ends
  `OutgoingPaymentProcessed`, `MissingFunding` (the safeguarding account is
  short) or `OutgoingPaymentRejected` (any other failure, or a forced
  outcome), each notified. A forced `Pending` leaves it booked for good.
  On `OutgoingPaymentProcessed` the beneficiary's bank is credited:
  `POST {BANK_CREDIT_URL}` with
  `{"iban", "account_number", "financial_institution", "holder", "amount", "currency", "reference"}`
  (reference: the `externalRef`, else the payment id), once,
  fire-and-forget, skipped when the
  payout names neither an IBAN nor an account number. A `payment.create`
  event in the `payments` log. A repeated `paymentId` is not refused: it
  replaces the earlier record and runs the lifecycle, and moves the money,
  again.

### `POST /internal/incoming-payments`

Money landing on a safeguarding account: the acquirer's lump sum. Called
by worldline on each morning slot, by the console's
`POST /api/actions/fund-sga`, and by the harness.

- Body: `{"currency": "EUR", "amount": "1250.00", "reference": "WORLDLINE SETTLEMENT EUR 2026-09-24"}`.
  `currency` is `EUR` or `GBP` and picks the account; there is no account
  field.
- Answers `202`:

  ```json
  {"id": "bcp_9b8a7c6d5e4f", "fromAccountId": "external_worldline",
   "toAccountId": "00000000-0000-4000-8000-000000000978", "amount": "1250.00", "currency": "EUR",
   "reference": "WORLDLINE SETTLEMENT EUR 2026-09-24", "paymentReferenceNumber": "010F100000000002",
   "remittanceInformation": ["WORLDLINE SETTLEMENT EUR 2026-09-24"], "state": "IncomingPaymentBooked",
   "createdAt": "2026-09-25T08:47:10Z", "updatedAt": "2026-09-25T08:47:10Z"}
  ```

  `400` for another currency, a non-positive or malformed amount, or an
  unknown field.
- Side effects: the account is credited at once; `IncomingPaymentBooked`
  is notified, then `IncomingPaymentProcessed` after `PROCESSING_DELAY`. A
  `payment.incoming` event in the `payments` log.

### `GET /internal/accounts/{accountId}/balances`

The balance without mTLS or a token. Called by the settlement stand-in's
safeguarding gate before each payout, and by the console's
`POST /api/actions/fund-sga` to skip an account that already holds money.

- Answers `200` `{"accountId": "00000000-0000-4000-8000-000000000978", "balances": [{"type": "CurrentBalance", "currency": "EUR", "beginOfDayAmount": "0.00", "financialDate": "2026-09-25", "intraDayAmount": "1250.00", "lastTransactionTimestamp": "…"}]}`,
  each entry as on `GET /api/v1/accounts/{accountId}/balances`. `400`
  for a non-UUID; `404` for an unknown account (never opened here).

## Lab control

Not part of Banking Circle. The `/sim/*` routes and the introspection
reads are on `:8085` behind the same mTLS and bearer token as the Connect
API; the test hooks under `/internal/payments/…` are on the bridge `:8095`
with no auth.

### `GET /health`

On `:8085` only (the bridge has none). No bearer token, but the TLS
handshake still wants a client certificate under `BC_MTLS=require`.
Answers `200` `{"status": "ok", "service": "banking-circle"}`.

### `GET /sim/activity`

The three logs described under *At a glance*, Bearer.

- Query: `limit` (default 100; each ring keeps 256).
- Answers `200` `{"logs": [{"name": "payments", …}, {"name": "notifications", …}, {"name": "reports", …}]}`,
  in that order, each `{"name", "title", "note", "total", "kept", "last", "events": [{"seq", "at", "op", "peer", "summary", "status", "detail"}]}`.

### `GET /sim/emails`

The warning and deactivation emails the retry schedule would have sent.

- Answers `200` `{"count": 1, "emails": [{"to": "alerts@fintechlab-simple.local", "kind": "warning", "subscription_id": "sub_8f3e1a0c2b6d", "endpoint": "https://…", "subject": "Webhook delivery is failing", "attempt": 7, "at": "2026-09-25T09:00:03Z"}]}`.
  `kind` is `warning` or `deactivation` (subject "Webhook subscription
  deactivated"); `to` is `"(no email configured)"` when the subscription
  has none. The last 500 are kept.

### `GET /sim/delivery-config`

The delivery table this process is running: the config file with
`BC_TIME_SCALE` applied.

- Answers `200` `{"retry_schedule": [{"after": "15s"}, {"after": "30s"}, …, {"after": "2h0m0s", "email": "warning"}, …, {"after": "48h0m0s", "email": "deactivation"}], "time_scale": 86400, "deactivate_after_retries": 11, "batch_flush_interval": "2s", "default_max_notifications_per_message": 1000, "request_timeout": "10s"}`.
  `after` is the unscaled delay. `request_timeout` is reported but not
  applied: a delivery attempt times out after a fixed 8 s.

### `POST /sim/subscription/{subscriptionId}/notifications`

Queues synthetic notifications without flushing, so batching can be
watched.

- Query: `count` (1–1000, default 1), `eventType` (default
  `OutgoingPaymentProcessed`), `flush=true` to send at once.
- Answers `202` `{"enqueued": 5, "subscriptionId": "sub_8f3e1a0c2b6d", "maxNotificationsPerMessage": 5}`.
  `400` for a count out of range; `404`.
- Side effects: each notification carries `{"paymentId": "sim_<subscriptionId>_<n>", "status": "<eventType>"}`
  under `payment` (or `payload` for `PaymentStatus`). Like `clienttest`, it
  ignores event types, targets and whether the subscription is active.

### `GET /sim/subscription/{subscriptionId}/pending`

What is not moving for a subscription, and why.

- Answers `200` `{"subscriptionId": "sub_8f3e1a0c2b6d", "pending": 2, "paused": false, "queued": 0}`.
  `pending` is retained after the retry schedule gave up (cleared by
  `PUT …/activate`); `queued` is parked behind a pause (cleared by
  `/resume`). An unknown id answers zeros, not `404`.

### `POST /sim/subscription/{subscriptionId}/pause`

Holds one subscription's deliveries. Payments, matching and ordering carry
on; the notifications wait.

- Answers `200` `{"subscriptionId": "sub_8f3e1a0c2b6d", "endpoint": "https://…", "paused": true, "queued": 0}`;
  `404`.
- Side effects: everything enqueued, and any batch about to be sent, is
  parked in order. The subscription's status and `rowVersion` do not
  change. `notification.pause` and, per parked batch,
  `notification.queued` in the `notifications` log.

### `POST /sim/subscription/{subscriptionId}/resume`

Releases a paused subscription.

- Answers `200` `{"subscriptionId": "sub_8f3e1a0c2b6d", "endpoint": "https://…", "paused": false, "released": 6, "queued": 0}`;
  `404`.
- Side effects: the parked notifications go out at once, oldest first, cut
  into batches of the subscription's `maxNotificationsPerMessage`, one
  batch after another.

### `POST /sim/payments/{id}/return`

The same handler as `POST /internal/payments/{id}/return`, on `:8085`
behind the bearer token, because that is the surface the console holds a
token for.

### `POST /sim/payments/{id}/reverse`

The same handler as `POST /internal/payments/{id}/reverse`, on `:8085`
behind the bearer token.

### `GET /accounts`

Introspection: every ledger account.

- Answers `200` `{"accounts": [{"id": "00000000-0000-4000-8000-000000000978", "viban": "BE00SIMSGA00000001", "holder": "Infinite Safeguarding Account EUR", "currency": "EUR", "balance": "1100.00"}]}`,
  unordered. Creditor accounts appear once paid into, held by
  `"Auto-Vivified Creditor <id>"`.

### `GET /accounts/{id}`

One account, by id or by vIBAN (no UUID check here).

- Answers `200` with the account as in `GET /accounts`; `404`
  `{"error": "account not found", "status": 404}`.

### `GET /payments`

Introspection: every payment, outgoing and incoming.

- Answers `200` `{"payments": [{…}]}`, unordered, each as
  `GET /payments/{id}` returns it.

### `GET /payments/{id}`

One payment with the lab's bookkeeping.

- Answers `200` with the payment as `POST /internal/payments` returns it,
  plus whatever applies: `processedAt`; `reversedAt` and `reversalReason`;
  `returnedBy` on a returned payout; `return`, `returnOf`,
  `returnedReference`, `returnReasonCode`, `returnReasonDescription` and
  `remittanceInformation` on a return. `state` is the last notification
  type fired. `404` `{"error": "payment not found", "status": 404}`.

### `GET /reconciliation`

The lab's older, simpler reconciliation: payments last changed on a date.

- Query: `date` (`YYYY-MM-DD`, required, matched against `updatedAt`, UTC),
  `account_id` (optional; the payment's debtor or creditor).
- Answers `200` `{"payments": [{…}]}`. `400`
  `{"error": "date query parameter required (YYYY-MM-DD)", "status": 400}`.

### `POST /internal/payments/{id}/reverse`

On the bridge, no auth. The scheme reverses a processed payout.

- Body: optional `{"reason": "scheme"}`.
- Answers `200` with the payment, now `"state": "Reversed"`, with
  `reversalReason` and `reversedAt`. `404` `{"error": "payment not found", "status": 404}`;
  `409` `{"error": "bankingcircle: payment is not in OutgoingPaymentProcessed state", "status": 409}`
  (also for a payout already returned, or when the money cannot move back).
- Side effects: the money moves from the creditor account back to the
  safeguarding account; the beneficiary's bank is not debited. Two
  notifications: `OutgoingPaymentBooked` for the reversal booking (positive
  amount), then `Reversed`. The intraday report gains the negative row.

### `POST /internal/payments/{id}/return`

On the bridge, no auth. The beneficiary's bank sends a processed payout
back.

- Body: optional `{"reasonCode": "AC04", "reasonDescription": "Closed account number"}`.
- Answers `200` with the new incoming return payment (the payout keeps its
  state and gains `returnedBy`):

  ```json
  {"id": "bcp_0f1e2d3c4b5a", "fromAccountId": "6f1c2b9e-4d0a-5c3b-8e7f-1a2b3c4d5e6f",
   "toAccountId": "00000000-0000-4000-8000-000000000978", "amount": "150.00", "currency": "EUR",
   "paymentReferenceNumber": "010F100000000004", "return": true,
   "returnOf": "bcp_3e9a4c1d7b20", "returnedReference": "010F100000000001",
   "returnReasonCode": "AC04", "returnReasonDescription": "Closed account number",
   "remittanceInformation": ["RETURN OF PAYMENT", "010F100000000001", "AC04 Closed account number"],
   "state": "IncomingPaymentBooked", "createdAt": "…", "updatedAt": "…"}
  ```

  `404`; `409` for a payout that is not processed or has already come
  back.
- Side effects: the money moves back to the safeguarding account; the
  beneficiary's bank is not debited. `IncomingPaymentBooked`, then
  `IncomingPaymentProcessed` after `PROCESSING_DELAY`, both with
  `return: true`. A returned payout can no longer be reversed.

### `GET /internal/payments/outcomes`

On the bridge, no auth. The forced outcomes still waiting for a payout.

- Answers `200` `{"queued": ["Rejected", "Pending"]}`; `[]` when none.

### `POST /internal/payments/outcomes`

On the bridge, no auth. Decides how the next payouts end, one each, in
order, after anything already queued.

- Body: `{"outcomes": ["Rejected", "Pending"]}`. `Rejected` ends a payout
  `OutgoingPaymentRejected` without moving money; `Pending` leaves it
  `OutgoingPaymentBooked` for good.
- Answers `200` with the whole queue, `{"queued": ["Rejected", "Pending"]}`.
  `400` `{"error": "bankingcircle: unknown outcome (want Rejected or Pending)", "status": 400}`
  or for a body that does not decode.
- Side effects: taken by whichever `POST /internal/payments` comes next,
  whoever sends it. In memory only.

## Called by

- **b4b** (`cmd/b4b/payments.go`): `POST :8095/internal/payments` once a
  payout reaches `B4BTMApproved`, with a fresh `bcp_…` id and the
  beneficiary's derived account id.
- **worldline** (`cmd/worldline/cycle.go`): `POST :8095/internal/incoming-payments`,
  the morning slot's lump sum, once per currency.
- **The settlement stand-in** (`cmd/settlement`):
  `GET :8095/internal/accounts/{accountId}/balances`, its safeguarding
  gate before each payout.
- **The platform under test**: the Connect API on `:8085` with the connect
  kit's credentials and client certificate: authorize, balances, its own
  subscription and its events (it subscribes on boot), and its
  reconciliation sweep's `intraday-reconciliation-paged-report`, with
  `payments/singles/{paymentId}/status` as the fallback. It receives the
  encrypted webhooks.
- **The console** (`cmd/console`), on `:8085` as `BC_USER` with the client
  certificate: `GET /health` (health monitor, `GET /api/overview`,
  `POST /api/services/{id}/probe`); authorize and `GET /accounts`
  (`GET /api/banks`); `GET …/notificationselfservice/subscription?PageSize=50`
  and `GET /sim/subscription/{subscriptionId}/pending`
  (`GET /api/banking-circle/subscriptions`); `POST …/clienttest/{subscriptionId}`
  (`POST /api/banking-circle/subscriptions/{id}/test`);
  `POST /sim/subscription/{subscriptionId}/pause` and `/resume`
  (`POST /api/banking-circle/subscriptions/{id}/pause`, `/resume`);
  `GET /payments` (`GET /api/banking-circle/payouts`);
  `POST /sim/payments/{id}/return` and `/reverse`
  (`POST /api/banking-circle/payouts/{id}/return`, `/reverse`);
  `GET /sim/activity` (`GET /api/services/{id}/activity`, `GET /api/flow`,
  `GET /api/flow/hops/{id}`); `GET /sim/delivery-config` (`GET /api/config`);
  the subscription list and `PUT …/subscription/{id}/activate` or
  `/deactivate` with `If-Match` on every subscription pointing at the
  receiver (`GET /api/stand-ins`, `POST /api/stand-ins/{id}`). On the
  bridge: `GET /internal/accounts/{accountId}/balances` and
  `POST /internal/incoming-payments` (`POST /api/actions/fund-sga`).
- **The harness**: `banking-circle-payout`
  (`internal/harness/scenario_bankingcircle.go`) authorizes, reads
  `GET /accounts/{id}`, funds with `POST :8095/internal/incoming-payments`
  and checks `GET /reconciliation?date=…&account_id=…`.
  `banking-circle-subscription` (`internal/harness/scenario_bcsubscription.go`)
  creates a subscription (and a duplicate, expecting `409`), adds an event,
  tries a `PUT` without `If-Match` (`412`) and with it, then
  `POST /sim/subscription/{subscriptionId}/notifications?count=5` and
  checks one batch of five arrives. `banking-circle-retry-deactivation`
  points a subscription at a dead endpoint, enqueues with `flush=true`,
  waits for the deactivation, reads `GET /sim/emails`, repoints it and
  `PUT …/activate` to see the two retained notifications redelivered.
  `b4b-oversight-gates` reaches `POST /internal/payments` through b4b.
- **Agents and operators** (agent.runbooks.md): `POST`/`GET :8095/internal/payments/outcomes`
  and `POST :8095/internal/payments/{id}/return` and `/reverse`.
- **Banking Circle calls out** to every subscription endpoint (the
  receiver's `/raw-events`, or the platform's listener) and to the business
  bank's `POST /internal/credit`.

Routes checked against the source at commit db5e695.
