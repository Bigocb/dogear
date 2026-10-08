const $ = (sel, el = document) => el.querySelector(sel);
const $$ = (sel, el = document) => [...el.querySelectorAll(sel)];

async function api(path, opts = {}) {
  const res = await fetch('/api' + path, {
    headers: { 'Content-Type': 'application/json' },
    ...opts,
  });
  if (!res.ok) {
    let msg = res.statusText;
    try { msg = (await res.json()).error || msg; } catch {}
    throw new Error(msg || `Request failed (${res.status})`);
  }
  if (res.status === 204) return null;
  return res.json();
}

function escapeHtml(s) { const d = document.createElement('div'); d.textContent = s ?? ''; return d.innerHTML; }

// gate: admin only
let me = null;
try { me = await api('/auth/me'); } catch {}
if (!me) { location.href = '/login'; }
else if (me.role !== 'admin') { document.body.innerHTML = '<main><div class="empty-state"><h3>Admins only</h3><p>Ask an admin for access.</p><a class="btn" href="/">Back to your library</a></div></main>'; }

// ---- toasts ----
function toast(msg, kind = '') {
  const t = document.createElement('div');
  t.className = `toast ${kind}`;
  t.textContent = msg;
  $('#toasts').appendChild(t);
  setTimeout(() => t.remove(), kind === 'error' ? 5200 : 3000);
}

// two-tap confirm for destructive buttons
function armed(btn, prompt) {
  if (btn.classList.contains('armed')) return true;
  const label = btn.textContent;
  btn.classList.add('armed');
  btn.textContent = prompt;
  setTimeout(() => { btn.classList.remove('armed'); btn.textContent = label; }, 3500);
  return false;
}

// ---- users ----

async function loadUsers() {
  const users = await api('/admin/users');
  const list = $('#users-list');
  list.innerHTML = '';
  for (const u of users) {
    const self = me && u.username === me.username;
    const row = document.createElement('div');
    row.className = 'admin-row';
    row.innerHTML = `
      <div class="grow"><strong>${escapeHtml(u.username)}</strong>
        <span class="you">${u.role === 'admin' ? 'Admin' : 'Reader'}${self ? ' · you' : ''}</span></div>
      <button class="mini" data-resetpw="${u.id}">Set password</button>
      ${self ? '' : `<button class="mini" data-role="${u.id}" data-cur="${u.role}">${u.role === 'admin' ? 'Make reader' : 'Make admin'}</button>
      <button class="mini danger" data-del="${u.id}">Delete</button>`}
    `;
    list.appendChild(row);
  }
  list.onclick = async (e) => {
    const t = e.target.closest('button');
    if (!t) return;
    const { resetpw: reset, role, del } = t.dataset;
    try {
      if (reset) {
        const row = t.closest('.admin-row');
        if (row.querySelector('.pw-inline')) return;
        const form = document.createElement('form');
        form.className = 'pw-inline';
        form.style.cssText = 'display:flex;gap:8px;width:100%';
        form.innerHTML = `<input class="admin-input" type="password" placeholder="New password" required style="flex:1;min-width:0">
          <button class="mini" type="submit">Save</button><button class="mini" type="button" data-cancel>Cancel</button>`;
        row.appendChild(form);
        form.querySelector('input').focus();
        form.querySelector('[data-cancel]').onclick = () => form.remove();
        form.onsubmit = async (ev) => {
          ev.preventDefault();
          try {
            await api(`/admin/users/${reset}`, { method: 'PUT', body: JSON.stringify({ password: form.querySelector('input').value }) });
            form.remove();
            toast('Password updated');
          } catch (err) { toast(err.message, 'error'); }
        };
      } else if (role) {
        await api(`/admin/users/${role}`, { method: 'PUT', body: JSON.stringify({ role: t.dataset.cur === 'admin' ? 'user' : 'admin' }) });
        loadUsers();
      } else if (del) {
        if (!armed(t, 'Tap again to delete')) return;
        await api(`/admin/users/${del}`, { method: 'DELETE' });
        toast('Deleted. Their shelf and reading progress are gone.');
        loadUsers();
      }
    } catch (err) { toast(err.message, 'error'); }
  };
}

