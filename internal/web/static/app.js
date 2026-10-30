/*
  Twinsweep browser app logic (plain JavaScript, no frameworks).

  Screens:
    Scan     - choose folders (typed or via the folder picker) and options,
               start/cancel a scan, or open a saved report file.
    Review   - scan statistics, export (JSON/CSV), rescan, auto-select, and
               one card per duplicate group with a "remove" tick per copy.
    Activity - the action log (what was removed, when, and what was kept).

  A single status loop polls /api/status about once a second (faster while
  something is running). It drives the scan progress panel, the cleanup
  progress dialog, and reloads the report whenever the server says it
  changed (scan finished, report loaded, copies removed).

  Cleanup runs on the server as a background job. The page starts it with
  POST /api/clean, then shows the progress dialog until the job finishes,
  with a Stop button that asks the server to stop before the next item.

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
  view: null,           // 'scan' | 'review' | 'log'
  status: null,         // latest /api/status response
  report: null,         // the live report from the server
  reportVersion: 0,     // which server report version we have loaded
  reclaimable: 0,
  tab: 'folder',        // review tab: 'folder' or 'file'
  selected: new Set(),  // entry IDs ticked for removal
  filter: '',
  shown: PAGE,
  sep: '/',             // path separator detected from the report
  roots: [],            // scan form folder list
  formFilled: false,    // scan form prefilled from server once
  wasScanning: false,
  shownJob: 0,          // cleanup job ID whose final results were already shown
  watchingJob: 0,       // cleanup job ID the progress dialog is following
  picker: { path: '', parent: null },
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

/** fill replaces a node's children, skipping null/false entries. */
function fill(node, ...children) {
  node.replaceChildren(...children.flat().filter((c) => c != null && c !== false));
}

