# Principles

## Sim vs prod

| Sim (this repo) | Prod |
| --- | --- |
| A ledger in memory or in one JSON file, rewritten whole | Durable books, reconciliation |
| Fake IBANs `GB00SIM…` | Issued IBANs, sanctions, KYC |
| HMAC secret `sim-hmac-dev-only` | Rotated secrets in a vault |
| Allowlist = compose hostnames | Named public CIDRs + mTLS + WAF |
| Retries in seconds | Hours, DLQ, paging |
| One happy path | Partial failures, returns, recalls |

The lab is **vendor-shaped** (idempotency, payment id, webhook) so the *conversation* transfers. It is **not** a substitute for a real integration.

## No vendor secrets

- Do not commit API tokens, doc-portal passwords, or customer dumps.
- Example secrets must be fake-and-obvious.
- Do not copy proprietary request/response bodies from any bank or PSP.
- If a payload looks specific to one vendor, rewrite it until it is generic.

## Connected, not decorative

A service that only answers `/health` is not wired. `make demo-payment` fails unless the receiver stores an event for the payment that was just created.
