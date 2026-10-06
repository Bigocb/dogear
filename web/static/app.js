const $ = (sel, el = document) => el.querySelector(sel);
const $$ = (sel, el = document) => [...el.querySelectorAll(sel)];

let currentFilter = '';
let view = 'library'; // 'library' | 'results'
let trackedProviders = new Set(); // "provider:book_id" already in library

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

// ---------- rendering ----------

function actionsFor(b, isResult) {
  if (isResult) {
    const key = `${b.provider}:${b.book_id}`;
    if (trackedProviders.has(key)) {
      return [{label: '✓ In library', cls: 'ghost disabled', fn: null}];
    }
    return [{label: '＋ Add to wanted', fn: addToWanted}];
  }
  // household books (in the shared library, not on my shelf)
  if (b.status === 'library') {
    return [{label: '＋ Add to my shelf', fn: addToShelf}];
  }
  const acts = [];
  switch (b.status) {
    case 'wanted':
      acts.push({label: '⬇ Grab', cls: 'grab-btn', fn: grabBook});
      break;
    case 'grabbed':
      acts.push({label: '⟳ Grabbing…', cls: 'ghost disabled', fn: null});
      acts.push({label: '↺ Re-pick', cls: 'ghost', fn: (b) => openPicker(b)});
      break;
    case 'imported':
      acts.push({label: '📖 Read', fn: openReader});
      break;
    case 'reading':
      acts.push({label: '📖 Continue', fn: openReader});
      acts.push({label: 'Mark read', fn: (b) => setStatus(b, 'read')});
      break;
    case 'read':
      acts.push({label: '↺ Reset', cls: 'ghost', fn: (b) => setStatus(b, 'imported')});
      break;
  }
  acts.push({label: 'ℹ', cls: 'ghost', fn: (b) => showDetail(b)});
  acts.push({label: 'Remove', cls: 'danger', fn: delBook});
  return acts;
}

function cardEl(tpl, b, isResult) {
  const el = tpl.content.firstElementChild.cloneNode(true);
  el.dataset.bookId = b.id || '';
  el.dataset.key = b.provider && b.book_id ? `${b.provider}:${b.book_id}` : '';
  $('.title', el).textContent = b.title || '(untitled)';
  $('.author', el).textContent = b.author || '';
  $('.status', el).textContent = b.status || '';
  const img = $('.cover', el);
  const coverSrc = isResult ? b.cover_url : `/api/covers/${b.id}`;
  if (coverSrc) img.src = coverSrc;
  img.onerror = () => { img.style.visibility = 'hidden'; };

  // tapping the cover/title opens the detail sheet (library books only)
  if (!isResult) {
    const openDetail = (ev) => { ev.preventDefault(); showDetail(b); };
    $('.cover-wrap', el).onclick = openDetail;
    $('.title', el).onclick = openDetail;
  }
  refreshActions(el, b, isResult);
  return el;
}

// ---------- book detail sheet ----------

let detailBook = null;

function showDetail(b) {
  detailBook = b;
  $('#detail-title').textContent = b.title || '';
  $('#detail-author').textContent = b.author || '';
  const cov = $('#detail-cover');
  cov.style.visibility = 'visible';
  cov.src = `/api/covers/${b.id}`;
  cov.onerror = () => { cov.style.visibility = 'hidden'; };
  const facts = [];
  if (b.publish_year) facts.push(b.publish_year);
  if (b.publisher) facts.push(b.publisher);
  if (b.language) facts.push(b.language);
  if (b.series_name) facts.push(`${b.series_name}${b.series_position ? ' #' + b.series_position : ''}`);
  if (b.page_count) facts.push(`${b.page_count} pp`);
  if (b.genres) facts.push(b.genres);
  if (b.isbn) facts.push(`ISBN ${b.isbn}`);
  $('#detail-facts').innerHTML = facts.map(f => `<span class="fact">${escapeHtml(String(f))}</span>`).join('');
  $('#detail-description').textContent = b.description || 'No description yet — tap “Fetch metadata”.';
  $('#detail-read').style.display = ['imported','reading','read'].includes(b.status) ? '' : 'none';
  $('#detail-overlay').hidden = false;
}