/** fmtBytes: 1536 -> "1.5 KB" (1024-based, like Explorer/Finder). */
function fmtBytes(n) {
  const u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  let i = 0;
  n = n || 0;
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

/** fmtDuration: milliseconds -> "1m 42s". */
function fmtDuration(ms) {
  const s = Math.round((ms || 0) / 1000);
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${s % 60}s`;
}

/** toast shows a short message for a few seconds. */
function toast(msg) {
  const t = el('div', { class: 'toast', role: 'status' }, msg);
  document.body.append(t);
  setTimeout(() => t.remove(), 3500);
}

/** notice shows (or with no message, hides) the banner under the header. */
function notice(msg, kind) {
  const box = $('notice');
  if (!msg) { box.hidden = true; return; }
  box.className = 'panel notice ' + (kind || '');
  fill(box, el('span', {}, msg), el('button', { class: 'ghost small', type: 'button', onclick: () => { box.hidden = true; } }, 'Dismiss'));
  box.hidden = false;
}

/**
 * api calls the server. With a body it's a POST carrying the CSRF token
 * header (a string body is sent as-is, anything else as JSON). Throws an
 * Error with the server's message on failure.
 */
async function api(path, body) {
  const init = { credentials: 'same-origin' };
  if (body !== undefined) {
    init.method = 'POST';
    init.headers = { 'Content-Type': 'application/json', 'X-Twinsweep-Token': TOKEN };
    init.body = typeof body === 'string' ? body : JSON.stringify(body);
  }
  const r = await fetch(path, init);
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || `${r.status} ${r.statusText}`);
  return data;
}

// =====================================================================
// Navigation
// =====================================================================

/** showView switches between the Scan, Review and Activity screens. */
function showView(name) {
  state.view = name;
  for (const v of ['scan', 'review', 'log']) {
    $('view-' + v).hidden = v !== name;
    $('nav-' + v).setAttribute('aria-current', v === name ? 'page' : 'false');
  }
  $('actionbar').hidden = !(name === 'review' && state.report);
  if (name === 'log') loadLog();
  window.scrollTo(0, 0);
}

// =====================================================================
// Status loop
// =====================================================================

/** tick polls /api/status and updates whatever changed, then reschedules itself. */
async function tick() {
  let st;
  try {
    st = await api('/api/status');
  } catch (e) {
    notice('Lost contact with Twinsweep (' + e.message + '). Is it still running?', 'bad');
    setTimeout(tick, 3000);
    return;
  }
  state.status = st;
  const first = state.view === null;

  // Prefill the scan form once from the last scan's settings.
  if (!state.formFilled && st.lastScan) { applyScanForm(st.lastScan); state.formFilled = true; }

  renderScanProgress(st);

  // A scan just ended: show its error, or go to the new results.
  if (state.wasScanning && !st.scanning) {
    if (st.scanError) notice(st.scanError, st.scanError.startsWith('Scan cancelled') ? '' : 'bad');
    else if (st.reportVersion !== state.reportVersion) state.pendingReview = true;
  }
  state.wasScanning = st.scanning;

  // Reload the report whenever the server's copy changed.
  if (st.hasReport && st.reportVersion !== state.reportVersion) {
    await loadReport(st.reportVersion);
    if (state.pendingReview) { state.pendingReview = false; showView('review'); }
  }

  if (first) showView(st.scanning || !st.hasReport ? 'scan' : 'review');
  renderCleanJob(st.clean);
  $('scan-start').disabled = st.scanning || st.cleaning;

  setTimeout(tick, st.scanning || st.cleaning ? 500 : 1500);
}

// =====================================================================
// Scan screen
// =====================================================================

/** renderScanProgress shows/hides the progress panel and the form. */
function renderScanProgress(st) {
  $('scan-progress').hidden = !st.scanning;
  $('scan-form').hidden = st.scanning;
  if (!st.scanning) return;
  const p = st.progress || {};
  const phases = { walking: 'Listing files…', hashing: 'Comparing file contents…', folders: 'Comparing folders…', done: 'Finishing…' };
  $('scan-phase').textContent = phases[p.phase] || 'Starting…';
  const pct = p.hashTotal ? Math.round((100 * p.hashDone) / p.hashTotal) : 0;
  $('scan-bar').style.width = (p.phase === 'walking' ? 5 : Math.max(5, pct)) + '%';
  $('scan-counts').textContent = `${(p.filesSeen || 0).toLocaleString()} files (${fmtBytes(p.bytesSeen)}) found` +
    (p.hashTotal ? ` · ${p.hashDone.toLocaleString()} of ${p.hashTotal.toLocaleString()} compared` : '');
}

/** applyScanForm fills the form from a scan request object. */
function applyScanForm(q) {
  state.roots = [...(q.roots || [])];
  for (const r of document.querySelectorAll('input[name="mode"]')) r.checked = r.value === (q.mode || 'exact');
  $('opt-folders').checked = q.folders !== false;
  $('opt-defexcl').checked = q.defaultExcludes !== false;
  $('opt-minsize').value = q.minSize || '1';
  $('opt-workers').value = q.workers || 4;
  $('opt-excludes').value = (q.excludes || []).join('\n');
  renderRoots();
}

/** readScanForm collects the form into a scan request object. */
function readScanForm() {
  const lines = (v) => v.split('\n').map((s) => s.trim()).filter(Boolean);
  return {
    roots: state.roots,
    mode: document.querySelector('input[name="mode"]:checked').value,
    folders: $('opt-folders').checked,
    defaultExcludes: $('opt-defexcl').checked,
    minSize: $('opt-minsize').value.trim() || '1',
    workers: parseInt($('opt-workers').value, 10) || 4,
    excludes: lines($('opt-excludes').value),
  };
}

/** renderRoots draws the folder list with move/remove buttons. */
function renderRoots() {
  const ol = $('roots');
  fill(ol, state.roots.map((r, i) => el('li', {},
    el('span', { class: 'path' }, r),
    el('span', { class: 'root-actions' },
      el('button', { class: 'ghost small', type: 'button', title: 'Move up', 'aria-label': 'Move up', disabled: i === 0, onclick: () => moveRoot(i, -1) }, '↑'),
      el('button', { class: 'ghost small', type: 'button', title: 'Move down', 'aria-label': 'Move down', disabled: i === state.roots.length - 1, onclick: () => moveRoot(i, 1) }, '↓'),
      el('button', { class: 'ghost small', type: 'button', title: 'Remove', 'aria-label': 'Remove ' + r, onclick: () => { state.roots.splice(i, 1); renderRoots(); } }, '✕'),
    ),
  )));
  $('roots-empty').hidden = state.roots.length > 0;
}

/** moveRoot swaps folder i with its neighbour (priority order). */
function moveRoot(i, d) {
  const j = i + d;
  [state.roots[i], state.roots[j]] = [state.roots[j], state.roots[i]];
  renderRoots();
}

/** addRoot adds a folder path to the list (ignoring duplicates). */
function addRoot(p) {
  p = (p || '').trim();
  if (!p) return;
  if (!state.roots.includes(p)) state.roots.push(p);
  renderRoots();
}

/** startScan sends the form to the server. */
async function startScan(q) {
  notice();
  try {
    await api('/api/scan', q || readScanForm());
    state.wasScanning = true;
    showView('scan');
    renderScanProgress({ scanning: true, progress: { phase: 'walking' } });
  } catch (e) {
    notice(e.message, 'bad');
  }
}

/** loadReportFile uploads a saved report chosen with the file input. */
async function loadReportFile(file) {
  if (!file) return;
  notice();
  try {
    const text = await file.text();
    const data = await api('/api/report/load', text);
    toast(`Opened ${file.name} (${data.groups.toLocaleString()} groups).`);
    state.pendingReview = true;
  } catch (e) {
    notice(e.message, 'bad');
  }
}

// ---------- folder picker ----------

/** openPicker shows the folder picker at the starting points. */
function openPicker() {
  $('picker').showModal();
  browse('');
}

/** browse lists the subfolders of path ('' = drives/home). */
async function browse(path) {
  $('picker-msg').textContent = '';
  try {
    const data = await api('/api/browse?path=' + encodeURIComponent(path));
    state.picker = { path: data.path, parent: data.parent };
    $('picker-path').textContent = data.path || 'Drives and places';
    $('picker-up').disabled = data.parent === null || data.parent === undefined;
    $('picker-choose').disabled = !data.path;
    fill($('picker-list'), data.dirs.map((d) => el('li', {},
      el('button', { type: 'button', class: 'picker-item', onclick: () => browse(d.path) }, '📁 ', d.name))));
    if (!data.dirs.length) $('picker-msg').textContent = 'No subfolders here.';
    if (data.truncated) $('picker-msg').textContent = 'Showing the first 5,000 folders.';
  } catch (e) {
    $('picker-msg').textContent = e.message;
  }
}

// =====================================================================
// Review screen
// =====================================================================

/** loadReport fetches the live report and redraws the review screen. */
async function loadReport(version) {
  try {
    const data = await api('/api/report');
    state.report = data.report;
    state.reclaimable = data.reclaimable;
    state.reportVersion = version ?? data.version;
  } catch (e) {
    return;
  }
  const any = [...state.report.folderGroups, ...state.report.fileGroups][0];
  state.sep = any && any.entries[0].path.includes('\\') ? '\\' : (state.report.roots[0] || '').includes('\\') ? '\\' : '/';
  // Drop selections for copies that no longer exist.
  const ids = new Set();
  for (const g of allGroups()) for (const e of g.entries) ids.add(e.id);
  for (const id of [...state.selected]) if (!ids.has(id)) state.selected.delete(id);
  if (!state.report.folderGroups.length) state.tab = 'file';
  else if (!state.report.fileGroups.length) state.tab = 'folder';
  renderReview();
}

/** allGroups returns folder + file groups. */
function allGroups() {
  return state.report ? [...state.report.folderGroups, ...state.report.fileGroups] : [];
}

/** renderStats draws the summary card at the top of the review screen. */
function renderStats() {
  const r = state.report, s = r.stats || {};
  const stat = (label, value) => el('div', { class: 'stat' }, el('span', { class: 'stat-value' }, value), el('span', { class: 'stat-label' }, label));
  const errors = s.errors && s.errors.length
    ? el('details', { class: 'errors' }, el('summary', {}, `${s.errorCount.toLocaleString()} item(s) couldn't be read`),
      el('ul', {}, ...s.errors.map((e) => el('li', { class: 'path small' }, e))))
    : null;
  fill($('stats'),
    el('div', { class: 'stats-head' },
      el('div', {},
        el('h2', {}, `${fmtBytes(state.reclaimable)} reclaimable`),
        el('p', { class: 'muted small' }, `${r.mode === 'fast' ? 'Fast (name + size)' : 'Exact'} scan of ${r.roots.length} folder(s), ${fmtDate(r.createdAt)}`),
        el('p', { class: 'path small' }, r.roots.join('  ·  ')),
      ),
      el('div', { class: 'row' },
        el('button', { class: 'ghost small', type: 'button', onclick: () => startScan(state.status.lastScan) }, 'Rescan'),
        el('a', { class: 'button ghost small', href: '/api/report/export?format=json', download: '' }, 'Download report'),
        el('a', { class: 'button ghost small', href: '/api/report/export?format=csv', download: '' }, 'Download CSV'),
      ),
    ),
    el('div', { class: 'stat-grid' },
      stat('duplicate folder groups', r.folderGroups.length.toLocaleString()),
      stat('duplicate file groups', r.fileGroups.length.toLocaleString()),
      stat('files scanned', (s.filesSeen || 0).toLocaleString()),
      stat('data scanned', fmtBytes(s.bytesSeen)),
      stat('files compared', (s.filesHashed || 0).toLocaleString()),
      stat('scan time', fmtDuration(s.durationMs)),
    ),
    (s.skipped || s.excluded || s.hardlinksCollapsed) ? el('p', { class: 'muted small' },
      `Skipped ${s.skipped || 0} links/special items, ${s.excluded || 0} excluded, ${s.tooSmall || 0} below minimum size, ${s.hardlinksCollapsed || 0} hardlinks (already share storage).`) : null,
    errors,
  );
}

