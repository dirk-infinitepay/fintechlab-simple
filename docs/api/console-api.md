# Console API — reference

The console's `/api/*` is the backend the console page calls, and it is
also the most convenient lab-level API for a script or an agent: merchants,
the clock, settlement runs and what happened, at one address. Most routes
are a call to one or more services' own HTTP endpoints; **Reaches** under
each route says which, so a caller can go to the service directly instead.
The rest read or write the console's own state: the merchant registry
(`console-data/registry.json`), plugin registrations and stand-in
visibility (both in memory). [README.md](README.md) places this API among
the others; [../console.md](../console.md) is the page it serves.

From the host the base URL is `http://127.0.0.1:8090` (`LISTEN`, default
`:8090`). There is no authentication and no CORS header. Requests and
answers are JSON, except the connect downloads (`text/plain` or
`application/octet-stream` attachments) and the plugin upload (raw bytes).
A body marked *strict* below is decoded with unknown fields refused, and
must be present: no body is `400 {"error": "EOF"}`, so send `{}` for all
defaults. Every answer, errors included, carries `X-Console-Build`: 12 hex
characters hashing the embedded page, which changes when the console is
rebuilt.

Errors are `{"error": "<message>", "status": <code>}`, the message in the
failing peer's own words where there is one. `400` is something the caller
can fix, `404` an unknown id, `409` a refusal by state (a plugin stopped,
the clock locked), `413` an upload over the limit, `503` a key file not
generated yet. `501` means *does not
exist here by design* (Banking Circle opens no accounts, a service keeps no
activity log, a plugin did not declare that path): not a failure, do not
retry. `502` means an upstream service failed: unreachable (`dial tcp …:
connection refused`), timed out (10 s per call), or refused. A refusal
keeps its message but not its status: B4B's `400` reaches you as `502`.
Routes marked *verbatim* are the exception and pass the upstream's status
and body through unchanged. `500` is used only when a request to a peer
cannot be built and should not occur. Views that read several services
(`/api/overview`, `/api/banks`, `/api/config`, `/api/flow`,
`/api/stand-ins`) answer `200` with the failure in an error field instead.

## Lab

### `GET /health`

Whether the console process is up. Says nothing about the services behind
it; that is `GET /api/overview`.

**Answers** — `200 {"status": "ok", "service": "console"}`

**Reaches** — nothing — console state only.

### `GET /api`

The index of this API: every route with its group, a one-line summary and
what it reaches. Read it first when driving the lab cold; this document is
its long form.

**Answers** — `200`

```json
{"service": "console", "build": "bf2025448b25", "docs": "docs/api/console-api.md",
 "services": "GET /api/overview lists every service: …",
 "groups": [{"id": "lab", "title": "Lab", "note": "…"}, …],
 "routes": [{"method": "GET", "path": "/api/overview", "group": "lab",
             "summary": "…", "reaches": "…"}, …]}
```

`build` equals the `X-Console-Build` header. Group ids, in order: `lab`,
`clock`, `config`, `merchants`, `banks`, `actions`, `plugins`, `connect`.

**Reaches** — nothing — console state only.

### `GET /api/overview`

Every service in the catalogue, registered plugins first, with its last
health probe. What the Vendors, Platform and Verification views draw.

**Answers** — `200`

```json
{"services": [
   {"id": "b4b", "name": "B4B Payments", "kind": "vendor", "summary": "…",
    "base_url": "http://b4b:8086", "browse_url": "http://127.0.0.1:8086",
    "health_path": "/health", "ports": ["8086/http"], "transport": "HTTP",
    "auth": "Bearer RS512 JWT (aud b4b-payments)", "swap_for": "…", "docs": "…",
    "activity_path": "/sim/activity",
    "endpoints": [{"method": "POST", "path": "/oversight/v1/payments", "note": "pay out (all gates apply)"}, …],
    "status": {"service_id": "b4b", "state": "up", "http_status": 200, "latency_ms": 3,
               "checked_at": "…", "since": "…", "history": [true, true, …]}},
   …],
 "counts": {"up": 12, "unknown": 1},
 "provisioning_ready": true, "provisioning_error": ""}
```

- `kind`: `vendor`, `platform` or `verification`. `state`: `up` (2xx),
  `down` (`detail` says why), `unknown` (not probed yet), `not-probed` (no
  health endpoint). `history` is the last 24 probes, oldest first.
- `base_url` is where the console reaches the service (a compose name
  inside the container); `browse_url` is where a browser on the host does.
- A plugin also carries `plugin`: `clock`, `start_hint`, `actions`,
  `settlement`, `empty_hints`, `state` (`live`, `lapsed`, `stopped`),
  `registered_at`, `last_seen`, `ttl_seconds`. A stopped plugin, or a
  lapsed one not answering, is reported `unknown` with `detail`
  `registration stopped` / `registration lapsed`.
- A stand-in also carries `stand_in`: `shown`, `set_by`, `set_at`,
  `plays`, `starts_off`.
- `provisioning_ready` is false while the B4B signing key is unreadable;
  `provisioning_error` says why. Registering outlets at B4B needs it.

**Reaches** — nothing at request time. A background monitor probes each
service's `health_path` every `CONSOLE_PROBE_INTERVAL` (5 s).

### `POST /api/services/{id}/probe`

Probe one service's health now instead of waiting for the next tick.

**Path parameters** — `id`: a catalogue id (`worldline`, `b4b`,
`banking-circle`, `aci`, `bank`, `verify`, `verification`, `clock`,
`settlement`, `receiver`, `payment-api`, `notifier`) or a registered
plugin's id.

**Answers** — `200` with the new status, shaped as `status` in
`GET /api/overview`. A service that is down is still `200`, with
`"state": "down"`. `404 unknown service <id>`.

**Reaches** — the service's `GET {base_url}{health_path}`
(`banking-circle` and `receiver` over TLS, Banking Circle with the client
certificate).

### `GET /api/services/{id}/activity`

One service's recent traffic, from its own activity rings: the panels on
its card.

