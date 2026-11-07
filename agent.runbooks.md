# Agent runbooks — driving the lab headless

For an agent (or a person) picking the lab up cold: how to bring it up, drive
it without the UI, and read what happened. Every command here has been run
against the lab. The design docs say *why*; this says *how*.

The platform under test is **buddy** (the Infinite monorepo), run locally by
its `infinite-local-runner` on the host. The lab is this repo, in containers.
The lab knows no platform by name: the runner **registers itself** with the
lab as a plugin (docs/plugins.md; it posts to the console's address, which
forwards to the lab API) and **follows the lab's clock**.
Paths below: `~/gh/fintechlab-simple` (this repo) and
`~/infinite/buddy/infinite-local-runner` (the runner) — adjust to your
checkout.

## What listens where

| Service | Port | Notes |
| --- | --- | --- |
| **lab API** | 8097 | `/v1/*` — the one API over the lab, and the headless surface: `GET /v1` lists every route |
| console | 8090 | the UI; forwards `/v1` (and the old `/api/*`) to the lab API |
| banking-circle | 8085 | Connect API: mTLS + Basic→Bearer — the platform's. To steer or read the bank, use the lab API |
| banking-circle bridge | 8095 | plain HTTP, no auth: `/internal/*` lab hooks |
| worldline | 8084, 2222 | SFT channel over HTTP; SFTP+PGP on 2222 |
| b4b | 8086 | Oversight API (RS512 JWT) |
| aci / verify / verification | 8087 / 8088 / 8089 | |
| bank / settlement / receiver | 8081 / 8083 / 8443 | the bank is a vendor; settlement and receiver are stand-ins |
| *every service's control port* | its port + 1000 (9081–9096, 9443) | `/sim/*` and `GET /health`, plain HTTP, no vendor login; not published by compose — reach it through the lab API's `/v1/services/<id>/sim/…` |
| **clock** | 8096 | the lab's business clock: `GET/POST /clock`, `/clock/holds` |
| **runner** control API (host) | 3109 | `/status`, `/sim/files`, `/sim/run`, `/sim/sweep`, `/sim/clock` (read-only), `/sim/fund-sga` |
| buddy `apps/banking-circle` (host) | 3114 | webhook receiver; internal API under `/api/v1/internal/...` |

The console's service cards list the routes worth knowing for each service
(Vendors → open a card), from `internal/console/catalogue.go`. Every route,
with bodies and answers, is in [docs/api/](docs/api/README.md); the lab API
lists its own with `curl -s localhost:8097/v1`, and the console draws all of
it on Docs → API reference (`#docs/api`). A service's `/sim/*` hooks are all
reachable through the lab API as `/v1/services/<id>/sim/…`, with Banking
Circle's mTLS and token and B4B's JWT added for you.

## Lifecycle

```sh
cd ~/gh/fintechlab-simple
make test     # vet + gofmt check + go test ./..., no containers
make up       # rebuild images and recreate the whole lab — after any code change
make start    # restart from the images already built
```

- **Rebuild the whole lab, not one vendor.** Recreating only banking-circle
  leaves b4b's bridge pointing at its old address and payouts time out. Use
  `make up` / `make start`.
