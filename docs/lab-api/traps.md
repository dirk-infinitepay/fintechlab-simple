# Traps

## 1. What a trap is

**[DECIDED 2026-10-20: the idea. The shape below is PROPOSED]**

A trap is an instruction armed in advance in one simulation. It decides
something about a future event at one of that sim's **hooks**: which
outcome the event takes, when, and whether the notification about it
arrives. The owner's example:

> At Banking Circle: if the payout's receiving vIBAN is X, end it
> `OutgoingPaymentRejected`, or end it `OutgoingPaymentProcessed` and then
> `Reversed` ten seconds later.

A trap is one of three ways the lab changes a sim, and the other two are
not traps:

| Kind | Acts | Example | Route |
| --- | --- | --- | --- |
| **trigger** | now, once | return payout `bcp_…`; run Worldline's morning cycle; simulate an ACI card payment | a plain `/sim/…` route |
| **setter** | from now on, on everything it covers | beneficiary X fails sanctions; screening answers `hit`; pause one subscription | a plain `/sim/…` route |
| **trap** | on the next event(s) that match | a payout to vIBAN X is processed, then reversed ten seconds later | `/sim/traps` |

**What a trap may change, and what it may not.** A trap only ever puts the
vendor in a state the real vendor can be in. It may pick among the
outcomes the vendor really has, decide when they happen, hold back, repeat
or delay a notification, make a report run late, or answer a request with
an error. It may not make the vendor tell two stories that never
converge. A trapped `Reversed` is reversed everywhere: the ledger, the
status endpoint, the reports and the notifications. "The webhook was lost"
is a delivery trap (§4.2). The bank's books still say what happened, and
finding that out is exactly the reconciliation sweep's job.

