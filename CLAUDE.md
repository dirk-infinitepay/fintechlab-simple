# Working in this repo

Read [docs/index.md](docs/index.md) for what the lab is and
[docs/principles.md](docs/principles.md) for the sim/prod line and the
no-vendor-secrets rule. [docs/structure/README.md](docs/structure/README.md)
explains how a service's code is split across `cmd/`, `internal/`,
`docker/`, `compose.yml`, `config/` and `docs/`.

## One architecture

This tree is the **standalone vendor simulations** only: `cmd/<service>`
plus `internal/<service>`, one process per simulated third party. They hold
the deep protocol work — RS512 JWT, AES-256-GCM webhook envelopes, real
SFTP+PGP, the Bambora file format — and that is the point of them.

The kernel/pod/recipe experiment lives on its own branches and is parked.
Do not add `core/`, `recipes/` or a pod runtime here. If a simulation needs
new behaviour, it goes in that vendor's own package.

## The platform under test is a plugin

The lab knows no platform by name. A platform registers its own card with
the console (`POST /api/plugins`) and follows the lab's clock service —
[docs/plugins.md](docs/plugins.md). Do not add a catalogue entry, a proxy
route or a UI special case for one particular platform; if a plugin needs
something the console cannot draw, add a generic component to
[docs/design-system.md](docs/design-system.md) and the contract.

## Driving the lab headless

[agent.runbooks.md](agent.runbooks.md) is the how-to: ports, bringing the
lab and the platform's runner up, the lab clock, running a settlement
and the reconciliation sweep, the Banking Circle test hooks, and what looks
broken but is not. Read it before driving the lab from a session.

## APIs

[docs/api/README.md](docs/api/README.md) maps every API in the lab: each
service's vendor surface, its `/internal/*` plumbing and `/sim/*` lab
control, and the console's own `/api`, with who calls which. A cold start is
`GET :8090/api` (every console route and what it reaches), then
`GET :8090/api/overview` (every service and where it lives).

A route and its documentation change together. The console's routes are
registered from the table in `cmd/console/routes.go`; each service's routes
are documented in `docs/api/services/<id>.md`. `go test ./cmd/console
./internal/console` fails when a route and its document disagree.

## What this exists to do

Run the settle path end to end against mocked third parties, so the real
platform can be pointed at it locally:

    worldline SFTP → settlement file → per-outlet payout → b4b (regulatory,
    positive) → banking-circle (funds, batched webhooks, intraday recon) →
    business bank

Every verification-style service answers positively. Failure injection is a
later concern; a happy path that actually connects is the deliverable.

## User interfaces

**Before writing or changing any UI, read
[docs/design-system.md](docs/design-system.md) and follow it.** It is the
source of truth for tokens, layout, components, behaviour contracts and copy
voice; `cmd/console/web/app.css` is one instance of it, not the definition.
If the two disagree, the document wins — change the document first, then
bring the implementations to it.
