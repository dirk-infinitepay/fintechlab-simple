/* fintech sim lab v1 console.
   Vanilla, no framework, no bundler: the whole point of the lab is that it
   runs offline from one `make up`, and a UI that needs a package registry
   would be the first thing to break. State is small enough to re-render
   the active view wholesale on every change. */

const state = {
  view: 'dashboard',
  open: new Set(),      // "view:id" of expanded cards, so a poll does not collapse them
  data: {},
  activity: {},         // service id -> {logs} | {error}, fetched only while a card is open
  files: {},            // plugin id -> its settlement files, likewise
  selected: {},         // plugin id -> Set of ticked file names (design-system.md, Row selection)
  runs: null,           // the settling plugin's run and sweep logs, for the dashboard
  hop: null,            // the arrow whose traffic the drawer is showing
  hopData: null,
  hopSeen: null,        // events already there when the drawer opened
  focus: null,          // a card to scroll to once it is drawn (#view/card)
  timer: null,
  env: 'default',       // the environment every view shows (design-system.md, Environment selector)
  envs: null,           // GET /v1/environments, read on every load
};

/* One view per kind, rather than one page listing every service under three
   headings. The headings were doing the work of navigation while the nav
   item was still called "Standalone" — a leftover from an architecture this
   tree no longer has. */
const VIEWS = {
  dashboard: {
    title: 'Dashboard',
    sub: 'The lab at a glance, then the last settlement run hop by hop — click an arrow to watch the traffic behind it.',
  },
  vendors: {
    title: 'Vendors',
    sub: 'The third parties this lab simulates. These are the deliverable: point your code at one and it should not notice the difference.',
    kind: 'vendor',
  },
  platform: {
    title: 'Platform',
    sub: 'Your own stack — the platform under test, which registers its own card here, then the lab clock and the stand-ins that exist so a hop can be proved connected.',
    kind: 'platform',
  },
  verification: {
    title: 'Verification',
    sub: 'The checks a merchant passes before any money exists. Every one answers positively by default, and every outcome can be flipped.',
    kind: 'verification',
  },
  banks: {
    title: 'Banks',
    sub: 'Where the money sits: Banking Circle holds the safeguarding accounts until a payout leaves, and the business bank is where it lands — the merchants\' own accounts.',
  },
  merchants: {
    title: 'Merchants',
    sub: 'Distributor → partner → merchant → outlet. The outlet is what the money cares about: Worldline calls it a submerchant and identifies it by MID.',
  },
  environments: {
    title: 'Environments',
    sub: 'The platforms this lab serves — each with its own token, its own logins at every vendor and its own data. The selector above picks whose every view shows.',
  },
  docs: {
    title: 'Docs',
    sub: 'How the three parts fit — your platform with its test harness, the runner library, and this lab — how to stand them up, and every API a caller can use.',
  },
  config: {
    title: 'Configuration',
    sub: 'How a platform connects to this lab, the knobs that change what the lab does, and the two config files behind the timings.',
  },
};

/* --- utilities --------------------------------------------------------- */

const $ = (sel) => document.querySelector(sel);

