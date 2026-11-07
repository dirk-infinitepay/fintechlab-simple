# Code Structure

`docs/catalogue.md` says which side of the vendor/platform line each service
is on. This is a different question: how the **Go code** for one service is
organized on disk, and why it's split the way it is. Read this once; then
[banking-circle.md](banking-circle.md), [b4b.md](b4b.md), and
[worldline.md](worldline.md) each walk one vendor's code file by file.

## One module, fifteen binaries

`go.mod` declares a single module (`github.com/webduvet/fintechlab-simple`)
for the whole repo — no per-service `go.mod`, no per-service dependency set.
`go build ./cmd/<name>` produces one of fifteen independent binaries (`aci`,
`b4b`, `bank`, `bankingcircle`, `clock`, `console`, `e2e`, `labapi`,
`notifier`, `paymentapi`, `receiver`, `settlement`, `verification`,
`verify`, `worldline`). `docker/Dockerfile.<name>` (named after the compose
service, so `Dockerfile.lab-api` builds `cmd/labapi`) builds exactly one of
them into its own minimal image; `compose.yml` runs each image as its own
container, all but `e2e`, which `make e2e-docker` runs once against
them.

That's the real service boundary: **one OS process per `cmd/` entry,
talking HTTP or SFTP to the others — never a Go function call across it.**

(Aside: the compiled binaries a local `go build` leaves at the repo root —
`bankingcircle`, `b4b`, `settlement`, `receiver` — happen to be checked
into git too. They're build output, not part of this layout; ignore them
when reading the tree.)

## Every service is a 6-way split

| Slice | Example (banking-circle) | Contains |
| --- | --- | --- |
| `cmd/<name>/` | `cmd/bankingcircle/` | `package main` — env/flags, HTTP routing, TLS/auth wiring, the process entrypoint |
| `internal/<name>/` | `internal/bankingcircle/` | `package <name>` — the actual domain logic, HTTP-free and unit-tested on its own |
| `docker/Dockerfile.<name>` | `Dockerfile.banking-circle` | builds *only* `./cmd/<name>` |
| `compose.yml`'s `<name>:` block | `banking-circle:` | ports, env vars, volumes, `depends_on` — the real network topology |
| `config/<name>.json` | `config/banking-circle.json` | tunable data tables, only where one is interesting enough to need a file |
| `docs/ARCHITECTURE-<name>*.md` | `ARCHITECTURE-banking-circle-webhooks.md` | the prose design record: what's real-protocol-accurate vs simplified |

A service's code is never missing a piece — it's just never all in one
folder.

## What `internal/` means in Go — not what it sounds like

This is the part that looks like a service boundary and isn't. Go's
compiler enforces exactly one rule: a package under a path segment
literally named `internal/` can be imported only by code rooted at that
`internal/`'s **parent** directory. That parent is the repo root here, so
**every `internal/*` package is importable by every `cmd/*` binary in this
module** — freely — and by nothing outside the repo. It's a wall against
the outside world, not a wall between `bankingcircle` and `b4b`.

Proof it's not a per-vendor wall: `cmd/settlement` — the platform stand-in,
not a vendor at all — directly imports `internal/b4b` (for
`b4b.SignBearerToken`, so its outbound JWTs can never drift from what
`cmd/b4b`'s `VerifyBearerToken` checks) and both `internal/worldline` and
`internal/wlsftp` (to parse the exact file format and speak the exact
transport `cmd/worldline` produces). None of that is a network call — it's
one binary (`settlement`) linking someone else's domain package because the
two sides of a wire format must never disagree. The network calls
`settlement` makes are separate, over HTTP/SFTP, via `B4B_URL`,
`BC_INTERNAL_URL`, `WORLDLINE_SFTP_HOST`.

## Two graphs, not one

- **Compile-time graph** (small, boring): `cmd/X` → `internal/X` → shared
  infra. Answers "what does this binary link".
- **Runtime graph** (the division you're picturing): independent containers
  exchanging HTTP/SFTP, wired entirely through `compose.yml` env vars
  (`BANKING_CIRCLE_URL`, `BC_INTERNAL_URL`, `B4B_URL`, `WEBHOOK_URL`,
  `WORLDLINE_SFTP_HOST`, …). Answers "who calls whom".

Each per-service doc draws the runtime graph for that service — that's
where the clean division actually lives.

## Shared infra vs vendor-specific packages

| Kind | Packages | Shape |
| --- | --- | --- |
| Generic infra | `httputilx`, `money`, `allowlist`, `retry`, `waitfor`, `webhook`, `hmacx` | no vendor shape at all; imported by several `cmd/*` each (JSON responses, cent arithmetic, destination allowlisting, retry-until-2xx, wait-for-file, HMAC signing) |
| Vendor/domain-specific | `bankingcircle`, `b4b`, `worldline`, `wlsftp`, `sftpgateway`, `aci`, `verify`, `settlement` | one real protocol/contract each; usually one `cmd/*` importer, sometimes two (see below) |

`worldline` is the extreme case, because its protocol needs both a producer
and a consumer to be exercised at all: `internal/worldline` (the Bambora
file format + the acquired-transaction store) and `internal/wlsftp` (the
real SSH/SFTP+PGP transport) are each imported by *two* binaries —
`cmd/worldline` (the acquirer, produces files) and `cmd/settlement` (the
platform stand-in, consumes them) — generator and parser sharing one
package is what makes a round-trip test possible. `internal/sftpgateway`
(a separate, plain-HTTP-simulated file-exchange channel for onboarding docs
and reports) is used only by `cmd/worldline`.

**Naming trap**: three packages have "sftp" in the name; only one of them
speaks it.

| Package | Real SSH/SFTP? | Used by | Job |
| --- | --- | --- | --- |
| `internal/wlsftp` | **Yes** (`golang.org/x/crypto/ssh` + `github.com/pkg/sftp`), plus real OpenPGP | `cmd/worldline` (server), `cmd/settlement` (client) | the actual settlement-file transport |
| `internal/sftpgateway` | No — plain HTTP handlers simulating an SFT directory tree | `cmd/worldline` only | onboarding/corrections upload, WX/financial report download, on `:8084` |
| `internal/sftp` | No — local filesystem, no network | `cmd/settlement` only | where settlement's *own* generated reports get written |

## Per-service walkthroughs

- [banking-circle.md](banking-circle.md) — the settlement bank: ledger,
  payment engine, webhook subscriptions, batched AES-256-GCM delivery with
  an eleven-step retry schedule
- [b4b.md](b4b.md) — the payout rail: RS512-JWT auth, beneficiary
  sanctions/creditor gates, the `B4BAccepted`→…→`B4BTMApproved` lifecycle,
  the bridge into banking-circle
- [worldline.md](worldline.md) — the acquirer: the real Bambora
  settlement-file format over a real SFTP+PGP transport, split across two
  binaries
