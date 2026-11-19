# KYC6 — API

Simulates Acuris KYC6: sanctions, PEP and adverse-media screening, profile lookups and monitoring worklists. One process, its own sim (`cmd/kyc6`, sharing
the lab side with the other verification vendors through
`internal/vendorsim`). It sits on the **vendor** side of the line in
[catalogue.md](../../catalogue.md): a real vendor the platform calls
directly. What is real: the paths, the auth handshake, and the response
fields the platform's own client reads. What is simplified: the judgement
(every check passes until flipped with `POST /sim/outcome`), credentials
(any non-empty value is accepted, and an environment's own login names that
environment), token lifetime (never enforced), the monitoring
re-screen (a test puts a match on a monitored record), and everything a
client does not read, which is minimal, fixed or echoed. Nothing is
persisted.

## At a glance

| | |
| --- | --- |
| Listens | `:8092`, plain HTTP: the vendor routes. Compose service `kyc6` (`http://kyc6:8092` on the compose network); host `http://127.0.0.1:8092`.<br>`:9092`, plain HTTP: the **control port** (`SIM_CONTROL_LISTEN`), every `/sim/*` route below and its own `GET /health`. Not published by compose; the lab API reaches it at `http://kyc6:9092` |
| Auth | an `x-api-key` header on every route. Any non-empty credential is accepted: an environment's own login makes the call that environment's, and any other is the default environment's ([Environments](#environments)). A customer's username with another password is `401`. `/health` and `/sim/*`: none. The control port has none: it takes calls from anything that can reach it, or only from `SIM_CONTROL_ALLOW` (addresses or CIDRs) when that is set |
| A platform points here with | `KYC6_API_BASE_URL` and `KYC6_MONITOR_API_BASE_URL` (both `http://127.0.0.1:8092/kyc6`), `KYC6_API_KEY`. The lab API hands these out at `GET :8097/v1/connect/env/kyc6` |
| Activity logs | `GET /sim/activity`: the request journal, every vendor request and its answer, and the traps log |
| Configuration | `LISTEN` (`:8092`).<br>`SIM_CONTROL_LISTEN` (`LISTEN`'s port plus 1000, `:9092`) and `SIM_CONTROL_ALLOW` (empty: any caller): the control port.<br>`LAB_CLOCK_URL` (compose: `http://clock:8096/clock`): every monitoring timestamp (`createdDateTimeISO`, `lastUpdatedDateTimeISO`, `lastReviewDateTimeISO`, a review's `at`) follows the [lab clock](../../plugins.md#the-clock).<br>Nothing else: no state file. The outcomes and worklists, per environment, every issued token and the customers live in memory and reset on restart; the lab API pushes the customers again |
| Design record | [integration/verification-vendor-edge.md](../../integration/verification-vendor-edge.md), the package comments in `cmd/kyc6` and `internal/vendorsim`, [catalogue.md](../../catalogue.md) |

## Environments

Each environment the lab creates
([lab-api/environments.md](../../lab-api/environments.md)) is a customer
here, pushed by the lab API with
[`PUT /sim/customers/{env}`](#put-simcustomersenv) and known by its
`verification` login: the customer's `password` is the `x-api-key`.

- A call made with a customer's login is that environment's, and so is
  every token issued to it. Any other login is the default environment's.
  A customer's username with another password is refused, `401`, as the
  vendor would refuse it.
- Each environment has its own outcomes. A flip in one never moves
  another's answers.
- The `/sim/*` hooks act in the environment the call names: `?env=`, else
  the `Lab-Environment` header, else the default. `*` reads every
  environment; a flip names one. An environment this service was never
  given is `404`.
- A reset names one environment, or every one with `*`. Deleting a
  customer takes its outcomes and tokens with it.

## Vendor surface

What the platform's KYC6 client calls. Errors this service writes
have one shape, `{"error": "<message>", "status": <code>}`. Request bodies
are decoded loosely: only the fields named per route are read. A screen's
malformed JSON is read as an empty object rather than refused; on the
monitoring routes, a body that is not one JSON value of the route's
shape (empty, cut short, an array, an address that is not an object, or
anything after the value) is a `400` with the decoder's error, as at
KYC6, and changes nothing.

| Check | Default | Moves |
| --- | --- | --- |
| `screening` | `clear` | every screen |

Sanctions, PEP and adverse-media screening, and ongoing monitoring. Every
route needs an `x-api-key` header (any non-empty value; `401`
`{"error":"x-api-key header required","status":401}` without it).

### `POST /kyc6/individuals`

Screens a person.

Body: only `name` is read, and only to fill a hit.
`{"name": "Ada Lovelace"}`.

Answers `200`; clear means zero matches:

```json
{"results": {"matchCount": 0, "matches": []}}
```

With the `screening` outcome at `hit`:

```json
{"results": {"matchCount": 1, "matches": [{"resourceId": "sim-match-1", "name": "Ada Lovelace", "score": 0.92, "datasets": ["SAN-CURRENT"], "matchTypes": ["name"]}]}}
```

`SAN-CURRENT` is KYC6's dataset key for a current sanctions listing.

### `POST /kyc6/businesses`

Screens a business. Same handler, body and answers as
`POST /kyc6/individuals`.

### `GET /kyc6/individuals/{resourceId}`

A match's profile. For the `resourceId` of a monitoring match on one of
the caller's individuals (put there with
[`POST /sim/kyc6/worklists/{worklistId}/individuals/{recordId}/matches`](#post-simkyc6worklistsworklistidindividualsrecordidmatches)),
the profile carries what the match was put there with:

```json
{"resourceId": "sim-om-1", "qrCode": "sim-qr-1", "name": "Ada Lovelace",
 "datasets": ["SAN-CURRENT"], "countries": [], "version": 1, "isDeleted": false}
```

Any other id answers `200` with
`{"resourceId": "<the path id>", "datasets": ["SAN-CURRENT"]}`.

### `GET /kyc6/businesses/{resourceId}`

Same as `GET /kyc6/individuals/{resourceId}`, for the matches on the
caller's businesses.

## Ongoing monitoring

A platform creates a worklist, enrols businesses and individuals on it as
monitor records, and reads back the matches KYC6's re-screening finds
against them; a reviewer's decision goes back as a review, which closes
the match. Each environment has its own worklists. They live in memory: a
restart loses them, and a flow reset keeps the worklists but clears
their records ([`POST /sim/reset`](#post-simreset)). Enrolling a record on
a worklist the environment does not have creates it, with no settings, so
the platform's enrolments still answer after a restart or an `all` reset.

Timestamps are RFC 3339 UTC on the lab clock.

The lab does not re-screen. A record has no match until a test puts one
on it with
[`POST /sim/kyc6/worklists/{worklistId}/businesses/{recordId}/matches`](#post-simkyc6worklistsworklistidbusinessesrecordidmatches)
(or `…/individuals/…`).

What is KYC6's and what is the lab's reading of it:

- **From the platform's client**: the paths, the request bodies, and every
  field below that the client reads (`openCount`, a match's
  `currentStatus`, `resourceId`, `qrCode`, `matchVersion`, `datasets`; a
  review batch's `hasErrors` and per-entry `results` with `referenceId`,
  `statusCode` and `error.code`; `MAPI-312` for an auto-review that already
  exists).
- **Not confirmed against KYC6's documentation**: the record `status`
  values (`open`, `closed`) and the `status` query's values; what `match`
  holds on a match (here, the matched name); whether a manual review may
  replace an earlier one (here it may); whether an auto-review is held for a
  match that has not appeared yet (here it is); the status code of a
  refused entry (here `400`, `404` or `409`) and the error of any refusal
  but `MAPI-312` (here a `message` with no `code`); the shape of a
  `404` (here the lab's own `{"error", "status"}`); and
  [`GET /kyc6/worklists/{worklistId}/matches`](#get-kyc6worklistsworklistidmatches)
  as a whole.
- **Not simulated**: `discarded` matches (`discardedCount` is always 0),
  re-screening on the worklist's `frequency`, `matchVersion` checks on a
  review (it is recorded, not compared), and MAPI-012, the conflict KYC6
  asks a client to retry: arm an `http.request` trap
  ([`POST /sim/traps`](#post-simtraps)) answering
  `{"status": 409, "body": {"code": "MAPI-012", "message": "Conflict, please retry"}}`
  on the path to see it.

### `PUT /kyc6/worklists/{worklistId}`

Creates a worklist in the caller's environment, or replaces the settings of
one that exists and keeps its records.

Body: the worklist's settings, stored as sent and not read:

```json
{"name": "Partner 7", "datasets": ["PEP", "SAN", "INS", "RRE", "DD", "POI", "REL", "SOE"],
 "threshold": 85, "frequency": "daily", "dobMatching": "sameYear", "dobRequired": false,
 "countryRequired": false, "matching": "fuzzy", "pepTiers": ["PEP Tier 1"], "transliterateName": true}
```

Answers `200` with the worklist, as
[`GET /kyc6/worklists/{worklistId}`](#get-kyc6worklistsworklistid) shows it,
and `worklistId`, the path id.

### `GET /kyc6/worklists/{worklistId}`

One worklist: the settings it was given, its timestamps and what is on it.

```json
{"id": "7", "name": "Partner 7", "threshold": 85, "…": "every setting as sent",
 "createdDateTimeISO": "2026-11-18T09:00:00Z", "lastUpdatedDateTimeISO": "2026-11-18T09:00:00Z",
 "businessMonitorRecordsCount": 1, "individualMonitorRecordsCount": 2,
 "businessOpenMatchesCount": 1, "individualOpenMatchesCount": 0}
```

`404` `{"error": "no worklist 7", "status": 404}` for one the caller's
environment does not have.

### `DELETE /kyc6/worklists/{worklistId}`

Deletes a worklist and everything on it. Answers `204`; `404` for an
unknown one.

### `GET /kyc6/worklists/{worklistId}/matches`

Every open match on the worklist, businesses' and individuals' alike,
oldest first; matches found at the same moment by kind (businesses
first), record id, and the order they were put on the record. **Not confirmed as a KYC6 route**: it answers in the shape the
platform's poll reads, and only with open matches, so a match stops being
reported once it is reviewed.

```json
{"matches": [{"matchId": "sim-om-match-1", "recordId": "company-42", "resourceId": "sim-om-1",
              "score": 92, "matchName": "Southwind Coffee Ltd", "datasets": ["SAN-CURRENT"], "status": "open"}],
 "totalMatches": 1}
```

`404` for an unknown worklist.

### `PUT /kyc6/worklists/{worklistId}/businesses/{recordId}`

Enrols a business as monitor record `recordId`, or updates the one
enrolled and keeps its matches.

Body: `{"name": "Southwind Coffee Ltd", "addresses": [{"geography": "GB"}]}`.
`name` is required (`400` without it).

Answers `200` with the record, as
[`GET /kyc6/worklists/{worklistId}/businesses`](#get-kyc6worklistsworklistidbusinesses)
lists it, and `monitorRecordId` and `worklistId`. A worklist the
environment does not have is created, with no settings: the platform
creates its worklist once, at customer setup, and the lab's does not
survive a restart or an `all` reset.

### `GET /kyc6/worklists/{worklistId}/businesses`

The worklist's businesses, by record id. Query: `status=open` lists the
records with a match nobody has reviewed yet; `status=closed` the rest;
without it, every record. Any other `status` is `400`.

```json
{"monitorRecords": [
   {"id": "company-42", "name": "Southwind Coffee Ltd", "countries": ["GB"],
    "addresses": [{"geography": "GB"}], "openCount": 1, "truePositiveCount": 0,
    "falsePositiveCount": 0, "discardedCount": 0, "status": "open", "openDatasets": ["SAN-CURRENT"],
    "createdDateTimeISO": "2026-11-18T09:00:00Z", "lastUpdatedDateTimeISO": "2026-11-18T09:05:00Z"}],
 "monitorRecordsCount": 1, "openMonitorRecordsCount": 1, "openMatchesCount": 1,
 "truePositiveMatchesCount": 0, "falsePositiveMatchesCount": 0, "discardedMatchesCount": 0}
```

`countries` are the addresses' `geography`, once each; `openDatasets` the
datasets of the record's open matches. An individual's record also
carries `dob` when it was enrolled with one. The counts are the whole
worklist's, whatever `status` filtered. `404` for an unknown worklist.

### `DELETE /kyc6/worklists/{worklistId}/businesses/{recordId}`

Removes the business from the worklist, with its matches and reviews.
Answers `204`; `404` for an unknown worklist or record.

### `GET /kyc6/worklists/{worklistId}/businesses/{recordId}/matches`

Every match on the record, open or reviewed, in the order they were found:

```json
{"matches": [
   {"currentStatus": "false-positive", "resourceId": "sim-om-1", "qrCode": "sim-qr-1",
    "score": 92, "match": "Southwind Coffee Ltd", "name": "Southwind Coffee Ltd",
    "datasets": ["SAN-CURRENT"], "countries": [], "version": 1, "matchVersion": 1,
    "lastReviewDateTimeISO": "2026-11-18T09:10:00Z", "lastReviewDescription": "different company"}]}
```

`currentStatus` is `open` until a review closes it as `true-positive` or
`false-positive`. `version` and `matchVersion` are the same number: `1`
when the match was found, one more each time the profile changes. The
`lastReview…` fields appear once it is reviewed; the description is the
review's `clientComment`. `404` for an unknown worklist or record.

### `PUT /kyc6/worklists/{worklistId}/businesses/{recordId}/matches/reviews`

A reviewer's decisions on the record's matches.

```json
{"reviews": [{"qrCode": "sim-qr-1", "referenceId": "11", "status": "false-positive",
              "clientComment": "different company", "clientUserId": "ada_lovelace_7", "matchVersion": 1}]}
```

Each entry is ruled on alone. One whose `qrCode` names a match on the
record closes it with `status` (`true-positive` or `false-positive`); a
match already reviewed takes the newer review. The batch answers `200`,
with one result per entry, in order, echoing its `referenceId`. A
`qrCode` or `referenceId` sent as a JSON number is read as its digits,
every one of them however large (`9007199254740993` is
`"9007199254740993"`):

```json
{"hasErrors": true, "results": [
   {"referenceId": "11", "statusCode": 200},
   {"referenceId": "12", "statusCode": 404, "error": {"message": "no match with qrCode nope on this record"}}]}
```

An entry is refused `400` without a `qrCode` or with a `status` that is
neither, and `404` when the record has no match with its `qrCode`.
`hasErrors` is whether any entry was refused; the journal marks such a
batch `warn`, `1 of 2 refused`. `400` for the whole call when `reviews` is
missing or empty; `404` for an unknown worklist or record. Every entry,
refused or not, is kept for
[`GET /sim/kyc6/worklists/{worklistId}/businesses/{recordId}`](#get-simkyc6worklistsworklistidbusinessesrecordid),
the newest 256 per record, as many as the journal keeps calls.

### `PUT /kyc6/worklists/{worklistId}/businesses/{recordId}/matches/auto-reviews`

Decisions taken before monitoring, held by the record: each closes the
match with its `qrCode` now, if the record has one, or as soon as one
appears.

Body: `{"autoReviews": [<entry>, …]}`, each entry as for `…/matches/reviews`.

Answers as `…/matches/reviews` does. An entry for a `qrCode` the record
already holds an auto-review for, or whose match is already reviewed, is
refused `409` with KYC6's code:

```json
{"hasErrors": true, "results": [
   {"referenceId": "21", "statusCode": 409, "error": {"code": "MAPI-312", "message": "Auto review already exists"}}]}
```

### `PUT /kyc6/worklists/{worklistId}/individuals/{recordId}`

Enrols an individual. Same as for a business; the body may also carry
`dob`, and an address its `city`:
`{"name": "Ada Lovelace", "addresses": [{"geography": "GB", "city": "London"}], "dob": "1815-12-10"}`.

### `GET /kyc6/worklists/{worklistId}/individuals`

Same as `GET /kyc6/worklists/{worklistId}/businesses`, for the individuals.

### `DELETE /kyc6/worklists/{worklistId}/individuals/{recordId}`

Same as for a business.

### `GET /kyc6/worklists/{worklistId}/individuals/{recordId}/matches`

Same as for a business.

### `PUT /kyc6/worklists/{worklistId}/individuals/{recordId}/matches/reviews`

Same as for a business.

### `PUT /kyc6/worklists/{worklistId}/individuals/{recordId}/matches/auto-reviews`

Same as for a business.

### `GET /kyc6/lifecycle/ongoing-monitoring/{alertId}`

An ongoing-monitoring alert. Answers `200` with
`{"alertId": "<the path id>", "status": "closed", "matches": []}`, always.

## Lab control

Hooks a test, agent or operator uses to observe or steer the sim. They do
not exist at the real vendor. They are on the control port, `:9092`
([the control contract](../README.md#the-control-contract)); the same
`/sim/*` paths still answer on `:8092` until phase 4 of
[the plan](../../lab-api/plan.md) retires them.

### `GET /health`

Liveness, on both ports. `:8092` answers `{"status": "ok", "service":
"kyc6"}`; the control port answers `{"status": "ok", "service":
"kyc6", "surface": "control"}`, and that is the one the lab API
probes.

### `GET /sim/outcome`

What each check currently answers, in the environment the call names
(`?env=` or `Lab-Environment`; the default without either).

Answers `200`:

```json
{"env": "default", "outcomes": {"screening": "clear"}}
```

With `?env=*`, every environment's, the default's included. `404`
`no environment <env> at kyc6` for one this service was never given.

### `POST /sim/outcome`

Flips one check in the environment the call names (`?env=` or
`Lab-Environment`; the default without either). It applies to every later
call made with that environment's login until it is set back, the
environment is reset ([`POST /sim/reset`](#post-simreset)), or the process
restarts. Another environment's answers do not move.

Body, decoded strictly (an unknown field is a 400): `{"check": "screening", "outcome": "hit"}`.

| `check` | `outcome` |
| --- | --- |
| `screening` | `clear`, `hit` |

Answers `200` with the environment's every outcome, as `GET /sim/outcome`
does. `400` for an unknown check or outcome, which changes nothing:
`{"error":"check \"screening\" cannot be \"maybe\" (want: clear, hit)","status":400}`;
`400` `an outcome is set in one environment, not in all of them` for
`env=*`; `404` for an environment this service was never given.

### `POST /sim/kyc6/worklists/{worklistId}/businesses/{recordId}/matches`

KYC6's re-screening found a match on this business: the record is open
until the match is reviewed. In the environment the call names (`?env=` or
`Lab-Environment`; the default without either); the worklist and record
must be ones the platform created there.

Body, decoded strictly (an unknown field is a 400); every field is
optional, and an empty body is the defaults:

```json
{"name": "Southwind Coffee Ltd", "datasets": ["SAN-CURRENT", "PEP-CURRENT"], "score": 96,
 "countries": ["GB"], "resourceId": "sim-om-1", "qrCode": "sim-qr-1"}
```

| Field | Default |
| --- | --- |
| `name` | the record's name |
| `datasets` | `["SAN-CURRENT"]`, a current sanctions listing |
| `score` | `92`, from 0 to 100 |
| `countries` | `[]` |
| `resourceId` | `sim-om-<n>`, numbered per environment |
| `qrCode` | `sim-qr-<n>` |

A number whose `sim-om-<n>` or `sim-qr-<n>` a match in the environment
already has, because a test gave it, is skipped.

A `resourceId` the record already has a match for is that profile
changing: the match takes the fields given, its `matchVersion` goes up by
one, and it is open again. A field not given keeps its value; `[]` for
`datasets` or `countries` empties it. A `qrCode` the record holds an auto-review for
arrives closed by it.

Answers `201` with the new match, `200` for one moved on a version, as
the record's matches show it:

```json
{"env": "default", "worklistId": "7", "kind": "businesses", "recordId": "company-42",
 "match": {"currentStatus": "open", "resourceId": "sim-om-1", "qrCode": "sim-qr-1", "score": 96,
           "match": "Southwind Coffee Ltd", "name": "Southwind Coffee Ltd",
           "datasets": ["SAN-CURRENT", "PEP-CURRENT"], "countries": ["GB"], "version": 1, "matchVersion": 1}}
```

`400` for an unknown field, a `score` outside 0 to 100, an empty dataset,
or `env=*`; `404` for an environment this service was never given, or a
worklist or record that environment does not have. `409`, changing
nothing, for a `qrCode` another match in the environment has (codes are
unique across the environment; on one record, a review finds its match by
`qrCode`, so a second match with the same code could never be reviewed):
`{"error": "qrCode Q1 is already match r-1's in environment default", "status": 409}`.
A re-arm of a match with its own `qrCode` is not a conflict, and one
`resourceId` may be matched on several records, as one KYC6 profile can
match two monitored businesses or people.

### `POST /sim/kyc6/worklists/{worklistId}/individuals/{recordId}/matches`

Same, for an individual.

### `GET /sim/kyc6/worklists/{worklistId}/businesses/{recordId}`

One business record and what the platform sent for it, in the environment
the call names (`*` is a 400): the record as
[`GET /kyc6/worklists/{worklistId}/businesses`](#get-kyc6worklistsworklistidbusinesses)
lists it, its `matches` as the vendor shows them, the `autoReviews` it
holds, by qrCode within one batch, and `reviews`, every review and
auto-review entry it was sent, in order, the newest 256, with the answer
each was given:

```json
{"env": "default", "worklistId": "7", "kind": "businesses",
 "id": "company-42", "name": "Southwind Coffee Ltd", "status": "closed", "openCount": 0, "…": "as listed",
 "matches": [{"currentStatus": "false-positive", "resourceId": "sim-om-1", "…": "as the vendor shows it"}],
 "autoReviews": [],
 "reviews": [{"endpoint": "reviews", "qrCode": "sim-qr-1", "referenceId": "11", "status": "false-positive",
              "clientComment": "different company", "clientUserId": "ada_lovelace_7", "matchVersion": 1,
              "at": "2026-11-18T09:10:00Z", "statusCode": 200}]}
```

`endpoint` is `reviews` or `auto-reviews`; a refused entry also carries its
`error`. `404` as for the `POST` above.

### `GET /sim/kyc6/worklists/{worklistId}/individuals/{recordId}`

Same, for an individual.

### `GET /sim/kyc6/worklists`

Every worklist in the environment the call names, or in every one for
`*`, by environment then id: each as
[`GET /kyc6/worklists/{worklistId}`](#get-kyc6worklistsworklistid) shows it,
with its `env`, and its `businesses` and `individuals` as
`GET /sim/kyc6/worklists/{worklistId}/businesses/{recordId}` shows each
record.

```json
{"env": "default", "worklists": [
   {"env": "default", "id": "7", "name": "Partner 7", "…": "as the vendor shows it",
    "businesses": [{"id": "company-42", "…": "as above"}], "individuals": []}]}
```

`404` for an environment this service was never given.

### `GET /sim/traps/hooks`

The catalogue: one hook, `http.request`, on every vendor request
([traps.md §9.7](../../lab-api/traps.md#97-every-sim-httprequest)), with
the `respond` effect ([§4.3](../../lab-api/traps.md#43-respond-answering-a-vendor-shaped-request)).

```json
{"service": "kyc6",
 "hooks": [{"hook": "http.request",
            "about": "Any request on the vendor surface. …",
            "subject": "request",
            "match": [{"field": "method", "type": "string", "about": "GET, POST, …"},
                      {"field": "path", "type": "path", "about": "the request path, without the query; * stands for one segment"}],
            "effect": "respond"}]}
```

A request is the environment its `x-api-key` names. `/health` and the
`/sim/*` paths are never caught.

### `GET /sim/traps`

The traps, newest first. Query: `state` (`armed`, `spent`, `expired` or
`disarmed`) filters; `400` for anything else.

Answers `200` with `{"service": "kyc6", "traps": [<trap>, …]}`.

### `POST /sim/traps`

Arms a trap. Body: `hook` (`http.request`), `match` (`{}` takes every
request), `effect`, and optionally `label`, `times` (`1` when left out,
`null` for every request until disarmed) and `expires_at`. An unknown field
is refused. The effect is one of:

| Effect | |
| --- | --- |
| `{"respond": {"status": 503, "body": …, "headers": {…}}}` | answered with this instead; the request has no effect. A string `body` is sent as it is (`text/plain` unless a header says otherwise), so a reply can be unreadable; anything else is sent as JSON |
| `{"delay": "8s"}` | handled normally, after the wait: for client timeouts |
| `{"close": true}` | the connection is dropped with no answer |

The next KYC6 business screen is an outage:

```json
{"hook": "http.request", "match": {"method": "POST", "path": "/kyc6/businesses"},
 "effect": {"respond": {"status": 503, "body": {"error": "Service Unavailable"}}}}
```

Answers `201` with the trap, `effect` as armed. `400` with the reason: a
hook or match field kyc6 does not have, an effect that is not
exactly one of the three, a status outside 100 to 599, or a delay that is
not a duration.

### `GET /sim/traps/{id}`

One trap. Each request it caught is in `caught`, with what was done to it:

```json
{"id": "trp_0001", "…": "as armed", "state": "spent",
 "caught": [{"subject": "POST /kyc6/businesses", "at": "2026-11-12T09:40:00Z",
             "applied": [{"to": "respond 503", "at": "2026-11-12T09:40:00Z"}]}],
 "pending_steps": []}
```

`404` for an unknown id.

### `DELETE /sim/traps/{id}`

Disarms one trap. Answers `200` with `{"trap": <trap>, "cancelled": []}`,
`404` for an unknown id.

### `DELETE /sim/traps`

Disarms every trap. Answers `200` with `{"service": "kyc6",
"disarmed": 1, "cancelled": []}`.

### `GET /sim/activity`

Two logs: `requests`, the journal, and `traps`, every trap armed, every
request one caught and what was done to it. A request a trap answers is
journalled with the trap's answer; one a trap drops (`close`) is `bad`,
`connection dropped`.

The request journal, newest first: every request the platform made to a
vendor on `:8092`, and the status it was answered with. Query: `limit`
(positive integer, default 100); `env` (the default when left out, `*` for
every environment's); `404` for an environment this service was never
given. A request is the environment its credentials name, a
login included; one the vendor refused is the default's.

Answers `200`:

```json
{"logs": [{"name": "requests", "title": "Vendor requests",
           "note": "Every request the platform made to KYC6, and the status it was answered with.",
           "total": 1, "kept": 1,
           "last": {"seq": 1, "at": "2026-11-12T09:40:00Z", "op": "kyc6", "peer": "10.89.0.7",
                    "summary": "POST /kyc6/businesses", "status": "ok", "detail": {"status": "OK"}},
           "events": [{"seq": 1, "…": "as last"}]}]}
```

`op` is `kyc6`. A `4xx` answer is `warn` and carries the vendor's error in the
summary. The journal keeps the first 64 KiB of a request body; the
vendor reads it all.

### `GET /sim/info`

Who this is and what it can do, as every service answers it
([the control contract](../README.md#the-control-contract)):
`"service": "kyc6"`, `"kind": "vendor"`,
`"reset_scopes": ["flow", "all"]`, `"hooks": ["http.request"]`,
`"traps": {"armed": 0, "pending_steps": 0}`, `"environments": true` and
`"customers"`, how many there are.

### `GET /sim/logs`

The process's own log lines, as every service answers it
([the control contract](../README.md#the-control-contract)): one line per
request, every flip of an outcome, and every match put on a record.

### `POST /sim/reset`

Puts every outcome back to its default, in the environment the reset
names (`"env"` in the body, `?env=` or `Lab-Environment`; the default
without any), or in every one for `*`.

- **`flow` clears**: the outcomes, every trap, the request journal, and
  every monitor record on the environment's worklists, with its matches,
  reviews and held auto-reviews.
- **`flow` keeps**: the worklists and their settings, empty. A platform
  creates its worklist once, when a customer is set up, and would not
  create it again; it enrols records again as it monitors. KYC6 issues no
  tokens.
- **`all`** is first start: `flow`, and the worklists too.

`cleared` counts `outcomes` that were flipped, journal events it held
(`activity`, at most 256 per environment), the `traps` disarmed (with
`trap_steps`, always 0 here), the `records`, `matches` and `reviews`
(review entries received) it removed, and with `all`, `worklists` and
`tokens` (always 0 here). Another environment's state is left alone.

```json
{"service": "kyc6", "scope": "flow", "env": "default",
 "cleared": {"activity": 5, "matches": 1, "outcomes": 1, "records": 3, "reviews": 1, "trap_steps": 0, "traps": 1},
 "kept": ["tokens", "worklists"], "at": "2026-11-07T12:00:00Z"}
```

### `GET /sim/customers`

The environments this service serves, the default first
([sim-control.md §8](../../lab-api/sim-control.md#8-environments)).

```json
{"customers": [
  {"env": "default", "created_at": "0001-01-01T00:00:00Z", "last_seen": "2026-11-10T09:00:00Z"},
  {"env": "sbx", "name": "sbx", "created_at": "2026-11-10T08:00:00Z", "last_seen": null}]}
```

`last_seen` is the last vendor call made with the environment's login, a
token request included; `null` until the first. The default has no
`created_at` of its own: it is the zero time.

### `PUT /sim/customers/{env}`

Adds or replaces one customer. The lab API pushes every customer again
when the service restarts, because they live in memory.

Body: a customer, of which this service reads `verification`:

```json
{"env": "sbx", "name": "sbx", "verification": {"username": "sbx-verification", "password": "…"}}
```

`env` may be left out; given, it must be the path's.

Answers `200` `{"env": "sbx", "created": true}`, `false` when it replaced
one; replacing keeps the environment's outcomes and tokens. `400` for a
body that is not a customer, an `env` that is not the path's, or an id
that cannot name an environment (`default`, `*`, or anything but
lower-case letters, digits and dashes, starting with a letter, at most
40).

### `DELETE /sim/customers/{env}`

Removes one customer and what it had here: its outcomes, its worklists,
every token issued to it, its traps and its journal. Its login is then a stranger's, the default's.

Answers `200` `{"env": "sbx", "deleted": true}`; `404`
`no environment sbx here`.

## Called by

- **The platform** under test: its own KYC6 client, through the
  variables in the table above. Nothing in the lab calls the vendor routes.
- **lab API** (`cmd/labapi`): only the control port, `http://kyc6:9092`.
  `GET /v1/lab` and `POST /v1/services/kyc6/probe` probe its
  `GET /health`. `POST /v1/lab/reset` and `POST /v1/services/kyc6/reset`
  call `POST /sim/reset`; `GET /v1/logs` and `GET /v1/services/kyc6/logs`
  read `GET /sim/logs`; every other `/sim/*` route is reached through
  `/v1/services/kyc6/sim/*`.