**Path / query parameters** — `id`: as for the probe. `limit`: events per
log, passed through (the service's default is 100; rings hold 256).

**Answers** — `200`, the service's body:

```json
{"logs": [{"name": "payments", "title": "Payouts received", "note": "…",
           "total": 1016, "kept": 256, "last": {…},
           "events": [{"seq": 1016, "at": "2026-11-05T22:47:50.5Z", "op": "payment.create",
                       "peer": "10.0.2.100", "summary": "payout 323.32 GBP to …",
                       "status": "ok", "detail": {"amount": "323.32 GBP", …}}, …]}, …]}
```

`status` is `ok`, `warn` or `bad`. Errors: `404 unknown service <id>`;
`501 <name> keeps no activity log` for `aci`, `verify`, `verification`,
`settlement`, `receiver` and a plugin without `activity_path`; `502` when
the service cannot be read (a non-2xx arrives as `GET <url>: HTTP <code>`,
its body dropped).

**Reaches** — the service's `GET /sim/activity?limit=` (Banking Circle's
on its credentialed listener, with a bearer token); for a plugin,
`base_url` + `activity_path`.

### `GET /api/flow`

The settlement run as a sequence diagram: the participants, one step per
arrow with its counts, the platform's run and a closing report. The
Dashboard polls it every second and diffs the counts between polls.

**Answers** — `200`, always; a service that could not be read is named in
`errors`.

```json
{"participants": [{"id": "worldline", "label": "Worldline", "role": "acquirer",
                   "kind": "vendor", "status": "up",
                   "stats": [{"label": "files collected", "value": "0"}, …]}, …],
 "steps": [{"id": "payouts", "from": "platform", "to": "b4b", "label": "create payouts",
            "note": "one per merchant settlement", "multi": true, "count": 256, "capped": true,
            "failed": 0, "refused": 0, "last_at": "…", "last_summary": "…",
            "source": "b4b", "source_log": "payments"}, …],
 "run": {"id": "…", "stages": […], "payouts": {"count": 6, "total": "…", "statuses": {…}}},
 "report": [{"label": "files collected", "value": "0"},
            {"label": "stages completed", "value": "0 of 0"}, …],
 "errors": {"infinite-local-runner-status": "Get \"http://…/status\": dial tcp …"},
 "hidden": [{"id": "receiver", "name": "Webhook receiver (platform stand-in)",
             "steps": ["notification batches"], "count": 209}]}
```

- Participants: `worldline`, `platform`, `b4b`, `banking-circle`, `bank`,
  `receiver`. The platform's id is always `platform`, labelled with the
  settling plugin's name. `receiver` and its arrow are left out while its
  card is hidden (the default), and counted under `hidden`.
- Step ids, in order: `pull`, `collect`, `ingest`, `fund`, `payouts`,
  `bridge`, `land`, `callbacks`, `notify`, `confirm`, `reports`, `recon`.
  `count` is events in the vendor's window; `capped` means the window was
  full, so the count is a floor. `ingest` and `reports` count the
  platform's run stages, not traffic. `refused` is warn events (for the two
  stage arrows: stages still running).
- `run` is the plugin's `running_now`, else its `last_run`; `null` without
  a settling plugin.
- `errors` is keyed by service id, or `<plugin id>-status` for the status
  read.

**Reaches** — in parallel: `worldline`, `b4b`, `banking-circle`, `bank`
`GET /sim/activity?limit=256`; the settling plugin's `activity_path` and
`settlement.status_path`.

### `GET /api/flow/hops/{id}`

The events behind one arrow of the diagram, exactly those its count
counted, newest first: the drawer that opens when an arrow is clicked.

**Path parameters** — `id`: a step id from `GET /api/flow`.

**Answers** — `200`

```json
{"step": {"id": "payouts", "count": 256, …},
 "events": [{"seq": 1016, "at": "…", "op": "payment.create", "summary": "…",
             "status": "ok", "detail": {…}}, …],
 "kept": 100,
 "stages": [{"stage": "SETTLEMENT_FILE_INGESTION", "status": "COMPLETED"}, …],
 "errors": {…}}
```

`step` is the same object `/api/flow` draws. `events` holds at most 100;
`kept` says how many. `stages` is set only for `ingest` and `reports`, when
the plugin maps them; a stage the run has not reached has `"status": ""`.
`404 {"error": "no hop \"<id>\" in the diagram"}` (no `status` field).
`notify` is served even while the receiver is hidden from `/api/flow`.

**Reaches** — the same reads as `GET /api/flow`.

### `GET /api/stand-ins`

The lab's stand-ins (services that play the platform's part until one
plugs in): whether each card is shown, and how its wiring stands, read live
from the services that own it.

**Answers** — `200`

```json
{"stand_ins": [
   {"id": "settlement", "name": "Settlement (platform stand-in)", "shown": false,
    "set_by": "console", "set_at": "…", "plays": "…",
    "starts_off": "SETTLEMENT_CONNECTED=false on the settlement service",
    "state": "connected",
    "connections": [{"id": "timer", "label": "Worldline pull and cutoff payouts",
                     "connected": true, "detail": "…"}]},
   {"id": "receiver", …,
    "connections": [{"id": "banking-circle", "label": "Banking Circle notifications", …},
                    {"id": "aci", "label": "ACI webhooks", …}]},
   {"id": "payment-api", …, "state": "unwired", "connections": []},
   {"id": "notifier", …}]}
```