A trap is invisible on the vendor surface
([architecture.md §3](architecture.md#3-the-drop-in-rule)). It is visible
in the sim's own activity and logs (§7).

## 2. The trap object

**[PROPOSED]**

```json
{
  "id": "trp_0003",
  "label": "merchant 7: processed, then reversed",
  "hook": "payment.outgoing",
  "match": {"creditor.iban": "GB00SIM0000000000777"},
  "times": 1,
  "expires_at": null,
  "effect": {
    "path": [
      {"to": "OutgoingPaymentProcessed"},
      {"to": "Reversed", "after": "10s", "reason": "scheme"}
    ]
  },

  "state": "armed",
  "created_at": "2026-10-20T09:00:00Z",
  "caught": [],
  "pending_steps": []
}
```

| Field | | |
| --- | --- | --- |
| `id` | assigned | `trp_` and a sequence, unique within the sim |
| `label` | optional | free text, shown in activity and logs |
| `hook` | required | one of the sim's hooks (`GET /sim/traps/hooks`) |
| `match` | required | which events it takes (§3); `{}` takes any |
| `times` | optional | how many events it takes; default `1`; `null` = until disarmed ([OPEN 5](open.md)) |
| `expires_at` | optional | wall-clock time after which it takes nothing |
| `effect` | required | what happens to a caught event (§4); its shape depends on the hook |
| `state` | read-only | `armed`, `spent`, `expired`, `disarmed` |
| `caught` | read-only | each event taken: subject id, when, the steps applied |
| `pending_steps` | read-only | steps scheduled and not yet applied: subject, step, due time |

## 3. Matching

**[PROPOSED]**

- Each hook names its fields (§8), for example `creditor.iban`, `amount` or
  `currency`. An unknown field is a `400` that lists the hook's fields.
- A value can be:
  - a string, for an exact match: `"GB00SIM0000000000777"`
  - `{"in": ["…", "…"]}` for any of several
  - `{"prefix": "GB00SIM00000000007"}`
  - `{"gte": "100.00", "lte": "500.00"}` for amounts and numbers, compared
    as decimals, so `"10"` equals `"10.00"`
- IBANs are compared normalised: upper case, spaces removed.
- Every field given must match. `{}` matches every event at the hook.

**Precedence.** An event is offered to the armed traps on its hook in the
order they were armed. The first that matches takes it, and only that one.
Traps on different hooks are independent: one payout can be caught at
`payment.outgoing`, and its notification at `notification.deliver`.

So "the next two payouts are rejected" is two traps with `match: {}`,
armed one after the other. That is exactly Banking Circle's outcome queue
today, which becomes a shortcut for it (§9.1).

## 4. Effects

**[PROPOSED]**

### 4.1 `path`: a state machine's next states

For hooks whose subject has a lifecycle (a Banking Circle payout, a B4B
payment). The hook's graph (§8) says which states can follow which.

```json
"effect": {"path": [
  {"to": "OutgoingPaymentProcessed"},
  {"to": "Reversed", "after": "10s", "reason": "scheme"}
]}
```

| Step field | |
| --- | --- |
| `to` | required: the next state |
| `after` | how long after the previous step (or the event, for the first), in real time. The first step defaults to the vendor's normal delay (Banking Circle's `PROCESSING_DELAY`); later steps default to `0` |
| `after_lab` | instead of `after`: lab-clock time. The step applies once the lab clock is that far past the previous step, whether time passed or the clock was moved. "Returned two days later" without waiting two days |
| state params | per state: `reason`, `reason_code`, `reason_description`; the hook lists which (§8) |

Rules:

- **Validated when armed.** Each `to` must be reachable from the state
  before it. If not, the answer is `400` naming the edge, for example
  "`Reversed` cannot follow `OutgoingPaymentRejected`".
- **The automatic first state is not in the path.** Banking Circle books a
  payout (`OutgoingPaymentBooked`) the moment it arrives, trap or no trap.
  The path starts after that.
- **A path may stop short.** If the last step is not terminal, the subject
  stays there for good. An empty path, `[]`, leaves a Banking Circle
  payout booked forever: today's `Pending` outcome.
- **Every step goes through the vendor's own engine:** the same ledger
  move, notifications, report rows and status answer as a transition no
  trap touched.
- **The books come first.** If the books cannot follow a step (for
  example `OutgoingPaymentProcessed` with too little on the safeguarding
  account), the vendor's own rule applies instead (`MissingFunding`). The
  rest of the path is dropped, and the trap's activity says
  `trap.diverted` ([OPEN 7](open.md)).

### 4.2 Delivery: notifications, callbacks, webhooks

For hooks on something a vendor sends: Banking Circle notifications, B4B
callbacks, ACI webhooks.

| Effect | |
| --- | --- |
| `{"drop": true}` | never sent. The vendor's records say it was produced. For Banking Circle that means no retry and no deactivation: the platform simply never heard |
| `{"copies": 3}` | sent three times in all |
| `{"delay": "30s"}` / `{"delay_lab": "1h"}` | held, then sent on its own, so it arrives after notifications produced later: out of order |

`copies` and a delay can be combined. Banking Circle batches
notifications, so a trap acts on one notification *before* batching; a
held one later goes out in a batch of its own. Pausing a subscription
stays a setter: it holds everything for one subscription, where a trap
picks out single notifications.

### 4.3 `respond`: answering a vendor-shaped request

For the `http.request` hook every sim has (§9.7).

| Effect | |
| --- | --- |
| `{"respond": {"status": 503, "body": {…}, "headers": {…}}}` | answered with this instead; the request has no effect |
| `{"delay": "8s"}` | handled normally, after the delay: for client timeouts |
| `{"close": true}` | the connection is dropped with no answer |

The body is sent as given. The trap author writes the vendor's error shape
if the test cares about it.

## 5. Time

**[PROPOSED]**

- `after` and `delay` are real elapsed time. `after_lab` and `delay_lab` are
  lab-clock time. Lab time also passes on its own (the offset is constant),
  so `after_lab: "2h"` with no clock move applies after two real hours.
- A clock move refused because the clock is held (a settlement run in
  flight) does not move lab time, so `after_lab` steps wait with it.
- Which one is the default when a test writes neither is
  [OPEN 6](open.md). The proposal: real time, because that is what elapsed
  time means everywhere else in the lab (`internal/labclock`).

## 6. Lifecycle

**[PROPOSED]**

```text
armed ──(times reached)──▶ spent
  │  └──(expires_at)─────▶ expired
  └──(DELETE)────────────▶ disarmed
```

- A spent, expired or disarmed trap stays listed, with what it caught,
  until a reset or a `DELETE`. "Did my trap fire?" is answered by reading
  it.
- `DELETE` disarms the trap **and cancels its pending steps**. The answer
  lists what was cancelled ([OPEN 9](open.md)).
- `POST /sim/reset` disarms every trap and cancels every pending step
  ([sim-control.md §6](sim-control.md#6-post-simreset)).
- Traps are in memory. A sim restart loses them, except the standing traps
  a sim arms from its own environment at start
  (`B4B_FORCE_FAILURE_BENEFICIARY_IDS`, `VERIFY_FORCE_DECLINE_IDS`). A sim
  that keeps its records on disk (B4B, Banking Circle) leaves a record a
  lost trap had caught where the trap had it, rather than walking it the
  vendor's own way.

## 7. What a trap leaves behind

**[PROPOSED]**

- **Activity**, on a `traps` log the sim adds: `trap.armed`, `trap.caught`
  (the subject), `trap.step` (each step applied), `trap.diverted`,
  `trap.disarmed`.
- **On the sim's existing logs**, an event a trap caused carries
  `detail.trap` (`trp_0003`). So the console, or a test, can tell a lab-made
  `Reversed` from one the engine came to on its own. Drawing that mark is
  a design-system change first ([design-system.md](../design-system.md)).
- **A log line** per catch and per step, in `GET /sim/logs`.
- **Nothing on the vendor surface.**

## 8. Endpoints

**[PROPOSED]**

On each sim's control listener:

| Route | |
| --- | --- |
| `GET /sim/traps/hooks` | the catalogue: each hook, its subject, its match fields, its effect kind; for `path`, its state graph and each state's params |
| `GET /sim/traps?state=armed` | the traps, newest first; `state` filters |
| `POST /sim/traps` | arm one. `201` with the trap; `400` with the reason |
| `GET /sim/traps/{id}` | one trap |
| `DELETE /sim/traps/{id}` | disarm it and cancel its pending steps; `200` with what was cancelled |
| `DELETE /sim/traps` | disarm all |

`GET /sim/traps/hooks`, one entry:

```json
{"hook": "payment.outgoing",
 "about": "A payout arriving over the bridge from B4B, once B4B approved it.",
 "subject": "payment_id",
 "match": [
   {"field": "creditor.iban", "type": "iban", "about": "the receiving account's IBAN; a vIBAN is an IBAN"},
   {"field": "amount", "type": "decimal"}
 ],
 "effect": "path",
 "graph": {
   "start": "OutgoingPaymentBooked",
   "next": {
     "OutgoingPaymentBooked": ["OutgoingPaymentProcessed", "OutgoingPaymentRejected", "MissingFunding"],
     "OutgoingPaymentProcessed": ["Reversed", "Returned"]
   },
   "params": {
     "OutgoingPaymentRejected": ["reason_code", "reason_description"],
     "Reversed": ["reason"],
     "Returned": ["reason_code", "reason_description"]
   }
 },
 "first_step_default_after": "2s"}
```

The catalogue is the documentation an agent reads. A hook that is not in
it does not exist.

## 9. Hooks by vendor

**[PROPOSED]** Phases are from [plan.md](plan.md).

### 9.1 banking-circle

**`payment.outgoing`** (phase 1). A payout arriving over the bridge from
B4B, once B4B approved it. Subject: the Banking Circle `paymentId`.

| Match field | |
| --- | --- |
| `payment_id` | `bcp_…`, B4B mints it |
| `external_ref` | what B4B forwarded as `externalRef` |
| `creditor.iban` | the receiving account's IBAN, a vIBAN included |
| `creditor.account_number`, `creditor.financial_institution` | a UK account number and sort code, when the payout has no IBAN |
| `creditor.holder` | the beneficiary's name as sent |
| `amount`, `currency` | |

```text
OutgoingPaymentBooked ──▶ OutgoingPaymentProcessed ──▶ Reversed
   │  (automatic)                     └──────────────▶ Returned
   ├──▶ OutgoingPaymentRejected
   └──▶ MissingFunding                 (──▶ OutgoingPaymentProcessed?  OPEN 8)
```

| `to` | What the bank does | Notifications | Params |
| --- | --- | --- | --- |
| `OutgoingPaymentProcessed` | moves the money off the safeguarding account; credits the business bank (`BANK_CREDIT_URL`) | `OutgoingPaymentProcessed` | — |
| `OutgoingPaymentRejected` | moves nothing; listed on the rejection report | `OutgoingPaymentRejected` | `reason_code`, `reason_description` (new: a rejection carries a fixed reason today) |
| `MissingFunding` | moves nothing | `MissingFunding` | — |
| `Reversed` | moves the money back, with a second booking (`Engine.Reverse`) | `OutgoingPaymentBooked`, then `Reversed` | `reason` |
| `Returned` | the payout stays processed; the money comes back as a new incoming payment, `return: true` (`Engine.Return`) | `IncomingPaymentBooked`, `IncomingPaymentProcessed` | `reason_code`, `reason_description` |

The owner's two examples:

```http
POST /sim/traps
{"label": "merchant 7 rejected",
 "hook": "payment.outgoing",
 "match": {"creditor.iban": "GB00SIM0000000000777"},
 "effect": {"path": [{"to": "OutgoingPaymentRejected", "reason_code": "AC01",
                      "reason_description": "Incorrect account number"}]}}
```

```http
POST /sim/traps
{"label": "merchant 7 processed, then reversed",
 "hook": "payment.outgoing",
 "match": {"creditor.iban": "GB00SIM0000000000777"},
 "effect": {"path": [{"to": "OutgoingPaymentProcessed"},
                     {"to": "Reversed", "after": "10s", "reason": "scheme"}]}}
```

Returned two days after it was processed, by the lab clock. Move the clock
to see it, or wait:

```json
{"hook": "payment.outgoing", "match": {"external_ref": "sttl_v1:4c1e…:7"},
 "effect": {"path": [{"to": "OutgoingPaymentProcessed"},
                     {"to": "Returned", "after_lab": "48h", "reason_code": "AC04",
                      "reason_description": "Closed account number"}]}}
```

Today's outcome queue, `POST /sim/payments/outcomes
{"outcomes": ["Rejected", "Pending"]}`, stays as a shortcut. It arms one
`match: {}` trap per outcome, in order: `Rejected` is
`[{"to": "OutgoingPaymentRejected"}]` and `Pending` is `[]`. Its `GET`
lists the traps it armed that are still waiting.

**`notification.deliver`** (phase 2). Each notification, before it is
batched for a subscription. Effect: delivery (§4.2).

| Match field | |
| --- | --- |
| `subscription_id`, `endpoint` (prefix) | which subscriber |
| `event_type` | `OutgoingPaymentProcessed`, `Reversed`, … |
| `payment_id`, `external_ref`, `creditor.iban`, `amount`, `currency` | the payment it is about |

The platform never hears that one payout was processed, and its sweep has
to find out from the intraday report:

```json
{"hook": "notification.deliver",
 "match": {"event_type": "OutgoingPaymentProcessed", "creditor.iban": "GB00SIM0000000000777"},
 "effect": {"drop": true}}
```

**`payment.incoming`** (phase 3). Money landing on a safeguarding account:
Worldline's lump sum, or the console's *Fund safeguarding accounts*. Match:
`currency`, `amount`, `reference` (prefix `WORLDLINE SETTLEMENT` takes the
acquirer's wires). Graph: `IncomingPaymentBooked → IncomingPaymentProcessed`;
`[]` leaves it booked. The balance is credited at booking, as today.

**`report.read`** (phase 3). A payment's row in the intraday
reconciliation report. `{"hide_for": {"after_lab": "2h"}}` leaves it out of
the report for that long, while its notifications and status answer stay
true: a report running behind the bank, which is the case the sweep's
fallback to the status read is for.

### 9.2 b4b

**`payment.create`** (phase 1). A payout accepted by
`POST /oversight/v1/payments`. Subject: B4B's payment id.

| Match field | |
| --- | --- |
| `beneficiary_id`, `beneficiary.external_ref` (the MID) | |
| `company_id`, `debtor_viban`, `creditor.account` | |
| `amount`, `currency`, `external_ref` | |

```text
B4BAccepted ─▶ B4BSanctionsPending ─▶ B4BSanctionsApproved ─▶ B4BTMPending ─▶ B4BTMApproved ─▶ (bridge to BC)
 (automatic)        │                        │                      │
                    └────────────────────────┴──────────────────────┴──▶ B4BFailed
```

A step may name a state further along. B4B walks the states in between at
its normal per-hop delay (`B4B_PROCESSING_DELAY`), and calls back on each,
as it does today. So:

- hang at TM review: `[{"to": "B4BTMPending"}]`
- fail at sanctions after five seconds:
  `[{"to": "B4BSanctionsPending"}, {"to": "B4BFailed", "after": "5s", "reason": "sanctions hit"}]`
- the default (no trap) is a walk to `B4BTMApproved`, then the bridge
  into Banking Circle, where a `payment.outgoing` trap can take it

`B4B_FORCE_FAILURE_BENEFICIARY_IDS` stays. Each id becomes a standing trap
at start-up: `{"match": {"beneficiary_id": "…"}, "times": null,
"effect": {"path": [{"to": "B4BTMPending"}, {"to": "B4BFailed"}]}}`, which
is what it does today.

Sanctions on a beneficiary, company or person stay setters (`PUT
/sim/…/sanctions`). They change the gate's answer for everything, and the
gate then fails the payment by its own rule.

**`callback.deliver`** (phase 2). Each lifecycle callback. Match:
`payment_id`, `status` (the state it reports), `beneficiary_id`,
`callback_url` (prefix). Effect: delivery (§4.2).

### 9.3 worldline

**`settlement.cycle`** (phase 2). Each file a cycle cuts, per slot and
currency, and the morning lump sum that goes with it. Subject: the file
name.

| Match field | |
| --- | --- |
| `slot` | `ER` (morning) or `AR` (afternoon confirmation) |
| `currency` | |
| `business_date` | the day the file covers |
| `mid` | the file has a row for this MID |

```json
"effect": {
  "file":     {"skip": true} | {"delay_lab": "2h"} | {"copies": 2} | {"corrupt": "truncate" | "bad_pgp"},
  "lump_sum": {"skip": true} | {"amount_delta": "-10.00"} | {"delay_lab": "3h"}
}
```

These cover: no file today; a file after the platform stopped looking; the
same file twice; a file the platform cannot decrypt; money short against
the file, which the balance check and reconciliation have to notice; and
money late. They apply in fixture mode too.

**`enrolment.batch`** (phase 2). Each submerchant row of an enrolment
batch. Match: `pf_account_id`, `batch` (the file name). Effects:
`{"reject": {"reason_code": "…"}}` (today's `/sim/enrolment/reject`, which
becomes a shortcut) or `{"delay_lab": "4h"}` (the receipt comes later).

Later, maybe: **`sftp.session`**, for each SFTP login (refuse it, or drop
the connection mid-transfer).

### 9.4 verify

**`aml.decision`** (phase 2). Match: `merchant_application_id`. Effect:
`{"decision": "declined"}`, the same negative decision
`VERIFY_FORCE_DECLINE_IDS` gives today. That variable stays, as standing
traps.

### 9.5 verification

**`check`** (phase 2). Match: `check` (`bank-account`, `screening`,
`identity`, `company`) and the subject's fields: `iban`, `account_number`,
`sort_code`, `company_number`, `name`. Effect: `{"outcome": "…"}`, one of
that check's outcomes. `POST /sim/outcome` stays the setter for the answer
when no trap matches.

### 9.6 aci

**`webhook.deliver`** (phase 3). Match: `merchant_transaction_id`,
`result_code`. Effect: delivery (§4.2).

### 9.7 Every sim: `http.request`

**`http.request`** (phase 3). Any request on the sim's **vendor** surface,
never on its control listener, so a trap cannot lock the lab out. Match:
`method`, `path` (with `*` for one segment), and later `header.<name>`.
Effect: `respond` (§4.3). One piece of shared middleware. *Built
2026-11-12 on verification* with `method` and `path`; a `header.<name>`
match is refused until a test needs one. A client that gives up during a
`delay` does not stop it: the vendor still handles the request once the
delay ends, and nothing records that the answer went unread.

The platform's next two payment-status reads get a `503`:

```json
{"hook": "http.request",
 "match": {"method": "GET", "path": "/api/v1/payments/singles/*/status"},
 "times": 2,
 "effect": {"respond": {"status": 503, "body": {"message": "Service Unavailable"}}}}
```
