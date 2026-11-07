# Lab API: architecture

## 1. What runs

**[PROPOSED]**

```text
          tests · agents · console UI · runner (the platform's plugin)
                                  │
                                  │  HTTP/JSON  /v1/*
                                  ▼
                        ┌────────────────────┐
                        │      lab-api       │ :8097
                        │ merchant registry  │
                        │ plugin registry    │
                        │ health · flow      │
                        └─────────┬──────────┘
                                  │  back link: HTTP/JSON to each sim's
                                  │  control listener, /sim/*
      ┌──────────────┬────────────┼─────────────┬──────────────┬───────────┐
      ▼              ▼            ▼             ▼              ▼           ▼
  worldline    banking-circle    b4b           aci       verify/verif.   clock
  ctl :9084    ctl :9085      ctl :9086     ctl :9087   ctl :9088/:9089   :8096
  ─────────    ─────────      ─────────     ─────────   ─────────
  :8084 :2222  :8085  :8095   :8086         :8087       :8088 :8089
      ▲              ▲            ▲             │
      └──────────────┴────────────┴─────────────┴── platform under test:
                                                    vendor-shaped traffic,
                                                    unchanged
```

| Service | Vendor surface (unchanged) | Control listener | Kind |
| --- | --- | --- | --- |
| `worldline` | `:8084` SFT over HTTP, `:2222` SFTP+PGP | `:9084` | vendor |
| `banking-circle` | `:8085` Connect API (mTLS + Basic→Bearer); `:8095` lab bridge | `:9085` | vendor |
| `b4b` | `:8086` Oversight API (RS512 JWT) | `:9086` | vendor |
| `aci` | `:8087` (sends webhooks) | `:9087` | vendor |
| `verify` | `:8088` | `:9088` | vendor |
| `verification` | `:8089` | `:9089` | vendor |
| `clock` | `:8096` `/clock` | the same `:8096` | lab |
| `bank`, `settlement`, `receiver`, `payment-api`, `notifier` | `:8081`, `:8083`, `:8443`, `:8080`, `:8082` | `:9081`, `:9083`, `:9443`, `:9080`, `:9082` | stand-in |
| `lab-api` | — | `:8097` `/v1/*` | lab |
| `console` | `:8090` the UI | — | lab |

Control ports are the vendor port plus 1000. In the podman pod every
container shares one network namespace, so the ports have to be unique
across the whole lab, and this rule keeps them unique.

**[DECIDED 2026-11-07, built]** The e2e suite (`cmd/e2e`) plays the
platform on the vendor surfaces, with each vendor's login, and steers and
reads the lab through `lab-api` with `LAB_API_TOKEN`, like any agent. It
calls no control port and no `/internal/*`, so one run works against a lab
on a developer's machine or in the cloud. It needs no `console`.

## 2. Three kinds of traffic

**[DECIDED 2026-10-20: all HTTP]**

| Kind | Examples | Changes with this work? |
| --- | --- | --- |
| **Vendor traffic**: what the lab simulates | platform → Banking Circle Connect, B4B Oversight, Worldline SFTP; BC's encrypted notification batches; B4B lifecycle callbacks; ACI webhooks | **No.** It is shaped by the vendors. Changing it would stop the lab testing the real integration |
| **Lab plumbing**: sims standing in for things that have no API | B4B → BC bridge (`:8095/internal/payments`); Worldline's lump sum → BC (`/internal/incoming-payments`); BC → business bank (`/internal/credit`); the `./sftp/outbound` bind mount; the shared `keys/` volume | No, apart from moving BC's *control* hooks off the bridge (§4) |
| **Control**: the lab changing or reading a sim | the clock; activity logs; Banking Circle's outcome queue, return and reverse; B4B sanctions; verification outcomes; the new logs, reset and traps | **Yes.** It moves to one contract on one listener per sim, called by `lab-api` |

## 3. The drop-in rule

**[DECIDED 2026-10-20]**

A simulation is a 1:1 drop-in for the vendor it simulates, and stays one.