`state` is `connected`, `disconnected`, `partial`, `unwired` or `unknown`
(a read failed: see the connection's `error`). A connection's `connected`
is `null` when that wiring does not reach the stand-in in the running lab:
no Banking Circle subscription points at the receiver, or ACI delivers
somewhere else.

**Reaches** — `settlement GET /sim/connected`; `banking-circle GET
/api/v1/notificationselfservice/subscription?PageSize=50` (the
subscriptions whose endpoint host is `receiver`); `aci GET /sim/connected`.

### `POST /api/stand-ins/{id}`

Show or hide a stand-in's card, and connect or disconnect its wiring.

**Path parameters** — `id`: `settlement`, `receiver`, `payment-api` or
`notifier`.

**Body** — either field or both; other fields are ignored.

```json
{"connected": false, "shown": false}
```

`shown` is the console's memory and is lost on a console restart;
`connected` is switched where the wiring lives and survives one.
`payment-api` and `notifier` have no wiring: `connected` is accepted and
changes nothing.

**Answers** — `200` with the stand-in read again after the change, shaped
as one entry of `GET /api/stand-ins`, plus `errors` listing wiring that
did not switch. `502` with the same body when nothing took.
`400 want {"connected": true|false} and/or {"shown": true|false}`;
`404 "<id>" is not a stand-in; the lab's are [...]`.

**Reaches** — `settlement POST /sim/connected` `{"connected", "reason"}`;
`banking-circle PUT /api/v1/notificationselfservice/subscription/{id}/activate`
or `/deactivate`, with `If-Match`, per receiver subscription; `aci POST
/sim/connected` (only while ACI delivers to the receiver); then the reads
of `GET /api/stand-ins`.

## Clock

### `GET /api/clock`

The lab clock as it stands: the lab's now, its offset from real time, its
mode, each calendar's business day and any holds.

**Answers** — `200`, the clock service's body:

```json
{"offset_ms": -46018278, "offset_hours": -12.78, "now": "2026-11-06T23:25:52.46Z",
 "mode": "pinned", "pinned": true, "reason": "pinned to 2026-11-05T10:00:00Z",
 "changed_at": "…", "business_day": false,
 "calendars": [{"code": "GB", "zone": "Europe/London", "date": "2026-11-06",
                "weekday": "Fri", "business_day": true}, …],
 "holds": [{"holder": "infinite-local-runner", "reason": "…", "until": "…"}]}
```

`mode` is `real`, `pinned` or `auto-business-day`. `502` when the clock
cannot be read.

**Reaches** — `clock GET /clock`.

### `POST /api/clock`

Move the lab clock, or change its mode. Every vendor follows within a
second, and so does a plugin registered with `"clock": "follows"`.

**Body** — forwarded *verbatim* (up to 8 KiB): exactly one of

```json
{"at": "2026-09-28T07:45:00Z"}
{"advance": "1d"}
{"mode": "real"}
{"mode": "auto-business-day"}
```

plus an optional `"reason"`. `advance` is a number and `d`, `h` or `m`:
`"1d"`, `"-90m"`, `"1.5h"`.

**Answers** — *verbatim* from the clock: `200` with the new clock (as
`GET /api/clock`); `400` with its reason (no field, two fields, a bad
timestamp); `409 clock is held: …` while a run holds it. The console
answers `409` itself, without calling the clock, while a live plugin is
registered with `"clock": "wall"`: `<name> is registered on the wall
clock and cannot follow the lab's; …`. `502` when the clock is unreachable.

**Reaches** — `clock POST /clock`.

## Configuration

### `GET /api/config`

What the lab is set to: the documented environment knobs, Banking Circle's
notification delivery table, and Worldline's file-exchange channel.

**Answers** — `200`, always:

```json
{"groups": [{"title": "Worldline -- the settlement cycle", "note": "…",
             "settings": [{"key": "WORLDLINE_FILE_SOURCE", "service": "worldline",
                           "purpose": "…", "configured": "generate"}, …]},
            …,
            {"title": "This console", "note": "…",
             "settings": [{"key": "LISTEN", "service": "console", "purpose": "…",
                           "configured": "", "live": ":8090"}, …]}],
 "banking_circle": {"retry_schedule": [{"after": "15s"}, {"after": "…", "email": "warning"}, …],
                    "time_scale": 86400, "deactivate_after_retries": 11,
                    "batch_flush_interval": "2s", "default_max_notifications_per_message": 1000,
                    "request_timeout": "10s"},
 "banking_circle_path": "/config/banking-circle.json",
 "banking_circle_source": "live from banking-circle (the file plus any BC_TIME_SCALE override)",
 "worldline_channel": {"merchant_id": "GB00SIM0000000000003", "sftp_directory": "…",
                       "report_types": ["WX", …], "report_formats": ["CSV", …],
                       "cut_off_time": "00:00 CET", "remittance_frequency": "daily",
                       "sftp_directory_count": 3}}
```

- `configured` is what compose.yml ships: documentation, not a reading of
  the running container. `live` is set only for the console's own
  variables; one that looks like a credential reads `(set, hidden)`.
- `banking_circle` is the table Banking Circle is running. When it cannot
  be asked, it is the file at `CONSOLE_BC_DELIVERY_CONFIG`, with
  `banking_circle_source` saying so, and `banking_circle_note` when the file
  is unreadable and the built-in defaults are shown. `banking_circle_error`
  replaces all of it when the file does not parse.
- `worldline_channel_error` replaces `worldline_channel` when Worldline
  cannot be read, including `HTTP 404` before any channel config has been
  saved.

**Reaches** — `banking-circle GET /sim/delivery-config` (bearer), or the
config file; `worldline GET /config`.

### `PUT /api/config/worldline-channel`

Change Worldline's SFT file-exchange channel config, the one setting the
console changes live.

**Body** — forwarded *verbatim*; Worldline decodes it strictly and stores
it whole, so a field left out is cleared.

```json
{"merchant_id": "GB00SIM0000000000003", "sftp_directory": "/sftp/outbound/GB00SIM0000000000003",
 "report_types": ["WX", "Financial"], "report_formats": ["CSV", "XML", "ASCII"],
 "cut_off_time": "00:00 CET", "remittance_frequency": "daily", "sftp_directory_count": 3}
```

`merchant_id` is required.

**Answers** — *verbatim* from Worldline: `200` with the saved config;
`400` with its reason (`merchant_id required`, `json: unknown field …`).
The console answers `400 body must be JSON: …` for a body it cannot parse,
and `502` when Worldline is unreachable.

**Reaches** — `worldline PUT /config`.

## Merchants

### `GET /api/merchants`

The console's merchant registry (distributor → partner → merchant →
outlet), its batches, and the countries it dresses merchants in. A
platform that settles these merchants seeds its own records from this.

**Answers** — `200`

```json
{"distributors": [{"id": "dst_0001", "name": "Simulator Distribution Ltd", "country": "GB",
                   "created_at": "…"}],
 "partners": [{"id": "prt_0002", "distributor_id": "dst_0001",
               "name": "Northwind Partner Services", "country": "NL", "created_at": "…"}],
 "merchants": [{"id": "mer_0003", "partner_id": "prt_0002", "legal_name": "Copperleaf Bakery Ltd",
                "trading_name": "Copperleaf Bakery", "country": "GB", "currency": "GBP",
                "mcc": "5462", "address": {"line1": "…", "city": "London", "post_code": "…", "country": "GB"},
                "email": "…", "status": "active", "batch": "bat_0004",
                "outlets": [{"id": "out_0005", "mid": "…", "name": "Copperleaf Bakery - outlet 1",
                             "address": {…}, "terminal_id": "…", "mcc": "5462",
                             "account_name": "Copperleaf Bakery", "account_number": "…",
                             "financial_institution": "…",
                             "beneficiary_id": "…", "sanctions_status": "pass"}],
                "created_at": "…"}, …],
 "batches": [{"id": "bat_0004", "seed": 42, "count": 50, "max_outlets": 3, "created_at": "…"}],
 "countries": [{"code": "GB", "name": "United Kingdom", "currency": "GBP"}, …],
 "provisioning_ready": true, "provisioning_error": ""}
```

