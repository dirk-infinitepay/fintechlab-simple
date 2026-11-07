# The runner and the plugin contract

`fintechlab-runner` (`@fintechlab/runner`) is the library a Node platform
uses to plug into the lab: register a card, follow the clock, serve
activity logs. This is what changes in it. The contract it implements,
[plugins.md](../plugins.md), changes first, as its README asks.

## 1. What stays

**[PROPOSED]**

- Registration, renewal every `renew_seconds`, unregistering on shutdown,
  the descriptor, actions, the settlement files table, stand-in wishes:
  same shapes, new path (`/v1/plugins`).
- The clock follower, the shim and holds: same behaviour.
- The platform's own control API (run, sweep, seed) stays the platform's.
  The lab still reaches it only by forwarding the buttons the plugin
  declared.
- No runtime dependencies. Everything below is `fetch`.

## 2. One address and a token

**[PROPOSED]**

| `fintechlab.json` | Environment | Default | Replaces |
| --- | --- | --- | --- |
| `lab_url` | `FINTECH_SIM_LAB_URL` | `http://127.0.0.1:8097` | `console_url` and `clock_url` |
| `lab_token` | `FINTECH_SIM_LAB_TOKEN` | none | — |

- `LabPlugin` registers at `{lab_url}/v1/plugins`. `LabClockFollower`
  follows `{lab_url}/v1/clock` and holds through `{lab_url}/v1/clock/holds`.
  A lab somewhere else is then one URL and one token in the platform's
  `.env`, not two URLs.
- With a token set, every call carries `Authorization: Bearer …`.
- `advertised_url` and `browse_url` are unchanged: they are about reaching
  the platform, not the lab.

## 3. The plugin contract

**[OPEN 11]** Two optional descriptor fields, so the lab's reset and logs
can include the platform's side when the platform offers it:

| Field | Called by | Contract |
| --- | --- | --- |
| `reset_path` | `POST /v1/lab/reset`, after the sims | `POST base_url+reset_path {"scope": "flow" \| "all"}` → `200 {"cleared": {…}}`. What it clears (the settle DB's runs and payouts, say) is the platform's business. `409` with a reason when it cannot right now (a run in flight) |
| `logs_path` | `GET /v1/logs` | `GET base_url+logs_path?since=&limit=&q=`, answering in the sims' log shape ([sim-control.md §5](sim-control.md#5-get-simlogs)), so the platform's lines merge with the vendors' |

The library would add a `LogRing` (the same ring the Go sims use, fed from
the `Supervisor`'s prefixed child output) and a `logsHandler`, as it has
`ActivityLog` and `activityBody` today. It adds no reset handler: what a
reset means is the platform's decision.

## 4. A client for tests and agents

**[PROPOSED]**

`LabClient`: a typed `/v1` client, so a platform's tests call the lab
without hand-writing URLs.

```ts
const lab = new LabClient({ url: process.env.FINTECH_SIM_LAB_URL, token: process.env.FINTECH_SIM_LAB_TOKEN });

await lab.reset({ scope: 'flow' });
await lab.clock.move({ at: '2026-09-25T09:00:00Z' });

const trap = await lab.traps.arm('banking-circle', {
    hook: 'payment.outgoing',
    match: { 'creditor.iban': 'GB00SIM0000000000777' },
    effect: { path: [{ to: 'OutgoingPaymentProcessed' }, { to: 'Reversed', after: '10s' }] },
});

// … run the settlement through the platform's own API …

await lab.traps.waitFor('banking-circle', trap.id, { steps: 2, timeoutMs: 60_000 });
const lines = await lab.logs({ services: ['banking-circle'], q: trap.id });
```

- The types are generated from `openapi.yaml` at build time
  (openapi-typescript, a dev dependency, types only) and checked in. There
  is no runtime dependency.
- `waitFor` uses the library's own `pollFor`. The SSE streams come later
  (`lab.events()`, phase 2), read with `fetch`'s body stream, because Node
  20 has no `EventSource`.

## 5. Compatibility

**[PROPOSED]**

- While the console's `/api/*` proxy exists
  ([phase 4](plan.md#phase-4--retire-the-aliases)), a runner that has not
  been updated keeps working against a new lab.
- An updated runner pointed at an old lab (no `/v1`: `GET {lab_url}/v1`
  answers `404`) falls back to `console_url` and `clock_url`, and logs one
  line saying so.