$('#detail-close').onclick = () => { $('#detail-overlay').hidden = true; };
$('#detail-overlay').onclick = (e) => { if (e.target.id === 'detail-overlay') $('#detail-overlay').hidden = true; };
$('#detail-read').onclick = () => { if (detailBook) openReader(detailBook); };
$('#detail-enrich').onclick = async () => {
  if (!detailBook) return;
  const btn = $('#detail-enrich');
  btn.disabled = true; btn.textContent = 'Fetching…';
  try {
    const updated = await api(`/books/${detailBook.id}/enrich`, { method: 'POST', body: JSON.stringify({}) });
    detailBook = updated;
    showDetail(updated);
  } catch (e) {
    alert('Metadata fetch failed: ' + e.message);
  } finally {
    btn.disabled = false; btn.textContent = '✨ Fetch metadata';
  }
};

function refreshActions(el, b, isResult) {
  const act = $('.actions', el);
  act.innerHTML = '';
  for (const {label, cls, fn} of actionsFor(b, isResult)) {
    const btn = document.createElement('button');
    if (cls) btn.className = cls;
    btn.textContent = label;
    if (label.includes('disabled')) btn.disabled = true;
    if (fn) btn.onclick = () => handleAction(btn, el, b, isResult, fn);
    act.appendChild(btn);
  }
}

// action wrapper: gives feedback (loading state) then re-renders
async function handleAction(btn, cardEl, b, isResult, fn) {
  if (btn.disabled) return;
  const orig = btn.textContent;
  btn.disabled = true;
  btn.textContent = '…';
  try {
    await fn(b, btn);
    if (isResult) {
      // success on add: flip button to "in library"
      refreshActions(cardEl, b, true);
    } else {
      await loadLibrary();
    }
  } catch (e) {
    alert(e.message);
    btn.disabled = false;
    btn.textContent = orig;
  }
}

// ---------- actions ----------

async function addToWanted(b) {
  await api('/books', {method: 'POST', body: JSON.stringify({
    title: b.title, author: b.author, isbn: b.isbn,
    provider: b.provider, provider_id: b.book_id, cover_url: b.cover_url,
  })});
  await refreshTracked();
}

async function grabBook(b) {
  // open the release picker for manual/best pick
  await openPicker(b);
}

async function openReader(b) {
  // flip to reading state first (fire & forget), then open the reader page
  try { await api(`/books/${b.id}/status`, {method: 'POST', body: JSON.stringify({status: 'reading'})}); } catch {}
  location.href = `/reader?book=${b.id}`;
}

// ---------- release picker ----------

let pickerBook = null;

async function openPicker(b) {
  pickerBook = b;
  $('#picker-title').textContent = 'Pick a release';
  $('#picker-book-label').textContent = `${b.title} — ${b.author || 'unknown'}`;
  $('#picker-list').innerHTML = '<div class="empty">Searching releases…</div>';
  $('#picker-overlay').hidden = false;
  try {
    const { releases } = await api(`/books/${b.id}/releases`);
    renderPickerReleases(releases || []);
  } catch (e) {
    $('#picker-list').innerHTML = `<div class="empty">Search failed: ${e.message}</div>`;
  }
}

function renderPickerReleases(releases) {
  const list = $('#picker-list');
  list.innerHTML = '';
  $('#picker-count').textContent = `${releases.length} found`;
  if (!releases.length) {
    list.innerHTML = '<div class="empty">No releases found.</div>';
    return;
  }
  // keep auto-pick order via explicit sort: format rank desc
  const sorted = [...releases].sort((a, b) => formatRank((b.format || '').toLowerCase()) - formatRank((a.format || '').toLowerCase()));
  for (const r of sorted) {
    const row = document.createElement('div');
    row.className = 'rel-row';
    const fmt = document.createElement('span');
    fmt.className = 'fmt';
    fmt.textContent = r.format || '—';
    const mid = document.createElement('div');
    mid.className = 'rel-mid';
    const t = document.createElement('div');
    t.className = 'rel-title';
    t.textContent = r.title || r.source_id;
    const sub = document.createElement('div');
    sub.className = 'rel-sub';
    sub.textContent = [r.indexer, r.size ? `${r.size}` : '', r.seeders != null ? `↑${r.seeders}` : '', r.language || ''].filter(Boolean).join(' · ');
    mid.appendChild(t); mid.appendChild(sub);
    row.appendChild(fmt); row.appendChild(mid);
    row.onclick = () => grabRelease(r);
    list.appendChild(row);
  }
}

document.querySelector('#picker-close').onclick = () => { $('#picker-overlay').hidden = true; pickerBook = null; };
$('#picker-overlay').onclick = (e) => { if (e.target.id === 'picker-overlay') { $('#picker-overlay').hidden = true; pickerBook = null; } };

