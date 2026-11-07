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
the lab API (`POST /v1/plugins`; the console's old `/api/plugins` still
forwards there) and follows the lab's clock service —
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

There is one API over the lab: the **lab API** (`cmd/labapi`, `:8097`,
`/v1`). The console page, scripts and agents all call it the same way; the
console (`cmd/console`) only serves the page and forwards `/v1` to it. Lab
state and lab-level behaviour go in the lab API, never in the console's
server; a vendor's behaviour still goes in that vendor's own package, and a
`/sim` hook it gains is reachable through the lab API's passthrough
(`/v1/services/{id}/sim/*`) with no lab API change.

The lab API reaches a simulation only on its **control port** (the vendor
port + 1000, `internal/simctl`): every `/sim/*` route, plain HTTP, no
vendor login. Never give the lab API a vendor's credentials or have it call
a vendor surface; if it needs something a sim only offers there, add a
`/sim` route that answers the same. Vendor auth is for the platform;
`LAB_API_TOKEN` is the only lab auth. Control ports are never published:
from outside the lab's network, the lab API is the one way in.

The e2e suite (`cmd/e2e`, `internal/e2e`, `make e2e`) plays the platform
on the vendor surfaces, with each vendor's login, and steers and reads the
lab through the lab API (`LAB_API_URL`, `LAB_API_TOKEN`) like any agent. It
never calls a control port or `/internal/*`, so it runs the same against a
lab in the cloud; `internal/e2e/lab_test.go` holds it to that.

A trap (`internal/trap`, `docs/lab-api/traps.md`) is armed in the sim that
applies it and acts through that vendor's own engine: a new hook lives in
the vendor's package, declares its fields and graph, offers each event to
the sim's trap store, and applies each step as an untouched transition
would. It only ever puts the vendor in a state the real vendor can be in.

[docs/api/README.md](docs/api/README.md) maps every API in the lab. A cold
start is `GET :8097/v1` (every route and what it reaches), then
`GET :8097/v1/services` (every service and where it lives).
[docs/lab-api/](docs/lab-api/README.md) is the design and the plan.

A route and its documentation change together. The lab API's routes are
registered from the table in `cmd/labapi/routes.go`, annotated for OpenAPI
in `cmd/labapi/openapi.go` (regenerate `docs/api/openapi.yaml` with
`go test ./cmd/labapi -run OpenAPI -update`) and documented in
`docs/api/lab-api.md`; each service's routes are documented in
`docs/api/services/<id>.md`. `go test ./cmd/labapi ./internal/console`
fails when a route and its document disagree.

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