An outlet has `beneficiary_id` and `sanctions_status` once registered at
B4B. Ids come from one counter: `dst_`, `prt_`, `mer_`, `out_`, `bat_`
and four digits. Countries: GB, NL, DE, FR, IE, ES, SE, PL.

**Reaches** — nothing — console state only.

### `POST /api/merchants`

Create a merchant and its opening outlets. Everything except `legal_name`
is optional and derived from the merchant's id when left out: address,
MCC, email, and per outlet a MID, terminal id and payout account.

**Body** — strict.

```json
{"legal_name": "Harbour Coffee Ltd", "trading_name": "Harbour Coffee", "partner_id": "prt_0002",
 "country": "GB", "currency": "GBP", "mcc": "5814", "email": "",
 "address": {"line1": "1 Ledger Lane", "line2": "", "city": "London", "post_code": "E1 6AN", "country": "GB"},
 "outlets": 2}
```

- `legal_name` is required. `outlets` is 0 to 20; 0 means 1.
- `country` is a code from the countries list; anything else is replaced
  by a derived country without an error. `currency` defaults to the
  country's.
- `address` is used only when `line1` is set.

**Answers** — `201` with the merchant, shaped as in `GET /api/merchants`.
`400` for a missing `legal_name`, `outlets` out of range, or a bad body;
`404` for an unknown `partner_id`.

**Reaches** — nothing — console state only.

### `DELETE /api/merchants/{id}`

Delete a merchant from the registry. Nothing registered at B4B or seeded at
Worldline is undone: B4B has no beneficiary-delete endpoint.

**Path parameters** — `id`: a merchant id.

**Answers** — `200 {"deleted": "mer_0003"}`; `404`.

**Reaches** — nothing — console state only.

### `POST /api/merchants/batches`

Generate many merchants at once, drawn from a seed: names, trades and
their MCCs, countries, company forms, outlet counts. The same seed makes
the same population again, with new ids and so new MIDs.

**Body** — strict.

```json
{"count": 50, "max_outlets": 3, "country": "", "seed": 42, "partner_id": ""}
```

- `count`: 1 to 500, required.
- `max_outlets`: 1 to 10; absent or 0 means 3. Most merchants get one.
- `country`: empty mixes the EUR and GBP countries (the currencies Banking
  Circle holds safeguarding accounts in); a code from the countries list
  puts every merchant there; anything else is `400`.
- `seed`: optional; without it the seed derives from the batch's id.
- `partner_id`: optional; must exist.

**Answers** — `201`

```json
{"batch": {"id": "bat_0007", "seed": 42, "count": 50, "max_outlets": 3, "created_at": "…"},
 "merchants": 50, "outlets": 71,
 "note": "50 merchants, 71 outlets — seed 42 makes the same names again."}
```

`400` for the validation above; `404` for an unknown `partner_id`.

**Reaches** — nothing — console state only.

### `DELETE /api/merchants/batches/{id}`

Delete a batch and every merchant in it, from the registry only. B4B and
Worldline keep what they were sent.

**Path parameters** — `id`: a batch id.

**Answers** — `200 {"deleted": "bat_0007", "merchants": 50}`; `404`.

**Reaches** — nothing — console state only.

### `POST /api/merchants/batches/{id}/provision`

Register every outlet of a batch as a B4B beneficiary, merchant by
merchant: `POST /api/merchants/{id}/provision` for the whole batch.

**Path parameters** — `id`: a batch id.

**Answers** — `200`, reported per merchant; a partial run is still `200`.

```json
{"merchants": 50, "done": 49,
 "failed": ["mer_0012: 1 of 2 outlet(s) refused — POST /oversight/v1/beneficiaries: HTTP 422: …"],
 "note": "49 of 50 merchants on the payout rail, 70 outlet(s) registered at B4B"}
```

`404` for an unknown batch. `400` when provisioning is unavailable (the
B4B signing key is unreadable); the call stops at the first merchant. The
whole batch may take up to two minutes.

**Reaches** — `b4b POST /oversight/v1/beneficiaries`, once per outlet,
with an RS512 bearer token.

### `POST /api/merchants/batches/{id}/trading`

Seed card payments at Worldline for every outlet of a batch. Each merchant
trades at its own typical ticket (3.00 to 150.00, derived from its id)
unless `amount_cents` is given.

**Path parameters** — `id`: a batch id.

**Body** — strict; the fields of `POST /api/merchants/{id}/trading`.

```json
{"count": 5}
```

**Answers** — `201`, reported per merchant:

```json
{"merchants": 50, "done": 50,
 "note": "355 card payment(s) at Worldline for 50 of 50 merchants, dated 2026-11-06 — Worldline settles T+1: run its morning cycle to have them in a settlement file."}
```

`failed` lists merchants Worldline refused, or whose parameters were
invalid (`count` over 200); the answer is still `201`. `400` for a bad
body; `404` for an unknown batch.

**Reaches** — `worldline POST /sim/transactions`, once per merchant.

### `POST /api/merchants/{id}/outlets`

Open another outlet, with a new MID, for a merchant. It is not known to
B4B until the merchant is provisioned again.

**Path parameters** — `id`: a merchant id.

**Body** — strict.

```json
{"name": "Harbour Coffee - Quayside"}
```

`name` may be empty: it then becomes `<merchant> - outlet <n>`.

**Answers** — `201 {"merchant": {…}, "outlet": {"id": "out_0009", "mid": "…", …}}`;
`404` for an unknown merchant.

**Reaches** — nothing — console state only.

### `POST /api/merchants/{id}/status`

Set a merchant's status. Registry-only: nothing in the lab acts on it.

**Path parameters** — `id`: a merchant id.

**Body** — strict.

```json
{"status": "suspended"}
```

`status` is `draft`, `active` or `suspended`.

**Answers** — `200` with the merchant; `400` for any other status; `404`.

**Reaches** — nothing — console state only.

### `POST /api/merchants/{id}/provision`

Register each outlet of a merchant as a B4B beneficiary, keyed on its MID
(`external_ref`), and record the beneficiary id and sanctions status on the
outlet. Safe to repeat: B4B corrects the record for a repeated
`external_ref` rather than creating a second payee.

**Path parameters** — `id`: a merchant id.

**Answers** — `200`, per outlet, whether or not each worked:

```json
{"outlets": [{"outlet_id": "out_0005", "mid": "…", "beneficiary_id": "…", "sanctions_status": "pass"},
             {"outlet_id": "out_0006", "mid": "…",
              "error": "POST /oversight/v1/beneficiaries: HTTP 422: …"}]}
```