$('#picker-autopick').onclick = async () => {
  const btn = $('#picker-autopick');
  btn.disabled = true;
  try {
    // re-fetch and auto-pick best server-side
    await api(`/books/${pickerBook.id}/grab`, {method: 'POST'});
    $('#picker-overlay').hidden = true;
    pickerBook = null;
    await loadLibrary();
  } catch (e) {
    alert('Grab failed: ' + e.message);
  } finally {
    btn.disabled = false;
  }
};

async function grabRelease(r) {
  try {
    await api(`/books/${pickerBook.id}/grab-release`, {method: 'POST', body: JSON.stringify(r)});
    $('#picker-overlay').hidden = true;
    pickerBook = null;
    await loadLibrary();
  } catch (e) {
    alert('Grab failed: ' + e.message);
  }
}

function formatRank(f) {
  const order = ['cbz', 'cbr', 'djvu', 'fb2', 'pdf', 'azw3', 'mobi', 'epub'];
  return order.indexOf(f);
}

async function setStatus(b, status) {
  await api(`/books/${b.id}/status`, {method: 'POST', body: JSON.stringify({status})});
}

async function delBook(b) {
  if (!confirm(`Remove "${b.title}" from your shelf?\n\nThe book stays in the shared household library.`)) {
    throw new Error('cancelled');
  }
  await api(`/books/${b.id}`, { method: 'DELETE' });
}

async function addToShelf(b) {
  await api(`/books/${b.id}/add-to-shelf`, { method: 'POST', body: JSON.stringify({}) });
}

// ---------- data ----------

async function refreshTracked() {
  const books = await api('/books');
  trackedProviders = new Set(
    books.filter(b => b.provider && b.provider_id)
         .map(b => `${b.provider}:${b.provider_id}`)
  );
  return books;
}

let lastResults = [];

function loadResults() {
  const grid = $('#results-grid');
  grid.innerHTML = '';
  const tpl = $('#card-tpl');
  lastResults.forEach(r => grid.appendChild(cardEl(tpl, r, true)));
}

let householdMode = false;

async function loadLibrary() {
  const params = new URLSearchParams();
  if (householdMode) params.set('household', '1');
  else if (currentFilter) params.set('status', currentFilter);
  const qs = params.toString();
  const books = await api(`/books${qs ? '?' + qs : ''}`);
  const grid = $('#library-grid');
  grid.innerHTML = '';
  if (!books.length) {
    grid.innerHTML = householdMode
      ? '<div class="empty">Every shared book is already on your shelf.</div>'
      : '<div class="empty">Your shelf is empty — search above, or browse the household library.</div>';
    return;
  }
  const tpl = $('#card-tpl');
  for (const b of books) {
    grid.appendChild(cardEl(tpl, b, false));
  }
}

// household toggle
function setHouseholdMode(on) {
  householdMode = on;
  $('#hh-toggle').classList.toggle('active', on);
  $('#hh-toggle').textContent = on ? '🏠 Household library' : '🏠 Household library';
  $('#lib-title').textContent = on ? 'Household library' : 'My shelf';
  $('#filterform').style.display = on ? 'none' : 'flex';
  loadLibrary();
}
document.addEventListener('DOMContentLoaded', () => {});
$('#hh-toggle').onclick = () => setHouseholdMode(!householdMode);

// ---------- views ----------

function showView(v) {
  view = v;
  $('#results-view').hidden = v !== 'results';
  $('#library-view').hidden = v !== 'library';
  $('#series-view').hidden = v !== 'series';
  $('#stats-view').hidden = v !== 'stats';
  $('#aa-grab-view').hidden = v !== 'aa';
  $('#home-view').hidden = v !== 'home';
  if (v === 'series') loadSeries();
  if (v === 'stats') loadStats();
  if (v === 'home') loadHome();
}

// ---------- home (Kindle-style) ----------

async function loadHome() {
  const h = await api('/home');
  const trow = $('#continue-row');
  const rrow = $('#recent-row');
  trow.innerHTML = '';
  rrow.innerHTML = '';
  $('#home-continue-section').hidden = !h.continue.length;
  for (const b of h.continue) trow.appendChild(homeCard(b, b.percent));
  if (!h.recent.length) {
    $('#home-recent-section').innerHTML = '<h2>✨ Recently added</h2><div class="empty">Import some books first.</div>';
  } else {
    $('#home-recent-section').innerHTML = '<h2>✨ Recently added</h2>';
    const sec = $('#home-recent-section');
    const row = document.createElement('div');
    row.className = 'home-row';
    row.id = 'recent-row';
    for (const b of h.recent) row.appendChild(homeCard(b, b.percent));
    sec.appendChild(row);
  }
}

