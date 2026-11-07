# Open questions

Answer in place: write the decision under a question, or strike the
question. Each has the proposal the draft currently assumes.

1. **What does `flow` reset keep, per sim?** The table in
   [sim-control.md §6](sim-control.md#6-post-simreset).
   *Proposal:* keep what a platform set up once (Banking Circle
   subscriptions, B4B boarding, Worldline enrolment), and clear what a run
   made.

2. **Are the control ports published to the host?**
   [architecture.md §4](architecture.md#4-the-control-listener).
   *Proposal:* published on `127.0.0.1` in compose (useful for debugging a
   sim on its own); not exposed in a deployed lab, where only `lab-api` is.

3. **Who serves the console page?** A tiny `console` container that also
   proxies `/v1`, or `lab-api` at `/`?
   [architecture.md §7](architecture.md#7-the-console).
   *Proposal:* the separate container, so `lab-api` stays headless and can
   run without a UI (CI, agents).

4. **How does a person's browser get into a deployed lab?** With a token
   in the page, or through an auth proxy in front (oauth2-proxy,
   Cloudflare Access)? [architecture.md §8](architecture.md#8-auth).
   *Proposal:* an auth proxy; the page itself stays token-free.

5. **A trap's default `times`: `1` or until disarmed?**
   [traps.md §2](traps.md#2-the-trap-object).
   *Proposal:* `1`. A forgotten trap then catches one event, not every one
   after it.

6. **Is `after` real time or lab time by default?**
   [traps.md §5](traps.md#5-time).
   *Proposal:* real time, with `after_lab` for lab time.

7. **When the books cannot follow a trapped step** (processed with too
   little on the safeguarding account): does the vendor's own rule win, or
   does the trap force it? [traps.md §4.1](traps.md#41-path-a-state-machines-next-states).
   *Proposal:* the vendor's rule wins, and the trap logs `trap.diverted`.

8. **Can Banking Circle's `MissingFunding` later become
   `OutgoingPaymentProcessed`** once money arrives? Today the lab never
   does it. [traps.md §9.1](traps.md#91-banking-circle). Needs the vendor
   docs.

9. **Does disarming a trap cancel its pending steps?**
   [traps.md §6](traps.md#6-lifecycle).
   *Proposal:* yes. After `DELETE`, nothing more happens because of that
   trap.

10. **Composite calls in `lab-api`?** For example "prepare a business
    morning" as one call. [lab-api.md §13](lab-api.md#13-not-in-v1).
    *Proposal:* none in v1. Clients make the calls.

11. **Plugin `reset_path` and `logs_path`**, so the lab's reset and logs
    take in the platform's side. [runner.md §3](runner.md#3-the-plugin-contract).
    *Proposal:* yes, both optional, in phase 2.

12. **Does reset clear a sim's log ring?**
    [sim-control.md §6](sim-control.md#6-post-simreset).
    *Proposal:* no. It writes one line marking the reset.

13. **`all` reset drops Banking Circle's issued tokens.** Does the platform
    re-authenticate on a `401`, or does that need a runner restart?
    [sim-control.md §6](sim-control.md#6-post-simreset). Needs a look at
    buddy's BC client.

14. **Ports**: `lab-api` on `:8097`, and each control listener on its vendor
    port plus 1000. [architecture.md §1](architecture.md#1-what-runs).

15. **Traps beyond picking an outcome**: `report.read` (a report running
    behind) and `http.request` (errors and timeouts on the vendor surface).
    In scope at all, and in phase 3? [traps.md §9](traps.md#9-hooks-by-vendor).

16. **`GET /sim/state`**: a dump of a sim's whole state for test
    assertions, beyond the list routes each sim has today. Wanted?
