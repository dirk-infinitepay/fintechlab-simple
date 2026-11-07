# Settlement (platform stand-in) — API

Scaffolding for the platform's settle pipeline. It pulls Worldline's
settlement file over real SFTP+PGP, decrypts and parses it, splits it per
submerchant (MID), and pays each outlet out through B4B, gated on the
safeguarding-account balance at Banking Circle, a merchant-verification
check and the beneficiary's sanctions status. Beside that it keeps the
platform's own internal settlement reports, state machine and
reconciliation API over the business bank's ledger, which pay nobody.
[catalogue.md](../../catalogue.md) files it as **scaffolding**, a platform
stand-in: it exists so `make harness` can prove each vendor hop is
connected, and it is not a model of any real platform. Replace it with
your own. Nothing in a real integration should call it, and a plugged-in
platform should disconnect it (`POST /sim/connected`), or both of them pay
the same outlets.

## At a glance

| | |
| --- | --- |
| Listens | `:8083`, plain HTTP. Compose service `settlement` (`http://settlement:8083`); host `http://127.0.0.1:8083`. Outbound it speaks SFTP to `worldline:2222` and HTTP to b4b, verify, banking-circle's bridge (`:8095`) and the bank.<br>`:9083`, plain HTTP: the **control port** (`SIM_CONTROL_LISTEN`), every `/sim/*` route below and its own `GET /health`. Not published by compose; the lab API reaches it at `http://settlement:9083` |
| Auth | None on its own routes. Outbound: an RS512 JWT for B4B signed with B4B's private key, `x-internal-api-key` for verify, SFTP password or key for Worldline, nothing for Banking Circle's bridge or the bank.<br>The control port has no vendor auth: it takes calls from anything that can reach it, or only from `SIM_CONTROL_ALLOW` (addresses or CIDRs) when that is set |
| Found with | `SETTLEMENT_URL` (`http://127.0.0.1:8083`) in the harness.<br>`B4B_CALLBACK_URL` on settlement itself (`http://settlement:8083/internal/b4b-webhook`), sent to B4B as each payout's `callback_url`; b4b's `WEBHOOK_ALLOWLIST` lists `settlement` and `settlement:8083`.<br>`CONSOLE_URL_SETTLEMENT` (`http://settlement:8083`) on the lab API, which takes the control port as that port plus 1000 unless `CONSOLE_CONTROL_URL_SETTLEMENT` names it |
| Activity logs | None. There is no `GET /sim/activity`, the lab's catalogue declares no activity path, and `GET /v1/services/settlement/activity` answers `501`. What it did is read from `GET /worldline/files`, the reports and each record's payout. Each request line is logged on stdout, and kept for `GET /sim/logs` |
| Configuration | The variables that change behaviour most: `SETTLEMENT_CONNECTED` (`true`), `WORLDLINE_SFTP_HOST` (unset: no pull at all), `CUTOFF_TIME` (`00:00`), `BC_SAFEGUARDING_ACCOUNT_ID_EUR`/`_GBP`. Everything below |
| Design record | [ARCHITECTURE-reconciliation-api.md](../../ARCHITECTURE-reconciliation-api.md), [ARCHITECTURE-settlement-state-machine.md](../../ARCHITECTURE-settlement-state-machine.md), [ARCHITECTURE-settlement-report.md](../../ARCHITECTURE-settlement-report.md), [ARCHITECTURE-sftp-staging.md](../../ARCHITECTURE-sftp-staging.md), [ARCHITECTURE-phase3-corrections.md §5](../../ARCHITECTURE-phase3-corrections.md#5-settlement-wire-in-verification-and-the-sga-balance-gate), [ARCHITECTURE-worldline-acquirer.md](../../ARCHITECTURE-worldline-acquirer.md), [structure/worldline.md](../../structure/worldline.md), [scenarios/settlement-to-payout.md](../../scenarios/settlement-to-payout.md), [plugins.md](../../plugins.md#stand-ins) (stand-ins) |

Configuration in full (default, then compose where it differs):

| Variable | Default | Effect |
| --- | --- | --- |
| `LISTEN` | `:8083` | |
| `SIM_CONTROL_LISTEN` | `:9083` (`LISTEN`'s port plus 1000) | the control port |
| `SIM_CONTROL_ALLOW` | unset | addresses or CIDRs, comma-separated, that may call the control port; unset, anyone who can reach it |
| `LAB_CLOCK_URL` | unset; compose `http://clock:8096/clock` | record timestamps, `pulled_at`, default dates, `requestedExecutionDate` and the cutoff follow the [lab clock](clock.md) |
| `SETTLEMENT_CONNECTED` | `true` | `false`, `0`, `off` or `no` starts the stand-in disconnected (`/sim/connected`) |
| `WORLDLINE_SFTP_HOST` | unset; compose `worldline` | unset: no puller, and `GET /worldline/files`, `POST /worldline/pull` and `POST /sim/pull` are not registered (`404`) |
| `WORLDLINE_SFTP_PORT`, `_USER`, `_PASSWORD` | `2222`, `infinitepay`, empty; compose password `sim-sftp-dev-only` | the SFTP login |
| `WORLDLINE_SFTP_PRIVATE_KEY_PATH` | empty | a client key, offered before the password |
| `WORLDLINE_SFTP_KNOWN_HOST_PATH` | empty; compose `/keys/wlsftp-keys/host_key.pub` | pins Worldline's host key |
| `WORLDLINE_SFTP_DIR` | `download` | the remote directory listed |
| `WORLDLINE_PGP_PRIVATE_KEY_PATH`, `_PUBLIC_KEY_PATH` | `/keys/wlsftp-keys/worldline_private.asc`, `…_public.asc` | the keypair that decrypts the files; waited for 30 s, after which it generates a pair of its own, which decrypts nothing Worldline sent |
| `WORLDLINE_PULL_INTERVAL` | `10s` | the background pull's period |
| `WORLDLINE_ARCHIVE_DIR` | `/data/worldline` | where each decrypted file is kept; a name already there is never pulled again, across restarts. A reset empties it of what the stand-in archived (`POST /sim/reset`) |
| `CUTOFF_TIME` | `00:00` | `HH:MM`, UTC on the lab clock: the daily internal-report batch for yesterday. Unparseable disables it |
| `B4B_URL` | `http://b4b:8086` | the payout rail |
| `B4B_JWT_PRIVATE_KEY_PATH` | `/keys/b4b-keys/private.pem` | signs the B4B JWT; waited for 30 s, then the service exits |
| `B4B_JWT_KEY_ID` | `b4b-mock-1` | the JWT's `kid` |
| `B4B_CALLBACK_URL` | `http://settlement:8083/internal/b4b-webhook` | sent as every payout's `callback_url` |
| `VERIFY_URL`, `VERIFY_INTERNAL_API_KEY` | `http://verify:8088`, `sim-verify-key-dev-only` | the merchant-verification gate |
| `BC_INTERNAL_URL` | `http://banking-circle:8095` | Banking Circle's bridge, for the balance gate only |
| `BC_SAFEGUARDING_ACCOUNT_ID_EUR`, `_GBP` | `00000000-0000-4000-8000-000000000978`, `…0826` | the safeguarding account checked per currency; another currency has none and fails its balance check |
| `BANK_URL` | `http://bank:8081` | the ledger the internal reports are built from |
| `SETTLEMENT_STATE_FILE` | `/data/settlements.json` | every record, rewritten on each change and loaded at start; a reset rewrites it empty |
| `SFTP_OUT_DIR`, `SFTP_STAGING_DIR`, `SFTP_OUTBOUND_DIR` | `/sftp/out`, `/sftp/staging`, `/sftp/outbound` | where internal reports are written and listed from |
| `WORLDLINE_SETTLEMENT_IDENTIFIER` | `Worldline_Settlement` | the `{identifier}` in Worldline's file names: how a reset recognises the archived settlement files it deletes |

## Service surface

The platform's own reports and reconciliation API, over its settlement
records. Errors are `{"error": "<message>", "status": <code>}` unless a
route says otherwise. Bodies are decoded strictly: an unknown field is a
`400`. Report amounts are integers in minor units.

**Records.** One settlement record per MID and currency per file, or per
IBAN and currency per internal batch, with id `set_<12 hex>`. States:
`Scheduled` → `Processing` → `Settled` or `Failed`; `Failed` → `Scheduled`
(retry); `Settled` → `Superseded` → `Scheduled` (reactivate). The full
record (`reason`, `replacement_id`, `created_at`, …) is only in the state
file; the routes serve its report, and its payout under Lab control.

**How a payout is made.** When a record from a Worldline morning file
settles, when a `settle` transition succeeds by hand, and on
`retry-payout`, settlement runs these steps synchronously and stores the
outcome as the record's `payout_state`:

1. Balance: `GET {BC_INTERNAL_URL}/internal/accounts/{sga}/balances`.
   `beginOfDayAmount` plus `intraDayAmount` below the record's
   `settlement_net_amount` stops at `awaiting_funding`. Any error,
   including a currency with no safeguarding account, stops at
   `balance_check_failed`: it fails closed.
2. Verification: `POST {VERIFY_URL}/api/v1/verification/verify/aml-decision`
   with `{"merchantApplicationId": <FNV-32a of the MID, as a signed
   32-bit integer made positive>}` and `x-internal-api-key`, then
   `GET …/verify/data/verify-decision/{id}` up to ten times, 50 ms apart.
   An `overallDecision` other than `APPROVE`, `APPROVED` or `PRE_APPROVE`
   stops at `verification_declined`. Unreachable, a non-200 or no decision
   counts as approved.
3. Beneficiary: `GET {B4B_URL}/oversight/v1/beneficiaries/{MID}` with the
   JWT. A non-200 stops at `submission_failed`; a `sanctions_status` other
   than `pass` stops at `sanctions_blocked`.
4. Payout: `POST {B4B_URL}/oversight/v1/payments`:

   ```json
   {"external_ref": "set_9c1e0a7b42d3", "beneficiary_id": "GB00SIM0000000000003",
    "company_id": "company_infinite", "callback_url": "http://settlement:8083/internal/b4b-webhook",
    "sca_applied": false, "amount": {"amount": 150.00, "currency": "EUR"},
    "currencyOfTransfer": "EUR",
    "debtorViban": {"account": "WORLDLINE_FUNDING_VIBAN_PLACEHOLDER", "country": "IE"},
    "creditorAccount": {"account": "<beneficiary account_number>", "financialInstitution": "<its financial_institution>", "country": "IE"},
    "creditorName": "<beneficiary account_name>", "chargeBearer": "SHA", "requestedExecutionDate": "2026-09-25"}
   ```

   `country` is `IE` for EUR and `GB` for GBP; the creditor fields come from
   the beneficiary record. A non-2xx, an unreachable B4B or an unreadable
   answer is `submission_failed`. B4B's `202` sets `payout_id` to its
   payment id and `payout_state` to its status, `B4BAccepted`; its
   callbacks move it on (`B4BSanctionsPending` … `B4BTMApproved`, or
   `B4BFailed`).

A record's settlement state never depends on its payout: a payout stuck in
a gate leaves the record `Settled`.

### `GET /reports/settlement`

Queries reports by transactions date.

Query:

- `from_date`, `to_date`: `YYYY-MM-DD`. Default seven days before today,
  and today, on the lab clock. The range may not exceed 90 days.
- `merchant_id`: exact match on the MID (or IBAN, for internal reports).
- `format`: `json` (default), `csv` or `xml`.

Answers `200`, JSON:

```json
{"data": [{"merchant_id": "GB00SIM0000000000003", "transactions_date": "2026-09-24", "currency": "EUR",
           "settlement_state": "Settled", "settlement_date": "2026-09-25",
           "approved_transaction_count": 2, "declined_transaction_count": 0,
           "sale_amount_total": 15000, "returned_amount_total": 0,
           "chargebacks_count": 0, "chargebacks_amount_total": 0,
           "discount_rate_fee_total": 0, "chargeback_fee_total": 0,
           "reserves_held": 0, "reserves_released": 0, "reserves_forward": 0,
           "settlement_net_amount": 15000}],
 "total": 1, "from_date": "2026-09-18", "to_date": "2026-09-25"}
```

Sorted by `transactions_date`, then record id. Each item is a record's
report with its live `settlement_state` and `settlement_date`; it carries
no record id and no payout. Find an id through `GET /worldline/files`
(`settlement_ids`) or `generate`'s `record_ids`.

`csv` is `text/csv`: a header row with the same snake_case names, then a
row per report. `xml` is `application/xml`: a `<settlementReport>` of
`<report>` elements with camelCase children (`<merchantId>`, …).

- `400`: `invalid from_date "…"`, `invalid to_date "…"`, `to_date before
  from_date`, `date range exceeds 90 days`, `unsupported format …`.
- `500`: a CSV or XML rendering error.

### `GET /reports/settlement/{id}`

One record's report.

Path: `id`, `set_…`. Query: `format`, as above; XML is a
`<settlementReport>` holding one `<report>`.

Answers `200` with one report object, as an item of `data` above. A
record created but not yet processed has zero counts and amounts.

- `400`: `unsupported format …`.
- `404`: `settlement: record set_… not found`.

### `POST /reports/settlement/generate`

Builds the platform's own internal reports from the business bank's
ledger. It pays nobody: payouts come only from Worldline's morning file.

Body, optional (an empty body takes every default):

```json
{"from_date": "2026-09-24", "to_date": "2026-09-24", "merchant_id": "GB00SIM0000000000003"}
```

Both dates default to yesterday on the lab clock and are not validated.
`merchant_id` filters by the IBAN of the account credited.

Answers `202`, although the work is done before it answers:

```json
{"message": "generation started", "records_created": 1, "record_ids": ["set_9c1e0a7b42d3"]}
```

- `400`: malformed JSON or an unknown field.
- `500`: `settlement: fetch accounts: …` or `settlement: fetch ledger: …`,
  when the bank cannot be read.

Side effects: reads the bank's `GET /accounts` and `GET /ledger`, and
groups the credit legs dated in the range (the UTC date of `at`) by
account IBAN and currency. Each group becomes a record, driven `Scheduled`
→ `Processing` → `Settled` (or `Failed`), whose report counts the credits
and sums them as both sale total and net amount; `transactions_date` is
`to_date`, `settlement_date` the day after. Each report is written as
CSV, XML and JSON to `{SFTP_OUT_DIR}/{iban}/{iban}_{to_date}_{currency}_WX.{csv,xml,json}`
and again to `{SFTP_OUTBOUND_DIR}/{iban}/daily/`, which Worldline's SFT
channel serves (`GET :8084/outbound/{merchant_id}/daily`). Not
idempotent: a second run over the same day creates second records.

### `POST /reports/settlement/{id}/transition`

Moves one record through the state machine by hand.

Path: `id`, `set_…`.

Body:

```json
{"event": "fail", "reason": "manual review", "replacement_id": ""}
```

`event` is one of `process` (`Scheduled` → `Processing`), `settle`
(`Processing` → `Settled`, needs a report), `fail` (`Processing` →
`Failed`, needs a reason), `retry` (`Failed` → `Scheduled`), `supersede`
(`Settled` → `Superseded`, needs a replacement id) or `reactivate`
(`Superseded` → `Scheduled`). The event's argument is `reason`, or
`replacement_id` when `reason` is empty, so a `supersede` that sends both
records the reason as the replacement id.

Answers `200`:

```json
{"record_id": "set_9c1e0a7b42d3", "from_state": "Processing", "to_state": "Settled", "success": true}
```

- `400`: `event required`, malformed JSON or an unknown field (error
  shape).
- `404` and `409` answer in the result shape, not the error shape:

  ```json
  {"record_id": "set_9c1e0a7b42d3", "from_state": "Settled", "to_state": "Processing",
   "success": false, "error": "settlement: invalid transition from Settled to Processing"}
  ```

  `404` for an unknown record; `409` for a transition the state does not
  allow, an unknown event (empty `to_state`), `settle` without a report,
  `fail` without a reason or `supersede` without a replacement id.

Side effects: the record is saved. A successful `settle` runs the payout
path above before answering.

### `GET /sftp/out`

Every file under `SFTP_OUT_DIR`, recursively, newest first.

Answers `200`:

```json
{"files": [{"path": "GB00SIM0000000000003/GB00SIM0000000000003_2026-09-24_EUR_WX.csv",
            "size": 412, "modTime": "2026-09-25T00:00:03Z"}]}
```

`path` is `{merchant_id}/{name}` when the name follows the
`{merchant}_{date}_{CCY}_WX.{ext}` pattern, otherwise the bare name.
`modTime` is the file's own time. An absent directory lists `[]`.

- `500`: a file-system error.

### `GET /sftp/out/{merchant_id}`

The files in one merchant's directory, newest first.

Answers `200` in the same shape, `[]` for a merchant with no directory.

- `400`: a `merchant_id` that is empty, `.`, `..` or contains a path
  separator.
- `500`: a file-system error.

### `GET /sftp/out/{merchant_id}/{filename}`

One file's bytes.

Answers `200` with `Content-Type` `text/csv` for `.csv`, `application/xml`
for `.xml`, otherwise `application/json`.

- `400`: either segment empty, `.`, `..` or containing a separator.
- `404`: the file-system error, for a file that is not there.

### `GET /sftp/staging`

Lists `SFTP_STAGING_DIR` the same way as `GET /sftp/out`. Nothing in this
service writes there, so it is `[]` unless something else puts files in
it.

## Lab plumbing

### `POST /internal/b4b-webhook`

B4B's payment-status callback: the `callback_url` settlement sends with
every payout. B4B calls it on every regulatory-phase change, unsigned,
unordered and possibly repeated.

Body (unknown fields are a `400`):

```json
{"id": "b4bp_5d0c2a91", "status": "B4BTMApproved", "payload": {"…": "…"},
 "banking_circle_api_response": {"paymentId": "bcp_7e21c0d4aa19"}}
```

Only `id` and `status` are used.

Answers `200` with `{"status": "ok"}` when a record has that `payout_id`,
whose `payout_state` becomes `status`; the last callback to arrive wins,
whatever its order. `200` with `{"status": "ignored"}` when no record has
it: a callback that beat the `202` carrying the id, or a payment someone
else addressed here.

- `400`: `id is required`, malformed JSON or an unknown field.
- `500`: only if the record disappears mid-call.

## Lab control

The `/sim/*` routes are on the control port, `:9083` ([the control
contract](../README.md#the-control-contract)). The same `/sim/*` paths
still answer on `:8083` until phase 4 of
[the plan](../../lab-api/plan.md) retires them. `GET /worldline/files`,
`POST /worldline/pull` and the two payout routes stay on `:8083`, where the
harness and a person call them.

### `GET /health`

Liveness, on both ports. `:8083` answers `{"status": "ok", "service":
"settlement"}`; the control port answers `{"status": "ok", "service":
"settlement", "surface": "control"}`, and that is the one the lab API's
monitor probes.

### `GET /sim/info`

Who this is and what it can do, as every service answers it
([the control contract](../README.md#the-control-contract)):
`"service": "settlement"`, `"kind": "stand-in"`,
`"vendor_listeners": [":8083"]`, `"control_listener": ":9083"`,
`"reset_scopes": ["flow", "all"]`.

### `GET /sim/logs`

The process's own log lines, as every service answers it
([the control contract](../README.md#the-control-contract)): every pull,
record, payout gate and request, on either port, is a line here.

### `POST /sim/reset`

Puts the stand-in back to a known state. Synchronous; the body is optional
(`flow`). Both scopes do the same: the stand-in has nothing a platform set
up once.

- **Clears**: every settlement record, and the state file, rewritten empty
  (`settlements`); the internal report files (`report_files`); the archive
  of Worldline files it took delivery of (`archived_files`); the pull
  history of `GET /worldline/files` and the names it has seen
  (`pulled_files`).
- **Keeps**: the switch (`/sim/connected`): it is live wiring, not run
  state.

```json
{"service": "settlement", "scope": "flow",
 "cleared": {"settlements": 2, "report_files": 6, "archived_files": 1, "pulled_files": 1},
 "kept": ["connected"], "at": "2026-11-07T12:00:00Z"}
```

**Files.** The folders are shared with Worldline's SFT channel and the
host, so a reset deletes only a regular file it can prove it wrote, by its
name in the folder it writes it to. Never a folder, a symlink, or anything
else:

- `{SFTP_OUT_DIR}/{merchant_id}/{merchant_id}_{YYYY-MM-DD}_{CCY}_WX.{csv|xml|json}`.
- `{SFTP_OUTBOUND_DIR}/{merchant_id}/daily/{merchant_id}_{YYYY-MM-DD}_{CCY}_WX.{csv|xml|json}`;
  a report Worldline already archived on download is Worldline's to
  delete.
- `{WORLDLINE_ARCHIVE_DIR}/{14 digits}_{WORLDLINE_SETTLEMENT_IDENTIFIER}_{ER|AR}_{CCY}.csv.pgp`,
  and any other name the pull history says was archived there (a fixture
  file's name). A fixture archived before the last restart is left.

A report whose name does not follow the pattern (a `generate` with a date
that is not `YYYY-MM-DD`) is left, and so is anything in
`SFTP_STAGING_DIR`, which the stand-in never writes.

**Not taken again.** The archive was what said a file had been taken. So
that a file still on Worldline's server is not taken and paid a second
time, the puller ignores any file there whose modification time is before
the reset. A file Worldline publishes after it, even under the same name,
is taken. Reset Worldline as well (the lab reset does, first) to start
from no files at all.

**Work in flight** is dropped, not raced: a pull, the daily cutoff batch, a
payout and a retried payout each stop at their next step after the reset,
so no file is archived or written, no record made and no payout sent or
stored after it. A payment already on its way to B4B when the reset
arrives is waited for, then the reset clears its record. The timers keep
running.

`500` with the reason when a file cannot be deleted or the state file
cannot be written; everything else is still done, and a second reset
finishes the job.

### `GET /sim/connected`

The stand-in's lab switch (`internal/labswitch`): whether its own timers
run.

Answers `200`:

```json
{"connected": true, "what": "pulls Worldline's files on a timer and pays outlets at the daily cutoff"}
```

`reason` and `changed_at` appear after the first `POST`. There is no
`target`: the stand-in delivers to no fixed listener. `what` says it pays
at the cutoff; the cutoff batch pays nobody, and the payouts come from the
pull.

### `POST /sim/connected`

Connects or disconnects the stand-in.

Body:

```json
{"connected": false, "reason": "a platform is plugged in"}
```

`reason` defaults to `set over /sim/connected`.

Answers `200` with the new state, as `GET`.

- `400`: `want {"connected": true|false}`, when `connected` is missing or
  the body is not JSON.

Side effects: disconnected, the background Worldline pull stops and the
daily cutoff batch is skipped. Anything asked for explicitly still works:
`POST /worldline/pull` or `POST /sim/pull` and the payouts it makes,
`generate`, `transition`, `retry-payout` and B4B's callbacks. In memory: a
restart is back on `SETTLEMENT_CONNECTED`; a reset keeps it as it is.

### `POST /sim/pull`

[`POST /worldline/pull`](#post-worldlinepull) on the control port: the same
handler, answering the same, so the lab forces a pull without calling the
stand-in's own routes. Registered only when `WORLDLINE_SFTP_HOST` is set.
Works while disconnected.

### `GET /worldline/files`

What the puller has taken delivery of, oldest first. Registered only when
`WORLDLINE_SFTP_HOST` is set.

Answers `200`:

```json
{"count": 1, "files": [
  {"name": "20260925083000_Worldline_Settlement_ER_EUR.csv.pgp", "slot": "ER",
   "pulled_at": "2026-09-25T08:30:04Z", "bytes": 2210,
   "archived_at": "/data/worldline/20260925083000_Worldline_Settlement_ER_EUR.csv.pgp",
   "processed": true, "settlement_ids": ["set_9c1e0a7b42d3", "set_04ad1f9e7c20"], "payouts_by_mid": 2}]}
```

- `slot` is `ER` (the morning file, which is processed and pays), `AR`
  (the afternoon confirmation, archived and never processed) or `""` (not
  recognised, archived only).
- `archived_at` is the archive path of the decrypted bytes, not a time;
  empty if archiving failed.
- `processed` is true only for a morning file that parsed.
  `settlement_ids` and `payouts_by_mid` are omitted when empty; `error`
  says why a download, decryption or parse failed.
- The history is in memory: empty after a restart, although archived files
  are still never pulled again. A reset empties it too.

### `POST /worldline/pull`

Pulls from Worldline now instead of waiting for the next tick. Registered
only when `WORLDLINE_SFTP_HOST` is set. Works while disconnected.

Answers `200`, once the pull and every payout it triggers are done:

```json
{"pulled": 1, "files": [{"name": "20260925083000_Worldline_Settlement_ER_EUR.csv.pgp", "…": "…"}]}
```

`pulled` counts files taken this time; `files` is the whole history, as
`GET /worldline/files`. A file that failed to download or parse is still
a `200`, with that file's `error`.

- `502`: `pull from worldline:2222 failed: …`, when connecting or listing
  fails.

Side effects: one pull runs at a time, so this waits for a background
pull in progress, and each file is claimed before it is downloaded, so no
file is processed twice. Each new file is downloaded, decrypted and
archived. For a morning file, each MID with a positive total becomes a
record (`transactions_date` the file's value date, `settlement_date` the
day after; report counts the MID's rows and its total as sale and net
amount, fees zero), driven to `Settled`, then the payout path above runs
for it.

### `GET /reports/settlement/{id}/payout`

The payout a record submitted, which the reports deliberately leave out.

Answers `200`:

```json
{"payout_id": "b4bp_5d0c2a91", "payout_state": "B4BTMApproved"}
```

Both are `""` before any attempt; records from `generate` never get one.
`payout_id` is `""` while the payout is stopped in a gate.

- `404`: `settlement: record set_… not found`.

### `POST /reports/settlement/{id}/retry-payout`

Runs the payout path again for a record stuck in a gate, typically after
funding the safeguarding account (Banking Circle's
`POST :8095/internal/incoming-payments`). No body.

Answers `200` with the outcome, as `GET …/payout`. It may land in a gate
again.

- `400`: `retry-payout requires a Settled record, got state=Processing`;
  `payout already in flight or completed: payout_state=B4BAccepted`. Only
  `awaiting_funding`, `verification_declined`, `submission_failed`,
  `balance_check_failed` and `sanctions_blocked` may be retried, so a
  record with no payout attempt (`""`) is refused too.
- `404`: `settlement record not found`.

## Called by

- **b4b**: `POST /internal/b4b-webhook` on every status change of a payout
  settlement submitted (`callback_url` from `B4B_CALLBACK_URL`), and of
  the harness's own gate-test payments, which name this URL too and are
  answered `ignored`.
- **lab API**, on the control port only (`http://settlement:9083`), never
  the stand-in's own routes: `POST /v1/actions/settlement-pull` calls
  `POST /sim/pull` and passes its answer through;
  `POST /v1/lab/reset` and `POST /v1/services/settlement/reset` call
  `POST /sim/reset`; `GET /v1/logs` and `GET /v1/services/settlement/logs`
  read `GET /sim/logs`; `GET /v1/stand-ins`
  reads `GET /sim/connected` and `POST /v1/stand-ins/settlement` sets it
  with `POST /sim/connected`; `GET /v1/lab` and
  `POST /v1/services/settlement/probe` probe the control port's
  `GET /health`.
  `GET /v1/services/settlement/activity` answers `501` without calling
  it.
- **harness**: `worldline-settlement-to-sftp` posts `/worldline/pull` until
  the published file is processed, then reads
  `GET /reports/settlement/{id}`; `worldline-confirmation-file` reads
  `GET /worldline/files` and pulls again, expecting the afternoon file to
  add nothing processed; `banking-circle-payout` reads
  `GET /reports/settlement/{id}/payout`, funds the safeguarding account and
  posts `retry-payout` when it is `awaiting_funding`, then polls the payout
  to `B4BTMApproved`; `b4b-oversight-gates` names `/internal/b4b-webhook`
  as its payments' `callback_url`.
- **by hand**: [settlement-to-payout.md](../../scenarios/settlement-to-payout.md)
  uses `POST /worldline/pull`, `GET /worldline/files`,
  `GET /reports/settlement/{id}`, `GET …/payout` and `POST …/retry-payout`.
  Nothing in the repo calls `GET /reports/settlement`, `generate`,
  `transition` or the `/sftp/*` routes; they are there to be called by
  hand.
- **the platform**: never. It replaces this service, and asks for it to be
  disconnected through its descriptor's `stand_ins`.

Routes checked against the source at commit db5e695.