function homeCard(b, percent) {
  const tpl = $('#card-tpl');
  const el = cardEl(tpl, b, false);
  el.classList.add('home-card');
  // bigger progress bar on the card
  if (percent > 0) {
    const bar = document.createElement('div');
    bar.className = 'progress-bar';
    bar.innerHTML = `<div class="progress-fill" style="width:${Math.min(100, percent)}%"></div>`;
    $('.meta', el).appendChild(bar);
    $('.status', el).textContent = `${Math.round(percent)}%`;
  }
  return el;
}

// ---------- series (M4a) ----------

async function loadSeries() {
  const groups = await api('/series-group');
  const grid = $('#series-grid');
  grid.innerHTML = '';
  if (!groups.length) {
    grid.innerHTML = '<div class="empty">No series detected (numbered titles like "Dune 3" group automatically).</div>';
    return;
  }
  for (const g of groups) {
    const card = document.createElement('div');
    card.className = 'card series-card';
    const count = g.books.length;
    const read = g.books.filter(b => b.status === 'read').length;
    card.innerHTML = `
      <div class="series-head">
        <div class="title">${escapeHtml(g.name)}</div>
        <div class="author">${escapeHtml(g.author || '')}</div>
        <div class="muted series-progress">${read}/${count} read</div>
      </div>
    `;
    const mini = document.createElement('div');
    mini.className = 'mini-strip';
    for (const b of g.books) {
      const dot = document.createElement('button');
      dot.className = `mini-dot st-${b.status}`;
      dot.title = `${b.title} (${b.status})`;
      dot.textContent = b.order || count;
      dot.onclick = () => { currentFilter = ''; showView('library'); loadLibrary().then(() => { /* highlight */ }); };
      mini.appendChild(dot);
    }
    card.appendChild(mini);
    grid.appendChild(card);
  }
}

function escapeHtml(s) {
  const d = document.createElement('div');
  d.textContent = s ?? '';
  return d.innerHTML;
}

// ---------- stats (M4b) ----------

async function loadStats() {
  // per-user reading stats + activity
  let st = null;
  try { st = await api('/stats'); } catch {}
  const books = await api('/books');
  const read = books.filter(b => b.status === 'read');
  const reading = books.filter(b => b.status === 'reading');
  const wanted = books.filter(b => b.status === 'wanted' || b.status === 'grabbed');
  const total = books.length;

  const fmtH = (sec) => sec >= 3600 ? `${(sec / 3600).toFixed(1)} h` : `${Math.round(sec / 60)} min`;

  const cards = [
    { label: 'Today', value: st ? fmtH(st.today_seconds_read) : '—' },
    { label: 'Reading streak', value: st ? `${st.streak_days} day${st.streak_days === 1 ? '' : 's'}` : '—' },
    { label: 'Total time', value: st ? fmtH(st.total_seconds_read) : '—' },
    { label: 'Currently reading', value: reading.length },
    { label: 'Finished', value: read.length },
  ];
  $('#stats-cards').innerHTML = cards.map(c => `
    <div class="stat-card"><div class="stat-value">${c.value}</div><div class="stat-label">${c.label}</div></div>
  `).join('');

  // 14-day bar chart
  if (st && st.daily && st.daily.length) {
    const max = Math.max(...st.daily.map(d => d.minutes), 1);
    const bars = st.daily.map(d => `
      <div class="bar-col" title="${d.day}: ${d.minutes} min">
        <div class="bar" style="height:${Math.round((d.minutes / max) * 60)}px"></div>
        <div class="bar-day">${d.day.slice(8)}</div>
      </div>`).join('');
    $('#stats-activity').innerHTML = `
      <h3 style="margin:1rem 0 .5rem">Last 14 days</h3>
      <div class="barchart">${bars}</div>
      ${st.top_books && st.top_books.length ? `<h3 style="margin:1rem 0 .5rem">Most-read books</h3>${st.top_books.map(b => `<div class="stat-row"><span>${escapeHtml(b.title)}</span><span class="muted">${b.minutes} min</span></div>`).join('')}` : ''}
    `;
  } else {
    $('#stats-activity').innerHTML = '';
  }
}