`404` for an unknown merchant. `400 … b4b signing key unavailable, set
B4B_JWT_PRIVATE_KEY_PATH …` when provisioning is unavailable.

**Reaches** — `b4b POST /oversight/v1/beneficiaries` per outlet,
`{"external_ref", "account_name", "account_number",
"financial_institution", "country"}`, with an RS512 bearer token.

### `POST /api/merchants/{id}/trading`

Tell Worldline the merchant's outlets took card payments. This is the
acquirer's own data: the platform learns of it only through the next
settlement file.

**Path parameters** — `id`: a merchant id.

**Body** — strict; `{}` for all defaults.

```json
{"count": 5, "amount_cents": 2500, "currency": "GBP", "date": "2026-11-06"}
```

- `count`: payments per outlet; default 3, at most 200.
- `amount_cents`: default 2500.
- `currency`: default the merchant's.
- `date`: `YYYY-MM-DD`, passed to Worldline unchecked. The default is
  yesterday (UTC) on the real clock, not the lab clock: with the lab clock
  moved, give `date` so the next cycle covers it.

**Answers** — `201`

```json
{"accepted": 10, "date": "2026-11-06",
 "note": "Worldline settles T+1: run the morning cycle to have these appear in a settlement file."}
```

`400` for `count` over 200, a merchant with no outlets, or a bad body;
`404`; `502` when Worldline refuses or is unreachable.

**Reaches** — `worldline POST /sim/transactions`: one array, `count`
entries per outlet, each `{"mid", "currency", "amount_cents", "date",
"mcc", "terminal_id"}`.

### `POST /api/outlets/{mid}/sanctions`

Move an outlet's beneficiary at B4B between `pass`, `review` and `fail`, to
watch the payout gate stop or release it.

**Path parameters** — `mid`: the outlet's MID.

**Body** — strict.

```json
{"sanctions_status": "fail"}
```