function esc(v) {
  return String(v == null ? '' : v).replace(/[&<>"']/g, (c) => (
    { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
  ));
}

/* Every call names the environment the page shows (Lab-Environment), so the
   lab API answers with its records only; the default's is named by naming
   none, as every call did before environments. */
async function api(method, path, body, opts = {}) {
  const headers = body === undefined ? {} : { 'Content-Type': 'application/json' };
  if (!opts.noEnv && state.env && state.env !== 'default') headers['Lab-Environment'] = state.env;
  const res = await fetch(path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  noticeBuild(res.headers.get('X-Console-Build'));
  const text = await res.text();
  let parsed = null;
  try { parsed = text ? JSON.parse(text) : null; } catch (_) { /* keep the raw text */ }
  if (!res.ok) {
    // The peer's own message, verbatim. A control panel that swallows a
    // vendor's 422 and shows "request failed" is worse than curl.
    const msg = (parsed && (parsed.error || parsed.message)) || text || `HTTP ${res.status}`;
    throw new Error(msg);
  }
  return parsed;
}

/* A link the browser follows itself (a download) cannot carry a header, so
   it carries the environment in ?env= instead. */
function envHref(path) {
  if (!state.env || state.env === 'default') return path;
  return path + (path.includes('?') ? '&' : '?') + 'env=' + encodeURIComponent(state.env);
}

/* This page is the console it was loaded from. After a rebuild an open tab
   keeps running the old one — its buttons post what the old console posted —
   so every answer names the page the console now serves (X-Console-Build),
   and a page that is no longer it says so instead of carrying on quietly. */
let loadedBuild = null;
function noticeBuild(build) {
  if (!build) return;
  if (loadedBuild === null) { loadedBuild = build; return; }
  const el = $('#stale');
  if (build === loadedBuild || !el.hidden) return;
  el.innerHTML = `This page is older than the console serving it — it was rebuilt since you opened it,
    and buttons here may not do what the new one does.
    <button class="btn small" type="button" id="stale-reload">Reload</button>`;
  el.hidden = false;
  $('#stale-reload').addEventListener('click', () => location.reload());
}

function toast(title, body, kind = '') {
  const el = document.createElement('div');
  el.className = `toast ${kind}`;
  el.innerHTML = `<div class="t-title">${esc(title)}</div>` +
    (body ? `<div class="t-body">${esc(typeof body === 'string' ? body : JSON.stringify(body, null, 2))}</div>` : '');
  $('#toasts').appendChild(el);
  // Failures linger: they carry a vendor's own message, which is usually
  // the thing worth reading twice.
  setTimeout(() => el.remove(), kind === 'bad' ? 12000 : 6000);
}

function ago(iso) {
  if (!iso) return '—';
  const secs = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (secs < 60) return `${Math.floor(secs)}s`;
  if (secs < 3600) return `${Math.floor(secs / 60)}m`;
  if (secs < 86400) return `${Math.floor(secs / 3600)}h`;
  return `${Math.floor(secs / 86400)}d`;
}

function spark(history) {
  if (!history || !history.length) return '';
  return `<span class="spark">${history.map((ok) => `<i class="${ok ? 'on' : 'off'}"></i>`).join('')}</span>`;
}

function cardKey(id) { return `${state.view}:${id}`; }
function isOpen(id) { return state.open.has(cardKey(id)); }

function bindCards(root) {
  root.querySelectorAll('.card-head[data-card]').forEach((head) => {
    head.addEventListener('click', (ev) => {
      // .form is a div, not a <form>, so it has to be named explicitly --
      // otherwise clicking the gap between two fields collapses the card
      // the operator is filling in.
      if (ev.target.closest('button, a, input, select, textarea, label, .form')) return;
      const key = cardKey(head.dataset.card);
      if (state.open.has(key)) state.open.delete(key); else state.open.add(key);
      renderView();
      // Opening a card can reveal a panel whose data is only fetched while
      // it is being read. Without this the first thing an operator sees is
      // "reading…" until the next poll comes round, which reads as broken.
      load();
    });
  });
}

/* Buttons declare what they do in data-action; one delegated listener runs
   it, reports the result and refreshes. Keeps every handler a one-liner
   and every failure surfaced the same way. */
function bindActions(root) {
  root.querySelectorAll('[data-action]').forEach((btn) => {
    btn.addEventListener('click', async (ev) => {
      ev.preventDefault();
      ev.stopPropagation();
      const fn = ACTIONS[btn.dataset.action];
      if (!fn) return;
      btn.disabled = true;
      const label = btn.textContent;
      btn.textContent = '…';
      try {
        await fn(btn.dataset, btn);
      } catch (err) {
        toast('Failed', err.message, 'bad');
      } finally {
        btn.disabled = false;
        btn.textContent = label;
        await load();
      }
    });
  });
}

function fieldsIn(scope) {
  const out = {};
  scope.querySelectorAll('[data-field]').forEach((el) => { out[el.dataset.field] = el.value.trim(); });
  return out;
}

/* --- data -------------------------------------------------------------- */

function isServiceView(view = state.view) { return !!(VIEWS[view] && VIEWS[view].kind); }

async function load() {
  try {
    await loadEnvs();
    if (state.view === 'dashboard') {
      // First, so the widgets below can find the platform in it.
      state.data.overview = await api('GET', '/v1/lab');
      await loadRuns();
      state.data.flow = await api('GET', '/v1/flow');
      flowObserve(state.data.flow);
      // The lab clock heads the diagram. A clock that is not up is a note
      // there, not a broken page.
      try {
        state.clock = await api('GET', '/v1/clock');
      } catch (err) {
        state.clock = { error: err.message };
      }
    }
    if (isServiceView()) {
      state.data.overview = await api('GET', '/v1/lab');
      await loadActivity();
    }
    if (state.view === 'platform') {
      // Read live from the services that own the wiring, never remembered.
      try {
        state.standins = await api('GET', '/v1/stand-ins');
      } catch (err) {
        state.standins = { error: err.message };
      }
    }
    // The index is the lab API's route table: it changes only with a new
    // build of the lab API, so it is read once per page load.
    if (state.view === 'docs' && docsPage() === 'api' && (!state.data.apiIndex || state.data.apiIndex.error)) {
      try {
        state.data.apiIndex = await api('GET', '/v1');
      } catch (err) {
        state.data.apiIndex = { error: err.message };
      }
    }
    if (state.view === 'banks') state.data.banks = await api('GET', '/v1/banks');
    if (state.view === 'merchants') state.data.merchants = await api('GET', '/v1/merchants');
    if (state.view === 'config') {
      state.data.config = await api('GET', '/v1/config');
      try {
        state.standins = await api('GET', '/v1/stand-ins');
      } catch (err) {
        state.standins = { error: err.message };
      }
      try {
        state.connect = await api('GET', '/v1/connect');
      } catch (err) {
        state.connect = { error: err.message };
      }
    }
    // The services badge is wanted on every view, so it is refreshed even
    // when the Services view is not the one on screen.
    // The badges are wanted on every view, so the overview is refreshed even
    // when the view on screen is not a service list.
    if (!isServiceView() && state.view !== 'dashboard') state.data.overview = await api('GET', '/v1/lab');
    state.error = null;
  } catch (err) {
    state.error = err.message;
  }
  renderBadges();
  renderView();
}

/* The environments, read with no environment named: the list is the
   admin's, whichever one the page shows. One that has gone falls back to
   the default, said once, rather than every call after it failing. */
async function loadEnvs() {
  try {
    const r = await api('GET', '/v1/environments', undefined, { noEnv: true });
    state.envs = r.environments || [];
  } catch (err) {
    state.envs = { error: err.message };
    return;
  }
  if (state.env !== 'default' && !state.envs.some((e) => e.id === state.env)) {
    toast('Environment gone', `${state.env} is no longer in this lab — showing the default environment.`, 'warn');
    setEnv('default', false);
  }
}

function setEnv(id, reload = true) {
  state.env = id || 'default';
  try { localStorage.setItem('console-env', state.env); } catch (_) { /* private window */ }
  if (reload) {
    state.data = {};
    state.activity = {};
    state.files = {};
    load();
  }
}

function currentEnv() {
  return Array.isArray(state.envs) ? state.envs.find((e) => e.id === state.env) : null;
}

/* The selector (design-system.md, Environment selector): first in the
   topbar, and only when there is more than the default to choose from. */
function envPicker() {
  if (!Array.isArray(state.envs) || state.envs.length < 2) return '';
  const cur = currentEnv();
  const dot = cur && cur.state === 'seen' ? 'up' : 'unknown';
  return `<label class="env-pick" title="The environment every view shows">
    <i class="dot ${dot}"></i>
    <select data-env-pick aria-label="Environment">
      ${state.envs.map((e) => `<option value="${esc(e.id)}"${e.id === state.env ? ' selected' : ''}>${
        esc(e.default ? 'Default' : (e.name && e.name !== e.id ? `${e.name} · ${e.id}` : e.id))}</option>`).join('')}
    </select>
  </label>`;
}

/* The vendors that do not keep environments apart yet, said while another
   environment is shown: the page leaves them out rather than pass the
   default's records off as this one's. */
function sharedNote() {
  const cur = currentEnv();
  if (!cur || cur.default || !(cur.shared || []).length) return '';
  return `<div class="note warn">Showing <b>${esc(cur.name || cur.id)}</b>. Shared by every environment, so left out
    here: ${cur.shared.map((id) => `<span class="mono">${esc(id)}</span>`).join(', ')} — they hold only the default
    environment's records.</div>`;
}

/* --- environments ------------------------------------------------------ */

function renderEnvironments() {
  if (!state.envs) return `<div class="empty">${esc(state.error || 'Loading…')}</div>`;
  if (state.envs.error) return `<div class="note bad">${esc(state.envs.error)}</div>`;
  const intro = `<div class="note">An environment is one platform using this lab — a deployed stack, a laptop, an e2e
    run. Its token reaches only its own data on the lab API, and every vendor knows it by its own login: its kit below
    is the <span class="mono">.env</span> its platform runs with. The default is the lab's own, with the logins the lab
    always had.</div>`;
  return intro + state.envs.map(envCard).join('');
}

function envCard(e) {
  const id = `env-${e.id}`;
  const open = isOpen(id);
  const seen = e.state === 'seen';
  const meta = seen ? `last seen ${esc(ago(e.last_seen))}` : 'never seen';
  let body = '';
  if (open) {
    const cfg = e.config || {};
    const cfgRows = [
      ['Settlement identifier', cfg.settlement_identifier],
      ['Webhook hosts', (cfg.webhook_hosts || []).join(', ')],
      ['ACI webhook URL', cfg.aci_webhook_url],
      ['B4B callback URL', cfg.b4b_callback_url],
      ['Clock', cfg.clock],
    ].filter(([, v]) => v);
    const seenBy = Object.entries(e.seen_by || {}).filter(([, t]) => t);
    body = `<div class="card-body">
      <dl class="kv">
        <dt>Id</dt><dd class="mono">${esc(e.id)}</dd>
        ${e.created_at ? `<dt>Created</dt><dd>${esc(ago(e.created_at))}</dd>` : ''}
        ${e.token ? `<dt>Token</dt><dd class="mono">${esc(e.token)}</dd>` : (e.default ? '<dt>Token</dt><dd>the admin\'s, LAB_API_TOKEN</dd>' : '')}
        ${cfgRows.map(([k, v]) => `<dt>${esc(k)}</dt><dd class="mono">${esc(v)}</dd>`).join('')}
        <dt>Seen by</dt><dd>${seenBy.length ? `<span class="pill-row">${seenBy.map(([svc, t]) =>
          `<span class="pill ok" title="${esc(ago(t))}">${esc(svc)}</span>`).join('')}</span>` : 'no vendor yet'}</dd>
      </dl>
      ${(e.shared || []).length ? `<div class="note warn">Not kept apart yet: ${e.shared.map((x) => `<span class="mono">${esc(x)}</span>`).join(', ')}.
        Its platform meets the default environment's data there.</div>` : ''}
      <div class="form">
        <a class="ghost small" href="${esc(e.kit)}" download>Download fintechlab-${esc(e.default ? 'env' : e.id)}.env</a>
        ${e.id === state.env ? '' : `<button class="ghost small" data-action="use-env" data-id="${esc(e.id)}">Use</button>`}
        ${e.default ? '' : `<button class="ghost small" data-action="rotate-env-token" data-id="${esc(e.id)}">Rotate token</button>
        <button class="ghost small" data-action="delete-env" data-id="${esc(e.id)}" data-name="${esc(e.name || e.id)}">Delete</button>`}
      </div>
    </div>`;
  }
  return `<div class="card ${open ? 'is-open' : ''}">
    <div class="card-head" data-card="${esc(id)}">
      <span class="chev">▸</span>
      <i class="dot ${seen ? 'up' : 'unknown'}"></i>
      <div class="card-title">
        <span class="name">${esc(e.default ? 'Default' : (e.name || e.id))} <span class="pill ${e.default ? 'platform' : 'vendor'}">${e.default ? "lab's own" : esc(e.id)}</span>
          ${e.id === state.env ? '<span class="pill supporting">shown</span>' : ''}</span>
        <span class="desc">${e.default ? 'Everything the lab had before environments: its built-in logins, its two safeguarding accounts, the stand-ins.'
          : `Its own logins at every vendor${(e.shared || []).length ? ', where the vendor keeps environments apart' : ''}.`}</span>
      </div>
      <div class="card-meta"><span>${meta}</span></div>
    </div>
    ${body}
  </div>`;
}

function openEnvModal() {
  $('#modal').innerHTML = `
    <div class="modal-head">
      <h2>Create an environment</h2>
      <p>A platform of its own on this lab: a token that reaches only its data, and its own login at every vendor. Its
        kit — the <span class="mono">.env</span> its platform runs with — is on its card once it exists.</p>
    </div>
    <div class="modal-body">
      <div class="form">
        <div class="field wide"><label>Name</label><input data-field="name" placeholder="Infinite sbx" autofocus></div>
        <div class="field"><label>Id</label><input data-field="id" placeholder="from the name"></div>
      </div>
      <div class="form">
        <div class="field wide"><label>Settlement identifier</label><input data-field="settlement_identifier" placeholder="the lab's"></div>
        <div class="field"><label>Clock</label>
          <select data-field="clock"><option value="">follows</option><option value="wall">wall</option></select></div>
      </div>
      <div class="form">
        <div class="field wide"><label>Webhook hosts</label><input data-field="webhook_hosts" placeholder="alb.example.com, other.example.com"></div>
      </div>
      <div class="form">
        <div class="field wide"><label>ACI webhook URL</label><input data-field="aci_webhook_url" placeholder="none"></div>
        <div class="field wide"><label>B4B callback URL</label><input data-field="b4b_callback_url" placeholder="per record"></div>
      </div>
      <p class="hint"><b>Settlement identifier</b> — what Worldline names this environment's settlement files.</p>
      <p class="hint"><b>Webhook hosts</b> — the platform's own callback hosts: its Banking Circle subscriptions and B4B
        callbacks may reach them.</p>
      <p class="hint"><b>Clock</b> — wall for a deployed platform that cannot follow the lab clock: once its platform is
        seen, the lab clock cannot be moved.</p>
    </div>
    <div class="modal-foot">
      <button class="ghost" id="modal-cancel">Cancel</button>
      <button class="btn" data-action="create-env">Create environment</button>
    </div>`;
  $('#modal-backdrop').hidden = false;
  $('#modal-cancel').addEventListener('click', closeModal);
  bindActions($('#modal'));
  $('#modal').querySelector('[data-field="name"]').focus();
}

/* The settlement files a registered platform can run, one row each
   (design-system.md, Plugin card). Every file is a run you can start: on
   its own, or several ticked together — which is how two currencies arrive
   on a real morning, and the case single runs never exercise. Both are one
   POST to the plugin's run_path; the platform starts the files together and
   owns the rule that a currency runs once at a time, which the boxes
   enforce too. A file whose merchants are not seeded settles nothing, so it
   gets the command that seeds them instead of a button. */
function filesPanel(s) {
  if (!s.plugin || !s.plugin.settlement || !isUp(s)) return '';
  const d = state.files[s.id];
  if (!d) return '';
  if (d.error) return `<div class="note bad">Settlement files unavailable — ${esc(d.error)}</div>`;
  const files = d.files || [];
  const st = s.plugin.settlement;
  const upload = uploadBlock(s, d);
  if (!files.length) {
    return `<div class="note warn">No settlement files in <span class="mono">${esc(d.dir || '')}</span> —
      ${esc(s.name)} offers nothing to run.</div>${upload}`;
  }

  const runnable = (f) => !f.problem && f.seeded !== false && !f.in_flight;
  const deletable = (f) => !f.in_flight && ((st.delete_path && f.deletable) || (st.upload_path && f.uploaded));
  // A selection only means something across currencies: within one, the
  // platform runs one file at a time and Run on the row already does that.
  const currencies = new Set(files.filter(runnable).map((f) => f.currency));
  const choosing = currencies.size > 1;
  const picked = selectedFiles(s.id, files.filter(runnable));
  const cols = {
    preview: !!st.preview_path,
    reveal: !!st.reveal_path,
    remove: files.some(deletable),
  };

  const rows = files.map((f) => {
    const pills = [];
    if (f.default) pills.push('<span class="pill">default</span>');
    if (f.uploaded) pills.push(`<span class="pill">uploaded${f.bytes != null ? ' · ' + esc(fmtBytes(f.bytes)) : ''}</span>`);
    if (f.problem) pills.push(`<span class="pill warn" title="${esc(f.problem)}">cannot run</span>`);
    if (f.in_flight && f.in_flight.file === f.name) {
      const stages = f.in_flight.stages || [];
      const at = stages.length ? stages[stages.length - 1].stage : 'starting';
      pills.push(`<span class="pill warn">running — ${esc(at)}</span>`);
    } else if (f.in_flight) {
      pills.push(`<span class="pill warn">${esc(f.currency)} busy</span>`);
    }
    if (f.seeded === false) pills.push(`<span class="pill bad">MIDs ${esc(midList(f.missing_mids))} not seeded</span>`);
    const r = f.last_run;
    let last = '—';
    if (r) {
      const p = r.payouts;
      last = r.error
        ? `<span class="pill bad">failed</span> ${esc(r.error)}`
        : `<span class="mono">${esc((r.root || '').slice(0, 8))}</span> · ` +
          `${p ? `${p.count} payouts ${esc(p.total)}` : 'no payouts'}` +
          `${r.finished_at ? ` · ${esc(platformTime(r.finished_at))}` : ''}`;
    }
    const data = `data-id="${esc(s.id)}" data-name="${esc(f.name)}"`;
    const box = choosing && runnable(f)
      ? `<input type="checkbox" data-select-file ${data} data-currency="${esc(f.currency)}"
          aria-label="Select ${esc(f.name)}"${picked.includes(f.name) ? ' checked' : ''}>`
      : '';
    const run = runnable(f)
      ? `<button class="btn small" data-action="plugin-run" data-id="${esc(s.id)}" data-files="${esc(f.name)}">Run</button>`
      : '';
    const where = f.location && f.location !== d.dir
      ? `<div class="hint mono">${esc(f.location)}</div>` : '';
    return `<tr>
      ${choosing ? `<td class="actions">${box}</td>` : ''}
      <td class="mono">${esc(f.name)}${where}</td>
      <td class="mono">${esc(f.currency || '—')}</td>
      <td class="mono">${esc(midList(f.mids))}</td>
      <td class="mono num">${esc(f.total)}</td>
      <td><div class="pill-row">${pills.join('')}</div>${f.problem ? `<div class="hint">${esc(f.problem)}</div>` : ''}</td>
      <td>${last}</td>
      ${cols.preview ? `<td class="actions"><button class="ghost small" data-action="plugin-preview" ${data}>Preview</button></td>` : ''}
      ${cols.reveal ? `<td class="actions"><button class="ghost small" data-action="plugin-reveal" ${data}>Open location</button></td>` : ''}
      <td class="actions">${run}</td>
      ${cols.remove ? `<td class="actions">${deletable(f)
        ? `<button class="danger small" data-action="plugin-delete-file" ${data}>Delete</button>` : ''}</td>` : ''}
    </tr>`;
  }).join('');

  const unseeded = files.filter((f) => f.seeded === false);
  const seedNote = unseeded.length
    ? `<div class="note warn">${unseeded.map((f) => `<span class="mono">${esc(f.name)}</span> pays MIDs
        ${esc(midList(f.missing_mids))}, which have no merchant yet${f.seed_command
          ? ` — seed them with <span class="mono">${esc(f.seed_command)}</span> in the platform's repo`
          : ''}.`).join('<br>')}</div>`
    : '';
  const unknown = d.seeded_error
    ? `<div class="note warn">Could not check which merchants are seeded — ${esc(d.seeded_error)}</div>`
    : '';
  const together = !choosing ? '' : picked.length
    ? `<div class="form">
        <button class="ghost small" data-action="plugin-run" data-id="${esc(s.id)}"
          data-files="${esc(picked.join('|'))}">Run ${picked.length} selected</button>
        <button class="ghost small" data-action="plugin-clear-selection" data-id="${esc(s.id)}">Clear</button>
      </div>
      <p class="hint">${esc(picked.join(' + '))} — started in the same moment. One file per currency: ticking
        another ${esc([...currencies].join(' or '))} file swaps it for the one ticked now.</p>`
    : `<p class="hint">Tick files to run them together — one per currency, started in the same moment, the way
        two currencies arrive on a real morning.</p>`;

  return `<div class="note">Settlement files in <span class="mono">${esc(d.dir || '')}</span>. Run one, or
      tick several and run them together — one run per currency at a time, and the lab clock decides the day.</div>
    <div class="table-scroll"><table>
      <thead><tr>${choosing ? '<th class="actions"></th>' : ''}<th>File</th><th>Currency</th><th>MIDs</th><th class="num">Total</th>
        <th>State</th><th>Last run</th>${cols.preview ? '<th class="actions"></th>' : ''}${cols.reveal ? '<th class="actions"></th>' : ''}<th class="actions"></th>${cols.remove ? '<th class="actions"></th>' : ''}</tr></thead>
      <tbody>${rows}</tbody>
    </table></div>
    ${together}${seedNote}${unknown}
    ${upload}`;
}

/* The files ticked on a plugin's table, in table order, keeping only those
   that can still run: a file that went busy or vanished since it was
   ticked drops out rather than being sent. */
function selectedFiles(id, runnable) {
  const set = state.selected[id];
  if (!set) return [];
  const names = runnable.map((f) => f.name).filter((n) => set.has(n));
  state.selected[id] = new Set(names);
  return names;
}

/* Ticking a file in a currency that already has one ticked swaps them:
   the platform runs one file per currency at a time. */
function toggleSelected(id, name, currency, on) {
  const set = state.selected[id] || new Set();
  const files = (state.files[id] && state.files[id].files) || [];
  if (on) {
    for (const f of files) if (f.currency === currency) set.delete(f.name);
    set.add(name);
  } else {
    set.delete(name);
  }
  state.selected[id] = set;
}

/* A file from anywhere (design-system.md, File field). The picker is not
   in the rendered view: the view re-renders on every poll, and a chosen
   file cannot be put back into a new <input> — so the button opens a
   picker made on the spot and the upload starts on the choice. Progress
   lives in state for the same reason. */
function uploadBlock(s, d) {
  const st = s.plugin.settlement;
  if (!st.upload_path) return '';
  const up = state.uploading && state.uploading.id === s.id ? state.uploading : null;
  return `<div class="section-title">Add a settlement file</div>
    <p class="hint">Any file, from anywhere on this machine. ${esc(s.name)} keeps it in
      <span class="mono">${esc(d.upload_dir || 'its own directory')}</span> and lists it above; a
      Worldline file with a Settlement row can be run like the fixtures. Big files are fine —
      they are streamed, and a preview reads only the head.</p>
    ${up ? `<div class="note">Uploading <span class="mono">${esc(up.name)}</span> — ${esc(up.progress)}</div>` : ''}
    <div class="form">
      <button class="ghost small" data-action="plugin-upload" data-id="${esc(s.id)}"${up ? ' disabled' : ''}>Upload a file to ${esc(s.name)}…</button>
    </div>`;
}

function fmtBytes(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1048576) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1073741824) return `${(n / 1048576).toFixed(1)} MB`;
  return `${(n / 1073741824).toFixed(2)} GB`;
}

/* The head of a file, in a wide modal: line numbers, the lines as they
   are, and how much of the file that is. */
async function openPreview(id, name, lines) {
  const r = await api('GET', `/v1/plugins/${encodeURIComponent(id)}/files/preview?name=${encodeURIComponent(name)}&lines=${lines}`);
  const rows = r.lines || [];
  const width = String(rows.length).length;
  const other = lines >= 1000 ? 100 : 1000;
  $('#modal').classList.add('wide');
  $('#modal').innerHTML = `
    <div class="modal-head">
      <h2 class="mono">${esc(r.name || name)}</h2>
      <p>${r.truncated ? `The first ${rows.length} lines` : `All ${rows.length} lines`} of ${esc(fmtBytes(r.bytes || 0))}${
        r.truncated ? ' — the rest is not read, however long the file is.' : '.'}${
        r.long_lines ? ` ${r.long_lines} long line${r.long_lines === 1 ? ' is' : 's are'} cut short.` : ''}</p>
    </div>
    <div class="modal-body">
      <div class="preview" style="--gutter:${width + 1}ch">${rows.map((l, i) =>
        `<div class="pl"><span class="pn">${i + 1}</span><span class="pt">${esc(l) || ' '}</span></div>`).join('')}</div>
    </div>
    <div class="modal-foot">
      ${r.truncated || lines > 100 ? `<button class="ghost small" data-preview-lines="${other}">Show ${other === 1000 ? 'the first 1000' : 'only 100'} lines</button>` : ''}
      <button class="btn small" data-close-modal>Close</button>
    </div>`;
  $('#modal-backdrop').hidden = false;
  const more = $('#modal').querySelector('[data-preview-lines]');
  if (more) {
    more.addEventListener('click', async () => {
      more.disabled = true;
      more.textContent = '…';
      try {
        await openPreview(id, name, Number(more.dataset.previewLines));
      } catch (err) {
        toast('Failed', err.message, 'bad');
      }
    });
  }
  $('#modal').querySelector('[data-close-modal]').addEventListener('click', closeModal);
  $('#modal').querySelector('[data-close-modal]').focus();
}

/* XHR rather than fetch: fetch cannot report upload progress, and a big
   file with no progress looks like a hung button. */
function uploadFile(id, file) {
  return new Promise((done, fail) => {
    const xhr = new XMLHttpRequest();
    xhr.open('POST', `/v1/plugins/${encodeURIComponent(id)}/files?name=${encodeURIComponent(file.name)}`);
    xhr.setRequestHeader('Content-Type', 'application/octet-stream');
    xhr.upload.onprogress = (ev) => {
      if (!ev.lengthComputable || !state.uploading) return;
      state.uploading.progress = `${Math.floor((ev.loaded / ev.total) * 100)}% of ${fmtBytes(ev.total)}`;
      if (!isEditing()) renderView();
    };
    xhr.onload = () => {
      let body = null;
      try { body = JSON.parse(xhr.responseText); } catch (_) { /* keep the raw text */ }
      if (xhr.status >= 200 && xhr.status < 300) done(body || {});
      else fail(new Error((body && (body.error || body.message)) || xhr.responseText || `HTTP ${xhr.status}`));
    };
    xhr.onerror = () => fail(new Error('the upload did not reach the console'));
    xhr.send(file);
  });
}

/* A time stamped on the lab clock. Not "ago": a platform following it
   stamps with the shifted clock, so measuring it against this browser's
   clock would say "1d ago" of a run that just finished. */
function platformTime(iso) {
  const d = new Date(iso);
  if (!iso || Number.isNaN(d.getTime())) return '—';
  return new Intl.DateTimeFormat('en-GB', {
    timeZone: 'UTC', weekday: 'short', day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit',
  }).format(d) + ' UTC';
}

// "1–5" for a run of consecutive MIDs, the list otherwise.
/* MIDs as a reader wants them: runs of three or more as a range, and a
   file paying hundreds of outlets as its first few and a count — a cell
   holding every one of them is a cell nobody reads. */
function midList(mids) {
  const list = mids || [];
  const parts = [];
  for (let i = 0; i < list.length;) {
    let j = i;
    while (j + 1 < list.length && /^\d+$/.test(list[j + 1]) && Number(list[j + 1]) === Number(list[j]) + 1) j++;
    parts.push(j - i >= 2 ? `${list[i]}–${list[j]}` : list.slice(i, j + 1).join(', '));
    i = j + 1;
  }
  const MAX = 6;
  return parts.length > MAX
    ? `${parts.slice(0, MAX).join(', ')}, … (${list.length} MIDs)`
    : parts.join(', ');
}

/* Who Banking Circle will call, and about what.
   This is the half of the vendor that is invisible until it is wrong: a
   subscription is a URL the bank POSTs to, and "nobody subscribed" and
   "subscribed, pointing at the wrong host" both show up downstream as
   silence. The card names the endpoint, the events behind it, and offers a
   probe, so the two can be told apart in one look. */
/* The lab clock.
   Settlement only runs on a business day, so "what happens on a Sunday" is a
   real test and so is "…and then on Monday, with the same money". The clock
   service owns one offset; every vendor follows it, and so does a platform
   that registered to — which is what makes this two buttons rather than a
   restart of everything with a different env. */
function clockPanel(s) {
  if (s.id !== 'clock') return '';
  const c = state.clock;
  if (!c) return '';
  if (c.error) return `<div class="note bad">Clock unavailable — ${esc(c.error)}</div>`;

  const shifted = Math.abs(c.offset_hours) >= 0.05;
  const when = (c.now || '').replace('T', ' ').slice(0, 16);
  const holds = c.holds || [];
  // What you are typing survives the five-second re-render; until you type,
  // the fields show the clock as it stands.
  const pick = state.clockPick || { date: (c.now || '').slice(0, 10), time: (c.now || '').slice(11, 16) };
  return `<div class="note${c.business_day ? '' : ' warn'}">
      Lab clock <span class="mono">${esc(when)} UTC</span> —
      <span class="mono">${esc(zoneTime(c.now, 'Europe/Paris'))}</span> in Paris,
      <span class="mono">${esc(zoneTime(c.now, 'Europe/Stockholm'))}</span> in Stockholm,
      <span class="mono">${esc(zoneTime(c.now, 'Europe/London'))}</span> in London.
      ${c.business_day
        ? 'A business day on every calendar, so settlement will run.'
        : `<b>Not a business day</b> on ${esc(offCalendars(c))}: the balance check will skip and settlement will not leave the safeguarding account.`}
      ${shifted ? ` Shifted ${esc(String(c.offset_hours))}h from the real clock (${esc(c.mode)} — ${esc(c.reason || '')}).` : ''}
      Every vendor follows this clock, and so does a platform registered to follow it.
    </div>
    ${holds.length ? `<div class="note warn">${holds.map((h) => `<b>Held</b> by <span class="mono">${esc(h.holder)}</span> —
      ${esc(h.reason)}, until ${esc(platformTime(h.until))}.`).join('<br>')} Moves are refused until it is released.</div>` : ''}
    <div class="form">
      <button class="ghost small" data-action="clock-real">Now</button>
      <button class="ghost small" data-action="clock-advance" data-spec="10m">+10 min</button>
      <button class="ghost small" data-action="clock-advance" data-spec="30m">+30 min</button>
      <button class="ghost small" data-action="clock-advance" data-spec="1h">+1 hour</button>
      <button class="ghost small" data-action="clock-advance" data-spec="1d">+1 day</button>
      <button class="ghost small" data-action="clock-advance" data-spec="-1d">−1 day</button>
      <button class="ghost small" data-action="clock-auto">Nearest business day</button>
      <button class="ghost small" data-action="clock-sunday">Move to Sunday</button>
    </div>
    <div class="form">
      <div class="field"><label>Date (UTC)</label>
        <input type="date" data-clock-pick="date" value="${esc(pick.date)}" aria-label="Date (UTC)"></div>
      <div class="field"><label>Time (UTC)</label>
        <input type="time" step="60" data-clock-pick="time" value="${esc(pick.time)}" aria-label="Time (UTC)"></div>
      <button class="ghost small" data-action="clock-set">Set clock</button>
      <span class="hint" data-clock-hint>${esc(pickHint(pick))}</span>
    </div>`;
}

/* The calendars a non-business day fails on, as "GB (Sat), SE (Sat)". */
function offCalendars(c) {
  const off = (c.calendars || []).filter((k) => !k.business_day);
  return off.map((k) => `${k.code} (${k.weekday} ${k.date})`).join(', ') || 'a calendar';
}

/* HH:MM, weekday, in a timezone — for saying what a UTC instant is where
   the platform's calendars live. */
function zoneTime(iso, timeZone) {
  const d = new Date(iso);
  if (!iso || Number.isNaN(d.getTime())) return '—';
  return new Intl.DateTimeFormat('en-GB', { timeZone, weekday: 'short', hour: '2-digit', minute: '2-digit' }).format(d);
}

/* The picked UTC date and time as an instant, or null while incomplete. */
function pickedInstant(pick) {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(pick.date || '') || !/^\d{2}:\d{2}$/.test(pick.time || '')) return null;
  return `${pick.date}T${pick.time}:00Z`;
}

function pickHint(pick) {
  const at = pickedInstant(pick);
  return at ? `= ${zoneTime(at, 'Europe/Paris')} Paris, ${zoneTime(at, 'Europe/Stockholm')} Stockholm` : 'pick a date and a time';
}

/* The next occurrence of a weekday at 11:00 UTC — late enough that every
   calendar the platform checks is on the same date. */
function nextWeekday(day) {
  const d = new Date();
  d.setUTCHours(11, 0, 0, 0);
  while (d.getUTCDay() !== day) d.setUTCDate(d.getUTCDate() + 1);
  return d.toISOString();
}

async function moveClock(body) {
  const c = await api('POST', '/v1/clock', body);
  state.clock = c;
  toast(
    c.business_day ? 'Clock moved — a business day' : 'Clock moved — not a business day',
    `${c.now}\n${(c.calendars || []).map((k) => `${k.weekday} ${k.date} on ${k.code}`).join(', ')}.\n` +
      (c.business_day ? 'Settlement will run.' : 'The balance check will skip and settlement will not leave the bank.'),
    c.business_day ? 'good' : ''
  );
}

function subscriptionPanel(s) {
  if (s.id !== 'banking-circle') return '';
  const d = state.subs;
  if (!d) return '';
  if (d.error) return `<div class="note bad">Subscriptions unavailable — ${esc(d.error)}</div>`;

  const subs = d.subscriptions || [];
  if (!subs.length) {
    return `<div class="note warn">Nothing is subscribed, so every notification this
      vendor produces goes nowhere. A service that subscribes on boot — as
      <span class="mono">apps/banking-circle</span> does — will have pointed its
      <span class="mono">BC_*</span> configuration at another host.</div>`;
  }

  const rows = subs.map((sub) => {
    const events = (sub.events || []).map((e) =>
      `<span class="pill${e.active ? '' : ' warn'}">${esc(e.type)}${e.targets ? ` ·&nbsp;${e.targets}` : ''}</span>`
    ).join('') || '<span class="pill warn">no events</span>';
    // Two stalls, two words, because they are undone differently: retained
    // is what the bank kept when it gave up on a dead endpoint, queued is
    // what you stopped on purpose.
    const queue = []
      .concat(sub.pending ? [`<span class="pill warn">${sub.pending} retained</span>`] : [])
      .concat(sub.queued ? [`<span class="pill warn">${sub.queued} queued</span>`] : [])
      .join('') || '—';
    // The label is the next action, never the current state — see
    // design-system.md, "Toggle — a button, not a switch".
    const toggle = sub.paused
      ? `<button class="ghost small" data-action="bc-resume" data-id="${esc(sub.id)}">Release${sub.queued ? ` ${sub.queued}` : ''}</button>`
      : `<button class="ghost small" data-action="bc-pause" data-id="${esc(sub.id)}">Pause</button>`;
    return `<tr>
      <td class="mono">${esc(sub.endpoint)}</td>
      <td><div class="pill-row">${events}</div></td>
      <td><div class="pill-row">
        <span class="pill ${sub.status === 'active' ? 'ok' : 'warn'}">${esc(sub.status)}</span>${
        sub.paused ? '<span class="pill warn">paused</span>' : ''}</div></td>
      <td><div class="pill-row num">${queue}</div></td>
      <td class="actions">${toggle}</td>
      <td class="actions"><button class="ghost small" data-action="bc-test" data-id="${esc(sub.id)}">Send test</button></td>
    </tr>`;
  }).join('');

  const paused = subs.filter((sub) => sub.paused);
  const held = paused.reduce((n, sub) => n + (sub.queued || 0), 0);
  // Said here as well as in the log, because this is the surface that can
  // explain it: the log below will go quiet, and a quiet log looks exactly
  // like a broken endpoint.
  const note = paused.length
    ? `<div class="note warn"><b>Delivery is paused</b> on ${paused.length}
        ${paused.length === 1 ? 'subscription' : 'subscriptions'}.
        ${held ? `${held} notification${held === 1 ? '' : 's'} waiting.` : 'Nothing is waiting yet.'}
        What queues shows up amber in <b>Notifications sent</b> below, and goes out in order when you release it.
        Nothing else stops: payments still process and notifications are still produced.</div>`
    : '';

  // The second note explains the pause only while nothing is paused. Once
  // something is, the amber note above says more and better, and two
  // stacked paragraphs saying the same thing is how a card stops being read.
  return `${note}<div class="note">Banking Circle POSTs to these, encrypted per subscription, in
      batches of up to ${esc(String(subs[0].max_per_message || 5))}.${paused.length ? ''
      : ` <b>Pause</b> holds one subscription's deliveries so you can watch a catch-up happen on purpose.`}</div>
    <div class="table-scroll"><table>
      <thead><tr>
        <th>Endpoint</th><th>Events</th><th>Status</th><th class="num">Queue</th>
        <th class="actions">Delivery</th><th class="actions"></th>
      </tr></thead>
      <tbody>${rows}</tbody>
    </table></div>`;
}

/* The payouts Banking Circle holds, with the two things the recipient's side
   can do to one after it has been processed: send it back (a return) or
   have the scheme undo it (a reversal). A nested card like the activity
   logs, so it can sit closed while you read something else. */
const PAYOUT_STATE = {
  OutgoingPaymentBooked: ['booked', ''],
  OutgoingPaymentProcessed: ['processed', 'ok'],
  OutgoingPaymentRejected: ['rejected', 'bad'],
  MissingFunding: ['missing funding', 'warn'],
  Reversed: ['reversed', 'warn'],
};

function payoutsPanel(s) {
  if (s.id !== 'banking-circle') return '';
  const d = state.payouts;
  if (!d) return '';
  if (d.error) return `<div class="note bad">Payouts unavailable — ${esc(d.error)}</div>`;

  const id = 'banking-circle:payouts';
  const open = isOpen(id);
  const payouts = d.payouts || [];
  const returned = payouts.filter((p) => p.returned_by).length;
  const reversed = payouts.filter((p) => p.state === 'Reversed').length;
  const rejected = payouts.filter((p) => p.state === 'OutgoingPaymentRejected').length;
  // Counts are data, so they carry status colour; the total never does.
  const pills = [`<span class="pill">${d.total} total</span>`]
    .concat(returned ? [`<span class="pill warn">${returned} returned</span>`] : [])
    .concat(reversed ? [`<span class="pill warn">${reversed} reversed</span>`] : [])
    .concat(rejected ? [`<span class="pill bad">${rejected} rejected</span>`] : [])
    .join(' ');
  const last = payouts[0];
  const desc = last
    ? `${esc(last.id)} ${esc(last.amount)} ${esc(last.currency)} ${esc((PAYOUT_STATE[last.state] || [last.state])[0])} — ${ago(last.created_at)} ago`
    : 'Nothing yet.';

  let body = '';
  if (open) {
    const rows = payouts.map((p) => {
      const [label, tone] = PAYOUT_STATE[p.state] || [p.state, ''];
      const statePills = `<span class="pill ${tone}">${esc(label)}</span>` +
        (p.returned_by ? `<span class="pill warn">returned</span>` : '');
      const act = (action, text, allowed) => allowed
        ? `<button class="danger small" data-action="${action}" data-id="${esc(p.id)}"
             data-amount="${esc(p.amount)}" data-currency="${esc(p.currency)}">${text}</button>`
        : '';
      return `<tr>
        <td class="mono">${esc(p.id)}</td>
        <td class="mono">${esc(p.reference || '—')}</td>
        <td class="mono">${esc(p.external_ref || '—')}</td>
        <td class="mono num">${esc(p.amount)} ${esc(p.currency)}</td>
        <td><div class="pill-row">${statePills}</div></td>
        <td class="actions">${act('bc-return', 'Return', p.can_return)}</td>
        <td class="actions">${act('bc-reverse', 'Reverse', p.can_reverse)}</td>
      </tr>`;
    }).join('');
    body = `<div class="card-body">
      <div class="note"><b>Return</b> is the beneficiary's bank sending a processed payout
        back: the payout stays processed, and the money arrives as a new incoming payment
        flagged <span class="mono">return</span>. <b>Reverse</b> is the scheme undoing it:
        the payout becomes reversed, with a second booking. Only a processed payout that
        has not come back can be either, so only those rows have buttons.</div>
      ${payouts.length ? `<div class="table-scroll"><table>
        <thead><tr>
          <th>Payment</th><th>Reference</th><th>External ref</th><th class="num">Amount</th>
          <th>State</th><th class="actions"></th><th class="actions"></th>
        </tr></thead>
        <tbody>${rows}</tbody>
      </table></div>
      ${d.total > payouts.length ? `<div class="log-foot">Showing the newest ${payouts.length} of ${d.total}.</div>` : ''}`
      : `<div class="empty">No payouts yet. A settlement run sends them here through B4B —
          start one from the platform's card on the Platform view.</div>`}
    </div>`;
  }

  return `<div class="card log ${open ? 'is-open' : ''}">
    <div class="card-head" data-card="${esc(id)}" role="button" tabindex="0" aria-expanded="${open}">
      <span class="chev">▸</span>
      <div class="card-title">
        <span class="name">Payouts ${pills}</span>
        <span class="desc">${desc}</span>
      </div>
    </div>
    ${body}
  </div>`;
}

/* A service's recent history is fetched only while its card is open. Every
   vendor polling its own log every five seconds would be a lot of traffic
   to show nobody, and the panel is the thing that says you are watching. */
async function loadActivity() {
  const ov = state.data.overview;
  if (!ov) return;
  const watched = ov.services.filter(
    (s) => s.activity_path && s.kind === VIEWS[state.view].kind && isOpen(s.id)
  );
  for (const id of Object.keys(state.activity)) {
    if (!watched.some((s) => s.id === id)) delete state.activity[id];
  }
  // Banking Circle's card carries one more thing: who is subscribed to its
  // notifications. It is fetched on the same condition — the card is open —
  // for the same reason.
  if (watched.some((s) => s.id === 'clock')) {
    try {
      state.clock = await api('GET', '/v1/clock');
    } catch (err) {
      state.clock = { error: err.message };
    }
  }
  // A platform's settlement files, while its card is open and it is up.
  const settling = ov.services.filter(
    (s) => s.plugin && s.plugin.settlement && isUp(s) && s.kind === VIEWS[state.view].kind && isOpen(s.id)
  );
  for (const id of Object.keys(state.files)) {
    if (!settling.some((s) => s.id === id)) delete state.files[id];
  }
  await Promise.all(settling.map(async (s) => {
    try {
      state.files[s.id] = await api('GET', `/v1/plugins/${encodeURIComponent(s.id)}/files`);
    } catch (err) {
      state.files[s.id] = { error: err.message };
    }
  }));
  if (watched.some((s) => s.id === 'banking-circle')) {
    try {
      state.subs = await api('GET', '/v1/banking-circle/subscriptions');
    } catch (err) {
      state.subs = { error: err.message };
    }
    try {
      state.payouts = await api('GET', '/v1/banking-circle/payouts');
    } catch (err) {
      state.payouts = { error: err.message };
    }
  }
  // What a platform needs to connect: read while any card here is open,
  // since every vendor card carries a panel for it.
  if (ov.services.some((s) => s.kind === VIEWS[state.view].kind && isOpen(s.id))) {
    try {
      state.connect = await api('GET', '/v1/connect');
    } catch (err) {
      state.connect = { error: err.message };
    }
  }
  await Promise.all(watched.map(async (s) => {
    try {
      state.activity[s.id] = await api('GET', `/v1/services/${encodeURIComponent(s.id)}/activity?limit=100`);
    } catch (err) {
      // Kept against the service rather than thrown: one vendor being down
      // must not blank the whole view, and "this panel could not be read,
      // here is why" is a better answer than an empty list that reads as
      // "nothing has happened".
      state.activity[s.id] = { error: err.message };
    }
  }));
}

/* Connect your platform.
   What a platform under test needs to reach this vendor: an .env in its own
   variable names, and the keys it has to hold. Every vendor card carries its
   own; the Configuration view carries all of them at once.
   Downloads are links, not actions (design-system.md, Download): the server
   names the file, and curl -OJ on the same URL saves the same thing. */
function connectPanel(s) {
  const c = state.connect;
  const kit = c && !c.error && (c.kits || []).find((k) => k.service === s.id);
  if (!kit) return '';
  const id = `${s.id}:connect`;
  const open = isOpen(id);
  const files = kit.files || [];
  const missing = files.filter((f) => !f.present).length;
  const pills = [`<span class="pill">${kit.vars.length} variables</span>`]
    .concat(files.length ? [`<span class="pill">${files.length} keys</span>`] : [])
    .concat(missing ? [`<span class="pill warn">${missing} not generated</span>`] : [])
    .join(' ');
  const envName = `fintechlab-${s.id}.env`;

  const body = open ? `<div class="card-body">
      ${kit.note ? `<div class="note">${esc(kit.note)}</div>` : ''}
      <p class="hint">Sets ${kit.vars.map((v) => `<span class="mono">${esc(v.key)}</span>`).join(', ')}.
        Addresses use <span class="mono">${esc(c.host)}</span>, the host this console was opened on.</p>
      ${files.length ? `<div class="table-scroll"><table>
        <thead><tr><th>File</th><th>What it is for</th><th class="actions"></th></tr></thead>
        <tbody>${files.map((f) => `<tr>
          <td class="mono">${esc(f.name)}</td>
          <td>${esc(f.purpose)}</td>
          <td class="actions">${f.present
            ? `<a class="ghost small" href="${esc(envHref(`/v1/connect/files/${encodeURIComponent(s.id)}/${encodeURIComponent(f.name)}`))}" download>Download</a>`
            : `<span class="hint">not generated yet — ${esc(s.name)} writes it on first start</span>`}</td>
        </tr>`).join('')}</tbody></table></div>` : ''}
      <div class="form">
        <a class="ghost small" href="${esc(envHref(`/v1/connect/env/${encodeURIComponent(s.id)}`))}" download>Download ${esc(envName)}</a>
      </div>
    </div>` : '';

  return `<div class="card log ${open ? 'is-open' : ''}">
    <div class="card-head" data-card="${esc(id)}" role="button" tabindex="0" aria-expanded="${open}">
      <span class="chev">▸</span>
      <div class="card-title">
        <span class="name">Connect your platform ${pills}</span>
        <span class="desc">An .env in the platform's own variable names${files.length ? ', and the keys it has to hold' : ''}.</span>
      </div>
    </div>
    ${body}
  </div>`;
}

/* Every vendor's connection kit at once, for pointing a platform at the
   whole lab. It is the lab's, not any platform's — whichever platform
   registers needs the same file — so it lives on the Configuration view. */
function connectAllPanel() {
  const c = state.connect;
  if (!c) return '';
  if (c.error) return `<div class="note bad">Connection settings unavailable — ${esc(c.error)}</div>`;
  const kits = c.kits || [];
  const id = 'connect';
  const open = isOpen(id);
  const missing = kits.reduce((n, k) => n + (k.files || []).filter((f) => !f.present).length, 0);
  const pills = [`<span class="pill">${kits.length} vendors</span>`]
    .concat(missing ? [`<span class="pill warn">${missing} keys not generated</span>`] : [])
    .join(' ');

  const body = open ? `<div class="card-body">
      <div class="note">One .env for a platform that runs against the whole lab: every vendor's
        addresses, lab credentials and keys, in the platform's own variable names. Use it as it is,
        or copy the blocks you need into your own. Keys are the ones the lab is running with now —
        regenerate them and download again.</div>
      <p class="hint">Addresses use <span class="mono">${esc(c.host)}</span>, the host this console
        was opened on. Add <span class="mono">?host=</span> to a link for another.</p>
      <div class="table-scroll"><table>
        <thead><tr><th>Vendor</th><th class="num">Variables</th><th class="num">Keys</th><th class="actions"></th></tr></thead>
        <tbody>${kits.map((k) => {
          const files = k.files || [];
          const absent = files.filter((f) => !f.present).length;
          return `<tr>
            <td>${esc(k.title)}</td>
            <td class="num">${k.vars.length}</td>
            <td class="num">${files.length}${absent ? ` <span class="pill warn">${absent} not generated</span>` : ''}</td>
            <td class="actions"><a class="ghost small" href="${esc(envHref(`/v1/connect/env/${encodeURIComponent(k.service)}`))}" download>fintechlab-${esc(k.service)}.env</a></td>
          </tr>`;
        }).join('')}</tbody></table></div>
      <p class="hint">Key and certificate files are on each vendor's own card, under Connect your platform.</p>
      <div class="form">
        <a class="ghost small" href="${esc(envHref('/v1/connect/env'))}" download>Download fintechlab${state.env === 'default' ? '' : '-' + esc(state.env)}.env</a>
      </div>
    </div>` : '';

  return `<div class="card ${open ? 'is-open' : ''}">
    <div class="card-head" data-card="${esc(id)}" role="button" tabindex="0" aria-expanded="${open}">
      <span class="chev">▸</span>
      <div class="card-title">
        <span class="name">All vendors, one .env ${pills}</span>
        <span class="desc">Every vendor's .env in one file, for pointing a platform at this lab.</span>
      </div>
    </div>
    ${body}
  </div>`;
}

/* One nested card per log the service keeps. Collapsed, the header is the
   headline — how many calls, and what the last one was. Opened, it is the
   last hundred. */
function activityPanels(s) {
  if (!s.activity_path) return '';
  const got = state.activity[s.id];
  if (!got) return `<div class="note">Reading recent activity…</div>`;
  if (got.error) return `<div class="note bad">Activity unavailable — ${esc(got.error)}</div>`;
  return (got.logs || []).map((log) => logPanel(s, log)).join('');
}

function logPanel(s, log) {
  const id = `${s.id}:${log.name}`;
  const open = isOpen(id);
  const events = log.events || [];
  const failed = events.filter((e) => e.status === 'bad').length;
  const refused = events.filter((e) => e.status === 'warn').length;
  // A vendor's amber is a request it refused. A log that means something
  // else by it — a platform's is a run with failed stages — says so in
  // `labels`, and the count says that instead.
  const labels = log.labels || {};

  // Counts are data, so they carry status colour; the total never does.
  const pills = [`<span class="pill">${log.total} total</span>`]
    .concat(refused ? [`<span class="pill warn">${refused} ${esc(labels.warn || 'refused')}</span>`] : [])
    .concat(failed ? [`<span class="pill bad">${failed} ${esc(labels.bad || 'failed')}</span>`] : [])
    .join(' ');

  const last = log.last;
  const desc = last
    ? `${esc(last.summary)} — ${ago(last.at)} ago`
    : 'Nothing yet.';

  const body = open ? `<div class="card-body">
      ${log.note ? `<div class="note">${esc(log.note)}</div>` : ''}
      ${events.length ? `<div class="log-rows" data-keep-scroll="${esc(id)}">${events.map(logRow).join('')}</div>
        ${log.total > events.length ? `<div class="log-foot">Showing the last ${events.length} of ${log.total}. Older calls have scrolled out of the service's buffer.</div>` : ''}`
      : `<div class="empty">${esc(emptyLogHint(s, log.name))}</div>`}
    </div>` : '';

  return `<div class="card log ${open ? 'is-open' : ''}">
    <div class="card-head" data-card="${esc(id)}" role="button" tabindex="0" aria-expanded="${open}">
      <span class="chev">▸</span>
      <div class="card-title">
        <span class="name">${esc(log.title)} ${pills}</span>
        <span class="desc">${desc}</span>
      </div>
    </div>
    ${body}
  </div>`;
}

function logRow(e) {
  const detail = e.detail || {};
  const keys = Object.keys(detail).filter((k) => k !== 'status').sort();
  return `<div class="log-row ${esc(e.status || 'ok')}">
    <span class="log-time">${esc(clock(e.at))}</span>
    <span class="log-op">${esc(e.op || '')}</span>
    <span class="log-main">
      <span class="log-summary">${esc(e.summary || '')}</span>
      ${keys.length ? `<span class="log-detail">${keys.map((k) => `${esc(k)}=${esc(detail[k])}`).join('  ')}</span>` : ''}
      ${(e.steps || []).length ? stepsTable(e.steps) : ''}
    </span>
    <span class="log-peer">${esc(e.peer || '')}</span>
  </div>`;
}

/* An empty state says what would put something in it. "No events" tells an
   operator nothing they did not already know from looking. */
const EMPTY_HINTS = {
  'b4b:payments': 'Nothing yet. A payout from the platform lands here the moment it is asked for — accepted or refused.',
  'b4b:callbacks': 'Nothing yet. Callbacks appear once a payment moves; a refused one is shown in red, which is usually the thing you are looking for.',
  'banking-circle:payments': 'Nothing yet. B4B posts here once a payout clears its gates, and funding the safeguarding account shows up here too.',
  'banking-circle:notifications': 'Nothing yet. Notification batches appear here as they are posted to a subscription endpoint.',
  'banking-circle:reports': 'Nothing yet. The platform\'s reconciliation sweep reads the intraday report here about an hour after a settlement — move the platform clock forward an hour and trigger it to see one.',
  'clock:changes': 'Nothing yet. Every move of the clock lands here, and every hold a platform takes on it while a run is in flight.',
  'worldline:sftp': 'Nothing yet. This fills when the platform connects and collects a settlement file — the pull, not the file being cut.',
  'bank:credits': 'Nothing yet. A payout lands here a couple of seconds after Banking Circle processes it — run a settlement and watch the outlets\' accounts open.',
  'bank:ledger': 'Nothing yet. make demo-payment moves money between two demo accounts here, through payment-api; the settle path never does.',
  'payment-api:payments': 'Nothing yet, which is normal: nothing in the settle path calls this. make demo-payment, or the e2e suite, lands a payment here.',
  'notifier:deliveries': 'Nothing yet, which is normal: only payment-api enqueues webhooks here, and only make demo-payment calls payment-api.',
};

/* A plugin names its own empty states: only the platform knows what would
   fill its logs. */
function emptyLogHint(s, logName) {
  const own = s.plugin && s.plugin.empty_hints && s.plugin.empty_hints[logName];
  return own || EMPTY_HINTS[`${s.id}:${logName}`] || 'Nothing yet.';
}

function isUp(s) { return !!(s.status && s.status.state === 'up'); }

/* An event's steps (a settlement run's stages): each with its status, when
   it started and finished, and how long it took. The times are the
   emitter's — a platform following the lab clock stamps them on that clock
   — so the durations are what to compare, not the times with the row's own. */
function stepsTable(steps) {
  const took = (s) => {
    const a = Date.parse(s.started_at || '');
    const b = Date.parse(s.finished_at || '');
    if (Number.isNaN(a) || Number.isNaN(b)) return s.status === 'running' ? 'running' : '—';
    return duration(b - a);
  };
  const pill = (st) => `<span class="pill ${st === 'running' ? 'warn' : esc(st || '')}">${esc(st === 'ok' ? 'done' : st || '?')}</span>`;
  return `<table class="steps">
    <thead><tr><th>Step</th><th>Status</th><th>Started</th><th>Finished</th><th class="num">Took</th></tr></thead>
    <tbody>${steps.map((s) => `<tr>
      <td class="mono">${esc(s.name)}${s.note ? `<div class="hint">${esc(s.note)}</div>` : ''}</td>
      <td>${pill(s.status)}</td>
      <td class="mono">${esc(clock(s.started_at) || '—')}</td>
      <td class="mono">${esc(clock(s.finished_at) || '—')}</td>
      <td class="mono num">${esc(took(s))}</td>
    </tr>`).join('')}</tbody>
  </table>`;
}

/* 950 ms, 16.1 s, 2 m 43 s, 1 h 5 m — a span at the precision it deserves. */
function duration(ms) {
  if (ms < 0) return '—';
  if (ms < 1000) return `${Math.round(ms)} ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)} s`;
  const m = Math.floor(ms / 60_000);
  if (m < 60) return `${m} m ${Math.round((ms % 60_000) / 1000)} s`;
  return `${Math.floor(m / 60)} h ${m % 60} m`;
}

function clock(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  return d.toTimeString().slice(0, 8);
}

function renderBadges() {
  const run = state.data.flow && state.data.flow.run;
  const el = $('#badge-dashboard');
  if (el) {
    const stages = (run && run.stages) || [];
    const done = stages.filter((s) => s.status === 'COMPLETED').length;
    el.textContent = stages.length ? `${done}/${stages.length}` : '';
  }
  const ov = state.data.overview;
  if (ov) {
    // Per section, because the whole-lab number stopped being answerable
    // from one screen the moment the list was split. Only services with a
    // health endpoint are counted: a CA script that cannot be probed is not
    // a service that is down.
    for (const [view, meta] of Object.entries(VIEWS)) {
      if (!meta.kind) continue;
      const group = ov.services.filter((s) => s.kind === meta.kind && s.health_path && !isHidden(s));
      const up = group.filter((s) => (s.status || {}).state === 'up').length;
      const el = $(`#badge-${view}`);
      if (el) el.textContent = group.length ? `${up}/${group.length}` : '';
    }
  }
  const banks = state.data.banks;
  if (banks) $('#badge-banks').textContent = String(banks.banks.length);
  const m = state.data.merchants;
  if (m) $('#badge-merchants').textContent = String(m.merchants.length);
}


