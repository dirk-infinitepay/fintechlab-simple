# B4B Payments — API

Simulates B4B Payments' Oversight API, the payout rail the platform pays
merchants through. It is a **vendor** simulation (see
[catalogue.md](../../catalogue.md)): a platform swaps it for the real thing by
pointing its B4B base URL at the real Oversight API. The wire shapes are
meant to be exact: the RS512 bearer JWT, JSON-number amounts,
`{account, financialInstitution, country}` account references,
`chargeBearer` `SHA`/`BEN`/`OUR`, the boarding refusals (duplicate
`external_ref`, missing identity document, second extended profile), the
payment gates and the `B4BAccepted`→…→`B4BTMApproved`/`B4BFailed` lifecycle
with a callback on every transition. The judgement is not real: every
company, person and beneficiary screens `pass` at creation, documents are
measured and discarded, statuses move only through the lab-only
`PUT /sim/...` hooks on its control port, a payout walks the lifecycle on
a fixed delay unless a [trap](#traps) decides its walk, and an approved
payout reaches Banking Circle through a lab bridge rather than a real
banking relationship. Everything it is told is persisted to one state file.

## At a glance

| | |
| --- | --- |
| Listens | `:8086`, plain HTTP (no TLS): the Oversight API. Compose service `b4b` (`http://b4b:8086` on the compose network); host `http://127.0.0.1:8086`.<br>`:9086`, plain HTTP: the **control port** (`SIM_CONTROL_LISTEN`), every `/sim/*` route below and its own `GET /health`. Not published by compose; the lab API reaches it at `http://b4b:9086` |
| Auth | Every `/oversight/v1/*` route: `Authorization: Bearer <RS512 JWT>`, see [Authentication](#authentication-the-rs512-bearer-jwt). `GET /health`: none. The control port has no vendor auth: it takes calls from anything that can reach it, or only from `SIM_CONTROL_ALLOW` (addresses or CIDRs) when that is set. The old `/sim/*` paths on `:8086` keep the JWT until phase 4, but for `GET /sim/activity` |
| A platform points here with | `B4B_API_BASE_URL` (`http://127.0.0.1:8086`), `B4B_JWT_KEY_ID` (`b4b-mock-1`) and `B4B_JWT_PRIVATE_KEY` (the PEM), as handed out by the lab API at `GET :8097/v1/connect/env/b4b`. The settlement stand-in uses `B4B_URL`, `B4B_JWT_PRIVATE_KEY_PATH` and `B4B_JWT_KEY_ID`, and sends its own `B4B_CALLBACK_URL` as each payment's `callback_url`. Callbacks reach the platform at whatever b4b's own `B4B_CALLBACK_URL` names |
| Activity logs | `GET /sim/activity` keeps three in-memory rings of 256 events. `payments` (*Payouts received*): every `POST /oversight/v1/payments` call, op `payment.create`, with what was answered, refusals included. `callbacks` (*Callbacks sent*): one event per callback, op `callback`: delivered, refused after the last attempt, or not sent because there was nowhere to send it. `traps` (*Traps*): every trap armed, every payout one caught, each hop it took and each one disarmed, ops `trap.armed`, `trap.caught`, `trap.step`, `trap.disarmed`. A payout a trap caught, and each callback for a transition the trap made, carries `detail.trap`. Boarding, beneficiary and other `/sim` calls are not logged |
| Configuration | `LISTEN` (`:8086`).<br>`SIM_CONTROL_LISTEN` (`LISTEN`'s port plus 1000, `:9086`) and `SIM_CONTROL_ALLOW` (empty: any caller): the control port.<br>`B4B_JWT_KEYS_DIR` (`/keys/b4b-keys`): where the keypair is loaded from, or generated into.<br>`B4B_JWT_PUBLIC_KEY_PATH` (empty): verify against this public key instead of the generated pair.<br>`B4B_JWT_KEY_ID` (`b4b-mock-1`): the `kid` every token must carry.<br>`B4B_CALLBACK_URL` (empty): the client-account callback endpoint; must pass the allowlist or b4b exits at start.<br>`WEBHOOK_ALLOWLIST` (`settlement,settlement:8083,localhost,127.0.0.1`; compose adds `host.containers.internal`): hosts a callback may go to.<br>`RETRY_BACKOFF` (`1s,2s,4s`), `MAX_ATTEMPTS` (`4`): callback retries.<br>`B4B_PROCESSING_DELAY` (`300ms`): wait before each lifecycle hop, a trap's hops included.<br>`B4B_FORCE_FAILURE_BENEFICIARY_IDS` (empty): comma-separated `beneficiary_id` values, as sent on the payment, whose payments end `B4BFailed` instead of `B4BTMApproved`: each is a [standing trap](#standing-traps), armed at start.<br>`B4B_ENFORCE_BENEFICIARY_COMPANY` (`false`): refuse a payment whose `company_id` is not the beneficiary's.<br>`B4B_STATE_PATH` (`/b4b-data/state.json`, compose binds `./b4b-data`): the state file; empty keeps everything in memory.<br>`BANKING_CIRCLE_URL` (`http://banking-circle:8095`): the bridge target.<br>`LAB_CLOCK_URL` (compose: `http://clock:8096/clock`): every `created_at`, `updated_at` and `disabled_at` follows the [lab clock](../../plugins.md#the-clock) |
| Design record | [ARCHITECTURE-b4b-oversight.md](../../ARCHITECTURE-b4b-oversight.md) (boarding), [ARCHITECTURE-vendor-corrections.md](../../ARCHITECTURE-vendor-corrections.md) §4 and Addendum §A/D/F (payout half), [ARCHITECTURE-phase3-corrections.md](../../ARCHITECTURE-phase3-corrections.md) §4 (wire-shape fixes), [structure/b4b.md](../../structure/b4b.md) (file by file) |

## Vendor surface

What a platform's B4B client calls, under `/oversight/v1`, in the order a
boarding chain walks it: company, addresses, people, extended profile,
vIBANs, uploads, then beneficiaries and payments.

Bodies are read leniently: a field this mock has no column for is not
refused. On the boarding records (company, address, person, extended
profile) it is kept and echoed back under `extra`; on beneficiaries,
payments and vIBANs it is accepted and dropped. Malformed JSON is a 400.
Every error this service writes has one shape:

```json
{"error": "b4b: not found: company \"cmp_3f9a1c2b7d4e\"", "status": 404}
```

The status codes follow one rule (`writeStoreError`): 400 for something the
caller fixes and resends, 404 for an unknown id, 422 for "you already did
this" or "a rule refuses it" (duplicate, unwaived document requirement,
payment gates).

Ids are B4B's own, minted with a prefix and 12 hex characters (`cmp_`,
`adr_`, `per_`, `ext_`, `vib_`, `doc_`, `ben_`); payment ids are UUIDv4.
Wherever a route takes a company `{id}`, the company's `external_ref` works
too. Timestamps are RFC 3339 UTC on the lab clock.

### Authentication: the RS512 bearer JWT

Every `/oversight/v1/*` call carries `Authorization: Bearer <token>`
(prefix exactly `Bearer `), and so does every old `/sim/*` path on `:8086`
but `GET /sim/activity` until phase 4. The control port asks for none. The
token is parsed by hand (`internal/b4b/jwt.go`, `VerifyBearerToken`), with
no JWT library:

- **Format.** Compact JWS: three base64url segments **without padding**. A
  padded segment fails to decode and is refused.
- **Header.** `alg` must be `RS512`. `kid` must be present and equal
  `B4B_JWT_KEY_ID` (default `b4b-mock-1`). Other header fields are ignored.
- **Claims.** `aud` must be the JSON **string** `"b4b-payments"`. An
  array-valued `aud` (`["b4b-payments"]`, legal under RFC 7519) fails to
  parse and is refused. No other claim is read: `exp`, `iat`, `nbf`, `iss`
  and `sub` are ignored, so a token never expires here.
- **Signature.** RSASSA-PKCS1-v1_5 with SHA-512 over
  `<header>.<payload>`, checked against one RSA public key.
- **Where the key comes from.** If `B4B_JWT_PUBLIC_KEY_PATH` is set, that PEM
  (PKIX `PUBLIC KEY` or PKCS#1 `RSA PUBLIC KEY`) is the only key, which is how
  a real vendor holds a client's credential. Otherwise b4b loads
  `B4B_JWT_KEYS_DIR/private.pem` (PKCS#1 or PKCS#8) and verifies with its
  public half; if the file does not exist it generates an RSA-2048 pair,
  writes `private.pem` (PKCS#1) and `public.pem` (PKIX), both mode 0644, and
  prints the public key to stdout. Compose mounts
  `${LAB_KEYS_DIR:-./keys}/b4b-keys` read-write here; settlement mounts it
  read-only and signs with `private.pem`. This is inverted from a real
  vendor (the lab owns the private key and hands it to the client); the lab
  API serves it at `GET :8097/v1/connect/env/b4b` and
  `GET :8097/v1/connect/files/b4b/b4b-jwt-private.pem`, and signs nothing
  with it.

The token a Go client needs is exactly what `b4b.SignBearerToken(key, kid)`
produces:

```text
header  {"alg":"RS512","kid":"b4b-mock-1"}
claims  {"aud":"b4b-payments"}
```

Refusals are 401 with no detail on the wire:
`{"error":"missing bearer token","status":401}` when the header is absent or
not `Bearer `, `{"error":"invalid bearer token","status":401}` for anything
else. The exact reason is on b4b's stdout, for example
`b4b: rejected POST /oversight/v1/payments: b4b: aud "payments" rejected, want "b4b-payments"`.

### `POST /oversight/v1/companies`

Boards a company. The registered office is supplied inline.

Body:

```json
{
  "external_ref": "merchant-0001",
  "legal_name": "Southwind Coffee Ltd",
  "trading_name": "Southwind Coffee",
  "company_type": "kyb",
  "acquirer": "Worldline",
  "primary_channel": "Infinite Payments",
  "merchant_category_code": "5814",
  "company_documents_waived": false,
  "person_documents_waived": false,
  "address": {"address_line_1": "1 Simulation Way", "city": "London", "postal_code": "EC1A 1AA", "country": "GB"}
}
```

`legal_name` and `address` are required; the address needs
`address_line_1`, `city` and a two-letter `country` (`address_line_2`,
`region`, `postal_code` optional). `merchant_category_code`, when present,
must be four digits. The two waiver flags switch off the document
requirements on the extended profile and on natural persons.

Answers `201` with the company:

```json
{
  "id": "cmp_3f9a1c2b7d4e",
  "external_ref": "merchant-0001",
  "legal_name": "Southwind Coffee Ltd",
  "merchant_category_code": "5814",
  "company_documents_waived": false,
  "person_documents_waived": false,
  "address": {"address_line_1": "1 Simulation Way", "city": "London", "postal_code": "EC1A 1AA", "country": "GB"},
  "status": "active",
  "sanctions_status": "pass",
  "created_at": "2026-11-07T09:30:00Z",
  "updated_at": "2026-11-07T09:30:00Z"
}
```

- `400`: a missing or invalid field (`b4b: invalid input: legal_name is required`).
- `422`: the `external_ref` is already taken
  (`b4b: already exists: a company with external_ref "merchant-0001" already exists (id cmp_…)`).

Side effects: the inline address is also stored as an address of type
`registered`, so `GET …/addresses` lists it. Persisted. No callback.

### `GET /oversight/v1/companies`

Lists every boarded company, oldest first. The design record calls this a
lab convenience; it is not confirmed at the real vendor.

Answers `200` with `{"companies": [<company>, …]}`.

### `GET /oversight/v1/companies/{id}`

One company, by B4B id or by `external_ref`.

Answers `200` with the company, `404` for an unknown id.

### `POST /oversight/v1/companies/{id}/addresses`

Registers another address against a company. The address fields sit at the
top level, next to `type`:

```json
{"type": "trading", "address_line_1": "2 High Street", "city": "Leeds", "postal_code": "LS1 1AA", "country": "GB"}
```

`type` is required (the comment names `registered`, `trading`, `operating`;
any non-empty value is accepted). Address rules as on the company.

Answers `201`:

```json
{"id": "adr_9c1d2e3f4a5b", "company_id": "cmp_3f9a1c2b7d4e", "type": "trading", "address_line_1": "2 High Street", "city": "Leeds", "postal_code": "LS1 1AA", "country": "GB", "created_at": "2026-11-07T09:31:00Z"}
```

`400` for a missing field, `404` for an unknown company.

### `GET /oversight/v1/companies/{id}/addresses`

Answers `200` with `{"addresses": [<address>, …]}`, the registered office
included. `404` for an unknown company.

### `POST /oversight/v1/companies/{id}/people`

Attaches a natural person or a shareholding company. Both go through this
endpoint, told apart by `entity_type`.

Body, natural person (the default when `entity_type` is empty):

```json
{
  "entity_type": "natural_person",
  "first_name": "Ada",
  "last_name": "Lovelace",
  "date_of_birth": "1815-12-10",
  "nationality": "GB",
  "roles": ["director", "ubo"],
  "ownership_percentage": "51",
  "is_pep": false,
  "document_type": "passport",
  "document_number": "P1234567",
  "document_country": "GB"
}
```

Also accepted: `middle_name`, `email`, `phone`, `address` (address object),
`external_ref`. A legal entity needs `"entity_type": "legal_entity"` and
`legal_name`, optionally `registered_company_no`.

Rules: a natural person needs `first_name`, `last_name` and
`date_of_birth` (400 otherwise), and `document_type`, `document_number` and
`document_country` unless the company was created with
`person_documents_waived` (422 otherwise). `external_ref` is echoed but is
not a lookup key, and b4b logs a warning when one is sent.

Answers `201` with the person, both screening statuses `pass`:

```json
{"id": "per_5a6b7c8d9e0f", "company_id": "cmp_3f9a1c2b7d4e", "entity_type": "natural_person", "first_name": "Ada", "last_name": "Lovelace", "date_of_birth": "1815-12-10", "roles": ["director", "ubo"], "is_pep": false, "document_type": "passport", "document_number": "P1234567", "document_country": "GB", "sanctions_status": "pass", "pep_sanctions_status": "pass", "created_at": "2026-11-07T09:32:00Z", "updated_at": "2026-11-07T09:32:00Z"}
```

- `400`: missing name or date of birth, unknown `entity_type`.
- `404`: unknown company.
- `422`: identity document missing on an unwaived company.

Side effects: fires the [person screening callback](#person-screening-callback)
in the background. Persisted.

### `GET /oversight/v1/companies/{id}/people`

Answers `200` with `{"people": [<person>, …]}` in creation order. `404` for
an unknown company.

### `GET /oversight/v1/companies/{id}/people/{personID}`

One person by B4B id. The company `{id}` in the path is not checked: a
person belonging to another company is returned all the same.

Answers `200` with the person, `404` for an unknown `personID`.

### `POST /oversight/v1/companies/{id}/extended`

Creates the company's regulatory profile, once ever. There is no update.

Body:

```json
{
  "registered_company_no": "12345678",
  "nace_codes": ["6201"],
  "documents": ["doc_1a2b3c4d5e6f"],
  "incorporation_date": "2019-04-01",
  "legal_form": "private_limited_company",
  "website": "https://southwind.example",
  "financial": {},
  "payments": {}
}
```

`registered_company_no` is required. `documents` holds upload ids from
`POST /oversight/v1/uploads` and is required unless the company has
`company_documents_waived`. `nace_codes` is not enforced; its absence is
logged, because the vendor's guide and schema disagree.

Answers `201` with the profile (`id` `ext_…`, `company_id`, the fields
above, `created_at`).

- `400`: `registered_company_no` missing, or a document id that was never
  uploaded (`b4b: invalid input: documents doc_x were never uploaded; POST /oversight/v1/uploads first`).
- `404`: unknown company.
- `422`: a profile already exists (checked before any body field is
  validated), naming it: `… already has an extended profile (id ext_…); it can only be created once and there is no update endpoint`;
  or `documents` missing on an unwaived company.

### `GET /oversight/v1/companies/{id}/extended`

The read a client makes before re-posting a profile; it does not need to
know the profile id.

Answers `200` with the profile. `404` for an unknown company or one with no
profile yet.

### `GET /oversight/v1/companies/{id}/extended/{extendedID}`

The spelling the vendor documents. Same handler as the route above.

Answers `200` with the profile; `404` as above, and also when `extendedID`
is not this company's profile
(`extended profile ext_x does not belong to company cmp_…`).

### `POST /oversight/v1/companies/{id}/vibans`

Issues the company a virtual IBAN for a currency.

Body: `{"currency": "EUR"}` (three letters, upper-cased).

Answers `201`:

```json
{"id": "vib_7e8f9a0b1c2d", "company_id": "cmp_3f9a1c2b7d4e", "currency": "EUR", "iban": "GB00SIM4F1C9A0B2D3E4F", "bic": "B4BBSIM5A", "status": "active", "created_at": "2026-11-07T09:33:00Z"}
```

The IBAN is derived from the company id and currency, so it is obviously
fake and stable. Asking again for the same currency answers `201` with the
existing vIBAN, not a second one. `400` for a bad currency, `404` for an
unknown company.

### `GET /oversight/v1/companies/{id}/vibans`

Answers `200` with `{"vibans": [<viban>, …]}`. `404` for an unknown company.

### `POST /oversight/v1/uploads`

Accepts a document and returns a receipt whose id the extended profile can
reference. Three body forms, chosen by `Content-Type`:

- `multipart/form-data`: a `file` part, plus an optional `document_type`
  form field. The file name and content type come from the part.
- `application/json`: metadata only, for a client that stores the file
  elsewhere: `{"document_type": "business_registry_extract", "file_name": "extract.pdf", "content_type": "application/pdf", "size": 20481}`.
- anything else: the raw bytes, with `?document_type=` and the file name in
  an `X-File-Name` header or `?file_name=`.

Answers `201`:

```json
{"id": "doc_1a2b3c4d5e6f", "document_type": "business_registry_extract", "file_name": "extract.pdf", "content_type": "application/pdf", "size": 20481, "sha256": "9f86d081…", "created_at": "2026-11-07T09:34:00Z"}
```

`sha256` is present only when bytes were sent. `400` for an unparseable
multipart body, a missing `file` part or malformed JSON.

Side effects: up to 32 MiB is read, measured, hashed and **discarded**;
nothing is stored or judged. Every upload is accepted. The receipt is
persisted.

### `GET /oversight/v1/uploads/{id}`

Answers `200` with the receipt (never the file, there is none), `404` for
an unknown id.

### `POST /oversight/v1/beneficiaries`

Registers a payee.

Body:

```json
{
  "company_id": "cmp_3f9a1c2b7d4e",
  "external_ref": "MID0001",
  "account_name": "Southwind Coffee Ltd",
  "account_number": "GB00SIM0000000000042",
  "financial_institution": "SC112233",
  "country": "GB",
  "callback_url": "http://settlement:8083/internal/b4b-webhook",
  "sanctions_status": "pass"
}
```

`account_name`, `account_number` and `financial_institution` are required.
`company_id` is optional and, when present, must name a boarded company (id
or `external_ref`). Two lab affordances that are not real wire fields:
`callback_url` (overrides `B4B_CALLBACK_URL` for this payee; must pass
`WEBHOOK_ALLOWLIST`) and `sanctions_status` (`pass`, the default, `review`
or `fail`, to register a payee that is already blocked).

Answers `201`:

```json
{"id": "ben_0a1b2c3d4e5f", "company_id": "cmp_3f9a1c2b7d4e", "external_ref": "MID0001", "account_name": "Southwind Coffee Ltd", "account_number": "GB00SIM0000000000042", "financial_institution": "SC112233", "country": "GB", "status": "active", "sanctions_status": "pass", "callback_url": "http://settlement:8083/internal/b4b-webhook"}
```

- `400`: a required field missing (`account_name is required`), an invalid
  `sanctions_status`, or `callback_url rejected: allowlist: …`.
- `404`: `company_id` names a company that was never boarded.

Side effects: fires the [beneficiary callback](#beneficiary-callback) in
the background, every time. Registering the same `external_ref` again mints
a new id; lookups by `external_ref` then resolve to the newest record, while
the old id still reads the old one. Persisted.

The lab registers payees through
[`POST /sim/beneficiaries`](#post-simbeneficiaries) on the control port,
which is this route without the JWT.

### `GET /oversight/v1/beneficiaries/{id}`

One payee, by B4B id or `external_ref`. Also the documented recovery read
when a beneficiary callback was missed.

Answers `200` with the beneficiary. **Never 404**: an unknown id
auto-vivifies a deterministic synthetic payee, where the real API answers
404:

```json
{"id": "MID0042", "account_name": "B4B Merchant MID0042", "account_number": "B4B5D41402ABC4B2A", "financial_institution": "B4BBANK76A1", "status": "active", "sanctions_status": "pass"}
```

The synthetic record is not stored unless a `PUT /sim/beneficiaries/...`
hook changes it.

### `POST /oversight/v1/payments`

Creates a payout. Checked, gated, then accepted and walked through the
lifecycle asynchronously.

Body:

```json
{
  "external_ref": "stl_20261107_MID0001",
  "beneficiary_id": "MID0001",
  "company_id": "cmp_3f9a1c2b7d4e",
  "callback_url": "http://settlement:8083/internal/b4b-webhook",
  "sca_applied": false,
  "amount": {"amount": 12.34, "currency": "EUR"},
  "currencyOfTransfer": "EUR",
  "debtorViban": {"account": "GB00SIM0000000000001", "country": "GB"},
  "debtorAccount": {"account": "GB00SIM0000000000009", "country": "GB"},
  "creditorAccount": {"account": "GB00SIM0000000000042", "financialInstitution": "SC112233", "country": "GB"},
  "creditorName": "Southwind Coffee Ltd",
  "chargeBearer": "SHA",
  "requestedExecutionDate": "2026-11-07",
  "remittanceInformation": {"line1": "Settlement 2026-11-07"}
}
```

Checks, in order, each a `400`: `beneficiary_id` required; `callback_url`
(a lab affordance) must pass the allowlist; `amount.amount` must be a
decimal with at most two places (a JSON number; a quoted decimal string is
also decoded); `currencyOfTransfer`, `debtorViban.account` and
`creditorAccount.account` required; `chargeBearer` one of `SHA`, `BEN`,
`OUR`. A missing `amount` is not refused: it reads as zero.
`amount.currency` is not checked.

Then the gates, each a `422` that forwards nothing. The beneficiary is
resolved by id, then `external_ref`, else auto-vivified:

- its `sanctions_status` must be `pass`;
- it must not be `disabled`;
- `creditorAccount.account`, `creditorAccount.financialInstitution` and
  `creditorName`, each when present, must match its `account_number`,
  `financial_institution` and `account_name` (case-insensitive): a
  consistency check, not a recipient override;
- with `B4B_ENFORCE_BENEFICIARY_COMPANY` on, `company_id` must be the
  beneficiary's company. Off (the default), a mismatch is only logged.

Answers `202`:

```json
{
  "id": "6f1c2a7e-3b4d-4e5f-8a9b-0c1d2e3f4a5b",
  "status": "B4BAccepted",
  "payload": {
    "amount": {"amount": 12.34, "currency": "EUR"},
    "currencyOfTransfer": "EUR",
    "debtorViban": {"account": "GB00SIM0000000000001", "country": "GB"},
    "creditorAccount": {"account": "GB00SIM0000000000042", "financialInstitution": "SC112233", "country": "GB"},
    "creditorName": "Southwind Coffee Ltd",
    "remittanceInformation": {"line1": "Settlement 2026-11-07"},
    "debtorReference": "B4BREF4c5d6e7f8a9b"
  }
}
```

`debtorAccount`, `sca_applied`, `company_id`, `chargeBearer` and
`requestedExecutionDate` are stored but not echoed.

Side effects: recorded in the `payments` activity log. The payment then
moves `B4BAccepted` → `B4BSanctionsPending` → `B4BSanctionsApproved` →
`B4BTMPending` → `B4BTMApproved`, one hop per `B4B_PROCESSING_DELAY`, unless
a [trap](#traps) on `payment.create` caught it, in which case the trap's
path is its walk instead (`B4B_FORCE_FAILURE_BENEFICIARY_IDS` are such
traps). Each transition fires a
[payment lifecycle callback](#payment-lifecycle-callback); reaching
`B4BTMApproved` first calls the [Banking Circle bridge](#lab-plumbing).
Persisted after every transition, and a payment caught mid-lifecycle by a
restart resumes from where it stopped; one a trap was walking does not
(see [Traps](#traps)).

### `GET /oversight/v1/payments/{id}`

The current state of a payment: the documented recovery path, because
callbacks are unsigned, unordered, may repeat and cannot be replayed.

Answers `200`:

```json
{"id": "6f1c2a7e-3b4d-4e5f-8a9b-0c1d2e3f4a5b", "status": "B4BTMApproved", "external_ref": "stl_20261107_MID0001", "payload": {"amount": {"amount": 12.34, "currency": "EUR"}, "currencyOfTransfer": "EUR", "…": "as in the create answer"}}
```

`status` is one of `B4BAccepted`, `B4BSanctionsPending`,
`B4BSanctionsApproved`, `B4BTMPending`, `B4BTMApproved`, `B4BFailed`.
`404` with `{"error":"payment not found","status":404}` for an unknown id.

### Callbacks B4B sends

B4B tells the client about screening results and payment progress by
POSTing to it. Common to all three kinds (`deliverCallback`):

- **Wire.** `POST`, `Content-Type: application/json`, plain JSON. No
  signature, no HMAC, no encryption, no auth header. The response body is
  ignored; any 2xx counts as taken. 8-second client timeout.
- **Destination.** The record's own `callback_url` if it has one
  (beneficiaries and payments only, a lab affordance), otherwise
  `B4B_CALLBACK_URL`, the client-account endpoint the real API uses. A
  payment's destination is fixed when it is created. The destination must
  pass `WEBHOOK_ALLOWLIST`, checked again at delivery. With no destination at
  all nothing is sent, and the `callbacks` log records a warning.
- **Retries.** Up to `MAX_ATTEMPTS` (4) attempts, waiting `RETRY_BACKOFF`
  (1s, 2s, 4s) between them, on any non-2xx (4xx included) or transport
  error. After the last attempt the callback is dropped and logged as
  refused. The real vendor is said to retry for about a day.
- **Ordering.** Each callback is delivered from its own goroutine, so a
  retried one can arrive after a later one. Read the current state
  (`GET /oversight/v1/payments/{id}`, `GET /oversight/v1/beneficiaries/{id}`)
  rather than trusting arrival order.

The lab's settlement stand-in receives payment callbacks at
`POST :8083/internal/b4b-webhook`.

#### Person screening callback

Sent after every `POST /oversight/v1/companies/{id}/people` (both entity
types), and after `PUT /sim/people/{id}/sanctions` when either status
actually changed. Always to `B4B_CALLBACK_URL`: a person has no
`callback_url`.

```json
{"id": "per_5a6b7c8d9e0f", "sanctions_status": "pass", "pep_sanctions_status": "pass"}
```

Nothing else: no `external_ref`, no `entity_type`, no `company_id`. A client
correlates on the id the create returned.

#### Beneficiary callback

Sent after every `POST /oversight/v1/beneficiaries`, and after
`PUT /sim/beneficiaries/{id}/sanctions` or `PUT /sim/beneficiaries/{id}/status`
when the value actually changed. To the beneficiary's `callback_url`, else
`B4B_CALLBACK_URL`.

```json
{"id": "ben_0a1b2c3d4e5f", "external_ref": "MID0001", "sanctions_status": "fail", "status": "disabled", "disabled_at": "2026-11-07T10:00:00Z"}
```

`external_ref` and `disabled_at` are omitted when empty. This is the
superset of two readings of the vendor's documentation; see the design
record.

#### Payment lifecycle callback

Sent once per transition: `B4BAccepted` (immediately on create), then
`B4BSanctionsPending`, `B4BSanctionsApproved`, `B4BTMPending`, and finally
`B4BTMApproved` or `B4BFailed`: five callbacks per payment. A
[trap](#traps) can stop a payment short or fail it earlier, and then there
are fewer; each looks exactly as it does without one. After a restart only
the transitions not yet made fire. To the payment's `callback_url`,
else `B4B_CALLBACK_URL` as it stood when the payment was created.

```json
{
  "id": "6f1c2a7e-3b4d-4e5f-8a9b-0c1d2e3f4a5b",
  "status": "B4BTMApproved",
  "payload": {"amount": {"amount": 12.34, "currency": "EUR"}, "currencyOfTransfer": "EUR", "…": "as in the create answer"},
  "banking_circle_api_response": {"paymentId": "bcp_8b9c0d1e2f3a"}
}
```

`banking_circle_api_response` appears only on `B4BTMApproved`, which is sent
after the bridge call returns. It always carries the Banking Circle
`paymentId` B4B minted, plus `"error"` when the bridge failed (for example
`"banking circle returned 400 Bad Request"`). `B4BTMApproved` is reported
even then.

## Lab plumbing

b4b registers no plumbing routes. It makes one plumbing call, the bridge
that stands in for B4B moving the money at its bank, for which no API
exists:

`POST {BANKING_CIRCLE_URL}/internal/payments` (banking-circle's plain-HTTP
`:8095` listener, no credentials), once per payment as it reaches
`B4BTMApproved`, not retried:

```json
{"paymentId": "bcp_8b9c0d1e2f3a", "accountId": "<UUID derived from beneficiary_id>", "amount": "12.34", "currency": "EUR", "externalRef": "stl_20261107_MID0001", "holder": "Southwind Coffee Ltd", "iban": "GB00SIM0000000000042"}
```

`amount` is `amount.amount` as sent; `currency` is `currencyOfTransfer`
(Banking Circle holds safeguarding accounts for EUR and GBP only).
`accountId` is `bankingcircle.AccountIDFor(beneficiary_id)`. The creditor
travels as `iban` when the creditor account is IBAN-shaped, otherwise as
`accountNumber` plus `financialInstitution`. The outcome is reported in the
`B4BTMApproved` callback, never to the caller of
`POST /oversight/v1/payments`.

## Lab control

Hooks a test, an agent or the lab API uses to drive or watch b4b. They do not
exist at the real vendor. They are on the control port, `:9086`, with no
vendor auth ([the control
contract](../README.md#the-control-contract)): the JWT is for a platform
calling B4B, not for the lab steering its simulation. The same `/sim/*`
paths still answer on `:8086` until phase 4 of
[the plan](../../lab-api/plan.md) retires them, behind the bearer JWT as
they were, but for `GET /sim/activity`, which never needed it.

### `GET /health`

Liveness, on both ports, no auth. `:8086` answers `{"status": "ok",
"service": "b4b"}`; the control port answers `{"status": "ok", "service":
"b4b", "surface": "control"}`, and that is the one the lab API's monitor
probes.

### `GET /sim/activity`

The three activity logs, newest event first. No auth on either port. Query:
`limit` (positive integer, default 100).

Answers `200`:

```json
{
  "logs": [
    {
      "name": "payments", "title": "Payouts received", "note": "Every call the platform made to the payments API, and what this vendor answered.",
      "total": 1, "kept": 1,
      "last": {"seq": 1, "at": "2026-11-07T09:40:00Z", "op": "payment.create", "peer": "10.89.0.7", "summary": "payout 12.34 EUR to MID0001 (Southwind Coffee Ltd)", "status": "ok", "detail": {"payment_id": "6f1c2a7e-…", "status": "Accepted"}},
      "events": [{"seq": 1, "…": "as last"}]
    },
    {"name": "callbacks", "title": "Callbacks sent", "total": 5, "kept": 5, "events": [{"op": "callback", "peer": "http://settlement:8083/internal/b4b-webhook", "summary": "payment 6f1c2a7e-… status=B4BAccepted delivered", "status": "ok", "detail": {"attempt": "1", "http": "200"}}]},
    {"name": "traps", "title": "Traps", "total": 0, "kept": 0, "events": []}
  ]
}
```

Event `status` is `ok`, `warn` (a 4xx answer, or a callback with nowhere to
go) or `bad` (a 5xx answer, or a callback refused after every attempt). A
refused payout's summary ends with b4b's own error message. `at` is wall
time, not the lab clock. What a trap caused is marked `detail.trap` (the
trap's id): the `payment.create` event of a payout it caught, and the
callback event of each transition it made, with `detail.reason` on a
`B4BFailed` it gave a reason for. `B4BAccepted` is b4b's own and is never
marked.

### `POST /sim/beneficiaries`

Registers a payee exactly as
[`POST /oversight/v1/beneficiaries`](#post-oversightv1beneficiaries) does,
without the JWT: the same handler, so the same body and validation, the
same answer (`201` with the beneficiary; `400`, `404` as there), the same
beneficiary callback and the same state file. The lab API registers
merchants' outlets through it, with `external_ref` set to the MID.

### `PUT /sim/beneficiaries/{id}/sanctions`

Moves a beneficiary's consolidated sanctions status, as continuous
screening would.

Body: `{"sanctions_status": "fail"}` (`pass`, `review` or `fail`).
`{id}` is a B4B id or `external_ref`; an unknown id materializes the
synthetic payee and stores it.

Answers `200` with the beneficiary. `400` for a missing or invalid status.
Fires the beneficiary callback if the status changed. Persisted.

### `PUT /sim/beneficiaries/{id}/status`

Moves a beneficiary between `active` and `disabled`. A disabled payee
screens clean and still cannot be paid.

Body: `{"status": "disabled"}`. Disabling stamps `disabled_at` from the lab
clock; re-activating clears it. `{id}` as above.

Answers `200` with the beneficiary, `400` for any other status. Fires the
beneficiary callback if the status changed. Persisted.

### `PUT /sim/companies/{id}/sanctions`

Moves a company's KYB screening status.

Body: `{"sanctions_status": "review"}` (`pass`, `review` or `fail`;
`pep_sanctions_status` is ignored). `{id}` is a B4B id or `external_ref`.

Answers `200` with the company, `400` for a missing or invalid status,
`404` for an unknown company. No callback (the real API has no company
callback), and nothing reads this status: it blocks no payment.

### `PUT /sim/people/{id}/sanctions`

Moves a person's sanctions and PEP statuses independently.

Body: `{"sanctions_status": "pass", "pep_sanctions_status": "review"}`. At
least one is required; an empty one is left alone. `{id}` is the person's
B4B id only.

Answers `200` with the person; `400` when both are empty or a value is
invalid; `404` for an unknown person. Fires the person screening callback
if either status changed. Persisted.

### Traps

One hook, `payment.create`
([traps.md §9.2](../../lab-api/traps.md#92-b4b); matching, precedence,
`times` and the lifecycle of a trap are the lab's, in
[the API overview](../README.md#traps)). It is offered every payout
`POST /oversight/v1/payments` accepts, after the gates: a payout refused
`422` is never offered. Subject: the payment id.

| Match field | |
| --- | --- |
| `beneficiary_id` | as sent on the payment: a B4B id or the payee's `external_ref` |
| `beneficiary.external_ref` | the payee's `external_ref`, the MID the lab registered it with; absent for a payee never registered |
| `company_id` | as sent |
| `debtor_viban` | `debtorViban.account`, compared as an IBAN |
| `creditor.account` | `creditorAccount.account`, an IBAN or an account number, compared upper case without spaces |
| `amount` | `amount.amount`, compared as a decimal |
| `currency` | `currencyOfTransfer`, the currency that moves |
| `external_ref` | the client's reference for the payout |

```text
B4BAccepted ─▶ B4BSanctionsPending ─▶ B4BSanctionsApproved ─▶ B4BTMPending ─▶ B4BTMApproved ─▶ (bridge to BC)
 (automatic)        │                        │                      │
                    └────────────────────────┴──────────────────────┴──▶ B4BFailed
```

- **`B4BAccepted` is automatic.** The create answers `202` and calls back
  as always; the path starts after it. A payment a trap caught never takes
  the default walk: the path is its walk.
- **A step may name a state further along** (`"walks": true` in the
  catalogue). b4b walks the states in between and calls back on each, as
  the default walk does. A step's first hop waits the step's own `after`
  or `after_lab`, or `B4B_PROCESSING_DELAY` when it names neither
  (`first_step_default_after`); every hop after it waits
  `B4B_PROCESSING_DELAY`. From `B4BAccepted`, `B4BFailed` is reached through
  `B4BSanctionsPending`.
- **Each hop goes through b4b's own engine**, exactly as a hop of the
  default walk: the state, the status read, the callback, the state file,
  and at `B4BTMApproved` the bridge call into Banking Circle, where a
  `payment.outgoing` trap can take it.
- **`B4BFailed` takes `reason`.** It is kept on the payment in the state
  file (`failure_reason`) and in the `B4BFailed` callback's activity
  (`detail.reason`). The callback and `GET /oversight/v1/payments/{id}`
  carry no reason, as without a trap.
- **A path may stop short.** If its last step is not `B4BTMApproved` or
  `B4BFailed`, the payment stays there for good; `[]` leaves it
  `B4BAccepted`.
- **The trap schedules every hop.** `pending_steps` names the next hop and
  when it is due, and `caught[].applied` lists each state the payment
  reached. Disarming the trap, or a reset, stops the walk at the next hop;
  a payment so stopped stays where it is.

Hang at TM review:

```json
{"hook": "payment.create", "match": {"beneficiary.external_ref": "MID0001"},
 "effect": {"path": [{"to": "B4BTMPending"}]}}
```

Fail at sanctions, five seconds after reaching it:

```json
{"hook": "payment.create", "match": {"beneficiary_id": "MID0001"},
 "effect": {"path": [{"to": "B4BSanctionsPending"},
                     {"to": "B4BFailed", "after": "5s", "reason": "sanctions hit"}]}}
```

The default, with no trap, is the walk to `B4BTMApproved`, then the
bridge. Sanctions on a beneficiary, company or person stay setters
(`PUT /sim/…/sanctions` above): the gates then refuse the payment by their
own rule, `422` at create.

#### Standing traps

Each id in `B4B_FORCE_FAILURE_BENEFICIARY_IDS` is armed at start as a
standing trap: `"label": "B4B_FORCE_FAILURE_BENEFICIARY_IDS <id>"`,
`"match": {"beneficiary_id": "<id>"}`, `"times": null`, path
`[{"to": "B4BTMPending"}, {"to": "B4BFailed"}]`. That is what the variable
always did: five callbacks, `B4BFailed` where `B4BTMApproved` would have
been, and no bridge call. They are listed with `"standing": true`, armed
before any other, so they take a payment first. A reset arms them again,
with new ids and nothing caught; disarming one lasts until the next reset.

#### Restarts

Traps are in memory, the standing ones apart. A payment a trap was walking
when b4b stopped is not walked the default way after a restart: a standing
trap that matches it takes it on from where it stopped (only the
transitions not yet made call back, as for any resumed payment), and any
other stays where it stopped, readable, for good. The log says which.

### `GET /sim/traps/hooks`

The catalogue: the `payment.create` hook with its match fields, its graph
and `first_step_default_after`, which is `B4B_PROCESSING_DELAY`.

```json
{"service": "b4b",
 "hooks": [{"hook": "payment.create",
            "about": "A payout accepted by POST /oversight/v1/payments, once it cleared the gates. …",
            "subject": "payment_id",
            "match": [{"field": "beneficiary_id", "type": "string", "about": "as sent on the payment: …"},
                      {"field": "debtor_viban", "type": "iban", "about": "debtorViban.account"},
                      {"field": "amount", "type": "decimal", "about": "amount.amount"},
                      {"…": "every field in the table above"}],
            "effect": "path",
            "graph": {"start": "B4BAccepted",
                      "next": {"B4BAccepted": ["B4BSanctionsPending"],
                               "B4BSanctionsPending": ["B4BSanctionsApproved", "B4BFailed"],
                               "B4BSanctionsApproved": ["B4BTMPending", "B4BFailed"],
                               "B4BTMPending": ["B4BTMApproved", "B4BFailed"]},
                      "params": {"B4BFailed": ["reason"]},
                      "walks": true},
            "first_step_default_after": "300ms"}]}
```

### `GET /sim/traps`

The traps, newest first, standing ones included. Query: `state` (`armed`,
`spent`, `expired` or `disarmed`) filters; `400` for anything else.

Answers `200` with `{"service": "b4b", "traps": [<trap>, …]}`.

### `POST /sim/traps`

Arms a trap. Body: `hook` (`payment.create`), `match` (`{}` takes every
payout), `effect` (`{"path": […]}`), and optionally `label`, `times` (`1`
when left out, `null` for every payout until disarmed) and `expires_at`. An
unknown field is refused.

Answers `201` with the trap:

```json
{"id": "trp_0002", "label": "MID0001 hangs at TM review", "hook": "payment.create",
 "match": {"beneficiary.external_ref": "MID0001"}, "times": 1, "expires_at": null,
 "effect": {"path": [{"to": "B4BTMPending"}]},
 "state": "armed", "created_at": "2026-11-07T09:00:00Z", "caught": [], "pending_steps": []}
```

`400` with the reason: a hook b4b does not have, a match field it does not
know (the answer lists the fields), a step that cannot follow the one
before it (`B4BFailed cannot follow B4BTMApproved (B4BTMApproved is final)`),
a parameter a state does not take (`B4BTMPending takes no parameters, not
reason`), both `after` and `after_lab` on one step, or a wait that is not a
duration (`10s`, `2h`, `1d`).

### `GET /sim/traps/{id}`

One trap. After it caught a payment and took two hops of three:

```json
{"id": "trp_0002", "…": "as armed", "state": "spent",
 "caught": [{"subject": "6f1c2a7e-3b4d-4e5f-8a9b-0c1d2e3f4a5b", "at": "2026-11-07T09:40:00Z",
             "applied": [{"to": "B4BSanctionsPending", "at": "2026-11-07T09:40:00.3Z"},
                         {"to": "B4BSanctionsApproved", "at": "2026-11-07T09:40:00.6Z"}]}],
 "pending_steps": [{"trap_id": "trp_0002", "subject": "6f1c2a7e-3b4d-4e5f-8a9b-0c1d2e3f4a5b",
                    "step": {"to": "B4BTMPending", "after": "300ms"}, "due": "2026-11-07T09:40:00.9Z"}]}
```

`caught[].cancelled` is `true` when a disarm or a reset stopped the walk.
`404` for an unknown id.

### `DELETE /sim/traps/{id}`

Disarms one trap and cancels its pending hop; the payment it was walking
stays where it is. Answers `200` with `{"trap": <trap>, "cancelled":
[<pending step>, …]}`, `404` for an unknown id.

### `DELETE /sim/traps`

Disarms every trap still armed or with a hop pending, standing ones
included until the next reset. Answers `200` with `{"service": "b4b",
"disarmed": 2, "cancelled": [<pending step>, …]}`.

### `GET /sim/info`

Who this is and what it can do, as every service answers it
([the control contract](../README.md#the-control-contract)):
`"service": "b4b"`, `"kind": "vendor"`, `"vendor_listeners": [":8086"]`,
`"control_listener": ":9086"`, `"reset_scopes": ["flow", "all"]`,
`"hooks": ["payment.create"]`, and
`"traps": {"armed": 1, "pending_steps": 0}`.

### `GET /sim/logs`

The process's own log lines, as every service answers it
([the control contract](../README.md#the-control-contract)). Every
request on either port but these reads and the health probes is a line, and
so is every callback attempt, every bridge call, every piece of work a
reset dropped, and every trap armed, catch and hop.

### `POST /sim/reset`

Puts b4b back to a known state.

- **`flow`** clears every payment (`payments`; `payments_in_flight` of them
  had not reached a final state), the callbacks and bridge calls in
  flight (`callbacks_in_flight`), the `payments` and `callbacks` activity
  logs (`activity_events`), and every trap but the standing ones (`traps`),
  cancelling the hops they had scheduled (`trap_steps`) and emptying the
  `traps` log. It keeps the boarding: companies, addresses, people,
  extended profiles, documents, vIBANs, and beneficiaries with their
  sanctions and status.
- **`all`** clears the boarding too, counted per kind: the state file as on
  first start.
- **Keeps**, either way, the JWT keys and
  `B4B_FORCE_FAILURE_BENEFICIARY_IDS`: they are configuration. Its standing
  traps are armed again, afresh.
- Work in flight is dropped, not raced. A payment still sleeping out
  `B4B_PROCESSING_DELAY`, or a trap's hop still waiting, takes no further
  step, and no callback, retry or bridge call begun before the reset is
  made after it — boarding callbacks
  included, even for a person or beneficiary a `flow` reset keeps; its
  status is still readable. The reset waits for a call already on the
  wire, so when it answers nothing it dropped is still being sent.
- The state file is written before it answers, so a restart neither brings
  back nor resumes a payment the reset dropped. `500` if the file cannot
  be written; the reset has then happened in memory only.

```json
{"service": "b4b", "scope": "flow",
 "cleared": {"payments": 3, "payments_in_flight": 1, "callbacks_in_flight": 1, "activity_events": 14, "traps": 1, "trap_steps": 1},
 "kept": ["companies", "addresses", "people", "extended_profiles", "documents", "vibans", "beneficiaries", "jwt_keys", "force_failure_beneficiary_ids"],
 "at": "2026-11-07T12:00:00Z"}
```

## Called by

- **settlement** (scaffolding, `cmd/settlement`), per settled record that
  clears its funding and verification gates: `GET /oversight/v1/beneficiaries/{id}`
  with the MID, then `POST /oversight/v1/payments` with creditor fields from
  that record and `callback_url` set to its own `B4B_CALLBACK_URL`
  (`http://settlement:8083/internal/b4b-webhook`). Signs with
  `b4b.SignBearerToken` and the shared `private.pem`.
- **lab API** (`cmd/labapi`), on the control port only (`http://b4b:9086`),
  with no token:
  `POST /v1/merchants/{id}/provision` and `POST /v1/merchants/batches/{id}/provision`
  call `POST /sim/beneficiaries` once per outlet, with `external_ref` set
  to the MID;
  `POST /v1/outlets/{mid}/sanctions` calls `PUT /sim/beneficiaries/{mid}/sanctions`;
  `GET /v1/lab` and `POST /v1/services/b4b/probe` probe `GET /health`;
  `GET /v1/services/b4b/activity`, `GET /v1/flow` and `GET /v1/flow/hops/{id}`
  read `GET /sim/activity`; `POST /v1/lab/reset` and
  `POST /v1/services/b4b/reset` call `POST /sim/reset`, and `GET /v1/logs`
  and `GET /v1/services/b4b/logs` read `GET /sim/logs`.
  `GET /v1/traps`, `GET /v1/traps/hooks` and `DELETE /v1/traps` call
  `GET /sim/traps`, `GET /sim/traps/hooks` and `DELETE /sim/traps`; a trap
  is armed through `/v1/services/b4b/sim/traps`, which passes every
  `/sim/*` route through.
  `GET /v1/connect/env/b4b` and `GET /v1/connect/files/b4b/{name}` hand
  out the signing key without calling b4b; the lab API signs nothing.
- **e2e suite** (`internal/e2e`): `b4b-oversight-gates`
  (`scenario_b4b.go`) registers a beneficiary, sends mismatched and matching
  payments, polls `GET /oversight/v1/payments/{id}`, blocks the payee with
  `PUT /sim/beneficiaries/{id}/sanctions` through the lab API, and re-reads
  it;
  `b4b-company-boarding` (`scenario_b4b_boarding.go`) walks companies,
  addresses, people, uploads, the extended profile (and its read), vIBANs
  and beneficiaries, asserting each refusal; `banking-circle-payout`
  (`scenario_bankingcircle.go`) reaches b4b only through settlement;
  `banking-circle-trap-paths` and `banking-circle-reset-drops-a-payout-in-flight`
  (`scenario_traps.go`) register a beneficiary and pay it, so that b4b
  hands Banking Circle the payout a trap is waiting for.
- **The platform** under test: its own B4B client, configured with
  `B4B_API_BASE_URL`, `B4B_JWT_KEY_ID` and `B4B_JWT_PRIVATE_KEY`; it receives
  callbacks wherever b4b's `B4B_CALLBACK_URL` points (compose carries a
  commented example, `http://host.containers.internal:3000/api/v1/b4b/callback`).

b4b itself calls banking-circle (the bridge) and whoever the callbacks are
addressed to; nothing else.

Every route above is held to the source by `internal/console/apidocs_test.go`: a route added, moved or removed without this page following fails `go test ./internal/console`.
