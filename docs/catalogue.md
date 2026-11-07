# Catalogue

## Which half is which

This repo simulates **the third parties**, so a platform can be developed
and tested against them locally. It is not the platform.

That distinction was previously not written down anywhere, and the code had
drifted across it — the platform side was generating the settlement file it
was supposed to be *receiving*. Everything below is labelled, and the rule
is: **a vendor simulation is the deliverable; scaffolding exists only so
the e2e suite can prove each vendor hop is really connected.**

Every simulated service is one of two kinds, and `GET /sim/info` says
which (`kind`):

- **vendor** — a third party's service, simulated: something the platform
  will talk to in production and that someone else runs (Worldline, B4B,
  Banking Circle, ACI, the verification vendors, the merchants' bank).
- **stand-in** — a service the platform team would provide itself, simulated
  because a test does not need the real one or it does not exist yet (the
  settlement path, the webhook listener, the verification-service, the
  demo payment API and notifier).

The rest is the lab itself (`lab`: the clock, the lab API, the console),
not a simulation of anything.

| Service | Side | Swap for the real thing by |
| --- | --- | --- |
| **worldline** | vendor | pointing `WORLDLINE_SFTP_HOST` and the credentials at real Worldline |
| **b4b** | vendor | pointing `B4B_URL` at the real Oversight API |
| **banking-circle** | vendor | pointing at the real Connect API |
| **aci** | vendor | — it is a *sender*; point `ACI_WEBHOOK_TARGET_URL` at your listener |
| **settlement** | stand-in | replacing it with your own platform |
| **receiver** | stand-in | pointing the vendors' webhook URLs at your own listener |
| **bank** (*business bank*) | vendor (the far end) | — the merchants' own bank: Banking Circle credits each processed payout here. Nothing to swap; a platform never calls it |
| **payment-api**, **notifier** | stand-in (demo) | — generic shapes, not vendor-specific; only `make demo-payment` and the e2e suite call them |
| **verification** | vendor | real KYC/AML vendors: Creditsafe, iban.com, KYC6, LexisNexis |
| **verify** | stand-in | your own verification-service: pointing the platform's verification-service URL at it replaces that service |
| **clock** | lab | nothing — the lab's business clock; vendors and a registered platform follow it ([plugins.md](plugins.md#the-clock)) |
| *a registered platform* | platform (plugin) | nothing — it *is* the platform; it registers its own card ([plugins.md](plugins.md)) |
| **ca** | supporting | a real PKI |
| **lab-api** | lab | nothing — the one API over the lab (`/v1`), a client of the others; see [api/lab-api.md](api/lab-api.md) |
| **console** | control panel | — a page that calls the lab API; see [console.md](console.md) |

The scaffolding is deliberately kept, and deliberately not grown. It is
what makes `make e2e` able to fail loudly when a hop is disconnected,
and it is a worked example of the consumer side — but it is a stand-in for
your platform, not a model of it.

| Service | What it fakes | What's stubbed | Still vendor-shaped |
| --- | --- | --- | --- |
| **ca** | Private CA + server certs (SAN `receiver`, `banking-circle`, `localhost`, plus `.test` demo domains — see `docs/local-domains.md`) + client cert | No CRL/OCSP, no HSM | Same *job* as a corporate PKI issuing mTLS material |
| **bank** *(business bank)* | The beneficiary's bank: takes each payout Banking Circle passes on (`POST /internal/credit`, by IBAN or account number + sort code) and opens the outlet's account on first sight; also a ledger with balances and transfers for `make demo-payment`. Logs *Payouts credited* and *Accounts and transfers* at `/sim/activity` | No rails, no FX, no reserved funds, memory only | The money's last hop, observable — and the internal core-ledger call a payment API would make |
| **payment-api** | `POST /payments` + `Idempotency-Key`, `GET /payments/{id}` | No authN/Z, no cut-off, no batch files | Generic payment facade (not B4B-shaped — see B4B below) |
| **notifier** | Signed webhook POST, retries, allowlist, failed subscription | No signed-cert pinning, no multi-tenant routing | PSP webhook worker |
| **receiver** *(scaffolding)* | HTTPS endpoint that verifies HMAC and stores events, plus a raw unvalidated capture sink (`/raw-events`) for wire formats it holds no key for (Banking Circle's AES-GCM, ACI's). Bodies are retained (bounded, base64) so a holder of the key can decrypt a capture and assert on what was delivered | No replay store beyond one process, no business handler, no keys of its own | Stand-in for your own webhook listener |
| **settlement** *(scaffolding)* | The platform side, as a stand-in: pulls Worldline's settlement file over real SFTP+PGP, decrypts and parses it, splits it per MID, and pays each outlet out through B4B — gated on the safeguarding-account balance and a merchant-verification check. Also its own internal reports and reconciliation API | No real clearing, no fee schedules, no FX. Not a model of any real platform | The consumer half of the causal chain, exercised over the wire rather than through a shared directory |
| **worldline** | The acquirer: holds the card transactions it acquired per submerchant (MID), cuts the daily Bambora settlement file from them in two slots (morning `ER` + afternoon `AR` confirmation), publishes them PGP-encrypted over a real SSH/SFTP server (:2222), wires the matching lump sum to the safeguarding account, and serves the SFT file-exchange channel over HTTP (:8084) | No Acquiring REST API (SFTP is the whole integration today). No chargebacks — the `CB` section is emitted and always empty, because there is no dispute source to invent one from | Real Bambora multi-section file format, the real filename pattern and its two delivery slots, real SFTP+PGP transport with key or password auth and host-key pinning |
| **banking-circle** | Basic→Bearer auth behind optional mTLS (`BC_MTLS`), a safeguarding-account ledger credited by the acquirer's lump sum, per-merchant auto-vivified creditor accounts, the full notification self-service API (subscriptions with `rowVersion`/`If-Match`, per-subscription encryption keys, `maxNotificationsPerMessage`, events with Account/Company/CompanyGroup targets), **batched** AES-256-GCM notifications routed by subscribed event and target, and the documented eleven-step retry schedule ending in auto-deactivation, retained notifications and redelivery on reactivation. The reconciliation reads: the intraday reconciliation report (paged, six required parameters, `PropertiesIncluded`/`PropertiesExcluded`), the rejection report, and the single-payment status. Bookings are dated on the bank's business day (CET, 19:00 cutoff, weekends to Monday) on the platform's clock. Lab-only hooks to reverse or return a processed payout, force the next payouts to reject or hang, and pause a subscription's delivery | No real credential store (any Basic auth succeeds), no FX, no bulk payments, no bank holidays, no direct debits | Banking Circle's auth/balance/notification/SGA contract, its subscription DTO and concurrency rules, its retry schedule, its exact webhook cipher and per-event payload shapes (from its payload examples), and its report and status contracts including the 400 body (no payment-creation endpoint — real Banking Circle has none) |
| **b4b** | B4B Payments: JWT(RS512)-authed Oversight API, both halves. **Boarding** — companies (422 on a repeated `external_ref`), addresses, people (natural and legal, screened on a callback carrying sanctions *and* PEP status separately), document uploads, the create-once extended profile, vIBANs. **Payout** — beneficiary registration with `pass`/`review`/`fail` sanctions and active/disabled status on a status-change callback, four 422 gates (sanctions, disabled, creditor-consistency, optionally paying company, all forwarding nothing), payout lifecycle (`B4BAccepted`→…→`B4BTMApproved`/`B4BFailed`) with a callback on every regulatory-phase change, `GET /payments/{id}` for a missed callback, and the bridge into Banking Circle once approved. All of it persisted to `b4b-data/state.json`, with an interrupted payment resuming after a restart | No real sanctions/PEP/KYB screening — everything boarded passes, on a delay, with lab-only `PUT /sim/...` endpoints to move any status and a configured force-fail list for payments. Documents are accepted, measured and **discarded**, never stored and never judged. No batch payments | B4B Oversight API shape end to end (byte-exact wire types: JSON-number amounts, `country` refs, `chargeBearer` `SHA`/`BEN`/`OUR`), the boarding rules a client actually trips over, its gates and its callback semantics — this lab's only real inbound-payment caller of Banking Circle |
| **aci** | Online card-payment gateway: AES-256-GCM encrypted webhook notification (hex IV/tag/body, distinct key encoding from Banking Circle's), simulating "a card payment just happened" | No merchant-onboarding REST client, no SFTP+PGP file-exchange channel (both real but separate ACI surfaces, out of scope) | ACI's real webhook crypto envelope and notification payload shape (known-answer-tested against ACI's own published reference vector) |
| **verify** | Merchant verification / compliance-check service gating a payout: always-approves by default, `x-internal-api-key` shared-secret auth | No real AML/sanctions/KYC vendor calls (explicitly a sample/stub, per product decision) | verification-service's real trigger-then-read-decision contract and decision-enum values |
| **lab-api** *(lab)* | Nothing. It holds no vendor contract at all: it reads and drives the services above over exactly the public surfaces a human with `curl` would use, passes their `/sim/*` hooks through with the credentials each asks for, and owns only the lab's own state (the merchant registry, `console-data/registry.json`, and the plugin registrations) | Not a dependency of anything — the e2e suite does not know it exists, and nothing in this lab is reachable only through it | — an API, not a simulation |
| **console** *(control panel)* | Nothing: a page, served with a forward of `/v1` to the lab API. No state, no credentials | Not a dependency of anything | — a UI, not a simulation |

Nothing in this table is a real clearing system.

## Architectural Documents

| Document | Description |
| --- | --- |
| [Settlement Report Generation](ARCHITECTURE-settlement-report.md) | Daily batch report generation (CSV/XML) per merchant |
| [SFTP Staging](ARCHITECTURE-sftp-staging.md) | File staging directory structure and naming conventions |
| [Reconciliation API](ARCHITECTURE-reconciliation-api.md) | `GET /reports/settlement` query endpoint with date range and format options |
| [Settlement State Machine](ARCHITECTURE-settlement-state-machine.md) | State lifecycle: Scheduled → Processing → Settled/Failed |
| [Worldline SFTP Channel](ARCHITECTURE-worldline-sftp-channel.md) | SFTP channel for onboarding docs and settlement file delivery |
| [Worldline as Acquirer](ARCHITECTURE-worldline-acquirer.md) | Who owns the settlement file, the two delivery slots, one file per currency, and swapping in the real host |
| [Banking Circle Webhooks](ARCHITECTURE-banking-circle-webhooks.md) | Subscriptions, `If-Match`, batching, the eleven-step retry schedule and making two days watchable |
| [The console](console.md) | The control panel: services, sim banks, the merchant/outlet registry and where the configuration lives |
| [Banking Circle Payout Rail](ARCHITECTURE-banking-circle.md) | Superseded — historical record only, see the doc itself |
| [Vendor Contract Corrections](ARCHITECTURE-vendor-corrections.md) | Phase 2 (buddy-verified): real Bambora file format, real SFTP+PGP transport, Banking Circle's real auth/balance/notification surface, the real B4B Oversight API |
| [Phase 3 Corrections](ARCHITECTURE-phase3-corrections.md) | Phase 3 (buddy-verified): the safeguarding-account model, ACI's webhook, merchant verification, B4B wire-shape fixes, and how they all wire into the payout gate |
| [B4B Oversight — the boarding half](ARCHITECTURE-b4b-oversight.md) | Companies, people, documents and the extended profile: what is simulated and what deliberately is not, the four refusals worth having, and the wire-shape questions still open with the vendor |

## Kernels and pods

The table above is the *standalone services*. There is now a second,
composable architecture beside them — brand-generic kernels composed into
exactly what has and has not moved.

| Document | Description |
| --- | --- |
| Kernels and pods | The kernel contract, the bus grammar, pod bootstrap, the money-plane/control-plane rule, and what is still in the standalone services |
| [core/](../core/README.md) | A tour of the runtime packages |

## Code structure

Which side of the vendor/platform line a service is on (above) is a
different question from how its Go code is organized on disk — see
[docs/structure/](structure/README.md) for the cmd/+internal split, what
`internal/` actually restricts, and a file-by-file walkthrough of three
services.

| Document | Description |
| --- | --- |
| [Code structure overview](structure/README.md) | One module, eleven binaries: the cmd/+internal split, what `internal/` actually restricts, compile-time graph vs runtime graph |
| [banking-circle](structure/banking-circle.md) | File-by-file: ledger, payment engine, subscriptions, dispatcher, delivery/retry, reconciliation |
| [b4b](structure/b4b.md) | File-by-file: RS512 JWT auth, the boarding chain, beneficiary gates, payout lifecycle, the Banking Circle bridge |
| [worldline](structure/worldline.md) | File-by-file: split across `cmd/worldline` (acquirer) and `cmd/settlement` (consumer), the Bambora file format, the real SFTP+PGP transport |

## End-to-end tests

`cmd/e2e` runs black-box scenarios against the whole chain above:
generic payment-api → ledger → webhook; a Worldline settlement batch
staging both the WX report and the real Bambora file, retrieved through
both the HTTP view and the real SFTP+PGP channel (SSH auth, file transfer,
PGP decryption all genuinely exercised); the real payout rail — settlement
(gated on Banking Circle's safeguarding-account balance, simulating
Worldline's lump sum landing and retrying when the gate first blocks it)
→ B4B (JWT-signed) → B4B's own lifecycle → Banking Circle (mTLS + bearer,
AES-256-GCM notification model) — confirmed via Banking Circle's
authenticated reconciliation and balance endpoints; and ACI's card-gateway
webhook, confirming a genuinely AES-256-GCM-encrypted notification is
built and delivery attempted. It fails loudly if any hop is disconnected.
`make e2e` runs it locally after `make up`; `make e2e-docker` runs
the same binary as one container attached to the compose network. See
`docs/scenarios/`.
