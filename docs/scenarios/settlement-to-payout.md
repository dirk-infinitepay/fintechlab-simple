# Scenario: acquirer → settlement file → payout → notification

The Worldline (acquirer), B4B (payout rail), and Banking Circle (B4B's own
downstream settlement bank) half of the lab, by hand.

Follow the money. Nothing here reads a shared directory: the platform side
learns what it is owed the same way it would in production — by pulling a
file off Worldline's SFTP server.

What only the lab can do — seed trading, fire a cycle now, land a wire,
read what a vendor sent — goes through the lab API on `:8097`. When the lab
sets `LAB_API_TOKEN`, add `-H "Authorization: Bearer $LAB_API_TOKEN"` to
those calls.

## 1. A card payment happens at the merchant's paypoint

Worldline is the one who knows, because Worldline acquired it. Seed it
there. `mid` is the submerchant identifier — the per-outlet key the
platform will eventually pay out against.

Worldline settles T+1, so date the transactions yesterday for today's
morning cycle to pick them up:

```bash
YESTERDAY=$(date -u -d yesterday +%F)   # macOS: date -u -v-1d +%F

curl -s -X POST http://127.0.0.1:8097/v1/services/worldline/sim/transactions \
  -H 'Content-Type: application/json' \
  -d "[
    {\"mid\":\"GB00SIM0000000000003\",\"currency\":\"EUR\",\"amount_cents\":10000,\"date\":\"$YESTERDAY\"},
    {\"mid\":\"GB00SIM0000000000003\",\"currency\":\"EUR\",\"amount_cents\":5000,\"date\":\"$YESTERDAY\"},
    {\"mid\":\"GB00SIM0000000000009\",\"currency\":\"EUR\",\"amount_cents\":2500,\"date\":\"$YESTERDAY\"}
  ]" | python3 -m json.tool
```

## 2. The morning cycle fires

Worldline delivers the same settlement twice a day: a **morning file**
(`ER`, in an 08:00–10:00 window) and an **afternoon confirmation** (`AR`).
The scheduler runs on its own, but you can fire a slot now:

```bash
curl -s -X POST "http://127.0.0.1:8097/v1/services/worldline/sim/settlement-cycle/run?slot=morning&date=$YESTERDAY" \
  | python3 -m json.tool
```

Two things come back, and both matter:

- `files` — **one file per currency**, covering *every* submerchant that
  traded. The filename carries a timestamp, the contract identifier, the
  slot and the currency, and **no merchant**: the per-outlet split lives in
  the rows (`ADDITIONAL_REF_2`), which is why the platform has to parse it.
- `lump_sums` — the money. `status: "credited"` means the safeguarding
  account was just credited with the file's own total. Only the morning
  slot moves money; the afternoon file confirms a settlement that already
  happened.

## 3. Read the file the way the platform does

The file sits on a real SSH/SFTP server on port `2222` — a genuine SSH
server, not an HTTP facade — plain, as Worldline leaves its settlement
files:

```bash
sftp -P 2222 infinitepay@127.0.0.1
# password: sim-sftp-dev-only  (WORLDLINE_SFTP_PASSWORD)
sftp> ls download
sftp> get download/<the _ER_ file>
```

A real deployment pins the server's host key. This lab writes the
simulator's public host key to `keys/wlsftp-keys/host_key.pub` at startup so you
can, and `settlement` already does
(`WORLDLINE_SFTP_KNOWN_HOST_PATH`).

## 4. The platform takes delivery and pays out per outlet

`settlement` polls the same server on `WORLDLINE_PULL_INTERVAL`. Force a
pull rather than waiting:

```bash
curl -s -X POST http://127.0.0.1:8083/worldline/pull | python3 -m json.tool
```

Each file it took delivery of shows its slot, whether it was `processed`,
how many per-MID payouts it found, and the settlement records it created.
The **afternoon file is retained and never processed** — paying against a
confirmation would pay every merchant twice.

```bash
curl -s http://127.0.0.1:8083/worldline/files | python3 -m json.tool
curl -s http://127.0.0.1:8083/reports/settlement/set_XXXXXXXXXXXX | python3 -m json.tool
```

## 5. B4B's gates

Every settlement record submits its net amount to **B4B** (JWT-signed,
RS512). Banking Circle is never called directly — real Banking Circle has
no payment-creation endpoint. Before anything is forwarded, three gates
apply:

1. The **safeguarding account** must hold the funds. On a fresh stack it
   starts empty, so a payout attempted before step 2's lump sum lands sits
   at `awaiting_funding`. That is the point, not a bug.
2. The beneficiary's **sanctions status** must be `pass`. Anything else and
   the payout stops at `sanctions_blocked` without calling B4B.
3. The payment's **creditor details must match the beneficiary**. They are
   a consistency check, not a recipient override: a mismatch is a 422 and
   nothing is forwarded.

```bash
curl -s http://127.0.0.1:8083/reports/settlement/set_XXXXXXXXXXXX/payout | python3 -m json.tool
```

