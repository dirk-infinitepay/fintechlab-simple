# Pointing the verification vendors at this lab

One process serves all four checks, each under its own path prefix. They
share nothing but the address — no state, and no vendor knows another
exists — so four env vars point at one container.

```dotenv
# Creditsafe — company data and UK bank verification
CREDITSAFE_API_URL=http://localhost:8091/creditsafe
CREDITSAFE_USERNAME=sim
CREDITSAFE_PASSWORD=sim-verification-dev-only

# iban.com — the base URL *is* the endpoint for this vendor
IBAN_COM_BASE_URL=http://localhost:8094/iban/verify
IBAN_COM_API_KEY=sim-verification-dev-only

# KYC6 — sanctions, PEP and adverse media
KYC6_API_BASE_URL=http://localhost:8092/kyc6
KYC6_MONITOR_API_BASE_URL=http://localhost:8092/kyc6
KYC6_API_KEY=sim-verification-dev-only

# LexisNexis — OAuth, then identity (IDU) and verification (IVI)
LEXISNEXIS_AUTH_URL=http://localhost:8093/lexisnexis
LEXISNEXIS_IDU_BASE_URL=http://localhost:8093/lexisnexis/idu
LEXISNEXIS_IVI_BASE_URL=http://localhost:8093/lexisnexis/ivi
LEXISNEXIS_CLIENT_ID=sim
LEXISNEXIS_CLIENT_SECRET=sim-verification-dev-only
LEXISNEXIS_IVI_API_KEY=sim-verification-dev-only
```

Each vendor is its own sim: Creditsafe on `:8091`, KYC6 on `:8092`,
LexisNexis on `:8093` and iban.com on `:8094`. Inside compose use the
service name instead of `localhost`: `creditsafe:8091`, `kyc6:8092`,
`lexisnexis:8093`, `iban-com:8094`.

## What is faked and what is not

The contract is real: the paths, the auth handshakes, the request bodies
and the response shapes are what the platform's own clients send and parse.
Tokens are issued and checked — a client that never authenticates, or sends
one this service did not issue, gets a 401 here rather than discovering it
against the real vendor.

The judgement is stubbed. Every check passes.

## Making one fail

Each sim has its own switches, on its own port, so a flip at one vendor
never moves another's answers:

```bash
curl -s localhost:8092/sim/outcome                       # what KYC6 answers now
curl -s -XPOST localhost:8092/sim/outcome \
  -H 'content-type: application/json' \
  -d '{"check":"screening","outcome":"hit"}'
```

| sim | check | outcomes | moves |
| --- | --- | --- | --- |
| creditsafe `:8091` | `bank-account` | `pass` · `mismatch` · `invalid` | `supplierResponse.result` / `nameMatchResult` / `accountTypeResult` |
| creditsafe `:8091` | `company` | `active` · `dissolved` | the company report's status |
| iban-com `:8094` | `bank-account` | `pass` · `mismatch` · `invalid` | `result.valid` / `name_match` |
| kyc6 `:8092` | `screening` | `clear` · `hit` | `results.matchCount` and the matches array |
| lexisnexis `:8093` | `identity` | `pass` · `refer` · `fail` | IDU `assessment.result`, and IVI's review status |

An unknown check or outcome is a 400, not a silent no-op: a typo that
changed nothing would leave you believing you had armed a failure that
never fires.

## Two things worth knowing

**`mismatch` is not `invalid`.** A mismatch is a real account in the wrong
name — `valid: true` with `name_match: no` — and a client that treats
the two the same will pass here and be wrong about a whole class of payment
in production.

**Creditsafe's monitoring poll answers with nothing.** An event this lab
invented would send the platform chasing a company that did not change.
When monitoring needs exercising, that endpoint is where to add a seam.
