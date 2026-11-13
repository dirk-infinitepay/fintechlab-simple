# Running the lab as one podman pod

`make up` builds the lab from a checkout and runs it under compose. That
stays the way to work *on* the lab. This is the other way: run it from
images, as a single podman pod, on a machine that has never seen the
source — a colleague's laptop, or a server the platform points at.

```
        podman pod "fintechlab"  (one network namespace)
  ┌────────────────────────────────────────────────────────────┐
  │ init (ca image): runs first, lays out volumes, issues PKI  │
  │                                                            │
  │ bank  notifier  payment-api  receiver  settlement          │
  │ worldline  banking-circle  b4b  aci  verify  verification  │
  │ console                                                    │
  └────────────────────────────────────────────────────────────┘
   published: 8080-8094, 8095, 8443, 2222 (the same as compose)
   state: six named volumes (see "What persists")
```

## From a checkout

```sh
make pod-images   # build all 13 images, tagged localhost/fintechlab/<service>:v1
make pod-up       # podman kube play deploy/pod/fintechlab.yaml
make pod-down     # stop and remove the pod; volumes stay
```

It publishes the same host ports as compose, so run one or the other, not
both.

For a lab a *deployed* platform talks to — no lab clock, its public address
on the certificates and in the `.env`, a host on AWS or anywhere — see
[deploy-cloud.md](deploy-cloud.md). It is this pod, rendered for the place
it runs.

## On a machine with no checkout

```sh
make pod-bundle   # → dist/fintechlab-v1-images.tar + dist/fintechlab-v1.yaml
```

Copy both files across, then:

```sh
podman load -i fintechlab-v1-images.tar
podman kube play --replace fintechlab-v1.yaml
```

Or push the images to a registry and pull them from there:
`make pod-images IMAGE_PREFIX=ghcr.io/you/fintechlab LAB_VERSION=v1`, push
each, and `make pod-bundle` with the same two variables writes a manifest
naming them. The checked-in manifest always says
`localhost/fintechlab/<service>:v1`; make rewrites that on the way out, so
the file in the repo stays one you can play as it is.

## Connecting the platform

Open the console (`http://<host>:8090`), then **Configuration → Connect
your platform**. It downloads one `.env` for every vendor —
addresses, lab credentials, and the keys inlined in the platform's own
variable names — or one vendor's from that vendor's own card, where its key
and certificate files are also offered one by one. Addresses in the file
use the host you opened the console on; `?host=` on the link picks another.

Nothing about the platform needs a path into the lab's filesystem any more:
that was the reason a checkout used to be required at all.

## What persists

The pod keeps state in named volumes, created on the first `play` and
kept by `podman kube down`:

