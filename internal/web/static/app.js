/*
  Twinsweep review page logic (plain JavaScript, no frameworks).

  What it does: polls /api/status while the scan runs, then loads the report
  from /api/report and draws one card per duplicate group. Ticking a copy
  marks it for removal. The action bar sends the ticked copy IDs to
  /api/clean for a dry run, a move to the Recycle Bin/Trash, or (after typing
  DELETE) a permanent delete. The server re-verifies everything; this page is
  only a picker.

  Security notes:
  - File names come from the user's disk and could contain HTML like
    "<img onerror=...>". Every name is inserted with textContent (as plain
    text), never innerHTML, so it can't become live markup. The page's CSP
    blocks inline scripts as a second layer.
  - Change requests carry the session token in the X-Twinsweep-Token header
    (CSRF protection; see internal/web/server.go).
*/
'use strict';

const TOKEN = document.querySelector('meta[name="twinsweep-token"]').content;
const TRASH = document.querySelector('meta[name="twinsweep-trash"]').content;
const PAGE = 100; // groups rendered per "Show more"

// All page state in one place.
const state = {
  report: null,        // the live report from the server
  tab: 'folder',       // 'folder' or 'file'
  selected: new Set(), // entry IDs ticked for removal
  filter: '',
  shown: PAGE,
  sep: '/',            // path separator detected from the report
};

const $ = (id) => document.getElementById(id);

/**
 * el builds a DOM element. Strings among `children` become text nodes, so
 * file paths are always shown as text, never parsed as HTML.
 */
function el(tag, attrs, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k === 'class') node.className = v;
    else if (k.startsWith('on')) node.addEventListener(k.slice(2), v);
    else if (v === true) node.setAttribute(k, '');
    else if (v !== false && v != null) node.setAttribute(k, v);
  }
  for (const c of children) {
    if (c == null || c === false) continue;
    node.append(typeof c === 'string' || typeof c === 'number' ? document.createTextNode(String(c)) : c);
  }
  return node;
}

/** fmtBytes: 1536 -> "1.5 KB" (1024-based, like Explorer/Finder). */
function fmtBytes(n) {
  const u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n : n.toFixed(1)) + ' ' + u[i];
}

/** fmtDate: ISO timestamp -> local "2024-03-01 14:05". */
function fmtDate(s) {
  const d = new Date(s);
  if (isNaN(d) || d.getFullYear() < 1980) return '';
  const p = (x) => String(x).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

/** toast shows a short message for a few seconds. */
function toast(msg) {
  const t = el('div', { class: 'toast', role: 'status' }, msg);
  document.body.append(t);
  setTimeout(() => t.remove(), 3500);
}

/**
 * api calls the server. With a body it's a POST carrying the CSRF token
 * header. Throws an Error with the server's message on failure.
 */
async function api(path, body) {
  const init = { credentials: 'same-origin' };
  if (body !== undefined) {
    init.method = 'POST';
    init.headers = { 'Content-Type': 'application/json', 'X-Twinsweep-Token': TOKEN };
    init.body = JSON.stringify(body);
  }
  const r = await fetch(path, init);
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || `${r.status} ${r.statusText}`);
  return data;
}

// ---------- Scan progress ----------

/** pollStatus checks scan state every 500 ms until the report is ready. */
async function pollStatus() {
  let st;
  try {
    st = await api('/api/status');
  } catch (e) {
    showError('Lost contact with Twinsweep: ' + e.message);
    return;
  }
  if (st.state === 'scanning') {
    $('scanning').hidden = false;
    const p = st.progress || {};
    const phases = { walking: 'Listing files…', hashing: 'Comparing file contents…', folders: 'Comparing folders…', done: 'Finishing…' };
    $('scan-phase').textContent = phases[p.phase] || 'Starting…';
    const pct = p.hashTotal ? Math.round((100 * p.hashDone) / p.hashTotal) : 0;
    $('scan-bar').style.width = (p.phase === 'walking' ? 5 : Math.max(5, pct)) + '%';
    $('scan-counts').textContent = `${(p.filesSeen || 0).toLocaleString()} files (${fmtBytes(p.bytesSeen || 0)}) found` +
      (p.hashTotal ? ` · ${p.hashDone.toLocaleString()} / ${p.hashTotal.toLocaleString()} compared` : '');
    setTimeout(pollStatus, 500);
    return;
  }
  $('scanning').hidden = true;
  if (st.state === 'error') { showError('Scan failed: ' + st.error); return; }
  await loadReport();
}

/** showError displays a message in the red panel. */
function showError(msg) {
  const box = $('error');
  box.replaceChildren(el('b', {}, 'Error: '), msg);
  box.hidden = false;
}

// ---------- Report and rendering ----------