/** visibleGroups applies the current tab and path filter. */
function visibleGroups() {
  const list = state.tab === 'folder' ? state.report.folderGroups : state.report.fileGroups;
  const f = state.filter.toLowerCase();
  if (!f) return list;
  return list.filter((g) => g.entries.some((e) => e.path.toLowerCase().includes(f)));
}

/** renderReview redraws the whole review screen. */
function renderReview() {
  const has = !!state.report;
  $('review-empty').hidden = has;
  $('review-body').hidden = !has;
  const total = has ? allGroups().length : 0;
  $('nav-review-count').hidden = !total;
  $('nav-review-count').textContent = total.toLocaleString();
  if (state.view === 'review') $('actionbar').hidden = !has;
  if (!has) return;
  renderStats();
  renderGroups();
}

/** renderGroups redraws tabs, group cards and the selection summary. */
function renderGroups() {
  const r = state.report;
  for (const tab of ['folder', 'file']) {
    $('tab-' + tab).setAttribute('aria-selected', String(state.tab === tab));
    $('count-' + tab).textContent = `(${(tab === 'folder' ? r.folderGroups : r.fileGroups).length})`;
  }
  const groups = visibleGroups();
  fill($('groups'), groups.slice(0, state.shown).map(renderGroup));
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
  return el('div', { class: 'entry' + (sel ? ' remove' : '') },
    el('input', { type: 'checkbox', checked: sel, 'aria-label': 'Remove ' + e.path, onchange: (ev) => toggle(g, e, ev.target) }),
    el('div', {},
      el('div', { class: 'path' }, el('span', { class: 'dir' }, dir), name),
      el('div', { class: 'meta' }, `Modified ${fmtDate(e.modTime)}`),
    ),
    el('div', { class: 'entry-side' },
      el('span', { class: 'badge ' + (sel ? 'remove' : 'keep') }, sel ? 'remove' : 'keep'),
      el('button', { class: 'ghost small', type: 'button', onclick: () => keepOnlyThis(g, e) }, 'Keep only this'),
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
  renderGroups();
}

/** keepOnlyThis ticks every other copy in the group. */
function keepOnlyThis(g, e) {
  for (const x of g.entries) {
    if (x.id === e.id) state.selected.delete(x.id);
    else state.selected.add(x.id);
  }
  renderGroups();
}

/** selectionTotals counts the ticked copies and their total size. */
function selectionTotals() {
  let n = 0, bytes = 0;
  for (const g of allGroups()) for (const e of g.entries) if (state.selected.has(e.id)) { n++; bytes += e.size; }
  return { n, bytes };
}

/** renderSelection updates the action bar's count and size. */
function renderSelection() {
  const { n, bytes } = selectionTotals();
  $('selinfo').textContent = n ? `${n.toLocaleString()} selected · up to ${fmtBytes(bytes)}` : 'Nothing selected';
  const busy = state.status && (state.status.cleaning || state.status.scanning);
  for (const id of ['preview', 'trash', 'delete']) $(id).disabled = n === 0 || busy;
}

/** autoSelect asks the server to apply a keep rule. */
async function autoSelect() {
  const prefer = $('prefer').value.split('\n').map((s) => s.trim()).filter(Boolean);
  try {
    const data = await api('/api/select', { strategy: $('strategy').value, prefer, kinds: $('kinds').value });
    state.selected = new Set(data.remove);
    renderGroups();
    toast(`Selected ${data.remove.length.toLocaleString()} copies for removal.`);
  } catch (e) { notice(e.message, 'bad'); }
}

// =====================================================================
// Cleanup job + progress dialog
// =====================================================================

/** runClean starts a cleanup job on the server and opens the progress dialog. */
async function runClean(action, apply, confirm) {
  try {
    const data = await api('/api/clean', { remove: [...state.selected], action, apply, confirm: confirm || '' });
    state.watchingJob = data.id;
    openProgress();
  } catch (e) {
    notice(e.message, 'bad');
  }
}

/** openProgress shows the dialog in its "running" state. */
function openProgress() {
  const dlg = $('progress');
  // Reset from any previous job instantly (no animated slide back from 100%).
  const bar = $('prog-bar');
  bar.style.transition = 'none';
  bar.style.width = '0%';
  void bar.offsetWidth; // force the browser to apply the reset before re-enabling animation
  bar.style.transition = '';
  $('prog-title').textContent = 'Starting…';
  $('prog-counts').textContent = '';
  $('prog-current').textContent = '';
  fill($('prog-chips'));
  $('prog-stop').hidden = false;
  $('prog-stop').disabled = false;
  $('prog-close').hidden = true;
  fill($('prog-summary'));
  fill($('prog-table'));
  if (!dlg.open) dlg.showModal();
}

/**
 * renderCleanJob updates the progress dialog from the job in /api/status.
 * Also re-attaches to a job that's running when the page is (re)opened.
 */
async function renderCleanJob(job) {
  if (!job) return;
  if (job.running && state.watchingJob !== job.id) { state.watchingJob = job.id; openProgress(); }
  if (state.watchingJob !== job.id) return;
  if (!job.running && state.shownJob === job.id) return;

  const verb = !job.applied ? 'Checking (dry run)' : job.action === 'delete' ? 'Permanently deleting' : `Moving to the ${TRASH}`;
  $('prog-title').textContent = job.running ? `${verb}…` : finishedTitle(job);
  // Progress by bytes when sizes are known (a big folder is most of the work), else by count.
  const pct = job.totalBytes > 0 ? (100 * job.doneBytes) / job.totalBytes : job.total ? (100 * job.done) / job.total : 100;
  $('prog-bar').style.width = Math.min(100, job.running ? Math.max(2, pct) : 100) + '%';
  $('prog-counts').textContent = `${job.done.toLocaleString()} of ${job.total.toLocaleString()} items · ${fmtBytes(job.doneBytes)} of ${fmtBytes(job.totalBytes)}`;
  $('prog-current').textContent = job.running && job.current ? `${capital(job.step)}: ${job.current}` : '';
  const chip = (label, n, cls) => el('span', { class: 'chip ' + cls }, `${label}: ${n.toLocaleString()}`);
  fill($('prog-chips'),
    job.applied ? chip('removed', job.removed, 'ok') : chip('would remove', job.wouldRemove, 'ok'),
    chip('refused', job.refused, job.refused ? 'bad' : ''),
    chip('failed', job.failed, job.failed ? 'bad' : ''),
  );
  renderOutcomes(job.outcomes);

  if (!job.running) {
    // Finished: fetch the full outcome list once and switch to "results".
    state.shownJob = job.id;
    try { job = (await api('/api/clean/status')).job || job; } catch (_) { /* keep partial */ }
    fill($('prog-summary'),
      (job.refused || job.failed) ? el('p', {}, `${job.refused} refused and ${job.failed} failed — they were left untouched. Reasons are listed below.`) : null,
      job.error ? el('p', { class: 'bad-text' }, job.error) : null,
      job.logPath ? el('p', { class: 'muted small' }, 'Action log: ' + job.logPath) : null,
    );
    renderOutcomes(job.outcomes);
    $('prog-stop').hidden = true;
    $('prog-close').hidden = false;
    if (job.applied) for (const o of job.outcomes) if (o.status === 'removed') state.selected.delete(o.entryId);
    renderSelection();
  }
}

/** finishedTitle summarizes a completed job in one line. */
function finishedTitle(job) {
  const stopped = job.stopped ? ' (stopped early)' : '';
  if (!job.applied) return `Dry run: ${job.wouldRemove} item(s) would be removed (${fmtBytes(job.bytesWouldFree)}). Nothing was changed.${stopped}`;
  const where = job.action === 'trash' ? `to the ${TRASH}` : 'permanently';
  return `Removed ${job.removed} item(s) ${where}, freeing ${fmtBytes(job.bytesFreed)}.${stopped}`;
}

/** capital upper-cases the first letter. */
function capital(s) { return s ? s[0].toUpperCase() + s.slice(1) : ''; }

/** renderOutcomes draws the per-copy results table (newest last). */
function renderOutcomes(outcomes) {
  const labels = { 'would-remove': 'would remove', removed: 'removed', refused: 'refused', failed: 'failed', 'in-folder': 'with folder' };
  fill($('prog-table'), el('tbody', {}, ...(outcomes || []).map((o) => el('tr', {},
    el('td', { class: 'st ' + o.status }, labels[o.status] || o.status),
    el('td', { class: 'path' }, o.path),
    el('td', { class: 'muted' }, o.reason || (o.keptPath ? 'kept: ' + o.keptPath : '')),
  ))));
}

// =====================================================================
// Activity screen
// =====================================================================

/** loadLog fetches and draws the action log. */
async function loadLog() {
  try {
    const data = await api('/api/log?lines=300');
    $('log-path').textContent = 'Log file: ' + data.path;
    $('log-empty').hidden = data.entries.length > 0;
    const head = el('thead', {}, el('tr', {}, ...['When', 'Action', 'Result', 'Item', 'Size', 'Kept copy'].map((h) => el('th', {}, h))));
    fill($('log-table'), data.entries.length ? head : null, el('tbody', {}, ...data.entries.map((e) => el('tr', {},
      el('td', { class: 'nowrap' }, fmtDate(e.time)),
      el('td', {}, e.action === 'trash' ? `to ${TRASH}` : 'deleted'),
      el('td', { class: 'st ' + (e.result === 'ok' ? 'removed' : 'failed') }, e.result === 'ok' ? 'ok' : 'error: ' + (e.error || '')),
      el('td', { class: 'path' }, e.path),
      el('td', { class: 'nowrap' }, fmtBytes(e.size)),
      el('td', { class: 'path muted' }, e.keptPath),
    ))));
  } catch (e) {
    notice(e.message, 'bad');
  }
}

// =====================================================================
// Wiring
// =====================================================================

/** wire connects all buttons and inputs to their handlers. */
function wire() {
  for (const v of ['scan', 'review', 'log']) $('nav-' + v).addEventListener('click', () => showView(v));

  // Scan screen
  $('root-add').addEventListener('click', () => { addRoot($('root-input').value); $('root-input').value = ''; });
  $('root-input').addEventListener('keydown', (ev) => { if (ev.key === 'Enter') { addRoot(ev.target.value); ev.target.value = ''; } });
  $('root-browse').addEventListener('click', openPicker);
  $('scan-start').addEventListener('click', () => {
    const typed = $('root-input').value.trim();
    if (typed) { addRoot(typed); $('root-input').value = ''; }
    if (!state.roots.length) { notice('Add at least one folder to scan.', 'bad'); return; }
    startScan();
  });
  $('scan-cancel').addEventListener('click', () => api('/api/scan/cancel', {}).catch((e) => notice(e.message, 'bad')));
  $('load-file').addEventListener('change', (ev) => { loadReportFile(ev.target.files[0]); ev.target.value = ''; });

  // Folder picker
  $('picker-up').addEventListener('click', () => browse(state.picker.parent || ''));
  $('picker-close').addEventListener('click', () => $('picker').close());
  $('picker-cancel').addEventListener('click', () => $('picker').close());
  $('picker-choose').addEventListener('click', () => { addRoot(state.picker.path); $('picker').close(); });

  // Review screen
  for (const tab of ['folder', 'file']) {
    $('tab-' + tab).addEventListener('click', () => { state.tab = tab; state.shown = PAGE; renderGroups(); });
  }
  $('filter').addEventListener('input', (ev) => { state.filter = ev.target.value; state.shown = PAGE; renderGroups(); });
  $('more').addEventListener('click', () => { state.shown += PAGE; renderGroups(); });
  $('clear').addEventListener('click', () => { state.selected.clear(); renderGroups(); });
  $('auto').addEventListener('click', autoSelect);
  $('preview').addEventListener('click', () => runClean('trash', false));
  $('trash').addEventListener('click', () => {
    if (window.confirm(`Move ${state.selected.size} selected item(s) to the ${TRASH}? You can restore them from there.`)) runClean('trash', true);
  });

  // Permanent delete: the confirm button unlocks only when DELETE is typed.
  const dlg = $('confirm');
  $('delete').addEventListener('click', () => {
    const { n, bytes } = selectionTotals();
    $('confirm-text').textContent = `${n} selected item(s), up to ${fmtBytes(bytes)}, will be permanently deleted after being re-checked.`;
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

  // Progress dialog. Escape must not hide a running job's dialog.
  $('progress').addEventListener('cancel', (ev) => { if ($('prog-close').hidden) ev.preventDefault(); });
  $('prog-stop').addEventListener('click', async () => {
    $('prog-stop').disabled = true;
    $('prog-stop').textContent = 'Stopping…';
    try { await api('/api/clean/cancel', {}); } catch (e) { notice(e.message, 'bad'); }
  });
  $('prog-close').addEventListener('click', () => { $('progress').close(); $('prog-stop').textContent = 'Stop after current item'; });

  // Activity
  $('log-refresh').addEventListener('click', loadLog);

  $('quit').addEventListener('click', async () => {
    if (!window.confirm('Stop Twinsweep? Any running scan is cancelled.')) return;
    try {
      await api('/api/shutdown', {});
    } catch (e) { notice(e.message, 'bad'); return; }
    document.body.replaceChildren(el('main', {}, el('section', { class: 'panel' },
      el('h2', {}, 'Twinsweep has stopped.'), el('p', { class: 'muted' }, 'You can close this tab.'))));
  });
}

wire();
tick();