$('#user-add-form').onsubmit = async (e) => {
  e.preventDefault();
  try {
    const username = $('#nu-username').value.trim();
    await api('/admin/users', { method: 'POST', body: JSON.stringify({
      username,
      password: $('#nu-password').value,
      role: $('#nu-role').value,
    })});
    $('#nu-username').value = ''; $('#nu-password').value = '';
    toast(`Added ${username}`);
    loadUsers();
  } catch (err) { toast(err.message, 'error'); }
};

// ---- system status + activity ----

const row = (label, value, cls = '') => `<div class="admin-row"><div class="grow">${label}</div><span class="val ${cls}">${value}</span></div>`;

// /library/Author/Title/Title.epub -> { title, author }
function describePath(p) {
  const parts = String(p).split('/').filter(Boolean);
  const file = parts[parts.length - 1] || p;
  const title = file.replace(/\.[^.]+$/, '');
  const author = parts.length >= 3 ? parts[parts.length - 3] : '';
  return { title, author };
}

function ago(iso) {
  const s = Math.round((Date.now() - new Date(iso)) / 1000);
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  return new Date(iso).toLocaleDateString();
}

async function loadStatus() {
  const s = await api('/admin/status');
  const aa = s.aa || {};
  const hc = s.hardcover || {};
  const abs = s.abs || {};
  const counts = s.counts || {};
  $('#sys-status').innerHTML =
    row('Hardcover', hc.configured ? 'Connected' : 'Using Open Library', hc.configured ? 'status-ok' : '') +
    row("Anna's Archive key", aa.key_configured ? 'Set' : 'Not set', aa.key_configured ? 'status-ok' : 'status-bad') +
    row('Audiobookshelf', abs.configured ? `${abs.matched || 0} matched` : 'Off', abs.configured ? 'status-ok' : '') +
    row('Books', counts.books ?? '—') +
    row('Book files', counts.files ?? '—') +
    row('Downloads requested', counts.grabs ?? '—') +
    row('Watched folders', (s.ingest_dirs || []).map(escapeHtml).join('<br>') || '—') +
    row('Library folder', escapeHtml(s.library_dir || '—')) +
    row('Keep originals after import', s.copy_mode ? 'On' : 'Off');

  const acts = s.activity || []; // newest first
  $('#activity-feed').innerHTML = acts.length
    ? acts.map(a => {
        const isImport = a.event === 'import';
        const d = isImport ? describePath(a.detail) : { title: a.detail, author: '' };
        return `<div class="act-row">
          <span class="ev">${isImport ? 'Imported' : 'Download'}</span>
          <span class="what">${escapeHtml(d.title)}${d.author ? ` <span class="hint">· ${escapeHtml(d.author)}</span>` : ''}</span>
          <span class="when" title="${escapeHtml(a.detail)}">${ago(a.time)}</span>
        </div>`;
      }).join('')
    : '<div class="empty">Nothing imported or downloaded yet.</div>';
}

// ---- acquisition sources (integrations) ----

async function loadSettings() {
  let s;
  try { s = await api('/admin/settings'); } catch (e) { return toast(e.message, 'error'); }
  $('#prowlarr-enabled').checked = !!s.prowlarr_enabled;
  $('#prowlarr-url').value = s.prowlarr_url || '';
  $('#prowlarr-key').value = '';
  $('#prowlarr-key').placeholder = s.prowlarr_api_key_set ? '•••••• (set — leave blank to keep)' : 'from Prowlarr → Settings → General';
  $('#prowlarr-state').textContent = s.prowlarr_api_key_set && s.prowlarr_url ? (s.prowlarr_enabled ? 'On' : 'Off') : 'Not configured';
  $('#prowlarr-state').className = 'intg-state ' + (s.prowlarr_api_key_set && s.prowlarr_url && s.prowlarr_enabled ? 'status-ok' : '');

  $('#aa-key').value = '';
  $('#aa-key').placeholder = s.aa_donator_key_set ? '•••••• (set — leave blank to keep)' : 'optional';
  $('#aa-base').value = s.aa_base_url || '';
  $('#aa-state').textContent = s.aa_donator_key_set ? 'Set' : 'Not set';
  $('#aa-state').className = 'intg-state ' + (s.aa_donator_key_set ? 'status-ok' : '');

  $('#hardcover-token').value = '';
  $('#hardcover-token').placeholder = s.hardcover_token_set ? '•••••• (set — leave blank to keep)' : 'optional';
  $('#hardcover-state').textContent = s.hardcover_token_set ? 'Set' : 'Using Open Library';
  $('#hardcover-state').className = 'intg-state ' + (s.hardcover_token_set ? 'status-ok' : '');

  $('#abs-enabled').checked = !!s.abs_enabled;
  $('#abs-url').value = s.abs_url || '';
  $('#abs-key').value = '';
  $('#abs-key').placeholder = s.abs_api_key_set ? '•••••• (set — leave blank to keep)' : 'from ABS → Settings → Users';
  $('#abs-state').textContent = s.abs_url && s.abs_api_key_set ? (s.abs_enabled ? 'On' : 'Off') : 'Not configured';
  $('#abs-state').className = 'intg-state ' + (s.abs_url && s.abs_api_key_set && s.abs_enabled ? 'status-ok' : '');
  loadABSLibraries(s.abs_library_id || '').catch(() => {});

  $('#tts-url').value = s.tts_url || '';
  $('#tts-state').textContent = s.tts_url ? 'Ready' : 'Not configured';
  $('#tts-state').className = 'intg-state ' + (s.tts_url ? 'status-ok' : '');
}