| Volume | Mounted at | Holds |
| --- | --- | --- |
| `fintechlab-keys` | `/keys` | the PKI and generated keypairs — `keys/` in a checkout |
| `fintechlab-sftp` | `/sftp` | the SFT channel tree |
| `fintechlab-settlement-data` | `/data` | settlement's state and archived files |
| `fintechlab-b4b-data` | `/b4b-data` | everything B4B has been told |
| `fintechlab-bc-data` | `/bc-data` | everything Banking Circle has been told: balances, payouts, subscriptions, notifications not yet delivered, tokens |
| `fintechlab-console-data` | `/console-data` | the merchant registry |
| `fintechlab-front-data` | `/data` in `front` | a cloud lab with a name only: the HTTPS front's Let's Encrypt account and certificates ([deploy-cloud.md](deploy-cloud.md#the-console-on-https)) |

**The keys survive every play.** A redeploy (`make pod-up` again, or
`kube play --replace`), a pod restart and a reboot all keep
`fintechlab-keys`, so a platform configured once stays configured.

## New keys

Pass one extra file to the play:

```sh
podman kube play --replace --configmap regenerate-keys.yaml fintechlab-v1.yaml
make pod-up REGENERATE_KEYS=true      # the same, from a checkout
```

`regenerate-keys.yaml` (in `deploy/pod/`, and copied into `dist/` by
`make pod-bundle`) sets `REGENERATE_KEYS=true` for the init container, which
empties the keys volume before anything starts: a new CA and certificates,
and new B4B and Worldline keypairs as those services start. Without the file
the variable is unset and the keys are kept — that is the default, and it
needs no edit to the manifest.

It applies to that one play. Podman runs the init container once per play,
not on a pod restart, so the new keys are not quietly replaced again the
next time the machine reboots. Every copy of the old keys a platform holds
stops working: download its `.env` again.

## Asking for a token

The lab API is open by default. Give the play a token and it asks every
caller but `GET /health` for `Authorization: Bearer <token>`:

```sh
make pod-up LAB_API_TOKEN=$(openssl rand -hex 32)   # from a checkout
podman kube play --replace --configmap lab-api-token.yaml fintechlab-v1.yaml
```

`lab-api-token.yaml` (in `deploy/pod/`, and copied into `dist/` by
`make pod-bundle`) is the ConfigMap the lab API and the console read
`LAB_API_TOKEN` from. It ships empty — an open lab — so put the token in
before playing it. `make pod-up` writes no file: it appends that ConfigMap,
with the token, to the manifest it pipes to the play. A token with a `$` in
it goes through the environment (`LAB_API_TOKEN=… make pod-up`), not the
make command line, where make would expand it.

Like the keys flag, it applies to that play: a play without it runs an open
lab API. Agents, scripts and the e2e suite send the token themselves
(`LAB_API_TOKEN` in their environment). The console adds it to every call
it forwards that brings no token of its own, so its page needs none — which
means `:8090` is as open as ever (a call that brings an environment's token
is forwarded with it, and held to that environment): keep it on a network you trust, or in front of it put an auth proxy
([lab-api/open.md](lab-api/open.md), question 4). A platform registers
through the console and follows the clock directly, so it needs no token.

## How it differs from compose, and why it does not matter

- **One network namespace.** Every container is on the pod's loopback. The
  manifest's `hostAliases` gives `127.0.0.1` every compose service name, so
  `https://banking-circle:8085`, the webhook allowlists, and the
  certificates' SANs all work unchanged — and each container's environment
  is its compose service's, verbatim. `deploy/pod/manifest_test.go` fails
  if the two drift.
- **Volumes, not bind mounts.** The init container (`deploy/pod/init.sh`,
  run from the `ca` image as root) does for them what the Makefile's
  `certs`, `sftp-dirs` and `key-dirs` do for a checkout.
- **Config baked in.** `config/banking-circle.json`, Worldline's fixture
  directory and the SFT channel's seed `merchant-onboarding.json` ship in
  the images. Compose mounts the checkout's copies over them, so editing
  them there still works as it did.

## Remote server notes

- **Certificates.** Banking Circle's and the receiver's certificates name
  `localhost` and `127.0.0.1`. For a platform that dials the server by name,
  set the init container's `CERT_EXTRA_SANS` in the manifest (e.g.
  `DNS:lab.example.test,IP:10.0.0.5`) before the first play, or set it and
  play with `regenerate-keys.yaml` to reissue. See [security/ca-and-tls.md](security/ca-and-tls.md).
- **Links.** The console's "open in a browser" links use
  `CONSOLE_BROWSE_HOST` (127.0.0.1 in the manifest). The `.env` downloads do
  not: they follow the host you opened the console on, unless the lab API's
  `CONSOLE_CONNECT_HOST` names the lab's address (the cloud profile sets it).
- **The clock and the platform's card.** The lab owns its clock (the
  `clock` container, `:8096`); the platform follows it from wherever it
  runs, outbound, so a laptop against a remote lab needs nothing extra. The
  platform registers its own card by calling `:8090` ([plugins.md](plugins.md));
  the card's live parts — health, logs, buttons — need the console to reach
  the platform's `base_url`, which from a remote pod means a tunnel to it
  (cloudflared, ngrok, pinggy). A platform deployed where its clock cannot
  move registers with `"clock": "wall"`, and the console then keeps the lab
  on the real time.
- **Authentication is one token, on the lab API.** With one set
  ([Asking for a token](#asking-for-a-token)), `:8097` refuses a caller
  without it. `:8090` does not: anyone who can reach the console can drive
  the lab and download its keys. Every one of them is fake, which is the
  only reason that is acceptable — keep it on a network you trust.

## The podman quirks

**A bridge podman cannot make.** `make pod-up` failing with
`netavark: create veth pair: Netlink error: Operation not supported` means
the host's `veth` kernel module is not loaded, and rootless podman cannot
load it — common after a kernel update without a reboot. Either load it
once (`sudo modprobe veth`), or play the pod without a bridge:

```sh
make pod-up POD_NETWORK=slirp4netns
podman kube play --replace --network slirp4netns fintechlab-v1.yaml
```

The containers share one network namespace either way, so the lab behaves
the same; only the published ports are forwarded differently.

**Where the play runs.**

`kube play` looks in its working directory for a directory named like each
image, to build it from — and podman 4.9 does so even with `--build=false`.
The repo root has a `b4b` binary and a `worldline` directory, and a play
from there fails on them. `make pod-up` plays from `deploy/pod/`. Playing
the bundled manifest from any other directory is fine.
