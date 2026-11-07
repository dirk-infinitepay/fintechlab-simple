# Worldline — API

Worldline is the acquirer, on the vendor side of the line
([catalogue](../../catalogue.md)). It holds the card transactions it
acquired per submerchant (MID), cuts the daily Bambora settlement file from
them in a morning (`ER`) and an afternoon (`AR`) slot, publishes each file
PGP-encrypted on a real SSH/SFTP server, wires the morning slot's total to
the platform's safeguarding account at Banking Circle, and answers
submerchant enrolment batches with receipts. Real-protocol-accurate: the
Bambora multi-section CSV, the filename pattern and its two slots, SSH/SFTP
with password or key auth and a pinnable host key, and OpenPGP
encrypt-and-sign. Simplified: no Acquiring REST API, no chargebacks (the
`CB` section is always empty), any SFTP username is accepted, and the SFT
file-exchange channel for onboarding documents and WX/financial reports is
served over plain HTTP rather than SFTP.

## At a glance

| | |
| --- | --- |
| Listens | HTTP `:8084` (`LISTEN`) and SSH/SFTP `:2222` (`WORLDLINE_SFTP_PORT`), both in one process. Compose service `worldline`; from the host `http://127.0.0.1:8084` and `sftp://127.0.0.1:2222` |
| Auth | HTTP: none, on every route. SFTP: password (`WORLDLINE_SFTP_PASSWORD`, compose `sim-sftp-dev-only`) and public key (`WORLDLINE_SFTP_AUTHORIZED_KEY`), each enabled independently. The username is never checked. With both unset, any credential is accepted (logged as a warning at start) |
| A platform points here with | The settlement stand-in (`compose.yml`): `WORLDLINE_SFTP_HOST`, `WORLDLINE_SFTP_PORT`, `WORLDLINE_SFTP_USER`, `WORLDLINE_SFTP_PASSWORD` or `WORLDLINE_SFTP_PRIVATE_KEY_PATH`, `WORLDLINE_SFTP_KNOWN_HOST_PATH`, `WORLDLINE_PGP_PRIVATE_KEY_PATH`, `WORLDLINE_SFTP_DIR` (default `download`). The console's connect kit (`GET :8090/api/connect/env/worldline`) hands a platform `WORLDLINE_SFTP_HOST`, `_PORT`, `_USERNAME`, `_PASSWORD`, `_HOST_KEY_FINGERPRINT`, `_DOWNLOAD_DIR`, `_RECONCILIATION_DIR`, `_DISPUTES_DIR`, `_UPLOAD_DIR`, `WORLDLINE_SETTLEMENT_IDENTIFIER`, `WORLDLINE_PGP_PUBLIC_KEY` and `INFINITE_PGP_PRIVATE_KEY`. The HTTP side is found with `WORLDLINE_URL` (harness) and `CONSOLE_URL_WORLDLINE` (console) |
| Activity logs | `GET /sim/activity` keeps one log, `sftp` (*File exchange*): `sftp.session` for each SSH login or refused handshake, then one event per SFTP request (`sftp.list`, `sftp.download`, `sftp.upload`, and the jail's other verbs, such as `sftp.rename`, `sftp.remove`, `sftp.mkdir`), each with `user` and `path`; a refused request is amber with the server's error. Stats are not logged. Each file taken over `GET /outbound/{merchant_id}/{category}/{filename}` lands in the same log as `http.download` |
| Configuration | The important variables are in the table below. Slot times are read in the container's local zone; the image sets no `TZ`, so that is UTC |
| Design record | [ARCHITECTURE-worldline-acquirer.md](../../ARCHITECTURE-worldline-acquirer.md) (who owns the file, the two slots, one file per currency), [ARCHITECTURE-vendor-corrections.md](../../ARCHITECTURE-vendor-corrections.md) §1–2 and Addendum E (Bambora format, SFTP+PGP, encryption direction), [ARCHITECTURE-worldline-sftp-channel.md](../../ARCHITECTURE-worldline-sftp-channel.md) (historical; its SFT directory conventions still hold), [structure/worldline.md](../../structure/worldline.md) |

| Variable | Default | Effect |
| --- | --- | --- |
| `LAB_CLOCK_URL` | unset (compose `http://clock:8096/clock`) | Follow the lab clock: slot fire times, coverage dates, filename timestamps, receipt times and the archive folder |
| `SFTP_BASE_DIR` | `/sftp` | Root of the HTTP SFT channel (`inbound/`, `outbound/`, `config/`, `archive/`); compose bind-mounts `./sftp/*` |
| `MERCHANT_ID` | `GB00SIM0000000000003` | Merchant id in the channel config seeded at start when none exists |
| `WORLDLINE_SFTP_ROOT` | `/wlsftp` | The SFTP server's root. Not a bind mount: it lives and dies with the container |
| `WORLDLINE_SFTP_PASSWORD` | unset | Enables password auth |
| `WORLDLINE_SFTP_AUTHORIZED_KEY` | unset | Enables public-key auth; one or more `authorized_keys` lines. Unparsable is fatal at start |
| `WORLDLINE_SFTP_HOST_KEY_PATH` | `/keys/wlsftp-keys/host_key` | ed25519 host key, generated on first start |
| `WORLDLINE_SFTP_HOST_KEY_PUB_PATH` | host key path + `.pub` | Where the host key's public half is written, for pinning |
| `WORLDLINE_PGP_PUBLIC_KEY_PATH`, `WORLDLINE_PGP_PRIVATE_KEY_PATH` | `/keys/wlsftp-keys/worldline_public.asc`, `/keys/wlsftp-keys/worldline_private.asc` | mock-Worldline's RSA-3072 OpenPGP keypair, generated if either file is missing |
| `INFINITE_PGP_PUBLIC_KEY_PATH` | unset | The platform's public key. Set, files and receipts are encrypted to it; unset, to Worldline's own key |
| `WORLDLINE_FILE_SOURCE` | `generate` | `generate` cuts files from the held transactions; `fixture` serves the files in `WORLDLINE_FIXTURE_DIR` whose name contains `_ER_` or `_AR_`, byte for byte. Anything else is fatal at start |
| `WORLDLINE_FIXTURE_DIR` | `/wlsftp-fixtures` | Fixture files (compose mounts `./worldline/fixtures`) |
| `WORLDLINE_SETTLEMENT_IDENTIFIER` | `Worldline_Settlement` | The `{identifier}` in the filename |
| `WORLDLINE_SETTLEMENT_ACCOUNT` | empty (compose `GB00SIM0000000000001`) | `TO_ACCOUNT` in the file's Settlement row |
| `WORLDLINE_MORNING_FILE_AT`, `WORLDLINE_MORNING_FILE_WINDOW` | `08:00`, `2h` | The morning slot fires at that time plus a per-day jitter inside the window, deterministic for a given date |
| `WORLDLINE_AFTERNOON_FILE_AT` | `15:30` | The afternoon slot, no jitter |
| `WORLDLINE_SETTLEMENT_DELAY` | `24h` | A scheduled run settles the single day this long before now (T+1) |
| `BC_INTERNAL_URL` | empty (compose `http://banking-circle:8095`) | Banking Circle's bridge, for the lump sum. Empty: lump sums report `not configured` |
| `BC_SAFEGUARDING_ACCOUNT_ID_EUR`, `BC_SAFEGUARDING_ACCOUNT_ID_GBP` | empty | A currency's lump sum is wired only when its account is set; no other currency is read |
| `WORLDLINE_ENROLMENT_DELAY` | `30s` | How old an enrolment batch must be before it is answered, by file modification time on the wall clock |
| `WORLDLINE_ENROLMENT_SWEEP` | `5s` | How often `to_WLNORDIC/` is checked for batches |

## Vendor surface

What a real platform calls: the SFTP server on `:2222`, shaped like a real
Worldline SFT account. The HTTP SFT channel routes below render Worldline's
onboarding and report directories over plain HTTP: the directory layout
and file names are the vendor's, the transport is the lab's (real Worldline
moves these files by SFTP).

**SFTP: connection.** SSH on `:2222`, one SFTP subsystem per session. Only
`session` channels asking for `subsystem sftp` are served: no shell, exec
or forwarding. The host key is ed25519, its public half published at
`WORLDLINE_SFTP_HOST_KEY_PUB_PATH` (compose: `keys/wlsftp-keys/host_key.pub`)
so a client can pin it. Any username is accepted; it is recorded in the
activity log. The settlement stand-in logs in as `infinitepay`, the harness
as `harness`.

**SFTP: what a session may do.** Every path is jailed under
`WORLDLINE_SFTP_ROOT`: absolute paths are jail-absolute, `..` cannot leave
the root, creating a symlink is refused and so is following one out of the
root. Inside the jail a session can list, read, write, rename, remove,
mkdir, truncate and hard-link anything. There are no per-directory
permissions, so a client can delete a settlement file.

**SFTP: directories.**

| Directory | Holds | Written by |
| --- | --- | --- |
| `download/` | Every settlement file | The settlement cycle |
| `to_WLNORDIC/` | A second copy of every settlement file, and the platform's enrolment batches | The cycle; the platform |
| `from_WLNORDIC/` | Enrolment receipts | The enrolment desk |
| `disputes/download/` | Nothing: created at start, never written | — |

`download/` and `to_WLNORDIC/` are the two real path conventions for the
same content. The server never deletes from either, so a client keeps
track of what it has already taken.

**Settlement files.** Named
`{YYYYMMDDHHMMSS}_{identifier}_{ER|AR}_{CCY}.csv.pgp`, for example
`20260925083412_Worldline_Settlement_ER_EUR.csv.pgp`. The timestamp is the
delivery time on the lab clock, in UTC. `ER` is the morning file a platform
processes; `AR` is the afternoon confirmation of the same settlement. There
is one file per currency, covering every MID that traded on the coverage
day, with no merchant in the name. The plaintext is the Bambora `V2.10`
always-quoted multi-section CSV: a `Settlement` row (`VERSION_NUMBER`,
`RECORD_TYPE`, `SETTLEMENT_AMOUNT`, `SETTLEMENT_CURRENCY`, `VALUE_DATE`,
`NUMBER_OF_ITEMS`, `TO_ACCOUNT`, `PAYMENT_REFERENCE`); per batch a `Batch`
row followed by its `TXER` rows; then a `CB` header with no rows. Each
section carries its own header line. Amounts are major units with two
decimals. The merchant key of a `TXER` row is `ADDITIONAL_REF_2`, not
`BAMBORA_MID` or `SUBMERCHANT_ID`. In `fixture` mode the fixture's own name
is kept and `.pgp` appended.

**PGP.** Binary (not armored) OpenPGP. Every file and receipt is encrypted
to `INFINITE_PGP_PUBLIC_KEY_PATH` when it is set, otherwise to Worldline's
own public key, and always signed with Worldline's own private key. In
compose nothing is set, so the platform decrypts with Worldline's private
key (`worldline_private.asc`, handed out as `INFINITE_PGP_PRIVATE_KEY`).
Generated key files are written mode 0644: lab material only.

**Enrolment.** The platform uploads `INFINITEENROLL_<NN>.<YYYYMMDD>.pgp`
(`NN` 01–99, a valid date) to `to_WLNORDIC/`. The plaintext is XML,
`<enrollment><submerchant><pf_account_id/><legal_name/><dba_name/><proxy_mid/></submerchant>…</enrollment>`,
ISO-8859-1 or UTF-8. Once the batch is `WORLDLINE_ENROLMENT_DELAY` old (or
on `POST /sim/enrolment/process`) the desk writes
`from_WLNORDIC/INFINITEENROLL_<NN>.<YYYYMMDD>.ack.pgp`, through a
`.partial` file and a rename:

```xml
<?xml version="1.0" encoding="ISO-8859-1"?>
<enrollment_receipt>
  <summary><fileid>01</fileid><filedate>20260925</filedate><filetime>091500</filetime>
    <approved_count>1</approved_count><rejected_count>0</rejected_count></summary>
  <approvals><approval><pf_account_id>pf-1001</pf_account_id>
    <proxy_mid>60010427</proxy_mid><virtual_account_id>31840562</virtual_account_id></approval></approvals>
</enrollment_receipt>
```

Every submerchant is approved unless a rejection was armed for its
`pf_account_id`; `proxy_mid` (`6001` + four digits) and
`virtual_account_id` are derived from `pf_account_id`, so the same batch
always boards to the same MIDs. A submerchant with no `pf_account_id` is
left out. Lab leniency: a batch that is plain XML (not encrypted) is
accepted. A batch that does not decrypt or parse, including one signed by
a key the lab does not hold, gets no receipt; it is logged and counted as
answered so it is not retried. A batch is answered once per process.

### `POST /inbound/onboarding/{merchant_id}`

Drops an onboarding document into the merchant's inbound directory.

- Query: `filename` (required). Extensions `.pdf`, `.csv`, `.xml`.
- Body: the raw file bytes, no multipart. At most 10 MiB.
- Answers `201` `{"status": "stored", "id": "up_5e1f0a2b9c3d", "path": "/sftp/inbound/onboarding/GB00SIM0000000000003/kyc.pdf"}`.
  `400` `{"error": "…", "status": 400}` for a missing `filename`, an
  unsupported extension, a merchant id or filename containing `/` or `\`
  (or `.`, `..`), or a file over the limit.
- Side effects: writes `{SFTP_BASE_DIR}/inbound/onboarding/{merchant_id}/{filename}`,
  replacing a file of the same name. Nothing reads it.

### `GET /inbound/onboarding/{merchant_id}`

Lists the merchant's onboarding documents.

- Answers `200` `{"merchant_id": "GB00SIM0000000000003", "category": "onboarding", "files": [{"name": "kyc.pdf", "size": 48211, "modified_at": "2026-09-25T09:00:00Z"}]}`;
  an empty `files` when nothing was dropped. `400` for an invalid merchant id.

### `POST /inbound/corrections/{merchant_id}`

Drops a settlement correction file. As `POST /inbound/onboarding/{merchant_id}`,
except that only `.csv` and `.xml` are accepted and the file lands in
`inbound/corrections/{merchant_id}/`.

### `GET /inbound/corrections/{merchant_id}`

Lists the merchant's correction files. Same answer shape as
`GET /inbound/onboarding/{merchant_id}`, with `"category": "corrections"`.

### `GET /outbound/{merchant_id}/daily`

Lists the WX reports waiting in `outbound/{merchant_id}/daily/`. The
settlement stand-in writes them there over a shared bind mount, named
`{merchant_id}_{date}_{currency}_WX.{csv|xml|json}`.

- Answers `200` `{"merchant_id": "…", "category": "daily", "files": [{"name": "GB00SIM0000000000003_2026-09-24_EUR_WX.csv", "size": 1873, "modified_at": "…"}]}`.
  `400` for an invalid merchant id.

### `GET /outbound/{merchant_id}/financial`

Lists `outbound/{merchant_id}/financial/`. Same shape as the daily list,
with `"category": "financial"`. Nothing in the lab writes financial
reports, so it is empty unless a file is put there by hand.

### `GET /outbound/{merchant_id}/{category}/{filename}`

Downloads one outbound file and archives it.

- Path: `category` is `daily` or `financial`.
- Answers `200` with the raw bytes, `Content-Type: application/octet-stream`
  and `Content-Disposition: attachment; filename="…"`. `404`
  `{"error": "file not found", "status": 404}`; `400` for any other
  category, an invalid merchant id or a path that escapes the merchant's
  directory.
- Side effects: moves the file to `{SFTP_BASE_DIR}/archive/{YYYY}/{MM}/`
  (year and month on the lab clock), so a second download is a `404`.
  Records an `http.download` event in the `sftp` activity log.

### `GET /archive/{year}/{month}`

Lists files already retrieved in that month.

- Path: `year` four digits, `month` two (`2026`, `09`), as the archive
  folders are named. Neither is validated: they are joined onto the archive
  path as given.
- Answers `200` `{"year": "2026", "month": "09", "files": [{"name": "…", "size": 1873, "modified_at": "…"}]}`;
  an empty `files` for a month with nothing in it.

### `GET /config`

The SFT channel's configuration, `{SFTP_BASE_DIR}/config/merchant-onboarding.json`.

- Answers `200` `{"merchant_id": "GB00SIM0000000000003", "sftp_directory": "/sftp/outbound/GB00SIM0000000000003", "report_types": ["WX", "Financial"], "report_formats": ["CSV", "XML", "ASCII"], "cut_off_time": "00:00 CET", "remittance_frequency": "daily", "sftp_directory_count": 3}`.
  `404` `{"error": "config not found", "status": 404}` if the file is gone
  (one is seeded at start when none exists).
- The configuration is descriptive: nothing in the service reads it back.

### `PUT /config`

Replaces the channel configuration.

- Body: the whole object, as `GET /config` returns it. Decoded strictly:
  an unknown field is a `400`.
- Answers `200` with the stored object. `400` when `merchant_id` is empty
  or the body does not decode; `500` when the file cannot be written. No
  other field is validated (`sftp_directory_count` is not capped at 3).

## Lab control

Not part of Worldline. Everything under `/sim` is how a test, agent or the
console says "a card payment happened" or "deliver the file now".

### `GET /health`

Liveness. Answers `200` `{"status": "ok", "service": "worldline"}`. The
SFTP listener is not checked.

### `GET /sim/activity`

The `sftp` activity log described under *At a glance*.

- Query: `limit` (default 100; the ring keeps 256).
- Answers `200` `{"logs": [{"name": "sftp", "title": "File exchange", "note": "…", "total": 9, "kept": 9, "last": {…}, "events": [{"seq": 9, "at": "2026-09-25T09:00:04Z", "op": "sftp.download", "peer": "10.89.0.7", "summary": "infinitepay collected 20260925083412_Worldline_Settlement_ER_EUR.csv.pgp", "status": "ok", "detail": {"user": "infinitepay", "path": "/download/20260925083412_Worldline_Settlement_ER_EUR.csv.pgp"}}]}]}`.

### `POST /sim/transactions`

Seeds card transactions the acquirer has acquired.

- Body: one transaction or an array of them:

  ```json
  [{"mid": "GB00SIM0000000000003", "currency": "EUR", "amount_cents": 10000, "date": "2026-09-24",
    "card_scheme_name": "Visa", "mcc": "5812", "terminal_id": "T0001"}]
  ```

  Required: `mid`, `currency`, `amount_cents` (non-zero; a negative amount
  is a refund) and `date` (`YYYY-MM-DD`, the transaction date). Optional:
  `id` (else `wltxn_000001`, …), `type`, `card_scheme_name`, `card_usage`,
  `card_category`, `mcc`, `country_merchant`, `country_issuer`,
  `terminal_id`; anything left empty is synthesized deterministically when
  the file is cut. Unknown fields are ignored.
- Answers `201` `{"accepted": 1, "transactions": [{"id": "wltxn_000001", "mid": "GB00SIM0000000000003", "currency": "EUR", "amount_cents": 10000, "date": "2026-09-24", "card_scheme_name": "Visa", "mcc": "5812", "terminal_id": "T0001"}]}`,
  currency upper-cased. `400` `{"error": "mid is required", "status": 400}`
  (or the failing rule) for an invalid transaction. Transactions earlier in
  the array than the invalid one are already stored.
- Side effects: in memory only; a restart forgets them. Nothing is settled
  until a slot runs.

### `GET /sim/transactions`

What the acquirer holds.

- Query: `mid` (optional); `from` and `to` (`YYYY-MM-DD`, inclusive) narrow
  by transaction date, and only apply together with `mid`.
- Answers `200` `{"count": 2, "transactions": [{…}]}`, `transactions` in
  the order they were added.

### `POST /sim/settlement-cycle/run`

Runs one slot now, exactly as the schedule would.

- Query: `slot` — `morning`, `er` or empty for `ER`; `afternoon`,
  `confirmation` or `ar` for `AR`. `date` (`YYYY-MM-DD`, optional) settles
  that single day instead of the day `WORLDLINE_SETTLEMENT_DELAY` before
  now. The file is delivered now either way.
- Answers `200` with the run:

  ```json
  {"at": "2026-09-25T09:00:00Z", "slot": "ER", "from_date": "2026-09-24", "to_date": "2026-09-24",
   "files": [{"filename": "20260925090000_Worldline_Settlement_ER_EUR.csv", "slot": "ER",
              "mids": ["GB00SIM0000000000003"], "currency": "EUR", "amount_cents": 15000, "items": 2,
              "from_date": "2026-09-24", "to_date": "2026-09-24"}],
   "lump_sums": [{"currency": "EUR", "amount_cents": 15000, "reference": "WORLDLINE SETTLEMENT EUR 2026-09-24",
                  "account_id": "00000000-0000-4000-8000-000000000978", "status": "credited"}],
   "published": ["20260925090000_Worldline_Settlement_ER_EUR.csv.pgp"]}
  ```

  `files`, `lump_sums` and `published` are `null` when empty; with no
  transactions on the day there is no file. `207` with the same body plus
  `"errors": ["…"]` when the cut or a publish failed. `400` for an unknown
  `slot` or a malformed `date`.
- Side effects: publishes each file to `download/` and `to_WLNORDIC/`.
  Morning slot only: for each currency with a positive total, posts
  `{"currency": "EUR", "amount": "150.00", "reference": "WORLDLINE SETTLEMENT EUR 2026-09-24"}`
  to Banking Circle's `POST {BC_INTERNAL_URL}/internal/incoming-payments`.
  A lump sum's `status` is `credited`, `not configured` (no bridge URL or
  no account for the currency) or `error: …`; a failed wire never stops the
  file. In `fixture` mode the cut files carry no currency or amount, so no
  lump sum is wired. Two runs of the same slot in the same second produce
  the same filename and the second overwrites the first. The run is kept
  in the history (`GET /sim/settlement-cycle/runs`).

### `GET /sim/settlement-cycle/runs`

The last 100 runs, scheduled and manual, oldest first.

- Answers `200` `{"count": 1, "runs": [{…}]}`, each run as
  `POST /sim/settlement-cycle/run` returns it.

### `GET /sim/enrolment`

The enrolment desk's state.

- Answers `200` `{"waiting": ["INFINITEENROLL_01.20260925.pgp"], "answered": [], "rejects": {"pf-1002": "09"}, "delay": "30s"}`.
  `waiting` is every well-named batch in `to_WLNORDIC/` not yet answered;
  `answered` includes batches that failed and got no receipt. `500` if the
  directory cannot be read.

### `POST /sim/enrolment/process`

Answers every waiting batch now, ignoring `WORLDLINE_ENROLMENT_DELAY`.

- Body: none.
- Answers `200` `{"answered": 1}`, counting only batches that got a
  receipt. `500` if the directory cannot be read.

### `POST /sim/enrolment/reject`

Arms a rejection: from now on, a batch naming this submerchant rejects it.

- Body: `{"pf_account_id": "pf-1002", "reason_code": "09"}`. Codes `01`–`13`,
  each with its text (`09` is "Invalid or not permitted MCC").
- Answers `200` `{"pf_account_id": "pf-1002", "reason_code": "09", "reason": "Invalid or not permitted MCC"}`.
  `400` when `pf_account_id` is empty, the code is not one of the thirteen,
  or the body is not JSON.
- Side effects: the rejection is never cleared. It applies to every later
  batch naming that `pf_account_id` until the process restarts, not only
  the next one. It cannot change a receipt already written.

## Called by

- **The settlement stand-in** (`cmd/settlement`): SFTP as `infinitepay`,
  listing and downloading `download/` every `WORLDLINE_PULL_INTERVAL`
  (10 s) and on its own `POST /worldline/pull`, pinning the host key and
  decrypting with `worldline_private.asc`. It also writes WX reports into
  `./sftp/outbound/{merchant}/daily/` over a shared bind mount, which
  `GET /outbound/{merchant_id}/daily` and the download route serve; that is
  a file, not a call.
- **The platform under test**: SFTP, with the settings from the console's
  `GET /api/connect/env/worldline`. It downloads settlement files, uploads
  enrolment batches to `to_WLNORDIC/` and collects receipts from
  `from_WLNORDIC/`.
- **The console** (`cmd/console`): `GET /health` (its health monitor, behind
  `GET /api/overview` and `POST /api/services/{id}/probe`); `GET /sim/activity`
  (`GET /api/services/{id}/activity`, `GET /api/flow`,
  `GET /api/flow/hops/{id}`); `POST /sim/transactions`
  (`POST /api/merchants/{id}/trading`, `POST /api/merchants/batches/{id}/trading`);
  `POST /sim/settlement-cycle/run` (`POST /api/actions/worldline-cycle`,
  passing `slot` and `date`); `GET /config` (`GET /api/config`); `PUT /config`
  (`PUT /api/config/worldline-channel`).
- **The harness**: `worldline-settlement-to-sftp`
  (`internal/harness/scenario_worldline.go`) seeds two transactions with
  `POST /sim/transactions`, runs
  `POST /sim/settlement-cycle/run?slot=morning&date=…`, then logs in over
  SFTP as `harness`, downloads the file from `download/` and decrypts and
  parses it itself. `worldline-confirmation-file` runs
  `POST /sim/settlement-cycle/run?slot=afternoon&date=…` and checks the
  confirmation is taken without a second payout.
- **Nobody in the repo** calls the inbound, outbound-list, download or
  archive routes, `GET /sim/transactions`, `GET /sim/settlement-cycle/runs`
  or the `/sim/enrolment` routes. They are for operators and agents.
- **Worldline calls out** to Banking Circle's bridge,
  `POST :8095/internal/incoming-payments`, once per currency on each
  morning slot (the lump sum).

Routes checked against the source at commit db5e695.