1. **The vendor surface is shaped by the vendor alone.** Nothing the lab
   adds appears on it: no extra fields, headers, routes or error shapes,
   and no trace of a trap. A trapped `Reversed` is indistinguishable, over
   the wire, from one the bank decided.
2. **A sim runs alone.** `docker run` one image with its own environment,
   point an application at it, and it works. It needs no `lab-api`, no
   `console` and no `clock` (with no clock it runs on the wall clock, as
   today). Its control surface works alone too: traps, logs and reset are
   on its own control listener, not in `lab-api`.
3. **The lab API is a client of the sims. No sim calls it.** `lab-api` being
   down is never why a scenario cannot run, which is the rule the console
   holds to today ([console.md](../console.md)). It follows that state a
   trap needs lives in the sim that applies it, never in `lab-api`.

## 4. The control listener

**[DECIDED 2026-11-07, built]** Every sim serves its control surface
(`/sim/*`, [sim-control.md](sim-control.md)) on a listener of its own, with
**no authentication**: the vendor's login is for the platform calling the
vendor's API, and the lab steering its simulation (reversing a payout,
reading a log, resetting) must not go through it. The control port is
trusted by where it is — compose does not publish it, so only the lab's
network reaches it — and `SIM_CONTROL_ALLOW` narrows that to the addresses
or CIDRs listed. There is no `SIM_CONTROL_TOKEN`: authentication belongs
between a person, an agent or the console and `lab-api` (§8). The
`lab-api` reaches every sim only here and holds no vendor credentials.

```text
SIM_CONTROL_LISTEN=:9085        # unset: the vendor port + 1000
SIM_CONTROL_ALLOW=10.89.0.0/16  # unset: anything that can reach the port
```

Where `lab-api` used to read or steer a sim through its vendor API, the
sim answers the same on its control port: Banking Circle's
`/sim/accounts`, `/sim/payments`, `/sim/subscriptions`,
`/sim/subscription/{id}/test|activate|deactivate`,
`/sim/accounts/{id}/balances`, `/sim/incoming-payments`; B4B's
`/sim/beneficiaries`; Worldline's `/sim/config`; settlement's `/sim/pull`;
the bank's `/sim/accounts`.

The text below is the original proposal, kept for its reasoning.

Why a separate listener:

- **The vendor port stays pure.** Exposing a sim to a platform somewhere
  else means exposing its vendor port. A separate control port is not
  exposed with it, and nothing on the vendor port can change the sim's
  behaviour.
- **One auth model for control.** Today it is mixed: Banking Circle's
  `/sim/*` sits behind mTLS and a bearer token on `:8085`, its outcome,
  return and reverse hooks are unauthenticated on the `:8095` bridge,
  B4B's `/sim/*` needs an RS512 JWT, and Worldline's and verification's
  need nothing. On the control listener it is the control token or
  nothing, for every sim.
- **The bridge becomes plumbing only.** `:8095` keeps
  `POST /internal/payments`, `POST /internal/incoming-payments` and
  `GET /internal/accounts/{id}/balances`. The control hooks it carries
  today (`/internal/payments/outcomes`, `…/{id}/return`, `…/{id}/reverse`)
  move to `:9085/sim/…`.