/* --- system in test ------------------------------------------------------

   A sequence diagram of one settlement run, drawn as SVG by hand.

   Not Mermaid, and not reluctantly: the repo's first non-negotiable is that
   these pages run from `git clone` with no network, which rules out a CDN
   and makes a ~1MB vendored bundle a poor trade for one diagram. The
   deciding reason is the animation, though — the whole point here is that an
   individual arrow lights as its own traffic arrives, and a diagram library
   that re-renders from a text description on every change cannot do that
   without throwing away the DOM it would need to animate.

   State lives in two places on purpose. The server says how many messages
   each hop has carried; the browser remembers what that number was a second
   ago. "Something just happened" is a difference between two observations,
   and only one end of this is in a position to notice it. */

const FLOW_HOLD_MS = 1200;   // a hop that takes 3ms is still worth a look
const FLOW_LIVE_MS = 1000;   // poll while a run is in flight
const FLOW_IDLE_MS = 5000;   // and at the console's usual rate when not

const flow = {
  counts: {},      // step id -> count at the last poll
  lit: {},         // step id -> when it last moved
  runID: null,
  baseline: {},    // step id -> count when this run started
  boxBaseline: {}, // participant id -> stat values when this run started
  timer: null,
};

function flowRunID(data) {
  const run = data && data.run;
  if (!run) return null;
  return run.id || run.root || null;
}

/* Diff first, render second. Every visual state below is derived from these
   two numbers and a clock, so the drawing code stays a pure function of
   state and can be reasoned about on its own. */
function flowObserve(data) {
  const id = flowRunID(data);
  if (id !== flow.runID) {
    // A new run rebases "this run" counters without losing the totals: the
    // vendors' rings are older than any one run and saying otherwise would
    // be the view lying about what it knows.
    flow.runID = id;
    flow.baseline = {};
    flow.boxBaseline = {};
    for (const s of data.steps || []) flow.baseline[s.id] = s.count;
    for (const p of data.participants || []) {
      flow.boxBaseline[p.id] = (p.stats || []).map((st) => Number(st.value) || 0);
    }
  }
  let moved = false;
  for (const s of data.steps || []) {
    const was = flow.counts[s.id];
    if (was !== undefined && s.count > was) {
      flow.lit[s.id] = Date.now();
      moved = true;
    }
    flow.counts[s.id] = s.count;
  }
  // Re-render once when the hold expires, or an arrow that lit on the last
  // poll of a run would stay green until something else happened.
  if (moved) setTimeout(() => { if (state.view === 'dashboard') renderView(); }, FLOW_HOLD_MS + 60);
}

function flowIsLit(id) {
  return flow.lit[id] && Date.now() - flow.lit[id] < FLOW_HOLD_MS;
}

/* The colour rules, in one place:
     idle      nothing has ever come this way          dark grey
     active    something arrived in the last second    green
     partial   some of a batch was refused             amber
     failed    something broke                         red
     done      finished, and it was fine               dark green
   A box is green while its own traffic is moving and light grey once it has
   been through — the participants tell you where you are, the arrows tell
   you what happened. */
function flowStepClass(s) {
  const lit = flowIsLit(s.id);
  if (s.count === 0) return s.failed > 0 ? 'is-failed' : 'is-idle';
  // Moving is one colour whether it is one message or a batch: a batch is
  // judged once it stops, which is what partial and failed below are for.
  if (lit) return s.failed > 0 ? 'is-failed is-lit' : 'is-active is-lit';
  if (s.failed > 0) {
    // Four of five report stages landing and one failing on the mail hop is
    // a partial success, and painting it the same red as "nothing arrived"
    // would make the two indistinguishable at a glance.
    return s.multi && s.failed < s.count ? 'is-partial' : 'is-failed';
  }
  if (s.multi && s.refused > 0) return 'is-partial';
  return 'is-done-ok';
}

/* A box reports on the participant, not on its traffic. One report stage
   failing on the mail hop does not make Banking Circle unwell, and a red
   box that means "something that touched this went wrong" is a box you
   stop believing. Red here is the service itself being unreachable. */
function flowBoxClass(p, steps) {
  if (p.error || p.status === 'down') return 'is-failed';
  const mine = steps.filter((s) => s.from === p.id || s.to === p.id);
  if (mine.some((s) => flowIsLit(s.id))) return 'is-active';
  if (mine.some((s) => s.count > 0)) return 'is-done';
  return 'is-idle';
}

function flowDelta(id, count) {
  const base = flow.baseline[id];
  if (base === undefined || count - base <= 0) return '';
  return `+${count - base}`;
}

/* --- the drawing ------------------------------------------------------- */

/* The canvas is measured in CSS pixels and rendered at 1:1 (see app.css):
   an SVG stretched to the viewport magnifies its own text, which is how a
   diagram ends up shouting over the prose around it. */
const FLOW_W = 1000;         // the canvas for five participants; more widen it
const FLOW_GAP = 202;        // lifeline to lifeline, so neighbouring boxes never touch
const FLOW_PAD = 96;         // half a box, so the outer lifelines sit inside
const FLOW_BOX_W = 178;
const FLOW_SUT_W = 196;      // the system under test's box: wider, still clear of its neighbours
const FLOW_SUT = 'platform'; // the participant being tested; the rest are the world it talks to
const FLOW_BOX_H = 78;
const FLOW_TOP = 10;
const FLOW_ROW = 52;
const FLOW_FIRST_ROW = 124;

/* The lab clock, above the diagram: in UTC and where its calendars live,
   shifted or not, business day or not. */
function flowClock() {
  const c = state.clock;
  if (!c) return '';
  if (c.absent) {
    // Deployed without a clock: the real time, and why there is nothing
    // to move. No business-day claim — the calendars are the clock's.
    const when = (c.now || '').replace('T', ' ').slice(0, 16);
    return `<div class="note">
      Real time <span class="mono">${esc(when)} UTC</span> —
      <span class="mono">${esc(zoneTime(c.now, 'Europe/Paris'))}</span> Paris,
      <span class="mono">${esc(zoneTime(c.now, 'Europe/Stockholm'))}</span> Stockholm,
      <span class="mono">${esc(zoneTime(c.now, 'Europe/London'))}</span> London.
      This lab was deployed without a lab clock, so every vendor dates what it does by the real calendar.
    </div>`;
  }
  if (c.error) {
    return `<div class="note">Lab clock unavailable — ${esc(c.error)}. The vendors keep the
      last time they were given, or the real clock if they never had one.</div>`;
  }
  const shifted = Math.abs(c.offset_hours) >= 0.05;
  const when = (c.now || '').replace('T', ' ').slice(0, 16);
  return `<div class="note${c.business_day ? '' : ' warn'}">
      Lab clock <span class="mono">${esc(when)} UTC</span> —
      <span class="mono">${esc(zoneTime(c.now, 'Europe/Paris'))}</span> Paris,
      <span class="mono">${esc(zoneTime(c.now, 'Europe/Stockholm'))}</span> Stockholm,
      <span class="mono">${esc(zoneTime(c.now, 'Europe/London'))}</span> London.
      ${shifted ? `Shifted ${esc(String(c.offset_hours))}h from the real clock.` : 'On the real clock.'}
      ${c.business_day ? 'A business day.' : `<b>Not a business day</b> on ${esc(offCalendars(c))}: settlement will not leave the bank.`}
      ${(c.holds || []).length ? `Held by <span class="mono">${esc(c.holds[0].holder)}</span>.` : ''}
      <a href="#platform/clock">Move it</a>.
    </div>`;
}

/* Wider for more participants rather than squeezed: a sixth box at the
   five-box spacing overlaps its neighbours, and the wrapper scrolls. */
function flowWidth(participants) {
  return Math.max(FLOW_W, FLOW_PAD * 2 + Math.max(0, participants.length - 1) * FLOW_GAP);
}

function flowColumns(participants) {
  const span = flowWidth(participants) - FLOW_PAD * 2;
  const step = participants.length > 1 ? span / (participants.length - 1) : 0;
  const at = {};
  participants.forEach((p, i) => { at[p.id] = FLOW_PAD + i * step; });
  return at;
}

function flowBox(p, x, klass, deltas) {
  const sut = p.id === FLOW_SUT;
  const width = sut ? FLOW_SUT_W : FLOW_BOX_W;
  const left = x - width / 2;
  const stats = (p.stats || []).map((st, i) => {
    const d = deltas[i] ? ` <tspan class="flow-delta">${esc(deltas[i])}</tspan>` : '';
    return `<text class="flow-stat" x="${left + 10}" y="${FLOW_TOP + 48 + i * 13}">${esc(st.label)}: <tspan class="flow-stat-v">${esc(st.value)}</tspan>${d}</text>`;
  }).join('');
  // The catalogue's full name is the one to keep in the tooltip; the box
  // gets what fits in it, because a title clipped by the viewBox reads as a
  // rendering bug rather than as a long name.
  const short = String(p.label || '').replace(/\s*\(.*\)\s*$/, '');
  const href = participantHref(p.id);
  return `<g class="flow-box ${klass}${sut ? ' is-sut' : ''}"${href ? ` data-href="${esc(href)}" role="link" tabindex="0"` : ''}>
    <title>${esc(p.label)}${p.error ? ' — ' + esc(p.error) : ''}</title>
    <rect x="${left}" y="${FLOW_TOP}" width="${width}" height="${FLOW_BOX_H}" rx="9"></rect>
    <text class="flow-title" x="${left + 10}" y="${FLOW_TOP + 19}">${esc(short)}</text>
    <text class="flow-role" x="${left + 10}" y="${FLOW_TOP + 33}">${esc(p.role || '')}</text>
    <circle class="flow-health ${esc(p.status || 'unknown')}" cx="${left + width - 12}" cy="${FLOW_TOP + 15}" r="3.5"></circle>
    ${stats}
  </g>`;
}

/* The tooltip carries what the line cannot: why the hop exists, and the
   vendor's own words for the last thing that came through it. */
function flowTip(s) {
  const parts = [s.note, s.last_summary].filter(Boolean);
  return parts.length ? `<title>${esc(parts.join(' — '))}</title>` : '';
}

function flowArrow(s, at, y) {
  const klass = flowStepClass(s);
  const delta = flowDelta(s.id, s.count);
  // "256" from a 256-event window is not a count, it is the window. Say so.
  const n = s.capped ? `${s.count}+` : `${s.count}`;
  const badge = s.count
    ? `${n}${delta ? ' (' + delta + ')' : ''}${s.failed ? ' · ' + s.failed + ' failed' : ''}`
    : '';

  // Every arrow is a button that opens its drawer (design-system.md, Hop
  // drawer). The selection band sits behind the line, so the line keeps its
  // status colour; the hit path is wide and invisible, because a 1.3px line
  // is not something anyone can click.
  const sel = state.hop === s.id ? ' is-selected' : '';
  const attrs = `class="flow-step ${klass}${sel}" data-step="${esc(s.id)}" data-hop="${esc(s.id)}"
    role="button" tabindex="0" aria-label="${esc(s.label)}: watch its traffic"`;

  if (s.from === s.to) {
    // A self-call: a small loop off the lifeline, because a hop that never
    // leaves the platform is still a step in the sequence.
    const x = at[s.from];
    const w = 48;
    const d = `M ${x} ${y - 8} h ${w} v 16 h ${-w}`;
    return `<g ${attrs}>
      ${flowTip(s)}
      <path class="flow-sel" d="${d}"></path>
      <path class="flow-hit" d="${d}"></path>
      <path class="flow-line" d="${d}"></path>
      <polygon class="flow-head" points="${x},${y + 8} ${x + 8},${y + 4.5} ${x + 8},${y + 11.5}"></polygon>
      <text class="flow-label at-start" x="${x + w + 10}" y="${y - 1}">${esc(s.label)}</text>
      ${badge ? `<text class="flow-count at-start" x="${x + w + 10}" y="${y + 13}">${esc(badge)}</text>` : ''}
    </g>`;
  }

  const x1 = at[s.from];
  const x2 = at[s.to];
  const dir = x2 > x1 ? 1 : -1;
  const tipX = x2 - 8 * dir;
  const mid = (x1 + x2) / 2;
  return `<g ${attrs}>
    ${flowTip(s)}
    <line class="flow-sel" x1="${x1}" y1="${y}" x2="${x2}" y2="${y}"></line>
    <line class="flow-hit" x1="${x1}" y1="${y - 14}" x2="${x2}" y2="${y - 14}"></line>
    <line class="flow-hit" x1="${x1}" y1="${y}" x2="${x2}" y2="${y}"></line>
    <line class="flow-line" x1="${x1}" y1="${y}" x2="${tipX}" y2="${y}"></line>
    <polygon class="flow-head" points="${x2},${y} ${tipX},${y - 5} ${tipX},${y + 5}"></polygon>
    <text class="flow-label" x="${mid}" y="${y - 7}">${esc(s.label)}</text>
    ${badge ? `<text class="flow-count" x="${mid}" y="${y + 14}">${esc(badge)}</text>` : ''}
  </g>`;
}

