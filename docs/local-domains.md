# Local domain names, for demoing

By default every service is reachable at `localhost:<port>` — that's what
`make up` and the e2e suite use, and nothing below is required for the lab
itself to work. This is for the other direction: pointing a **real buddy
service**, running on the host (`pnpm nx serve ...`), at this lab's mocks
using config values shaped like the ones it would use against a real
staging/prod vendor endpoint, instead of bare `localhost:<port>`.

## Add hosts entries

```bash
sudo tee -a /etc/hosts >/dev/null <<'EOF'
127.0.0.1 payment-api.fintechlab-simple.test
127.0.0.1 bank.fintechlab-simple.test
127.0.0.1 notifier.fintechlab-simple.test
127.0.0.1 receiver.fintechlab-simple.test
127.0.0.1 settlement.fintechlab-simple.test
127.0.0.1 worldline.fintechlab-simple.test
127.0.0.1 worldline-sftp.fintechlab-simple.test
127.0.0.1 banking-circle.fintechlab-simple.test
127.0.0.1 b4b.fintechlab-simple.test
127.0.0.1 aci.fintechlab-simple.test
127.0.0.1 verify.fintechlab-simple.test
EOF
```

`.test` is the RFC 2606 TLD reserved for testing — guaranteed never
publicly delegated. Deliberately **not** `.local`: on some Linux setups
`nss-mdns`/`systemd-resolved` intercepts `.local` lookups via mDNS ahead of
`/etc/hosts`, which would make these names resolve unreliably or not at
all depending on what's installed. `.test` has no such ambiguity.

All eleven names resolve to `127.0.0.1`; which one you use just needs to
match the port compose already publishes there (see the table in
[README.md](../README.md)). `worldline-sftp` is a second name for the same
host as `worldline` — SSH doesn't do SNI/cert-hostname matching the way
TLS does (host identity is a key fingerprint,
`WORLDLINE_SFTP_HOST_KEY_FINGERPRINT`, not a name in a certificate), so it
exists purely so a `WORLDLINE_SFTP_HOST` value can read as its own thing
rather than reusing the HTTP gateway's name.

## TLS: only receiver and banking-circle carry certificates

Every other service here is plain HTTP, matching how buddy's own
equivalents actually run locally too (confirmed: `settle-aci-webhook`
proxies over `http://localhost:3013`, `verification-service` defaults to
`http://localhost:3003`, buddy's own B4B integration-test mock is plain
`node:http` — real B4B/ACI/verification-service auth is JWT/shared-secret/
payload-encryption, not transport-level, so there is nothing for TLS to add
there). `ca/generate.sh` gives `receiver.pem` and `banking-circle.pem`
these `.test` names as **additional** SANs (alongside their existing
`receiver`/`banking-circle`/`localhost` ones, for the in-container
hostnames compose's own network still uses) — nothing else needs a
certificate.

Trust the lab CA from a real buddy Node process (confirmed convention —
buddy's own local Banking Circle mock docs the same thing,
`apps/banking-circle/README.md`):

```bash
export NODE_EXTRA_CA_CERTS=/home/andrej/gh/fintechlab-simple/keys/certs/ca.pem
```

Set it **before** the Node process starts (Node reads it once at startup,
not lazily) — same caveat buddy's own doc calls out.

## Pointing buddy's env at this lab

Illustrative `.env.local` values for the buddy apps each service stands in
for — adjust ports/paths to whichever real env var name your buddy
checkout actually reads (see `docs/catalogue.md` for which service is which
vendor):

```bash
# apps/banking-circle — mTLS, so client cert + CA both matter
BC_API_BASE_URL=https://banking-circle.fintechlab-simple.test:8085
BC_AUTH_BASE_URL=https://banking-circle.fintechlab-simple.test:8085
# BC_API_CLIENT_CERT_PEM_BASE64 / _KEY_PEM_BASE64: base64 of
# keys/certs/client.pem / keys/certs/client-key.pem

# apps/accounts-settlement (B4B payout caller)
B4B_API_BASE_URL=http://b4b.fintechlab-simple.test:8086

# apps/gateway (verification-service caller)
VERIFICATION_SERVICE_API_URL=http://verify.fintechlab-simple.test:8088/api/v1/verification
VERIFICATION_INTERNAL_API_KEY=sim-verify-key-dev-only

# apps/settle-aci-webhook — this lab calls OUT to it, not the other way
# around: point THIS lab's ACI mock at wherever settle-aci-webhook (or its
# gateway proxy) actually listens, e.g. its own real default:
ACI_WEBHOOK_TARGET_URL=http://localhost:3013/api/v1/webhook

# apps/acquirer-fts (Worldline SFTP)
WORLDLINE_SFTP_HOST=worldline-sftp.fintechlab-simple.test
WORLDLINE_SFTP_PORT=2222
```

Nothing here changes what the containers call each other by — compose's
own internal network still uses the plain service names
(`http://banking-circle:8095`, etc.), same as before. This is only about
what a process running *outside* compose, on the host, points at.
