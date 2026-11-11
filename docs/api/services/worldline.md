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
encrypt-and-sign. It serves several platforms at once, one per
[environment](#environments), each with its own SFTP user, folder, files
and lump sums. Simplified: no Acquiring REST API, no chargebacks (the
`CB` section is always empty), any SFTP username that is not an
environment's is accepted as the default's, and the SFT file-exchange
channel for onboarding documents and WX/financial reports is served over
plain HTTP rather than SFTP.

## At a glance

| | |
| --- | --- |
| Listens | HTTP `:8084` (`LISTEN`) and SSH/SFTP `:2222` (`WORLDLINE_SFTP_PORT`), both in one process. Compose service `worldline`; from the host `http://127.0.0.1:8084` and `sftp://127.0.0.1:2222`.<br>`:9084`, plain HTTP: the **control port** (`SIM_CONTROL_LISTEN`), every `/sim/*` route below and its own `GET /health`. It steers the SFTP side too, which has no control port of its own. Not published by compose; the lab API reaches it at `http://worldline:9084` |
| Auth | HTTP: none, on every route; it is the default environment's. SFTP: an environment's username takes only its own password, and opens that environment's folder ([Environments](#environments)). Every other username is the default environment's: password (`WORLDLINE_SFTP_PASSWORD`, compose `sim-sftp-dev-only`) and public key (`WORLDLINE_SFTP_AUTHORIZED_KEY`), each enabled independently, and the username itself is not checked. With both unset, any credential is accepted for it (logged as a warning at start).<br>The control port has no vendor auth: it takes calls from anything that can reach it, or only from `SIM_CONTROL_ALLOW` (addresses or CIDRs) when that is set |
| A platform points here with | The settlement stand-in (`compose.yml`): `WORLDLINE_SFTP_HOST`, `WORLDLINE_SFTP_PORT`, `WORLDLINE_SFTP_USER`, `WORLDLINE_SFTP_PASSWORD` or `WORLDLINE_SFTP_PRIVATE_KEY_PATH`, `WORLDLINE_SFTP_KNOWN_HOST_PATH`, `WORLDLINE_PGP_PRIVATE_KEY_PATH`, `WORLDLINE_SFTP_DIR` (default `download`). the lab API's connect kit (`GET :8097/v1/connect/env/worldline`) hands a platform `WORLDLINE_SFTP_HOST`, `_PORT`, `_USERNAME`, `_PASSWORD`, `_HOST_KEY_FINGERPRINT`, `_DOWNLOAD_DIR`, `_RECONCILIATION_DIR`, `_DISPUTES_DIR`, `_UPLOAD_DIR`, `WORLDLINE_SETTLEMENT_IDENTIFIER`, `WORLDLINE_PGP_PUBLIC_KEY` and `INFINITE_PGP_PRIVATE_KEY`. The HTTP side is found with `CONSOLE_URL_WORLDLINE` (lab API); the lab API takes the control port as that port plus 1000 unless `CONSOLE_CONTROL_URL_WORLDLINE` names it |
| Activity logs | `GET /sim/activity` keeps one log, `sftp` (*File exchange*): `sftp.session` for each SSH login or refused handshake, then one event per SFTP request (`sftp.list`, `sftp.download`, `sftp.upload`, and the jail's other verbs, such as `sftp.rename`, `sftp.remove`, `sftp.mkdir`), each with `user` and `path`; a refused request is amber with the server's error. Stats are not logged. Each file taken over `GET /outbound/{merchant_id}/{category}/{filename}` lands in the same log as `http.download`, and each file the lab hands Worldline to publish ([`POST /sim/settlement-files/publish`](#post-simsettlement-filespublish)) as `settlement-file.published`. Each event is its environment's: a session's, and the refused handshake of an environment's username, carry `env` and are kept apart from the default's |
| Configuration | The important variables are in the table below. Slot times are read in the container's local zone; the image sets no `TZ`, so that is UTC |
| Design record | [ARCHITECTURE-worldline-acquirer.md](../../ARCHITECTURE-worldline-acquirer.md) (who owns the file, the two slots, one file per currency), [ARCHITECTURE-vendor-corrections.md](../../ARCHITECTURE-vendor-corrections.md) §1–2 and Addendum E (Bambora format, SFTP+PGP, encryption direction), [ARCHITECTURE-worldline-sftp-channel.md](../../ARCHITECTURE-worldline-sftp-channel.md) (historical; its SFT directory conventions still hold), [structure/worldline.md](../../structure/worldline.md) |

| Variable | Default | Effect |
| --- | --- | --- |
| `LAB_CLOCK_URL` | unset (compose `http://clock:8096/clock`) | Follow the lab clock: slot fire times, coverage dates, filename timestamps, receipt times and the archive folder |
| `SFTP_BASE_DIR` | `/sftp` | Root of the HTTP SFT channel (`inbound/`, `outbound/`, `config/`, `archive/`); compose bind-mounts `./sftp/*` |
| `MERCHANT_ID` | `GB00SIM0000000000003` | Merchant id in the channel config seeded at start when none exists |
| `WORLDLINE_SFTP_ROOT` | `/wlsftp` | The SFTP server's root, the default environment's jail. Not a bind mount: it lives and dies with the container |
| `WORLDLINE_SFTP_ENV_ROOT` | `WORLDLINE_SFTP_ROOT` + `-environments` (`/wlsftp-environments`) | Where every other environment's jail is made, one folder each, `{WORLDLINE_SFTP_ENV_ROOT}/{env}`. Created at start if it can be; one that cannot be leaves every `PUT /sim/customers/{env}` refused until it can. It may not be `WORLDLINE_SFTP_ROOT`, be inside it or hold it (fatal at start): the default's user must not see an environment's folder |
| `WORLDLINE_SFTP_PASSWORD` | unset | Enables password auth |
| `WORLDLINE_SFTP_AUTHORIZED_KEY` | unset | Enables public-key auth; one or more `authorized_keys` lines. Unparsable is fatal at start |
| `WORLDLINE_SFTP_HOST_KEY_PATH` | `/keys/wlsftp-keys/host_key` | ed25519 host key, generated on first start |
| `WORLDLINE_SFTP_HOST_KEY_PUB_PATH` | host key path + `.pub` | Where the host key's public half is written, for pinning |
| `WORLDLINE_PGP_PUBLIC_KEY_PATH`, `WORLDLINE_PGP_PRIVATE_KEY_PATH` | `/keys/wlsftp-keys/worldline_public.asc`, `/keys/wlsftp-keys/worldline_private.asc` | mock-Worldline's RSA-3072 OpenPGP keypair, generated if either file is missing |
| `INFINITE_PGP_PUBLIC_KEY_PATH` | unset | The platform's public key. Set, files and receipts are encrypted to it; unset, to Worldline's own key |
| `WORLDLINE_FILE_SOURCE` | `generate` | `generate` cuts files from the held transactions; `fixture` serves the files in `WORLDLINE_FIXTURE_DIR` whose name contains `_ER_` or `_AR_`, byte for byte. Anything else is fatal at start |
| `WORLDLINE_FIXTURE_DIR` | `/wlsftp-fixtures` | Fixture files (compose mounts `./worldline/fixtures`) |
| `WORLDLINE_SETTLEMENT_IDENTIFIER` | `Worldline_Settlement` | The `{identifier}` in the filename: the default environment's, and every other's that names none of its own |
| `WORLDLINE_SETTLEMENT_ACCOUNT` | empty (compose `GB00SIM0000000000001`) | `TO_ACCOUNT` in the file's Settlement row |
| `WORLDLINE_MORNING_FILE_AT`, `WORLDLINE_MORNING_FILE_WINDOW` | `08:00`, `2h` | The morning slot fires at that time plus a per-day jitter inside the window, deterministic for a given date |
| `WORLDLINE_AFTERNOON_FILE_AT` | `15:30` | The afternoon slot, no jitter |
| `WORLDLINE_SETTLEMENT_DELAY` | `24h` | A scheduled run settles the single day this long before now (T+1) |
| `BC_INTERNAL_URL` | empty (compose `http://banking-circle:8095`) | Banking Circle's bridge, for the lump sum. Empty: lump sums report `not configured` |
| `BC_SAFEGUARDING_ACCOUNT_ID_EUR`, `BC_SAFEGUARDING_ACCOUNT_ID_GBP` | empty | The default environment's: a currency's lump sum is wired only when its account is set; no other currency is read. Every other environment's accounts are its own ([Environments](#environments)) |
| `WORLDLINE_ENROLMENT_DELAY` | `30s` | How old an enrolment batch must be before it is answered, by file modification time on the wall clock |
| `WORLDLINE_ENROLMENT_SWEEP` | `5s` | How often `to_WLNORDIC/` is checked for batches |
| `SIM_CONTROL_LISTEN` | `:9084` (`LISTEN`'s port plus 1000) | The control port |
| `SIM_CONTROL_ALLOW` | unset | Addresses or CIDRs, comma-separated, that may call the control port; unset, anyone who can reach it |

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
so a client can pin it. An environment's username opens that
environment's folder, with its own password only; any other username is
accepted as the default's ([Environments](#environments)). The username is
recorded in the activity log. The settlement stand-in logs in as
`infinitepay`, the e2e suite as `harness`, both the default's.

**SFTP: what a session may do.** Every path is jailed under the
session's root, `WORLDLINE_SFTP_ROOT` for the default's and
`{WORLDLINE_SFTP_ENV_ROOT}/{env}` for an environment's: absolute paths are
jail-absolute, `..` cannot leave the root, creating a symlink is refused
and so is following one out of the root. Inside the jail a session can list, read, write, rename, remove,
mkdir, truncate and hard-link anything. There are no per-directory
permissions, so a client can delete a settlement file.

**SFTP: directories.** Every jail has the same four.

| Directory | Holds | Written by |
| --- | --- | --- |
| `download/` | Every settlement file | The settlement cycle |
| `to_WLNORDIC/` | A second copy of every settlement file, and the platform's enrolment batches | The cycle; the platform |
| `from_WLNORDIC/` | Enrolment receipts | The enrolment desk |
| `disputes/download/` | Nothing: created at start, never written | — |

`download/` and `to_WLNORDIC/` are the two real path conventions for the
same content. The server never deletes from either, except on a lab reset
(`POST /sim/reset`), so a client keeps track of what it has already taken.

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
answered so it is not retried. A batch is answered once per process, or
again after a reset of everything.

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

## Environments

Worldline serves one platform per environment
([environments.md](../../lab-api/environments.md)): `default`, which is the
acquirer exactly as it was before environments, and every environment the
lab API pushes with `PUT /sim/customers/{env}`. Each is a Worldline
customer of its own, and nothing of one reaches another.

- **The SFTP login says whose a session is.** A customer's `worldline`
  `username` and `password` open a session jailed in
  `{WORLDLINE_SFTP_ENV_ROOT}/{env}`; that username with any other password,
  or with a key, is refused. Any other username is the default's, on
  `WORLDLINE_SFTP_ROOT`, with the default's own password, key or open
  login as before. A customer with no `worldline` login has a folder and
  files but no way in. The session's events are the environment's, and it
  is recorded as seen (`last_seen` in `GET /sim/customers`).
- **Each jail is its own.** A customer's folder is made, with the four SFT
  directories, when it is first put, outside the default's root: the
  default's user cannot see it, nor one environment another's. Enrolment
  batches a platform uploads into its own `to_WLNORDIC/` are its own, and
  their receipts land in its own `from_WLNORDIC/`.
- **Each environment is settled apart.** Its transactions, cycle runs, lump
  sums and published files are its own. Every slot, scheduled or run by
  hand, cuts each environment's files from its own transactions only: one
  file per currency per environment, named with the customer's
  `settlement_identifier` (else `WORLDLINE_SETTLEMENT_IDENTIFIER`;
  letters, digits, `_` and `-`), and published into its own `download/`
  and `to_WLNORDIC/`. The schedule, the coverage day, the PGP keys and the
  file source are the lab's: in `fixture` mode every environment is
  served the fixtures.
- **Each environment is paid into its own account.** The morning slot's
  lump sum goes to the environment's safeguarding account: the
  default's from `BC_SAFEGUARDING_ACCOUNT_ID_EUR` and `_GBP`, any other's
  the one Banking Circle derives for it
  (`internal/tenant.SafeguardingAccount`). The wire to the bridge names
  the environment in a `Lab-Environment: {env}` header, and the
  default's carries none.
- **MIDs are apart.** The default boards a `pf_account_id` as it always
  has, `6001` and four digits. Any other environment boards it as eight
  digits from `61000000` to `69999999`, derived from the environment and
  the `pf_account_id` (`worldline.EnvProxyMID`), so two platforms boarding
  the same account never share a MID and a recreated environment gets the
  same ones back. Its `virtual_account_id` is derived the same way.
- **The lab hooks act in one environment.** Every `/sim/*` route below
  takes `?env={env}` (or the `Lab-Environment` header); naming none is
  the default's, as every call was before environments, and naming one
  Worldline has no customer for is `404` `{"error": "no environment nope
  at worldline"}`. Reads answer that environment's records only, and
  `*` every environment's. A hook that makes something
  (`POST /sim/transactions`, `POST /sim/enrolment/reject`,
  `POST /sim/settlement-files/render` and `/publish`) makes it in the
  named environment and refuses `*` with `400`. A record read across
  environments says whose it is with `env`; the default's carry none.
  `GET` and `PUT /sim/config` are the SFT channel's, which is the
  default's and lab-wide, and take no `env`.
- **A reset names its environment** and clears only that environment's
  records and files; `*` clears every environment's. Deleting a customer
  clears everything it made, as an `all` reset would, and removes its
  folder with whatever its platform left there.

## Lab control

Not part of Worldline. Everything under `/sim` is how a test, an agent or the
lab API says "a card payment happened" or "deliver the file now". It is on
the control port, `:9084` ([the control
contract](../README.md#the-control-contract)). The same `/sim/*` paths
still answer on `:8084` until phase 4 of
[the plan](../../lab-api/plan.md) retires them.

### `GET /health`

Liveness, on both ports. `:8084` answers `200` `{"status": "ok",
"service": "worldline"}`; the control port answers `{"status": "ok",
"service": "worldline", "surface": "control"}`, and that is the one the lab
API's monitor probes. Neither checks the SFTP listener.

### `GET /sim/activity`

The `sftp` activity log described under *At a glance*. A reset clears it.

- Query: `limit` (default 100; the ring keeps 256 per environment);
  `env`, whose events (the default's when none, every environment's for
  `*`). An event of another environment's carries `"env": "sbx"`.
- Answers `200` `{"logs": [{"name": "sftp", "title": "File exchange", "note": "…", "total": 9, "kept": 9, "last": {…}, "events": [{"seq": 9, "at": "2026-09-25T09:00:04Z", "op": "sftp.download", "peer": "10.89.0.7", "summary": "infinitepay collected 20260925083412_Worldline_Settlement_ER_EUR.csv.pgp", "status": "ok", "detail": {"user": "infinitepay", "path": "/download/20260925083412_Worldline_Settlement_ER_EUR.csv.pgp"}}]}]}`.

### `GET /sim/info`

Who this is and what it can do, as every service answers it
([the control contract](../README.md#the-control-contract)):
`"service": "worldline"`, `"kind": "vendor"`,
`"vendor_listeners": [":8084", ":2222"]`, `"control_listener": ":9084"`,
`"reset_scopes": ["flow", "all"]`, `"environments": true` and
`"customers"`, how many environments besides the default it holds.

### `GET /sim/logs`

The process's own log lines, as every service answers it
([the control contract](../README.md#the-control-contract)): each slot
planned and run, each lump sum, each enrolment batch answered, and every
HTTP request on either port.

### `POST /sim/reset`

Puts the acquirer back to a known state. Synchronous; the body is optional
(`flow`, the default environment).

- **`env`** (in the body, or `?env=`) is the environment to reset: its
  records, and the files in its own folder, only. `*` resets every
  environment, and an environment Worldline has no customer for is `404`.
  The archive and the channel configuration below are the default's, and
  only a reset that reaches the default touches them; another
  environment's `all` recreates its own folders instead, and reports
  `archived_reports` and `channel_config` as `0`. Counts are summed over
  the environments reset.

- **`flow`** clears what a run makes: acquired transactions, whose ids
  start again at `wltxn_000001` (`transactions`); the cycle-run history
  (`cycle_runs`) and the lump sums in it (`lump_sums`; the wires are not
  recalled, Banking Circle's own reset clears them); the settlement files
  the cycle and `POST /sim/settlement-files/publish` published
  (`settlement_files`), and the lump sums wired with the latter (in
  `lump_sums`); the reports the download route archived
  (`archived_reports`); armed enrolment rejections
  (`enrolment_rejections`); and the `sftp` activity log (`activity`). It
  keeps what a platform set up once: the enrolment receipts and the desk's
  record of the batches it answered (the MIDs it issued), and the SFT
  channel's configuration.
- **`all`** also forgets which batches were answered (`answered_batches`),
  deletes the receipts (`enrolment_receipts`), rewrites the channel
  configuration to its seed when it differs (`channel_config`, `1` or `0`)
  and recreates the SFTP and channel folders. The platform's batches stay
  in `to_WLNORDIC/`, so the desk answers them again on its next sweep,
  with the same MIDs.
- **Keeps**, either way, the platform's own files: its enrolment batches,
  its inbound documents and correction files, and the PGP and host keys.

```json
{"service": "worldline", "scope": "flow", "env": "default",
 "cleared": {"transactions": 2, "cycle_runs": 1, "lump_sums": 1, "settlement_files": 2,
             "archived_reports": 0, "enrolment_rejections": 0, "activity": 9},
 "kept": ["enrolment_receipts", "channel_config", "inbound_uploads"], "at": "2026-11-07T12:00:00Z"}
```

`kept` is `["enrolment_batches", "inbound_uploads", "keys"]` on `all`.

**Files.** The SFTP root and `./sftp` are shared with the platform and the
host, so a reset deletes only a regular file it can prove it wrote, by its
name in the folder it writes it to. Never a folder, a symlink, or anything
else:

- `download/` and `to_WLNORDIC/` in the environment's jail
  (`{WORLDLINE_SFTP_ROOT}` for the default):
  `{14 digits}_{identifier}_{ER|AR}_{CCY}.csv.pgp`, with the environment's
  identifier as it stands; in `fixture` mode also `{fixture name}.pgp` for
  each fixture the cycle serves; and any name the environment's run
  history says was published, or `POST /sim/settlement-files/publish`
  published (its last 100).
- `{SFTP_BASE_DIR}/archive/{YYYY}/{MM}/`:
  `{merchant_id}_{YYYY-MM-DD}_{CCY}_WX.{csv|xml|json}`, the WX reports the
  download route archived. A file of another name there (a financial
  report put in `outbound/` by hand, then downloaded) is left.
- On `all`, the jail's `from_WLNORDIC/`:
  `INFINITEENROLL_{NN}.{YYYYMMDD}.ack.pgp` and its `.partial`.

It leaves the platform's enrolment batches, the inbound documents and
correction files, the WX reports in `outbound/` (the settlement
stand-in's; its own reset deletes them), the fixtures and the keys.

**Work in flight** is dropped, not raced: a slot that is cutting and
publishing, a file being published by hand, the lump-sum wire and the
desk answering a batch each stop at their next step after the reset, so
no file is published, no wire sent, no receipt written and no run
recorded after it. Only the environments reset are held: another
environment's run carries on. A wire already on its way
when the reset arrives is waited for, so it lands before the reset, and
Banking Circle's own reset clears the payment. The schedule keeps running:
the next slot fires as it would have.

`500` with the reason when a file cannot be deleted; everything else is
still done, and a second reset finishes the job.

### `POST /sim/transactions`

Seeds card transactions the acquirer has acquired, in the environment
`?env=` names (the default's when none; `*` is `400`).

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
  the file is cut. Unknown fields are ignored, and so is an `env` in the
  body: the query names the environment.
- Answers `201` `{"accepted": 1, "transactions": [{"id": "wltxn_000001", "mid": "GB00SIM0000000000003", "currency": "EUR", "amount_cents": 10000, "date": "2026-09-24", "card_scheme_name": "Visa", "mcc": "5812", "terminal_id": "T0001"}]}`,
  currency upper-cased; another environment's carry `"env": "sbx"`, and
  their ids are counted per environment. `400` `{"error": "mid is required", "status": 400}`
  (or the failing rule) for an invalid transaction. Transactions earlier in
  the array than the invalid one are already stored.
- Side effects: in memory only; a restart or a reset forgets them. Nothing
  is settled until a slot runs, and then only in the environment's own
  files.

### `GET /sim/transactions`

What the acquirer holds.

- Query: `env` (the default's when none, every environment's for `*`);
  `mid` (optional); `from` and `to` (`YYYY-MM-DD`, inclusive) narrow
  by transaction date, and only apply together with `mid`.
- Answers `200` `{"count": 2, "transactions": [{…}]}`, `transactions` in
  the order they were added, environment by environment for `*`, the
  default's first.

### `POST /sim/settlement-cycle/run`

Runs one slot now, exactly as the schedule would, for one environment.

- Query: `slot` — `morning`, `er` or empty for `ER`; `afternoon`,
  `confirmation` or `ar` for `AR`. `date` (`YYYY-MM-DD`, optional) settles
  that single day instead of the day `WORLDLINE_SETTLEMENT_DELAY` before
  now. The file is delivered now either way. `env`: whose cycle (the
  default's when none); `*` runs every environment's in turn, as a
  scheduled slot does.
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
  transactions on the day there is no file. Another environment's run
  carries `"env": "sbx"`, its files are named with its identifier and its
  lump sums name its own account. With `*` the answer is
  `{"count": 2, "runs": [{…}, {…}]}`, one run per environment, the
  default's first. `207` with the same body plus
  `"errors": ["…"]` when the cut or a publish failed, or a reset landed
  while it ran (the rest of the run is dropped and it is not kept in the
  history). `400` for an unknown `slot` or a malformed `date`.
- Side effects: publishes each file to `download/` and `to_WLNORDIC/` in
  the environment's jail. Morning slot only: for each currency with a
  positive total, posts
  `{"currency": "EUR", "amount": "150.00", "reference": "WORLDLINE SETTLEMENT EUR 2026-09-24"}`
  to Banking Circle's `POST {BC_INTERNAL_URL}/internal/incoming-payments`,
  with `Lab-Environment: {env}` for any environment but the default.
  A lump sum's `status` is `credited`, `not configured` (no bridge URL or
  no account for the currency) or `error: …`; a failed wire never stops the
  file. In `fixture` mode the cut files carry no currency or amount, so no
  lump sum is wired. Two runs of the same slot in the same second produce
  the same filename and the second overwrites the first. The run is kept
  in the history (`GET /sim/settlement-cycle/runs`).

### `GET /sim/settlement-cycle/runs`

The last 100 runs of an environment, scheduled and manual, oldest first.

- Query: `env` (the default's when none; `*` every environment's, merged
  oldest first).
- Answers `200` `{"count": 1, "runs": [{…}]}`, each run as
  `POST /sim/settlement-cycle/run` returns it.

### `POST /sim/settlement-files/render`

The settlement file of one currency the cycle would cut now from an
environment's transactions, as plain text: the cycle's own cut and
naming, and nothing else. Nothing is published, no lump sum is wired and
no run is recorded. The lab API keeps what it answers as a file
([plan](../../lab-api/plan.md), phase 6).

- Query: `env`, whose transactions and identifier (the default's when
  none; `*` is `400`).
- Body: `{"currency": "EUR", "date": "2026-09-24", "slot": "ER"}`.
  `currency` is required, three letters, any case. `date` (`YYYY-MM-DD`)
  is the single day settled; without it, the day the cycle would settle
  now by the lab clock (`WORLDLINE_SETTLEMENT_DELAY` before now). `slot`
  as `POST /sim/settlement-cycle/run` takes it, `ER` when empty. Unknown
  fields are `400`.
- Answers `200`:

  ```json
  {"name": "20260925090000_Infinite_Payment_ER_EUR.csv",
   "content": "\"VERSION_NUMBER\",\"RECORD_TYPE\",…",
   "currency": "EUR", "mids": ["MID-A", "MID-B"], "total": "100.00", "items": 2,
   "slot": "ER", "from_date": "2026-09-24", "to_date": "2026-09-24"}
  ```

  `name` is exactly the name the cycle would publish the file under now,
  before `.pgp`: the lab clock's time, the environment's
  `settlement_identifier`, the slot and the currency. `content` is the
  file byte for byte as the cycle would cut it, `total` its
  `SETTLEMENT_AMOUNT` in major units, `mids` every MID it settles
  (`ADDITIONAL_REF_2`). In `fixture` mode it is the slot's first fixture
  in that currency, under the fixture's own name.
- `404` `{"error": "nothing settles in EUR on 2026-09-24 in sbx: worldline
  holds no EUR transaction of that day there"}` when no transaction would
  be in the file. `400` for a missing or malformed `currency`, `date` or
  `slot`. `409` when a reset of the environment landed while it was
  rendered: render again.

### `POST /sim/settlement-files/publish`

Publishes a settlement file the lab hands over, in an environment's SFTP
folder, exactly as the cycle publishes one it cut, so the platform's next
poll takes it as it takes the cycle's.

A file named as Worldline names its own (`<14-digit timestamp>_…`) is
published under the moment's timestamp, as Worldline names a file when it
drops it: a platform's poll takes only its own day's files, so a file
rendered yesterday reads as today's. Any other name is kept.

- Query: `name`, required: the file's name, to which `.pgp` is added, as
  the cycle names its own; no `/`, `\`, `..` or control character, at
  most 250 bytes. `wire`, `true` or `false` (default `false`). `env`, whose
  folder (the default's when none; `*` is `400`).
- Body: the file, plain text, at most 64 MiB (`413` beyond).
- The body is read with Worldline's own parser
  (`worldline.ParseSettlementFile`): `400` with the parser's reason when
  it is not a settlement file, such as `{"error": "worldline: not a
  settlement file: it has no \"Settlement\" row"}` or an amount with
  three decimals. Then it is encrypted and signed as the cycle's files
  are, and written as `{name}.pgp` into `download/` and `to_WLNORDIC/` of
  the environment's jail, replacing a file of that name.
- With `wire=true`, the file's total (its `SETTLEMENT_AMOUNT`) is wired as
  the morning slot wires its lump sum: to the environment's safeguarding
  account over Banking Circle's bridge, referenced `WORLDLINE SETTLEMENT
  {CCY} {VALUE_DATE}`, with `Lab-Environment: {env}` for any environment
  but the default. As the cycle, it wires nothing for a file whose name
  says it is an afternoon (`_AR_`) confirmation, or whose total is not
  positive.
- Answers `201`:

  ```json
  {"published": "20260925090000_Infinite_Payment_ER_EUR.csv.pgp",
   "name": "20260925090000_Infinite_Payment_ER_EUR.csv",
   "currency": "EUR", "total": "100.00", "mids": ["MID-A", "MID-B"],
   "lump_sum": {"currency": "EUR", "amount_cents": 10000, "reference": "WORLDLINE SETTLEMENT EUR 2026-09-24",
                "account_id": "…", "status": "credited"}}
  ```

  `published` is the name in the folder, as a cycle run lists it.
  `lump_sum` is as in a cycle run (`credited`, `not configured` or
  `error: …`; a failed wire never stops the file), and `null` when
  nothing was wired. `409` when a reset of the environment landed while it
  ran: what it published went with the reset, and nothing after it
  happened. `500` when the file cannot be written.
- Side effects: no cycle run is recorded. The `sftp` activity log of the
  environment gains a `settlement-file.published` event, with `file`,
  `path`, `currency`, `total`, `mids` and, when wired, `lump_sum` and
  `account_id` (amber when the wire did not credit). A `flow` reset
  deletes the file, whatever its name, and counts its lump sum.

### `GET /sim/enrolment`

An environment's enrolment desk: each environment has its own, answering
the batches in its own `to_WLNORDIC/`.

- Query: `env` (the default's when none).
- Answers `200` `{"waiting": ["INFINITEENROLL_01.20260925.pgp"], "answered": [], "rejects": {"pf-1002": "09"}, "delay": "30s"}`.
  `waiting` is every well-named batch in `to_WLNORDIC/` not yet answered;
  `answered` includes batches that failed and got no receipt. With `*`,
  `{"environments": [{"env": "default", "waiting": […], …}, {"env": "sbx", …}]}`.
  `500` if a directory cannot be read.

### `POST /sim/enrolment/process`

Answers every waiting batch now, ignoring `WORLDLINE_ENROLMENT_DELAY`.

- Query: `env` (the default's when none; `*` every environment's).
- Body: none.
- Answers `200` `{"answered": 1}`, counting only batches that got a
  receipt. `500` if a directory cannot be read.

### `POST /sim/enrolment/reject`

Arms a rejection: from now on, a batch naming this submerchant rejects it,
at the environment's desk.

- Query: `env` (the default's when none; `*` is `400`).
- Body: `{"pf_account_id": "pf-1002", "reason_code": "09"}`. Codes `01`–`13`,
  each with its text (`09` is "Invalid or not permitted MCC").
- Answers `200` `{"pf_account_id": "pf-1002", "reason_code": "09", "reason": "Invalid or not permitted MCC"}`.
  `400` when `pf_account_id` is empty, the code is not one of the thirteen,
  or the body is not JSON.
- Side effects: the rejection applies to every later batch naming that
  `pf_account_id` until a reset or a restart, not only the next one. It
  cannot change a receipt already written.

### `GET /sim/customers`

The environments Worldline serves, the default first, from
[`internal/tenant`](../../lab-api/sim-control.md#8-environments).

- Answers `200` `{"customers": [{"env": "default", "created_at": "0001-01-01T00:00:00Z", "last_seen": "2026-11-10T09:00:04Z"}, {"env": "sbx", "name": "Sandbox", "created_at": "2026-11-10T08:00:00Z", "last_seen": null}]}`.
  `last_seen` is the last SFTP session or request of that environment's
  login, `null` until there is one; the HTTP SFT channel does not count.
  Logins are not shown.

### `PUT /sim/customers/{env}`

Adds an environment, or replaces its logins. The lab API pushes every
environment again when Worldline restarts, since they are held in memory.

- Body: a customer; Worldline reads only its `worldline` part:

  ```json
  {"env": "sbx", "name": "Sandbox",
   "worldline": {"username": "sbx-platform", "password": "…", "settlement_identifier": "Infinite_Payment"}}
  ```

  `env` may be left out; it is the path's.
- Answers `200` `{"env": "sbx", "created": true}`, `false` when it
  replaced one. `400` for an id that is not lower-case letters, digits and
  dashes starting with a letter, or is `default` or `*`; for a body whose
  `env` is not the path's; for a `settlement_identifier` that is not
  letters, digits, `_` and `-` (at most 64, starting and ending with a
  letter or digit); and when the environment's folder cannot be made.
- Side effects: makes `{WORLDLINE_SFTP_ENV_ROOT}/{env}` with the four SFT
  directories. Put again, the environment keeps its transactions, runs
  and files, and later files take the new identifier. The login works at
  once.

### `DELETE /sim/customers/{env}`

Removes an environment and everything it made.

- Answers `200` `{"env": "sbx", "deleted": true}`; `404` for an
  environment Worldline does not hold; `500` when its folder cannot be
  removed (the environment stays, and a second delete finishes the job).
- Side effects: its transactions, runs, lump sums, enrolment desk and
  activity are cleared, work of its still in flight is dropped, and
  `{WORLDLINE_SFTP_ENV_ROOT}/{env}` is removed with everything in it. Its
  login no longer opens anything: the username is then just another
  default login.

### `GET /sim/config`

[`GET /config`](#get-config) on the control port: the same handler,
answering the same, so the lab reads the channel configuration without
calling the vendor's own API.

### `PUT /sim/config`

[`PUT /config`](#put-config) on the control port: the same body, answers
and refusals, and the same file written.

## Called by

- **The settlement stand-in** (`cmd/settlement`): SFTP as `infinitepay`,
  listing and downloading `download/` every `WORLDLINE_PULL_INTERVAL`
  (10 s) and on its own `POST /worldline/pull`, pinning the host key and
  decrypting with `worldline_private.asc`. It also writes WX reports into
  `./sftp/outbound/{merchant}/daily/` over a shared bind mount, which
  `GET /outbound/{merchant_id}/daily` and the download route serve; that is
  a file, not a call.
- **The platform under test**: SFTP, with the settings from the lab API's
  `GET /v1/connect/env/worldline`. It downloads settlement files, uploads
  enrolment batches to `to_WLNORDIC/` and collects receipts from
  `from_WLNORDIC/`.
- **The lab API** (`cmd/labapi`), on the control port only
  (`http://worldline:9084`), never the vendor's own surface, naming the
  environment of the call it is making with `?env=`: `GET /health`
  (its health monitor, behind `GET /v1/lab` and
  `POST /v1/services/{id}/probe`); `POST /sim/reset`
  (`POST /v1/lab/reset`, first in the order, and
  `POST /v1/services/{id}/reset`); `GET /sim/logs` (`GET /v1/logs`,
  `GET /v1/services/{id}/logs`); `GET /sim/activity`
  (`GET /v1/services/{id}/activity`, `GET /v1/flow`,
  `GET /v1/flow/hops/{id}`); `POST /sim/transactions`
  (`POST /v1/merchants/{id}/trading`, `POST /v1/merchants/batches/{id}/trading`);
  `POST /sim/settlement-cycle/run` (`POST /v1/actions/worldline-cycle`,
  passing `slot` and `date`); `GET /sim/config` (`GET /v1/config`);
  `PUT /sim/config` (`PUT /v1/config/worldline-channel`);
  `GET`, `PUT` and `DELETE /sim/customers…` (`/v1/environments`, and its
  sync).
- **The e2e suite**, its lab hooks through the lab API:
  `worldline-settlement-to-sftp`
  (`internal/e2e/scenario_worldline.go`) seeds two transactions with
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

Every route above is held to the source by `internal/console/apidocs_test.go`: a route added, moved or removed without this page following fails `go test ./internal/console`.
