# Vision

Build a **local, honest simulation of the third parties** a payment
facilitator settles through — an acquirer that produces settlement files,
a payout rail with regulatory gates, and a settlement bank that notifies by
webhook — so a platform can be built and tested against them without a
vendor contract.

Around them sit the shapes you meet in any bank-grade payment stack: a
ledger, an idempotent payment API, signed webhooks, TLS from a private CA,
and an allowlisted destination.

The goal is not theatre. A payment that is "accepted" but never delivered to a receiver is a disconnected ping. This lab treats **payment → ledger move → enqueue → HMAC POST → stored event** as one path.

Use it to:

- Practice the *shape* of B4B-style `POST /payments` + `Idempotency-Key` without a vendor contract.
- See why webhook retries exist, and why 2xx is the only success.
- See why destinations are allowlisted in code, not in a comment.
- See a daily settlement file cut by the acquirer, delivered over real
  SFTP, and turned into a per-outlet payout — three vendors, one
  connected path, no vendor contract for any of them.
- See why a confirmation file must not be reprocessed, why webhook
  notifications arrive batched, and what happens when a subscriber stops
  answering for two days (compressed into four seconds).
- Keep secrets fake (`sim-hmac-dev-only`) and IBANs obviously fake (`GB00SIM…`).
- Drive all of it from a [control panel](console.md) — create a merchant
  and its outlets, put them on the payout rail, seed trading at the
  acquirer, cut the file and pay it out — without that UI ever becoming the
  only way to reach a state.

**The repo is the vendors, not the platform.** `settlement` and `receiver`
are stand-ins for your side, kept only so the e2e suite can prove each vendor
hop is really connected — see
[catalogue.md](catalogue.md#which-half-is-which).
Your real platform is not in this repo at all: it registers its own card in
the console and follows the lab's clock — see [plugins.md](plugins.md).

Every API the lab answers on — each vendor's surface, the lab-only hooks,
the console's own `/api` — who calls which, and each route in full:
[api/README.md](api/README.md).

Do **not** use it to reconstruct a vendor's private API or to store real bank data.

The console's look and feel is a design record of its own:
[design-system.md](design-system.md), written so another operator app in
this product family comes out looking and behaving the same.


