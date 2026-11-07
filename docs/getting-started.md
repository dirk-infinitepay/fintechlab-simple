# Getting started: your platform against the lab

The README's five-minute start runs the lab on its own. This page is the
loop the lab exists for: a real platform settles a Worldline file through
B4B into Banking Circle, and reconciles what the bank says it did. The
platform here is buddy, run locally by its `infinite-local-runner`; any
platform that reads the same variables works the same way.

Once through, by hand, with the console. Every step is also one `curl`:
[agent.runbooks.md](../agent.runbooks.md) has those, and a list of things
that look broken and are not.

## Before you start

- This repo's prerequisites (README): Go, Podman or Docker, `openssl`,
  `make`.
- A buddy checkout with the runner set up once: from
  `infinite-local-runner/`, `podman compose up -d && ./setup.sh`
  (its README, *Quick start*). `setup.sh` links the runner's library,
  `fintechlab-runner` — clone it to `~/gh/fintechlab-runner` first, or set
  `FINTECHLAB_RUNNER_DIR`.
- The ports free: 8080–8096, 8443 and 2222 for the lab, 3109 and 3114 for
  the runner.

## 1. Start the lab

```sh
cd ~/gh/fintechlab-simple
make up
```

Keys and certificates are generated into `keys/` on the first run and kept
after that. To keep them somewhere else, `export LAB_KEYS_DIR=/some/path`
before `make up` — and use that path below instead.

Open the console: <http://127.0.0.1:8090>. Vendors should show every card
green within a few seconds.

## 2. Start the platform, pointed at the lab's keys

```sh
cd ~/infinite/buddy
pnpm nx up infinite-local-runner
```

The runner reads the lab's mTLS client certificate for Banking Circle from
the lab's keys directory, and `setup.sh` takes the B4B and Worldline keys
from there when it first writes its `.env`. That directory is
`FINTECH_SIM_LAB`, default `~/gh/fintechlab-simple/keys` — the lab's own
default. If the lab is checked out elsewhere or you set `LAB_KEYS_DIR`,
export `FINTECH_SIM_LAB` to the same path first; a wrong one shows up as
Banking Circle refusing the platform's TLS handshake.

The runner **registers its own card** with the console and **follows the
lab's clock** — it logs `🧩 registered with the lab console` and `⏰
following the lab clock`. In the console, **Platform → Local runner**
appears at the top, green and *registered*, and the **Vendors →
Banking Circle** card lists a subscription to
`host.containers.internal:3114`: the platform registered for webhooks.

**Restart the runner whenever the lab restarts.** Banking Circle keeps its
subscriptions in memory, and the platform only subscribes when it starts.

## 3. Run a settlement

Everything happens on the **Platform** view: the runner's card at the top,
the **Lab clock** card under it.

1. **The clock.** Settlement only leaves the bank on a business day. On the
   Lab clock card, set a weekday morning — e.g. *Date* `2026-09-25`, *Time*
   `09:00`, **Set clock**. The lab starts on the most recent business day,
   so this is only needed to pick a particular one — and it gives the sweep
   below an hour of room before Stockholm's day ends.
2. **Run.** On the Local runner card, in *settlement files*, press **Run** on
   `worldline-reconciliation-sample.csv` (EUR). The row turns amber with the
   stage it is on; about forty seconds later it shows the root id and the
   payouts.
3. **Watch it.** The **Dashboard** draws the run as it happens: the file
   collected, the safeguarding account funded, payouts to B4B, B4B handing
   them to Banking Circle, and Banking Circle's webhooks to the platform.

After a run the payouts are `IN_PROGRESS` on the platform. That is expected:
Banking Circle's webhook usually arrives before the platform has recorded
which payment it is about. The sweep is what resolves them.

## 4. Reconcile

1. On the Lab clock card, press **+1 hour** — a settlement is only swept an
   hour after it ran, by the lab clock. (While a run is in flight the clock
   is held and refuses to move.)
2. On the Local runner card, press **Run reconciliation sweep**. About
   twenty seconds later its **Reconciliation sweeps** panel shows one row, e.g.
   *1 root(s) at 2026-09-25 10:00 UTC: 6 payout(s) resolved*.
   If a payout is still open, sweep once more: a sweep straight after a run
   can beat the bank to the last payout.
3. Press **Nearest business day** on the clock to put the lab back where it
   started (or **Now** for the real clock).

On **Vendors → Banking Circle**, *Reconciliation reads* shows the intraday
report the sweep read, and the diagram's bottom arrow lights.

## 5. Read the result

The payouts, as the platform recorded them:

```sh
R=<root id from the Run row>
podman exec infinite-local-runner-postgres-settle psql -U postgres -d settle_db -tAc \
  "select s.status||'/'||t.status, count(*) from settlement_submissions s
   join transactions t on t.id=s.transaction_id
   where t.external_source_reference like 'sttl_v1:$R:%' group by 1"
```

`SUCCESS/SUCCESS|6` is the happy path: six payouts (five merchants and the
platform's own fee), each confirmed by the bank.

## What next

- **Break it on purpose:** queue a rejected payout, pause the platform's
  webhooks, return or reverse a payout — [agent.runbooks.md](../agent.runbooks.md),
  *Make Banking Circle misbehave on purpose*, and [console.md](console.md).
- **Two currencies at once:** seed the GBP merchant, then tick both files
  and **Run 2 selected** ([agent.runbooks.md](../agent.runbooks.md)).
- **Developing a vendor without a platform:** the lab's stand-ins play the
  platform's part — `settlement` pulls Worldline's file and pays it out
  through B4B and Banking Circle, `receiver` takes the webhooks. They run
  and are connected from `make up`, so the settle path works end to end on
  its own; only their cards are hidden. **Configuration → Stand-ins**
  shows them, and connects or disconnects each live. Once your platform
  plugs in, disconnect them (its `stand_ins` may do it for you) so what
  lands in the vendors' logs is yours.
- **A platform that does not read the lab's disk:** **Connect your
  platform** on the Configuration view downloads one `.env` with every
  address, credential and key inlined.
- **New keys:** `make keys-regenerate`, then restart the runner.
- **The lab on another machine:** [deploy-pod.md](deploy-pod.md); for a
  deployed platform, without the clock: [deploy-cloud.md](deploy-cloud.md).
- **Your own platform instead of buddy's runner:** register a card and
  follow the clock the same way — [plugins.md](plugins.md).