function renderFlow() {
  const d = state.data.flow;
  if (!d) return `<div class="empty">${esc(state.error || 'Loading…')}</div>`;

  const parts = d.participants || [];
  const steps = d.steps || [];
  const at = flowColumns(parts);
  const height = FLOW_FIRST_ROW + steps.length * FLOW_ROW + 20;

  const boxes = parts.map((p) => {
    const base = flow.boxBaseline[p.id] || [];
    const deltas = (p.stats || []).map((st, i) => {
      const now = Number(st.value) || 0;
      const was = base[i];
      return was !== undefined && now - was > 0 ? `+${now - was}` : '';
    });
    return flowBox(p, at[p.id], flowBoxClass(p, steps), deltas);
  }).join('');

  const lifelines = parts.map((p) =>
    `<line class="flow-life${p.id === FLOW_SUT ? ' is-sut' : ''}" x1="${at[p.id]}" y1="${FLOW_TOP + FLOW_BOX_H}" x2="${at[p.id]}" y2="${height - 12}"></line>`
  ).join('');

  const arrows = steps.map((s, i) => flowArrow(s, at, FLOW_FIRST_ROW + i * FLOW_ROW)).join('');

  const run = d.run;
  const live = run && !run.finished_at;
  const banner = run
    ? `<div class="note${live ? '' : ' flow-note-done'}">${live ? 'Running now' : 'Last run'} —
        <span class="mono">${esc(run.id || run.root || '')}</span>${run.error ? ' — ' + esc(run.error) : ''}</div>`
    : `<div class="note">No run yet. Start one from the platform's card on
        <a href="${esc(platformHref())}">Platform</a> — a platform under test registers it there — and this
        diagram lights up as it goes.</div>`;

  const unreachable = Object.entries(d.errors || {})
    .map(([id, msg]) => `<div class="note bad">${esc(id)} could not be read — ${esc(msg)}</div>`).join('');

  return `${flowClock()}${banner}${unreachable}
    <div class="flow-wrap">
      <svg class="flow" viewBox="0 0 ${flowWidth(parts)} ${height}" style="max-width:${flowWidth(parts)}px" role="img"
           aria-label="Sequence diagram of one settlement run">
        ${lifelines}${boxes}${arrows}
      </svg>
    </div>
    ${flowHiddenNote(d)}
    ${flowLegend()}
    ${flowReport(d)}`;
}

/* A stand-in taken out of the diagram is a silence; say what it hides
   (design-system.md, contract 10). */
function flowHiddenNote(d) {
  const hidden = (d.hidden || []);
  if (!hidden.length) return '';
  return `<div class="note">${hidden.map((h) => `<b>${esc(shortName(h.name || h.id))}</b> is a hidden stand-in, so it
      is not drawn${h.steps.length ? ` — nor its ${esc(h.steps.join(', '))} hop` : ''}${h.count
        ? `, which still carried ${h.count}${h.count >= 256 ? '+' : ''} in this window` : ''}.`).join(' ')}
      <a href="#config/stand-ins">Stand-ins →</a></div>`;
}

function flowLegend() {
  const keys = [
    ['is-idle', 'not yet'],
    ['is-active', 'happening now'],
    ['is-done-ok', 'done'],
    ['is-partial', 'partly refused'],
    ['is-failed', 'failed'],
  ];
  return `<div class="flow-legend">${keys.map(([k, label]) =>
    `<span class="flow-key ${k}"><i></i>${esc(label)}</span>`).join('')}</div>`;
}

function flowReport(d) {
  const open = isOpen('report');
  const stats = d.report || [];
  const body = open ? `<div class="card-body">
      <dl class="kv">${stats.map((s) =>
        `<dt>${esc(s.label)}</dt><dd>${esc(s.value)}</dd>`).join('')}</dl>
      <div class="note">Fees are not here: they are computed inside the platform's own
        workers and never leave them, so this would have to invent a number. The rest is
        read from the services themselves.</div>
    </div>` : '';
  const headline = stats.slice(0, 3).map((s) => `${s.label} ${s.value}`).join(' · ');
  return `<div class="card ${open ? 'is-open' : ''}">
    <div class="card-head" data-card="report" role="button" tabindex="0" aria-expanded="${open}">
      <span class="chev">▸</span>
      <div class="card-title">
        <span class="name">Run report</span>
        <span class="desc">${esc(headline || 'Nothing to report yet.')}</span>
      </div>
    </div>
    ${body}
  </div>`;
}

/* --- dashboard -----------------------------------------------------------

   The home view: a row of widgets, then the diagram (design-system.md,
   Dashboard). A widget is a doorway — it summarises one thing and links to
   the card where that thing lives — so nothing here can be done only from
   here. The one button it repeats is the platform's own primary action,
   which is the same call its card makes. */

const VIEW_OF_KIND = { vendor: 'vendors', platform: 'platform', verification: 'verification' };

function services() { return (state.data.overview && state.data.overview.services) || []; }

/* #view/id[:log] for a service's card — and, with a log, the panel in it. */
function serviceHref(id, log) {
  const s = services().find((x) => x.id === id);
  const view = s && VIEW_OF_KIND[s.kind];
  if (!view) return '';
  return `#${view}/${encodeURIComponent(id)}${log ? ':' + encodeURIComponent(log) : ''}`;
}

/* The platform the diagram draws: the registered plugin that settles, else
   any registered plugin. */
function settlingPlugin() { return services().find((s) => s.plugin && s.plugin.settlement) || null; }
function platformPlugin() { return settlingPlugin() || services().find((s) => s.plugin) || null; }
function platformHref() { const p = platformPlugin(); return p ? serviceHref(p.id) : '#platform'; }
function participantHref(pid) { return pid === 'platform' ? platformHref() : serviceHref(pid); }

function shortName(name) { return String(name || '').replace(/\s*\(.*\)\s*$/, ''); }

/* The settling platform's logs, for the recent-runs widget. Only while it is
   up: a stopped platform's last answer would be reported as current. */
async function loadRuns() {
  const p = settlingPlugin();
  if (!p || !p.activity_path || !isUp(p)) { state.runs = null; return; }
  try {
    state.runs = { id: p.id, ...(await api('GET', `/v1/services/${encodeURIComponent(p.id)}/activity?limit=50`)) };
  } catch (err) {
    state.runs = { id: p.id, error: err.message };
  }
}

function renderDashboard() {
  return `<div class="widgets">
      ${widgetClock()}${widgetPlatform()}${widgetServices()}${widgetRuns()}${widgetLinks()}
    </div>
    ${renderFlow()}`;
}

function widgetLine(text, klass = '') { return `<span class="w-line${klass ? ' ' + klass : ''}">${text}</span>`; }

function widgetClock() {
  const c = state.clock;
  const shell = (value, lines) => `<a class="widget" href="#platform/clock">
      <span class="w-title">Lab clock</span>${value}${lines}
      <span class="w-foot">Move it →</span>
    </a>`;
  if (!c) return shell('<span class="w-value">…</span>', '');
  if (c.absent) {
    // Not deployed is not unknown: the widget keeps its place, says what
    // stands in for the clock, and links nowhere — there is no card.
    const at = new Date(c.now);
    const when = at.toLocaleString('en-GB', { timeZone: 'UTC', weekday: 'short', hour: '2-digit', minute: '2-digit' });
    return `<div class="widget">
        <span class="w-title">Lab clock</span>
        <span class="w-value mono">${esc(when)} UTC</span>
        ${widgetLine(`${esc((c.now || '').slice(0, 10))} · the real time`)}
        ${widgetLine('deployed without a lab clock: nothing to move', 'faint')}
      </div>`;
  }
  if (c.error) {
    return shell('<span class="w-value">unknown</span>',
      widgetLine('The clock did not answer, so vendors keep the last time they were given.', 'faint') +
      widgetLine(esc(c.error), 'faint mono'));
  }
  const at = new Date(c.now);
  const when = at.toLocaleString('en-GB', { timeZone: 'UTC', weekday: 'short', hour: '2-digit', minute: '2-digit' });
  const shifted = Math.abs(c.offset_hours) >= 0.05;
  const hold = (c.holds || [])[0];
  return shell(`<span class="w-value mono">${esc(when)} UTC</span>`,
    widgetLine(`${esc((c.now || '').slice(0, 10))} · ${c.business_day
      ? 'a business day'
      : `<span class="pill warn">not a business day</span> on ${esc(offCalendars(c))}`}`) +
    widgetLine(shifted ? `shifted <span class="mono">${esc(String(c.offset_hours))}h</span> · ${esc(c.mode)}` : `on the real clock · ${esc(c.mode)}`) +
    (hold ? widgetLine(`held by <span class="mono">${esc(hold.holder)}</span>`) : ''));
}

function widgetPlatform() {
  const p = platformPlugin();
  if (!p) {
    return `<a class="widget" href="#platform">
        <span class="w-title">Platform under test</span>
        <span class="w-value">none registered</span>
        ${widgetLine('A platform puts its own card here by registering with the lab API — see <span class="mono">docs/plugins.md</span>.')}
        <span class="w-foot">Platform →</span>
      </a>`;
  }
  const up = isUp(p);
  const pl = p.plugin;
  const reg = pl.state === 'live'
    ? `registered · renewed ${ago(pl.last_seen)} ago`
    : `<span class="pill warn">${esc(pl.state)}</span> last heard ${ago(pl.last_seen)} ago`;
  const run = state.data.flow && state.data.flow.run;
  const running = run && !run.finished_at;
  const primary = (pl.actions || []).find((a) => a.primary);
  const button = up && primary
    ? `<div class="w-actions"><button class="btn small" data-action="plugin-action" data-id="${esc(p.id)}" data-act="${esc(primary.id)}">${esc(primary.label)}</button></div>`
    : '';
  return `<div class="widget">
      <a class="w-title" href="${esc(serviceHref(p.id))}">Platform under test →</a>
      <span class="w-value"><i class="dot ${esc((p.status || {}).state || 'unknown')}"></i> ${esc(p.name)}</span>
      ${widgetLine(reg)}
      ${up
        ? widgetLine(running ? `running now · <span class="mono">${esc(run.id || run.root || '')}</span>` : (pl.clock === 'wall' ? 'on the wall clock' : 'follows the lab clock'))
        : widgetLine(esc(pl.start_hint || 'Not running. Start it, and it registers again.'), 'faint')}
      ${button}
    </div>`;
}

function widgetServices() {
  const shown = services().filter((s) => !isHidden(s));
  const hidden = services().filter(isHidden);
  const list = shown.filter((s) => s.health_path && !(s.plugin && s.plugin.state !== 'live'));
  const st = (s) => (s.status || {}).state || 'unknown';
  const up = list.filter((s) => st(s) === 'up').length;
  const down = list.filter((s) => st(s) === 'down');
  const chips = shown
    .filter((s) => VIEW_OF_KIND[s.kind])
    .map((s) => `<a class="w-chip" href="${esc(serviceHref(s.id))}" title="${esc(s.name)} — ${esc(st(s))}"><i class="dot ${esc(st(s))}"></i>${esc(shortName(s.name))}</a>`)
    .join('');
  return `<div class="widget">
      <a class="w-title" href="#vendors">Services →</a>
      <span class="w-value">${up}/${list.length} up</span>
      ${down.length
        ? widgetLine(`<span class="pill bad">${down.length} down</span> ${esc(down.map((s) => shortName(s.name)).join(', '))}`)
        : widgetLine(list.length ? 'every service with a health check answers' : 'not probed yet', list.length ? '' : 'faint')}
      <span class="w-chips">${chips}</span>
      ${hidden.length ? `<a class="w-line faint" href="#config/stand-ins">${hidden.length} stand-in${hidden.length === 1 ? '' : 's'} hidden — ${esc(hidden.map((s) => shortName(s.name)).join(', '))}</a>` : ''}
    </div>`;
}

function widgetRuns() {
  const p = settlingPlugin();
  if (!p) {
    return `<a class="widget" href="#platform">
        <span class="w-title">Recent runs</span>
        <span class="w-value">—</span>
        ${widgetLine('No platform that settles has registered, so nothing has run.', 'faint')}
        <span class="w-foot">Platform →</span>
      </a>`;
  }
  const logName = p.plugin.settlement.runs_log;
  const href = serviceHref(p.id, logName);
  const r = state.runs;
  const shell = (value, lines) => `<a class="widget" href="${esc(href)}">
      <span class="w-title">Recent runs</span>${value}${lines}
      <span class="w-foot">All runs →</span>
    </a>`;
  if (!isUp(p)) return shell('<span class="w-value">—</span>', widgetLine(`${esc(p.name)} is not running, so its runs cannot be read.`, 'faint'));
  if (!r) return shell('<span class="w-value">…</span>', '');
  if (r.error) return shell('<span class="w-value">unknown</span>', widgetLine(esc(r.error), 'faint mono'));
  const log = (r.logs || []).find((l) => l.name === logName);
  const events = (log && log.events) || [];
  if (!events.length) return shell('<span class="w-value">0 runs</span>', widgetLine(esc(emptyLogHint(p, logName)), 'faint'));
  const labels = log.labels || {};
  const recent = events.slice(0, 12);
  const n = (k) => recent.filter((e) => (e.status || 'ok') === k).length;
  // Newest on the right, like the sparkline: history reads left to right.
  const bars = recent.slice().reverse().map((e) => `<i class="${esc(e.status || 'ok')}" title="${esc(e.summary)}"></i>`).join('');
  const parts = [`${n('ok')} clean`]
    .concat(n('warn') ? [`<span class="pill warn">${n('warn')} ${esc(labels.warn || 'refused')}</span>`] : [])
    .concat(n('bad') ? [`<span class="pill bad">${n('bad')} ${esc(labels.bad || 'failed')}</span>`] : []);
  return shell(`<span class="w-value">${log.total} run${log.total === 1 ? '' : 's'}</span>`,
    `<span class="w-bars" aria-label="last ${recent.length} runs">${bars}<small>last ${recent.length}</small></span>` +
    widgetLine(parts.join(' ')) +
    widgetLine(`${esc(events[0].summary)} — ${ago(events[0].at)} ago`, 'faint clamp'));
}

function widgetLinks() {
  const p = platformPlugin();
  const links = [
    ['#config/connect', 'Connect your platform', 'every vendor’s .env and keys'],
    ['/v1/connect/env', 'Download fintechlab.env', 'the whole lab, one file', true],
    ['#vendors/worldline', 'Cut a settlement file', 'Worldline’s morning cycle'],
    ['#vendors/banking-circle', 'Notification subscriptions', 'who Banking Circle calls'],
    ['#merchants', 'Merchants and outlets', 'what the money is for'],
  ];
  return `<div class="widget wide">
      <span class="w-title">Quick links</span>
      <span class="w-links">${links.map(([href, label, note, dl]) =>
        `<a href="${esc(href)}"${dl ? ' download' : ''}>${esc(label)}<small>${esc(note)}</small></a>`).join('')}
        ${p && p.browse_url ? `<a href="${esc(p.browse_url)}" target="_blank" rel="noreferrer">Open ${esc(p.name)} ↗<small class="mono">${esc(p.browse_url)}</small></a>` : ''}
      </span>
    </div>`;
}

/* --- the hop drawer ------------------------------------------------------

   Click an arrow, and the traffic behind it opens beside the diagram: the
   events the server counted for that arrow, newest first, refreshed every
   second while it is open (design-system.md, Hop drawer). A panel, not a
   modal — the diagram keeps animating next to it, and another arrow
   switches it. */

const evKey = (e) => `${e.seq}|${e.at}`;

function setHop(id, updateHash = true) {
  if ((id || null) === state.hop) return;
  state.hop = id || null;
  state.hopData = null;
  state.hopSeen = null;
  clearInterval(state.hopTimer);
  state.hopTimer = null;
  if (state.hop) {
    loadHop();
    // Watching a hop is the reason to poll fast; the rest of the dashboard
    // keeps its own rate.
    state.hopTimer = setInterval(() => { if (!document.hidden) loadHop(); }, FLOW_LIVE_MS);
  }
  if (updateHash && state.view === 'dashboard') {
    history.replaceState(null, '', state.hop ? `#dashboard/${encodeURIComponent(state.hop)}` : '#dashboard');
  }
  if (state.view === 'dashboard') renderView();
}

async function loadHop() {
  const id = state.hop;
  if (!id) return;
  let data;
  try {
    data = await api('GET', `/v1/flow/hops/${encodeURIComponent(id)}`);
  } catch (err) {
    data = { error: err.message };
  }
  if (state.hop !== id) return; // switched while this was in flight
  if (!state.hopSeen && data.events) state.hopSeen = new Set(data.events.map(evKey));
  state.hopData = data;
  renderDrawer();
}

const STAGE_PILL = { COMPLETED: 'ok', FAILED: 'bad' };

/* A drawer row is stacked: the activity panel's four columns do not fit
   420px, and the summary column ends up a letter a line. */
function drawerRow(e, isNew) {
  const detail = e.detail || {};
  const keys = Object.keys(detail).filter((k) => k !== 'status').sort();
  const at = new Date(e.at);
  return `<div class="d-row ${esc(e.status || 'ok')}${isNew ? ' is-new' : ''}">
      <div class="d-meta">
        <span class="mono" title="${esc(e.at)}">${esc(shortStamp(e.at))}</span>
        <span>${Number.isNaN(at.getTime()) ? '' : `${ago(e.at)} ago`}</span>
        ${e.op ? `<span class="mono">${esc(e.op)}</span>` : ''}
        ${e.peer ? `<span class="mono">${esc(e.peer)}</span>` : ''}
      </div>
      <div class="d-summary">${esc(e.summary || '')}</div>
      ${keys.length ? `<dl class="d-detail">${keys.map((k) =>
        `<dt>${esc(k)}</dt><dd>${esc(detail[k])}</dd>`).join('')}</dl>` : ''}
    </div>`;
}

/* HH:MM:SS today; the date too when it is not. */
function shortStamp(iso) {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  const time = d.toTimeString().slice(0, 8);
  if (d.toDateString() === new Date().toDateString()) return time;
  return `${d.toLocaleDateString('en-GB', { day: 'numeric', month: 'short' })} ${time}`;
}

function renderDrawer() {
  const el = $('#drawer');
  const open = !!state.hop && state.view === 'dashboard';
  if (!open) {
    el.hidden = true;
    el.innerHTML = '';
    return;
  }
  const d = state.hopData;
  const flowData = state.data.flow || {};
  const step = (d && d.step) || (flowData.steps || []).find((s) => s.id === state.hop) || { id: state.hop, label: state.hop };
  const who = (pid) => {
    const p = (flowData.participants || []).find((x) => x.id === pid);
    return shortName(p ? p.label : pid);
  };
  const route = step.from === step.to ? `inside ${who(step.from)}` : `${who(step.from)} → ${who(step.to)}`;
  const src = step.source && services().find((s) => s.id === step.source);
  const srcHref = step.source ? serviceHref(step.source, step.source_log) : '';

  let pills = '';
  let body = '<div class="empty">Reading…</div>';
  if (d && d.error) {
    body = `<div class="note bad">This hop could not be read — ${esc(d.error)}</div>`;
  } else if (d && d.stages) {
    const reached = d.stages.filter((s) => s.status).length;
    pills = `<span class="pill">${reached} of ${d.stages.length} reached</span>`;
    body = `<table><thead><tr><th>Stage</th><th class="num">Status</th></tr></thead><tbody>${d.stages.map((s) => `<tr>
        <td class="mono">${esc(s.stage)}</td>
        <td class="num">${s.status
          ? `<span class="pill ${STAGE_PILL[s.status] || ''}">${esc(s.status.toLowerCase())}</span>`
          : '<span class="hint">not reached</span>'}</td></tr>`).join('')}</tbody></table>
      <p class="hint">Stages of the platform's ${flowData.run && !flowData.run.finished_at ? 'run in flight' : 'last run'}, in the order it declared them.</p>`;
  } else if (d) {
    const evs = d.events || [];
    const failed = evs.filter((e) => e.status === 'bad').length;
    const refused = evs.filter((e) => e.status === 'warn').length;
    pills = [`<span class="pill">${step.count}${step.capped ? '+' : ''} total</span>`]
      .concat(refused ? [`<span class="pill warn">${refused} refused</span>`] : [])
      .concat(failed ? [`<span class="pill bad">${failed} failed</span>`] : [])
      .join(' ');
    const seen = state.hopSeen || new Set();
    body = evs.length
      ? `<div class="d-rows">${evs.map((e) => drawerRow(e, !seen.has(evKey(e)))).join('')}</div>
        ${step.count > evs.length ? `<div class="log-foot">Showing the newest ${evs.length} of ${step.count}${step.capped ? '+' : ''}.</div>` : ''}`
      : `<div class="empty">Nothing has come this way yet. It lands here the moment it does — this refreshes every second while it is open.</div>`;
  }

  const prev = el.querySelector('.drawer-body');
  const scroll = prev ? prev.scrollTop : 0;
  el.hidden = false;
  el.innerHTML = `<div class="drawer-head">
      <div class="drawer-title">
        <span class="w-title">Hop · live</span>
        <h2>${esc(step.label)}</h2>
        <span class="drawer-route">${esc(route)}</span>
      </div>
      <button class="ghost small" data-close aria-label="Close">×</button>
    </div>
    <div class="drawer-body">
      ${pills ? `<div class="pill-row">${pills}</div>` : ''}
      ${step.note ? `<div class="note">${esc(step.note)}</div>` : ''}
      ${srcHref ? `<p class="hint"><a href="${esc(srcHref)}">Open ${esc(src ? shortName(src.name) : step.source)}${step.source_log ? ` — its ${esc(step.source_log)} log` : ''} →</a></p>` : ''}
      ${body}
    </div>`;
  el.querySelector('.drawer-body').scrollTop = scroll;
  el.querySelector('[data-close]').addEventListener('click', () => setHop(null));
}

/* Arrows open their drawer; boxes go to their service's card. Both are
   reachable from the keyboard. */
function bindFlow(root) {
  const activate = (el, fn) => {
    el.addEventListener('click', fn);
    el.addEventListener('keydown', (ev) => {
      if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); fn(); }
    });
  };
  root.querySelectorAll('[data-hop]').forEach((g) => activate(g, () => {
    setHop(g.dataset.hop === state.hop ? null : g.dataset.hop);
  }));
  root.querySelectorAll('.flow-box[data-href]').forEach((g) => activate(g, () => {
    location.hash = g.dataset.href;
  }));
}

/* The sidebar folds to its icon rail at any width, remembered per browser. */
function setRail(on) {
  $('.app').classList.toggle('is-rail', on);
  const label = on ? 'Expand the sidebar' : 'Collapse the sidebar';
  for (const btn of [$('#rail-toggle'), $('#brand-mark')]) {
    btn.title = label;
    btn.setAttribute('aria-label', label);
  }
  try { localStorage.setItem('console-rail', on ? '1' : '0'); } catch (_) { /* private window */ }
}

