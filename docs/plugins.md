# Plugins: putting your platform in the lab

The lab simulates vendors. The system under test is somebody else's, so
its card in the console is too: a platform **registers itself** with the
lab API, which forwards its buttons and reads its logs, and the console
draws its card and puts it in the Dashboard's diagram. The lab knows no platform by name. A new
button on a platform is a change to that platform, never a release of the
lab.

buddy's `infinite-local-runner` is the reference plugin. Its generic half —
the descriptor types, register/renew/unregister, the clock follower and the
clock shim, activity logs, the `fintechlab.json` config — is the library
`fintechlab-runner` (`@fintechlab/runner`, a sibling repo), which
any Node platform can use to implement this contract; its README has a
minimal example.

Two things a platform does with the lab, both outbound from the platform:

| | Call | Why |
| --- | --- | --- |
| Register | `POST {lab}/v1/plugins`, renewed every `renew_seconds` | its card, buttons and diagram |
| Follow the clock | `GET {lab}/v1/clock`, every second | its business dates agree with the vendors' |

Neither needs the lab to reach the platform first, so both work from a
laptop to a lab on another machine. What does need the lab to reach the
platform is the card's live parts — its health, its logs, its buttons —
which the lab API calls at the `base_url` the plugin gave (see *Reaching the
platform*).