**Answers** — `200 {"mid": "…", "sanctions_status": "fail"}`. `400` when
provisioning is unavailable. `502` when B4B refuses (a status other than
`pass`, `review` or `fail` is B4B's `400`, arriving as `502`) or is
unreachable. `404` when the MID is not in the registry; B4B has already
been changed by then.

**Reaches** — `b4b PUT /sim/beneficiaries/{mid}/sanctions`, with an RS512
bearer token.

### `POST /api/partners`

Add a partner under a distributor.

**Body** — strict.

```json
{"distributor_id": "dst_0001", "name": "Eastgate Partners", "country": "IE"}
```

`name` is required. `distributor_id` may be empty: an existing distributor
is used. No route creates distributors, so there is one, `dst_0001`, unless
the registry file was edited. `country` is not checked: anything but a code
from the countries list is stored as `""`.

**Answers** — `201 {"id": "prt_0010", "distributor_id": "dst_0001", "name": "…", "country": "IE", "created_at": "…"}`;
`400` for a missing name; `404` for an unknown distributor.

**Reaches** — nothing — console state only.

## Banks

### `GET /api/banks`

Both banks and their accounts: the business bank (the merchants' own,
where payouts land) and Banking Circle (the safeguarding accounts and a
creditor account per merchant).

**Answers** — `200`, always; a bank that cannot be read has `error` and
no accounts.

```json
{"banks": [{"id": "core-ledger", "name": "Business bank", "kind": "vendor", "service_id": "bank",
            "summary": "…", "number_label": "IBAN", "can_open_account": true,
            "accounts": [{"id": "acc_alice", "number": "GB00SIM0000000000001",
                          "holder": "Alice Simulator", "currency": "EUR", "balance": "10000.00"}, …]},
           {"id": "banking-circle", "name": "Banking Circle", "kind": "vendor",
            "service_id": "banking-circle", "summary": "…", "number_label": "vIBAN",
            "can_open_account": false, "note": "Banking Circle has no account-opening endpoint -- …",
            "accounts": […]}]}
```

The bank ids are `core-ledger` and `banking-circle`; `service_id` is the
catalogue id.

**Reaches** — `bank GET /accounts`; `banking-circle GET
/api/v1/authorizations/authorize` (Basic for Bearer, cached) and `GET
/accounts`, over mTLS.

### `POST /api/banks/{id}/accounts`

Open an account at the business bank. Banking Circle has no
account-opening endpoint: its accounts appear when first paid into.

**Path parameters** — `id`: `core-ledger`. `banking-circle` answers `501`.

**Body** — strict.

```json
{"holder": "Carol Simulator", "currency": "EUR", "opening_balance": "250.00"}
```

The bank requires `holder` and defaults `currency` to EUR;
`opening_balance` is a decimal string, optional.

**Answers** — `201 {"id": "acc_…", "number": "GB00SIM…", "holder": "Carol Simulator", "currency": "EUR", "balance": "250.00"}`.
`400` for a bad body; `404 unknown bank <id>`; `501` for Banking Circle;
`502` when the bank refuses or is unreachable. A missing `holder` is the
bank's refusal: `502 POST /accounts: HTTP 400: …`.

**Reaches** — `bank POST /accounts` `{"holder", "currency",
"openingBalance"}`.

### `GET /api/banking-circle/subscriptions`

Banking Circle's notification subscriptions: where each delivers, for which
events, its status, and how much is waiting.

**Answers** — `200`

```json
{"subscriptions": [{"id": "sub_46bbc839629e",
                    "endpoint": "http://host.containers.internal:3114/api/v1/banking-circle/webhook",
                    "active": true, "status": "active",
                    "status_message": "Subscription successfully created",
                    "version": 1, "max_per_message": 1000, "mtls": false, "email": "…",
                    "events": [{"type": "IncomingPaymentProcessed", "active": true, "targets": 2}, …],
                    "pending": 0, "paused": false, "queued": 0}, …]}
```

- `status` names Banking Circle's number: `inactive` (1), `active` (2),
  `retired` (4), `none` otherwise. `targets` counts the event's target
  accounts; 0 is all.
- `pending` is what Banking Circle kept when it gave up on a dead endpoint
  and deactivated the subscription (it clears on reactivation). `queued` is
  what a pause holds (it clears on resume). When that read fails they stay
  0 and `false`, silently.
- At most 50 subscriptions. `502` when Banking Circle refuses or is
  unreachable.

**Reaches** — `banking-circle GET
/api/v1/notificationselfservice/subscription?PageSize=50`, then `GET
/sim/subscription/{id}/pending` per subscription.

### `POST /api/banking-circle/subscriptions/{id}/test`

Fire Banking Circle's own test notification at one subscription, and report
what the endpoint did with it, not only that it was sent.

**Path parameters** — `id`: a subscription id.

**Answers** — `200`, within about three seconds:

```json
{"subscription": "sub_46bbc839629e", "delivered": true,
 "result": "batch of 1 to https://receiver:8443/raw-events — PaymentStatus",
 "detail": {"endpoint": "…", …}}
```

When nothing has landed after three seconds: `{"subscription": "…",
"delivered": false, "result": "queued, but nothing had been delivered
three seconds later — …"}`. `result` and `detail` are the newest entry in
Banking Circle's notifications log, which during a run may be another
delivery. `502` when the test call fails, an unknown id included.

**Reaches** — `banking-circle POST
/api/v1/notificationselfservice/clienttest/{id}`, and `GET
/sim/activity?limit=1` before it and every 200 ms after.

### `POST /api/banking-circle/subscriptions/{id}/pause`

Stop Banking Circle delivering to one subscription. Notifications queue in
order, and show in its *Notifications sent* log as queued; payments still
process.

**Path parameters** — `id`: a subscription id.

**Answers** — `200`, Banking Circle's body:
`{"subscriptionId": "sub_…", "endpoint": "…", "paused": true, "queued": 0}`.
`502` when Banking Circle refuses (an unknown id included) or is
unreachable.

**Reaches** — `banking-circle POST /sim/subscription/{id}/pause`.

### `POST /api/banking-circle/subscriptions/{id}/resume`

Release a paused subscription: what it queued goes out oldest first, in
the batches it would have gone out in.

**Path parameters** — `id`: a subscription id.

**Answers** — `200`, Banking Circle's body:
`{"subscriptionId": "sub_…", "endpoint": "…", "paused": false, "released": 2, "queued": 0}`.
`502` as for pause.

**Reaches** — `banking-circle POST /sim/subscription/{id}/resume`.

### `GET /api/banking-circle/payouts`

Banking Circle's outgoing payments, newest first, with whether each can
still be returned or reversed.

**Answers** — `200`

```json
{"payouts": [{"id": "bcp_f8443acbe8db", "reference": "010F100000000689",
              "external_ref": "sttl_v1:…", "amount": "4394.87", "currency": "EUR",
              "state": "OutgoingPaymentProcessed", "created_at": "2026-11-05T10:02:49Z",
              "can_return": true, "can_reverse": true}, …],
 "total": 1016}
```

At most 20 are listed; `total` counts every outgoing payment. Incoming
payments are left out: a return shows as the payout's `returned_by`.
`can_return` and `can_reverse` are true only for an
`OutgoingPaymentProcessed` payout not already returned. `reference` is
Banking Circle's `paymentReferenceNumber`; `external_ref` is the payout's
`settlementId`. `502` when Banking Circle cannot be read.

**Reaches** — `banking-circle GET /payments` (lab introspection, bearer).

### `POST /api/banking-circle/payouts/{id}/return`

The beneficiary's bank sends a processed payout back. The payout stays
processed and records `returnedBy`; the money arrives as a new incoming
payment flagged `return`. Cannot be undone.

**Path parameters** — `id`: a payout id (`bcp_…`).

**Answers** — `200`, Banking Circle's body: the new incoming payment.

```json
{"id": "bcp_…", "amount": "4394.87", "currency": "EUR", "return": true,
 "returnOf": "bcp_f8443acbe8db", "returnedReference": "010F100000000689",
 "paymentReferenceNumber": "…", "state": "IncomingPaymentBooked", "createdAt": "…", …}
```

The console sends no reason code. `502` when Banking Circle refuses (not
processed, already returned, unknown id) or is unreachable.

**Reaches** — `banking-circle POST /sim/payments/{id}/return` on the
credentialed listener (the same handler as the bridge's
`/internal/payments/{id}/return`).

### `POST /api/banking-circle/payouts/{id}/reverse`

The scheme undoes a processed payout: it becomes `Reversed`, with a second
booking. Cannot be undone.

**Path parameters** — `id`: a payout id.

**Answers** — `200`, Banking Circle's body: the payout,
`{"id": "bcp_…", "state": "Reversed", "processedAt": "…", "reversedAt": "…", …}`.
`502` as for return.

**Reaches** — `banking-circle POST /sim/payments/{id}/reverse` on the
credentialed listener.

### `POST /api/actions/fund-sga`

Top up the empty safeguarding accounts: the wire Worldline makes on its
morning slot, for a run that did not come through one. An account already
holding money is left alone, so pressing twice is not twice the money.

**Query parameters** — `amount`: the credit per account as a decimal
string, default `1000000.00`; not checked by the console.

**Answers** — `200`, per currency (EUR, then GBP), partial success
included:

```json
{"results": [{"currency": "EUR", "account": "00000000-0000-4000-8000-000000000978",
              "before": "0.00", "funded": "1000000.00"},
             {"currency": "GBP", "account": "00000000-0000-4000-8000-000000000826",
              "before": "5321.10", "skipped": "already holds 5321.10"}]}
```

`502 {"error": "EUR: …; GBP: …", "results": […]}` when every currency
failed (no `status` field).

**Reaches** — Banking Circle's bridge (`CONSOLE_BC_INTERNAL_URL`,
`:8095`, no auth): `GET /internal/accounts/{id}/balances` per account,
then `POST /internal/incoming-payments` `{"currency", "amount",
"reference"}` for each empty one.

## Vendor actions

### `POST /api/actions/worldline-cycle`

Make Worldline cut and publish its settlement files now rather than at its
slot: one PGP-encrypted file per currency onto its SFTP root and, on the
morning slot only, the matching lump sum wired to the safeguarding
account.

**Query parameters** — `slot`: `morning` (default; also `er`) or
`afternoon` (also `ar`, `confirmation`). `date`: `YYYY-MM-DD`, settle that
day's transactions; without it, the transactions due by Worldline's lab
clock (T+1: yesterday). Both are passed on unescaped.

**Answers** — *verbatim* from Worldline:

```json
{"at": "…", "slot": "ER", "from_date": "2026-11-05", "to_date": "2026-11-05",
 "files": [{"filename": "…", "slot": "ER", "mids": ["…"], "currency": "EUR",
            "amount_cents": 439487, "items": 30, "from_date": "…", "to_date": "…"}, …],
 "lump_sums": [{"currency": "EUR", "amount_cents": 439487, "reference": "…",
                "account_id": "…", "status": "…"}],
 "published": ["…"]}
```