/* --- docs -----------------------------------------------------------------

   How the pieces fit, for someone who has cloned one of them and wonders
   what the other two are (design-system.md, Guide). Static: nothing here
   is fetched, so it reads the same with the lab half up — except the one
   line that says what this lab can see right now. */

const GUIDE_PARTS = [
  {
    id: 'platform',
    name: 'The app in test',
    tag: 'your platform + its test harness',
    where: '~/infinite/buddy · infinite-local-runner/',
    what: 'Your platform, run locally — for buddy the settle path: orchestrator, SQS workers, gateway, the Banking Circle webhook app. Inside it lives the test harness: one command that supervises those processes, drives a whole settlement run, and puts a card for all of it in this console.',
    needs: [
      'Node ≥ 22 and pnpm; Podman for its own postgres and queues',
      'the lab’s keys — FINTECH_SIM_LAB, default ~/gh/fintechlab-simple/keys',
      'fintechlab-runner, linked by init/link-lab-runner.sh',
      'ports 3109 (control) and 3114 (webhooks) free',
      'optional fintechlab.json: which stand-ins to switch off',
    ],
    cmd: 'podman compose up -d && ./setup.sh\npnpm nx up infinite-local-runner',
    see: 'Platform → Local runner, registered and green',
    href: '#platform',
  },
  {
    id: 'runner',
    name: 'fintechlab-runner',
    tag: 'the library the harness is built on',
    where: '~/gh/fintechlab-runner',
    what: 'The lab’s plugin contract as typed code, so a harness only writes what is its own: register the card and keep it alive, follow the lab clock (and hold it during a run), the clock shim, logs in the lab’s shape, a store for uploaded files, a process supervisor. TypeScript, no runtime dependencies.',
    needs: [
      'Node ≥ 20 and npm',
      'npm install && npm run build — the harness loads dist/',
      'nothing from the lab at build time: the contract is in its types',
    ],
    cmd: 'git clone … ~/gh/fintechlab-runner\ncd ~/gh/fintechlab-runner && npm install && npm run build',
    see: 'the harness starts; without it: “@fintechlab/runner is not linked”',
    href: '',
  },
  {
    id: 'lab',
    name: 'fintechlab',
    tag: 'this lab: the vendors, the console, the clock',
    where: '~/gh/fintechlab-simple',
    what: 'The third parties, simulated with their real protocols — Worldline over SFTP+PGP, B4B with RS512 JWTs, Banking Circle with mTLS and encrypted webhook batches, ACI — plus the lab API every client calls, this console, the one business clock everyone follows, and the stand-ins that played the platform before one plugged in.',
    needs: [
      'Go 1.22+, Podman or Docker, openssl, make',
      'ports 8080–8097, 8443 and 2222 free',
      'nothing from the platform: it waits for one to register',
    ],
    cmd: 'git clone https://github.com/webduvet/fintechlab-simple.git\ncd fintechlab-simple && make up',
    see: 'Dashboard → Services, every vendor green',
    href: '#vendors',
  },
];

const GUIDE_STEPS = [
  { part: 'lab', title: 'Start the lab', cmd: 'cd ~/gh/fintechlab-simple\nmake up', see: 'The console on :8090, every vendor green. Keys and certificates are generated into keys/ the first time.', href: '#vendors', link: 'Vendors' },
  { part: 'runner', title: 'Build the runner library', cmd: 'cd ~/gh/fintechlab-runner\nnpm install && npm run build', see: 'dist/ built. Rebuild after changing it — the harness loads what is built.', href: '', link: '' },
  { part: 'platform', title: 'Set the harness up once', cmd: 'cd ~/infinite/buddy/infinite-local-runner\npodman compose up -d && ./setup.sh', see: 'setup.sh links the library, writes .env from the lab’s keys, creates queues and seeds merchants. A platform that does not read the lab’s disk takes one .env instead.', href: '#config/connect', link: 'Connect your platform' },
  { part: 'platform', title: 'Choose your stand-ins (optional)', cmd: 'cp fintechlab.example.json fintechlab.json', see: 'On registration the lab disconnects and hides its settlement service and webhook receiver: what lands in the vendors’ logs is then yours alone.', href: '#platform', link: 'Platform' },
  { part: 'platform', title: 'Start the platform', cmd: 'cd ~/infinite/buddy\npnpm nx up infinite-local-runner', see: '“🧩 registered with the lab console” and “⏰ following the lab clock” in its log; its card heads the Platform view.', href: platformHrefOrPlatform, link: 'its card' },
  { part: 'lab', title: 'Run, watch, reconcile', cmd: 'Run a settlement file — or upload your own — on its card.\nLab clock +1 hour, then Run reconciliation sweep.', see: 'The Dashboard draws every hop as it happens; click an arrow to watch its traffic. The sweep moves payouts to SUCCESS.', href: '#dashboard', link: 'Dashboard' },
];

function platformHrefOrPlatform() { return platformHref(); }

const GUIDE_LAYOUTS = [
  {
    name: 'All on your laptop',
    note: 'The default, and what the steps above build.',
    lines: ['platform and lab on one machine', 'the lab API reaches the platform at host.containers.internal:3109', 'the platform follows the lab clock, so a Sunday is a Sunday everywhere'],
  },
  {
    name: 'Lab elsewhere, platform local',
    note: 'A shared lab on a server (docs/deploy-pod.md).',
    lines: ['FINTECH_SIM_LAB_CONSOLE_URL and _CLOCK_URL point at the lab', 'registering and the clock are outbound — no tunnel needed', 'the card’s live parts need one: RUNNER_ADVERTISED_URL = a tunnel (cloudflared, ngrok, pinggy) to :3109'],
  },
  {
    name: 'Platform deployed, lab elsewhere',
    note: 'A real stack pointed at the lab.',
    lines: ['it registers with clock: "wall" — it cannot be moved', 'so the lab API refuses to move the lab clock while it is live', 'point its vendor URLs at the lab with the downloaded .env'],
  },
];

/* Two pages in one view (design-system.md, API reference): the guide and
   the API reference, picked by the hash — #docs, #docs/api,
   #docs/api:banking-circle with that service's card open. */
function docsPage() {
  return parseHash().item.split(':')[0] === 'api' ? 'api' : 'guide';
}

function renderDocs() {
  const page = docsPage();
  const link = (id, href, label) =>
    `<a class="doc-page${page === id ? ' is-active' : ''}" href="${href}"${page === id ? ' aria-current="page"' : ''}>${label}</a>`;
  return `<nav class="doc-pages" aria-label="Docs pages">
      ${link('guide', '#docs', 'Guide')}
      ${link('api', '#docs/api', 'API reference')}
    </nav>
    ${page === 'api' ? renderApiRef() : renderGuide()}`;
}

function renderGuide() {
  const ov = state.data.overview;
  const plugin = ov && platformPlugin();
  const up = ov ? services().filter((s) => isUp(s)).length : null;
  const now = ov
    ? `<div class="note">Right now: ${up} of ${services().filter((s) => s.health_path).length} services answer,
        and ${plugin ? `<a href="${esc(serviceHref(plugin.id))}">${esc(plugin.name)}</a> is registered as the platform under test` : 'no platform has registered yet — steps 2 to 5 below'}.</div>`
    : '';
  return `<div class="guide">
    ${now}
    <section class="guide-hero" data-anchor="overview">
      <h2>Three parts, two directions</h2>
      <p class="guide-lede">The lab is the world your platform talks to. Your platform talks to it
        exactly as it would to the real vendors — and, through its test harness, tells the lab it is
        there: it puts its own card in this console and keeps its business clock on the lab’s.
        The runner library is what makes the second half a few lines instead of a few hundred.</p>
      <div class="guide-diagram">${guideDiagram()}</div>
      <p class="hint">Solid arrows are what happens while it runs; the dashed one is set-up.
        Purple marks the system under test, as on the Dashboard.</p>
    </section>

    <section data-anchor="parts">
      <div class="section-title">The parts</div>
      <div class="guide-parts">${GUIDE_PARTS.map(guidePart).join('')}</div>
    </section>

    <section data-anchor="setup">
      <div class="section-title">Setting it up <small>once through, in this order</small></div>
      <ol class="guide-steps">${GUIDE_STEPS.map(guideStep).join('')}</ol>
    </section>

    <section data-anchor="layouts">
      <div class="section-title">Where each part can run</div>
      <div class="guide-layouts">${GUIDE_LAYOUTS.map((l) => `<div class="guide-card">
          <h3>${esc(l.name)}</h3>
          <p class="hint">${esc(l.note)}</p>
          <ul>${l.lines.map((x) => `<li>${esc(x)}</li>`).join('')}</ul>
        </div>`).join('')}</div>
    </section>

    <section data-anchor="reference">
      <div class="section-title">Further reading <small>in the repos</small></div>
      <div class="table-scroll"><table>
        <thead><tr><th>Document</th><th>For</th></tr></thead>
        <tbody>
          <tr><td class="mono">fintechlab-simple/docs/getting-started.md</td><td>the loop above, by hand, with what each screen should show</td></tr>
          <tr><td class="mono">fintechlab-simple/docs/plugins.md</td><td>the contract a platform implements: the card, the clock, files, stand-ins</td></tr>
          <tr><td class="mono">fintechlab-simple/docs/api/README.md</td><td>every API in the lab, who calls which, and the full reference — <a href="#docs/api">API reference</a></td></tr>
          <tr><td class="mono">fintechlab-simple/agent.runbooks.md</td><td>every step as one curl, and what looks broken but is not</td></tr>
          <tr><td class="mono">fintechlab-simple/docs/console.md</td><td>this console, view by view</td></tr>
          <tr><td class="mono">fintechlab-runner/README.md</td><td>the library’s modules and a minimal platform</td></tr>
          <tr><td class="mono">buddy/infinite-local-runner/README.md</td><td>the reference harness</td></tr>
        </tbody></table></div>
    </section>
  </div>`;
}

function guidePart(p) {
  return `<div class="guide-card part-${esc(p.id)}">
      <div class="guide-card-head"><span class="guide-badge part-${esc(p.id)}"></span>
        <div><h3>${esc(p.name)}</h3><span class="hint">${esc(p.tag)}</span></div></div>
      <p>${esc(p.what)}</p>
      <div class="w-title">Needs</div>
      <ul>${p.needs.map((n) => `<li>${esc(n)}</li>`).join('')}</ul>
      <div class="w-title">Lives in</div>
      <p class="mono">${esc(p.where)}</p>
      <pre class="guide-cmd">${esc(p.cmd)}</pre>
      <p class="hint">Then: ${p.href ? `<a href="${esc(p.href)}">${esc(p.see)}</a>` : esc(p.see)}</p>
    </div>`;
}

function guideStep(s, i) {
  const part = GUIDE_PARTS.find((p) => p.id === s.part);
  const href = typeof s.href === 'function' ? s.href() : s.href;
  return `<li class="guide-step">
      <span class="guide-num part-${esc(s.part)}">${i + 1}</span>
      <div class="guide-step-body">
        <h3>${esc(s.title)} <span class="pill">${esc(part ? part.name : s.part)}</span></h3>
        <pre class="guide-cmd">${esc(s.cmd)}</pre>
        <p>${esc(s.see)}${href && s.link ? ` <a href="${esc(href)}">${esc(s.link)} →</a>` : ''}</p>
      </div>
    </li>`;
}

/* Hand-drawn, like the sequence diagram: tokens for every colour, so it
   is right in both themes, and no library. The guide and the API reference
   draw with the same three parts. */
function gdBox(x, y, w, h, klass, title, sub) {
  return `<g class="gd-box ${klass}">
      <rect x="${x}" y="${y}" width="${w}" height="${h}" rx="10"></rect>
      <text class="gd-title" x="${x + 14}" y="${y + 22}">${esc(title)}</text>
      ${sub ? `<text class="gd-sub" x="${x + 14}" y="${y + 38}">${esc(sub)}</text>` : ''}
    </g>`;
}

function gdChip(x, y, w, label, klass = '') {
  return `<g class="gd-chip ${klass}">
      <rect x="${x}" y="${y}" width="${w}" height="24" rx="6"></rect>
      <text x="${x + w / 2}" y="${y + 16}">${esc(label)}</text>
    </g>`;
}

function gdArrow(x1, y, x2, label, sub, klass = '') {
  const dir = x2 > x1 ? 1 : -1;
  const tip = x2 - 9 * dir;
  return `<g class="gd-arrow ${klass}">
      <line x1="${x1}" y1="${y}" x2="${tip}" y2="${y}"></line>
      <polygon points="${x2},${y} ${tip},${y - 5} ${tip},${y + 5}"></polygon>
      <text class="gd-label" x="${(x1 + x2) / 2}" y="${y - 7}">${esc(label)}</text>
      ${sub ? `<text class="gd-code" x="${(x1 + x2) / 2}" y="${y + 15}">${esc(sub)}</text>` : ''}
    </g>`;
}

function guideDiagram() {
  const box = gdBox;
  const chip = gdChip;
  const arrow = gdArrow;
  return `<svg class="gd" viewBox="0 0 1000 470" role="img"
      aria-label="The platform with its test harness and the runner library on the left, the lab on the right, and the calls between them">
    ${box(10, 10, 390, 450, 'is-sut', 'The app in test', 'your platform, run locally')}
    ${chip(30, 64, 170, 'orchestrator')}
    ${chip(210, 64, 170, 'SQS workers')}
    ${chip(30, 96, 170, 'gateway (callbacks)')}
    ${chip(210, 96, 170, 'webhook app :3114')}
    ${box(30, 150, 350, 290, 'is-harness', 'Test harness', 'infinite-local-runner · control API :3109')}
    <text class="gd-note" x="46" y="212">supervises the processes above</text>
    <text class="gd-note" x="46" y="230">drives a settlement run end to end</text>
    <text class="gd-note" x="46" y="248">keeps uploaded files, previews them</text>
    ${box(46, 290, 318, 130, 'is-lib', 'fintechlab-runner', 'library · linked into node_modules')}
    ${chip(62, 342, 90, 'card', 'lib')}
    ${chip(160, 342, 90, 'clock', 'lib')}
    ${chip(258, 342, 90, 'logs', 'lib')}
    ${chip(62, 374, 90, 'shim', 'lib')}
    ${chip(160, 374, 90, 'files', 'lib')}
    ${chip(258, 374, 90, 'supervisor', 'lib')}

    ${box(600, 10, 390, 450, 'is-lab', 'fintechlab', 'the lab: vendors, its API, console, clock')}
    ${chip(620, 64, 170, 'Worldline SFTP :2222')}
    ${chip(800, 64, 170, 'B4B :8086')}
    ${chip(620, 96, 170, 'Banking Circle :8085')}
    ${chip(800, 96, 170, 'ACI :8087')}
    ${chip(620, 128, 350, 'verification · AML · business bank :8081')}
    ${box(620, 160, 170, 76, 'is-core', 'Lab API :8097', 'cards · buttons · logs')}
    ${chip(840, 186, 130, 'console :8090', 'browser')}
    ${arrow(840, 198, 790, '', '', 'is-browse')}
    ${box(620, 254, 170, 44, 'is-core', 'Lab clock :8096', '')}
    <text class="gd-group" x="620" y="330">STAND-INS</text>
    ${chip(620, 340, 170, 'settlement', 'standin')}
    ${chip(800, 340, 170, 'webhook receiver', 'standin')}
    <text class="gd-note" x="620" y="386">played the platform before one plugged in —</text>
    <text class="gd-note" x="620" y="402">hidden by default; Configuration → Stand-ins</text>

    ${arrow(380, 76, 620, 'SFTP+PGP · JWT · mTLS', '', 'is-traffic')}
    ${arrow(620, 108, 380, 'webhooks · callbacks', '', 'is-traffic')}
    ${arrow(380, 180, 620, 'registers its card, renews it', 'POST /v1/plugins')}
    ${arrow(620, 222, 380, 'buttons, files, logs', 'on its base_url, :3109')}
    ${arrow(380, 276, 620, 'follows the clock, holds it in a run', 'GET /clock')}
    ${arrow(600, 436, 400, 'fintechlab.env', 'addresses · credentials · keys', 'is-setup')}
  </svg>`;
}

/* --- docs: the API reference ----------------------------------------------

   Every route a caller can use (design-system.md, API reference). The lab
   API's routes come from GET /v1, which lists the table the lab API
   registers them from, so this page cannot document a route that does not
   answer. The services come from the overview, like every card. */

/* Where a script reaches the lab API. This page reaches it through its own
   origin; the lab API's own address is :8097 on the same host. */
function labApiURL() {
  return `${location.protocol}//${location.hostname}:8097`;
}

const API_INTENTS = [
  ['Drive the lab from a script or an agent: merchants, the clock, a run, what happened',
    'The lab API', 'GET /v1 first, then /v1/*'],
  ['Make a vendor misbehave, or read what it saw',
    'The lab API’s passthrough', '/v1/services/{id}/sim/* — on the service’s control port'],
  ['Decide how a future payout ends, and when',
    'A trap, in the vendor that applies it', 'POST /v1/services/{id}/sim/traps — GET /v1/traps/hooks first'],
  ['Test your integration with one vendor',
    'That vendor’s own surface', 'exactly the calls your platform makes to the real one'],
  ['Put your platform’s card in the console',
    'The lab API', 'POST /v1/plugins — docs/plugins.md'],
  ['Keep your platform’s business dates on the lab’s',
    'The clock', 'GET /clock every second — docs/plugins.md, The clock'],
  ['Prove every hop is connected',
    'make e2e', 'plays the platform on the vendor surfaces, and steers the lab through the lab API'],
];

const API_KIND_TITLES = [
  ['vendor', 'Vendors'],
  ['verification', 'Verification'],
  ['platform', 'Platform, clock and stand-ins'],
];

function renderApiRef() {
  const idx = state.data.apiIndex;
  return `<div class="guide api-ref">
    <section class="guide-hero">
      <h2>One lab API, and the services behind it</h2>
      <p class="guide-lede">The <b>lab API</b> is the one address for everything lab-level: merchants,
        the clock, settlement runs, what happened, and every service's lab hooks. This page calls
        it exactly as a script or an agent does — the console only serves the page and forwards
        <span class="mono">/v1</span> to it. Behind it, every service has two ports. Its
        <b>vendor port</b> carries the vendor's own API behind the vendor's own login — your platform
        calls it exactly as it would call the real vendor — and <b>/internal/*</b> plumbing where one
        simulation stands in for something that has no API. Its <b>control port</b>, the vendor port
        plus 1000, carries <b>/sim/*</b>, the lab-only hooks that drive and observe it, with no vendor
        login: only the lab API calls it, and the lab API holds no vendor's credentials.</p>
      <div class="guide-diagram">${apiDiagram()}</div>
      <p class="hint">Purple arrows go to or from the lab API; grey ones are calls to a service's own API.
        Purple marks the system under test, as on the Dashboard.</p>
    </section>

    <section data-anchor="which">
      <div class="section-title">Which to call</div>
      <div class="table-scroll"><table>
        <thead><tr><th>To</th><th>Call</th><th>How</th></tr></thead>
        <tbody>${API_INTENTS.map(([to, call, how]) => `<tr>
          <td>${esc(to)}</td><td>${esc(call)}</td><td class="mono">${esc(how)}</td></tr>`).join('')}</tbody>
      </table></div>
      <div class="note">A <b>trap</b> decides something about a future event in one simulation — this
        payout is rejected, that one processed and reversed ten seconds later. Arm one where it applies,
        through the passthrough (<span class="mono">POST /v1/services/{id}/sim/traps</span>), after reading
        what can be trapped (<span class="mono">GET /v1/traps/hooks</span>); read and disarm them across the lab
        at <span class="mono">/v1/traps</span>.</div>
    </section>

    <section data-anchor="lab-api">
      <div class="section-title">The lab API <small>${esc(labApiURL())}/v1 — and through this console at ${esc(location.origin)}/v1</small></div>
      ${apiLabRoutes(idx)}
    </section>

    <section data-anchor="service-apis">
      <div class="section-title">Each service's API <small>where a script on this machine reaches it, and the routes worth knowing</small></div>
      <p class="hint">The full reference for each, with bodies and answers, is in the repo:
        <span class="mono">docs/api/services/&lt;id&gt;.md</span>, indexed by <span class="mono">docs/api/README.md</span>.</p>
      ${apiServiceCards()}
    </section>
  </div>`;
}

function apiLabRoutes(idx) {
  if (!idx) return `<div class="empty">Reading the lab API's index…</div>`;
  if (idx.error) return `<div class="note bad">GET /v1 did not answer: ${esc(idx.error)}</div>`;
  const groups = (idx.groups || []).map((g) => {
    const rows = (idx.routes || []).filter((r) => r.group === g.id);
    if (!rows.length) return '';
    return `<div class="api-group">
        <h3>${esc(g.title)} <span class="pill">${rows.length}</span></h3>
        <p class="hint">${esc(g.note)}</p>
        <div class="table-scroll"><table class="api-routes is-console">
          <colgroup><col class="c-method"><col class="c-path"><col class="c-what"><col></colgroup>
          <thead><tr><th>Method</th><th>Path</th><th>What it does</th><th>Reaches</th></tr></thead>
          <tbody>${rows.map((r) => `<tr>
            <td><span class="pill">${esc(r.method)}</span></td>
            <td class="mono">${esc(r.path)}${r.alias ? `<div class="hint mono">also ${esc(r.alias)}</div>` : ''}</td>
            <td>${esc(r.summary)}</td>
            <td class="api-reaches">${esc(r.reaches)}</td></tr>`).join('')}</tbody>
        </table></div>
      </div>`;
  }).join('');
  const aliased = (idx.routes || []).filter((r) => r.alias).length;
  return `<p class="hint">${(idx.routes || []).length} routes, listed by the lab API itself
      (<a href="/v1" target="_blank" rel="noreferrer" class="mono">GET /v1</a>; as OpenAPI 3.1 for a code generator or an
      agent's tools, <a href="/v1/openapi.json" target="_blank" rel="noreferrer" class="mono">GET /v1/openapi.json</a>).
      ${aliased} also answer at the console's old <span class="mono">/api</span> path, for plugins that
      still register there. No auth unless <span class="mono">LAB_API_TOKEN</span> is set; JSON in and out.
      Bodies, answers and errors for each: <span class="mono">${esc(idx.docs || 'docs/api/lab-api.md')}</span>.</p>
    ${groups}`;
}

function apiServiceCards() {
  const all = services();
  if (!all.length) return `<div class="empty">${esc(state.error || 'Loading…')}</div>`;
  return API_KIND_TITLES.map(([kind, title]) => {
    const group = all.filter((s) => s.kind === kind);
    if (!group.length) return '';
    return `<div class="api-group"><h3>${esc(title)}</h3>${group.map(apiServiceCard).join('')}</div>`;
  }).join('');
}