/** loadReport fetches the live report and redraws everything. */
async function loadReport() {
  const data = await api('/api/report');
  state.report = data.report;
  const any = [...data.report.folderGroups, ...data.report.fileGroups][0];
  if (any && any.entries[0].path.includes('\\')) state.sep = '\\';
  // Drop selections for copies that no longer exist.
  const ids = new Set();
  for (const g of allGroups()) for (const e of g.entries) ids.add(e.id);
  for (const id of [...state.selected]) if (!ids.has(id)) state.selected.delete(id);
  if (!state.report.folderGroups.length) state.tab = 'file';
  $('review').hidden = false;
  $('actionbar').hidden = false;
  renderSummary(data.reclaimable);
  render();
}

/** allGroups returns folder + file groups. */
function allGroups() {
  return state.report ? [...state.report.folderGroups, ...state.report.fileGroups] : [];
}

/** renderSummary fills the header line. */
function renderSummary(reclaimable) {
  const r = state.report;
  $('summary').textContent = `${r.roots.join(' · ')} — ${r.mode} mode · ${fmtBytes(reclaimable)} reclaimable`;
}

/** visibleGroups applies the current tab and path filter. */
function visibleGroups() {
  const list = state.tab === 'folder' ? state.report.folderGroups : state.report.fileGroups;
  const f = state.filter.toLowerCase();
  if (!f) return list;
  return list.filter((g) => g.entries.some((e) => e.path.toLowerCase().includes(f)));
}

/** render redraws tabs, group cards and the selection summary. */
function render() {
  const r = state.report;
  for (const tab of ['folder', 'file']) {
    $('tab-' + tab).setAttribute('aria-selected', String(state.tab === tab));
    $('count-' + tab).textContent = `(${(tab === 'folder' ? r.folderGroups : r.fileGroups).length})`;
  }
  const groups = visibleGroups();
  const box = $('groups');
  box.replaceChildren(...groups.slice(0, state.shown).map(renderGroup));
  $('more').hidden = groups.length <= state.shown;
  $('more').textContent = `Show more (${(groups.length - state.shown).toLocaleString()} left)`;
  $('empty').hidden = groups.length > 0;
  renderSelection();
}

/** splitPath returns [folder part, name] for display. */
function splitPath(p) {
  const i = p.lastIndexOf(state.sep);
  return i < 0 ? ['', p] : [p.slice(0, i + 1), p.slice(i + 1)];
}

/** renderGroup draws one duplicate group card. */
function renderGroup(g) {
  const copies = g.entries.length;
  const head = el('div', { class: 'ghead' },
    el('b', {}, g.kind === 'folder' ? `${copies} identical folders` : `${copies} identical files`),
    el('span', {}, `${fmtBytes(g.size)} each`),
    g.kind === 'folder' ? el('span', { class: 'muted' }, `${g.fileCount.toLocaleString()} files inside`) : null,
    el('span', { class: 'muted' }, `${fmtBytes(g.size * (copies - 1))} reclaimable`),
    state.report.mode === 'fast' ? el('span', { class: 'muted' }, 'name + size match (contents verified before removal)') : null,
  );
  const card = el('div', { class: 'group' }, head);
  for (const e of g.entries) card.append(renderEntry(g, e));
  return card;
}

/** renderEntry draws one copy row with its remove checkbox. */
function renderEntry(g, e) {
  const sel = state.selected.has(e.id);
  const [dir, name] = splitPath(e.path);
  const box = el('input', {
    type: 'checkbox', checked: sel, 'aria-label': 'Remove ' + e.path,
    onchange: (ev) => toggle(g, e, ev.target),
  });
  const keepOnly = el('button', { class: 'ghost small', type: 'button', onclick: () => keepOnlyThis(g, e) }, 'Keep only this');
  return el('div', { class: 'entry' + (sel ? ' remove' : '') },
    box,
    el('div', {},
      el('div', { class: 'path' }, el('span', { class: 'dir' }, dir), name),
      el('div', { class: 'meta' }, `Modified ${fmtDate(e.modTime)}`),
    ),
    el('div', { class: 'entry-side' },
      el('span', { class: 'badge ' + (sel ? 'remove' : 'keep') }, sel ? 'remove' : 'keep'),
      keepOnly,
    ),
  );
}

/**
 * toggle ticks/unticks one copy, refusing to tick the last unticked copy of
 * a group (the server enforces the same rule).
 */
function toggle(g, e, box) {
  if (box.checked) {
    const remaining = g.entries.filter((x) => x.id !== e.id && !state.selected.has(x.id)).length;
    if (remaining === 0) {
      box.checked = false;
      toast('At least one copy in each group must be kept.');
      return;
    }
    state.selected.add(e.id);
  } else {
    state.selected.delete(e.id);
  }
  render();
}

/** keepOnlyThis ticks every other copy in the group. */
function keepOnlyThis(g, e) {
  for (const x of g.entries) {
    if (x.id === e.id) state.selected.delete(x.id);
    else state.selected.add(x.id);
  }
  render();
}

/** renderSelection updates the action bar's count and size. */
function renderSelection() {
  let n = 0, bytes = 0;
  for (const g of allGroups()) for (const e of g.entries) if (state.selected.has(e.id)) { n++; bytes += e.size; }
  $('selinfo').textContent = n ? `${n.toLocaleString()} selected · up to ${fmtBytes(bytes)}` : 'Nothing selected';
  for (const id of ['preview', 'trash', 'delete']) $(id).disabled = n === 0;
}

