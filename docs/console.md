# The console

`http://127.0.0.1:8090` after `make up`, or `make console`.

A control panel for the lab — the thing that turns a set of containers and
a page of `curl` invocations into something you can hand to someone who
has not read the repo. Views: **Dashboard** (the home page — the lab at a
glance, then one run as a sequence diagram), **Vendors**, **Platform**,
**Verification**, **Banks**, **Merchants**, **Docs** (the guide and the
[API reference](#docs); how the platform, its
test harness, the runner library and this lab fit, and how to set them up),
**Configuration**.

## What it is, and what it deliberately is not

The console is **a page, and a client of the lab API like any other**.
Every button is exactly one call to the lab API (`cmd/labapi`, `:8097`,
`/v1`) — the same call a script or an agent makes, and the lab API's
route is in turn one call to a service's own public API, the same call a
human with `curl` would make. The console's server (`cmd/console`) serves
the page and forwards `/v1` to the lab API, so the page calls its own
origin; it keeps no state and holds no credential.

That constraint is the whole design, and it is worth stating plainly:

- **Nothing in this lab is reachable only through the console.** Every
  button is a lab API route ([api/lab-api.md](api/lab-api.md)), and if a
  state could only be produced by clicking, that state would be untestable:
  the harness — which is what actually judges this repo — could never
  assert on it.
- **The console being down is never why a scenario cannot run.** It is not
  a dependency of anything. Neither is the lab API: `make harness` knows
  neither exists.
- **It holds no vendor contract.** Nothing real-shaped lives here. The
  contracts live in `worldline`, `b4b`, `banking-circle` and `aci`, which
  is where you would go to check them against a vendor's documentation.

The lab's own state — the merchant registry in
`console-data/registry.json`, a plain file you can read, diff and delete,
and the plugin registrations — is the lab API's.

## Dashboard

The home view, and the one to open first. It answers four questions
without a click, in a row of **widgets** — each a link to the card where
its detail and controls live:

| Widget | Says | Goes to |
| --- | --- | --- |
| Lab clock | the lab's time in UTC, business day or not, shifted or not, held or not | Platform → Lab clock |
| Platform under test | the registered plugin, how its registration stands, and its primary button (the same call as on its card) | its card |
| Services | how many are up, a dot per service, which are down, how many stand-ins are hidden | each dot to its card |
| Recent runs | the settling plugin's runs log: total, a bar per recent run, clean vs. with failed stages, the last one | its card, runs log open |
| Quick links | connect your platform, download `fintechlab.env`, cut a Worldline file, BC subscriptions, merchants, the platform's own page | |

Links are `#view/card` — `#vendors/b4b` opens the B4B card,
`#vendors/b4b:payments` its payments log too — so any card can be linked
to. The sidebar folds to an icon rail — the two-line button beside the
title, or the logo itself — and stays as you left it (remembered per
browser).

Under the widgets, a **sequence diagram of one settlement run**. The platform sits in the middle, the vendors it talks to
either side, and each hop between them is an arrow that lights as its own
traffic arrives — the SFTP pull, the payouts into B4B, B4B's bridge into
Banking Circle, the lifecycle callbacks coming back, the notification
batches going out, and the confirmations coming back in.

**The last arrow closes the loop, and it is the one worth watching.**
Banking Circle delivers the same encrypted batches to two places: the lab's
own receiver stub, and whatever the system under test subscribed. Only the
second is evidence that the platform heard anything, so they are two
arrows, split by the endpoint Banking Circle recorded on each delivery. A
green **notification batches** with a grey **payout confirmations** under
it is a complete picture of a real failure: the bank sent it, the lab took
it, and nothing of yours did. A batch whose endpoint cannot be read is
counted on the lab's side — the confirmations arrow claims your listener was
called, and that claim is never a guess.

The lab clock heads the diagram — UTC, Paris, Stockholm and London,
shifted or not, business day or not (amber when it is not, naming the
calendar), held or not — with a link to move it. The platform is whichever
registered plugin settles ([plugins.md](plugins.md)); with none registered
the diagram still draws the vendors' side. And it is drawn as what it is, the system under test:
a wider box with a thicker border and a purple wash, and a solid purple
lifeline where the vendors' are dashed grey.

**The bottom arrow is the reconciliation sweep**: *intraday reconciliation*,
from Banking Circle to the platform, lights when the platform reads the
intraday report — about an hour after a run, or when the lab clock is
moved forward and the sweep triggered. It counts intraday report reads only;
the status fallback and the rejection report are in the Banking Circle
card's *Reconciliation reads* panel. A refused read shows amber.

It is drawn by hand in SVG rather than with a diagram library, for the
reasons in [design-system.md](design-system.md#sequence-diagram): no CDN is
allowed here, and a library that re-renders from a text description cannot
animate one arrow while the others hold still.

**What the colours mean** (they are the design system's, and they are all
data): grey is *nothing has come this way*, green *is happening now* —
one message or a whole batch — dark green *done and fine*, amber *partly
refused*, red *broke*. Everything at rest is grey so that the one
thing that just moved is the one thing you see. A state is held for at
least a second with a glow that ramps up and back down, because a hop that
takes three milliseconds is still worth looking at.

Boxes carry the participant's own title, role, health dot and two counters,
with a `+N` for what this run added. A **red box means that service is
unreachable** — not that something that touched it went wrong; the arrows
carry those verdicts.

At the bottom, **Run report** expands into what the run did: files
collected, stages completed, merchant payouts and the amount settled,
payouts at the bank, callbacks delivered, notification batches,
confirmations to the platform, and **payouts confirmed** as "2 of 6" — the
bank's verdict as the platform's own books record it, reported even when it
is zero, because zero is the number somebody is looking for. Fees are
deliberately absent — they are computed inside the platform's own workers
and never leave them, and the panel says so rather than inventing a figure.

The data is `GET /v1/flow`, which reads the vendors' activity rings and the
platform's stage chain (its plugin's `status_path`) and puts them in the shape a sequence needs. The server
counts; the browser remembers what the count was a second ago and lights
what changed.

### Watching one hop

**Click an arrow** and the traffic behind it opens in a drawer beside the
diagram: exactly the events that arrow counted, newest first, refreshed
every second while the drawer is open, with anything that arrived since you
opened it marked. The payouts arrow lists B4B's `payment.create` calls; the
confirmations arrow lists only the batches Banking Circle delivered to
*your* subscriber, not the receiver's. The two arrows the platform draws to
itself list its stages instead, in the order it declared them, with how
each stands. The drawer links to the card whose log it reads. Click another
arrow to switch, `Escape` or `×` to close; `#dashboard/payouts` links
straight to one. Clicking a box goes to that service's card.

The drawer's data is `GET /v1/flow/hops/{id}` — the same step
`/v1/flow` draws, computed by the same code, with its events attached, so
an arrow saying 6 and a drawer listing 5 cannot happen.

## Vendors, Platform, Verification

Three views over the same catalogue, one per kind — the distinction
[catalogue.md](catalogue.md#which-half-is-which) draws, promoted from
headings on one page to the navigation itself. They used to be three
sections under a nav item called *Standalone*, a name left over from an
architecture this tree no longer has.

- **Vendors** — the third parties the lab simulates. The deliverable.
- **Platform** — your own stack: the platform under test, which registers
  its own card here and is listed first because it is the thing under test,
  then the lab clock, then the stand-ins that exist so a hop can be proved
  connected.
- **Verification** — the checks a merchant passes before any money exists.
  Their own view rather than filed under vendors, because they are what an
  operator goes and flips an outcome on.

Each view carries four tiles: up, down and not reporting **for that
section**, and the whole lab's `up/total` in accent. Splitting the list took
away the one screen that answered *is everything up?*, and that answer is
worth keeping somewhere you always are. The nav badges carry the same
per-section count.

Each row carries a live health state, the last two minutes of probe
outcomes as a sparkline, and the round-trip latency. Expanding one shows
where it is reached, its ports and transport, how it authenticates, its
main endpoints, and — the field that matters most — **how to point it at
the real thing**.

Four states, and the difference between them is deliberate:

| State | Means |
| --- | --- |
| **up** | answered 2xx |
| **down** | refused, timed out, or answered non-2xx — the reason is shown, because a 401 from a credentialed listener is a different problem from a refused connection |
| **unknown** | not probed yet |
| **not probed** | no health endpoint at all (the CA is a one-shot script) |

A service that has not been probed yet is never reported as down. A status
page that cries wolf on start-up is a status page people learn to ignore.

### Activity: what the platform just did

Three vendors keep a short history of the traffic that reached them, and an
expanded card shows it as a panel per conversation:

| Service | Panels |
| --- | --- |
| **B4B Payments** | *Payouts received* — every call to the payments API and what was answered · *Callbacks sent* — each lifecycle callback, and whether the client took it |
| **Banking Circle** | *Payments received* — payouts arriving over the lab bridge, and money landing on the safeguarding accounts · *Notifications sent* — each encrypted batch, its event types, and the endpoint's answer, plus whatever a paused subscription is holding · *Reconciliation reads* — every intraday report, rejection report and payment-status call: when, what was asked (dates, page, properties) and what went back; a refused one amber with the parameters it lacked · *Payouts* — the outgoing payments, with *Return* and *Reverse* on the processed ones |
| **Worldline** | *File exchange* — every SFTP session the platform opened, and the files it listed, collected or delivered |

This exists because the three most common questions during a run —
*did the payout arrive, did the callback get taken, did anyone actually
collect the file* — were each answerable only by tailing a container's
stdout, and one of them (a callback refused at the client's door) is
completely invisible from the settlement side. A payout that was accepted
perfectly and then had every callback rejected looks, from the platform,
exactly like nothing happening.

The panels read from each service's own `GET /sim/activity`, which the
lab API proxies at `/v1/services/{id}/activity`. Nothing is persisted:
it is a ring of the last few hundred events per log, so it is a window onto
a run and never a record of one. Refusals are amber and failures red;
an ordinary accepted call is left uncoloured, so the two that went wrong
are the two you see. A service that keeps no log shows no panel, and its
endpoint answers `501` rather than an empty list.

Three cards offer actions, because they are the ones that drive the money:

- **Worldline** — run the morning (`ER`) or afternoon (`AR`) cycle now,
  instead of waiting for 08:00.
- **Settlement** — pull from Worldline now, instead of waiting out
  `WORLDLINE_PULL_INTERVAL`.
- **Banking Circle** — *Fund safeguarding accounts*: credit each empty
  safeguarding account 1,000,000 through the bank's internal bridge
  (`CONSOLE_BC_INTERNAL_URL`), the wire Worldline makes on its morning slot.
  An account already holding money is left alone, and the toast says which.

And a registered platform brings its own.

### The platform under test: a plugin

The console knows no platform by name. A platform **registers its own
card** — `POST /v1/plugins` with a descriptor, renewed every ten seconds —
and the console draws it at the top of the Platform view with the same
components as every other card: summary, where it is reached, its logs as
activity panels, its endpoints, and the buttons it declared, each forwarded
to the path it declared and nowhere else. [plugins.md](plugins.md) is the
contract; buddy's `infinite-local-runner` is the reference plugin and
registers on start (`pnpm nx up infinite-local-runner`).

A card's **Registered** row says how that stands: *live* (renewed within
thirty seconds), *lapsed* (stopped renewing) or *stopped* (it said goodbye
on shutdown). A lapsed or stopped platform is grey, not red — it is not a
lab service that is down — and shows the platform's own words on how to
start it where its buttons were. No platform registered is a note on the
Platform view saying how to register one.

A platform that settles gets the **settlement files** table: one row per
file it can run (its `files_path`), with currency, the MIDs it pays, its
total, and its last run (root, payouts, when).

- **Run** on a row — one `POST /v1/plugins/{id}/run {"files":[name]}`,
  forwarded to the platform's `run_path`. The runner uploads the file under
  a name not used before, triggers the pipeline, waits for the balance
  check, funds the safeguarding accounts, ticks the check and follows the
  stages to the end. It answers `202` at once: the run takes about forty
  seconds, and the part worth watching is the traffic arriving in the vendor
  panels, not a spinner. While it runs it **holds the lab clock**.
- **Run N selected** — tick files in the first column and run them as
  one call. The platform starts them in the same moment, which is how two
  currencies arrive on a real morning and the case a single run never
  exercises. One file per currency: ticking a second EUR file unticks the
  first, because the platform runs one per currency at a time. The column
  is only there when the runnable files span more than one currency.
- **Delete** — on a file the platform marks deletable (buddy's runner: its
  generated files, never the fixtures the repo tracks), after a confirm
  that says there is no undo. One `DELETE /v1/plugins/{id}/files?name=`,
  forwarded to the platform's `delete_path`.
- **Open location** — the platform opens the file's folder in the file
  manager of the machine it runs on, the file selected where the desktop
  allows (`POST /v1/plugins/{id}/files/reveal`). The console cannot do it
  itself — it is in a container, and a web page may not open a local
  folder — so a platform that does not declare `reveal_path` gets no
  button. On a machine with no desktop session the toast says so and gives
  the path.

**Files from anywhere.** *Upload a file to …* under the table sends any
file on your machine to the platform, which keeps it in its own directory
(buddy's runner: `.runs/uploads/`) and lists it with its fixtures — runnable
if it is a Worldline file, *cannot run* with the reason if not. The bytes
are streamed through the console and never held by it (capped at
`CONSOLE_UPLOAD_MAX_MB`, 512); progress shows above the button. **Preview**
on any row opens the head of the file — the first 100 lines, or 1000 —
read no further however long the file is. **Delete** removes a file the
platform says may go (below). A platform that declares no `upload_path` or
`preview_path` gets no such buttons ([plugins.md](plugins.md#settlement)).

A row has no *Run* while its currency has a run in flight (one per
currency; a second is the platform's `409`). A file whose MIDs have no
merchant would settle nothing, so it has no *Run* either: the row says which
MIDs are missing and a note gives the platform's seed command.

buddy's runner declares one more button, **Run reconciliation sweep**
(primary): one tick of the platform's hourly BC payment reconciliation,
which is what moves payouts from `IN_PROGRESS` to `SUCCESS` (or
`REJECTED`). A tick takes about twenty seconds, so the runner answers `202`
at once and the outcome appears in its **Reconciliation sweeps** panel —
per root, how many payouts were open and how many it resolved; amber when
some stay open. A root is only swept an hour after its settlement *by the
lab clock*: after a run, press *+1 hour* on the Lab clock card, sweep, then
*Now*.

### Stand-ins: disconnect and hide

The **settlement** service and the **webhook receiver** played the
platform's part while the lab was being built — the settlement service
pulls Worldline's files every ten seconds and pays outlets at the daily
cutoff; Banking Circle's seeded subscription and ACI deliver to the
receiver. With a real platform plugged in they are noise: payouts at B4B
nobody made, deliveries to a listener that is not yours, a Worldline
*connect + list* arrow that is not your platform connecting.

Each stand-in's card has a **Stand-in** section listing its wiring, read
live from the services that own it, and two buttons:

- **Disconnect / Reconnect** — the settlement service's timers stop (a
  forced *Pull from Worldline now* still works); the receiver's Banking
  Circle subscriptions are deactivated through the bank's own API, so the
  Banking Circle card says so too; ACI, when it points at the receiver,
  records its webhooks as `held` instead of sending them. Nothing already
  delivered is undone.
- **Hide this card** — off the Platform view and out of the diagram (the
  receiver's box and its *notification batches* arrow). A note where the
  cards were lists what is hidden and by whom, with a *Show* for each.

`payment-api` and `notifier` are stand-ins with nothing calling them on
their own — only `make demo-payment` and the harness's first scenario do —
so they can be hidden, not disconnected. Their cards say what they play, and
each now keeps a log (*Payments received*, *Webhooks delivered*) so it is
plain whether anything has called them.

**Every stand-in card starts hidden.** With a platform plugged in they are
noise; developing a vendor without one, you want them, and the place to
switch them is **Configuration → Stand-ins**: one table with what each
plays, how it is wired right now, *Connect* / *Disconnect* (live, where the
wiring lives) and *Show card* / *Hide card*. The Platform view lists the
hidden ones in a note linking there, and a hidden receiver is said under
the diagram, with how many batches still went to it.
`CONSOLE_STAND_INS_SHOWN=true` on the console starts the cards shown. The
wiring starts connected, so the lab runs end to end with no platform:
`SETTLEMENT_CONNECTED=false` on the settlement service and
`ACI_WEBHOOK_CONNECTED=false` on ACI start it off — read at start-up, so
they take a `make up`.

A platform can ask for this itself, in its descriptor's `stand_ins`
([plugins.md](plugins.md#stand-ins)). The lab API applies that when the
platform registers or its wishes change — not on every renewal, so a
button pressed here holds. The lab API keeps *shown* in memory: after a
lab API restart every card is back until the platform's next registration
re-applies its wishes. *Connected* lives in the services and survives a
lab API restart.

```sh
curl -s localhost:8097/v1/stand-ins                                           # each stand-in, its wiring, live
curl -s -XPOST localhost:8097/v1/stand-ins/receiver -d '{"connected":false}'  # disconnect
curl -s -XPOST localhost:8097/v1/stand-ins/settlement -d '{"shown":false}'    # hide
```

### The lab clock

Settlement only runs on a business day, which makes the calendar a test
input rather than an obstacle. The **Lab clock** card (the `clock` service,
`:8096`) says what time the lab thinks it is — in UTC, and in Paris,
Stockholm and London, where the platform's and the bank's calendars live —
whether it is a business day on every calendar (GB and SE, with their bank
holidays), and whether anything holds it. The buttons — *Now* (back to the
real clock), *+10 min*, *+30 min*, *+1 hour*, *±1 day*, *Nearest business
day*, *Move to Sunday* — and the *Date (UTC)* / *Time (UTC)* fields with
*Set clock* are each one `POST /v1/clock`, forwarded to the clock
service's `POST /clock` (`advance`, `at` or `mode`). Its *Clock changes*
panel is every move and every hold.

Every vendor follows it (`LAB_CLOCK_URL`), so bookings, files and report
dates move together, within a second and without a restart; a registered
platform with `"clock": "follows"` follows the same service — buddy's
runner writes the offset into the file every one of its processes watches.
The lab starts on *auto-business-day* (`CLOCK_START` in compose.yml): the
most recent business-day instant, so a weekend evening still settles.

A move is refused while a settlement run holds the clock (the clock
service's `409`, with the holder's reason), and while a platform registered
on the wall clock is live — that platform cannot follow, and moving the
vendors would put them on a different day from it.

Moving the clock drives scheduled vendor work too: move past Worldline's
morning or afternoon slot (08:30 / 15:30) and that slot's file is delivered
within a few seconds, as it would have been when the day got there.

The scenario worth knowing: move to a Sunday and run, and the balance check
completes as `NON_BUSINESS_DAY` with **0 payouts**; advance a day and run
again, and the same money settles — in one measured pair, 6 payouts of
55,397.56, exactly twice the usual daily figure, because Sunday's movements
were booked and not paid.

### Connect your platform: the .env and the keys

Pointing a platform at the lab takes a handful of variables and, for three
vendors, key material. Each vendor card carries a nested **Connect your
platform** panel: which variables it sets, in the platform's own names
(`BC_API_BASE_URL`, `B4B_JWT_PRIVATE_KEY`, `WORLDLINE_SFTP_HOST_KEY_FINGERPRINT`
…), a **Download fintechlab-<vendor>.env** link, and — for Banking Circle,
B4B and Worldline — each key or certificate file on its own row. The
**Configuration** view carries it for every vendor at once, with
**Download fintechlab.env** — it is the lab's, not any one platform's.

- **Keys are live, the rest is compose.yml.** PEMs and base64 certificates
  are read from the lab's keys directory at the moment you click, so they
  are the ones it is running with; the SFTP host key's fingerprint is
  derived from its public key. Addresses, account ids and lab credentials
  are what compose.yml ships — the console cannot read another container's
  environment — and a test holds the two together.
- **The host is the one you came in on.** Open the console at
  `lab.example.test:8090` and the file says `lab.example.test`. `?host=` on
  any of the links picks another.
- **Choices are commented out.** `NODE_EXTRA_CA_CERTS` is a path, not a
  value; the AML stub replaces the platform's own verification-service. Both
  are in the file with a note, and neither is set.
- **A key not generated yet** is a note on its row and in the file, never a
  blank that looks like a value.
- Only client-side files are offered. See
  [security/ca-and-tls.md](security/ca-and-tls.md#what-the-console-hands-out).

The same thing, headless:

```sh
curl -OJ localhost:8097/v1/connect/env                         # every vendor → fintechlab.env
curl -OJ localhost:8097/v1/connect/env/banking-circle          # one vendor
curl -OJ localhost:8097/v1/connect/files/banking-circle/fintechlab-ca.pem
curl -s  localhost:8097/v1/connect                             # what each kit sets and offers (no values)
```

### Banking Circle: who is subscribed

Expanding the Banking Circle card shows its notification subscriptions
before its activity panels: the **endpoint** it will POST to, the **event
types** behind it (an inactive one is amber, and a count appears when the
event is narrowed to specific targets), the subscription's **status**, and
the **queue depth** if anything is not moving.

The queue column names two different stalls, because they are undone
differently. **Retained** is what the vendor kept when it gave up on an
endpoint that never answered and deactivated the subscription; it clears by
reactivating. **Queued** is what an operator paused; it clears by pressing
release.

This is the half of the vendor that is invisible until it is wrong. A
subscription is a URL the bank calls, and *nobody subscribed* and
*subscribed, pointing at the wrong host* both look the same from the
settlement side — silence. A service that subscribes on boot (as
`apps/banking-circle` does: authenticate, list, subscribe if there is none)
will have pointed its `BC_*` configuration somewhere else, and the empty
state says so rather than showing a blank table.

**Send test** fires the vendor's own
`POST /api/v1/notificationselfservice/clienttest/{id}` — a reachability
probe, not an event, so it is not filtered by event type or target. The
vendor answers `200 "sent"` whether or not anything took it, so the console
then reads Banking Circle's own notification log and reports what the
endpoint actually did:

```
Endpoint took it     batch of 1 to https://receiver:8443/raw-events — PaymentStatus
Nothing took it      … — Post "https://receiver:8443/raw-events": dial tcp: lookup …
```

### Pausing notifications

**Pause** stops Banking Circle delivering to one subscription. Everything it
would have sent waits, in order, and **Release** sends it — cut into the
same batches it would have gone out in, oldest first.

This exists because a webhook that arrives four milliseconds after the
event it describes is a webhook nobody can watch arrive. Paused, the gap
between "the bank did something" and "the platform was told" is as wide as
you want it, which is where the interesting questions live: what does the
platform show while a payout is settled at the bank but unconfirmed, does
it recover when eleven notifications land at once, and does it cope with
them arriving in order but late. Breaking a subscriber to find out means
breaking it, and a broken subscriber also loses the notifications.

The pause is the vendor's own switch —
`POST /sim/subscription/{id}/pause` and `/resume`, under `/sim` because the
real API has nothing like it — not something the console remembers. That
matters: while it is paused, the queued notifications appear in **Notifications
sent** below, amber, named by event type, with the number still waiting:

```
notification.queued   queued: 2 notification(s) for … — OutgoingPaymentBooked ×2 — delivery is paused
notification.pause    delivery to … resumed — 2 notification(s) released
notification          batch of 2 to … — OutgoingPaymentBooked ×2
```

A pause that hid the notifications would be a worse lie than a broken
endpoint, since a quiet log reads as *nothing happened*. It stops delivery
and nothing else: payments still process, notifications are still produced,
matched against subscriptions and ordered. They just wait.

### Payouts: return and reverse

The card's **Payouts** panel lists Banking Circle's outgoing payments,
newest first, and offers the two things that can happen to one after it
has been processed. **Return** is the beneficiary's bank sending it back:
the payout stays processed, and the money arrives as a new incoming payment
flagged `return`, the way the real bank models it. **Reverse** is the
scheme undoing it: the payout becomes reversed, with a second booking.
Only a processed payout that has not already come back gets the buttons;
the console decides that from the vendor's own record, so it never offers
a call the vendor would refuse.

Each button is one call to the vendor's own hook,
`POST /sim/payments/{id}/return` or `/reverse` on the credentialed listener
(the same handlers as `/internal/payments/{id}/…` on the bridge), through
`POST /v1/banking-circle/payouts/{id}/return|reverse` here. Both are
irreversible, so the confirm says so rather than asking whether you are
sure. Pair either with a paused subscription to watch the platform find out
late.

## Banks

Two banks, and they are where the money is at two different moments:

- **Banking Circle** — a vendor. Its accounts are where the safeguarding
  money sits until a payout leaves. Read over mTLS with a real `Basic` →
  `Bearer` exchange, exactly as a client would; the console has no
  privileged back door into a vendor's state, which is what keeps the view
  honest.
- **Business bank** (`bank`, once called the *core ledger*) — the
  merchants' own bank, the far end of every payout. Banking Circle credits
  each processed payout here (`BANK_CREDIT_URL`) by the account B4B handed
  it — an IBAN, or an account number and sort code — and the outlet's
  account opens the first time money arrives for it. The InfinitePay
  accounts take the platform's fee; the Alice/Bob demo accounts are
  `make demo-payment`'s. In memory, fake `GB00SIM…` IBANs. You can open
  accounts here.

Each is also a card on the Vendors view, and each bank card here links to
it: this view is their accounts; the service card is their health,
endpoints and traffic. The business bank's card keeps two logs —
*Payouts credited* and *Accounts and transfers* — and the Dashboard's
*payouts credited* arrow, Banking Circle → business bank, counts the first.

Until this change, B4B's bridge into Banking Circle never said which account
a payout was for, so Banking Circle had nothing to credit and logged "no
beneficiary IBAN" for every platform payout: the last hop of the money's
path did not happen. The bridge now carries the creditor account the payout
named (or the beneficiary's registered one).

Banking Circle has **no account-opening endpoint**, so the console does not
offer one, and says why. An account there appears when it is first paid
into. The API returns `501`, not `500`, for that call: a UI that cannot
tell "this vendor has no such endpoint" from "this call failed" will offer
a retry button forever.

## Merchants

The hierarchy the platform sells through: **distributor → partner →
merchant → outlet**. The outlet is the unit the money cares about —
Worldline calls it a submerchant and identifies it by **MID**, and the
platform pays out per MID, not per merchant.

**Whose these are.** The merchants here are the lab's: this registry and
what it made at the vendors (a beneficiary per outlet at B4B, card payments
per MID at Worldline). The platform's own database knows none of them. To
settle them end to end, a platform seeds its records from
`GET /v1/merchants` — each outlet's MID, payout account, country and
currency. Testing only the settle path, the platform's own seeder (buddy's
runner: *Seed merchants*) works the other way round, and these are not
needed. The view opens with a note saying so.

Before this view, MIDs were string literals in tests. That is fine for a
scenario and useless for a demo, where the first question is "who am I
paying?".

Creating a merchant generates everything you did not supply — address,
post code, MCC, contact, and per outlet a MID, a terminal id and a payout
account. All of it is **derived from the record's own id**, not drawn from
a random source, so the same merchant always looks the same: a screenshot,
a reloaded registry and a test fixture agree. All of it is obviously fake —
`GB00SIM` accounts, `.test` domains (RFC 2606, so they can never be
mailed), streets nobody can post to.

Per merchant:

| Action | What it actually does |
| --- | --- |
| **Register outlets at B4B** | `POST /oversight/v1/beneficiaries` per outlet, keyed on the MID, signed with the same RS512 JWT settlement uses. Results are reported per outlet: a partial run is a normal outcome and you need to see which one failed. |
| **Seed card payments** | `POST /sim/transactions` on Worldline. This is the **acquirer's** own data posted to the acquirer — the platform never learns of a transaction except through the settlement file, and nothing here shortcuts that. Dated yesterday, because Worldline settles T+1. |
| **Sanctions dropdown** | `PUT /sim/beneficiaries/{mid}/sanctions`. Move an outlet to `fail` and watch the payout gate stop it, rather than reading that it would. |
| **Suspend / Delete** | Registry-local. Deleting does **not** unwind anything already registered at B4B — the rail has no beneficiary-delete endpoint, and pretending otherwise would be a lie about what it supports. |

### Batches: many merchants at once

**Create a batch…** (top right) makes up to 500 merchants in one call:
each with a name and a trade (and that trade's MCC), a country and its
currency, a company form to match (*Ltd*, *GmbH*, *B.V.*…), and one outlet
— about a quarter two, a few more, never above the cap you set. A mix of
countries draws only from the EUR and GBP ones, the currencies Banking
Circle holds safeguarding accounts in; pick a country by name for any
other. Everything
is drawn from a **seed**, not at random (the design system's contract 8):
the toast says the seed, and the same seed makes the same names, trades
and countries again, with new ids and so new MIDs.

Each batch is a row under **Batches**, with one call per button over the
whole batch, reported per merchant: **Register at B4B** (every outlet a
beneficiary), **Seed 5 payments / outlet** (dated yesterday, each merchant
at its own typical ticket rather than one amount for all), **Delete** (the
registry only — B4B and Worldline keep what they were sent).

```sh
curl -s -XPOST localhost:8097/v1/merchants/batches -d '{"count":50,"max_outlets":3,"country":"","seed":42}'
curl -s -XPOST localhost:8097/v1/merchants/batches/bat_0007/provision
curl -s -XPOST localhost:8097/v1/merchants/batches/bat_0007/trading -d '{"count":5}'
curl -s -XDELETE localhost:8097/v1/merchants/batches/bat_0007
```

The registry is on disk, and so is B4B's beneficiary store
(`B4B_STATE_PATH`), so both survive a restart; delete one without the other
and an outlet shown here as registered is one B4B has never heard of.
Re-registering is safe: a lookup by `external_ref` answers with the latest
registration of that reference.

The `external_ref` is the **MID**, and that matters. B4B mints its own
beneficiary id, so a platform that keys payouts on its own reference has
only that reference to look up by — B4B resolves either. Without it,
`settlement` would look up an id nothing had registered, land on an
auto-vivified record, and pay against account details nobody supplied
while the registered ones sat unread. Which is precisely what it did
before the console existed to notice.

### The five-minute demo

1. **Merchants** → *Create merchant*, two outlets.
2. *Register outlets at B4B* — each MID comes back with a beneficiary and
   a `pass`.
3. *Seed 5 card payments / outlet*.
4. **Services** → Worldline → *Run morning cycle (ER)*. One file per
   currency, covering every MID that traded, PGP-encrypted onto Worldline's
   SFTP root, with the lump sum credited to the safeguarding account.
5. **Services** → Settlement → *Pull from Worldline now*. It dials SFTP,
   authenticates, downloads, decrypts, parses, splits per MID and pays each
   outlet through B4B.
6. **Banks** → Banking Circle — the safeguarding balance moved.

Every one of those steps is a plain HTTP call you can make yourself; the
console just saves you finding it.

## Configuration

Four things, kept strictly apart by whether they are **live** or merely
**documented** — a console that shows a stale value as if it were live will
send you debugging the wrong thing for an hour:

- **Stand-ins** — the lab's scaffolding for the platform's part, with the
  live switches: connect or disconnect each, show or hide its card (see
  *Stand-ins* above). The one section here that changes what the lab does
  the moment you press it.
- **Banking Circle notification delivery** — the retry table, rendered
  with both the documented wait and the compressed one, so you can see that
  `time_scale` turns a 48-hour story into a few seconds. Asked of the
  *service* (`GET /sim/delivery-config`), not read from the file, because
  `BC_TIME_SCALE` in compose.yml overrides the file's `time_scale` — a
  panel that read only the file would confidently report a schedule nobody
  is on. The file is the fallback when the service cannot be reached, and
  says so. Read-only either way: `banking-circle` loads the file at
  start-up, so an edit that appeared to take effect immediately would be a
  lie. Edit it on the host and restart that one service.
- **Worldline file-exchange channel** — genuinely live and genuinely
  editable (`GET`/`PUT /config` on the worldline service).
- **Environment reference** — the knobs that shape the lab, grouped by
  service and described, so finding one does not mean grepping a 400-line
  compose file. Labelled as *what compose.yml ships*, never as a reading of
  the running container. The console's own environment is the one block
  shown live, and it says so.

## Docs

Two pages, switched at the top of the view.

**Guide** (`#docs`): how the three parts fit (your platform with its test
harness, the runner library, and this lab) as a diagram, a card per part,
and the set-up steps in order, each with its command and what you should
see afterwards.

**API reference** (`#docs/api`): every API a caller can use.

- A diagram of who calls which API: the console page, scripts and agents,
  the harness, and your platform, against the lab API and the services.
- *Which to call*: intent to API ("make a vendor misbehave" goes to the lab
  API's passthrough to that service's `/sim/*`).
- **The lab API's routes**, read from the lab API itself (`GET /v1`),
  grouped, each with what it does, which service it reaches, and its old
  `/api` path where it has one. The table that registers the routes is the
  table this page draws, so it cannot list a route that does not answer.
- **Each service's API** as a card: where a script on this machine reaches
  it, ports, auth, the endpoints worth knowing, and the path of its full
  reference in the repo. `#docs/api:banking-circle` links straight to one.

The long form is in the repo: [api/README.md](api/README.md) (the
overview), [api/lab-api.md](api/lab-api.md) (every lab API route
with bodies and answers) and `api/services/<id>.md` (each service). Tests
hold all three to the code.

## Configuring the console and the lab API

The console takes two variables:

| Variable | Default | What it does |
| --- | --- | --- |
| `LISTEN` | `:8090` | listen address |
| `LAB_API_URL` | `http://127.0.0.1:8097` | the lab API it forwards `/v1` (and the old `/api`) to |
| `LAB_API_TOKEN` | unset | when the lab API asks for a token, the console adds it to everything it forwards |

Everything else is the lab API's. The names keep their `CONSOLE_` prefix
from when the console's server did this work, so an existing setup keeps
working:

| Variable | Default | What it does |
| --- | --- | --- |
| `LISTEN` | `:8097` | listen address |
| `LAB_API_TOKEN` | unset | ask every caller but `GET /health` for `Authorization: Bearer <it>` |
| `CONSOLE_URL_<SERVICE>` | the published `127.0.0.1` port | where to reach a peer (`CONSOLE_URL_BANKING_CIRCLE`, etc.) |
| `CONSOLE_BROWSE_HOST` | `127.0.0.1` | the host the published ports are on, for the "open in a browser" links |
| `CONSOLE_STATE_FILE` | `console-data/registry.json` | the merchant registry |
| `CONSOLE_PROBE_INTERVAL` | `5s` | how often to health-check |
| `CONSOLE_BC_DELIVERY_CONFIG` | `config/banking-circle.json` | the delivery table to display |
| `CONSOLE_KEYS_DIR` | `keys` | the lab's keys directory, for the connect-your-platform downloads |
| `CONSOLE_BC_INTERNAL_URL` | `http://127.0.0.1:8095` | Banking Circle's internal bridge, for *Fund safeguarding accounts* |
| `CONSOLE_UPLOAD_MAX_MB` | `512` | the largest settlement file the lab API passes on to a platform |
| `CONSOLE_URL_CLOCK` | `http://127.0.0.1:8096` | the lab clock, for the clock card and `/v1/clock` |
| `CONSOLE_STAND_INS_SHOWN` | `false` | start the stand-ins' cards shown rather than hidden |
| `CA_FILE`, `CLIENT_CERT`, `CLIENT_KEY` | `keys/certs/…` | mTLS material for Banking Circle |
| `B4B_JWT_PRIVATE_KEY_PATH`, `B4B_JWT_KEY_ID` | `keys/b4b-keys/private.pem`, `b4b-mock-1` | signs the bearer token for beneficiary registration |

Missing credentials are **not** fatal. Without the B4B key, merchants can
still be created and the view says why they cannot be registered; without
the CA, the Banking Circle panels report the error instead of vanishing. A
control panel that refuses to start because one peer's credentials are
missing is worse than one that tells you which.

## The UI

One HTML file, one stylesheet, one script, embedded in the binary with
`//go:embed`. No framework, no bundler, no CDN — the lab's promise is one
`make up` on a laptop with no network, and a package registry in that path
would be the first thing to break. A test asserts the assets reference no
external host.

Dark by default with a light theme behind the toggle; purple is the accent
throughout, and status colour is reserved for status, so nothing decorative
is ever green or red.

The look and feel is written down in
[design-system.md](design-system.md) — tokens, layout, components, the
behaviour contracts (polling that does not eat a half-typed form, errors
shown verbatim, four health states) and the copy voice — so another app in
this family comes out the same without reading this one's stylesheet. That
document is the source of truth; `app.css` is one instance of it, and the
last section lists where the console has not caught up yet.