function apiServiceCard(s) {
  const id = `api:${s.id}`;
  const open = isOpen(id);
  const st = s.status || {};
  const where = s.browse_url || s.base_url || '';
  const body = !open ? '' : `<div class="card-body">
      <div class="note">${esc(s.summary)}</div>
      <dl class="kv">
        ${where ? `<dt>From this machine</dt><dd>${esc(where)}</dd>` : ''}
        ${s.base_url && s.base_url !== where ? `<dt>From the lab API</dt><dd>${esc(s.base_url)}</dd>` : ''}
        ${s.control_url ? `<dt>Control port</dt><dd>${esc(s.control_url)} <span style="font-family:var(--sans);color:var(--text-faint)">— /sim/*, no vendor login; the lab API's way in</span></dd>` : ''}
        <dt>Ports</dt><dd>${esc((s.ports || []).join('  '))}</dd>
        ${s.transport ? `<dt>Transport</dt><dd>${esc(s.transport)}</dd>` : ''}
        ${s.auth ? `<dt>Auth</dt><dd>${esc(s.auth)}</dd>` : ''}
        ${s.health_path ? `<dt>Health</dt><dd>GET ${esc(s.health_path)}</dd>` : ''}
        ${s.activity_path ? `<dt>Activity</dt><dd>GET ${esc(s.activity_path)}</dd>` : ''}
        <dt>Full reference</dt><dd>${s.plugin
          ? '<span style="font-family:var(--sans)">its own — a platform documents itself</span>'
          : esc(`docs/api/services/${s.id}.md`)}</dd>
      </dl>
      ${(s.endpoints || []).length ? `<div class="table-scroll"><table class="api-routes">
        <thead><tr><th class="actions">Method</th><th>Path</th><th>What it does</th></tr></thead>
        <tbody>${s.endpoints.map((e) => `<tr>
          <td class="actions"><span class="pill-row">${String(e.method).split('|').map((m) => `<span class="pill">${esc(m)}</span>`).join('')}</span></td>
          <td class="mono">${esc(e.path)}</td>
          <td>${esc(e.note || '')}</td></tr>`).join('')}</tbody></table></div>`
        : `<p class="hint">It declares no endpoints.</p>`}
      <p class="hint">Its card, with its logs and buttons: <a href="${esc(serviceHref(s.id))}">${esc(s.name)} →</a></p>
    </div>`;
  return `<div class="card ${open ? 'is-open' : ''}">
    <div class="card-head" data-card="${esc(id)}">
      <span class="chev">▸</span>
      <i class="dot ${esc(st.state || 'unknown')}"></i>
      <div class="card-title">
        <span class="name">${esc(s.name)} <span class="pill ${esc(s.kind)}">${esc(s.kind)}</span>${s.plugin ? ' <span class="pill">plugin</span>' : ''}</span>
        <span class="desc mono">${esc(where)}${s.auth ? ` · ${esc(s.auth)}` : ''}</span>
      </div>
      <div class="card-meta"><span>${(s.endpoints || []).length} routes listed</span></div>
    </div>
    ${body}
  </div>`;
}

/* Who calls which API: callers on both sides of the lab, because two of
   them (scripts, the platform) call the console and the services both. */
function apiDiagram() {
  const vert = (x, y1, y2) => {
    const tip = y2 - 9;
    return `<g class="gd-arrow is-traffic">
      <line x1="${x}" y1="${y1}" x2="${x}" y2="${tip}"></line>
      <polygon points="${x},${y2} ${x - 5},${tip} ${x + 5},${tip}"></polygon>
    </g>`;
  };
  return `<svg class="gd" viewBox="0 0 1000 470" role="img"
      aria-label="Callers on both sides — the console page, scripts, agents and the e2e suite, the platform — and the lab API and services they call">
    ${gdBox(20, 20, 210, 60, 'is-lab', 'Console page', 'your browser · served on :8090')}
    ${gdBox(20, 110, 210, 230, 'is-lab', 'Scripts and agents', 'curl, an LLM, make e2e')}
    <text class="gd-note" x="36" y="180">read GET /v1 first:</text>
    <text class="gd-note" x="36" y="198">it lists every route</text>
    <text class="gd-note" x="36" y="216">and what each one reaches</text>
    <text class="gd-note" x="36" y="250">make e2e is one of them:</text>
    <text class="gd-note" x="36" y="268">the platform on the vendor</text>
    <text class="gd-note" x="36" y="286">surfaces, the rest via /v1</text>

    ${gdBox(380, 20, 240, 150, 'is-core', 'Lab API :8097', 'the one API over the lab')}
    ${gdChip(396, 76, 208, 'GET /v1 — the index')}
    ${gdChip(396, 108, 208, '/v1/services/{id}/sim/*')}
    ${gdChip(396, 140, 208, 'own state: merchants, plugins', 'standin')}

    ${gdBox(380, 200, 240, 260, 'is-lab', 'Services', 'one process each, own address')}
    ${gdChip(396, 256, 208, 'vendor surface')}
    ${gdChip(396, 288, 208, '/internal/* — plumbing')}
    ${gdChip(396, 320, 208, 'control port — /sim/*', 'lib')}
    ${gdChip(396, 352, 208, 'no vendor login there', 'lib')}
    ${gdChip(396, 400, 208, 'clock :8096 — GET /clock')}

    ${gdBox(770, 20, 210, 290, 'is-sut', 'Your platform', 'a registered plugin')}

    ${gdArrow(230, 50, 380, 'every button', 'one /v1 call')}
    ${gdArrow(230, 140, 380, 'everything lab-level', 'GET /v1 · /v1/*')}
    ${gdArrow(230, 300, 380, 'vendor surfaces', 'as a platform would', 'is-traffic')}
    ${vert(500, 170, 200)}
    <text class="gd-code gd-start" x="510" y="189">control ports only</text>

    <text class="gd-note" x="786" y="170">its production code calls</text>
    <text class="gd-note" x="786" y="188">the vendor surfaces only;</text>
    <text class="gd-note" x="786" y="206">its test harness may also</text>
    <text class="gd-note" x="786" y="224">use the lab API, like any script</text>

    ${gdArrow(770, 60, 620, 'registers its card', 'POST /v1/plugins')}
    ${gdArrow(620, 125, 770, 'its buttons, files, logs', 'on its base_url')}
    ${gdArrow(770, 240, 620, 'vendor traffic, the clock', 'SFTP+PGP · JWT · mTLS', 'is-traffic')}
    ${gdArrow(620, 285, 770, 'webhooks · callbacks', '', 'is-traffic')}
  </svg>`;
}

/* --- services ---------------------------------------------------------- */

/* Some cards earn the top of their list. A registered platform is the
   thing being tested and the thing with the buttons, then the lab clock
   that decides its day; below them the stand-ins are reference material. */
function cardRank(s) {
  if (s.plugin) return 0;
  return s.id === 'clock' ? 1 : 2;
}

function renderServices() {
  const ov = state.data.overview;
  if (!ov) return `<div class="empty">${esc(state.error || 'Loading…')}</div>`;
  const kind = VIEWS[state.view].kind;
  const all = ov.services.filter((s) => s.kind === kind);
  const hidden = all.filter(isHidden);
  const group = all.filter((s) => !isHidden(s));

  // Three tiles for this section, one for the lab. Splitting the list took
  // away the screen that answered "is everything up?", and that answer is
  // worth keeping somewhere you always are.
  const state_ = (s) => (s.status || {}).state;
  const probed = group.filter((s) => s.health_path);
  const labUp = ov.counts.up || 0;
  const labProbed = ov.services.filter((s) => s.health_path).length;
  let html = `<div class="stats">
    <div class="stat up"><b>${probed.filter((s) => state_(s) === 'up').length}</b><span>up</span></div>
    <div class="stat down"><b>${probed.filter((s) => state_(s) === 'down').length}</b><span>down</span></div>
    <div class="stat"><b>${probed.filter((s) => state_(s) !== 'up' && state_(s) !== 'down').length}</b><span>not reporting</span></div>
    <div class="stat accent"><b>${labUp}/${labProbed}</b><span>whole lab</span></div>
  </div>`;

  // The Platform view always says what would fill it (below), and which
  // stand-ins are hidden -- even with no card at all, as on a lab deployed
  // without a clock before a platform registers.
  if (!group.length && kind !== 'platform') return html + `<div class="empty">Nothing in this section yet.</div>`;

  const ordered = group.map((s, i) => [s, i])
    .sort((a, b) => cardRank(a[0]) - cardRank(b[0]) || a[1] - b[1])
    .map(([s]) => s);
  // The one thing the Platform view is for, said where it would be.
  const noClock = kind === 'platform' && !all.some((s) => s.id === 'clock')
    ? ' This lab was deployed without a lab clock, so there is no clock card either.'
    : '';
  const noPlatform = kind === 'platform' && !group.some((s) => s.plugin)
    ? `<div class="note">No platform has registered. A platform under test puts its own card here by
        posting a descriptor to the lab API's <span class="mono">/v1/plugins</span> and renewing it — see
        <span class="mono">docs/plugins.md</span>. It then heads this list, with its buttons.${noClock}</div>`
    : '';
  return html + noPlatform + ordered.map(serviceCard).join('') + hiddenNote(hidden);
}

function isHidden(s) { return !!(s.stand_in && !s.stand_in.shown); }

/* Hidden is said where the cards would have been (design-system.md,
   contract 10): which stand-ins are out of the way, how each is wired, and
   where the switches are. */
function hiddenNote(hidden) {
  if (!hidden.length) return '';
  const views = (state.standins && !state.standins.error && state.standins.stand_ins) || [];
  const said = hidden.map((s) => {
    const v = views.find((x) => x.id === s.id);
    const st = v && v.state !== 'unwired' ? ` <span class="pill${v.state === 'connected' ? ' ok' : v.state === 'unknown' ? '' : ' warn'}">${esc(v.state)}</span>` : '';
    return `<span class="nowrap">${esc(shortName(s.name))}${st}</span>`;
  });
  return `<div class="note">Lab stand-ins, kept out of the way: ${said.join(', ')}. They play the platform's part
      when none is plugged in — useful for developing a vendor without one, noise once yours is here.
      <a href="#config/stand-ins">Connect, disconnect or show them in Configuration →</a></div>`;
}

/* A stand-in's wiring and the two switches (design-system.md, Stand-in).
   Connected is read live from the services that own the wiring. */
function standInPanel(s) {
  if (!s.stand_in) return '';
  const all = state.standins;
  if (!all) return '<div class="note">Reading how this stand-in is wired…</div>';
  if (all.error) return `<div class="note bad">Stand-in wiring unavailable — ${esc(all.error)}</div>`;
  const v = (all.stand_ins || []).find((x) => x.id === s.id);
  if (!v) return '';
  const conns = v.connections || [];
  const rows = conns.map((c) => `<tr>
      <td>${esc(c.label)}</td>
      <td>${c.error ? `<span class="pill">unknown</span>` : c.connected == null ? '<span class="hint">not wired here</span>'
        : c.connected ? '<span class="pill ok">connected</span>' : '<span class="pill warn">disconnected</span>'}</td>
      <td class="hint">${esc(c.error || c.detail || '')}</td></tr>`).join('');
  const switchable = conns.some((c) => c.connected != null);
  const toggle = !switchable ? ''
    : v.state === 'connected' || v.state === 'partial'
      ? `<button class="ghost small" data-action="standin" data-id="${esc(s.id)}" data-connected="false">Disconnect</button>`
      : `<button class="ghost small" data-action="standin" data-id="${esc(s.id)}" data-connected="true">Connect</button>`;
  return `<div class="section-title">Stand-in</div>
    <p class="hint">${esc(v.plays || '')}</p>
    <div class="note">This service plays your platform's part until your platform does. Disconnect it
      once yours is plugged in: its timers stop and vendors stop delivering to it, so what lands
      in their logs is yours. Hiding takes the card off this view and out of the diagram.
      ${v.set_by ? `Last set by <span class="mono">${esc(v.set_by)}</span>.` : ''}</div>
    ${conns.length ? `<div class="table-scroll"><table>
      <thead><tr><th>Wiring</th><th>State</th><th>What it is</th></tr></thead>
      <tbody>${rows}</tbody></table></div>`
      : '<p class="hint">Nothing in the lab calls this service on its own, so there is nothing to disconnect — only to hide. Its log below shows whatever did call it.</p>'}
    <div class="form">${toggle}
      <button class="ghost small" data-action="standin" data-id="${esc(s.id)}" data-shown="false">Hide this card</button>
    </div>`;
}

function standInPill(s) {
  if (!s.stand_in) return '';
  const v = state.standins && !state.standins.error && (state.standins.stand_ins || []).find((x) => x.id === s.id);
  const st = v && v.state;
  return ` <span class="pill">stand-in</span>${st === 'disconnected' || st === 'partial' ? ` <span class="pill warn">${esc(st)}</span>` : ''}`;
}

function serviceCard(s) {
  const st = s.status || {};
  const open = isOpen(s.id);
  const latency = st.latency_ms != null && st.state === 'up' ? `${st.latency_ms}ms` : '';
  const stateLabel = st.state === 'not-probed' ? 'no health endpoint' : st.state;

  let body = '';
  if (open) {
    body = `<div class="card-body">
      <div class="note">${esc(s.summary)}</div>
      <dl class="kv">
        <dt>Reached at</dt><dd>${esc(s.base_url)}${s.health_path ? esc(s.health_path) : ''}</dd>
        ${s.control_url ? `<dt>Control port</dt><dd>${esc(s.control_url)}/sim/* <span style="font-family:var(--sans);color:var(--text-faint)">— where the lab steers it, no vendor login</span></dd>` : ''}
        <dt>Ports</dt><dd>${esc((s.ports || []).join('  '))}</dd>
        <dt>Transport</dt><dd>${esc(s.transport)}</dd>
        <dt>Auth</dt><dd>${esc(s.auth)}</dd>
        <dt>State</dt><dd>${esc(stateLabel)}${st.detail ? ' — ' + esc(st.detail) : ''}${st.since ? ` (for ${ago(st.since)})` : ''}</dd>
        ${s.swap_for ? `<dt>Swap for the real thing</dt><dd style="font-family:var(--sans)">${esc(s.swap_for)}</dd>` : ''}
        ${s.docs ? `<dt>Design doc</dt><dd>${esc(s.docs)}</dd>` : ''}
        ${s.plugin ? pluginRows(s.plugin) : ''}
      </dl>
      ${clockPanel(s)}
      ${standInPanel(s)}
      ${filesPanel(s)}
      ${subscriptionPanel(s)}
      ${payoutsPanel(s)}
      ${connectPanel(s)}
      ${activityPanels(s)}
      ${(s.endpoints || []).length ? `<div class="table-scroll"><table>
        <thead><tr><th>Method</th><th>Path</th><th>What it does</th></tr></thead>
        <tbody>${s.endpoints.map((e) => `<tr>
          <td class="mono">${esc(e.method)}</td>
          <td class="mono">${esc(e.path)}</td>
          <td>${esc(e.note || '')}</td></tr>`).join('')}</tbody></table></div>` : ''}
      ${serviceNote(s)}
      <div class="form">
        <button class="ghost small" data-action="probe" data-id="${esc(s.id)}">Probe now</button>
        ${s.browse_url ? `<a class="ghost small" href="${esc(s.browse_url)}" target="_blank" rel="noreferrer">Open ${esc(s.browse_url)}</a>` : ''}
        ${serviceExtras(s)}
      </div>
    </div>`;
  }

  return `<div class="card ${open ? 'is-open' : ''}">
    <div class="card-head" data-card="${esc(s.id)}">
      <span class="chev">▸</span>
      <i class="dot ${esc(st.state || 'unknown')}"></i>
      <div class="card-title">
        <span class="name">${esc(s.name)} <span class="pill ${esc(s.kind)}">${esc(s.kind)}</span>${
          s.plugin ? ` <span class="pill${s.plugin.state === 'live' ? '' : ' warn'}">${s.plugin.state === 'live' ? 'registered' : esc(s.plugin.state)}</span>` : ''}${standInPill(s)}</span>
        <span class="desc">${esc(s.summary)}</span>
      </div>
      <div class="card-meta">${spark(st.history)}<span>${esc(latency)}</span></div>
    </div>
    ${body}
  </div>`;
}

/* The two actions that drive the whole money flow, offered where the
   service that performs them is. Each is one call to that service's own
   endpoint -- nothing here is reachable only from this UI. */
/* A sentence where a button would be wrong. Prose belongs above the action
   row, not inside it: .form is a flex row of fields and a note dropped in
   between two buttons gets squeezed to nothing. */
function serviceNote(s) {
  if (s.plugin && !isUp(s)) {
    return `<div class="note warn">Not running, so there is nothing to drive.
      ${s.plugin.start_hint ? esc(s.plugin.start_hint) : 'Start it, and it registers this card again.'}</div>`;
  }
  return '';
}

/* How a plugin's registration stands, and whether it moves with the lab. */
function pluginRows(p) {
  const when = p.state === 'live'
    ? `live — renewed ${ago(p.last_seen)} ago, every ${Math.round(p.ttl_seconds / 3)}s`
    : `${p.state} — last heard ${ago(p.last_seen)} ago`;
  return `<dt>Registered</dt><dd>${esc(when)}</dd>
    <dt>Clock</dt><dd style="font-family:var(--sans)">${p.clock === 'wall'
      ? 'the wall clock — it cannot follow the lab, so the lab clock is not moved while it is registered'
      : 'follows the lab clock'}</dd>`;
}

function serviceExtras(s) {
  if (s.plugin) {
    // Offered only while the platform is up (design-system.md, Plugin card).
    if (!isUp(s)) return '';
    return (s.plugin.actions || []).map((act) =>
      `<button class="${act.primary ? 'btn' : 'ghost'} small" data-action="plugin-action"
        data-id="${esc(s.id)}" data-act="${esc(act.id)}">${esc(act.label)}</button>`).join('');
  }
  if (s.id === 'banking-circle') {
    return `<button class="ghost small" data-action="fund-sga">Fund safeguarding accounts</button>`;
  }
  if (s.id === 'worldline') {
    return `<button class="btn small" data-action="cycle" data-slot="morning">Run morning cycle (ER)</button>
            <button class="ghost small" data-action="cycle" data-slot="afternoon">Afternoon confirmation (AR)</button>`;
  }
  if (s.id === 'settlement') {
    return `<button class="btn small" data-action="pull">Pull from Worldline now</button>`;
  }
  return '';
}

/* --- pods -------------------------------------------------------------- */

/* A pod is a bundle of kernels behind one address. Collapsed, a card is the
   one-line claim; expanded, it is the wiring — which kernel listens to what,
   what each is actually configured with, and which routes exist.

   The schematic is drawn from the *running* pod where one answers, and from
   its recipe otherwise. Those are different facts and the card says which,
   because a diagram of what somebody intended is not a diagram of what is
   happening. */

// Boot phases, in order. Grouping the graph by phase is what makes it
// readable: bring-up order is also, roughly, dependency order.
const PHASES = [
  ['port', 'Ports', 'capabilities everything else is handed'],
  ['store', 'Stores', 'books of record'],
  ['transform', 'Transforms', 'codecs and file boundaries'],
  ['gate', 'Gates', 'decisions — they may block, they may not book'],
  ['orchestrate', 'Orchestrators', 'case and window drivers'],
  ['ingress', 'Ingress', 'the HTTP edge, up last so nothing is reachable early'],
  ['worker', 'Workers', 'senders and pollers'],
];

// The four-way "one truth" taxonomy each kernel declares.
const TRUTH_LABEL = {
  'decide': 'decides',
  'hold-state': 'holds state',
  'transform': 'transforms',
  'move-bytes': 'moves bytes',
};

function renderBanks() {
  const d = state.data.banks;
  if (!d) return `<div class="empty">${esc(state.error || 'Loading…')}</div>`;
  return d.banks.map(bankCard).join('');
}

function bankCard(b) {
  const open = isOpen(b.id);
  const total = b.accounts.length;
  let body = '';
  if (open) {
    // The same service as a card on Vendors; this is its accounts, that is
    // its health and its traffic. Said, so one service is not read as two.
    const svc = b.service_id ? services().find((x) => x.id === b.service_id) : null;
    body = `<div class="card-body">
      <div class="note">${esc(b.summary)}${svc ? ` This is its accounts; its health, endpoints and traffic are on
        <a href="${esc(serviceHref(svc.id))}">its ${esc(VIEWS[VIEW_OF_KIND[svc.kind]] ? VIEWS[VIEW_OF_KIND[svc.kind]].title : '')} card →</a>` : ''}</div>
      ${b.error ? `<div class="note bad">${esc(b.error)}</div>` : ''}
      ${total ? `<div class="table-scroll"><table>
        <thead><tr><th>Account</th><th>${esc(b.number_label)}</th><th>Holder</th><th>Ccy</th><th class="num">Balance</th></tr></thead>
        <tbody>${b.accounts.map((a) => `<tr>
          <td class="mono">${esc(a.id)}</td>
          <td class="mono">${esc(a.number)}</td>
          <td>${esc(a.holder)}</td>
          <td class="mono">${esc(a.currency)}</td>
          <td class="num mono">${esc(a.balance)}</td></tr>`).join('')}</tbody></table></div>`
        : (b.error ? '' : '<div class="empty">No accounts yet.</div>')}
      ${b.can_open_account ? `
        <div class="form" id="open-${esc(b.id)}">
          <div class="field wide"><label>Holder</label><input data-field="holder" placeholder="Acme Ltd"></div>
          <div class="field"><label>Currency</label><input data-field="currency" value="EUR"></div>
          <div class="field"><label>Opening balance</label><input data-field="opening_balance" placeholder="0.00"></div>
          <button class="btn small" data-action="open-account" data-id="${esc(b.id)}">Open account</button>
        </div>`
        : `<div class="note warn">${esc(b.note || 'This bank has no account-opening endpoint.')}</div>`}
    </div>`;
  }
  return `<div class="card ${open ? 'is-open' : ''}">
    <div class="card-head" data-card="${esc(b.id)}">
      <span class="chev">▸</span>
      <i class="dot ${b.error ? 'down' : 'up'}"></i>
      <div class="card-title">
        <span class="name">${esc(b.name)} <span class="pill ${esc(b.kind)}">${esc(b.kind)}</span></span>
        <span class="desc">${esc(b.summary)}</span>
      </div>
      <div class="card-meta"><span>${total} account${total === 1 ? '' : 's'}</span></div>
    </div>
    ${body}
  </div>`;
}

/* --- merchants --------------------------------------------------------- */

function renderMerchants() {
  const d = state.data.merchants;
  if (!d) return `<div class="empty">${esc(state.error || 'Loading…')}</div>`;

  let html = '';
  if (!d.provisioning_ready) {
    html += `<div class="note warn">Payout-rail provisioning is unavailable: ${esc(d.provisioning_error || 'B4B signing key not readable')}.
      Merchants can still be created; they just cannot be registered as beneficiaries until the key is there.</div>`;
  }

  html += `<div class="note">These merchants are <b>the lab's</b>: this console's registry
      (<span class="mono">console-data/registry.json</span>) and what it made at the vendors — a B4B beneficiary per
      outlet, card payments per MID at Worldline. Your platform's own database knows none of them. To settle them end
      to end, the platform seeds its records from <span class="mono">GET /v1/merchants</span> (each outlet's MID,
      payout account, country and currency); testing only the settle path, the platform's own seeder — a runner's
      <i>Seed merchants</i> — is the other way round, and these are not needed.</div>`;

  const batches = d.batches || [];
  if (batches.length) {
    html += `<div class="section-title">Batches <small>${batches.length} — many merchants made at once</small></div>`;
    html += `<div class="card"><div class="card-body" style="border-top:0;padding-top:14px"><div class="table-scroll"><table>
      <thead><tr><th>Batch</th><th class="num">Merchants</th><th class="num">Outlets</th><th class="num">On the rail</th>
        <th>Countries</th><th class="num">Seed</th><th class="actions"></th><th class="actions"></th><th class="actions"></th></tr></thead>
      <tbody>${batches.map((b) => {
        const ms = d.merchants.filter((m) => m.batch === b.id);
        const outlets = ms.reduce((n, m) => n + m.outlets.length, 0);
        const onRail = ms.reduce((n, m) => n + m.outlets.filter((o) => o.beneficiary_id).length, 0);
        const ccs = [...new Set(ms.map((m) => m.country))].sort().join(' ');
        return `<tr>
          <td class="mono">${esc(b.id)}<div class="hint">${esc(ago(b.created_at))} ago</div></td>
          <td class="num mono">${ms.length}</td>
          <td class="num mono">${outlets}</td>
          <td class="num mono">${onRail}</td>
          <td class="mono">${esc(ccs || '—')}</td>
          <td class="num mono">${esc(String(b.seed))}</td>
          <td class="actions"><button class="ghost small" data-action="batch-provision" data-id="${esc(b.id)}"${d.provisioning_ready ? '' : ' hidden'}>Register at B4B</button></td>
          <td class="actions"><button class="ghost small" data-action="batch-trading" data-id="${esc(b.id)}" data-count="5">Seed 5 payments / outlet</button></td>
          <td class="actions"><button class="danger small" data-action="delete-batch" data-id="${esc(b.id)}" data-count="${ms.length}">Delete</button></td>
        </tr>`;
      }).join('')}</tbody></table></div>
      <p class="hint">Each button is one call over the whole batch, reported per merchant. Payments are dated yesterday
        (Worldline settles T+1) at each merchant's own typical ticket; run Worldline's morning cycle to cut the file.</p>
    </div></div>`;
  }

  const partners = d.partners || [];
  html += `<div class="section-title">Hierarchy <small>${d.distributors.length} distributor(s), ${partners.length} partner(s), ${d.merchants.length} merchant(s)</small></div>`;
  html += `<div class="card"><div class="card-body" style="border-top:0;padding-top:14px">
    <div class="table-scroll"><table>
      <thead><tr><th>Partner</th><th>Distributor</th><th>Country</th><th class="num">Merchants</th></tr></thead>
      <tbody>${partners.map((p) => {
        const dist = d.distributors.find((x) => x.id === p.distributor_id);
        const n = d.merchants.filter((m) => m.partner_id === p.id).length;
        return `<tr><td>${esc(p.name)} <span class="mono">${esc(p.id)}</span></td>
          <td>${esc(dist ? dist.name : p.distributor_id)}</td>
          <td class="mono">${esc(p.country)}</td>
          <td class="num">${n}</td></tr>`;
      }).join('')}</tbody></table></div>
    <div class="form">
      <div class="field wide"><label>New partner</label><input data-field="name" placeholder="Southwind Partner Services"></div>
      <div class="field"><label>Country</label>${countrySelect(d.countries, '')}</div>
      <button class="ghost small" data-action="add-partner">Add partner</button>
    </div>
  </div></div>`;

  html += `<div class="section-title">Merchants</div>`;
  html += d.merchants.length
    ? d.merchants.map((m) => merchantCard(m, d)).join('')
    : `<div class="empty">No merchants yet. Create one — or a batch of them — it is what every outlet, MID and payout in this lab hangs off.</div>`;
  return html;
}

function countrySelect(countries, selected, field = 'country') {
  return `<select data-field="${esc(field)}">
    <option value="">auto</option>
    ${(countries || []).map((c) => `<option value="${esc(c.code)}" ${c.code === selected ? 'selected' : ''}>${esc(c.code)} — ${esc(c.name)} (${esc(c.currency)})</option>`).join('')}
  </select>`;
}

function merchantCard(m, d) {
  const open = isOpen(m.id);
  const provisioned = m.outlets.filter((o) => o.beneficiary_id).length;
  const blocked = m.outlets.some((o) => o.sanctions_status && o.sanctions_status !== 'pass');
  const statusPill = m.status === 'active' ? 'ok' : (m.status === 'suspended' ? 'bad' : '');

  let body = '';
  if (open) {
    body = `<div class="card-body">
      <dl class="kv">
        <dt>Legal name</dt><dd style="font-family:var(--sans)">${esc(m.legal_name)}</dd>
        <dt>Address</dt><dd style="font-family:var(--sans)">${esc([m.address.line1, m.address.city, m.address.post_code, m.address.country].filter(Boolean).join(', '))}</dd>
        <dt>Settles in</dt><dd>${esc(m.currency)}</dd>
        <dt>MCC</dt><dd>${esc(m.mcc)}</dd>
        <dt>Contact</dt><dd>${esc(m.email)}</dd>
        <dt>Partner</dt><dd>${esc(m.partner_id || '—')}</dd>
      </dl>

      <div class="table-scroll"><table>
        <thead><tr><th>Outlet</th><th>MID</th><th>Terminal</th><th>Paid into</th><th>Beneficiary</th><th>Sanctions</th></tr></thead>
        <tbody>${m.outlets.map((o) => `<tr>
          <td>${esc(o.name)}<div class="mono" style="color:var(--text-faint)">${esc(o.address.city)}, ${esc(o.address.country)}</div></td>
          <td class="mono">${esc(o.mid)}</td>
          <td class="mono">${esc(o.terminal_id)}</td>
          <td class="mono">${esc(o.account_number)}<div style="color:var(--text-faint)">${esc(o.financial_institution)}</div></td>
          <td class="mono">${o.beneficiary_id ? esc(o.beneficiary_id) : '<span style="color:var(--text-faint)">not provisioned</span>'}</td>
          <td>${o.beneficiary_id ? sanctionsControl(o) : '—'}</td>
        </tr>`).join('')}</tbody></table></div>

      <div class="form">
        <button class="ghost small" data-action="add-outlet" data-id="${esc(m.id)}">Add outlet</button>
        <button class="btn small" data-action="provision" data-id="${esc(m.id)}">Register outlets at B4B</button>
        <button class="ghost small" data-action="seed-trading" data-id="${esc(m.id)}" data-count="5" data-amount="2500">Seed 5 card payments / outlet</button>
        <button class="ghost small" data-action="set-status" data-id="${esc(m.id)}" data-status="${m.status === 'suspended' ? 'active' : 'suspended'}">${m.status === 'suspended' ? 'Reinstate' : 'Suspend'}</button>
        <button class="danger small" data-action="delete-merchant" data-id="${esc(m.id)}">Delete</button>
      </div>
      <p class="hint">Seeded card payments are dated yesterday, because Worldline settles T+1 — run the morning cycle on the Services view to have them appear in a settlement file.
        B4B keeps beneficiaries in memory, so after it restarts an outlet shown here as registered is one B4B has forgotten; re-registering is safe and corrects the record rather than creating a second payee.</p>
    </div>`;
  }

  return `<div class="card ${open ? 'is-open' : ''}">
    <div class="card-head" data-card="${esc(m.id)}">
      <span class="chev">▸</span>
      <div class="card-title">
        <span class="name">${esc(m.trading_name || m.legal_name)}
          <span class="pill ${statusPill}">${esc(m.status)}</span>
          ${blocked ? '<span class="pill bad">sanctions</span>' : ''}
        </span>
        <span class="desc">${esc(m.id)}${m.batch ? ` · ${esc(m.batch)}` : ''} · ${esc(m.country)} · ${esc(m.currency)} · ${m.outlets.length} outlet${m.outlets.length === 1 ? '' : 's'}, ${provisioned} on the payout rail</span>
      </div>
      <div class="card-meta"><span>${esc(m.mcc)}</span></div>
    </div>
    ${body}
  </div>`;
}

function sanctionsControl(o) {
  const opts = ['pass', 'review', 'fail'].map((s) =>
    `<option value="${s}" ${o.sanctions_status === s ? 'selected' : ''}>${s}</option>`).join('');
  return `<select data-action-change="sanctions" data-mid="${esc(o.mid)}">${opts}</select>`;
}

/* --- configuration ------------------------------------------------------ */

function renderConfig() {
  const d = state.data.config;
  if (!d) return `<div class="empty">${esc(state.error || 'Loading…')}</div>`;
  let html = '';

  html += `<div class="section-title">Connect your platform <small>every vendor's addresses, credentials and keys</small></div>`;
  html += connectAllPanel();

  html += `<div class="section-title" data-anchor="stand-ins">Stand-ins <small>the lab's own scaffolding for the platform's part — live switches</small></div>`;
  html += standInsConfig();

  const bc = d.banking_circle;
  html += `<div class="section-title">Banking Circle notification delivery <small>${esc(d.banking_circle_source || d.banking_circle_path || '')}</small></div>`;
  if (d.banking_circle_error) {
    html += `<div class="note bad">${esc(d.banking_circle_error)}</div>`;
  } else if (bc) {
    const scale = bc.time_scale || 1;
    html += `<div class="card"><div class="card-body" style="border-top:0;padding-top:14px">
      ${d.banking_circle_note ? `<div class="note warn">${esc(d.banking_circle_note)}</div>` : ''}
      <div class="note">Time scale <b>${esc(scale)}×</b> — the documented schedule really ends 48 hours after the first failure; here it plays out in about ${esc(fmtSeconds(totalSeconds(bc) / scale))}. Deactivation after ${esc(bc.deactivate_after_retries)} failed retries.</div>
      <div class="table-scroll"><table>
        <thead><tr><th class="num">Retry</th><th>Documented wait</th><th>Wait here</th><th>Email</th></tr></thead>
        <tbody>${(bc.retry_schedule || []).map((s, i) => `<tr>
          <td class="num mono">${i + 1}</td>
          <td class="mono">${esc(fmtSeconds(parseDur(s.after)))}</td>
          <td class="mono">${esc(fmtSeconds(parseDur(s.after) / scale))}</td>
          <td>${s.email ? `<span class="pill ${s.email === 'deactivation' ? 'bad' : 'warn'}">${esc(s.email)}</span>` : ''}</td>
        </tr>`).join('')}</tbody></table></div>
      <p class="hint">Read-only here: banking-circle loads <span class="mono">${esc(d.banking_circle_path || 'config/banking-circle.json')}</span> at start-up. Edit it on the host and restart that service —
        <span class="mono">$EDITOR config/banking-circle.json &amp;&amp; docker compose restart banking-circle</span>.
        <span class="mono">BC_TIME_SCALE</span> in compose.yml overrides the file's <span class="mono">time_scale</span>; the value above is what the service reported it is running.</p>
    </div></div>`;
  }

  html += `<div class="section-title">Worldline file-exchange channel <small>live, and editable</small></div>`;
  if (d.worldline_channel_error) {
    html += `<div class="note bad">${esc(d.worldline_channel_error)}</div>`;
  } else if (d.worldline_channel) {
    html += `<div class="card"><div class="card-body" style="border-top:0;padding-top:14px" id="wl-channel">
      <textarea data-field="json">${esc(JSON.stringify(d.worldline_channel, null, 2))}</textarea>
      <div class="form">
        <button class="btn small" data-action="save-channel">Save to Worldline</button>
      </div>
      <p class="hint">PUT /config on the worldline service. This is the SFT file-exchange channel configuration, not the settlement cycle — the cycle's timings are environment variables, listed below.</p>
    </div></div>`;
  }

  for (const g of d.groups || []) {
    html += `<div class="section-title">${esc(g.title)}${g.note ? ` <small>${esc(g.note)}</small>` : ''}</div>`;
    html += `<div class="card"><div class="card-body" style="border-top:0;padding-top:14px"><div class="table-scroll"><table>
      <thead><tr><th>Variable</th><th>Service</th><th>${g.settings.some((s) => s.live !== undefined && s.live !== '') ? 'Live value' : 'In compose.yml'}</th><th>What it does</th></tr></thead>
      <tbody>${g.settings.map((s) => `<tr>
        <td class="mono">${esc(s.key)}</td>
        <td class="mono">${esc(s.service)}</td>
        <td class="mono">${esc(s.live || s.configured || '—')}</td>
        <td>${esc(s.purpose || '')}</td></tr>`).join('')}</tbody></table></div></div></div>`;
  }
  return html;
}

/* Every stand-in on one table: what it plays, how it is wired right now,
   and the two switches — connect (where its wiring lives, live) and show
   (its card on the Platform view and its box in the diagram). Hidden by
   default; this is the place to bring one back. */
function standInsConfig() {
  const all = state.standins;
  if (!all) return '<div class="note">Reading how the stand-ins are wired…</div>';
  if (all.error) return `<div class="note bad">Stand-ins unavailable — ${esc(all.error)}</div>`;
  const rows = (all.stand_ins || []).map((v) => {
    const conns = v.connections || [];
    const switchable = conns.some((c) => c.connected != null);
    const pill = v.state === 'connected' ? '<span class="pill ok">connected</span>'
      : v.state === 'disconnected' || v.state === 'partial' ? `<span class="pill warn">${esc(v.state)}</span>`
        : v.state === 'unknown' ? '<span class="pill">unknown</span>'
          : '<span class="hint">nothing calls it on its own</span>';
    const wiring = conns.map((c) => `<div class="hint">${esc(c.label)}: ${c.error ? esc(c.error)
      : c.connected == null ? esc(c.detail || 'not wired here') : `${c.connected ? 'connected' : 'disconnected'}${c.detail ? ' — ' + esc(c.detail) : ''}`}</div>`).join('');
    const toggle = !switchable ? ''
      : v.state === 'connected' || v.state === 'partial'
        ? `<button class="ghost small" data-action="standin" data-id="${esc(v.id)}" data-connected="false">Disconnect</button>`
        : `<button class="ghost small" data-action="standin" data-id="${esc(v.id)}" data-connected="true">Connect</button>`;
    const show = `<button class="ghost small" data-action="standin" data-id="${esc(v.id)}" data-shown="${v.shown ? 'false' : 'true'}">${v.shown ? 'Hide card' : 'Show card'}</button>`;
    return `<tr>
      <td>${esc(shortName(v.name))}<div class="hint">${v.shown ? 'card shown' : 'card hidden'}${v.set_by ? ` · set by <span class="mono">${esc(v.set_by)}</span>` : ''}</div></td>
      <td>${esc(v.plays || '')}${v.starts_off ? `<div class="hint">Start it disconnected: <span class="mono">${esc(v.starts_off)}</span> — read at start-up, so it takes a restart (<span class="mono">make up</span>).</div>` : ''}</td>
      <td>${pill}${wiring}</td>
      <td class="actions">${toggle}</td>
      <td class="actions">${show}</td>
    </tr>`;
  }).join('');
  return `<div class="card"><div class="card-body" style="border-top:0;padding-top:14px">
    <div class="note">Before a platform plugged in, these played its part, so each vendor's outbound half could be
      proved connected. With yours plugged in they are noise — payouts at B4B nobody made, a second listener taking
      Banking Circle's batches — so their cards start hidden. Developing a vendor <i>without</i> a platform?
      Connect them here and the settle path runs end to end on its own: Worldline's file is pulled, paid out
      through B4B and Banking Circle, and the receiver takes the webhooks. <b>Connect</b> switches the wiring live,
      where it lives (a lab switch on the service, the subscription at the bank); <b>Show card</b> puts it back on
      the Platform view and in the diagram.</div>
    <div class="table-scroll"><table>
      <thead><tr><th>Stand-in</th><th>Plays</th><th>Wiring now</th><th class="actions"></th><th class="actions"></th></tr></thead>
      <tbody>${rows}</tbody></table></div>
    <p class="hint">Connected lives in the services and survives a lab API restart. Shown is the lab API's own and is
      not: after a restart every card starts hidden again (<span class="mono">CONSOLE_STAND_INS_SHOWN=true</span> on
      the lab API starts them shown). A registered platform may ask for either in its descriptor's
      <span class="mono">stand_ins</span>; that is applied when it registers or changes its mind, so a switch pressed
      here holds.</p>
  </div></div>`;
}

/* Go renders a duration as "15s", "1m0s", "2h0m0s" -- a sum of components,
   not a single number and unit. Parsing only the first one would read 2h0m0s
   as two hours and 1m0s as one minute by luck, and 90m as 90 minutes but
   1h30m0s as one hour. Sum them all. */
function parseDur(s) {
  const units = { ns: 1e-9, us: 1e-6, 'µs': 1e-6, ms: 1e-3, s: 1, m: 60, h: 3600 };
  const re = /(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g;
  let total = 0, m;
  while ((m = re.exec(String(s || '')))) total += parseFloat(m[1]) * units[m[2]];
  return total;
}
function totalSeconds(bc) {
  return (bc.retry_schedule || []).reduce((acc, s) => acc + parseDur(s.after), 0);
}
function fmtSeconds(s) {
  if (s < 1) return `${+(s * 1000).toFixed(s < 0.01 ? 1 : 0)}ms`;
  if (s < 60) return `${+s.toFixed(2)}s`;
  if (s < 3600) return `${+(s / 60).toFixed(1)}m`;
  return `${+(s / 3600).toFixed(1)}h`;
}

/* --- actions ------------------------------------------------------------ */

const ACTIONS = {
  probe: async (d) => { await api('POST', `/v1/services/${d.id}/probe`); },

  cycle: async (d) => {
    const r = await api('POST', `/v1/actions/worldline-cycle?slot=${encodeURIComponent(d.slot)}`);
    const files = (r.files || []).map((f) => f.filename).join('\n');
    toast(`Cycle run (${d.slot})`, files || 'No file cut — nothing had traded in the settled window.', 'good');
  },

  pull: async () => {
    const r = await api('POST', '/v1/actions/settlement-pull');
    toast('Pulled from Worldline', r, 'good');
  },

  /* The platform answers 202 and keeps going: a settlement run takes the
     best part of a minute, and a button that sat spinning through it would
     hide the thing worth watching, which is the stages arriving in the
     panels. */
  'plugin-upload': async (d) => {
    const picker = document.createElement('input');
    picker.type = 'file';
    const file = await new Promise((done) => {
      picker.addEventListener('change', () => done(picker.files[0] || null), { once: true });
      picker.addEventListener('cancel', () => done(null), { once: true });
      picker.click();
    });
    if (!file) return;
    state.uploading = { id: d.id, name: file.name, progress: `0% of ${fmtBytes(file.size)}` };
    try {
      const r = await uploadFile(d.id, file);
      toast(`Uploaded ${file.name}`, r.note || `kept as ${r.name}, ${fmtBytes(r.bytes || file.size)}`, 'good');
    } finally {
      state.uploading = null;
    }
  },

  'plugin-preview': async (d) => { await openPreview(d.id, d.name, 100); },

  'plugin-delete-file': async (d) => {
    if (!confirm(`Delete ${d.name}? It is removed from disk on the platform's machine. Runs it already did stay done — only the file goes, and there is no undo.`)) return;
    await api('DELETE', `/v1/plugins/${encodeURIComponent(d.id)}/files?name=${encodeURIComponent(d.name)}`);
    if (state.selected[d.id]) state.selected[d.id].delete(d.name);
    toast('Deleted', d.name, 'good');
  },

  /* The platform opens the folder on its own machine; the toast is its
     answer — the folder, or why it could not and where the file is. */
  'plugin-reveal': async (d) => {
    const r = await api('POST', `/v1/plugins/${encodeURIComponent(d.id)}/files/reveal`, { name: d.name });
    toast('Opened its location', r.note || r.path || d.name, 'good');
  },

  'plugin-clear-selection': async (d) => { delete state.selected[d.id]; },

  'plugin-run': async (d) => {
    const files = d.files.split('|');
    const r = await api('POST', `/v1/plugins/${encodeURIComponent(d.id)}/run`, { files });
    if (files.length > 1) delete state.selected[d.id];
    const runs = r.runs || [r];
    toast(runs.length > 1 ? `${runs.length} settlement runs started` : 'Settlement run started',
      `${runs.map((x) => `${x.currency} ${x.run}`).join('\n')}\nWatch the panels below — this takes about a minute.`, 'good');
  },

  /* The probe is the vendor's own clienttest, and the answer is whatever the
     endpoint did with it — reported from Banking Circle's notification log
     rather than from the 200 that only means "queued". */
  /* Each of these is one POST the clock service owns the meaning of. The toast
     reports the day it landed on, because "it worked" is not the answer —
     "you are now on Sunday, and settlement will skip" is. */
  'clock-sunday': async () => {
    const next = nextWeekday(0); // Sunday
    await moveClock({ at: next, reason: 'non-business-day scenario' });
  },
  'clock-advance': async (d) => moveClock({ advance: d.spec }),
  'clock-auto': async () => moveClock({ mode: 'auto-business-day' }),
  'clock-real': async () => { state.clockPick = null; await moveClock({ mode: 'real' }); },
  'clock-set': async () => {
    const at = pickedInstant(state.clockPick || {
      date: $('[data-clock-pick="date"]').value, time: $('[data-clock-pick="time"]').value,
    });
    if (!at) throw new Error('Pick a date and a time (UTC) first.');
    state.clockPick = null;
    await moveClock({ at, reason: `set to ${at}` });
  },

  'bc-test': async (d) => {
    const r = await api('POST', `/v1/banking-circle/subscriptions/${encodeURIComponent(d.id)}/test`);
    toast(r.delivered ? 'Endpoint took it' : 'Nothing took it', r.result, r.delivered ? 'good' : 'bad');
  },

  /* Pause and release are two routes, not one with a flag, so each button is
     one call somebody could make with curl — and so a mistyped URL is a 404
     rather than a pause nobody asked for. */
  'bc-pause': async (d) => {
    const r = await api('POST', `/v1/banking-circle/subscriptions/${encodeURIComponent(d.id)}/pause`);
    toast('Delivery paused', `${r.endpoint}\nNotifications will queue in the log below until you release them.` +
      (r.queued ? `\n${r.queued} already waiting.` : ''));
  },

  'bc-resume': async (d) => {
    const r = await api('POST', `/v1/banking-circle/subscriptions/${encodeURIComponent(d.id)}/resume`);
    toast('Delivery resumed',
      r.released ? `${r.released} notification(s) released to ${r.endpoint}, oldest first.`
                 : `${r.endpoint}\nNothing was waiting.`,
      'good');
  },

  /* Return and reverse are two routes for the same reason pause and release
     are, and both are irreversible, so the confirm says what cannot be
     undone rather than asking whether you are sure. */
  'bc-return': async (d) => {
    if (!confirm(`Return ${d.id}? It stays processed, and ${d.amount} ${d.currency} comes back to the safeguarding account as a new incoming payment flagged return. A payout comes back once — there is no undo.`)) return;
    const r = await api('POST', `/v1/banking-circle/payouts/${encodeURIComponent(d.id)}/return`);
    toast('Payout returned', `${r.id} brings ${r.amount} ${r.currency} back, flagged return.\n${d.id} stays processed.`, 'good');
  },

  'bc-reverse': async (d) => {
    if (!confirm(`Reverse ${d.id}? It becomes reversed, and ${d.amount} ${d.currency} is booked back to the safeguarding account. There is no undo.`)) return;
    const r = await api('POST', `/v1/banking-circle/payouts/${encodeURIComponent(d.id)}/reverse`);
    toast('Payout reversed', `${r.id} is now ${r.state}.\nThe reversal is booked back to the safeguarding account.`, 'good');
  },

  /* Connect, disconnect, show or hide a stand-in: one call. Partial
     success is reported as partial, per wiring. */
  standin: async (d) => {
    const body = {};
    if (d.connected) body.connected = d.connected === 'true';
    if (d.shown) body.shown = d.shown === 'true';
    if (body.connected === false && !confirm('Disconnect this stand-in? Its timers stop and vendors stop delivering to it until it is connected again — nothing already delivered is undone.')) return;
    const v = await api('POST', `/v1/stand-ins/${encodeURIComponent(d.id)}`, body);
    const what = body.shown === undefined ? `now ${v.state}` : body.shown ? 'shown' : 'hidden';
    toast(`${shortName(v.name)} ${what}`, (v.errors || []).join('\n') || '', (v.errors || []).length ? 'bad' : 'good');
  },

  /* A button a platform declared: one call to the path it declared, the
     toast in the platform's own words. One that declares fields asks for
     them first, in a modal (design-system.md, Action form). */
  'plugin-action': async (d) => {
    const act = declaredAction(d.id, d.act);
    if ((act.fields || []).length) {
      openActionForm(d.id, act);
      return;
    }
    if (act.confirm && !confirm(act.confirm)) return;
    const r = await api('POST', `/v1/plugins/${encodeURIComponent(d.id)}/actions/${encodeURIComponent(d.act)}`, {});
    toast(act.label, (r && (r.note || r.output)) || act.note || r, 'good');
  },

  'plugin-action-submit': async (d, btn) => {
    const act = declaredAction(d.id, d.act);
    const body = actionFormBody(act.fields || [], fieldsIn(btn.closest('.modal')));
    if (act.confirm && !confirm(act.confirm)) return;
    const r = await api('POST', `/v1/plugins/${encodeURIComponent(d.id)}/actions/${encodeURIComponent(d.act)}`, body);
    closeModal();
    toast(act.label, (r && (r.note || r.output)) || act.note || r, 'good');
  },

  /* Worldline's wire into the safeguarding accounts, for a run that did not
     come through a morning cycle. An account already holding money is left
     alone, and the toast says which. */
  'fund-sga': async () => {
    const r = await api('POST', '/v1/actions/fund-sga');
    const rows = r.results || [];
    const bad = rows.filter((x) => x.error);
    toast(bad.length ? 'Safeguarding funded in part' : 'Safeguarding accounts',
      rows.map((x) => `${x.currency}: ${x.error ? x.error : x.funded ? `funded ${x.funded}` : x.skipped}`).join('\n'),
      bad.length ? 'bad' : 'good');
  },

  'open-account': async (d, btn) => {
    const scope = btn.closest('.form');
    const f = fieldsIn(scope);
    if (!f.holder) throw new Error('holder is required');
    const a = await api('POST', `/v1/banks/${d.id}/accounts`, {
      holder: f.holder, currency: f.currency || 'EUR', opening_balance: f.opening_balance || '',
    });
    toast('Account opened', `${a.id}  ${a.number}`, 'good');
  },

  'add-partner': async (_d, btn) => {
    const f = fieldsIn(btn.closest('.form'));
    if (!f.name) throw new Error('name is required');
    await api('POST', '/v1/partners', { name: f.name, country: f.country || '' });
    toast('Partner added', f.name, 'good');
  },

  'create-merchant': async (_d, btn) => {
    const f = fieldsIn(btn.closest('.modal'));
    const body = {
      legal_name: f.legal_name,
      trading_name: f.trading_name,
      partner_id: f.partner_id || '',
      country: f.country || '',
      currency: f.currency || '',
      mcc: f.mcc || '',
      email: f.email || '',
      outlets: parseInt(f.outlets || '1', 10) || 1,
    };
    if (f.line1) {
      body.address = { line1: f.line1, city: f.city, post_code: f.post_code, country: f.country || '' };
    }
    const m = await api('POST', '/v1/merchants', body);
    closeModal();
    state.open.add(`merchants:${m.id}`);
    toast('Merchant created', `${m.legal_name}\n${m.outlets.map((o) => o.mid).join('\n')}`, 'good');
  },

  'add-outlet': async (d) => {
    const r = await api('POST', `/v1/merchants/${d.id}/outlets`, { name: '' });
    toast('Outlet opened', `${r.outlet.name}\nMID ${r.outlet.mid}`, 'good');
  },

  provision: async (d) => {
    const r = await api('POST', `/v1/merchants/${d.id}/provision`);
    const bad = r.outlets.filter((o) => o.error);
    if (bad.length) {
      toast('Provisioned with failures', bad.map((o) => `${o.mid}: ${o.error}`).join('\n'), 'bad');
    } else {
      toast('Registered at B4B', r.outlets.map((o) => `${o.mid} → ${o.beneficiary_id} (${o.sanctions_status})`).join('\n'), 'good');
    }
  },

  'seed-trading': async (d) => {
    const r = await api('POST', `/v1/merchants/${d.id}/trading`, {
      count: parseInt(d.count, 10), amount_cents: parseInt(d.amount, 10),
    });
    toast('Worldline acquired the trading', `${r.accepted} transactions dated ${r.date}\n${r.note}`, 'good');
  },

  'set-status': async (d) => {
    await api('POST', `/v1/merchants/${d.id}/status`, { status: d.status });
    toast('Status changed', `${d.id} → ${d.status}`, 'good');
  },

  'delete-merchant': async (d) => {
    if (!confirm('Delete this merchant? Anything already registered at B4B stays there — the rail has no delete.')) return;
    await api('DELETE', `/v1/merchants/${d.id}`);
    state.open.delete(`merchants:${d.id}`);
    toast('Merchant deleted', d.id);
  },

  'save-channel': async (_d, btn) => {
    const ta = btn.closest('.card-body').querySelector('[data-field="json"]');
    let parsed;
    try { parsed = JSON.parse(ta.value); } catch (err) { throw new Error('not valid JSON: ' + err.message); }
    await api('PUT', '/v1/config/worldline-channel', parsed);
    toast('Channel configuration saved', '', 'good');
  },

  'open-create-merchant': async () => { openMerchantModal(); },

  'open-create-env': async () => { openEnvModal(); },

  'create-env': async (_d, btn) => {
    const f = fieldsIn(btn.closest('.modal'));
    const config = {};
    if (f.settlement_identifier) config.settlement_identifier = f.settlement_identifier;
    if (f.clock) config.clock = f.clock;
    if (f.aci_webhook_url) config.aci_webhook_url = f.aci_webhook_url;
    if (f.b4b_callback_url) config.b4b_callback_url = f.b4b_callback_url;
    const hosts = (f.webhook_hosts || '').split(',').map((h) => h.trim()).filter(Boolean);
    if (hosts.length) config.webhook_hosts = hosts;
    const r = await api('POST', '/v1/environments', { name: f.name, id: f.id || '', config }, { noEnv: true });
    closeModal();
    state.open.add(`environments:env-${r.environment.id}`);
    const pushed = Object.entries(r.pushed || {}).map(([svc, how]) => `${svc}: ${how}`).join('\n');
    toast(`Environment ${r.environment.id} created`, `Token ${r.token}\n${pushed}`, 'good');
  },

  'use-env': async (d) => { setEnv(d.id); },

  'rotate-env-token': async (d) => {
    if (!confirm(`Give ${d.id} a new token? The old one stops working at once: its platform's runner, scripts and agents need the new one. Its logins at the vendors do not change.`)) return;
    const r = await api('POST', `/v1/environments/${encodeURIComponent(d.id)}/token`, undefined, { noEnv: true });
    toast('Token rotated', `${d.id}: ${r.token}`, 'good');
  },

  'delete-env': async (d) => {
    if (!confirm(`Delete ${d.name}? Every vendor forgets its records — payouts, accounts, files, merchants — and its token and logins stop working. The default environment and every other one are untouched. There is no undo.`)) return;
    const r = await api('DELETE', `/v1/environments/${encodeURIComponent(d.id)}`, undefined, { noEnv: true });
    if (state.env === d.id) setEnv('default', false);
    toast('Environment deleted', Object.entries(r.vendors || {}).map(([svc, how]) => `${svc}: ${how}`).join('\n') || d.id, 'good');
  },
  'open-create-batch': async () => { openBatchModal(); },

  'create-batch': async (_d, btn) => {
    const f = fieldsIn(btn.closest('.modal'));
    const body = {
      count: parseInt(f.count || '20', 10),
      max_outlets: parseInt(f.max_outlets || '3', 10),
      country: f.country || '',
      partner_id: f.partner_id || '',
    };
    if (f.seed !== '') {
      const seedValue = Number(f.seed);
      if (!Number.isInteger(seedValue) || seedValue < 0) throw new Error('Seed is a whole number, or blank for a new population.');
      body.seed = seedValue;
    }
    if (!(body.count >= 1 && body.count <= 500)) throw new Error('Merchants is 1 to 500.');
    const r = await api('POST', '/v1/merchants/batches', body);
    closeModal();
    toast(`Batch ${r.batch.id} created`, r.note, 'good');
  },

  /* Batch actions report per merchant: a partial run is a normal outcome,
     and the merchants that failed are the ones worth reading
     (design-system.md, contract 4). */
  'batch-provision': async (d) => {
    const r = await api('POST', `/v1/merchants/batches/${encodeURIComponent(d.id)}/provision`);
    toast((r.failed || []).length ? 'Registered with failures' : 'Registered at B4B',
      [r.note, ...(r.failed || [])].join('\n'), (r.failed || []).length ? 'bad' : 'good');
  },

  'batch-trading': async (d) => {
    const r = await api('POST', `/v1/merchants/batches/${encodeURIComponent(d.id)}/trading`, { count: parseInt(d.count, 10) });
    toast((r.failed || []).length ? 'Trading seeded with failures' : 'Worldline acquired the trading',
      [r.note, ...(r.failed || [])].join('\n'), (r.failed || []).length ? 'bad' : 'good');
  },

  'delete-batch': async (d) => {
    if (!confirm(`Delete batch ${d.id} and its ${d.count} merchants? Anything already registered at B4B or traded at Worldline stays there — neither has a delete.`)) return;
    const r = await api('DELETE', `/v1/merchants/batches/${encodeURIComponent(d.id)}`);
    toast('Batch deleted', `${r.deleted}: ${r.merchants} merchant(s) gone from the registry`);
  },
  refresh: async () => { },
};

function bindChanges(root) {
  // The clock picker keeps what is being typed in state, so a re-render
  // does not throw it away, and says live what the time is in Paris.
  root.querySelectorAll('[data-clock-pick]').forEach((input) => {
    input.addEventListener('input', () => {
      const scope = input.closest('.form');
      state.clockPick = {
        date: scope.querySelector('[data-clock-pick="date"]').value,
        time: scope.querySelector('[data-clock-pick="time"]').value,
      };
      const hint = scope.querySelector('[data-clock-hint]');
      if (hint) hint.textContent = pickHint(state.clockPick);
    });
    input.addEventListener('click', (ev) => ev.stopPropagation());
  });
  // Ticking a settlement file: held in state, so the next poll draws it
  // ticked again (design-system.md, Row selection).
  root.querySelectorAll('[data-select-file]').forEach((box) => {
    box.addEventListener('click', (ev) => ev.stopPropagation());
    box.addEventListener('change', () => {
      toggleSelected(box.dataset.id, box.dataset.name, box.dataset.currency, box.checked);
      renderView();
    });
  });
  root.querySelectorAll('[data-action-change="sanctions"]').forEach((sel) => {
    sel.addEventListener('change', async (ev) => {
      ev.stopPropagation();
      try {
        await api('POST', `/v1/outlets/${sel.dataset.mid}/sanctions`, { sanctions_status: sel.value });
        toast('Sanctions status moved', `${sel.dataset.mid} → ${sel.value}`, sel.value === 'pass' ? 'good' : 'bad');
      } catch (err) {
        toast('Failed', err.message, 'bad');
      }
      await load();
    });
    sel.addEventListener('click', (ev) => ev.stopPropagation());
  });
}

/* --- modal --------------------------------------------------------------- */

function openMerchantModal() {
  const d = state.data.merchants || { countries: [], partners: [] };
  $('#modal').innerHTML = `
    <div class="modal-head">
      <h2>New merchant</h2>
      <p>Anything left blank is generated — obviously fake, and derived from the merchant's own id so it never changes under you.</p>
    </div>
    <div class="modal-body">
      <div class="form">
        <div class="field wide"><label>Legal name</label><input data-field="legal_name" placeholder="Quiet Coffee Ltd" autofocus></div>
        <div class="field wide"><label>Trading name</label><input data-field="trading_name" placeholder="Quiet Coffee"></div>
      </div>
      <div class="form">
        <div class="field"><label>Country</label>${countrySelect(d.countries, '')}</div>
        <div class="field"><label>Currency</label><input data-field="currency" placeholder="auto"></div>
        <div class="field"><label>MCC</label><input data-field="mcc" placeholder="auto"></div>
        <div class="field"><label>Outlets</label><input data-field="outlets" type="number" min="0" max="20" value="1"></div>
      </div>
      <div class="form">
        <div class="field wide"><label>Partner</label>
          <select data-field="partner_id">
            <option value="">none</option>
            ${(d.partners || []).map((p) => `<option value="${esc(p.id)}">${esc(p.name)}</option>`).join('')}
          </select>
        </div>
        <div class="field wide"><label>Contact email</label><input data-field="email" placeholder="auto (.test)"></div>
      </div>
      <div class="form">
        <div class="field wide"><label>Address line 1</label><input data-field="line1" placeholder="auto"></div>
        <div class="field"><label>City</label><input data-field="city" placeholder="auto"></div>
        <div class="field"><label>Post code</label><input data-field="post_code" placeholder="auto"></div>
      </div>
      <p class="hint">Each outlet gets its own MID, terminal id and payout account. Register them at B4B afterwards to put them on the payout rail.</p>
    </div>
    <div class="modal-foot">
      <button class="ghost" id="modal-cancel">Cancel</button>
      <button class="btn" data-action="create-merchant">Create merchant</button>
    </div>`;
  $('#modal-backdrop').hidden = false;
  $('#modal-cancel').addEventListener('click', closeModal);
  bindActions($('#modal'));
  $('#modal').querySelector('[data-field="legal_name"]').focus();
}

/* Many merchants in one call (internal/console/batch.go): names, trades,
   countries and outlet counts drawn from a seed, so a batch can be made
   again. Defaults are filled, so the button works straight away. */
function openBatchModal() {
  const d = state.data.merchants || { countries: [], partners: [] };
  $('#modal').innerHTML = `
    <div class="modal-head">
      <h2>Create a batch of merchants</h2>
      <p>A population in one go — every merchant with a name, a trade and its MCC, a country and its currency, and one
        to a few outlets, each with its own MID and payout account. Obviously fake, and drawn from a seed rather than at
        random, so the same seed makes the same names again.</p>
    </div>
    <div class="modal-body">
      <div class="form">
        <div class="field"><label>Merchants</label><input data-field="count" type="number" min="1" max="500" value="20"></div>
        <div class="field"><label>Outlets, up to</label><input data-field="max_outlets" type="number" min="1" max="10" value="3"></div>
        <div class="field"><label>Country</label>${countrySelect(d.countries, '').replace('<option value="">auto</option>', '<option value="">a mix</option>')}</div>
        <div class="field"><label>Seed</label><input data-field="seed" type="number" min="0" placeholder="new"></div>
      </div>
      <div class="form">
        <div class="field wide"><label>Partner</label>
          <select data-field="partner_id">
            <option value="">none</option>
            ${(d.partners || []).map((p) => `<option value="${esc(p.id)}">${esc(p.name)}</option>`).join('')}
          </select>
        </div>
      </div>
      <p class="hint"><b>Outlets</b> — most merchants get one, about a quarter two, a few more, never above this.</p>
      <p class="hint"><b>Country</b> — a mix draws from the EUR and GBP countries, the currencies Banking Circle holds
        safeguarding accounts in; a country picked by name is made whatever it settles in.</p>
      <p class="hint"><b>Seed</b> — blank for a new population; the toast says the seed used, and that seed makes the
        same names, trades and countries again (with new ids and MIDs).</p>
      <p class="hint">Then, on the batch's row: register its outlets at B4B, and seed card payments at Worldline — the
        settlement file pays them once the platform knows them too (see the note on this view).</p>
    </div>
    <div class="modal-foot">
      <button class="ghost" id="modal-cancel">Cancel</button>
      <button class="btn" data-action="create-batch">Create batch</button>
    </div>`;
  $('#modal-backdrop').hidden = false;
  $('#modal-cancel').addEventListener('click', closeModal);
  bindActions($('#modal'));
  $('#modal').querySelector('[data-field="count"]').focus();
}

/* The action a plugin declares now, not the one it declared when the page
   was drawn: a descriptor can change under an open card. */
function declaredAction(id, actionId) {
  const s = ((state.data.overview || {}).services || []).find((x) => x.id === id);
  const act = s && s.plugin && (s.plugin.actions || []).find((a) => a.id === actionId);
  if (!act) throw new Error(`${id} no longer declares ${actionId}`);
  return act;
}

/* An action's form, drawn from its declared fields and nothing else. The
   plugin's words are text, escaped like any peer's. */
function openActionForm(id, act) {
  const input = (f) => {
    const dflt = f.default === undefined || f.default === null ? '' : String(f.default);
    if (f.type === 'select') {
      return `<select data-field="${esc(f.id)}" aria-label="${esc(f.label)}">${(f.options || []).map((o) =>
        `<option value="${esc(o.value)}"${o.value === dflt ? ' selected' : ''}>${esc(o.label)}</option>`).join('')}</select>`;
    }
    if (f.type === 'number') {
      return `<input type="number" data-field="${esc(f.id)}" value="${esc(dflt)}" aria-label="${esc(f.label)}"
        ${f.min !== undefined ? `min="${esc(String(f.min))}"` : ''} ${f.max !== undefined ? `max="${esc(String(f.max))}"` : ''}>`;
    }
    return `<input data-field="${esc(f.id)}" value="${esc(dflt)}" placeholder="${esc(f.placeholder || '')}" aria-label="${esc(f.label)}">`;
  };
  const hints = act.fields.filter((f) => f.hint);
  $('#modal').innerHTML = `
    <div class="modal-head">
      <h2>${esc(act.label)}</h2>
      ${act.note ? `<p>${esc(act.note)}</p>` : ''}
    </div>
    <div class="modal-body">
      <div class="form">${act.fields.map((f) =>
        `<div class="field${f.type === 'text' ? ' wide' : ''}"><label>${esc(f.label)}</label>${input(f)}</div>`).join('')}
      </div>
      ${hints.map((f) => `<p class="hint"><b>${esc(f.label)}</b> — ${esc(f.hint)}</p>`).join('')}
    </div>
    <div class="modal-foot">
      <button class="ghost" id="modal-cancel">Cancel</button>
      <button class="btn" data-action="plugin-action-submit" data-id="${esc(id)}" data-act="${esc(act.id)}">${esc(act.label)}</button>
    </div>`;
  $('#modal-backdrop').hidden = false;
  $('#modal-cancel').addEventListener('click', closeModal);
  bindActions($('#modal'));
  const first = $('#modal').querySelector('[data-field]');
  if (first) first.focus();
}

/* The JSON body an action form sends: numbers as numbers, empty fields
   left out, and a number outside its declared range refused here, where
   the operator can still fix it. */
function actionFormBody(fields, values) {
  const body = {};
  for (const f of fields) {
    const raw = values[f.id];
    if (raw === undefined || raw === '') continue;
    if (f.type !== 'number') { body[f.id] = raw; continue; }
    const n = Number(raw);
    if (!Number.isFinite(n)) throw new Error(`${f.label} must be a number.`);
    if (f.min !== undefined && n < f.min) throw new Error(`${f.label} is at least ${f.min}.`);
    if (f.max !== undefined && n > f.max) throw new Error(`${f.label} is at most ${f.max}.`);
    body[f.id] = n;
  }
  return body;
}

function closeModal() {
  $('#modal-backdrop').hidden = true;
  $('#modal').innerHTML = '';
  $('#modal').classList.remove('wide');
}

/* --- shell ---------------------------------------------------------------- */

function renderView() {
  const meta = VIEWS[state.view];
  $('#view-title').textContent = meta.title;
  $('#view-sub').textContent = meta.sub;

  $('#topbar-actions').innerHTML = envPicker() + (state.view === 'merchants'
    ? `<button class="ghost small" data-action="refresh">Refresh</button>
       <button class="ghost" data-action="open-create-batch">Create a batch…</button>
       <button class="btn" data-action="open-create-merchant">+ Create merchant</button>`
    : state.view === 'environments'
      ? `<button class="ghost small" data-action="refresh">Refresh</button>
       <button class="btn" data-action="open-create-env">+ Create environment</button>`
      : `<button class="ghost small" data-action="refresh">Refresh</button>`);
  const pick = $('#topbar-actions').querySelector('[data-env-pick]');
  if (pick) pick.addEventListener('change', () => setEnv(pick.value));

  const html =
    state.view === 'dashboard' ? renderDashboard() :
    isServiceView() ? renderServices() :
    state.view === 'banks' ? renderBanks() :
    state.view === 'merchants' ? renderMerchants() :
    state.view === 'environments' ? renderEnvironments() :
    state.view === 'docs' ? renderDocs() :
    renderConfig();

  const content = $('#content');
  // Keep the scroll position across a re-render. The poll rebuilds the
  // view every few seconds, and a page that jumps back to the top while
  // you are reading the retry table is a page you close.
  const scroll = content.scrollTop;
  // The same for every box that scrolls on its own (a log's rows): the
  // re-render replaces it with a new one at the top, so an operator reading
  // half-way down a log was thrown back to its first row every five seconds.
  const inner = new Map([...content.querySelectorAll('[data-keep-scroll]')]
    .map((el) => [el.dataset.keepScroll, el.scrollTop]));
  content.innerHTML = (state.error ? `<div class="note bad">${esc(state.error)}</div>` : '') +
    (state.view === 'environments' ? '' : sharedNote()) + html;
  content.querySelectorAll('[data-keep-scroll]').forEach((el) => {
    const top = inner.get(el.dataset.keepScroll);
    if (top) el.scrollTop = top;
  });
  content.scrollTop = scroll;
  bindCards(content);
  bindActions(content);
  bindActions($('#topbar-actions'));
  bindChanges(content);
  bindFlow(content);
  scrollToFocus();
  renderDrawer();
}

/* The hash is the view, and optionally the one thing open in it:
   #vendors/b4b opens the B4B card, #vendors/b4b:payments that card and its
   payments log, #dashboard/payouts the payouts hop's drawer
   (design-system.md, contract 9). A dashboard widget is a plain link
   because of this. */
function parseHash() {
  const raw = decodeURIComponent(location.hash.replace(/^#/, ''));
  const [view, ...rest] = raw.split('/');
  // #flow was this view's name while it only held the diagram, and
  // #services the old single list; a bookmark to either still lands.
  const alias = { flow: 'dashboard', services: 'vendors' };
  const v = alias[view] || view;
  return { view: VIEWS[v] ? v : 'dashboard', item: rest.join('/') || '' };
}

function route() {
  const { view, item } = parseHash();
  if (view === 'dashboard') {
    setHop(item || null, false);
    // Landing on a hop's link: bring its arrow into view too.
    if (item) state.focus = { hop: item };
  } else if (view === 'docs' && (item === 'api' || item.startsWith('api:'))) {
    // #docs/api is a page, not a place on one: it opens at its top, where
    // the page switch is. #docs/api:banking-circle opens that service's
    // card and brings it into view.
    if (item !== 'api') {
      state.open.add(`docs:${item}`);
      state.focus = item;
    }
  } else if (item) {
    // "b4b:payments" opens the card and the panel inside it.
    const parts = item.split(':');
    for (let i = 1; i <= parts.length; i++) state.open.add(`${view}:${parts.slice(0, i).join(':')}`);
    state.focus = parts[0];
  }
  setView(view, item);
}

function setView(view, item = '') {
  if (view !== 'dashboard') setHop(null, false);
  state.view = view;
  document.querySelectorAll('.nav-item').forEach((b) => b.classList.toggle('is-active', b.dataset.view === view));
  const want = item ? `${view}/${item}` : view;
  if (decodeURIComponent(location.hash.replace(/^#/, '')) !== want) {
    history.replaceState(null, '', `#${want}`);
  }
  renderView();
  load();
}

/* After a deep link, bring the card it opened into view — once it exists,
   which may be a poll later. */
function scrollToFocus() {
  if (!state.focus) return;
  const el = state.focus.hop
    ? $('#content').querySelector(`[data-hop="${CSS.escape(state.focus.hop)}"]`)
    : $('#content').querySelector(`.card-head[data-card="${CSS.escape(state.focus)}"], [data-anchor="${CSS.escape(state.focus)}"]`);
  if (!el) return;
  const block = state.focus.hop ? 'center' : 'start';
  state.focus = null;
  el.scrollIntoView({ block });
}

function isEditing() {
  const el = document.activeElement;
  // A ticked checkbox holds nothing half-typed, so it does not pause the poll.
  if (el && el.tagName === 'INPUT' && el.type === 'checkbox') return false;
  return !!el && /^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName);
}

function setTheme(theme) {
  document.documentElement.dataset.theme = theme;
  try { localStorage.setItem('console-theme', theme); } catch (_) { /* private window */ }
}

function init() {
  try {
    const saved = localStorage.getItem('console-theme');
    if (saved) document.documentElement.dataset.theme = saved;
    state.env = localStorage.getItem('console-env') || 'default';
  } catch (_) { /* private window: the default stands */ }

  $('#nav').addEventListener('click', (ev) => {
    const item = ev.target.closest('.nav-item');
    if (item) setView(item.dataset.view);
  });
  // A widget, a card link or a typed URL changes the hash; each lands.
  window.addEventListener('hashchange', route);
  try {
    if (localStorage.getItem('console-rail') === '1') setRail(true);
  } catch (_) { /* private window: the sidebar stays open */ }
  const toggleRail = () => setRail(!$('.app').classList.contains('is-rail'));
  $('#rail-toggle').addEventListener('click', toggleRail);
  $('#brand-mark').addEventListener('click', toggleRail);
  $('#theme-toggle').addEventListener('click', () => {
    setTheme(document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark');
  });
  $('#modal-backdrop').addEventListener('click', (ev) => {
    if (ev.target === $('#modal-backdrop')) closeModal();
  });
  document.addEventListener('keydown', (ev) => {
    if (ev.key !== 'Escape') return;
    if (!$('#modal-backdrop').hidden) closeModal();
    else if (state.hop) setHop(null);
  });

  route();

  // Poll, so a service coming up or going down shows without a reload.
  // Paused while a modal is open or a field has focus: re-rendering
  // replaces the DOM, and doing that under a half-typed form throws the
  // operator's input away.
  // The base rate is the console's usual five seconds. The diagram runs at
  // one, but only while a run is actually in flight: a settlement takes
  // forty seconds and an arrow that lights up four seconds after its
  // traffic arrived is not showing you a sequence, it is showing you a
  // summary.
  let sinceLoad = 0;
  state.timer = setInterval(() => {
    if (!$('#modal-backdrop').hidden) return;
    if (document.hidden) return;
    if (isEditing()) return;
    sinceLoad += 250;
    const run = state.data.flow && state.data.flow.run;
    const live = state.view === 'dashboard' && run && !run.finished_at;
    if (sinceLoad < (live ? FLOW_LIVE_MS : FLOW_IDLE_MS)) return;
    sinceLoad = 0;
    load();
  }, 250);
}

init();