If it is waiting on funds, credit the account and retry:

```bash
curl -s -X POST http://127.0.0.1:8097/v1/services/banking-circle/sim/incoming-payments \
  -d '{"currency":"EUR","amount":"1000000.00","reference":"worldline-lump-sum"}'

curl -s -X POST http://127.0.0.1:8083/reports/settlement/set_XXXXXXXXXXXX/retry-payout | python3 -m json.tool
```

B4B calls back on **every** regulatory-phase change — `B4BAccepted` →
`B4BSanctionsPending` → `B4BSanctionsApproved` → `B4BTMPending` →
`B4BTMApproved`. Callbacks are unsigned, unordered and may repeat, with no
replay endpoint, so if you miss one, read the current state instead:

```bash
curl -s -H "Authorization: Bearer $B4B_JWT" \
  http://127.0.0.1:8086/oversight/v1/payments/b4bp_XXXXXXXX | python3 -m json.tool
```

Only at `B4BTMApproved` does B4B hand the payment to Banking Circle.

## 6. Banking Circle: subscribe, then receive a batch

Real Banking Circle requires a Basic→Bearer token exchange (and mTLS,
unless `BC_MTLS` says otherwise). `make up` already generated a lab client
cert:

```bash
TOKEN=$(curl -s --cacert keys/certs/ca.pem --cert keys/certs/client.pem --key keys/certs/client-key.pem \
  -u harness:harness https://127.0.0.1:8085/api/v1/authorizations/authorize \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')
BC="curl -s --cacert keys/certs/ca.pem --cert keys/certs/client.pem --key keys/certs/client-key.pem -H \"Authorization: Bearer $TOKEN\""
```

A subscription needs an endpoint, a status, and its **own** 32-character
encryption key (the raw UTF-8 bytes are the AES-256 key, so no other length
can work):

```bash
curl -s --cacert keys/certs/ca.pem --cert keys/certs/client.pem --key keys/certs/client-key.pem \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -X POST https://127.0.0.1:8085/api/v1/notificationselfservice/subscription \
  -d '{"endpoint":"https://receiver:8443/raw-events?sub=byhand","status":2,
       "encryptionKey":"by-hand-notification-key-32chars",
       "email":"you@example.com","maxNotificationsPerMessage":5}' | python3 -m json.tool
```

Note what comes back: `encryptionKey` is `*Hidden*` — it is never echoed —
and a `rowVersion`. **Every** mutation needs that value in an `If-Match`
header, and a mismatch is a 412. It also changes on delivery retries, so an
in-flight update can be invalidated by a retry it knows nothing about.

Add an event, or the subscription receives nothing:

```bash
curl -s --cacert keys/certs/ca.pem --cert keys/certs/client.pem --key keys/certs/client-key.pem \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -X POST https://127.0.0.1:8085/api/v1/notificationselfservice/subscriptionEvent \
  -d '{"subscriptionId":"sub_XXXX","eventType":"OutgoingPaymentProcessed","targetType":1}'
```

Notifications are **batched**: a message goes out when the queue reaches
`maxNotificationsPerMessage`, or when the flush interval elapses. Deliveries
land in `receiver`'s capture sink, ciphertext and all:

```bash
curl -s https://127.0.0.1:8443/raw-events --cacert keys/certs/ca.pem | python3 -m json.tool
```

The `Nonce`, `AuthenticationTag` and `Checksum` headers are what you need
to decrypt it: AES-256-GCM over UTF-16LE-encoded JSON, tag carried
separately from the ciphertext.

## 7. Watch a subscriber fail

Point a subscription at something that does not answer and Banking Circle
retries on its documented schedule — eleven steps, ending 48 hours after
the first failure — then **deactivates** it, emails you, and **retains**
the undelivered notifications:

```bash
curl -s http://127.0.0.1:8097/v1/services/banking-circle/sim/emails | python3 -m json.tool
```

You are not waiting two days for that: `BC_TIME_SCALE` (86400 in compose)
divides every delay, so the whole story plays out in about four seconds
with the step count and shape intact. Reactivate and what it missed is
redelivered — events that happened *while* it was down are not backfilled.

## 8. What you just proved

The acquirer produced the settlement file and put it
somewhere the platform could only reach over SSH. The lump sum that landed
matched the file's own total, and the per-MID rows inside it added up to
that lump sum. The platform pulled it, parsed it, split it
per outlet, and each payout waited for real funds, a passing sanctions
status, and creditor details that agreed with the beneficiary — before B4B
drove its own lifecycle and handed the payment to Banking Circle, which
delivered a batched, encrypted notification and would have deactivated the
subscriber had it stopped answering.

That is `worldline-settlement-to-sftp`, `worldline-confirmation-file`,
`banking-circle-payout`, `banking-circle-subscription`,
`banking-circle-retry-deactivation` and `b4b-oversight-gates` in
`make e2e` — this walkthrough is the same assertions, by hand.
