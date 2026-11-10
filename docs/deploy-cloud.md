# Running the lab in the cloud

[deploy-pod.md](deploy-pod.md) runs the lab as one podman pod from images.
This is the same pod on a machine somewhere else, for a platform that is
*deployed* rather than run from a laptop: an environment of your own that
switches its vendor endpoints to the lab by configuration.

```
          your network / VPN                    the platform's network
                │  console :8090, .env                │  its NAT addresses:
                │                                     │  vendors, lab API :8097
                ▼                                     ▼
  ┌──────────────────── the lab's own VPC (one stack) ─────────────────────┐
  │  Elastic IP ── security group: the allowlist (a managed prefix list)   │
  │                                                                        │
  │  EC2 host (SSM only, no SSH)                                           │
  │    podman pod "fintechlab"  — the cloud profile: no lab clock          │
  │      bank notifier payment-api receiver settlement worldline           │
  │      banking-circle b4b aci verify verification lab-api console       │
  │    data volume: podman storage — keys, SFTP tree, state, the token     │
  └────────────────────────────────────────────────────────────────────────┘
          │ webhooks and callbacks, out through the internet gateway
          ▼
     the platform's public webhook endpoints (LAB_WEBHOOK_ALLOW)
```

## Two profiles, one set of images

The images are the ones `make pod-images` builds, unchanged. What differs is
the manifest, and only in a few values, so there is no second manifest:
`deploy/pod/render` derives the deployment's from the checked-in
`deploy/pod/fintechlab.yaml`, and its tests fail if a rendering changes
anything it was not asked to.

| | local (`make up`, `make pod-up`) | cloud (`make cloud-bundle`, `lab.sh deploy`) |
| --- | --- | --- |
| Lab clock | the `clock` container; every vendor follows it | none: every vendor is on the real time |
| `/v1/clock` | the clock; the console's card, controls, *Move it* | `{"absent": true}` and the real time; a move or a hold is a `501` (`CONSOLE_URL_CLOCK=off` on the lab API) |
| Lab API token | none unless you set `LAB_API_TOKEN` | always: made on the host at the first deploy, kept with the lab's state |
| `.env` downloads | addressed to the host the request came in on | addressed to the lab's public host (`CONSOLE_CONNECT_HOST`), even through a tunnel |
| Certificates | `localhost`, `127.0.0.1`, the lab names | the same, plus the public host (`CERT_EXTRA_SANS`) |
| Webhook allowlists | lab names, `host.containers.internal` | the same, plus the platform's hosts (`LAB_WEBHOOK_ALLOW`) |

**Why no clock.** Moving time works when the lab and the platform share a
clock: a local runner follows the lab's offset through its clock shim. A
deployed platform runs on its own wall clock and cannot be moved, so a lab
it talks to must not move either — a payout booked "on Monday" by the
platform has to book on Monday at the bank. Without `LAB_CLOCK_URL` each
vendor dates what it does by its own wall clock, which is what the code has
always done with no clock to follow; nothing is compiled differently. The
lab API's other controls — Worldline cycles, funding, returns and reversals,
traps, pausing Banking Circle's delivery, a reset — work as they do locally.

