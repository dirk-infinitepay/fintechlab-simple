# Local CA and TLS

`ca/generate.sh` (and the **ca** compose service) writes:

| File | Role |
| --- | --- |
| `keys/certs/ca.pem` | Trust anchor for notifier, banking-circle, and `curl --cacert` |
| `keys/certs/ca-key.pem` | Lab-only CA key — **do not reuse** outside this directory |
| `keys/certs/receiver.pem` / `receiver-key.pem` | Server cert for the receiver (`DNS:receiver`, `DNS:localhost`, `DNS:receiver.local`, `DNS:receiver.fintechlab-simple.test`, `IP:127.0.0.1`) |
| `keys/certs/banking-circle.pem` / `banking-circle-key.pem` | Server cert for the Banking Circle mock's mTLS listener (`DNS:banking-circle`, `DNS:localhost`, `DNS:banking-circle.fintechlab-simple.test`, `IP:127.0.0.1`) |
| `keys/certs/client.pem` / `client-key.pem` | Client cert every caller presents to Banking Circle's mTLS listener — **not optional**: real Banking Circle requires one from every caller, and this mock enforces it the same way (`tls.RequireAndVerifyClientCert`) |

Receiver speaks HTTPS only. Banking Circle's `:8085` listener requires
**both** a trusted client certificate at the TLS handshake **and** a
Bearer token on every route after `authorize` — mTLS is not a substitute
for the auth flow, it's an additional layer real Banking Circle also
enforces. Its `:8095` listener (the B4B bridge and the other lab-only
plumbing) is deliberately plain HTTP, no TLS, no auth — see
`docs/ARCHITECTURE-vendor-corrections.md` Addendum section C for why the
two can't share one listener. Notifier and the e2e suite both load
`CA_FILE=/keys/certs/ca.pem` so they will not accept a random public cert for
`receiver`/`banking-circle`.

Every other service in this lab (`bank`, `payment-api`, `settlement`,
`worldline`'s HTTP side, `b4b`, `aci`, `verify`) is plain HTTP — matching
how buddy's own real equivalents actually run in local dev too (their auth
is JWT/shared-secret/payload-encryption, not transport-level; see
`docs/ARCHITECTURE-phase3-corrections.md`'s "Existing Patterns Followed").
`worldline`'s SFTP side (`:2222`) uses SSH's own transport security
(host-key fingerprint pinning, not a certificate) — see
`docs/ARCHITECTURE-worldline-sftp-channel.md`.

This is **not** public PKI. Browsers will warn. That is expected.

Private keys stay under `keys/certs/` (gitignored). Never paste them into
tickets.

See [../local-domains.md](../local-domains.md) for mapping these
certificates' `.test` SANs to `/etc/hosts` entries, so a real buddy service
running on the host can point at this lab using config shaped like a real
vendor endpoint instead of bare `localhost:<port>`.

## A lab on another machine

The server certificates name `localhost` and `127.0.0.1`, which is wrong
the moment the platform dials the lab by any other name. `CERT_EXTRA_SANS`
adds names to the two certificates a client outside the lab dials
(`receiver`, `banking-circle`):

```sh
CERT_EXTRA_SANS="DNS:lab.example.test,IP:10.0.0.5" FORCE_CERTS=1 make certs
```

In the pod it is the init container's `CERT_EXTRA_SANS`
([../deploy-pod.md](../deploy-pod.md)). It only applies when a certificate
is issued, so an existing PKI has to be reissued (`FORCE_CERTS=1 make certs`
or `make keys-regenerate`; in the pod, a play with `regenerate-keys.yaml`) —
and the platform's copy of the CA downloaded again.

## What the console hands out

Each vendor card's **Connect your platform** panel serves the files a
*client* holds, and nothing else: the CA certificate, the mTLS client
certificate and key, B4B's signing keypair, Worldline's PGP keypair and SSH
host public key. The CA's key and the servers' private keys are not in the
list, and the list is fixed in `internal/console/connect.go` — the file
name in the URL is looked up in it, never joined onto a path. The console
has no authentication, so on a shared network anyone who can reach `:8090`
can take these; that is acceptable only because every one of them is fake
lab material, which is also why nothing real may ever be put under `keys/`.
