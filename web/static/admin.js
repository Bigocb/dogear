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
    throw new Error(`${res.status}: ${msg}`);
  }
  if (res.status === 204) return null;
  return res.json();
}

function escapeHtml(s) { const d = document.createElement('div'); d.textContent = s ?? ''; return d.innerHTML; }

// gate: admin only
let me = null;
try { me = await api('/auth/me'); } catch {}
if (!me) { location.href = '/login'; }
else if (me.role !== 'admin') { document.body.innerHTML = '<main><div class="empty">Admins only. <a href="/">← Back</a></div></main>'; }

// ---- users ----

async function loadUsers() {
  const users = await api('/admin/users');
  const list = $('#users-list');
  list.innerHTML = '';
  for (const u of users) {
    const row = document.createElement('div');
    row.className = 'admin-row';
    row.innerHTML = `
      <div class="grow"><strong>${escapeHtml(u.username)}</strong> <span class="muted">(${u.role})</span></div>
      <button class="mini" data-resetpw="${u.id}">Reset password</button>
      <button class="mini" data-role="${u.id}" data-cur="${u.role}">${u.role === 'admin' ? 'Demote' : 'Make admin'}</button>
      <button class="mini danger" data-del="${u.id}">Delete</button>
    `;
    list.appendChild(row);
  }
  list.onclick = async (e) => {
    const reset = e.target.dataset.resetpw;
    const role = e.target.dataset.role;
    const del = e.target.dataset.del;
    try {
      if (reset) {
        const pw = prompt('New password for this user:');
        if (pw) await api(`/admin/users/${reset}`, { method: 'PUT', body: JSON.stringify({ password: pw }) });
        alert('Password updated');
      } else if (role) {
        const cur = e.target.dataset.cur;
        await api(`/admin/users/${role}`, { method: 'PUT', body: JSON.stringify({ role: cur === 'admin' ? 'user' : 'admin' }) });
        loadUsers();
      } else if (del) {
        if (!confirm('Delete this user? Their shelf items and progress disappear.')) return;
        await api(`/admin/users/${del}`, { method: 'DELETE' });
        loadUsers();
      }
    } catch (err) { alert(err.message); }
  };
}

$('#user-add-form').onsubmit = async (e) => {
  e.preventDefault();
  try {
    await api('/admin/users', { method: 'POST', body: JSON.stringify({
      username: $('#nu-username').value.trim(),
      password: $('#nu-password').value,
      role: $('#nu-role').value,
    })});
    $('#nu-username').value = ''; $('#nu-password').value = '';
    loadUsers();
  } catch (err) { alert(err.message); }
};

// ---- system status ----

async function loadStatus() {
  const s = await api('/admin/status');
  const sm = s.shelfmark || {};
  const aa = s.aa || {};
  const counts = s.counts || {};
  $('#sys-status').innerHTML = `
    <div class="admin-row"><div class="grow">Shelfmark</div><span class="${sm.reachable ? 'status-ok' : 'status-bad'}">${sm.reachable ? '✓ reachable' : '✕ unreachable'}</span></div>
    <div class="admin-row"><div class="grow">Anna's Archive key</div><span class="${aa.key_configured ? 'status-ok' : 'status-bad'}">${aa.key_configured ? '✓ configured' : '✕ missing'}</span></div>
    <div class="admin-row"><div class="grow">Library books</div><span>${counts.books ?? '—'}</span></div>
    <div class="admin-row"><div class="grow">Library files</div><span>${counts.files ?? '—'}</span></div>
    <div class="admin-row"><div class="grow">Grab requests</div><span>${counts.grabs ?? '—'}</span></div>
    <div class="admin-row"><div class="grow">Watched ingest dirs</div><span class="muted">${(s.ingest_dirs || []).map(escapeHtml).join(', ')}</span></div>
    <div class="admin-row"><div class="grow">Library folder</div><span class="muted">${escapeHtml(s.library_dir || '')}</span></div>
    <div class="admin-row"><div class="grow">Copy mode (originals kept)</div><span>${s.copy_mode ? '✓ on' : 'off'}</span></div>
  `;
}

// ---- activity ----

async function loadActivity() {
  const s = await api('/admin/status');
  const acts = (s.activity || []).slice().reverse();
  const feed = $('#activity-feed');
  feed.innerHTML = acts.length
    ? acts.map(a => `
      <div class="admin-row"><span class="muted" style="min-width:150px">${new Date(a.time).toLocaleString()}</span>
      <strong style="min-width:60px">${a.event}</strong><span class="muted" style="flex:1">${escapeHtml(a.detail.slice(0, 90))}</span></div>`).join('')
    : '<div class="empty">No activity yet.</div>';
}

loadUsers(); loadStatus(); loadActivity();

// metadata enrichment batch
const btnEnrich = $('#btn-enrich-all');
if (btnEnrich) {
  let poll = null;
  const render = (job) => {
    const el = $('#enrich-result');
    if (!job || !job.total) { el.textContent = 'Nothing to enrich — every book already has metadata.'; return; }
    el.textContent = job.running
      ? `Enriching… ${job.done}/${job.total} (${job.failed} had no match)`
      : `✓ Done — ${job.total - job.failed} enriched, ${job.failed} had no match.`;
  };
  btnEnrich.onclick = async () => {
    try {
      const res = await api('/admin/enrich', { method: 'POST', body: JSON.stringify({}) });
      render({ running: true, total: res.total || 0, done: 0, failed: 0 });
      // poll progress
      if (poll) clearInterval(poll);
      poll = setInterval(async () => {
        try {
          const job = await api('/admin/enrich');
          render(job);
          if (!job.running) clearInterval(poll);
        } catch { clearInterval(poll); }
      }, 2000);
    } catch (e) {
      $('#enrich-result').textContent = '✕ ' + e.message;
    }
  };
  // show current state on load
  api('/admin/enrich').then(render).catch(() => {});
}