`200`; `207` with `errors` when some files did not publish; `400` for a
bad `slot` or `date`. `502` when Worldline is unreachable or its body is
not JSON.

**Reaches** — `worldline POST /sim/settlement-cycle/run?slot=&date=`.

### `POST /api/actions/settlement-pull`

Make the settlement stand-in pull Worldline's SFTP now rather than on its
timer. Works while the stand-in is disconnected: that stops only its
timers.

**Answers** — *verbatim* from settlement:

```json
{"pulled": 1,
 "files": [{"name": "…", "slot": "…", "pulled_at": "…", "bytes": 4096, "archived_at": "…",
            "processed": true, "settlement_ids": ["…"], "payouts_by_mid": 6}, …]}
```

`files` is the whole pull history, not only this pull. `502 pull from
<host>:<port> failed: …` from settlement when SFTP fails; `502` from the
console when settlement is unreachable.

**Reaches** — `settlement POST /worldline/pull`.

## Plugins

### `GET /api/plugins`

The registered platforms, each as a catalogue entry plus how its
registration stands. Registrations live in memory: after a console restart
the list is empty until each plugin renews.

**Answers** — `200`

```json
{"plugins": [{"id": "infinite-local-runner", "name": "Local runner", "kind": "platform",
              "summary": "…", "base_url": "http://host.containers.internal:3109",
              "browse_url": "http://127.0.0.1:3109", "health_path": "/status",
              "activity_path": "/sim/activity", "endpoints": […],
              "plugin": {"clock": "follows", "start_hint": "…", "actions": […],
                         "settlement": {…}, "empty_hints": {…}, "state": "live",
                         "registered_at": "…", "last_seen": "…", "ttl_seconds": 30}}, …]}
```

`state` is `live` (renewed within 30 s), `lapsed` or `stopped`. In
registration order. No health here: `GET /api/overview` has it.

**Reaches** — nothing — console state only.

### `POST /api/plugins`

Register a platform's card, or renew it: the same call every
`renew_seconds`, and a changed descriptor replaces the old one.
[../plugins.md](../plugins.md) is the contract.

**Body** — the descriptor, up to 64 KiB; unknown fields are ignored.

```json
{"id": "my-platform", "name": "My platform", "summary": "…",
 "base_url": "http://host.containers.internal:3109", "browse_url": "http://127.0.0.1:3109",
 "health_path": "/health", "activity_path": "/sim/activity", "clock": "follows",
 "start_hint": "…",
 "actions": [{"id": "sweep", "label": "Run reconciliation sweep", "path": "/sim/sweep", "primary": true}],
 "settlement": {"files_path": "/sim/files", "run_path": "/sim/run", "status_path": "/status",
                "runs_log": "runs"},
 "stand_ins": {"settlement": {"connected": false, "shown": false}}}
```

Refused with `400 bad plugin descriptor: …` unless:

- `id` matches `^[a-z0-9][a-z0-9-]{0,40}$` and is not a lab service's id;
  `name` is set; `clock` is `follows` or `wall`;
- `base_url`, and `browse_url` if set, is an http(s) URL with a host;
- `health_path` is set, and every path is absolute on `base_url` (no
  `//` prefix, `..`, whitespace or backslash);
- there are at most 8 actions, ids unique, each with a label, `method`
  `POST` (the default), `PUT` or `DELETE`, and at most 6 valid `fields`;
- `settlement`, if present, has `files_path` and `run_path`;
- `runs_log` and the `empty_hints` keys are log names, and `stand_ins` names
  only stand-ins.

**Answers** — `200 {"id": "my-platform", "state": "live", "ttl_seconds": 30, "renew_seconds": 10, "clock": "follows"}`.
`400 descriptor: …` for a body that is not JSON.

**Reaches** — nothing before answering. After: the plugin's `health_path`
(an immediate probe) and, when `stand_ins` differs from what was last
applied for this plugin, the stand-in wiring as in
`POST /api/stand-ins/{id}`.

### `DELETE /api/plugins/{id}`

Unregister on a clean shutdown. The card stays, grey and `stopped`, showing
`start_hint`; the plugin is no longer probed and its buttons answer `409`.

**Path parameters** — `id`: a plugin id.

**Answers** — `204`; `404 no plugin <id>`.

**Reaches** — nothing — console state only.

### `POST /api/plugins/{id}/actions/{action}`

Press one of the plugin's declared buttons: the body goes to the method and
path the plugin declared for it.

**Path parameters** — `id`: a plugin id. `action`: an action id from its
descriptor.

**Body** — anything, forwarded as is (up to 8 KiB), as
`application/json` when not empty. For an action with fields,
`{"<field id>": value, …}`.

**Answers** — *verbatim* from the platform; a non-JSON body arrives as
`{"output": "<text>"}`, an empty one as `{}`. `404` for no such plugin or
`<name> declares no action <action>`; `409 <name> has stopped; start it
again first`; `502` when the platform is unreachable. A lapsed plugin is
still called.

**Reaches** — the plugin's `base_url` + the action's `path`, with the
action's method.

### `GET /api/plugins/{id}/files`

The settlement files the platform can run: the files table on its card.

**Path parameters** — `id`: a plugin id.