**Why a token.** Locally the lab API is open, as a laptop's lab usually is.
A lab reached over a network asks every caller of `:8097` for its token
(`LAB_API_TOKEN`, [deploy-pod.md](deploy-pod.md#asking-for-a-token)).
`deploy/host/play.sh` makes one on the first deploy and keeps it on the data
volume, so a redeploy, a stop or a replaced host keeps it; `lab.sh token`
prints it. The console adds it to everything it forwards, so its page needs
none — which is also why the console itself is kept behind
the allowlist ([Reaching it](#reaching-it)).

`make cloud-manifest` prints the rendered manifest; `LAB_PROFILE=local`
keeps the clock, for a lab on another machine that a laptop runner follows.

## The AWS stack

`deploy/aws/fintechlab.yaml` is one CloudFormation template; `deploy/aws/lab.sh`
drives it with nothing but the AWS CLI (no CDK bootstrap, no state backend).

| Resource | Why |
| --- | --- |
| VPC, one public subnet, internet gateway | the lab's own network: nothing in it depends on, peers with, or blocks deleting anyone else's |
| Allowlist and security group | three rules — the vendors' own surfaces and the console (8080-8090), the lab API (8097), SFTP (2222) — open to the addresses on a managed prefix list, the [allowlist](#who-may-reach-it-the-allowlist). Never `:8095` (Banking Circle's bridge, no auth), a control port (a vendor's port plus 1000) or the receiver stand-in's 8443: from outside, the lab API is the one way to steer the lab. An empty list opens nothing |
| EC2 host, Ubuntu 24.04, `t3.small` | SSM only: no key pair, no port 22. IMDSv2 with hop limit 1, so no container can act as the host's role, which can only be managed by SSM anyway |
| Data volume (20 GiB) | podman's storage. A replaced host — a new image, a new type — keeps every key the platform holds |
| Elastic IP | the lab's one stable address; optionally a Route 53 record for `LAB_DOMAIN` |
| Private bucket | how a bundle reaches the host: `lab.sh` uploads it and hands the host a presigned URL, so the host's role has no S3 access |

There is no user data. `deploy/host/play.sh` prepares the host on the first
deploy (data volume, podman, start on boot) and is a no-op after that, so
editing it never replaces the instance. The host image is resolved once, when
the stack is created, and kept: a new Ubuntu image never replaces the lab
host on its own (`LAB_REFRESH_IMAGE=1 lab.sh infra` takes one on purpose).

### Who may reach it: the allowlist

The security group's rules name a managed prefix list (`<stack>-allowed`,
the stack's `AllowListId` output), not addresses. The template creates the
list and never sets its entries, so the addresses on it are people's to
change, and changing them is not drift: CloudFormation checks only the
properties a template sets.

```sh
deploy/aws/lab.sh allow                       # your address, as https://checkip.amazonaws.com sees it
deploy/aws/lab.sh allow 203.0.113.7 "QA laptop"   # someone else's, with a note
deploy/aws/lab.sh deny 203.0.113.7/32
deploy/aws/lab.sh allowed                     # every entry: who added it, when, why
```

An entry records who added it and when, so the next person reading the list
can tell what may go. In effect within seconds; nothing is redeployed. The
VPC console does the same (*Managed prefix lists* → the list → *Modify*).

- **Who may edit it.** Anyone whose role can call
  `ec2:ModifyManagedPrefixList` on it — an account administrator, or a role
  given the stack's `AllowListEditorsPolicyArn` output: a policy for this
  one list and nothing else.
- **Twenty entries**, at most. Each rule that names the list costs its size
  against the security group's 60 rules, and there are three.
- **Nothing broader than /16.** `lab.sh` refuses it: the console asks for
  no login, so the list is all that keeps it private.
- **`LAB_ALLOWED_CIDRS`** in `lab.env` seeds the list at `lab.sh infra`:
  missing entries are added, nothing is ever removed. `infra` also puts back
  any entry an update of the stack might drop.

### Sharing an account

The stack is built to sit in an account that holds other workloads:

- **It references nothing.** No imported values, no other stack's outputs,
  no shared bucket, VPC, zone or role. Deleting it, or anything else, never
  waits on the other.
- **It names nothing.** Every physical name is CloudFormation's, prefixed
  with the stack's, so it cannot collide with a name another tool chose.
- **It changes nothing account-wide.** No CDK bootstrap, no account
  settings, no service-linked configuration.
- **It is tagged** `Project=fintechlab`, `ManagedBy=fintechlab-simple`.
- **`LAB_ACCOUNT_ID`** makes `lab.sh` refuse any other account, so a shell
  pointed at the wrong profile fails before it changes anything.

What it does share is the region's quotas: one VPC and one Elastic IP
(the defaults are five of each).

## First deploy

```sh
cp deploy/aws/lab.env.example deploy/aws/lab.env   # then fill it in
make aws-infra     # the stack: a few minutes
make aws-deploy    # build, upload, play; a new host installs podman first
make aws-e2e       # the e2e suite, inside the pod: is the lab connected?
make aws-env       # the platform's .env, addressed to the lab
make aws-token     # the lab API's token
```

`lab.env` holds the account, the stack's name, who may reach the lab and
the platform's webhook hosts — each variable is described in
[lab.env.example](../deploy/aws/lab.env.example). Everything set in the
environment wins over the file.

`aws-deploy` builds `dist/fintechlab-cloud.tgz` (the images, the e2e
suite's, the rendered manifest, `play.sh`, `e2e.sh`), uploads it, and runs
`play.sh` on the host over SSM, printing what it printed. The pod is up when
it says so: the lab API and the console answer by then, and the lab API has
refused a call without its token.

`aws-e2e` is the check that the deploy is a lab and not a set of
containers: it runs `cmd/e2e` on the host, inside the pod, playing the
platform on every vendor's surface and steering the lab through the lab
API with its token. Inside, because the suite registers callbacks the lab
must reach (B4B's, on the settlement stand-in) — the same reason
`make e2e-docker` joins the compose network. `E2E_ONLY=…` narrows it.

## Reaching it

**The console asks for nothing.** It adds the lab API's token to what it
forwards, so whoever reaches `:8090` can drive the lab, download its keys
and register a plugin. The lab API on `:8097` asks for the token. Every key
is fake, but never open either to `0.0.0.0/0`. Three ways in, from least to
most exposed:

1. **SSM port forwarding** — `make aws-console` puts it on
   `http://127.0.0.1:8090` for as long as the command runs. Needs the
   [Session Manager plugin](https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html)
   and nothing else: it works with nobody on the allowlist.
2. **An address on the allowlist** — `deploy/aws/lab.sh allow` adds yours,
   and the console is `http://<PublicHost>:8090`.
3. **A name** — `LAB_DOMAIN` (with `LAB_HOSTED_ZONE_ID` the stack writes
   the record). The link is `http://<LAB_DOMAIN>:8090`; the certificates the
   platform dials carry the name. Set it before the first deploy, or
   redeploy with new keys.

A link that anyone in the company can open — HTTPS on 443, behind the
company's sign-in — is a load balancer with an OIDC action in front of
`:8090`. It needs a domain and a certificate, so it is not part of this
stack yet; the console needs no change for it. The vendors' ports stay
direct either way: Banking Circle's mTLS ends at the lab, so anything in
front of `:8085` has to pass TCP through.

## Connecting a platform

**Give a deployed platform an environment of its own** when the lab serves
anything else too: a runner on someone's laptop, an e2e run, a second
stack ([lab-api/environments.md](lab-api/environments.md)). Create it once,
with the admin token:

```sh
T=$(deploy/aws/lab.sh token)
curl -s -H "Authorization: Bearer $T" http://<PublicHost>:8097/v1/environments \
  -d '{"name": "sbx", "config": {"settlement_identifier": "Infinite_Payment",
       "webhook_hosts": ["<the platform's callback ALB hosts>"]}}'
curl -OJ -H "Authorization: Bearer $T" "http://<PublicHost>:8097/v1/connect/env?env=sbx"
```

`fintechlab-sbx.env` is that platform's kit: its own Banking Circle login
and safeguarding account ids, its own B4B signing key, its own SFTP user,
and its own lab token (`FINTECH_SIM_LAB_TOKEN`), which reaches only its
data. Its settlement identifier and callback hosts are its environment's
config, not `LAB_SET` or `LAB_WEBHOOK_ALLOW`, which stay for the default
environment. The console's *Environments* view does the same, and its
selector shows each one's payouts, files and activity apart. A platform on
the default environment needs none of this, and everything below still
applies to it.

- **Addresses and keys.** `make aws-env` (or *Configuration → Connect your
  platform*) downloads one `.env` with every vendor's address, lab
  credentials and keys, addressed to the lab's public host. The certificates
  name that host, so Banking Circle's mTLS verifies against the lab CA in the
  file. From where the console is not reachable, `aws-env` fetches the same
  file over SSM.
- **The lab's address and token.** A platform that registers its card,
  follows the lab or calls its hooks does it on the lab API:
  `FINTECH_SIM_LAB_URL=http://<PublicHost>:8097` (the stack's `LabApiUrl`
  output) and `FINTECH_SIM_LAB_TOKEN` from `make aws-token`
  ([plugins.md](plugins.md#reaching-the-platform)).
- **Its outbound addresses.** The platform reaches the lab from its NAT
  gateways' Elastic IPs: `deploy/aws/lab.sh allow <ip> "platform NAT"` for each.
- **Its inbound endpoints.** Banking Circle's webhooks and B4B's callbacks
  go to URLs the platform registers; their hosts go in `LAB_WEBHOOK_ALLOW`.
  A target the lab dials from its own configuration (ACI's webhook) is a
  `LAB_SET` override, e.g.
  `LAB_SET="aci.ACI_WEBHOOK_TARGET_URL=https://hooks.example.com/aci"`.
  Outbound, the lab trusts the public CAs as well as its own
  ([security/ca-and-tls.md](security/ca-and-tls.md)). Then `make aws-deploy`.
- **Its card, if it wants one.** An environment turns green in the console
  once a vendor has seen its platform log in, with no card at all. A
  platform that brings buttons registers at `/v1/plugins` with
  `"clock": "wall"` ([plugins.md](plugins.md)), with its environment's
  token. The card's live parts need the lab API to reach the platform's
  `base_url`, which has to be a public address of the platform's.
- **Banking Circle forgets.** Its subscriptions and payments are in memory:
  after a redeploy or a stop, the platform subscribes again when it
  restarts. The environments do not forget: they are in the lab API's
  database, on the data volume, and every vendor gets its customers back
  within seconds of starting.

## A runner on your machine

The cloud profile is for a deployed platform. A platform run from a laptop
— a local runner that follows the lab's clock — can drive the cloud lab
too, as it drives a local one:

1. **The lab, with its clock.** `LAB_PROFILE=local` in `lab.env`, then
   `make aws-deploy`. Moving time works again; set it back to the cloud
   profile (and redeploy) before a deployed platform points at the lab.
2. **Its callbacks.** The lab calls the platform back — Banking Circle's
   webhooks, B4B's callbacks, the plugin card's buttons — and a laptop has
   no address the lab can reach. `deploy/aws/lab.sh tunnel 3000 3114 3109`
   (the platform's ports; leave it running) makes each one
   `host.containers.internal:<port+10000>` inside the lab, over SSH within
   an SSM session: no port opened, no third party. Those are the addresses
   the platform registers, and the lab's webhook allowlists already accept
   that host.
3. **Its configuration.** Every vendor address becomes the lab's public
   host; the keys come from the lab's *Connect your platform* files (the
   `.env` from `make aws-env`, or `/v1/connect/files/…` into a keys
   directory laid out like `keys/`); the lab itself is
   `FINTECH_SIM_LAB_URL=http://<PublicHost>:8097` with the token from
   `make aws-token`. Nothing reaches Banking Circle's bridge (`:8095`): the
   platform asks its own Banking Circle app, as it does deployed.

The platform's own callback addresses become
`http://host.containers.internal:<port+10000>/…`; its card's `base_url`
the same for its control port.

**A callback through the tunnel arrives from loopback.** ssh hands it to
`localhost`, so a platform that checks where a callback comes from (an IP
allowlist on B4B's callback, say) sees `::1` or `127.0.0.1`, not the lab's
address, and has to allow those for as long as it runs on the tunnel.

## Day to day

| | |
| --- | --- |
| `make aws-deploy` | a new build onto the same host. The keys survive |
| `REGENERATE_KEYS=true deploy/aws/lab.sh deploy` | new PKI and keypairs; every copy the platform holds stops working |
| `ROTATE_TOKEN=true deploy/aws/lab.sh deploy` | a new lab API token; every caller holding the old one is refused |
| `make aws-e2e` | the e2e suite inside the pod; `E2E_ONLY=a,b deploy/aws/lab.sh e2e` for some |
| `make aws-token` | the lab API's token |
| `deploy/aws/lab.sh tunnel <port>…` | this machine's ports, reachable from the lab as `host.containers.internal:<port+10000>`, until Ctrl-C |
| `make aws-allow` / `make aws-allowed` | add your address to the allowlist / list it; `deploy/aws/lab.sh deny <cidr>` takes one off |
| `make aws-status` | the stack's outputs, the host, the revision it runs, each container |
| `deploy/aws/lab.sh logs banking-circle` | the last 200 lines (`LAB_TAIL`) of one container |
| `make aws-shell` | a shell on the host: `sudo podman ps --pod` |
| `make aws-stop` / `make aws-start` | a stopped host costs only its disks and address; the pod comes back on start |
| `make aws-destroy` | everything, after you type the stack's name |

**What persists:** the data volume holds podman's storage — the five named
volumes ([deploy-pod.md](deploy-pod.md#what-persists)) and the images — and
the lab API's token. It
survives a redeploy, a stop and a replaced host; it goes with
`aws-destroy`. **What does not:** everything the vendors keep in memory,
as locally.

**Several labs** are several stacks: `LAB_STACK=fintechlab-qa lab.sh infra`.
The pod is single-host by design — its containers find each other on the
pod's loopback — so a lab is one host.

**Rough cost** (eu-west-1, on-demand): the host about $15 a month, the
address $3.60, the disks $3. Stopped, only the last two.

## Without AWS

Nothing in the bundle knows it is on AWS. On any Linux host with `apt` or
`dnf` and systemd:

```sh
make cloud-bundle LAB_PUBLIC_HOST=lab.example.com LAB_WEBHOOK_ALLOW=hooks.example.com
scp dist/fintechlab-cloud.tgz host:
ssh host 'mkdir -p lab && tar -xzf fintechlab-cloud.tgz -C lab && sudo sh lab/play.sh'
```

`play.sh` installs podman if it is missing, loads the images, plays the
pod with a lab API token (`/var/lib/fintechlab/lab-api-token`) and has it
start on boot; `sudo sh lab/e2e.sh` runs the e2e suite inside it. A host
whose podman cannot make a bridge plays with `POD_NETWORK=slirp4netns`
([deploy-pod.md](deploy-pod.md#the-podman-quirks)). Open the lab's ports —
8080-8090, 8097, 2222 — to whoever needs them with that host's own
firewall, and never a control port.
