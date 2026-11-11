# Design system — operator consoles

This is the design record for the look and feel of
[the console](console.md), written so a *different* app in this product
family comes out looking and behaving like it without anyone having to
reverse-engineer `cmd/console/web/app.css`.

**This document is the source of truth.** `cmd/console/web/app.css` is one
instance of it, not the definition. If the two disagree, the document wins
and the stylesheet is the thing that is wrong.

## How to use it

Three ways, all supported on purpose:

1. **Implicitly.** The repo's `CLAUDE.md` points an agent here whenever it
   touches a UI. Nothing to say in the prompt.
2. **Explicitly.** "Build X, follow `docs/design-system.md`." Use this for
   an app in another repo — copy this file across with it.
3. **By editing.** Change a token, a rule or a "don't" below, and the next
   thing built picks the change up. That is the intended way to evolve the
   system: edit the document, then bring the implementations to it.

If you only read one section, read [Colour law](#colour-law) and
[Behaviour contracts](#behaviour-contracts) — those two are what make an
app *feel* like this one. The tokens only make it *look* like it.

---

## The brief in one paragraph

An **operator console for a system that handles money**: dense, dark by
default, monospace wherever a value is something you might paste into a
terminal, and legible at a glance from two metres away during a demo. It
should read as a control panel someone technical built for themselves —
not as a marketing site, not as an enterprise dashboard product. Every
surface tells the truth about what it knows, including when it knows
nothing. Purple is the identity; green and red are *data*, never
decoration.

## Non-negotiables

These constraints produced the look. Drop one and the result stops
belonging to this family.

| Constraint | Why |
| --- | --- |
| **No build step, no bundler, no framework** | One HTML, one CSS, one JS file, served as written. The app must survive `git clone` and nothing else. |
| **No CDN, no external host, no web font** | These apps run on a laptop with no network. A `fonts.googleapis.com` link is the first thing to break, and it breaks silently — a page that renders in a fallback font on the day of the demo. Assert it in a test (see below). |
| **Ships inside the binary** | `//go:embed web`. One artefact, no static-file deployment step. |
| **Dark first, light behind a toggle** | Both are first-class; neither is a filter over the other. |
| **Status colour is reserved for status** | Nothing decorative is ever green or red. |
| **The UI is a client, never a back door** | Every button is exactly one call to a public API a human could make with `curl` — in this lab, the lab API every script and agent calls too. No state exists that is only reachable by clicking. |

The no-external-host rule is enforceable, and should be enforced. The
console's test:

```go
external := regexp.MustCompile(`(?i)(https?:)?//(?:cdn|unpkg|fonts\.|ajax\.|code\.jquery)`)
```

served against every embedded asset. Copy it into any new app.

---

## Tokens

Canonical. Copy verbatim; do not re-pick these by eye.

```css
:root {
  /* surfaces, darkest to lightest — four steps, no more */
  --bg: #0e0c14;          /* the page */
  --panel: #16131f;       /* cards, sidebar, modal */
  --panel-2: #1c1828;     /* inputs, ghost buttons, toasts */
  --panel-3: #221d30;     /* badges, hover, scrollbar thumb */
  --border: #2c2540;      /* card and control edges */
  --border-soft: #241f33; /* row separators, internal rules */

  /* text, three weights of presence */
  --text: #ece9f5;        /* anything you must read */
  --text-dim: #a49bba;    /* supporting prose, secondary values */
  --text-faint: #837a99;  /* labels, units, metadata */

  /* identity */
  --accent: #a855f7;
  --accent-hi: #c084fc;
  --accent-lo: #7c3aed;
  --accent-ghost: rgba(168, 85, 247, 0.14);   /* tinted fills */
  --accent-line: rgba(168, 85, 247, 0.35);    /* hover/active edges */
  --accent-btn-hi: #9333ea;  /* primary button gradient, light stop */
  --accent-btn-lo: #7c3aed;  /* primary button gradient, dark stop */

  /* status — data only */
  --up: #4ade80;
  --down: #fb7185;
  --warn: #fbbf24;
  --unknown: #8b8299;

  --radius: 12px;
  --radius-sm: 8px;
  --mono: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace;
  --sans: ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
}

:root[data-theme="light"] {
  --bg: #f7f5fb;
  --panel: #ffffff;
  --panel-2: #f4f1fa;
  --panel-3: #ece7f7;
  --border: #ded6ee;
  --border-soft: #e9e3f5;
  --text: #1d1730;
  --text-dim: #5d5478;
  --text-faint: #767089;
  --accent: #7c3aed;
  --accent-hi: #9333ea;
  --accent-lo: #6d28d9;
  --accent-ghost: rgba(124, 58, 237, 0.10);
  --accent-line: rgba(124, 58, 237, 0.28);
  --accent-btn-hi: #9333ea;
  --accent-btn-lo: #6d28d9;
  --up: #15803d;
  --down: #c81e1e;
  --warn: #b45309;
  --unknown: #6f6885;
}
```

Notes on the values, because they are not arbitrary:

- **Four surface steps, and they are close together.** `--bg` → `--panel`
  is an 8-point lift, not a 30-point one. Density comes from many quiet
  edges, not from high-contrast slabs.
- **Purple is warm-shifted into the greys.** `--bg` is `#0e0c14`, not
  `#0e0e0e`. Every neutral in the dark palette carries a little violet, and
  that is what stops the accent looking stuck on.
- **`--accent` is the identity, `--accent-hi`/`--accent-lo` are the
  gradient.** Gradients run `140deg`, high → low. Never the reverse.
- **`--accent-btn-*` exists separately from `--accent-hi`/`--accent-lo`**
  because white text on `#c084fc` is 2.6:1 — a filled button using the raw
  identity gradient is unreadable at its light end. See
  [Contrast](#contrast-the-measured-floor).
- **`--unknown` is a grey, deliberately.** "I have not checked" must not
  look like "it is fine" or "it is broken".

### Theme

Dark is the default; light is a real theme, not a filter. Respect the OS
on first paint and let an explicit choice win afterwards:

```html
<html lang="en" data-theme="dark">
```

```js
try {
  const saved = localStorage.getItem('<app>-theme');
  if (saved) document.documentElement.dataset.theme = saved;
  else if (matchMedia('(prefers-color-scheme: light)').matches)
    document.documentElement.dataset.theme = 'light';
} catch (_) { /* private window: the default stands */ }
```

Every `localStorage` read and write is wrapped — a private window throws
on access, and a UI that white-screens because it could not remember a
theme preference is a UI nobody trusts with a payout button.

### Type

One sans stack for prose, one mono stack for values. No third family, no
web font.

| Use | Size | Notes |
| --- | --- | --- |
| Body | 14px / 1.5 | `html, body` |
| View title (`h1`) | 19px, 600, `-0.2px` | one per screen |
| Card title | 13.5px, 600 | |
| Body copy in cards, tables | 12.5px | |
| Section heading | 12px, 600, uppercase, `.08em` | always `--text-faint` |
| Field label | 10.5px, uppercase, `.06em` | always `--text-faint` |
| Pill | 10.5px mono, uppercase, `.06em` | |
| Mono value | 11.5px | one notch smaller than the prose beside it |
| Stat number | 21px, 600, `tabular-nums` | |

**Mono is not styling, it is a claim.** A value is monospace if and only if
it is a machine identifier a human might copy: an id, a MID, an IBAN, a
URL, a path, an amount, a duration, an env var name. A *legal name* is
sans. A *description* is sans. This is why the console overrides
`style="font-family:var(--sans)"` on some `<dd>` elements inside a `.kv`
whose default is mono — the exception proves the rule is being applied.

Everything numeric that sits in a column gets
`font-variant-numeric: tabular-nums`.

### Space, radius, motion

- Rhythm is **10 / 12 / 16 / 20 / 26px**. Content padding is `20px 26px`,
  card padding `13px 15px`, gaps `8–12px`.
- Radius: `12px` for cards and stats, `8px` for controls and inputs,
  `14px` for modals, `5px` for pills, `20px` for badges.
- Motion is **120ms** for state (`background`, `color`, `border-color`),
  **150ms** for the disclosure chevron, **180ms** for a toast entering.
  Nothing animates longer than 180ms; nothing eases in on load. Respect
  `prefers-reduced-motion`:

```css
@media (prefers-reduced-motion: reduce) {
  * { animation-duration: .01ms !important; transition-duration: .01ms !important; }
}
```

---

## Colour law

The rule that carries most of the identity:

> **Purple is decoration. Green, red and amber are data. A colour never
> crosses that line.**

Concretely:

- A primary button is purple. A "success" button is *still purple* — the
  success is reported afterwards, by a toast whose left border is green.
- A selected nav item is purple. An active tab is purple.
- Green appears only as: a health dot, an `ok` pill, a `.stat.up` number, a
  sparkline bar, a good toast's border. Same for red, inverted.
- Amber (`--warn`) means *degraded or caveated*, not *severe*. A
  `.note.warn` says "this vendor has no such endpoint" — it is not an
  error, and must not be red.
- Grey (`--unknown`) means *not yet known*. Reach for it more often than
  feels natural. **Never report unknown as bad.** A status page that cries
  wolf on start-up is a status page people learn to ignore.

Status tints are always derived, never hand-picked:

```css
.pill.ok   { color: var(--up);   border-color: color-mix(in srgb, var(--up)   35%, transparent); }
.note.bad  { border-left-color: var(--down); background: color-mix(in srgb, var(--down) 9%, transparent); }
.dot.up    { background: var(--up); box-shadow: 0 0 0 3px color-mix(in srgb, var(--up) 18%, transparent); }
```

Three derivation strengths, and only three: **9%** for a note's wash, **18%**
for a dot's halo, **35%** for a border.

### Contrast: the measured floor

Measured against `--panel`, WCAG 2.1:

| Pair | Dark | Light |
| --- | --- | --- |
| `--text` | 15.3 | 17.3 |
| `--text-dim` | 7.0 | 7.0 |
| `--text-faint` | **4.5** | **4.7** |
| `--accent` | 4.6 | 5.7 |
| `--up` | 10.5 | 5.0 |
| `--down` | 6.8 | 5.7 |
| `--warn` | 11.0 | 5.0 |
| white on primary button | 5.4–5.7 | 5.4–7.1 |

Floor: **4.5:1 for anything a user has to read**, including 10.5px
uppercase labels — small caps are the *hardest* text on the page, so they
do not get the large-text exemption. Three of the values in the token block
above exist because the console's originals missed that floor:
`--text-faint` (was `#6f6685`, 3.4:1), light `--up` (was `#16a34a`, 3.3:1
on white, and `.pill.ok` renders it at 10.5px), and the primary-button
gradient. Do not revert them.

Colour is never the only channel. A health dot carries a shape difference
too (`not-probed` is a dashed ring, not a filled circle); a state is always
spelled out in words somewhere in the expanded view.

---

## Layout

```
┌────────────┬──────────────────────────────────────────┐
│ brand      │ topbar: h1 + one-line sub | actions      │
│ nav        ├──────────────────────────────────────────┤
│  ▸ item    │ content — the only scrolling region      │
│  ▸ item    │   section title                          │
│            │   ┌ card ─────────────────────────────┐  │
│            │   └───────────────────────────────────┘  │
│ legend     │                                          │
│ theme      │                                          │
└────────────┴──────────────────────────────────────────┘
```

- `.app { display: flex; height: 100vh; overflow: hidden; }` — the shell
  never scrolls; `.content` does. The topbar and nav stay put while you
  read a long table.
- Sidebar is **232px**, fixed, `--panel`, with a right border *and* a
  vertical accent gradient bleeding through it:

```css
.sidebar::after {
  content: ""; position: absolute; inset: 0 -1px 0 auto; width: 1px;
  background: linear-gradient(180deg, transparent, var(--accent-line) 25%,
                                      var(--accent-line) 60%, transparent);
}
```

  That one line does more for the identity than anything else in the
  stylesheet. Keep it.

- Topbar: `align-items: flex-end`, `linear-gradient(180deg, var(--panel), var(--bg))`,
  bottom border. Title plus **one sentence** of sub-copy, `max-width: 78ch`.
  Actions right, `flex: none`.
- Content: `padding: 20px 26px 60px`. The 60px bottom is deliberate — the
  last card must not sit against the viewport edge.
- Below **760px** the sidebar collapses to a 62px icon rail
  (`.brand-text, .nav-label, .nav-badge, .legend { display: none }`) and
  horizontal padding drops to 14px. That is the only breakpoint. These are
  desktop tools; a phone layout would be a different product.
- **The operator can fold the sidebar to that rail at any width**, and
  only the operator does — nothing folds it on their behalf. Two controls,
  both in the brand row: a two-line button at its right edge (two lines,
  not a hamburger's three: it folds the sidebar, it opens no menu), and the
  brand mark itself, which is the only thing left to click once folded.
  Both carry the action as their `title` ("Collapse the sidebar" /
  "Expand the sidebar"). The choice is remembered per browser
  (`localStorage`, in `try/catch` like the theme). Folded, every nav item
  keeps its glyph and its `title` tooltip, so nothing becomes unreachable.
- **The brand mark is drawn, not typed.** An inline SVG diamond in the
  gradient tile: a font's `◈` sits on the font's baseline and never quite
  centres in a 32px square.

Every wide thing (tables, code, diagrams) scrolls inside its own
`.table-scroll { overflow-x: auto }`. The page body never scrolls sideways.

---

## Components

### Sidebar nav item

A `<button>`, never an `<a>` — these are view switches, not navigations.
Active state is a ghost-purple fill plus a 3px gradient rail bleeding off
the left edge (`::before` at `left: -10px`). Icon is a **Unicode glyph**
(`▤ ▦ ◍ ⚙ ◈ ▸ ◐`), 16px wide, centred, `--accent`. No icon font, no SVG
sprite — a glyph is one character and cannot fail to load.

Trailing `.nav-badge` carries a live count (`3/7`, `12`). It is
`:empty { display: none }`, so it simply is not there before the first poll
returns rather than flashing a zero.

### Card — the workhorse

Everything that is a *thing* is a card: a service, a bank, a merchant.
Collapsed it is one row; expanded it is the full record.

```html
<div class="card is-open">
  <div class="card-head" data-card="{id}">
    <span class="chev">▸</span>
    <i class="dot up"></i>
    <div class="card-title">
      <span class="name">Worldline <span class="pill vendor">vendor</span></span>
      <span class="desc">One-line summary, ellipsised when collapsed.</span>
    </div>
    <div class="card-meta"><span class="spark">…</span><span>34ms</span></div>
  </div>
  <div class="card-body">…</div>
</div>
```

Rules:

- The **head is the whole hit target**, but clicks on
  `button, a, input, select, textarea, label, .form` are excluded — a form
  laid out inside a card must not collapse it when the operator clicks the
  gap between two fields.
- `.desc` is `white-space: nowrap` + ellipsis when collapsed, `normal` when
  open. One line at rest, full text when you have asked for it.
- Open state is **held in app state keyed `view:id`**, not in the DOM, so a
  background poll re-render does not collapse what you are reading.
- Border goes `--accent-line` on both `:hover` and `.is-open`. Hover and
  open are the same visual affordance.
- The chevron points right when closed and down when open (90°, 150ms).
  Only a card's *own* chevron turns — the rule is
  `.card.is-open > .card-head .chev` — or a closed card nested in an open
  one points down too, and open and closed look the same.

**Accessibility:** the head must be reachable and operable from the
keyboard — `tabindex="0"`, `role="button"`, `aria-expanded`, and Enter/Space
handled alongside the click. The console does not do this yet; new apps
should.

### Plugin card — a card the system under test draws

The platform under test is not part of the lab, so its card is not part of
the console. A platform registers itself (`POST /v1/plugins` on the lab API, contract in
[plugins.md](plugins.md)) with a **descriptor**: the same fields every
service card is drawn from, plus its buttons, the settlement files it can
run, and whether it follows the lab clock. The console draws that with the
components on this page and nothing else.

- **Data, never markup.** Every string in a descriptor is text and is
  escaped like any peer's words. A plugin cannot bring HTML, CSS or script;
  if it needs a component this page does not have, the component is added
  here first — for every plugin at once.
- **First in its view.** Plugins head the Platform view, before the lab's
  own clock and stand-ins: the thing being tested is the thing with the
  buttons.
- **A declared button is one call.** `actions` become buttons in the card's
  action row, each forwarded by the console to the path the plugin declared
  and nowhere else. `primary: true` makes one of them the card's `.btn`
  (still at most one); the rest are `.ghost`. `confirm` is asked first, in
  the plugin's words. The toast is the platform's own answer — its `note`,
  or the whole body. A button that declares `fields` opens an *Action
  form* first (below).
- **Say how the registration stands.** A *Registered* row in the `.kv`:
  live, *lapsed* (stopped renewing) or *stopped* (said goodbye), and when.
  A lapsed or stopped platform is **grey, not red** — it is not a lab
  service that is down, it is someone else's process that is not running —
  and its card shows the plugin's own `start_hint` in a `.note.warn` where
  the buttons would be. No buttons while it is not up: a button on a
  process that is not listening fails in a way that reads as the lab being
  broken.
- **Its logs are activity panels.** Everything under *Activity panel*
  applies; the empty states are the plugin's `empty_hints`, so the hint
  names what *that* platform would do to fill it.
- **Settlement files are a table the lab defines.** A plugin that settles
  declares `settlement.files_path`; the rows, their pills and their buttons
  are this console's component, in the shape the contract fixes. Each row
  can be run on its own (*Run*); several are run together by ticking them
  (*Row selection*, below) and pressing *Run N selected* — never by a
  button that guesses which ones "all" means. One run per currency at a
  time is the platform's rule: ticking a second file in a currency swaps it
  for the first, and the platform's own 409 is still the last word. A row
  the platform marks deletable has *Delete*; a platform that can open a
  file's folder on its own machine gives every row *Open location*. Each
  of those columns exists only when the platform declares the path behind
  it.

### Dashboard — the home view

The first screen answers four questions without a click: *what time is it
in the lab, is the thing under test here, is everything up, and how did the
last runs go?* Then it shows the run itself. Top to bottom:

1. **A row of widgets** (below), in a grid that wraps
   (`repeat(auto-fill, minmax(230px, 1fr))`, gap 10px). Order is fixed so
   the eye learns it: lab clock, platform under test, services, recent runs,
   quick links.
2. **The sequence diagram** (below), with the clock line and run banner
   above it as before.
3. **The run report** card.

The dashboard adds nothing that cannot be done elsewhere. It is a set of
doorways: every widget links to the view and card where its detail lives
and its controls are. The one exception is the platform's primary action —
the button the whole lab exists to be pressed — which the platform widget
repeats, as the same call, so a run can be started from the screen that
shows it.

### Widget — a doorway, not a destination

A small card on the dashboard. It summarises one thing and links to where
that thing lives.

```html
<a class="widget" href="#platform/clock">
  <span class="w-title">Lab clock</span>
  <span class="w-value mono">Fri 09:00 UTC</span>
  <span class="w-line">a business day · shifted −62.5h</span>
  <span class="w-foot">Move it →</span>
</a>
```

- **The whole widget is one link** (`<a>`, since it navigates), to
  `#view/card` — the card opens when you land. A widget that holds a button,
  or lists several things, is a `<div class="widget">` whose title is the
  link: each listed thing links to its own card, and the button is the one
  call it names — never two buttons.
- **Title** is a section heading (12px uppercase faint). **Value** is the
  one headline — 17px, 600, tabular; mono when it is a machine value.
  **Lines** are 12px `--text-dim`, at most three. **Foot** names where the
  link goes, in accent: "Move it →", "All vendors →".
- **Status colour only on status.** A widget border is `--border`, accent
  on hover like a card. A health dot or a pill inside it carries the colour
  — never the whole tile.
- **Bars, not charts.** A history is the sparkline's idiom: one 6px bar
  per item, `ok`/`warn`/`bad`, newest right, with its count beside it ("last
  12 runs"). No axes, no library.
- **Empty says what would fill it**, like every other empty state: "No
  platform has registered — see docs/plugins.md".
- **Unknown stays grey.** A widget whose source did not answer says so in
  its line, grey, with the error in the `title`; it does not disappear, and
  it does not go red unless the source is a lab service that is down.
- **Not deployed is not unknown.** A widget for something this deployment
  does not run — the lab clock on a lab deployed without one — keeps its
  place and says what stands in for it ("the real time"), as a
  `<div class="widget">` with no foot: there is no card to link to.

### Hop drawer — watching one arrow

Every arrow in the sequence diagram is a button. Clicking it opens a
**drawer** on the right of the content area: a live view of the traffic
behind that one hop — the same events the arrow counts, newest first,
refreshed every second while the drawer is open.

- **It is a panel, not a modal.** The diagram stays visible and keeps
  animating beside it, the poll does not pause, and another arrow can be
  clicked to switch the drawer to it. `Escape` or the `×` closes it. Width
  `min(420px, 42%)`, `--panel`, a left border carrying the sidebar's accent
  gradient, full height below the topbar, its own scroll. Fold the
  sidebar if the diagram beside it needs the width.
- **Rows are stacked, not columns.** The activity panel's four-column row
  does not fit 420px — the summary column collapses to a letter a line. In
  the drawer a row is a meta line (time, how long ago, op, peer — small,
  faint, wrapping), then the summary at full width, then each detail key on
  its own line with long values broken anywhere. Times are `HH:MM:SS` plus
  "12s ago"; the date only when it is not today.
- **The selected arrow is marked in the identity colour** — its label goes
  accent and a wide `--accent-ghost` band sits behind its line — because
  selection is decoration. The line itself keeps its status colour.
- **Head:** `from → to` and the hop's label, the counts as pills (the
  activity-panel rules: total neutral, refusals amber, failures red), the
  hop's note, and a link to the card of the service whose log it reads
  ("Open B4B Payments →").
- **Body:** the events as activity-panel rows (`.log-row`), exactly the
  events the server counted for this arrow — not the whole log it came
  from. Rows that arrived since the drawer opened carry a thin accent rail
  so "that one just came in" is visible without reading times. A stage hop
  (the platform talking to itself) lists its stages with their status pills
  instead.
- **The server picks the events.** Which log, which ops and which
  destinations make up a hop is judgement, and it lives where the arrow's
  count is computed, so the drawer and the arrow can never disagree
  (`GET /v1/flow/hops/{id}`).
- **Deep-linkable.** The open hop is the second segment of the hash
  (`#dashboard/payouts`), so "look at this arrow" is a link.

### Stand-in — scaffolding that steps aside

A few of the lab's own services play the platform's part until a platform
plugs in — the settlement service, the webhook receiver. Once one has,
they are noise, so each carries two switches:

- **Disconnect / Reconnect** stops the wiring it owns — its timers, the
  vendor deliveries aimed at it — where that wiring lives (a lab switch on
  the service, a subscription at the bank). The card lists each piece of
  wiring with a pill read live from its owner: `connected` plain status
  green, `disconnected` amber (caveated, not broken), and *not wired here*
  as a hint when a vendor points somewhere else. The button's label is the
  action; the pill in the card head says the state.
- **Hide this card** takes it off its view and out of the diagram, hop and
  all. Contract 10 applies: where the cards were, a note lists what is
  hidden and how each is wired, linking to where the switches are; under
  the diagram, a note names the hidden box and the hops that went with it,
  with how much they still carried. The dashboard's services widget says
  how many are hidden.
- **Hidden by default, switched in one place.** Scaffolding is not what a
  developer opens the app to see, so its cards start hidden, and the
  switches for all of them live together in the app's settings view (the
  console: Configuration → Stand-ins) — one row each: what it plays, its
  wiring as read just now, *Connect*/*Disconnect*, *Show card*/*Hide
  card*, and, where a start-up state is set by an environment variable,
  that variable and the restart it takes. The wiring keeps its own default
  (connected), so the app still works end to end with nothing plugged in.
- **Say what it plays.** Every stand-in card and row carries one sentence
  on the part it stands in for and who calls it — "nothing in the settle
  path calls it; make demo-payment does" — because that sentence is how a
  developer decides whether they need it at all. A stand-in that nothing
  calls still keeps a log, so "has anything called it?" is answered by
  looking, not by assuming.

A platform can ask for both in its descriptor (`stand_ins`,
[plugins.md](plugins.md)); the console applies that when the platform
registers or changes its wishes, never on a renewal — so a button pressed
here is not undone ten seconds later. Every card says who set it last.

### Environment selector — whose lab you are looking at

A lab that serves several platforms shows one **environment** at a time
([lab-api/environments.md](lab-api/environments.md)): its merchants, its
payouts, its activity. The selector says which, on every view, so no
screen can be read as another platform's.

```html
<label class="env-pick" title="The environment every view shows">
  <span class="dot up"></span>
  <select data-env-pick>
    <option value="default">Default</option>
    <option value="sbx" selected>Infinite sbx · sbx</option>
  </select>
</label>
```

- **In the topbar, first, before the view's own actions** — it scopes the
  whole page, not one card. It is there on every view, and only when the
  lab has environments to choose between; a lab with only the default
  draws none.
- **A native `<select>`**, styled as every input is. Options read
  `name · id`, the id in mono where the option allows it, `Default` first.
- **The dot is the selected environment's state**, from the vendors'
  traffic, never from a heartbeat: green once a vendor has seen its
  platform, flat grey until then. It never turns red with age — a deployed
  platform may be quiet for a day and be fine.
- **Changing it re-renders the view at once** and is remembered per browser
  (`localStorage`, in `try/catch` like the theme). Every call the page
  makes names it (the `Lab-Environment` header), and every download link
  carries it (`?env=`), so a downloaded `.env` is the selected
  environment's.
- **An environment that has gone falls back to Default**, with a toast
  saying so; the page never keeps asking for something that does not exist.
- **Vendors that do not keep environments apart yet** are named in a `.note`
  under the topbar while another environment is selected: they hold only
  the default's records, so the page leaves them out rather than showing
  the default's data as if it were this environment's.
- There is no *All environments* entry: every view is one environment's.
  Lab-wide reads are the API's (`Lab-Environment: *`).

### Environments — the view that makes them

One **Card** per environment, the default first and marked `lab's own`:

- Collapsed: the state dot, the name, the id in mono, and *last seen 3 h
  ago* or *never seen* on the right.
- Open: a `.kv` of the id, when it was created, its config, its token in
  mono (shown — it is a test lab with fake data; *Rotate* replaces it),
  *seen by* as a `.pill-row` of the vendors that have seen it, and the
  vendors still shared, as a `.note.warn`.
- The kit is a **Download** of its `.env`, named `fintechlab-<id>.env`.
- *Use* switches the selector to it; *Rotate token* and *Delete* are
  `.ghost` and confirmed in the vendor's own words (contract 7): delete says
  that every vendor forgets the environment's records and its token stops
  working, and that the default is untouched.
- *+ Create environment* in the topbar opens a **Modal**: name, id (blank:
  made from the name), and the config fields, each with a `.hint`. The
  answer's token is shown in the toast, because the environment's card
  shows it again anyway.

### Environment card — the platform card every environment has

The Platform view opens on the card of the environment the selector shows
([lab-api/plan.md, phase 6](lab-api/plan.md#phase-6--the-platform-card-is-the-environments)),
drawn by the lab from the environment itself. It is there whether or not a
runner registered a **Plugin card**, which sits below it with its own
buttons when there is one.

- **Head:** the environment's state dot (green once a vendor has seen its
  platform, grey until then; never red with age), its name, and *last seen
  … ago by banking-circle* on the right.
- **Its settlement files**, a **Table** with **Row selection**: file,
  currency, MIDs, total, a `generated` or `uploaded` **Pill**, and the
  parser's reason under a file it refuses (that row has no box and no
  *Run*). One box per currency counts: ticking another file of a currency
  swaps it for the one ticked now, because a platform settles two
  currencies at once, never two files of one. *Preview* and *Delete* per
  row, each in its own column.
- **Run N selected** publishes the ticked files at Worldline, in the
  environment's SFTP folder. Its toast says the platform's next poll picks
  them up. **Run** on a row does the same for that one file.
- **Poll now (ER)** and **Poll now (AR)** call the platform's own poll and
  toast its answer verbatim. Without a `poll_url` in the environment's
  config there are no buttons, and a `.hint` says where to set one.
- **Seed merchants…** and **Create settlement file…** open a **Modal**
  each, the forms buddy's runner had: merchants, what to do with the
  generated merchants already there and with their files, a seed; and
  transactions, currency, a seed. They run the platform's seeder against
  the environment's databases. Without a seeder the card says so and offers
  **Cut from the lab's trading** instead (currency, trading day), and
  without the databases it says which to set.
- **Upload a file…** opens a picker made on the spot, as the **File field**
  does, and streams the file to the environment's files.

The Dashboard's *Platform under test* widget shows this environment when no
plugin is registered: its dot and name, when it was last seen, and how many
files its card holds.

### Guide — the Docs view

A page to read inside a tool to operate: how the parts of the system fit,
and how to stand them up. It is the one view where prose leads.

- **A diagram first**, hand-drawn SVG on the tokens like the sequence
  diagram — never an image, which would be wrong in one of the themes.
  The system under test is in the identity colour, as on the Dashboard;
  running traffic is a solid line, set-up a dashed one; every arrow is
  labelled with what it does and, in mono beneath, the call that does it.
- **One card per part**: what it is, what it needs, where it lives, the
  command, and what you should see afterwards — linked to the view that
  shows it.
- **The set-up track**: numbered stops on a single rail, each naming the
  part it happens in (a small marker in that part's colour: accent for the
  platform, the accent's dark stop for the library, grey for the lab),
  the command in a mono block, and the check.
- **Say what is true now** in one line at the top, from the live overview:
  how many services answer and whether a platform is registered — the
  guide's step two is not news to someone whose platform is already here.
- Reading width: `max-width` on the page, `78ch` on its lede.

### API reference — the Docs view's second page

Every route a caller can use, the console's own and each service's, with
what it is for and where it goes. It is the page a script writer or an
agent reads before calling anything.

- **Two pages in one view, switched by a row of links** at the top of the
  Docs view (`.doc-pages`): *Guide* and *API reference*. They are links,
  since they navigate, and they look like nav items: the current one has
  the ghost-purple fill, because selection is decoration. The page goes in
  the hash (contract 9): `#docs` is the guide, `#docs/api` the reference,
  `#docs/api:banking-circle` the reference with that service's card open.
- **A diagram first**, drawn with the guide's own boxes, chips and arrows:
  who calls which API. The lab's own callers are on the left: the console
  page, and scripts, agents and the e2e suite, which call the lab API for
  everything lab-level and a vendor surface only as a platform would. The
  lab API and the services, each with its three surfaces, are in the
  middle; the platform is on the right. The system under test is in the
  identity colour.
- **Which to call comes before any route.** A table maps intent to surface
  ("make a vendor misbehave" goes to that service's `/sim/*`), so a reader
  picks the right API before reading paths.
- **The API's routes come from the API** (`GET /v1` on the lab API), in its
  own groups, and never from a list kept in the page. The table that
  registers the routes is the one the page draws, so the page cannot
  document a route that does not exist. Columns: method, path (with its old
  path under it, small, where it has one), what it does and what it reaches. The method is a neutral `.pill`, because a method is
  a classification and not a status: a `DELETE` is not red.
- **Each service is a card** (the Card component, open state keyed
  `docs:api:<id>`). The head has the health dot, the name, the kind pill,
  and as its desc the address a script on the operator's machine reaches it
  at (`browse_url`). The body has a `.kv` of ports, transport and auth, its
  control port (`control_url`, where the lab steers it with no vendor
  login), the service's endpoints from the catalogue, and the path of its full
  reference in the repo (`docs/api/services/<id>.md`). A registered plugin
  is a card too, with the endpoints it declared and no reference path: its
  documentation is its own.
- **Read once, then follow the poll.** The index is fetched once per page
  load; the service cards re-render with the overview poll like everywhere
  else, so a health dot here is as true as on the Vendors view.
- **Planned is not documented.** Routes that are designed but not built
  (the lab API's event streams, in `docs/lab-api/`) get one `.note`
  pointing at the design, never rows in the reference. A reference that lists routes which
  do not answer yet is a reference nobody can trust.

### Sequence diagram

The home view of an app that drives a system: participants as boxes across
the top, lifelines down, one arrow per hop, lighting as its own traffic
arrives.

**Hand-drawn SVG, not a diagram library.** Two reasons, and the second is
the deciding one. A CDN breaks the first non-negotiable in this document,
and a vendored bundle is a megabyte for one picture. More importantly, a
library that renders from a text description rebuilds its DOM on every
change — and the whole value here is that *one* arrow animates while the
rest hold still. You cannot animate an element you replace every second.

**Five states, and they are the vocabulary:**

| State | Means | Colour |
| --- | --- | --- |
| idle | nothing has come this way | `--flow-idle`, a dark grey |
| active | something arrived in the last second | `--up` |
| done | finished, and it was fine | `--flow-done-ok`, a dark green |
| partial | some of a batch was refused | `--warn` |
| failed | it broke | `--down` |

**Moving is one colour, whatever is moving.** There used to be a sixth
state, a bright neutral for "a batch is in flight, verdict unknown". On
screen it sat beside the green it was meant to be distinct from and read as
a second word for the same thing — which it was: both said *traffic arrived
in the last second*. A batch is judged once it stops moving, and that is
what done, partial and failed already say. Do not add a state whose
difference from another only its legend can explain.

Everything at rest is grey **on purpose**: if the resting state carried any
colour, the one arrow that just moved would compete with eleven that did
not. Participants get their own, quieter scale — active while their traffic
moves, `--flow-done` light grey once it has been through.

**The system under test is marked in the identity colour, never a status
one.** One participant is the thing being tested; the rest are the world
it talks to. It gets a wider box, a thicker border and a purple wash, and a
solid purple lifeline where the others are dashed grey — purple because
that is decoration, which this is. Its border still carries its state like
every other box, so being the platform never looks like being healthy.

**Say what time it is in the lab.** Above the diagram, one line with the
lab clock in UTC and in the zones its calendars live in, whether it is
shifted from the real clock, whether it is a business day — amber when it
is not, naming the calendar that fails — and who holds it, if anyone. A run
that did nothing because the lab was on a Sunday must be explainable from
this screen. The clock belongs to the lab, not to the platform: the vendors
and a registered platform both follow it, and it is moved from its own card
(Platform → Lab clock), never from a platform's.

**A lab with no clock still says what time it is.** A deployment may run
without the clock service (the cloud profile: a deployed platform cannot
follow one). The line then shows the real time in the same zones and says
the lab is on the real time because it was deployed without a clock — plain
`.note`, not amber, and no *Move it* link to a card that is not there. It
claims nothing about business days: the calendars live in the clock service,
and a guess would be a bluff.

**A box reports on the participant, an arrow on the hop.** One report stage
failing on a mail hop does not make the bank unwell. Red on a box means the
service is unreachable; red on an arrow means that traffic failed. A box
that means "something that touched this went wrong" is a box people stop
believing.

**Hold every state for at least a second.** A hop can take three
milliseconds; without a hold it repaints twice between two frames and is
never seen. The glow ramps up and back down across the same 1.2s
(`@keyframes flow-pulse`), so the eye is drawn and then released. Under
`prefers-reduced-motion` the colour still changes and the hold still
applies — only the ramp goes.

**The server counts, the browser notices.** The endpoint reports how many
messages each hop has carried; the view remembers what that number was a
poll ago. "Something just happened" is a difference between two
observations, and only the client is in a position to see it. It also means
the server stays a pure function of what the services say.

**Poll faster, but only while it matters.** The base rate stays the app's
usual 5s; the diagram runs at 1s *while a run is in flight*. A run takes
forty seconds, and an arrow lighting four seconds late is not a sequence,
it is a summary.

**Say what a hop is for.** Each arrow carries a `<title>`: why it exists,
and the peer's own words for the last thing through it. An arrow that lights
from background polling rather than from the run says so there, rather than
implying a story that did not happen.

**One arrow per recipient, not per message type.** Two hops carrying
identical bytes to different listeners are two arrows, because the question
the diagram answers is *did it arrive*, and that has a different answer for
each of them. A vendor fanning a webhook out to the app's own stub and to
the system under test looks healthy on one merged arrow while the system
under test hears nothing — which is exactly the failure a diagram is for.
Split on whatever the vendor recorded about the destination, and when a
message cannot be attributed, count it against the app's own side: an arrow
into the system under test asserts *your listener was called*, and that
assertion must never rest on a guess.

**Render at 1:1 and never magnify.** An SVG at `width: 100%` scales its own
text with the viewport: on a wide screen the labels come out half again the
size of the prose beside them and the page reads as two documents. Cap it at
its natural width (`max-width`), let it shrink below that, and give the
wrapper `overflow-x: auto`.

**Annotations are annotations.** Everything inside the diagram sits *below*
the body size: 12px participant titles, 11px arrow labels, 10px stats, 9.5px
counts, 9px roles. If a label needs to be as loud as the prose, it is not a
label.

**A count off a bounded window is a floor, not a total.** Print `256+`, never
`256`, or the diagram is reporting the size of its own buffer as traffic.

One trap: **CSS `text-anchor` beats the `text-anchor` attribute.** A
self-call's label set to `text-anchor="start"` inline will still be centred
by a `.label { text-anchor: middle }` rule — and it lands on top of the loop
it is labelling. Use a class.

### Activity panel

A vendor that is being driven by another system needs somewhere to say what
that system just did to it. The panel is a **card nested inside a card**:
same chevron, same open affordance, same `view:id` state key — the nested
one keys on `"<service>:<log>"`, which is unique without inventing a second
mechanism.

```html
<div class="card log is-open">
  <div class="card-head" data-card="b4b:payments" role="button" tabindex="0" aria-expanded="true">
    <span class="chev">▸</span>
    <div class="card-title">
      <span class="name">Payouts received <span class="pill">6 total</span> <span class="pill bad">2 failed</span></span>
      <span class="desc">payout 733.34 EUR to ben_… — 12s ago</span>
    </div>
  </div>
  <div class="card-body">
    <div class="note">What this log is for.</div>
    <div class="log-rows">
      <div class="log-row warn">
        <span class="log-time">22:19:03</span>
        <span class="log-op">payment.create</span>
        <span class="log-main">
          <span class="log-summary">payout 951.98 EUR to ben_7 — beneficiary is sanctions-blocked</span>
          <span class="log-detail">amount=951.98 EUR  external_ref=sttl_v1:…</span>
        </span>
        <span class="log-peer">10.89.0.1</span>
      </div>
    </div>
  </div>
</div>
```

Rules, and they are the whole component:

- **Collapsed, the header is the headline.** Counts as pills — total is
  neutral, refusals amber, failures red — and the most recent event's own
  summary as the `.desc`. An operator should be able to close every panel
  and still see, from four collapsed headers, which hop is broken.
- **The count says what the amber is.** For a vendor, amber is a request
  it refused, and the pill says *refused*. A log whose amber means
  something else names it in its snapshot's `labels` (`{"warn": …,
  "bad": …}`) and the pill uses that: a platform's runs are *with failed
  stages*, its processes *restarted*. "1 refused" on a run that nothing
  refused is a false statement in the one place people look first.
- **Times are wall-clock.** Every log stamps `at` with the real time, even
  where the lab clock has been moved — a platform's logs included — so
  "2m ago" is true and events from different services sort together. The
  business date lives in the summary or the detail, never in `at`.
- **A good row is not coloured.** Only `.warn` and `.bad` take a left
  border and a wash (6% / 9%). Painting every accepted call green makes the
  two that are not green *harder* to find. This is the same reason
  `--unknown` exists.
- **The row carries the peer's own words.** A refusal's summary ends with
  the vendor's message verbatim, exactly as a toast does. "Payout rejected"
  is not a log line.
- **The body scrolls, the card does not grow.** `max-height: 340px` with
  its own `overflow-y`, so a hundred rows never push the rest of the card
  off the screen.
- **Say when the list is a window.** "Showing the last 100 of 4000" under
  the rows; a ring buffer that silently drops history is a ring buffer
  people misread.
- **The empty state names what would fill it** — "Nothing yet. A payout
  from the platform lands here the moment it is asked for" — never "no
  events".
- **Fetch only while it is being read.** The parent card being open is the
  signal; polling every vendor's log for nobody is traffic with no reader.
- **An unreachable service is not an empty list.** The panel renders
  `.note.bad` with the error. Those two states look identical once the
  failure is swallowed, and they send you to opposite ends of the stack.
- A service that keeps no log renders **no panel at all**, and its API
  answers `501` — the same rule as everything else here: never offer what
  the backend cannot do.


**Steps.** An event may carry `steps` (a run's stages, say). They are drawn
under the event's summary and detail as a compact `.steps` table — *Step*
(mono, with its `note` as a `.hint` under it), *Status* (a pill: `done`,
amber `running`, red `bad`), *Started*, *Finished*, and *Took*, right-aligned
in tabular figures at the precision it deserves (950 ms, 16.1 s, 2 m 43 s).
The emitter's times are on its own clock, so *Took* is the column to read.

### Download

A file the operator takes away — an `.env`, a certificate, a key — is a
**link, not an action**:

```html
<a class="ghost small" href="/v1/connect/env/b4b" download>Download fintechlab-b4b.env</a>
```

- **A `.ghost`, never a `.btn`.** It changes nothing; the one primary
  action in the card is still the one that does.
- **No `data-action`, no toast.** The browser's own download is the
  feedback, and a toast saying "downloaded" would be a claim the page
  cannot check.
- **The label names the file it saves**, and the server's
  `Content-Disposition` gives it exactly that name, so the button and
  `curl -OJ` of the same URL produce the same file.
- **A file that does not exist yet is not a link.** Say what generates it,
  in the row where the link would be — a link to a 503 is the same lie as a
  disabled button.
- Key material is served `Cache-Control: no-store`, and a listing of what
  can be downloaded names files and variables, never their contents.

### Pill

`10.5px` mono, uppercase, one-word. Two families that must not be confused:

- **Classification** — `vendor` (accent), `platform` (dim), `supporting`
  (faint). Neutral. Says what kind of thing this is.
- **Status** — `ok`, `bad`, `warn`. Coloured, derived at 35%.

Never invent a third colour. If a new classification is needed it is a
grey.

**More than two together go in a `.pill-row`**: `display: flex`,
`flex-wrap: wrap`, `gap: 5px 6px`. A pill is 10.5px mono on a unit
line-height, so a group left to wrap on its own stacks its rows tighter
than the pills are tall and eight event types read as one grey mass. The
row gap is what keeps them countable. Add `.num` to push the group right
in a numeric cell.

### Health dot

8px circle, four states, and the fourth is the point:

| Class | Meaning | Look |
| --- | --- | --- |
| `.up` | answered 2xx | green + 18% halo |
| `.down` | refused, timed out, or non-2xx | red + 18% halo |
| `.unknown` | not probed yet | flat grey, no halo |
| `.not-probed` | has no health endpoint at all | dashed ring, no fill |

Distinguishing the last two is not pedantry. "I have not asked yet" and
"there is nothing to ask" are different facts, and both are different from
"it is down".

### Sparkline

Twenty-four 3px bars (the probe history the console keeps), `on` green / `off` red, 12px tall on a 5px `--panel-3`
baseline. Pure CSS, no canvas, no library. Shows the last couple of minutes
of probes. Cheap history beats an instantaneous truth: it tells you
*flapping* apart from *down*.

### Stat tile

A row of `min-width: 108px` tiles above a list: big tabular number, tiny
uppercase caption. Modifiers `.up`, `.down`, `.accent` colour **the number
only**. Four tiles maximum — this is a summary of the list below it. The
home view's summary of *everything* is made of widgets, not more tiles.

### Definition list (`.kv`)

`grid-template-columns: max-content 1fr`, `gap: 6px 18px`. Term in
`--text-faint` sans, value in mono. This is the default renderer for "the
record" inside an expanded card. Override to sans on the values that are
prose (see [Type](#type)).

### Table

Uppercase 10.5px faint headers with a `--border` underline; rows separated
by `--border-soft`; last row's border removed. Numerics right-aligned and
tabular. No zebra striping, no row hover — the borders are enough and
striping in a dark palette reads as noise.

**One action per column, and each action column is `width: 1%` with
`white-space: nowrap`.** The width makes the table hand its slack to the
columns carrying text; the nowrap stops a two-word label being broken
across lines. Two buttons sharing one cell get squeezed by whatever is
widest in the row and then wrap into what looks like one broken control —
if a row has a toggle and a probe, that is two columns. Leave the header
blank where the button says what it is, and label it where a reader would
otherwise have to click to find out.

### Row selection

When a table's rows can be acted on together — settlement files run in the
same moment — the first column is a **native checkbox**, `width: 1%`, and
the action sits under the table as a `.ghost` naming the count: *Run 2
selected*, beside a *Clear*. Nothing is ticked to begin with.

- **Only rows the action can take get a box.** A row that cannot run has an
  empty cell, not a disabled box.
- **A rule about which rows go together is enforced by the boxes**, and
  said in a hint under the table: ticking a second file in a currency
  unticks the first, because the platform runs one per currency.
- **No column at all when there is nothing to choose between** — every
  runnable row in the same currency means a selection could only ever hold
  one, and *Run* on the row already does that.
- The selection is held in app state keyed by the table's owner, like open
  cards, so a poll's re-render keeps it; a row that stops being runnable
  drops out of it. A ticked checkbox having focus does **not** pause the
  poll the way a text field does — there is nothing half-typed to lose.

This is not a toggle (below): a checkbox here selects a row for an action
named elsewhere, it does not switch anything on.

### Form

`.form` is a **flex row of `.field` columns**, `align-items: flex-end`, so
labels of different lengths still line up their inputs, and the submit
button sits on the same baseline. `.field.wide { flex: 1 1 220px }` for the
one that should soak up space.

Inputs are `--panel-2` on `--border`, `--radius-sm`. Focus is the accent
border plus a 3px `--accent-ghost` ring — the same ring everywhere, on
every focusable thing:

```css
input:focus, select:focus, textarea:focus,
:where(button, a, [tabindex]):focus-visible {
  outline: none; border-color: var(--accent);
  box-shadow: 0 0 0 3px var(--accent-ghost);
}
```

Textareas are mono, `min-height: 120px`, `resize: vertical` — they hold
JSON.

### Action form — inputs a plugin asks for

A plugin's button that needs input (how many merchants, which directory)
declares `fields` ([plugins.md](plugins.md#action-fields)), and the console
draws them — a plugin never brings its own form. The button opens a
**Modal**: the action's label as the title, its `note` under it, one `.form`
row of `.field`s in declared order (a `text` field is `.wide`), each
field's `hint` as a `.hint` line naming the field, and the foot's `.btn`
repeating the action's label — the operator confirms the thing they
clicked, not "Submit".

- Three input types and no more: `number` (`<input type="number">` with the
  declared `min`/`max`), `text`, `select`. Anything else is refused at
  registration, so a form never renders with a field missing.
- Defaults are filled in; the operator should be able to press the button
  straight away and get the plugin's ordinary case.
- A number outside its range is refused in a toast before any call, and the
  modal stays open with what was typed. The platform's own `400` does the
  same, verbatim.
- The poll is already paused while a modal is open, so a re-render never
  eats a half-typed value.

### File field

Native `<input type="file">`. No dropzone, no library, no drag overlay.
The control sits in a `.field` like any other input; the submit button is
sentence-case and names the destination ("Upload to SFTP location"), not
"Choose file" twice. A `.note` above the form says where the bytes will
land and why a field is required. The toast body is the peer's response
verbatim (path written, byte count, rejection reason).

The console never writes a bind-mount itself. The button is exactly one
`POST` a human could make with `curl --data-binary`.

**In a view that polls, the picker is made on the spot.** A re-render
replaces the DOM, and a file chosen in an `<input type="file">` cannot be
put back into its replacement — so the button ("Upload a file to …")
creates a picker, the upload starts on the choice, and the progress
("43% of 120 MB") is held in app state and drawn as a `.note` above the
button, where every re-render draws it again. Upload with
`XMLHttpRequest`: `fetch` cannot report upload progress, and a big file
with no progress looks like a hung button. Bytes are streamed end to end;
nothing in between holds the whole file.

### File preview

A **Preview** button on a file's row opens the head of the file in a wide
modal (`min(1100px, 100%)`): mono, never wrapped — a CSV row wrapped is a
different row — with a line-number gutter that is not selected with the
text, scrolling both ways inside the modal. It shows the first 100 lines,
and offers the first 1000; never more, and the head says so: "The first
100 lines of 214.3 MB — the rest is not read, however long the file is."
A line cut short says that too. The server reads only as far as it
returns, so a million-line file previews as fast as a short one.

### Open location

**Open location** on a file's row asks the machine the file is on to show
it in its own file manager. The console cannot do that itself — it may be
in a container, and a web page may not open a local folder — so it is a
call to the platform, which is on that machine. A `.ghost`, never a link:
`file://` URLs are blocked from an `http` page, and a link that does
nothing on click is a broken control.

The toast is the platform's answer: the folder it opened, or — on a
machine with no desktop session — why it could not, with the absolute path
so the operator can go there by hand. A platform that cannot open folders
declares no `reveal_path` and its rows carry no button.

### Note

A left-bordered wash, used for the sentence that explains what you are
looking at. Default is accent; `.bad` and `.warn` shift it. A note is
*prose*, not a value — it is the place to say why something is read-only,
or why an action is not offered.

### Toast

Bottom-right stack, `--panel-2`, 3px left border in accent / green / red,
title in prose, **body in mono, pre-wrapped, `max-height: 8em`, scrollable**.
The body carries the peer's own message verbatim.

Failures live **12 seconds**, successes **6**. Failures carry the thing
worth reading twice.

The stack needs `aria-live="polite"` (and `role="alert"` for failures) —
otherwise the entire feedback channel of the app is invisible to a screen
reader.

### Modal

Backdrop `rgba(6, 4, 12, .6)` + `blur(3px)`, `display: grid; place-items: center`,
panel `min(620px, 100%)`, `max-height: 86vh`. Head / body / foot; foot is
right-aligned with ghost-cancel then primary-confirm. Closes on backdrop
click and on Escape.

**One trap worth knowing about**, because it cost this repo a commit: the
browser's built-in `[hidden]` rule is a bare attribute selector, so *any*
class that sets `display` outranks it. A `.modal-backdrop { display: grid }`
plus `hidden` leaves the backdrop covering — and blurring — the whole page
from first paint. Every app in this family ships:

```css
[hidden] { display: none !important; }
```

Also give the panel `role="dialog" aria-modal="true"`, move focus into it
on open, and return focus to the trigger on close.

### Buttons

Three kinds and no more:

| Class | Use | Look |
| --- | --- | --- |
| `.btn` | the one primary action in its context | accent gradient, white, soft shadow |
| `.ghost` | everything else | `--panel-2`, `--text-dim`, bordered |
| `.danger` | destructive | transparent, red text, 40% red border |

`.small` shrinks any of them. A card body has **at most one `.btn`**; if
two actions both look primary, one of them is not.

`.danger` is transparent by default and only fills on hover — a delete
button that is a solid red slab pulls the eye to the worst thing on the
screen.

### Toggle — a button, not a switch

Something that is on or off is still a `.ghost` button. No switch widget,
no checkbox dressed up as one. Two rules make it readable:

- **The label is the action, not the state.** "Pause" when it is running,
  "Release 3 queued" when it is paused. A control labelled with its own
  current state — "Paused" — is a control nobody can predict the effect of
  without clicking it.
- **The state lives next to the thing it affects, as a status pill**, not
  in the button. `paused` is an amber pill on the row it applies to. The
  button says what happens next; the pill says what is true now.

A toggle whose off-state has a consequence — held mail, a stopped queue —
carries the count in the label, because "release" and "release eleven
things" are different decisions.

---

## Behaviour contracts

The tokens make it look right. These make it *feel* right.

### 1. Poll, and re-render wholesale

State is small; re-render the active view completely on every change rather
than diffing. Poll every **5s**. But **pause the poll** when:

- a modal is open,
- `document.hidden`,
- the active element is an `INPUT`, `TEXTAREA` or `SELECT`.

Re-rendering replaces the DOM, and doing that under a half-typed form
throws the operator's input away.

And preserve, across every re-render:

- **which cards are open** (state keyed `view:id`),
- **scroll position** (`const y = content.scrollTop; … content.scrollTop = y`),
  and the same for every box that scrolls on its own — a log's rows — which
  carries `data-keep-scroll="<its card key>"` and is put back where it was.
  Without it, reading half-way down a log meant being thrown back to its
  first row on every poll.

A page that jumps to the top while you are reading a table is a page you
close.

### 2. Declare actions in markup, run them from one listener

```html
<button class="btn small" data-action="provision" data-id="{id}">Register outlets</button>
```

```js
const ACTIONS = {
  provision: async (d) => { … },
};
```

One delegated handler disables the button, swaps its label to `…`, awaits,
toasts the outcome, restores the label and reloads. Every handler is a
one-liner and every failure surfaces identically. No per-button
`onclick`, no bespoke error handling.

### 3. Show the peer's error verbatim

```js
const msg = (parsed && (parsed.error || parsed.message)) || text || `HTTP ${res.status}`;
```

A control panel that swallows a vendor's 422 and shows "request failed" is
worse than `curl`. Wrong-looking text from the peer is better than
right-looking text from you.

### 4. Report partial success as partial

A bulk action over N things reports **per thing**. "Provisioned with
failures" listing the two MIDs that failed, red, is a correct outcome —
not an error, and not a success.

### 5. Never offer what the backend cannot do

If a vendor has no account-opening endpoint, do not render a disabled
button. Render a `.note.warn` **saying why there is no button**. And make
the API distinguish it: `501`, not `500`. A UI that cannot tell "this
vendor has no such endpoint" from "this call failed" will offer a retry
button forever.

### 6. Degrade to an explanation, never to a blank

Missing credentials are not fatal. Without the signing key, the view still
renders and says which key is missing and what it would have unlocked.
Without the CA, the affected panels show the error rather than
disappearing. An app that refuses to start because one peer's credentials
are missing is worse than one that tells you which.

### 7. Confirm the irreversible, and say what stays

```js
confirm('Delete this merchant? Anything already registered at B4B stays there — the rail has no delete.')
```

The confirm text states the *limit of the undo*, not "are you sure".

### 8. Derive generated data from the record's own id

Anything the UI invents — addresses, reference numbers, contact emails —
is derived deterministically from the record's id, never from a random
source, so a screenshot, a reloaded file and a test fixture agree. And it
is **obviously fake**: `GB00SIM…` accounts, `.test` domains (RFC 2606, so
they can never be mailed), streets nobody can post to.

### 9. Route the view in the hash

`location.hash` is the view, optionally followed by the one thing open in
it: `#vendors/b4b` opens the B4B card, `#dashboard/payouts` the payouts
hop. Reload lands where you were; a link to a view or a card is shareable,
which is what lets a dashboard widget be a plain link. Nothing else goes in
the URL — no filters, no scroll, no second card.

### 10. A control that suppresses something says so where the silence lands

Any switch that stops a system doing something must surface that state on
the surface where its absence shows up — not only on the control. Pausing a
vendor's webhook delivery makes its notification log go quiet, and a quiet
log is indistinguishable from a broken endpoint, a wrong subscription and
an idle lab. So the log itself carries the line: the suppressed items
appear in it, amber, labelled as queued, with the count still waiting.

The rule generalises. A filter that hides rows says how many it hid. A
muted alert says it is muted in the place the alert would have been. The
failure this prevents is the one where the operator debugs the wrong thing
for twenty minutes because the UI showed them an empty list and let them
assume it meant nothing happened.

---

### 11. A page that outlived its server says so

A single-page app loads its script once. After the server is rebuilt, a tab
left open keeps running the old page: its buttons post what the old server
expected, quietly — an action form that used to ask first now fires at
once. `Cache-Control: no-cache` on the assets only helps the *next* load.

So every answer carries `X-Console-Build` (a hash of the embedded page). The
page remembers the first one it sees and, when an answer names another,
shows a `.note.warn` under the header — "This page is older than the console
serving it" — with a **Reload** button. It does not reload by itself: the
operator may be halfway through something.

## Voice

The copy is half the design.

- **Sentence case** for titles, headings, buttons. Not Title Case.
- **Lowercase for machine states**: `up`, `down`, `unknown`,
  `no health endpoint`. They are values, not sentences.
- Every view has **one sentence** under its title saying what it is *for*,
  and it may be opinionated: "The outlet is what the money cares about."
- Explain the **why** at the point of confusion, not in a docs page. Notes
  and hints exist to say "read-only here because the service loads this
  file at start-up, so an edit that appeared to take effect immediately
  would be a lie."
- Prefer "what it actually does" over "description".
- Empty states carry the next action: *"No merchants yet. Create one — it
  is what every outlet, MID and payout in this lab hangs off."*
- Never bluff. If a number is from a config file rather than the running
  service, label it *what compose.yml ships*, not a live reading.

---

## Checklist for a new app

- [ ] One `index.html`, one `app.css`, one `app.js`, embedded with `//go:embed`.
- [ ] Token block copied verbatim from this document.
- [ ] `[hidden] { display: none !important; }` present.
- [ ] Theme: `data-theme` on `<html>`, OS default, `localStorage` in `try/catch`.
- [ ] Test asserting no asset references an external host.
- [ ] Sidebar + topbar + single scrolling content region; 760px breakpoint.
- [ ] Cards for records; open state keyed `view:id` and preserved across polls.
- [ ] Poll paused on modal / hidden tab / focused field; scroll preserved.
- [ ] `data-action` + one delegated listener + toast, for every action.
- [ ] Peer errors shown verbatim; partial results shown per item.
- [ ] Four health states including *unknown* and *not probed*.
- [ ] Purple decoration, status colour only for status.
- [ ] Contrast ≥ 4.5:1 for every readable string, small caps included.
- [ ] `:focus-visible` ring on everything focusable; card heads keyboard-operable.
- [ ] `aria-live` on the toast stack; `role="dialog"` + focus return on the modal.
- [ ] `prefers-reduced-motion` honoured.
- [ ] Every button is one call to a public API. Nothing is UI-only.

---

## Where the console does not yet match this document

The document is deliberately ahead of the implementation in a few places.
None of these are decisions to revisit — they are work to do:

| Item | Console today |
| --- | --- |
| `--text-faint`, light `--up`, primary-button gradient | still the pre-contrast-fix values |
| `:focus-visible` ring on buttons and nav | inputs only |
| Card head keyboard operation | mouse only |
| `aria-live` toasts, `role="dialog"` modal, focus return | none |
| `prefers-color-scheme` on first paint | hard-coded dark |
| `prefers-reduced-motion` | not honoured |
| Firefox scrollbar (`scrollbar-color`) | `::-webkit-scrollbar` only |

## Changing the system

Edit this file. Then bring the implementations to it — and if a change
invalidates a row in the table above, delete the row. A design document
that describes an app nobody updated is worse than none, because the next
app is built from the document.