$('#save-settings').onclick = async (e) => {
  const btn = e.target;
  btn.disabled = true;
  try {
    const body = {
      prowlarr_enabled: $('#prowlarr-enabled').checked,
      prowlarr_url: $('#prowlarr-url').value.trim(),
      aa_base_url: $('#aa-base').value.trim(),
      tts_url: $('#tts-url').value.trim(),
      abs_enabled: $('#abs-enabled').checked,
      abs_url: $('#abs-url').value.trim(),
      abs_library_id: $('#abs-library').value || '',
    };
    // only send secrets when typed, so blank leaves them untouched
    if ($('#prowlarr-key').value) body.prowlarr_api_key = $('#prowlarr-key').value.trim();
    if ($('#aa-key').value) body.aa_donator_key = $('#aa-key').value.trim();
    if ($('#hardcover-token').value) body.hardcover_token = $('#hardcover-token').value.trim();
    if ($('#abs-key').value) body.abs_api_key = $('#abs-key').value.trim();
    await api('/admin/settings', { method: 'PUT', body: JSON.stringify(body) });
    toast('Sources saved');
    await loadSettings();
    loadStatus().catch(() => {});
  } catch (err) { toast(err.message, 'error'); }
  finally { btn.disabled = false; }
};

$$('#admin-root [data-test]').forEach(btn => {
  btn.onclick = async () => {
    const svc = btn.dataset.test;
    const out = $(`#${svc}-test`);
    btn.disabled = true;
    out.textContent = 'Testing…';
    try {
      const r = await api(`/admin/test/${svc}`, { method: 'POST', body: '{}' });
      out.textContent = (r.ok ? '✓ ' : '✕ ') + (r.detail || '');
      out.className = 'test-out hint ' + (r.ok ? 'status-ok' : 'status-bad');
    } catch (e) {
      out.textContent = '✕ ' + e.message;
      out.className = 'test-out hint status-bad';
    } finally { btn.disabled = false; }
  };
});

if (me && me.role === 'admin') {
  loadUsers().catch(e => toast(e.message, 'error'));
  loadSettings().catch(e => toast(e.message, 'error'));
  loadStatus().catch(e => toast(e.message, 'error'));
  loadWantedWatch().catch(() => {});
}

// ---- wanted-list watcher ----

async function loadWantedWatch() {
  let d;
  try { d = await api('/admin/wanted-watch'); } catch (e) { return; }
  const el = $('#wanted-watch');
  const wants = d.wanted || [];
  if (!wants.length) {
    el.innerHTML = '<div class="hint">Nothing on the wanted list right now.</div>';
    return;
  }
  el.innerHTML = wants.map(w => `
    <div class="admin-row">
      <div class="grow">${escapeHtml(w.title)}</div>
      <span class="val ${w.auto_grab ? 'status-ok' : ''}">${w.auto_grab ? 'Auto' : 'Off'}</span>
    </div>`).join('');
}

