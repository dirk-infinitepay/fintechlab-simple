# Pointing the verification vendors at this lab

One process serves all four checks, each under its own path prefix. They
share nothing but the address — no state, and no vendor knows another
exists — so four env vars point at one container.

```dotenv
# Creditsafe — company data and UK bank verification
CREDITSAFE_API_URL=http://localhost:8089/creditsafe
CREDITSAFE_USERNAME=sim
CREDITSAFE_PASSWORD=sim-verification-dev-only

# iban.com — the base URL *is* the endpoint for this vendor
IBAN_COM_BASE_URL=http://localhost:8089/iban/verify
IBAN_COM_API_KEY=sim-verification-dev-only

# KYC6 — sanctions, PEP and adverse media
KYC6_API_BASE_URL=http://localhost:8089/kyc6
KYC6_MONITOR_API_BASE_URL=http://localhost:8089/kyc6
KYC6_API_KEY=sim-verification-dev-only

# LexisNexis — OAuth, then identity (IDU) and verification (IVI)
LEXISNEXIS_AUTH_URL=http://localhost:8089/lexisnexis
LEXISNEXIS_IDU_BASE_URL=http://localhost:8089/lexisnexis/idu
LEXISNEXIS_IVI_BASE_URL=http://localhost:8089/lexisnexis/ivi
LEXISNEXIS_CLIENT_ID=sim
LEXISNEXIS_CLIENT_SECRET=sim-verification-dev-only
LEXISNEXIS_IVI_API_KEY=sim-verification-dev-only
```

Inside compose use `verification:8089` instead of `localhost:8089`.

## What is faked and what is not

The contract is real: the paths, the auth handshakes, the request bodies
and the response shapes are what the platform's own clients send and parse.
Tokens are issued and checked — a client that never authenticates, or sends
one this service did not issue, gets a 401 here rather than discovering it
against the real vendor.

The judgement is stubbed. Every check passes.

## Making one fail

```bash
curl -s localhost:8089/sim/outcome                       # what each answers now
curl -s -XPOST localhost:8089/sim/outcome \
  -H 'content-type: application/json' \
  -d '{"check":"screening","outcome":"hit"}'
```

| check | outcomes | moves |
| --- | --- | --- |
| `bank-account` | `pass` · `mismatch` · `invalid` | Creditsafe `supplierResponse.result` / `nameMatchResult`, iban.com `result.valid` / `name_match` |
| `screening` | `clear` · `hit` | KYC6 `results.matchCount` and the matches array |
| `identity` | `pass` · `refer` · `fail` | LexisNexis `assessment.result`, and IVI's review status |
| `company` | `active` · `dissolved` | Creditsafe company status |

An unknown check or outcome is a 400, not a silent no-op: a typo that
changed nothing would leave you believing you had armed a failure that
never fires.

## Two things worth knowing

**`mismatch` is not `invalid`.** A mismatch is a real account in the wrong
name — `valid: true` with `name_match: NOMATCH` — and a client that treats
the two the same will pass here and be wrong about a whole class of payment
in production.

**Creditsafe's monitoring poll answers with nothing.** An event this lab
invented would send the platform chasing a company that did not change.
When monitoring needs exercising, that endpoint is where to add a seam.