// ---------- AA grab (paste link) ----------

$('#btn-aa-grab').onclick = (e) => { e.preventDefault(); showView('aa'); };

$('#aa-grab-form').onsubmit = async (e) => {
  e.preventDefault();
  const btn = $('#aa-submit');
  btn.disabled = true;
  btn.textContent = 'Downloading…';
  $('#aa-result').textContent = '';
  try {
    const res = await api('/aa-grab', { method: 'POST', body: JSON.stringify({
      link: $('#aa-link').value.trim(),
      title: $('#aa-title').value.trim(),
      author: $('#aa-author').value.trim(),
    })});
    $('#aa-result').textContent = `✓ Downloaded → importing "${res.book.title}". Check your library in a few seconds.`;
    await refreshTracked();
    setTimeout(loadLibrary, 4000);
  } catch (err) {
    $('#aa-result').textContent = `✕ ${err.message}`;
  } finally {
    btn.disabled = false;
    btn.textContent = 'Download';
  }
};

// ---------- events ----------

// library search (client-side filter via server q param, debounced)
let libSearchTimer = null
$('#libsearch').oninput = (e) => {
  clearTimeout(libSearchTimer)
  const q = e.target.value
  libSearchTimer = setTimeout(async () => {
    if (view !== 'library') showView('library')
    searchLibrary(q)
  }, 250)
}
async function searchLibrary(q) {
  const url = q ? `/books?q=${encodeURIComponent(q)}` : (currentFilter ? `/books?status=${currentFilter}` : '/books')
  const books = await api(url)
  const grid = $('#library-grid')
  grid.innerHTML = ''
  if (!books.length) {
    grid.innerHTML = '<div class="empty">No matching books.</div>'
    return
  }
  const tpl = $('#card-tpl')
  for (const b of books) grid.appendChild(cardEl(tpl, b, false))
}

$('#searchform').onsubmit = async (e) => {
  e.preventDefault();
  const q = $('#searchbox').value.trim();
  if (!q) return;
  const btn = $('#searchform button');
  btn.disabled = true;
  btn.textContent = 'Searching…';
  try {
    lastResults = (await api(`/search?q=${encodeURIComponent(q)}`)) || [];
    loadResults();
    $('#results-back').textContent = '← Back to library';
    showView('results');
  } catch (err) {
    alert('Search failed: ' + err.message);
  } finally {
    btn.disabled = false;
    btn.textContent = 'Search';
  }
};

$('#results-back').onclick = () => showView('library');

$$('header nav a').forEach(a => a.onclick = (e) => {
  e.preventDefault();
  $$('header nav a').forEach(x => x.classList.remove('active'));
  a.classList.add('active');
  if (a.dataset.view) {
    currentFilter = a.dataset.filter || '';
    $('#lib-title').textContent = a.dataset.view === 'home' ? 'Home' : (a.textContent || 'Library');
    showView(a.dataset.view);
    return;
  }
  // plain "Library" tab: show all + set lib title
  currentFilter = a.dataset.filter || '';
  $('#lib-title').textContent = 'Library';
  showView('library');
  loadLibrary();
});

// status chips inside the library view
$$('#filterform .chip').forEach(a => a.onclick = (e) => {
  e.preventDefault();
  $$('#filterform .chip').forEach(x => x.classList.remove('active'));
  a.classList.add('active');
  currentFilter = a.dataset.filter;
  loadLibrary();
});

// ---------- auth gate ----------

let me = null;

async function whoAmI() {
  try { me = await api('/auth/me'); } catch { me = null; }
  if (!me) { location.href = '/login'; return false; }
  const chip = document.createElement('span');
  chip.className = 'user-chip';
  chip.id = 'user-chip';
  chip.innerHTML = `<span class="user-name">${me.username}</span>${me.role === 'admin' ? '<a href="/admin" class="admin-link">⚙︎ Admin</a>' : ''}<button id="btn-logout">Sign out</button>`;
  document.body.appendChild(chip);
  $('#btn-logout').onclick = async () => {
    await api('/auth/logout', { method: 'POST' }).catch(() => {});
    location.href = '/login';
  };
  return true;
}

// ---------- init ----------

(async () => {
  if (!await whoAmI()) return;
  await refreshTracked();
  showView('home');
})();

// service worker (PWA offline)
if ('serviceWorker' in navigator && location.protocol === 'https:') {
  navigator.serviceWorker.register('/static/sw.js').catch(e => console.warn('sw:', e));
}