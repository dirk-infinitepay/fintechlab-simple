# Scenario: ACI card-gateway webhook

ACI is the online card-payment gateway. This lab's mock plays ACI's role: a
customer paying with Visa or via Shopify is not something anything here
processes — it's simulated as a trigger, and the mock sends the exact
encrypted webhook notification ACI would, so a real buddy `settle-aci-webhook`
(or its gateway proxy) can be pointed at it and receive something
indistinguishable from the real vendor. Independent of every other flow in
this lab — no settlement, B4B, or Banking Circle involvement.

## 1. Simulate a card payment

```bash
curl -s -X POST http://127.0.0.1:8087/internal/simulate-payment \
  -d '{
        "merchant_transaction_id": "demo-tx-1",
        "payment_type": "RX",
        "payment_brand": "VISA",
        "presentation_amount": "42.50",
        "presentation_currency": "EUR",
        "plugin_type": "SHOPFY",
        "source": "OPP",
        "result_code": "000.300.100",
        "result_description": "Risk check successful"
      }' | python3 -m json.tool
```

Returns `{"id":"aci_evt_..."}` immediately — encryption happens
synchronously, delivery is fire-and-retry in the background (same
convention as every other webhook sender in this lab).

## 2. Inspect what was actually sent

```bash
curl -s http://127.0.0.1:8087/internal/sent/aci_evt_XXXXXXXXXXXX | python3 -m json.tool
```

`notification` is the exact plaintext payload (real camelCase field names,
including ACI's own mixed casing like `RiskOrderId`) — this lab already
holds it, no need to decrypt its own ciphertext to show it to you.
`ciphertext_hex` / `iv_hex` / `tag_hex` are the real wire material: AES-256-GCM,
hex-encoded (not base64 — that's Banking Circle's convention, not ACI's),
IV and tag delivered as headers (`X-Initialization-Vector` /
`X-Authentication-Tag`) alongside the hex ciphertext body. `delivery_status`
is `delivered` or `failed` once the retry loop settles.

## 3. Where it went

By default (`compose.yml`), `ACI_WEBHOOK_TARGET_URL` points at `receiver`'s
raw, unvalidated capture sink — proof delivery reached its destination
with the right headers, not proof of content (`receiver` holds no ACI
key, same reasoning as Banking Circle's webhook):

```bash
curl -sk https://127.0.0.1:8443/raw-events | python3 -m json.tool
```

Point a real, locally-running `settle-aci-webhook` there instead
(`ACI_WEBHOOK_TARGET_URL=http://localhost:3013/api/v1/webhook`, its own
real default — see [docs/local-domains.md](../local-domains.md)) to watch
it decrypt and store the notification for real.

## 4. What you just proved

The mock genuinely runs ACI's real AES-256-GCM envelope — not a canned
fixture — and it is byte-interoperable with ACI's own decryptor: the
`internal/aci` package's known-answer test decrypts ACI's own published
reference vector and asserts the exact plaintext fields, so the same
algorithm encrypting *this* notification is provably correct, not just
internally self-consistent. That is `aci-payment-notification` in `make
e2e`; this walkthrough is the same assertions, by hand.