**Answers** — `200`, the platform's body, in the shape
[../plugins.md](../plugins.md#settlement) fixes:

```json
{"dir": "…", "files": [{"name": "…csv", "currency": "EUR", "mids": ["1", "2"],
                        "total": "6660.27", "default": true, "seeded": true,
                        "missing_mids": [], "in_flight": null, "last_run": {…}}, …],
 "seeded_error": null}
```

`404` for no such plugin, or `<name> runs no settlement files`; `409` when
stopped; `502` when the platform is unreachable or answers non-2xx
(`GET <url>: HTTP <code>`, its body dropped).

**Reaches** — the plugin's `base_url` + `settlement.files_path`.

### `POST /api/plugins/{id}/run`

Start settlement runs on the platform. Several files in one call start
together; one file per currency.

**Path parameters** — `id`: a plugin id.

**Body** — forwarded verbatim (up to 8 KiB).

```json
{"files": ["worldline-reconciliation-sample-gbp.csv"]}
```

**Answers** — *verbatim* from the platform: `202 {"runs": [{"currency": "GBP", "run": "…"}]}`,
or its `409` for a currency already running. `404`, `409` and `502` as for
`GET /api/plugins/{id}/files`.

**Reaches** — `POST` the plugin's `base_url` + `settlement.run_path`.

### `POST /api/plugins/{id}/files`

Upload a settlement file from anywhere to the platform, which keeps it and
lists it with its own. The bytes are streamed through; the console holds
none of them.

**Path / query parameters** — `id`: a plugin id. `name`: the file's name,
required.

**Body** — the raw file, at most `CONSOLE_UPLOAD_MAX_MB` (512) MB; sent on
as `application/octet-stream`.

**Answers** — *verbatim* from the platform:
`201 {"name": "…", "bytes": 123456, "note": "…"}`, `name` being what it
was kept as. `400 name the file: ?name=<file name>`; `413` over the limit;
`501 <name> does not offer uploads` without `upload_path`; `404`, `409`
and `502` as above. No timeout but the caller's own.

**Reaches** — `POST` the plugin's `base_url` + `settlement.upload_path?name=`.

### `DELETE /api/plugins/{id}/files`

Delete a file on the platform: any listed file it marks `deletable` when
it declares `delete_path`, otherwise only files uploaded to it.

**Path / query parameters** — `id`: a plugin id. `name`: the file's name
(forwarded as given, even empty).

**Answers** — *verbatim* from the platform: `204`; `403` with its reason
for a file it will not delete; `404` for a name it does not list. `501`
when it declares neither `delete_path` nor `upload_path`; `404`, `409` and
`502` as above.

**Reaches** — `DELETE` the plugin's `base_url` + `settlement.delete_path?name=`,
or `settlement.upload_path?name=` without one.

### `GET /api/plugins/{id}/files/preview`

The first lines of one settlement file, read no further however long it
is.

**Path / query parameters** — `id`: a plugin id. `name`: the file's name.
`lines`: 1 to 1000, default 100; above 1000 is 1000, anything else 100.

**Answers** — *verbatim* from the platform:
`200 {"name": "…", "lines": ["…", …], "truncated": true, "long_lines": 0, "bytes": 123456}`.
`501` without `preview_path`; `404`, `409` and `502` as above.

**Reaches** — `GET` the plugin's `base_url` + `settlement.preview_path?name=&lines=`.

### `POST /api/plugins/{id}/files/reveal`

Open the folder a file is in, in the file manager of the machine the
platform runs on. Only the platform can: the console may be in a
container, and a web page may not open a local folder.

**Path parameters** — `id`: a plugin id.

**Body** — forwarded verbatim (up to 8 KiB).

```json
{"name": "worldline-reconciliation-sample-gbp.csv"}
```

**Answers** — *verbatim* from the platform:
`200 {"path": "…", "dir": "…", "note": "…"}`, or `409` with the path when
that machine has no desktop. `501` without `reveal_path`; `404`, `409` and
`502` as above.

**Reaches** — `POST` the plugin's `base_url` + `settlement.reveal_path`.

## Connect

### `GET /api/connect`

What each vendor asks of a platform that connects to it: the variables it
sets, and the key files on offer and whether they exist yet. Names only, no
values: those are in the `.env` and file downloads.

**Query parameters** — `host`: the hostname or IP the platform reaches the
lab at (letters, digits, `.`, `:` and `-`). Default: the host this request
came in on, or `127.0.0.1`.

**Answers** — `200`

```json
{"host": "127.0.0.1",
 "kits": [{"service": "worldline", "title": "Worldline", "note": "…",
           "vars": [{"key": "WORLDLINE_SFTP_HOST"},
                    {"key": "WORLDLINE_SFTP_HOST_KEY_FINGERPRINT", "note": "…"},
                    {"key": "INFINITE_PGP_PRIVATE_KEY", "note": "…",
                     "missing": "keys/wlsftp-keys/worldline_private.asc is not there yet — …"}, …],
           "files": [{"name": "worldline-pgp-public.asc", "purpose": "…", "present": true}, …]},
          …]}
```

Kits, in order: `worldline`, `b4b`, `banking-circle`, `aci`,
`verification`, `verify`. A var with `"optional": true` is written
commented out in the `.env`. `400 host must be a hostname or an IP
address`.

**Reaches** — nothing — console state and the keys directory
(`CONSOLE_KEYS_DIR`).

### `GET /api/connect/env`

Download `fintechlab.env`: every vendor's variables with their values, for
a platform's `.env`.

**Query parameters** — `host`: as for `GET /api/connect`.

**Answers** — `200`, `text/plain`, `Content-Disposition: attachment;
filename="fintechlab.env"`, `Cache-Control: no-store`:

```
# fintech sim lab v1 — connection settings for a platform under test
# Downloaded from the lab console 2026-11-07T12:00:00Z, for a lab reached at 127.0.0.1.
…
# ── Worldline ───────────────────────────────────────────────────────
…
WORLDLINE_SFTP_HOST=127.0.0.1
WORLDLINE_SFTP_PORT=2222
…
# NODE_EXTRA_CA_CERTS=./fintechlab-ca.pem
```

Keys and certificates are read from the keys directory at request time;
addresses, account ids and credentials are what compose.yml ships.
Multi-line values are double-quoted. A key not generated yet is a comment
on its variable, never a value. `400` (JSON) for a bad `host`.

**Reaches** — nothing — console state and the keys directory.

### `GET /api/connect/env/{service}`

Download one vendor's part of `fintechlab.env`, as
`fintechlab-<service>.env`.

**Path / query parameters** — `service`: `worldline`, `b4b`,
`banking-circle`, `aci`, `verification` or `verify`. `host`: as for
`GET /api/connect`.

**Answers** — `200` as for `GET /api/connect/env`. `404 <service> has
nothing a platform needs to connect`; `400` for a bad `host`.

**Reaches** — nothing — console state and the keys directory.

### `GET /api/connect/files/{service}/{name}`

Download one key or certificate file, the one the lab is running with now.
Only client-side material is offered.

**Path parameters**

| `service` | `name` |
| --- | --- |
| `worldline` | `worldline-pgp-public.asc`, `worldline-pgp-private.asc`, `worldline-sftp-host-key.pub` |
| `b4b` | `b4b-jwt-private.pem`, `b4b-jwt-public.pem` |
| `banking-circle` | `fintechlab-ca.pem`, `bc-client.pem`, `bc-client-key.pem` |

**Answers** — `200`, `application/octet-stream`, `Content-Disposition:
attachment; filename="<name>"`, `Cache-Control: no-store`. `404 <service>
has no keys to hand out` for a service with no kit; `404 no such file in
this kit: …` for a name its kit does not offer (every name, for `aci`,
`verification` and `verify`); `503 <name> is not available: …` when the
file is not generated yet or the keys directory is not mounted.

**Reaches** — nothing — the keys directory.