// ---------- Actions ----------

/** runClean sends the selection to the server and shows the outcome. */
async function runClean(action, apply, confirm) {
  setBusy(true);
  try {
    const data = await api('/api/clean', { remove: [...state.selected], action, apply, confirm: confirm || '' });
    showResults(data.result, data.error);
    if (apply) {
      for (const o of data.result.outcomes) if (o.status === 'removed') state.selected.delete(o.entryId);
      await loadReport();
    }
  } catch (e) {
    showError(e.message);
  } finally {
    setBusy(false);
  }
}

/** setBusy disables the action buttons while a request runs. */
function setBusy(b) {
  for (const id of ['preview', 'trash', 'delete', 'auto']) $(id).disabled = b;
  if (!b) renderSelection();
}

/** showResults draws the outcome summary and per-copy table. */
function showResults(res, err) {
  const box = $('results');
  const labels = { 'would-remove': 'would remove', removed: 'removed', refused: 'refused', failed: 'failed', 'in-folder': 'with folder' };
  const where = res.action === 'trash' ? `to the ${TRASH}` : 'permanently';
  const title = res.applied
    ? `Removed ${res.removed} item(s) ${where}, freeing ${fmtBytes(res.bytesFreed)}.`
    : `Dry run: ${res.wouldRemove} item(s) would be removed (${fmtBytes(res.bytesWouldFree)}). Nothing was changed.`;
  const rows = res.outcomes.map((o) => el('tr', {},
    el('td', { class: 'st ' + o.status }, labels[o.status] || o.status),
    el('td', { class: 'path' }, o.path),
    el('td', { class: 'muted' }, o.reason || (o.keptPath ? 'kept: ' + o.keptPath : '')),
  ));
  // replaceChildren would print a literal "null", so drop absent parts first.
  box.replaceChildren(...[
    el('h2', {}, title),
    (res.refused || res.failed) ? el('p', {}, `${res.refused} refused and ${res.failed} failed — they were left untouched. Reasons are listed below.`) : null,
    err ? el('p', { class: 'muted' }, err) : null,
    res.logPath ? el('p', { class: 'muted' }, 'Action log: ' + res.logPath) : null,
    el('button', { class: 'ghost small', type: 'button', onclick: () => { box.hidden = true; } }, 'Dismiss'),
    el('table', { class: 'results-table' }, el('tbody', {}, ...rows)),
  ].filter(Boolean));
  box.hidden = false;
  box.scrollIntoView({ behavior: 'smooth', block: 'start' });
}

/** wire connects all buttons and inputs to their handlers. */
function wire() {
  for (const tab of ['folder', 'file']) {
    $('tab-' + tab).addEventListener('click', () => { state.tab = tab; state.shown = PAGE; render(); });
  }
  $('filter').addEventListener('input', (ev) => { state.filter = ev.target.value; state.shown = PAGE; render(); });
  $('more').addEventListener('click', () => { state.shown += PAGE; render(); });
  $('clear').addEventListener('click', () => { state.selected.clear(); render(); });
  $('auto').addEventListener('click', async () => {
    const prefer = $('prefer').value.trim();
    try {
      const data = await api('/api/select', { strategy: $('strategy').value, prefer: prefer ? [prefer] : [], kinds: 'all' });
      state.selected = new Set(data.remove);
      render();
      toast(`Selected ${data.remove.length.toLocaleString()} copies for removal.`);
    } catch (e) { showError(e.message); }
  });
  $('preview').addEventListener('click', () => runClean('trash', false));
  $('trash').addEventListener('click', () => {
    if (window.confirm(`Move ${state.selected.size} selected item(s) to the ${TRASH}? You can restore them from there.`)) {
      runClean('trash', true);
    }
  });

  // Permanent delete: the confirm button unlocks only when DELETE is typed.
  const dlg = $('confirm');
  $('delete').addEventListener('click', () => {
    $('confirm-text').textContent = `${state.selected.size} selected item(s) will be permanently deleted after being re-checked.`;
    $('confirm-input').value = '';
    $('confirm-go').disabled = true;
    dlg.showModal();
  });
  $('confirm-input').addEventListener('input', (ev) => { $('confirm-go').disabled = ev.target.value !== 'DELETE'; });
  $('confirm-cancel').addEventListener('click', () => dlg.close());
  $('confirm-go').addEventListener('click', () => {
    const typed = $('confirm-input').value;
    dlg.close();
    runClean('delete', true, typed);
  });

  $('quit').addEventListener('click', async () => {
    try { await api('/api/shutdown', {}); } catch (_) { /* server may already be gone */ }
    document.body.replaceChildren(el('main', {}, el('section', { class: 'panel' },
      el('h2', {}, 'Twinsweep has stopped.'), el('p', { class: 'muted' }, 'You can close this tab.'))));
  });
}

wire();
pollStatus();