`{lab}` is the lab API, `http://127.0.0.1:8097` from the host
([api/lab-api.md](api/lab-api.md#plugins)): one address, and, when the lab
asks for one, one token (`Authorization: Bearer …`). Every route below also
answers at the console's old path, `{console}/api/plugins…` on `:8090`,
which the console forwards to the lab API with the token added, and the
clock service still answers `GET /clock` on `:8096`: a runner that knows
only those addresses keeps working, until phase 4 of
[the lab API plan](lab-api/plan.md) retires the old paths.

## Environments

A lab can serve several platforms, each in an **environment** of its own
([lab-api/environments.md](lab-api/environments.md)). For a plugin, three
things follow:

- **A card is its environment's.** Register with that environment's token
  (its kit's `FINTECH_SIM_LAB_TOKEN`), and the card shows when the console
  shows that environment. The admin token registers into the environment it
  names (`Lab-Environment`), or the default. A plugin id is one platform's
  across the lab: registering one another environment holds is refused.
- **A card is optional.** An environment is green once a vendor has seen
  its platform log in, with no heartbeat. Register only for buttons,
  settlement files or logs. A lapsed card greys out its buttons, not its
  environment.
- **`stand_ins` is the default environment's.** The stand-ins play the
  default's platform, so only a default-environment card switches them. An
  environment of its own never meets them.

## Register

```http
POST /v1/plugins
Content-Type: application/json

{ …descriptor… }
```

```json
{"id": "infinite-local-runner", "state": "live", "ttl_seconds": 30, "renew_seconds": 10, "clock": "follows"}
```

Send the same descriptor again every `renew_seconds`. It is the same call:
registering is renewing, and a changed descriptor replaces the old one. A
registration not renewed within `ttl_seconds` is **lapsed**; the card stays,
grey. On a clean shutdown:

```http
DELETE /v1/plugins/{id}          → 204
```

and the card goes grey at once, **stopped**, showing the plugin's
`start_hint`. The lab API keeps registrations in memory: after a lab API
restart the card is back within one renewal.

`400` carries the reason for a descriptor it refuses.

## The descriptor

The first block is a catalogue entry — the same fields every vendor card is
drawn from.

| Field | | |
| --- | --- | --- |
| `id` | required | lower-case letters, digits, dashes; not one of the lab's own service ids |
| `name` | required | the card title |
| `summary` | | one paragraph: what this platform is |
| `base_url` | required | where **the lab API** reaches the platform (http/https) |
| `browse_url` | | where a **person's browser** does; the card's *Open* link |
| `health_path` | required | a `2xx` here is *up* |
| `activity_path` | | logs in the lab's activity shape (below); one panel per log |
| `ports`, `transport`, `auth`, `swap_for`, `docs` | | shown on the card, as for a vendor |
| `endpoints` | | `[{"method", "path", "note"}]`, the table at the foot of the card |
| `clock` | required | `"follows"` or `"wall"` — see *The clock* |
| `start_hint` | | shown when it is not running: how to start it |
| `actions` | | buttons, below |
| `settlement` | | a platform that settles Worldline files, below |
| `empty_hints` | | `{"<log name>": "what would put something here"}` |
| `stand_ins` | | how the lab's stand-ins should be, below |

Every path is **a path on `base_url`**: absolute, no `..`, no host. The
lab API appends it to `base_url` and calls nothing else, which is what
makes forwarding a button safe. Every string is text; the console escapes
it. A plugin cannot bring markup — if it needs a component the console
does not have, the component goes into
[design-system.md](design-system.md) first, for every plugin.

### Actions

```json
"actions": [
  {"id": "sweep", "label": "Run reconciliation sweep", "path": "/sim/sweep",
   "primary": true, "note": "Started — the outcome lands in Reconciliation sweeps."}
]
```

| Field | |
| --- | --- |
| `id`, `label`, `path` | required; ids unique |
| `method` | `POST` (default), `PUT` or `DELETE` — an action changes something |
| `primary` | the card's filled button |
| `confirm` | asked first, in these words |
| `note` | the toast when the platform's answer has no `note` of its own |

| `fields` | inputs asked for first, in a form — below |

The button calls `POST /v1/plugins/{id}/actions/{action}`; the lab API
forwards the body to `method base_url+path` and answers with the platform's
status and body, verbatim. Buttons show only while the platform is up. At
most eight.

#### Action fields

An action that needs input declares it, and the button opens a form in a
modal instead of calling at once:

```json
{"id": "seed", "label": "Seed merchants", "path": "/sim/seed",
 "fields": [
   {"id": "merchants", "label": "Merchants", "type": "number", "default": 50, "min": 1, "max": 5000},
   {"id": "mode", "label": "Existing merchants", "type": "select", "default": "add",
    "options": [{"value": "add", "label": "Keep, add more"}, {"value": "reseed", "label": "Replace"}]},
   {"id": "dir", "label": "Write to", "type": "text", "placeholder": ".runs/generated",
    "hint": "a directory on the platform's machine"}
 ]}
```

| Field | |
| --- | --- |
| `id` | required; lower-case letters, digits, underscores; unique in the action. The key in the body |
| `label` | required |
| `type` | `number`, `text` or `select` |
| `default` | a number for `number`, text otherwise; a select's must be one of its options |
| `min`, `max` | `number` only |
| `placeholder` | `text` only |
| `options` | `select` only: 1–20 `{"value", "label"}` |
| `hint` | a sentence under the form, naming the field |

At most six. The form's submit is the action's label; `confirm`, if any, is
asked after the form. The body is `{"merchants": 50, "mode": "add"}` —
numbers as numbers, a field left empty left out, a number outside
`min`/`max` refused before anything is sent. What the values mean is the
platform's business: it validates them itself and answers `400` with its
reason, which the console shows verbatim.

### Settlement

A platform that settles Worldline files gets the files table on its card
and is the platform the Dashboard's diagram draws:

```json
"settlement": {
  "files_path": "/sim/files",
  "run_path": "/sim/run",
  "status_path": "/status",
  "upload_path": "/sim/files/upload",
  "preview_path": "/sim/files/preview",
  "delete_path": "/sim/files",
  "reveal_path": "/sim/files/reveal",
  "runs_log": "runs",
  "stages": {
    "ingest": ["SETTLEMENT_FILE_INGESTION", "DAILY_MOVEMENT_PROCESSING"],
    "reports": ["DAILY_SETTLEMENT_REPORT", "…"]
  }
}
```

`GET files_path` (the lab API proxies it at
`GET /v1/plugins/{id}/files`):

```json
{"dir": "apps/settle-ingest/local",
 "files": [{"name": "…csv", "currency": "EUR", "mids": ["1","2"], "total": "6660.27",
            "default": true, "seeded": true, "missing_mids": [], "seed_command": "…",
            "in_flight": null, "last_run": {"root": "…", "payouts": {"count": 6, "total": "…"},
                                          "finished_at": "…", "error": null}}],
 "seeded_error": null}
```

A file may also carry `"uploaded": true`, `"bytes"`, and `"problem"` —
why it cannot be run (not a Worldline file) — and the answer
`"upload_dir"`, where uploads are kept. A file with a `problem` gets no
*Run*. `"deletable": true` gives the row *Delete* (through `delete_path`,
below), and `"location"` — the directory it is in, as the platform names
it — is shown under the file's name when it is not the answer's `dir`:
files can live in more than one place, and the operator should not have to
guess which.

The table runs one file per row (*Run*), or several ticked together (*Run N
selected*) — the console lets only one file per currency be ticked at a
time, since that is the platform's rule, and the platform's `409` is still
the last word.

**Files from anywhere** (optional; no `upload_path`, no upload button):

| Lab API | Forwarded to | |
| --- | --- | --- |
| `POST /v1/plugins/{id}/files?name=<file>` | `POST upload_path?name=` | the raw bytes, streamed — the lab API holds none of it and caps it at `CONSOLE_UPLOAD_MAX_MB` (512). Answer `201 {"name", "bytes", "note"}`; `name` is what it was kept as, `note` the toast |
| `DELETE /v1/plugins/{id}/files?name=` | `DELETE upload_path?name=` | forget an uploaded file; `204` |
| `GET /v1/plugins/{id}/files/preview?name=&lines=` | `GET preview_path?name=&lines=` | the head of any listed file, `lines` clamped to 1–1000: `{"name", "lines": […], "truncated", "long_lines", "bytes"}` |

**Delete and open location** (both optional; no path, no button):

| Lab API | Forwarded to | |
| --- | --- | --- |
| `DELETE /v1/plugins/{id}/files?name=` | `DELETE delete_path?name=` | delete a listed file the platform marked `deletable` — its generated files, say, never a fixture its repo tracks. `204`; `403` with the platform's reason for one it will not delete; `404` for a name it does not list. Without `delete_path` this goes to `upload_path` and only uploads can be removed |
| `POST /v1/plugins/{id}/files/reveal` | `POST reveal_path` `{"name"}` | *Open location*: show the file in the file manager **of the machine the platform runs on**. Only the platform can — the lab API may be in a container, and a web page may not open a local folder. `200 {"path", "dir", "note"}`, the `note` being the toast; `409` with the path when there is no desktop to show it on (a server, an SSH session) |

`fintechlab-runner` has both: `deleteHandler` (the platform says which
files may go, and why not) and `revealHandler` (`open -R` on macOS,
`explorer /select,` on Windows, the freedesktop FileManager1 D-Bus call
on Linux with `xdg-open` on the folder as the fallback).

The platform keeps uploads where it likes and lists them with the rest.
`fintechlab-runner` has all three: `FileStore` (streamed to disk under a
safe, free name), `uploadHandler` and `previewHandler` (reads no further
than the lines it returns). buddy's runner keeps them in
`infinite-local-runner/.runs/uploads/`.

`POST run_path` with `{"files": ["…csv", …]}` — the lab API's
`POST /v1/plugins/{id}/run` forwards it — answers `202`
`{"runs": [{"currency", "run"}]}`, or the platform's own `409` for a
currency already running.

`GET status_path`:

```json
{"running_now": null,
 "last_run": {"id": "…", "root": "…", "started_at": "…", "finished_at": "…", "error": null,
              "stages": [{"stage": "SETTLEMENT_FILE_INGESTION", "status": "COMPLETED"}],
              "payouts": {"count": 6, "total": "27698.78", "statuses": {"SUCCESS": 6}}}}
```

`stages` maps the diagram's two platform-internal arrows — `ingest` and
`reports` — to the platform's own stage names. `runs_log` names the
activity log whose total is the diagram's *runs*.

### Activity logs

`GET activity_path` returns the shape every vendor serves
(`internal/activity`):

```json
{"logs": [{"name": "runs", "title": "Settlement runs", "note": "…", "total": 4,
           "labels": {"warn": "with failed stages", "bad": "failed"},
           "last": {…}, "events": [{"seq", "at", "op", "summary", "status", "detail", "peer"}]}]}
```

`status` is `ok`, `warn` or `bad`; `at` is wall-clock time.

An event that has parts — a settlement run and its stages — may carry
`steps`, which the console draws as a table under it:

```json
"steps": [{"name": "SETTLEMENT_FILE_INGESTION", "status": "ok",
           "started_at": "2026-10-01T00:15:11.083Z", "finished_at": "2026-10-01T00:15:27.140Z"},
          {"name": "BC_ACCOUNT_BALANCE_CHECK", "status": "running",
           "started_at": "2026-10-01T00:15:35.425Z", "note": "waiting on funding"}]
```

A step's `status` is an event status or `running`; `finished_at` is absent
while it runs. The times are the emitter's own clock — a platform that
follows the lab clock stamps them on that — so the panel shows how long
each took rather than lining them up with the event's `at`.

### Stand-ins

The lab's **stand-ins** played the platform's part before one plugged in:
`settlement` pulls Worldline's files on a timer and pays outlets at the
daily cutoff; `receiver` is where Banking Circle's seeded subscription and
ACI deliver. `payment-api` and `notifier` are stand-ins nothing calls on its
own — only `make demo-payment` and the e2e suite do. Their cards start
hidden in the console (Configuration → *Stand-ins* has the switches); their
wiring starts connected, so the lab runs end to end with no platform at
all. A platform that replaces them says so:

```json
"stand_ins": {
  "settlement": {"connected": false, "shown": false},
  "receiver":   {"connected": false, "shown": false}
}
```

- **`connected: false`** stops the wiring the stand-in owns, where it lives:
  the settlement service's timers, the receiver's Banking Circle
  subscriptions (deactivated through the bank's API) and ACI's delivery
  when ACI points at the receiver (held, not sent). `true` puts it back.
- **`shown: false`** hides its card and takes it out of the diagram.

Either field may be left out, and so may any stand-in: absent is "leave it
as it is". Anything but a stand-in's id is a `400` — a plugin cannot hide a
vendor. The console applies `stand_ins` when the plugin first registers and
whenever the value changes, **not on every renewal**, so a developer's
*Reconnect* in the console holds until the plugin's wishes change. Each
stand-in card says who set it last. The same switches are
`POST /v1/stand-ins/{id}` for anyone else (see [console.md](console.md)).

buddy's runner sends it when `infinite-local-runner/fintechlab.json` exists
(`cp fintechlab.example.json fintechlab.json`: both stand-ins disconnected
and hidden); with no file it sends none.

## The clock

The lab owns one business clock: the `clock` service, `:8096`. Every
vendor follows its offset, so a payout booked "on Monday" by the platform
books on Monday at the bank.

- **`"clock": "follows"`** — the platform polls `GET {lab}/v1/clock` (the
  clock service's `GET /clock`, through the lab API) and applies
  `offset_ms` (always whole milliseconds, so `Date.now()` plus it is still
  an integer) to its own business time (buddy's runner writes it to the
  file its clock shim, `@fintechlab/runner/clock-shim`, watches). Only the offset changes hands, so a
  follower anywhere is right to within its poll.
- **`"clock": "wall"`** — the platform cannot be moved (a deployed stack).
  While one is live, the console refuses to move the lab clock: the
  vendors would be on another day from the platform they serve.

A lab deployed for such a platform usually runs **without a clock** at all
(the cloud profile, [deploy-cloud.md](deploy-cloud.md)): there is no
`:8096`, `GET {lab}/v1/clock` answers `{"absent": true, "mode": "real",
"offset_ms": 0, "now": …}`, a move or a hold is a `501` and a release a
`404`. A follower reads offset zero and keeps the real time, which is right.
Register with `"wall"`.

A platform that must not have the clock moved under it — a settlement run
in flight — **holds** it:

```http
POST {lab}/v1/clock/holds   {"holder": "infinite-local-runner", "reason": "settlement in flight: EUR …", "ttl_seconds": 900}
DELETE {lab}/v1/clock/holds/infinite-local-runner
```

Moves are refused with `409` and the holder's reason until it is released
or the hold lapses (at most an hour), so a platform killed mid-run cannot
freeze the lab.

`GET {lab}/v1/clock`:

```json
{"offset_ms": -225000000, "offset_hours": -62.5, "now": "2026-09-25T09:00:00Z",
 "mode": "pinned", "reason": "…", "business_day": true,
 "calendars": [{"code": "GB", "zone": "Europe/London", "date": "2026-09-25", "weekday": "Fri", "business_day": true}],
 "holds": []}
```

`POST /clock` takes one of `{"at": "<RFC 3339>"}`, `{"advance": "1h"}`,
`{"mode": "real"}` or `{"mode": "auto-business-day"}` (the most recent
instant that is a business day on every calendar, recomputed as time
passes). The console's clock card makes the same calls through the lab
API's `/v1/clock`.

## Reaching the platform

Three addresses, because three different parties dial them. For buddy's
runner:

| Who dials | Variable (runner) | Default |
| --- | --- | --- |
| the platform → the lab API: registration, the clock, lab hooks | `FINTECH_SIM_LAB_URL`, and `FINTECH_SIM_LAB_TOKEN` (or `LAB_API_TOKEN`) when the lab asks for one | `http://127.0.0.1:8097` |
| the lab API → the platform | `RUNNER_ADVERTISED_URL` → `base_url` | `http://host.containers.internal:3109` |
| a browser → the platform | `RUNNER_BROWSE_URL` → `browse_url` | `http://127.0.0.1:3109` |

`host.containers.internal` is how the containerised lab API reaches the
host. With the lab on another machine, the first points at that machine
and `base_url` must be an address it can reach — a tunnel (cloudflared,
ngrok, pinggy) to the platform's control port does it. Registration and
the clock need no tunnel; only the card's live parts do.

## Trust

The lab API has no authentication by default, like every lab service.
Anyone who can reach it — or the console, which forwards to it — can
register a plugin, and the lab API will then call that plugin's
`base_url` — only on the declared paths, only with the methods above.
`LAB_API_TOKEN` on the lab API makes it ask every caller for a bearer
token (`LAB_API_TOKEN=… make up`, or `make pod-up`); the console adds the
token for whoever reaches the console, so that is the address to keep
private. A platform registering through the console, as the runner does,
needs no token of its own. Run the lab where only the people using it can reach it
([deploy-pod.md](deploy-pod.md)).
