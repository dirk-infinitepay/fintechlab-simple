# Lab API: draft spec

**Draft for review, 2026-10-20.** It is written to be edited: change a tag,
strike a line, answer an [open question](open.md), and the next pass builds
on what is left.

**Built so far (2026-11-07):** the `lab-api` service itself, the first part
of [phase 1](plan.md#phase-1--the-control-contract-lab-api-first-traps).
`cmd/labapi` serves `/v1` on `:8097` with everything the console's server
did, plus `/v1/services`, the passthrough to every sim's `/sim/*`, the
clock holds, a reset of the whole lab and every service's logs in one
read; `LAB_API_TOKEN`; every sim serves `/sim/info`, `/sim/logs` and
`/sim/reset`; the console is a page that forwards `/v1` to it, and the page
already calls `/v1`. The reference for what answers today
is [../api/lab-api.md](../api/lab-api.md); this directory stays the design
for the rest.

## What this is

A documented HTTP API for the whole lab. A test framework or an agent can
do everything the console can (move the clock, create merchants, read what
happened, reset) and one thing the console cannot yet: **arm traps** inside
a simulation, so a vendor answers a future request the way a test needs.
It works the same when the lab runs on this laptop or somewhere else.

It has two parts:

1. **Each simulation's control surface** (`/sim/*`, on each sim). This is
   the back link. Through it the lab changes state *inside* a vendor:
   arm a trap, reset, read logs. Every sim serves it, whether it runs in
   the lab or alone. See [sim-control.md](sim-control.md) and
   [traps.md](traps.md).
2. **The lab API** (a `lab-api` container serving `/v1/*`). It is one
   address for everything. It owns the lab's own state (merchant registry,
   plugin registrations, health), fans out to the sims' control surfaces,
   and is what the console UI, the runner, tests and agents call. See
   [lab-api.md](lab-api.md).

Neither part changes what a vendor looks like to the platform. Each
simulation stays a 1:1 drop-in for the real service
([architecture.md §3](architecture.md#3-the-drop-in-rule)).

## Tags

| Tag | Means |
| --- | --- |
| **[DECIDED]** | the owner said so; the date is beside it |
| **[PROPOSED]** | drafted here, not confirmed |
| **[OPEN]** | undecided; listed in [open.md](open.md) |

A section's tag covers everything under it unless a line says otherwise.

## Decided so far

All 2026-10-20.

- **HTTP everywhere.** The lab API is JSON over HTTP. So is the back link
  from the lab API into each sim. gRPC was considered and not taken. The
  one place another protocol was thought about is the back link, for
  example pushing a clock change to every sim; it stays HTTP
  ([architecture.md §5](architecture.md#5-the-back-link)).
- **Vendor traffic is not touched.** Every sim stays deployable on its own,
  as a 1:1 drop-in for the real vendor when an application is pointed at
  it. The lab API adds power to change state inside a sim. It does not
  front, proxy or replace the vendor surface.
- **One long-running lab, one tenant**, aimed at local development.
  Parallel, isolated environments (each with its own copy of the platform
  and its settle DB) are in the plan and deferred
  ([plan.md, phase 5](plan.md#phase-5--parallel-environments-deferred)).
- **Lab-level operations for now: read logs, reset.** No container control:
  the lab API holds no Docker/Podman socket.
- **Traps.** A sim can be told in advance how to treat a future event that
  matches. The owner's example: at Banking Circle, a payout whose receiving
  vIBAN is X ends `OutgoingPaymentRejected`, or ends
  `OutgoingPaymentProcessed` and then `Reversed` ten seconds later. The
  model is in [traps.md](traps.md).

## Files

| File | What it covers |
| --- | --- |
| [architecture.md](architecture.md) | containers and ports, the three kinds of traffic, the drop-in rule, the control listener, auth |
| [sim-control.md](sim-control.md) | the control contract every simulation serves: info, activity, logs, reset, traps |
| [traps.md](traps.md) | the trap model, and each vendor's hooks |
| [lab-api.md](lab-api.md) | the `/v1` resources, and how today's console `/api` maps onto them |
| [runner.md](runner.md) | what changes in `fintechlab-runner` and in the plugin contract |
| [plan.md](plan.md) | phases, including the deferred parallel environments |
| [open.md](open.md) | every [OPEN] question in one list |

Sections are numbered so a review can say "traps §4.1" and mean one place.
