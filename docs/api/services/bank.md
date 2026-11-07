# Business bank — API

The merchants' own bank, and the far end of every payout: Banking Circle
credits each processed payout here by IBAN (or by account number and sort
code), and the account opens the first time money arrives for it. It is
also the small in-memory ledger `make demo-payment` moves money in.
[catalogue.md](../../catalogue.md) files it as **the far end**: not a
vendor a platform integrates with, and not scaffolding standing in for one
(the lab's catalogue gives it the `vendor` kind so it draws as one). A
platform never calls it; tests, agents and the console (through the lab API) read it to see
whether money arrived. The IBANs are obviously fake (`GB00SIM…`), and
everything is in memory: a restart, or [`POST /sim/reset`](#post-simreset),
puts every balance back to the seed.

## At a glance

| | |
| --- | --- |
| Listens | `:8081`, plain HTTP: the ledger's own API. Compose service `bank` (`http://bank:8081`); host `http://127.0.0.1:8081`.<br>`:9081`, plain HTTP: the **control port** (`SIM_CONTROL_LISTEN`), every `/sim/*` route below and its own `GET /health`. Not published by compose; the lab API reaches it at `http://bank:9081` |
| Auth | None, on every route. Who may credit it is decided on the sending side: Banking Circle checks `BANK_CREDIT_URL` against its own `WEBHOOK_ALLOWLIST`, which lists `bank` and `bank:8081`.<br>The control port has no vendor auth: it takes calls from anything that can reach it, or only from `SIM_CONTROL_ALLOW` (addresses or CIDRs) when that is set |
| Found with | `BANK_URL` (`http://bank:8081`) on payment-api, settlement and the harness.<br>`BANK_CREDIT_URL` on banking-circle: the full `http://bank:8081/internal/credit`; empty turns crediting off.<br>`CONSOLE_URL_BANK` (`http://bank:8081`) on the lab API, which takes the control port as that port plus 1000 unless `CONSOLE_CONTROL_URL_BANK` names it |
| Activity logs | `GET /sim/activity` keeps two, in this order.<br>`credits` (*Payouts credited*): every `POST /internal/credit`, refusals included, op `credit`.<br>`ledger` (*Accounts and transfers*): every `POST /accounts` and `POST /sim/accounts` (op `account.open`) and `POST /transfers` (op `transfer`) |
| Configuration | `LISTEN` (`:8081`).<br>`SIM_CONTROL_LISTEN` (`:9081`, `LISTEN`'s port plus 1000) and `SIM_CONTROL_ALLOW` (unset: anyone who can reach it): the control port.<br>`LAB_CLOCK_URL` (unset: the wall clock; compose: `http://clock:8096/clock`): every ledger entry's `at` is on the [lab clock](clock.md).<br>No config file. The seeded accounts are in code (below) |
| Design record | [catalogue.md](../../catalogue.md), [services/fake-bank.md](../../services/fake-bank.md) (older: no credit route, no InfinitePay accounts), [console.md](../../console.md#banks), [scenarios/payment-to-webhook.md](../../scenarios/payment-to-webhook.md) |

## Vendor surface

The ledger's own API. Amounts are decimal strings with at most two decimals
(`"25.00"`), held as integer cents. Bodies are decoded strictly: an unknown
field is a `400`. Errors are `{"error": "<message>", "status": <code>}`.

Seeded at start:

| `id` | `iban` | `holder` | `currency` | `balance` | `kind` |
| --- | --- | --- | --- | --- | --- |
| `acc_infinite_eur` | `GB00SIMINF00000001` | InfinitePay Ltd | EUR | `0.00` | `infinite` |
| `acc_infinite_gbp` | `GB00SIMINF00000002` | InfinitePay Ltd | GBP | `0.00` | `infinite` |
| `acc_alice` | `GB00SIM0000000000001` | Alice Simulator | EUR | `10000.00` | `demo` |
| `acc_bob` | `GB00SIM0000000000002` | Bob Simulator | EUR | `500.00` | `demo` |
| `acc_merchant` | `GB00SIM0000000000003` | Merchant Simulator | EUR | `0.00` | `demo` |

Merchant accounts are not seeded: `POST /internal/credit` opens them.

### `GET /accounts`

Every account, in no particular order.

Answers `200`:

```json
{"accounts": [{"id": "acc_alice", "iban": "GB00SIM0000000000001", "holder": "Alice Simulator",
               "currency": "EUR", "balance": "9975.00", "kind": "demo", "opened_by": "seed"}]}
```

`kind` is `infinite`, `merchant`, `demo`, or `""` for an account opened
with `POST /accounts`. `opened_by` is `seed`, the reference of the credit
that opened the account, or absent.

### `GET /accounts/{id}`

One account.

Path: `id`, an account id or an IBAN. An account opened by account number
is keyed `"<financial_institution> <account_number>"` (URL-encode the
space).

Answers `200` with the account object above.

- `404`: `account not found`.

### `POST /accounts`

Opens an account by hand: the console's *Open account* on the business
bank.

Body:

```json
{"holder": "Acme Ltd", "currency": "EUR", "openingBalance": "100.00"}
```

`holder` is required. `currency` defaults to `EUR`, `openingBalance` to
zero; its sign is not checked.

Answers `201` with the account: id `acc_<12 hex>`, a generated IBAN
(`GB00SIM` and 12 digits, one digit shorter than the seeded ones), `kind`
`""`, no `opened_by`.

- `400`: `holder required`, an unparseable `openingBalance`, malformed JSON
  or an unknown field.

Side effects: the opening balance writes no ledger entry. Recorded as
`account.open` in the `ledger` log.

### `POST /transfers`

Moves money between two accounts. This is payment-api's call.

Body:

```json
{"fromAccountId": "acc_alice", "toIban": "GB00SIM0000000000003", "amount": "25.00",
 "currency": "EUR", "reference": "invoice-42", "paymentId": "pay_9f8e7d6c5b4a"}
```

The debtor is named by account id only. The creditor is `toAccountId` or
`toIban`; `toAccountId` wins when both are set, even if it matches nothing.
`currency` defaults to the debtor's. `reference` and `paymentId` are copied
onto both entries.

Answers `201`:

```json
{"from": {"id": "acc_alice", "balance": "9975.00", "…": "…"},
 "to": {"id": "acc_merchant", "balance": "25.00", "…": "…"},
 "entries": [
   {"id": "le_4b1c9e0a7d22", "accountId": "acc_alice", "paymentId": "pay_9f8e7d6c5b4a",
    "amount": "-25.00", "currency": "EUR", "reference": "invoice-42", "at": "2026-09-25T09:00:00Z"},
   {"id": "le_0e7f3a915c4b", "accountId": "acc_merchant", "paymentId": "pay_9f8e7d6c5b4a",
    "amount": "25.00", "currency": "EUR", "reference": "invoice-42", "at": "2026-09-25T09:00:00Z"}]}
```

- `400`: `amount must be a positive decimal`, `cannot transfer to the same
  account`, malformed JSON or an unknown field.
- `404`: `debtor account not found`, `creditor account not found`.
- `409`: `currency mismatch (sim is single-ccy per account)`,
  `insufficient funds`.

Side effects: two ledger entries, debit then credit. Recorded as `transfer`
in the `ledger` log.

### `GET /ledger`

Every ledger entry in the order written: credit-rail entries (`ent_<n>`)
and transfer legs (`le_<hex>`).

Answers `200`:

```json
{"entries": [{"id": "ent_3", "accountId": "acc_gb00simmerch0000001", "amount": "125.00",
              "currency": "EUR", "reference": "SETL-1", "at": "2026-09-25T09:00:04Z"}]}
```

`entries` is `null`, not `[]`, until the first entry. `paymentId` and
`reference` are omitted when empty; credits never carry `paymentId`.
`amount` is signed. `at` is RFC 3339 on the lab clock.

## Lab plumbing

### `POST /internal/credit`

A payout arriving over the rail. Banking Circle posts here when a payment
reaches `OutgoingPaymentProcessed`: fire-and-forget on its own goroutine,
5 s timeout, no retry; a non-2xx is only logged at Banking Circle.

Body:

```json
{"iban": "GB00SIMMERCH0000001", "account_number": "", "financial_institution": "",
 "holder": "Southwind Coffee Ltd", "amount": "125.00", "currency": "EUR",
 "reference": "SETL-1", "kind": ""}
```

- The beneficiary is `iban`, or, where there is none, `account_number` and
  `financial_institution` (a UK account number and sort code). The account
  is then keyed `"<financial_institution> <account_number>"` in the `iban`
  field. `iban` wins when both are set.
- `currency` defaults to `EUR`.
- `holder` and `kind` are used only when the credit opens the account;
  `kind` defaults to `merchant`. Banking Circle never sends `kind`.

Answers `200`:

```json
{"account": {"id": "acc_gb00simmerch0000001", "iban": "GB00SIMMERCH0000001",
             "holder": "Southwind Coffee Ltd", "currency": "EUR", "balance": "125.00",
             "kind": "merchant", "opened_by": "SETL-1"},
 "opened": true, "credited": "125.00"}
```

- `400`: `iban or account_number required: a rail names the beneficiary's
  account`, `amount: money: …` (unparseable), `amount must be positive`,
  malformed JSON or an unknown field.
- `422`: `account GB00SIMMERCH0000001 is EUR, credit is GBP`, for an
  existing account in another currency.

Side effects: the first credit for an identifier opens its account: id
`acc_` plus the identifier lower-cased with spaces as `_`, holder from the
body or `Beneficiary <identifier>`, `opened_by` the credit's reference.
Every accepted credit writes an `ent_<n>` ledger entry. Every call,
refused or not, is a `credit` event in the `credits` log, for example
*credited 125.00 EUR to GB00SIMMERCH0000001 (Southwind Coffee Ltd) —
account opened*, with `iban`, `reference`, `opened`, `balance` and
`account` in its detail.

## Lab control

On the control port, `:9081` ([the control
contract](../README.md#the-control-contract)). The same `/sim/*` paths
still answer on `:8081` until phase 4 of
[the plan](../../lab-api/plan.md) retires them.

### `GET /health`

Liveness, on both ports. `:8081` answers `{"status": "ok", "service":
"bank"}`; the control port answers `{"status": "ok", "service": "bank",
"surface": "control"}`, and that is the one the lab API's monitor probes.

### `GET /sim/activity`

The two logs, `credits` first.

Query: `limit` (default 100; each log keeps its last 256 events).

Answers `200`:

```json
{"logs": [
  {"name": "credits", "title": "Payouts credited", "note": "Every payout Banking Circle passed on, …",
   "total": 6, "kept": 6, "last": {"…": "…"},
   "events": [{"seq": 6, "at": "2026-11-07T09:00:04Z", "op": "credit", "peer": "10.89.0.12",
               "summary": "credited 733.34 EUR to GB00SIM0000000000042 (Acme Ltd) — account opened",
               "status": "ok",
               "detail": {"iban": "GB00SIM0000000000042", "reference": "set_9c1e0a7b42d3", "opened": "true",
                          "balance": "733.34", "account": "acc_gb00sim0000000000042", "status": "OK"}}]},
  {"name": "ledger", "title": "Accounts and transfers", "…": "…"}]}
```

`status` is `ok` for a 2xx, `warn` for a 4xx, `bad` for a 5xx; a refused
call's summary ends with the bank's own error message. `at` is wall-clock
time, not the lab clock.

### `GET /sim/accounts`

[`GET /accounts`](#get-accounts) on the control port: the same handler,
answering the same, so the lab lists the accounts without calling the
ledger's own API.

### `POST /sim/accounts`

[`POST /accounts`](#post-accounts) on the control port: the same body,
answers and refusals, and recorded the same, as `account.open` in the
`ledger` log.

### `GET /sim/info`

Who this is and what it can do, as every service answers it
([the control contract](../README.md#the-control-contract)):
`"service": "bank"`, `"kind": "vendor"`, `"vendor_listeners": [":8081"]`,
`"control_listener": ":9081"`, `"reset_scopes": ["flow", "all"]`.

### `GET /sim/logs`

The process's own log lines, as every service answers it
([the control contract](../README.md#the-control-contract)): one line per
request on either port, method and path.

### `POST /sim/reset`

Puts the bank back as it was on first start. Both scopes do the same: a
platform never calls the bank, so nothing here was set up once.

- **Clears**: every account that was not seeded, whether a payout or
  `POST /accounts` opened it; the ledger, so the next credit is `ent_1`
  again; both activity logs.
- **Keeps**: the seeded accounts, each back at its seed balance (the table
  above).

`cleared` counts `accounts` closed, seeded `balances` moved back, ledger
`credits` and `transfers`, and `activity` events across both logs.

```json
{"service": "bank", "scope": "flow",
 "cleared": {"accounts": 2, "activity": 4, "balances": 3, "credits": 2, "transfers": 1},
 "kept": ["seeded_accounts"], "at": "2026-11-07T12:00:00Z"}
```

## Called by

- **banking-circle**: `POST /internal/credit` for every payment that
  reaches `OutgoingPaymentProcessed` (`BANK_CREDIT_URL`), the last hop of a
  payout.
- **payment-api**: `POST /transfers`, once per accepted `POST /payments`.
- **settlement** (stand-in): `GET /accounts` and `GET /ledger`, when
  `POST /reports/settlement/generate` runs or the daily cutoff batch fires,
  to build its internal reports from the ledger's credit legs.
- **lab API**, on the control port only (`http://bank:9081`), never the
  ledger's own: `GET /v1/banks` reads `GET /sim/accounts`;
  `POST /v1/banks/core-ledger/accounts` calls `POST /sim/accounts`;
  `GET /v1/services/bank/activity` proxies `GET /sim/activity`;
  `GET /v1/flow` and `GET /v1/flow/hops/{id}` count `credit` events in
  the `credits` log for the `land` hop and the *payouts credited* stat;
  `POST /v1/lab/reset` and `POST /v1/services/bank/reset` call
  `POST /sim/reset`; `GET /v1/logs` and `GET /v1/services/bank/logs` read
  `GET /sim/logs`; `GET /v1/lab` and `POST /v1/services/bank/probe` probe
  the control port's `GET /health`.
- **harness**: `payment-api-to-webhook` reads `GET /accounts/acc_alice`
  and `GET /accounts/acc_merchant` before and after its payment, to prove
  exactly one debit and one credit.
- **by hand**: [payment-to-webhook.md](../../scenarios/payment-to-webhook.md)
  reads `GET /accounts`, `GET /accounts/{id}` and `GET /ledger`;
  [agent.runbooks.md](../../../agent.runbooks.md) reads `GET /sim/activity`.
- **the platform**: never. A merchant's real bank is not something a
  platform integrates with.

Every route above is held to the source by `internal/console/apidocs_test.go`: a route added, moved or removed without this page following fails `go test ./internal/console`.