- **There is no console-only rebuild** (nor lab-API-only). podman-compose 1.0.6 ignores
  `--no-deps`: `podman-compose up -d --no-deps console` runs the console with
  `--requires` on everything it `depends_on`, and recreates or restarts bank,
  receiver, verify, banking-circle, worldline, b4b and settlement with it —
  even with `compose.yml` unchanged since the lab came up. Banking Circle's
  in-memory state (the platform's subscription included) goes with it; after
  a compose.yml edit the dependants were also left `Created` rather than
  running. So treat a console rebuild as a lab restart: `make up`, then
  restart the runner (next point).
- **After the lab restarts, restart the runner.** A fresh banking-circle has
  no subscriptions; buddy's `apps/banking-circle` subscribes on boot, so it
  needs the restart to get a new subscription (its id changes every time —
  read it from `GET :8097/v1/banking-circle/subscriptions`). The runner's
  card and clock need nothing: it re-registers within ten seconds of a
  lab API restart, and keeps the last offset while the clock is away.

Runner, from `~/infinite/buddy/infinite-local-runner`. Its generic half is
the library `~/gh/fintechlab-runner`, linked into the runner's
`node_modules`: once per checkout, and after changing the library, run
`bash init/link-lab-runner.sh` (`setup.sh` does it; `FINTECHLAB_RUNNER_DIR`
overrides the path). To have the runner disconnect and hide the lab's
stand-ins on registration, `cp fintechlab.example.json fintechlab.json`.

```sh
# start (long-running: background it, output to a file). apps/banking-circle
# reads the lab's mTLS certificate from FINTECH_SIM_LAB, default
# ~/gh/fintechlab-simple/keys; export it only if the lab's LAB_KEYS_DIR differs.
# FINTECH_SIM_LAB_CONSOLE_URL / FINTECH_SIM_LAB_CLOCK_URL default to this host.
node -r ./src/clock-shim.cjs -r @swc-node/register src/up.ts > up.log 2>&1 &
# stop: SIGINT the up.ts process, it takes its children with it
kill -INT "$(pgrep -f '^node -r ./src/clock-shim.cjs -r @swc-node/register src/up.ts$')"
# ready when every process reports up
curl -s localhost:3109/status | python3 -c "import json,sys; d=json.load(sys.stdin); print([(p['name'],p['state']) for p in d['processes']])"
# registered, and following the lab clock
grep -E '🧩|⏰ following' up.log
curl -s localhost:8097/v1/plugins | python3 -c "import json,sys; print([(p['id'],p['plugin']['state']) for p in json.load(sys.stdin)['plugins']])"
```

Kill it with `kill -INT <pid>`, not `pkill -f up.ts`: that pattern also
matches any shell whose command line mentions `up.ts` — including the one
running the `pkill`.

## Keys and connecting the platform

Everything the lab generates is under `keys/` (`certs/`, `b4b-keys/`, `wlsftp-keys/`;
`keys/README.md` says what each file is for). Containers see it at `/keys`.
The platform does not need to read it: the console hands out an `.env` with
the keys inlined, in buddy's variable names.

```sh
curl -OJ localhost:8097/v1/connect/env                  # every vendor → fintechlab.env
curl -OJ localhost:8097/v1/connect/env/b4b              # one vendor
curl -OJ localhost:8097/v1/connect/files/banking-circle/fintechlab-ca.pem   # NODE_EXTRA_CA_CERTS
```

`LAB_KEYS_DIR` moves them: `make` generates into that path instead. buddy
reads the same directory as `FINTECH_SIM_LAB`, whose default is this repo's
`keys/` (`~/gh/fintechlab-simple/keys`), so it needs setting only when
`LAB_KEYS_DIR` does (the subdirectory names are the ones its `setup.sh` and
`init/run-banking-circle.sh` expect).

`make keys-regenerate CONFIRM=yes` issues new keys and restarts the lab
(`make pod-up REGENERATE_KEYS=true` for the pod). Every copy the platform
holds stops working: restart the runner, and download the `.env` again.

## Running it as one pod instead

`make pod-images && make pod-up` runs the same lab as one podman pod from
images (docs/deploy-pod.md), on the same ports — so `make down` first.
Container names become `fintechlab-<service>` (e.g.
`podman logs fintechlab-banking-circle`), and state lives in `fintechlab-*`
volumes rather than `keys/`, `console-data/` and `b4b-data/`. `make pod-down`
stops it and keeps the volumes.

## The clock

One clock for the whole lab: the `clock` service (`:8096`). Every vendor
follows it (`LAB_CLOCK_URL` → `GET clock:8096/clock`, polled every second),
and so does the runner, which writes the offset into the file its
processes' clock shim watches. Bookings, files, report and balance dates
all move together.

```sh
curl -s localhost:8096/clock                                   # now, offset, calendars, holds
curl -s -X POST localhost:8096/clock -d '{"advance":"1h"}'     # also "10m", "-1d"
curl -s -X POST localhost:8096/clock -d '{"at":"2026-09-28T07:45:00Z"}'
curl -s -X POST localhost:8096/clock -d '{"mode":"auto-business-day"}'   # where the lab starts
curl -s -X POST localhost:8096/clock -d '{"mode":"real"}'
```

`POST localhost:8097/v1/clock` is the same through the lab API (it also
refuses while a platform registered on the wall clock is live).

- A vendor logs `labclock: now … (1h0m0s from real)` when it follows a
  move: `podman logs fintechlab-simple_banking-circle_1 | grep labclock`.
  The runner logs `⏰ clock-shim: now … — lab clock: pinned …` per process.
- Moving past Worldline's 08:30 / 15:30 slot delivers that slot's file
  within ~5 s. A move undone within 5 s can go unnoticed by the scheduler.
- A run in flight **holds** the clock: a move is a `409` naming the run
  until it finishes (`holds` in `GET /clock`; a hold lapses after 15 min).
- Elapsed time (tokens, retries, delivery windows) stays on the real clock.
- The clock is in memory: `make up` puts the lab back on `CLOCK_START`.

## Run a settlement

```sh
curl -s -X POST localhost:3109/sim/run
# wait for it: running_now is null when done; last_run.root is the root id
curl -s localhost:3109/status | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('running_now'), d['last_run']['root'], d['last_run'].get('error'))"
```

From the console: Platform → **Local runner** (the runner's own card) → the
**settlement files** table. It is the runner's `GET /sim/files`, one row per
`worldline-reconciliation-sample*.csv` in buddy's `apps/settle-ingest/local`:

- **File, Currency, MIDs, Total** — read from the file itself; a file dropped
  into that folder shows up on the next refresh, no code change.
- **State** — `default` (what a bare `POST /sim/run` uploads), amber
  `running — <stage>` while its run is in flight, amber `<CCY> busy` when
  another file in the same currency is running, red `MIDs … not seeded` when
  the settle DB has no master account for them.
- **Last run** — root (first 8), payouts and total, and when it finished *on
  the lab clock* (e.g. `Fri 25 Sept, 13:00 UTC`).
- **Run** on a row is one `POST /sim/run {"files":[name]}`. Tick files in
  the first column (one per currency — ticking a second of a currency
  swaps it) and **Run N selected** is one call with those files.
  A busy or unseeded row has no *Run* and no box; an unseeded one gets a
  note with the seed command instead.
- **Delete** (generated files only; fixtures are refused with a 403) is
  `DELETE /sim/files?name=`; **Open location** is `POST /sim/files/reveal
  {"name"}` and opens the file manager *on the runner's machine* — both
  need the runner on buddy's `feature/fintechlab-file-actions` (or later)
  and `@fintechlab/runner` with `deleteHandler`/`revealHandler`. A runner
  without them declares no `delete_path`/`reveal_path`, and the rows have
  no such buttons.

Below it, the **Settlement runs** log: one row per run, rewritten as it goes.
Amber there is *with failed stages* — normally just the two report-email
stages, which have no mailer locally — and red a run that stopped.

Read the outcome from the settle DB (the runner's Postgres container):

```sh
q() { podman exec infinite-local-runner-postgres-settle psql -U postgres -d settle_db "$@"; }
R=<root id>
q -tAc "select s.status||'/'||t.status, count(*) from settlement_submissions s
        join transactions t on t.id=s.transaction_id
        where t.external_source_reference like 'sttl_v1:$R:%' group by 1"
q -tAc "select stage, status, outcome from settlement_processing_stage
        where root_id='$R' and stage in ('INFINITE_SETTLEMENT','BC_PAYMENT_RECONCILIATION')"
```

Expect the payouts to sit at `IN_PROGRESS` after a run: Banking Circle's
`Processed` webhook usually arrives before buddy has recorded the
`bcPaymentId` (the workers log `No transaction found … skipping`). The sweep
is what resolves them.

### Two currencies at once

The default run is the EUR file (MIDs 1–5). The GBP file (MIDs 6–10) needs
the GBP merchant seeded once, on top of the base seed; EUR-only runs ignore
it:

```sh
cd ~/infinite/buddy && pnpm exec tsx infinite-local-runner/seeders/seed-gbp.ts   # idempotent
curl -s -X POST localhost:8096/clock -d '{"at":"2026-09-25T09:00:00Z"}'         # a business morning
curl -s localhost:3109/sim/files | python3 -c "
import json,sys
for f in json.load(sys.stdin)['files']: print(f['name'], f['currency'], f['mids'], 'seeded' if f['seeded'] else 'NOT SEEDED')"
curl -s -X POST localhost:3109/sim/run \
     -d '{"files":["worldline-reconciliation-sample.csv","worldline-reconciliation-sample-gbp.csv"]}'
# one run per currency: a second in the same currency, or a clock move, is a 409 until they finish
curl -s localhost:3109/status | python3 -c "
import json,sys; d=json.load(sys.stdin)
print('in flight:', [r['currency'] for r in d['runs_now']])
for c,r in d['last_runs'].items(): print(c, r['root'], r['payouts'])"
```

In the console: tick both files and **Run 2 selected** on the settlement
files panel (see *Run a settlement*); both rows go amber `running — …`,
then each shows its own root and payout count.

Pick a morning, not `auto-business-day`: that can land late on a Friday, and
the sweep's +1h then crosses 22:00 UTC, when Stockholm is already Saturday.

Expect each root to pay its own merchants in its own currency, and **both**
Infinite fee payouts (EUR and GBP) under whichever root reached
`INFINITE_SETTLEMENT` first — buddy's internal settlement sweeps every
currency, not the root's. Alone, an EUR run makes 6 payouts (5 merchants +
Infinite); together it makes 5 and the GBP root 7. The sweep resolves them
all either way.

### Many merchants: generated test data

The fixtures are ten outlets. The runner generates more — merchants boarded
into both databases (25% IE/EUR, 75% UK/GBP; 75% one outlet, 20% two, 5%
three to ten; MIDs `7xxxxxxx`) and a Worldline file per currency paying
every one of them, with refunds, ~0.1% chargebacks and some reversed. The
rules are buddy's `infinite-local-runner/README.md`, *Generated merchants and
settlement files*. On the card they are **Seed merchants** and **Create
settlement file**, each a form; headless, the same two calls:

```sh
curl -s -X POST localhost:3109/sim/seed -d '{"merchants":200,"mode":"reseed"}'     # add | reseed | remove
curl -s -X POST localhost:3109/sim/settlement-file -d '{"transactions":20000}'     # "currency":"GBP", "dir":"…"
# both answer 202; each lands in the runner's test-data log a few seconds later
curl -s localhost:3109/sim/activity | python3 -c "
import json,sys
for l in json.load(sys.stdin)['logs']:
    if l['name'] == 'test-data': [print(e['status'], e['summary']) for e in l['events'][:3]]"
# the files are rows in /sim/files (…_payfac_ER_GBP.csv), run them like any other
curl -s -X POST localhost:3109/sim/run -d '{"files":["<stamp>_payfac_ER_GBP.csv","<stamp>_payfac_ER_EUR.csv"]}'
```

Both refuse with `409` while a settlement runs, and a run refuses while
test data is being made.

## Run the BC payment reconciliation sweep

It picks up sweep stages at least an hour old **by the lab clock**, so
move the clock, trigger, and move it back. The trigger is one call (the
console's *Run reconciliation sweep* on the Local runner card, a button the
runner declares); it answers
`202` and the tick takes ~20 s, so read the outcome from the runner's
`sweeps` log rather than the response:

```sh
curl -s -X POST localhost:8096/clock -d '{"advance":"1h"}'
curl -s -X POST localhost:3109/sim/sweep                                  # 409 if one is running
sleep 25; curl -s localhost:3109/sim/activity | python3 -c "
import json,sys
for l in json.load(sys.stdin)['logs']:
    if l['name']=='sweeps':
        for e in l['events'][:3]: print(e['status'], e['summary'], e.get('detail'))"
curl -s -X POST localhost:8096/clock -d '{"mode":"auto-business-day"}'
```

A sweep run seconds after the settlement can leave one payout open (the
bank had not processed it yet); the next tick resolves it.

The same tick without the runner's HTTP API, printing the handler's own log:
`node -r ./src/clock-shim.cjs -r @swc-node/register src/trigger-bc-recon.ts`
from `~/infinite/buddy/infinite-local-runner`.

It logs `open=N resolved=N unresolved=0` per root. Each resolved submission
records how in `raw_webhook_payload->>'report'`: `intraday-reconciliation`
(the report) or `payment-status` (the fallback). The stage stays
`PROCESSING` until a tick finds nothing open, and completes as
`PARTIALLY_PROCESSED` if anything is still open at 19:00 Paris time.

Where the lab shows it:

- **Diagram** (Dashboard): the bottom arrow, *intraday reconciliation*,
  Banking Circle → platform, lights on each intraday report read. It counts
  those reads only; a refused (400) read turns it amber.
- **Banking Circle card → *Reconciliation reads***: every intraday report,
  rejection report and payment-status call — time, caller, dates, page,
  properties asked for, rows or status returned. Refused calls are amber
  with the parameters they lacked; a status read for an unknown payment is
  amber `not found (404)`. A healthy sweep is one line like
  `intraday report 2026-09-24, page 1 → 8 row(s)` (payouts plus the day's
  incoming lump sums) and no status lines.

## Make Banking Circle misbehave on purpose

All through the lab API; each is one call to Banking Circle's control port,
with no vendor login. (The old bridge paths on `:8095/internal/…` still
answer until phase 4.)

```sh
BC=localhost:8097/v1/services/banking-circle/sim
# the next payouts end Rejected / stay Pending, one each, in order
curl -s -X POST $BC/payments/outcomes -d '{"outcomes":["Rejected","Pending"]}'
curl -s $BC/payments/outcomes                                       # what is still queued

# hold one subscription's webhooks, then release them
curl -s -X POST localhost:8097/v1/banking-circle/subscriptions/<sub_id>/pause
curl -s -X POST localhost:8097/v1/banking-circle/subscriptions/<sub_id>/resume

# after a payout processed: the recipient's bank sends it back, or the scheme reverses it
curl -s -X POST $BC/payments/<bcPaymentId>/return \
     -d '{"reasonCode":"AC04","reasonDescription":"Closed account number"}'
curl -s -X POST $BC/payments/<bcPaymentId>/reverse -d '{"reason":"scheme"}'
curl -s localhost:8097/v1/banking-circle/payouts                  # the payouts, and which can be returned/reversed
```

Worked scenario — a rejection and a hang, resolved only by the sweep: pause
buddy's subscription, queue `["Rejected","Pending"]`, run a settlement,
advance 1h and trigger the sweep (one payout goes `REJECTED` with its ledger
reversal, the pending one stays open, the rest `SUCCESS`), then resume the
subscription and check nothing changes twice.

## Reading what happened

```sh
podman logs --since 5m fintechlab-simple_banking-circle_1 | grep -v "GET /health"
sed 's/\x1b\[[0-9;]*m//g' up.log | grep -E "BcWebhookConsumerService|SettlementStatusHandler|BC payment reconciliation"
curl -s "localhost:3114/api/v1/internal/reconciliation/intraday-payments?transactionDate=2026-09-24&fromCreatedAt=2026-09-23T22:00:00.000Z&toCreatedAt=$(date -u +%FT%T.000Z)"
curl -s localhost:3114/api/v1/internal/payments/<bcPaymentId>/status
```

The last two go through buddy's own BC client to the lab's report and status
endpoints — the same path the sweep takes.

The console's panels and diagram are the lab API's JSON underneath:

```sh
# a vendor's activity logs — banking-circle has payments, notifications, reports
curl -s localhost:8097/v1/services/banking-circle/activity | python3 -c "
import json,sys
for l in json.load(sys.stdin)['logs']:
    if l['name'] == 'reports':
        for e in l['events'][:5]: print(e['at'], e['op'], e['status'], e['summary'])"
# one diagram arrow: count, when it last moved, and what moved it
curl -s localhost:8097/v1/flow | python3 -c "
import json,sys
for s in json.load(sys.stdin)['steps']:
    if s['id'] == 'recon': print(s['count'], s.get('last_at'), s.get('last_summary'))"
# the events behind one arrow — exactly what it counted (the hop drawer's data)
curl -s localhost:8097/v1/flow/hops/payouts | python3 -c "
import json,sys; d=json.load(sys.stdin)
print(d['step']['count'], d['step'].get('source'), d['step'].get('source_log'))
for e in d['events'][:5]: print('   ', e['at'], e['status'], e['summary'])"
```

The stand-ins (settlement, receiver; payment-api and notifier only hide) can
be disconnected so their own traffic stops competing with the platform's —
the runner's `fintechlab.json` may already have asked for it on
registration. Their cards start hidden (console: Configuration →
Stand-ins):

```sh
curl -s localhost:8097/v1/stand-ins | python3 -c "
import json,sys
for s in json.load(sys.stdin)['stand_ins']: print(s['id'], s['state'], 'shown' if s['shown'] else 'hidden', s.get('set_by'))"
curl -s -X POST localhost:8097/v1/stand-ins/receiver -d '{"connected":false}'   # BC subscription inactive, ACI holds
curl -s -X POST localhost:8097/v1/stand-ins/receiver -d '{"connected":true,"shown":true}'
```

The runner's side, through the lab API — its card is a plugin, so every
call is under `/v1/plugins/infinite-local-runner`:

```sh
curl -s localhost:8097/v1/plugins/infinite-local-runner/files | python3 -c "
import json,sys
for f in json.load(sys.stdin)['files']:
    r=f['last_run'] or {}
    print(f['name'], f['currency'], f['seeded'], 'busy' if f['in_flight'] else 'free',
          r.get('root'), (r.get('payouts') or {}).get('count'), r.get('finished_at'))"
curl -s -X POST localhost:8097/v1/plugins/infinite-local-runner/run -d '{"files":["worldline-reconciliation-sample-gbp.csv"]}'
curl -s -X POST localhost:8097/v1/plugins/infinite-local-runner/actions/sweep -d '{}'
curl -s localhost:8097/v1/services/infinite-local-runner/activity | python3 -c "
import json,sys
for l in json.load(sys.stdin)['logs']:
    print(l['name'], l.get('labels'))
    for e in l['events'][:3]: print('   ', e['at'], e['status'], e['summary'])"
```

The lab API passes the body to the path the runner declared
(`POST :3109/sim/run`, `/sim/sweep`) untouched, and the runner's refusals
come back as they are (`400` for an unknown file, `409`
for a busy currency). A log's `labels` is what its amber and red mean; the
console's pills use them.

Step ids, top to bottom: `pull`, `collect`, `ingest`, `fund`, `payouts`,
`bridge`, `land`, `callbacks`, `notify`, `confirm`, `reports`, `recon`.
`land` is Banking Circle → the business bank (`bank`, :8081): each processed
payout credited to the merchant's account there, read from the bank's
*Payouts credited* log (`curl -s localhost:8081/sim/activity`). It stays at 0
on a lab started before B4B's bridge carried the creditor account — `make up`.
`notify` is left out while the receiver's card is hidden; `hidden` in
`/v1/flow` says how much it still carried.

## Start clean, and read every service's log

Between runs, reset the lab instead of restarting it: a restart loses
Banking Circle's subscriptions and needs the runner restarted, a `flow`
reset keeps them (and B4B's boarding, Worldline's enrolment, the merchant
registry) and clears only what a run made.

```sh
curl -s -XPOST localhost:8097/v1/lab/reset | python3 -c "
import json,sys; d=json.load(sys.stdin); print('ok' if d['ok'] else 'FAILED')
for s in d['services']: print(' ', s['service'], s['status'], s.get('cleared') or s.get('error'))"
curl -s -XPOST localhost:8097/v1/lab/reset -d '{"scope":"all"}'              # as on first start, registry too
curl -s -XPOST localhost:8097/v1/lab/reset -d '{"services":["banking-circle"]}' # one service
```

A `409` names who holds the lab clock: a settlement run is in flight.
Wait for it, or add `"force": true`.

Every service's own log lines, merged by time — what `podman logs` on each
container would show, without a shell on the host:

```sh
curl -s 'localhost:8097/v1/logs?q=bcp_7f3a' | python3 -c "
import json,sys; d=json.load(sys.stdin)
for l in d['lines']: print(l['at'][11:23], l['service'].ljust(15), l['text'])
print('next:', d['next'], 'unreachable:', d['unreachable'])"
curl -s 'localhost:8097/v1/logs?services=b4b,banking-circle&since=<next>'   # what arrived since
curl -s 'localhost:8097/v1/services/settlement/logs?limit=50'                # one service
```

## Screenshots without a browser window

Playwright is in buddy's `node_modules` and its headless Chromium is cached:

```js
// node shot.cjs
const { chromium } = require('/home/andrej/infinite/buddy/node_modules/playwright');
(async () => {
  const b = await chromium.launch({ args: ['--no-sandbox'],
    executablePath: require('fs').readdirSync(process.env.HOME + '/.cache/ms-playwright')
      .filter((d) => d.startsWith('chromium_headless_shell'))
      .map((d) => `${process.env.HOME}/.cache/ms-playwright/${d}/chrome-headless-shell-linux64/chrome-headless-shell`)[0] });
  const p = await b.newPage({ viewport: { width: 1500, height: 1100 } });
  await p.goto('http://127.0.0.1:8090/#platform/infinite-local-runner'); // #view/card opens the card
  await p.waitForTimeout(1500);
  await (await p.$('.card.is-open')).screenshot({ path: 'runner.png' });
  await b.close();
})();
```

Inside the runner card: the settlement files table is
`table:has(th:text("Last run"))`, its buttons
`[data-select-file][data-name="<file>"]` (the tick boxes),
`button:has-text("selected")` and `[data-action="plugin-run"]`,
and the runs log opens with `[data-card="infinite-local-runner:runs"]`.
Switch views with `.nav-item[data-view="dashboard"]` clicks, or go straight
to a card with `#vendors/b4b` (or `#vendors/b4b:payments` for its log) — a
hash change routes. An arrow's drawer is `[data-hop="payouts"]` or
`#dashboard/payouts`; the drawer itself is `#drawer`. Wait ~10 s
after a click before expecting a stage name: the upload stage takes that
long to appear, and the panel refreshes every few seconds.

## Things that look broken and are not

- **The runner's balance check fails with Banking Circle `401` right after
  you rebuilt the console:** with podman-compose 1.0.6,
  `podman-compose up -d --no-deps console` still recreates the services the
  console depends on — Banking Circle, B4B, worldline, the bank — and they
  keep tokens and subscriptions in memory. Restart the runner after any
  console rebuild, as after any lab restart.
- **Every runner process dies at start with "@fintechlab/runner is not
  linked":** the link script has not run — `bash init/link-lab-runner.sh`.
- **Mailer `Service Unavailable` errors** in the runner log: the report
  emails have no mailer locally. Unrelated to payments.
- **`IncomingPaymentProcessed` ignored** by accounts-settlement: buddy does
  not act on incoming payments (including returns) today.
- **A subscription id you used an hour ago is gone:** the lab was restarted.
- **The sweep logs `Banking Circle does not know paymentId=…` and closes a
  root as `PARTIALLY_PROCESSED`:** that root's payouts were made before the
  lab last restarted, and Banking Circle keeps payments in memory. The amber
  `not found (404)` status reads in *Reconciliation reads* are the same thing.
  Only roots from the current lab session say anything about the sweep.
- **No *intraday reconciliation* arrow and an empty *Reconciliation reads*
  panel right after a run:** the sweep only takes stages an hour old by the
  lab clock. Advance the clock an hour and trigger it. The panel is
  also emptied by a lab restart (activity logs are in memory).
- **The files panel says `Fri 25 Sept, 13:00 UTC` and the runs log says
  `44s ago` about the same run:** both are right. *Last run* is the lab
  clock (the one the run happened on); every activity log is stamped with
  the wall clock, the runner's included, so "ago" is real.
- **The runner's card is grey, *stopped*:** it unregistered on shutdown.
  Start it again and the card is live within seconds. *Lapsed* means it
  stopped renewing without saying goodbye (killed, or cannot reach :8090).
- **A file row has no *Run*:** its currency has a run in flight (one per
  currency), or its MIDs are not seeded — the row says which.
- **Every run in *Settlement runs* is amber `with failed stages`:** the two
  report-email stages fail on the missing mailer. The payouts are unaffected.
- **The report has no rows for a late-evening payout on today's date:** the
  bank dates bookings on its business day — CET, 19:00 cutoff, weekends to
  Monday. Ask for the next business day.

## Vendor documentation

Banking Circle's docs (https://docs.bankingcircleconnect.com) are behind a
password — ask the user for it; never commit it. Append `.md` to any page URL
for markdown; API reference pages carry the OpenAPI JSON. What the lab
changed to match them, and why, is in
[docs/ARCHITECTURE-vendor-corrections.md](docs/ARCHITECTURE-vendor-corrections.md),
section H.
