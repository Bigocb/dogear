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
  const sm = s.shelfmark || {};
  const aa = s.aa || {};
  const counts = s.counts || {};
  $('#sys-status').innerHTML =
    row('Shelfmark', sm.reachable ? 'Connected' : 'Not reachable', sm.reachable ? 'status-ok' : 'status-bad') +
    row("Anna's Archive key", aa.key_configured ? 'Set' : 'Not set', aa.key_configured ? 'status-ok' : 'status-bad') +
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

if (me && me.role === 'admin') {
  loadUsers().catch(e => toast(e.message, 'error'));
  loadStatus().catch(e => toast(e.message, 'error'));
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