const runWatch = $('#btn-run-watch');
if (runWatch) {
  runWatch.onclick = async () => {
    runWatch.disabled = true;
    const orig = runWatch.textContent;
    runWatch.textContent = 'Checking…';
    $('#watch-result').textContent = 'Searching for wanted books now…';
    try {
      await api('/admin/wanted-watch', { method: 'POST', body: JSON.stringify({}) });
      $('#watch-result').textContent = 'Started. Results will appear as books download.';
      setTimeout(loadWantedWatch, 3000);
    } catch (e) {
      $('#watch-result').textContent = '✕ ' + e.message;
    } finally {
      runWatch.disabled = false;
      runWatch.textContent = orig;
    }
  };
}

// ---- metadata enrichment batch ----
const btnEnrich = $('#btn-enrich-all');
if (btnEnrich) {
  let poll = null;
  const render = (job, { justStarted = false } = {}) => {
    const el = $('#enrich-result');
    if (!job) { el.textContent = ''; return; }
    if (!job.total) { el.textContent = justStarted ? 'Every book already has details.' : ''; return; }
    el.textContent = job.running
      ? `Fetching… ${job.done} of ${job.total} (${job.failed} not found)`
      : `Done. Updated ${job.total - job.failed} of ${job.total}; ${job.failed} had no match.`;
    btnEnrich.disabled = !!job.running;
  };
  btnEnrich.onclick = async () => {
    try {
      const res = await api('/admin/enrich', { method: 'POST', body: JSON.stringify({}) });
      const job = res.job || { running: true, total: res.total || 0, done: 0, failed: 0 };
      render(job, { justStarted: true });
      if (!job.total) return;
      if (poll) clearInterval(poll);
      poll = setInterval(async () => {
        try {
          const j = await api('/admin/enrich');
          render(j);
          if (!j?.running) { clearInterval(poll); btnEnrich.disabled = false; loadStatus().catch(() => {}); }
        } catch { clearInterval(poll); }
      }, 2000);
    } catch (e) {
      $('#enrich-result').textContent = `Couldn't start: ${e.message}`;
    }
  };
  api('/admin/enrich').then(j => render(j)).catch(() => {});
}

// ---- audiobookshelf ----
let absLibraries = [];

async function loadABSLibraries(selectID = '') {
  const sel = $('#abs-library');
  try {
    const d = await api('/admin/abs/libraries');
    absLibraries = d.libraries || [];
    const prev = selectID || sel.value;
    sel.innerHTML = absLibraries.map(l => `<option value="${escapeHtml(l.id)}">${escapeHtml(l.name)}</option>`).join('') +
      '<option value="">Auto-select</option>';
    if (prev && absLibraries.some(l => l.id === prev)) sel.value = prev;
    else if (absLibraries.length === 1) sel.value = absLibraries[0].id;
  } catch (e) {
    sel.innerHTML = '<option value="">Unable to load libraries</option>';
  }
}

const absURL = $('#abs-url');
const absKey = $('#abs-key');
if (absURL) {
  async function refreshLibs() {
    if (!absURL.value.trim() || !absKey.value.trim()) return;
    // save temporarily so the server can use the new credentials
    try {
      await api('/admin/settings', { method: 'PUT', body: JSON.stringify({
        abs_url: absURL.value.trim(),
        abs_api_key: absKey.value.trim(),
        abs_enabled: $('#abs-enabled').checked,
        abs_library_id: $('#abs-library').value || ''
      })});
      await loadABSLibraries($('#abs-library').value || '');
    } catch {}
  }
  absKey.addEventListener('change', refreshLibs);
  absURL.addEventListener('change', refreshLibs);
}

const btnAbsSync = $('#btn-abs-sync');
if (btnAbsSync) {
  btnAbsSync.onclick = async () => {
    btnAbsSync.disabled = true;
    const orig = btnAbsSync.textContent;
    btnAbsSync.textContent = 'Syncing…';
    $('#abs-sync-result').textContent = '';
    try {
      const r = await api('/admin/abs/sync', { method: 'POST', body: JSON.stringify({}) });
      $('#abs-sync-result').textContent = `✓ Synced ${r.synced} items · matched ${r.matched}`;
      $('#abs-sync-result').className = 'test-out hint status-ok';
      loadStatus().catch(() => {});
    } catch (e) {
      $('#abs-sync-result').textContent = '✕ ' + e.message;
      $('#abs-sync-result').className = 'test-out hint status-bad';
    } finally {
      btnAbsSync.disabled = false;
      btnAbsSync.textContent = orig;
    }
  };
}

if (me && me.role === 'admin') {
  loadABSLibraries().catch(() => {});
}
