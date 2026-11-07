# Lab clock — API

The lab's business clock: wall time moved by one offset, which every vendor
in the lab follows and a registered platform can follow too, so "what
happens on a Sunday, and then on Monday with the same money" is two
requests rather than a restart of every process. It is lab
infrastructure. [catalogue.md](../../catalogue.md) files it on the
**platform** side because there is nothing to swap it for: real vendors
are on the real clock. Vendors and a platform registered with
`"clock": "follows"` should read it. Nothing should treat it as a
scheduler: it says what time the lab thinks it is, and each follower
decides what that means for its own work.

## At a glance

| | |
| --- | --- |
| Listens | `:8096`, plain HTTP. Compose service `clock` (`http://clock:8096`); host `http://127.0.0.1:8096` |
| Auth | None |
| Found with | `LAB_CLOCK_URL` on bank, settlement, worldline, banking-circle, b4b, aci and verify: the full `http://clock:8096/clock`, not a base URL.<br>`CONSOLE_URL_CLOCK` (`http://clock:8096`) on the console.<br>A platform's own variable; buddy's runner uses `FINTECH_SIM_LAB_CLOCK_URL` (`http://127.0.0.1:8096/clock`) |
| Activity logs | `changes` (*Clock changes*): every move (op `clock.real`, `clock.pinned` or `clock.auto-business-day`), every move refused under a hold (`clock.refused`, amber), every new hold (`hold.take`) and every release (`hold.release`). A renewed hold, a hold lapsing and a malformed request are not logged |
| Configuration | `CLOCK_START` (unset: the real clock; compose: `auto-business-day`): `real`, `auto-business-day`, or an RFC 3339 instant to pin to. Anything else stops the service at start.<br>`CLOCK_CALENDARS` (`GB=Europe/London,SE=Europe/Stockholm`): the calendars a business day must satisfy, `CODE=IANA zone`, comma-separated.<br>`CLOCK_HOLIDAYS_<CODE>` (empty): that calendar's bank holidays as `YYYY-MM-DD`, separated by commas, spaces or newlines. Compose sets GB and SE for 2026 and 2027.<br>`LISTEN` (`:8096`).<br>All state is in memory: a restarted clock is back on `CLOCK_START`, with no holds |
| Design record | [plugins.md](../../plugins.md#the-clock) (the follower contract), [console.md](../../console.md#the-lab-clock) (the clock card), [agent.runbooks.md](../../../agent.runbooks.md#the-clock), and the package comments in `internal/labclock/server.go` and `follow.go` |

## Service surface

Errors are `{"error": "<message>", "status": <code>}`. Bodies are JSON, at
most 8 KiB; unknown fields are ignored.

**Modes.** Business time is the wall clock plus an offset, and the mode
decides the offset:

| Mode | Offset | Set by |
| --- | --- | --- |
| `real` | zero | `{"mode": "real"}` |
| `pinned` | a fixed offset. The lab clock keeps running from the instant it was set to; it is not frozen | `{"at": …}` or `{"advance": …}` |
| `auto-business-day` | recomputed on every read: the latest instant at or before the real time that is a business day on every calendar, found by stepping back one hour at a time for up to six days (zero if none is found) | `{"mode": "auto-business-day"}` |

A business day is a weekday that is not a listed holiday, read in each
calendar's own zone, on every calendar at once. With no calendars
configured every day is one.

**Following.** A follower needs one field, `offset_ms` from `GET /clock`:

- Poll `GET /clock` (the lab's own followers do it every second) and add
  `offset_ms` to your own wall clock to get business time. It is always a
  whole number of milliseconds, so `Date.now() + offset_ms` stays an
  integer. Only the offset travels, so a follower anywhere is right to
  within its poll.
- Move business time only: booking dates, file names, report and balance
  dates. Token lifetimes, retry backoff, delivery windows and timeouts stay
  on the real clock.
- If the clock cannot be read, keep the last offset. A clock service
  restarting is not the lab going back to the real time.
- Hold the clock (`POST /clock/holds`) while work is in flight that must
  not straddle two days, and release it after.

`internal/labclock/follow.go` is that follower in Go, used by every vendor
here:

- `FollowEnv(ctx, service)` does nothing when `LAB_CLOCK_URL` is empty, so
  business time is the wall clock. Otherwise it polls that URL every second
  in the background.
- `Fetch` sends `GET` with a 2 s timeout. Anything but a `200` carrying a
  numeric `offset_ms` is an error.
- On an error the last offset stands. It logs `labclock: cannot read …;
  keeping offset …` once when the clock goes away and `… answering again`
  once when it returns; each change of offset logs `labclock: now … (…
  from real)`.
- `Now()` is the wall clock plus the offset, in UTC.
- `Wait(next)` blocks until the lab clock reaches the next scheduled
  instant, re-reading at least every 5 s. Moved forward past that instant,
  it fires; moved back, it re-plans to the earlier instant `next` gives. A
  move undone within 5 s can go unnoticed. Settlement's daily cutoff and
  Worldline's delivery slots run on it.

### `GET /clock`

The clock as it stands. `offset_ms` is what a follower reads; the rest is
for the people looking at it.

Answers `200`:

```json
{"offset_ms": -225000000, "offset_hours": -62.5, "now": "2026-09-25T09:00:00Z",
 "mode": "pinned", "pinned": true, "reason": "pinned to 2026-09-25T09:00:00Z",
 "changed_at": "2026-09-27T21:30:00Z", "business_day": true,
 "calendars": [{"code": "GB", "zone": "Europe/London", "date": "2026-09-25", "weekday": "Fri", "business_day": true},
               {"code": "SE", "zone": "Europe/Stockholm", "date": "2026-09-25", "weekday": "Fri", "business_day": true}],
 "holds": []}
```

- `offset_hours` is rounded to two places; `now` is business time in UTC.
- `changed_at` is the real time of the last move.
- `business_day` is true when every calendar's `business_day` is.
- `holds` lists live holds only; reading drops lapsed ones.

### `POST /clock`

Moves the clock, or changes its mode.

Body: exactly one of `at`, `advance` or `mode`, and an optional `reason`:

```json
{"at": "2026-09-27T10:00:00Z", "reason": "Sunday morning"}
```

```json
{"advance": "1d"}
```

```json
{"mode": "auto-business-day"}
```

- `at`: an RFC 3339 instant. Pins the offset so the lab is at that instant
  now.
- `advance`: a number and a unit, `d`, `h` or `m`, which may be negative
  or fractional (`"1d"`, `"-2d"`, `"6h"`, `"90m"`, `"1.5h"`). It is added
  to the offset in force, an `auto-business-day` one included, and the
  result is pinned.
- `mode`: `real` or `auto-business-day`. `pinned` is not accepted: pin
  with `at` or `advance`.
- `reason` defaults to what was done (`advanced 1d`, `back on the real
  clock`) and shows in `GET /clock` and the log.

Answers `200` with the new state, as `GET /clock`.

- `400`: `body: …` (not JSON, or over 8 KiB); `bad clock request: send one
  of {"at": "<RFC 3339>"}, {"advance": "1d"}, {"mode": "real"} or {"mode":
  "auto-business-day"}` (none, or more than one); `bad clock request: at:
  … is not an RFC 3339 timestamp`; `bad clock request: advance: "soon" is
  not like "1d", "6h" or "-90m"`; `bad clock request: mode: "pinned" is not
  real or auto-business-day`.
- `409`: `clock is held: infinite-local-runner holds the clock: settlement
  in flight: EUR … (until 21:45:00Z)`. Any live hold refuses every move,
  its own holder's included. With several holds, which one is named is
  arbitrary.

Side effects: every follower moves within its poll, about a second.
Moving past one of Worldline's delivery slots (compose: the morning file
somewhere in 08:00–10:00, the afternoon one at 15:30) delivers that slot's
file; moving past settlement's `CUTOFF_TIME` runs its daily batch.
Recorded as `clock.<mode>` in the `changes` log, or `clock.refused` when
held. The service itself refuses nothing else: the lab API's rule against
moving the clock while a wall-clock platform is registered lives in the
lab API (`POST /v1/clock`), not here.

### `POST /clock/holds`

Takes, or renews, a hold: the clock refuses every move until it is
released or lapses.

Body:

```json
{"holder": "infinite-local-runner", "reason": "settlement in flight: EUR …", "ttl_seconds": 900}
```

`holder` is required. `reason` defaults to `held`. `ttl_seconds` that is
absent, zero or negative means ten minutes; anything over 3600 is cut to
an hour, so a holder that crashes cannot freeze the lab for longer.

Answers `200`:

```json
{"holder": "infinite-local-runner", "reason": "settlement in flight: EUR …", "until": "2026-09-27T21:45:00Z"}
```

`until` is real time, not business time.

- `400`: `body: …`; `bad clock request: holder is required`.

Side effects: posting again for the same holder replaces its reason and
expiry. Only the first take is logged (`hold.take`); a hold that lapses
leaves no event.

### `DELETE /clock/holds/{holder}`

Releases a hold.

Path: `holder`, as it was taken.

Answers `204` with no body.

- `404`: `no hold by infinite-local-runner`, including for a hold that has
  already lapsed.

Side effects: recorded as `hold.release`.

## Lab control

### `GET /health`

Liveness. The lab API's monitor probes it.

Answers `200` with `{"status": "ok", "service": "clock"}`.

### `GET /sim/activity`

The `changes` log.

Query: `limit` (default 100; the log keeps its last 256 events).

Answers `200`:

```json
{"logs": [{"name": "changes", "title": "Clock changes", "note": "Every move of the lab clock, …",
           "total": 3, "kept": 3, "last": {"…": "…"},
           "events": [{"seq": 3, "at": "2026-09-27T21:30:00Z", "op": "clock.pinned",
                       "summary": "pinned → 2026-09-25 09:00 UTC (Fri 2026-09-25 on GB, Fri 2026-09-25 on SE), a business day — pinned to 2026-09-25T09:00:00Z",
                       "status": "ok", "detail": {"offset_hours": "-62.5"}}]}]}
```

Events carry no `peer`. `at` is wall-clock time.

### `GET /sim/info`

Who this is and what it can do, as every service answers it
([the control contract](../README.md#the-control-contract)):
`"service": "clock"`, `"kind": "lab"`, `"reset_scopes": ["flow", "all"]`.

### `GET /sim/logs`

The process's own log lines, as every service answers it
([the control contract](../README.md#the-control-contract)): every move,
hold and refusal is a line here too.

### `POST /sim/reset`

Puts the clock back where it started. Both scopes do the same: the clock
has nothing a platform set up once.

- **Clears**: every hold, and the `changes` log; then applies `CLOCK_START`
  as on first start, or the real clock when it is unset. The move back is
  the first event of the new log.
- **Keeps**: the calendars.
- **Refuses** with `409` while a hold is live — a settlement run is in
  flight, and resetting the clock under it would move the run's business
  day. `{"force": true}` releases the hold and resets anyway.

```json
{"service": "clock", "scope": "flow", "cleared": {"holds": 0, "changes": 7},
 "kept": ["calendars"], "at": "2026-11-07T12:00:00Z"}
```

## Called by

- **every vendor, as a follower**: bank, settlement, worldline,
  banking-circle, b4b, aci and verify poll `GET /clock` every second
  (`LAB_CLOCK_URL`). payment-api, notifier, receiver, verification and the
  lab API do not follow it.
- **lab API**: `GET /v1/clock` reads `GET /clock`; `POST /v1/clock`
  forwards its body verbatim to `POST /clock`, after refusing with its own
  `409` while a platform registered on the wall clock is live;
  `GET /v1/services/clock/activity` proxies `GET /sim/activity`;
  `GET /v1/lab` and `POST /v1/services/clock/probe` probe
  `GET /health`.
- **the platform**: a plugin registered with `"clock": "follows"` polls
  `GET /clock`; buddy's runner writes the offset into the file its clock
  shim watches. While a settlement run is in flight it takes
  `POST /clock/holds` (holder `infinite-local-runner`, 900 s in
  [plugins.md](../../plugins.md#the-clock)) and releases it with
  `DELETE /clock/holds/infinite-local-runner`.
- **harness**: no scenario reads or moves the clock.
- **by hand**: [agent.runbooks.md](../../../agent.runbooks.md#the-clock)
  moves it with `curl`.

Routes checked against the source at commit db5e695.