Compatibility: with `SIM_CONTROL_LISTEN` unset, nothing moves; that is the
default and what a sim run alone gets. In compose it is set. The old paths
stay as aliases until [phase 4](plan.md#phase-4--retire-the-aliases).
The control ports are not published to the host
([OPEN 2](open.md), decided): from outside the lab's network, `lab-api` is
the one way to them.

## 5. The back link

**[DECIDED 2026-10-20: HTTP]**

`lab-api` → sim is request/response over HTTP/JSON. Nothing travels the
other way: a sim never calls `lab-api` (§3).

Pushing a change to the sims, the clock being the example, also stays HTTP
[PROPOSED]:

- **The clock stays its own container** and is not folded into `lab-api`.
  Every vendor follows it, so folding it in would make every vendor depend
  on `lab-api`, against §3.3. `lab-api` fronts it (`/v1/clock`).
- **Followers keep polling `GET /clock` every second** (today's
  `LAB_CLOCK_URL`). Where a second of lag matters, the clock adds
  `GET /clock?wait=<offset_ms>` as a long poll that answers when the offset
  differs from the one the caller holds. A follower that does not use it
  still works. Nothing is pushed into a sim.
- **Streams to clients** (logs, activity, events) are Server-Sent Events
  from `lab-api` ([lab-api.md §9](lab-api.md#9-streams-sse)). That is HTTP
  too; browsers read it natively and `curl -N` prints it.

## 6. What lab-api owns

**[PROPOSED]**

Everything the console's Go server owns today moves to `lab-api`
unchanged. The console becomes a page.

| State | Today | After |
| --- | --- | --- |
| Merchant registry | `console-data/registry.json` | `lab-api-data/registry.json` |
| Plugin registrations | console memory | `lab-api` memory |
| Stand-in *shown* flags | console memory | `lab-api` memory |
| Health monitor (probes every 5 s) | console | `lab-api` |
| Flow and hop derivation (`/api/flow`) | console | `lab-api` |
| Connect kit (`.env`, key files) | console | `lab-api` |
| B4B signing key, BC client certificate | console (to provision merchants, read BC) | `lab-api` |

The vendors keep all vendor state, including traps (§3.3).

## 7. The console

**[PROPOSED]**

The console becomes a static page with no state of its own. It is served
by a tiny container that also reverse-proxies `/v1/*` to `lab-api`, so the
page calls its own origin and needs no CORS. Its `/api/*` routes stay as a
proxy to the matching `/v1/*` until
[phase 4](plan.md#phase-4--retire-the-aliases), because a running plugin
registers at `{console}/api/plugins`.

Alternative: `lab-api` serves the page itself at `/` and the `console`
container goes away. That is fewer containers, but the API container
then carries UI assets. See [OPEN 3](open.md).

## 8. Auth

**[DECIDED 2026-11-07, built]**

| Surface | Auth |
| --- | --- |
| Vendor surfaces | vendor-shaped (mTLS + Bearer, RS512 JWT, SFTP password/key, API keys), unchanged (§3) |
| Control listeners | none: network placement, optionally `SIM_CONTROL_ALLOW` (§4) |
| `lab-api` | `Authorization: Bearer $LAB_API_TOKEN` when set; open when unset |
| Plugin registration | the same `LAB_API_TOKEN` |
| `console` → `lab-api` | the console adds `LAB_API_TOKEN` to what it forwards |

Unset is the local default, so `make up` stays one command. A lab deployed
somewhere reachable sets it, and `lab-api` and `console` get the same one:
compose takes it from the host (`LAB_API_TOKEN=… make up`), the pod from
an optional ConfigMap at play time (`make pod-up LAB_API_TOKEN=…`, or
`--configmap lab-api-token.yaml`; [deploy-pod.md](../deploy-pod.md#asking-for-a-token)).
`deploy/pod/manifest_test.go` holds the two to that. How a person's browser
gets through to the console of a deployed lab is [OPEN 4](open.md).

## 9. Deploying the lab elsewhere: what this does not solve

**[PROPOSED notes]**

The API makes a remote lab drivable. It does not make every part of the
lab remote-friendly. These remain:

- **Vendor endpoints are still separate ingress.** SFTP on `:2222` is raw
  TCP, and Banking Circle wants mTLS end to end. Neither can sit behind
  `lab-api`, and they should not (§3).
- **Shared bind mounts.** `./sftp/outbound` (settlement ↔ worldline) and
  `keys/` assume one host. That is fine for one pod; it is a problem for
  containers spread across nodes.
- **Reaching the platform.** Plugin buttons are forwarded from `lab-api` to
  the platform's `base_url`, so a remote lab still needs a route back to a
  local platform: a tunnel, as [plugins.md](../plugins.md#reaching-the-platform)
  says today.
