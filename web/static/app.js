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
    const err = new Error(msg || `Request failed (${res.status})`);
    err.status = res.status;
    throw err;
  }
  if (res.status === 204) return null;
  return res.json();
}

function escapeHtml(s) {
  const d = document.createElement('div');
  d.textContent = s ?? '';
  return d.innerHTML;
}

// ---------- toasts (replace alert) ----------

function toast(msg, kind = '') {
  const t = document.createElement('div');
  t.className = `toast ${kind}`;
  t.textContent = msg;
  $('#toasts').appendChild(t);
  setTimeout(() => t.remove(), kind === 'error' ? 5200 : 3000);
}
const toastError = (prefix, e) => toast(`${prefix}: ${e.message}`, 'error');

// Shelfmark failures come back as raw Go errors; say what to do instead
function shelfmarkMessage(e) {
  if (e.status === 502 || /dial tcp|connection refused|no such host|timeout/i.test(e.message)) {
    return "Couldn't reach Shelfmark. Check that it's running, then try again.";
  }
  return e.message;
}

// ---------- state ----------

let me = null;
let shelf = [];                 // my shelf books (all statuses)
let percents = new Map();       // book id -> reading percent
let trackedProviders = new Set();
let scope = 'shelf';            // 'shelf' | 'household'
let filter = '';
let libQuery = '';

const STATUS_LABEL = {
  wanted: 'Wanted', grabbed: 'Downloading', imported: 'Unread',
  reading: 'Reading', read: 'Finished', library: 'In the household library',
};

async function refreshShelf() {
  const [books, home] = await Promise.all([api('/books'), api('/home').catch(() => null)]);
  shelf = books || [];
  trackedProviders = new Set(shelf.filter(b => b.provider && b.provider_id).map(b => `${b.provider}:${b.provider_id}`));
  percents = new Map();
  for (const b of [...(home?.continue || []), ...(home?.recent || [])]) {
    if (b.percent > 0) percents.set(b.id, b.percent);
  }
  return home;
}

// ---------- covers ----------

const CLOTH = ['#6b2b2b', '#24443a', '#22324f', '#6e5220', '#46304f', '#33414b', '#5a3a26', '#2f4a4a'];
function clothFor(s) {
  let h = 0;
  for (const c of s || '') h = (h * 31 + c.charCodeAt(0)) >>> 0;
  return CLOTH[h % CLOTH.length];
}

function coverEl(b, { remote = false } = {}) {
  const c = document.createElement('div');
  c.className = 'cover';
  c.style.background = clothFor(b.title + b.author);
  c.innerHTML = `
    <div class="typeset" aria-hidden="true">
      <div class="t">${escapeHtml(b.title || 'Untitled')}</div>
      <div class="rule"></div>
      <div class="a">${escapeHtml(b.author || '')}</div>
    </div>`;
  const src = remote ? b.cover_url : (b.id ? `/api/covers/${b.id}` : null);
  if (src) {
    const img = new Image();
    img.alt = '';
    img.loading = 'lazy';
    img.onload = () => { if (img.naturalWidth > 8) $('.typeset', c).hidden = true; else img.remove(); };
    img.onerror = () => img.remove();
    img.src = src;
    c.appendChild(img);
  }
  if (!remote) {
    if (b.status === 'reading') c.classList.add('dogeared');
    if (b.status === 'read') c.insertAdjacentHTML('beforeend', '<span class="finished-tag" title="Finished"><svg viewBox="0 0 24 24"><path d="M5 12.5l4.5 4.5L19 7.5"/></svg></span>');
    if (b.status === 'wanted' || b.status === 'grabbed') c.classList.add('pending');
  }
  return c;
}

function bookTile(b, { onClick } = {}) {
  const el = document.createElement('button');
  el.className = 'book';
  el.type = 'button';
  el.setAttribute('aria-label', `${b.title}${b.author ? ' by ' + b.author : ''}`);
  el.appendChild(coverEl(b));
  const pct = percents.get(b.id);
  if (b.status === 'reading' && pct > 0) {
    el.insertAdjacentHTML('beforeend', `<div class="meter"><i style="width:${Math.min(100, pct)}%"></i></div>`);
  }
  el.insertAdjacentHTML('beforeend', `<div class="bt">${escapeHtml(b.title || 'Untitled')}</div><div class="ba">${escapeHtml(b.author || '')}</div>`);
  if (b.status === 'wanted' || b.status === 'grabbed') {
    el.insertAdjacentHTML('beforeend', `<div class="bs">${b.status === 'grabbed' ? 'Downloading' : 'Wanted'}</div>`);
  }
  el.onclick = onClick || (() => openBook(b));
  return el;
}

// ---------- navigation ----------

const VIEWS = ['home', 'library', 'find', 'stats'];

function showView(v, { push = true } = {}) {
  if (!VIEWS.includes(v)) v = 'home';
  for (const name of VIEWS) $(`#${name}-view`).hidden = name !== v;
  $$('#tabbar a').forEach(a => a.classList.toggle('active', a.dataset.view === v));
  if (push && location.hash !== `#${v}`) history.replaceState(null, '', `#${v}`);
  window.scrollTo(0, 0);
  if (v === 'home') loadHome();
  if (v === 'library') loadLibrary();
  if (v === 'stats') loadStats();
  if (v === 'find') setTimeout(() => { if (!$('#results-grid').children.length) $('#searchbox').focus({ preventScroll: true }); }, 50);
}

$$('#tabbar a').forEach(a => a.onclick = (e) => { e.preventDefault(); showView(a.dataset.view); });

// ---------- home ----------

async function loadHome() {
  let home;
  try { home = await refreshShelf(); } catch (e) { return toastError('Could not load your shelf', e); }
  const hour = new Date().getHours();
  $('#home-view .page-title').textContent = hour < 5 ? 'Late reading' : hour < 12 ? 'Good morning' : hour < 18 ? 'Good afternoon' : 'Good evening';

  const reading = home?.continue || [];
  const unread = shelf.filter(b => b.status === 'imported');
  const hero = $('#home-hero');
  hero.innerHTML = '';
  const lead = reading[0] || unread[0];
  if (lead) {
    const isReading = lead.status === 'reading';
    const pct = Math.round(lead.percent || percents.get(lead.id) || 0);
    const card = document.createElement('div');
    card.className = 'hero';
    const coverWrap = document.createElement('div');
    coverWrap.appendChild(coverEl(lead));
    coverWrap.style.cursor = 'pointer';
    coverWrap.onclick = () => openBook(lead);
    card.appendChild(coverWrap);
    card.insertAdjacentHTML('beforeend', `
      <div>
        <div class="eyebrow">${isReading ? 'Continue reading' : 'Up next'}</div>
        <h2>${escapeHtml(lead.title)}</h2>
        <div class="by">${escapeHtml(lead.author || '')}</div>
        ${isReading ? `<div class="progress-line"><div class="meter"><i style="width:${pct}%"></i></div><span>${pct}%</span></div>` : '<div style="height:16px"></div>'}
        <button class="btn primary block">${isReading ? 'Continue' : 'Start reading'}</button>
      </div>`);
    $('.btn', card).onclick = () => openReader(lead);
    hero.appendChild(card);
  } else {
    hero.innerHTML = `
      <div class="empty-state" style="margin-bottom:30px">
        <h3>Your shelf is empty</h3>
        <p>Find a book to download, or pick one from the household library.</p>
        <button class="btn primary" data-go="find">Find a book</button>
        <button class="btn quiet" data-go="household">Browse household</button>
      </div>`;
  }

  const shelves = $('#home-shelves');
  shelves.innerHTML = '';
  const addShelf = (title, books, more) => {
    if (!books.length) return;
    const sec = document.createElement('section');
    sec.className = 'shelf';
    sec.innerHTML = `<h2 class="shelf-title"><span>${title}</span>${more ? `<button data-more="${more}">See all</button>` : ''}</h2>`;
    const row = document.createElement('div');
    row.className = 'row';
    for (const b of books) row.appendChild(bookTile(b));
    sec.appendChild(row);
    shelves.appendChild(sec);
  };
  addShelf('Also reading', reading.slice(lead && lead.status === 'reading' ? 1 : 0), 'reading');
  addShelf('Recently added', (home?.recent || []).filter(b => b.id !== lead?.id), 'imported');
  addShelf('On the way', shelf.filter(b => b.status === 'wanted' || b.status === 'grabbed'), 'wanted');
  addShelf('Finished', shelf.filter(b => b.status === 'read'), 'read');
}

document.addEventListener('click', (e) => {
  const more = e.target.closest('[data-more]');
  if (more) { scope = 'shelf'; filter = more.dataset.more; showView('library'); return; }
  const go = e.target.closest('[data-go]');
  if (go) {
    if (go.dataset.go === 'household') { scope = 'household'; filter = ''; showView('library'); }
    else showView(go.dataset.go);
  }
});

// ---------- library ----------

async function loadLibrary() {
  $$('#scope-seg button').forEach(b => b.classList.toggle('on', b.dataset.scope === scope));
  $('#filter-chips').hidden = scope !== 'shelf';
  $('#libsearch').value = libQuery;
  if (scope === 'shelf') {
    try { await refreshShelf(); } catch (e) { return toastError('Could not load your shelf', e); }
  }
  renderLibrary();
}

async function renderLibrary() {
  const grid = $('#library-grid');
  const q = libQuery.trim().toLowerCase();
  let books;
  if (scope === 'household') {
    try { books = await api(`/books?household=1${q ? '&q=' + encodeURIComponent(q) : ''}`); }
    catch (e) { return toastError('Could not load the household library', e); }
  } else {
    const matches = shelf.filter(b => !q || `${b.title} ${b.author}`.toLowerCase().includes(q));
    // chip counts reflect the current search
    $$('#filter-chips button').forEach(c => {
      const f = c.dataset.filter;
      const n = f ? matches.filter(b => b.status === f).length : matches.length;
      c.innerHTML = `${c.dataset.label || (c.dataset.label = c.textContent)}<span class="n">${n}</span>`;
      c.classList.toggle('on', f === filter);
    });
    books = filter ? matches.filter(b => b.status === filter) : matches;
  }
  grid.innerHTML = '';
  if (!books.length) {
    grid.innerHTML = q
      ? `<div class="empty-state"><h3>No matches</h3><p>Nothing ${scope === 'household' ? 'in the household library' : 'on your shelf'} matches “${escapeHtml(libQuery)}”.</p></div>`
      : scope === 'household'
        ? '<div class="empty-state"><h3>All caught up</h3><p>Every book in the household library is already on your shelf.</p></div>'
        : filter
          ? `<div class="empty-state"><h3>Nothing here</h3><p>No books marked ${escapeHtml(STATUS_LABEL[filter].toLowerCase())}.</p></div>`
          : '<div class="empty-state"><h3>Your shelf is empty</h3><p>Find a book, or add one from the household library.</p><button class="btn primary" data-go="find">Find a book</button></div>';
  } else {
    for (const b of books) grid.appendChild(bookTile(b));
  }
  if (scope === 'shelf' && !filter && !q) loadSeries(); else $('#series-block').hidden = true;
}

$$('#scope-seg button').forEach(b => b.onclick = () => { scope = b.dataset.scope; filter = ''; loadLibrary(); });
$$('#filter-chips button').forEach(c => c.onclick = () => { filter = c.dataset.filter; renderLibrary(); });
let libTimer = null;
$('#libsearch').oninput = (e) => {
  clearTimeout(libTimer);
  libQuery = e.target.value;
  libTimer = setTimeout(renderLibrary, scope === 'household' ? 250 : 0);
};

async function loadSeries() {
  let groups = [];
  try { groups = await api('/series-group'); } catch {}
  const block = $('#series-block');
  block.hidden = !groups.length;
  if (!groups.length) return;
  const list = $('#series-list');
  list.innerHTML = '';
  for (const g of groups) {
    const read = g.books.filter(b => b.status === 'read').length;
    const el = document.createElement('div');
    el.className = 'series';
    el.innerHTML = `
      <div class="series-name">${escapeHtml(g.name)}</div>
      <div class="series-sub">${escapeHtml(g.author || '')}${g.author ? ' · ' : ''}${read} of ${g.books.length} finished</div>
      <div class="series-dots"></div>`;
    for (const b of g.books) {
      const dot = document.createElement('button');
      dot.className = `st-${b.status}`;
      dot.textContent = b.order || '·';
      dot.title = `${b.title} — ${STATUS_LABEL[b.status] || b.status}`;
      dot.onclick = () => openBook(shelf.find(x => x.id === b.id) || b);
      $('.series-dots', el).appendChild(dot);
    }
    list.appendChild(el);
  }
}

// ---------- book sheet ----------

let sheetBook = null;

function openSheet(id) { $(id).hidden = false; document.body.style.overflow = 'hidden'; }
function closeSheet(id) { $(id).hidden = true; if ($$('.scrim:not([hidden])').length === 0) document.body.style.overflow = ''; }
for (const id of ['#book-scrim', '#picker-scrim', '#account-scrim']) {
  $(id).addEventListener('click', (e) => { if (e.target === $(id)) closeSheet(id); });
}
document.addEventListener('keydown', (e) => {
  if (e.key !== 'Escape') return;
  const open = $$('.scrim:not([hidden])').pop();
  if (open) closeSheet('#' + open.id);
});

function openBook(b) {
  sheetBook = b;
  renderBookSheet();
  openSheet('#book-scrim');
}

function renderBookSheet() {
  const b = sheetBook;
  const body = $('#book-sheet-body');
  const pct = Math.round(percents.get(b.id) || 0);
  const facts = [
    b.series_name && `${b.series_name}${b.series_position ? ' #' + b.series_position : ''}`,
    b.publish_year, b.publisher, b.page_count && `${b.page_count} pages`,
    b.language, b.genres, b.isbn && `ISBN ${b.isbn}`,
  ].filter(Boolean);

  body.innerHTML = `
    <div class="bk-head">
      <div class="bk-cover"></div>
      <div>
        <h2 id="sheet-title">${escapeHtml(b.title || 'Untitled')}</h2>
        <div class="by">${escapeHtml(b.author || 'Unknown author')}</div>
        <div class="bk-state ${b.status}">${escapeHtml(STATUS_LABEL[b.status] || b.status)}</div>
        ${b.status === 'reading' && pct ? `<div class="bk-progress"><div class="meter"><i style="width:${pct}%"></i></div><span>${pct}%</span></div>` : ''}
      </div>
    </div>
    <div class="bk-actions"></div>
    ${facts.length ? `<div class="bk-facts">${facts.map(f => `<span>${escapeHtml(String(f))}</span>`).join('')}</div>` : ''}
    <div class="bk-desc ${b.description ? '' : 'none'}">${b.description ? escapeHtml(b.description) : 'No description yet. Use “Fetch details” to look it up.'}</div>
    <div class="bk-remove"></div>
  `;
  $('.bk-cover', body).appendChild(coverEl(b));

  const actions = $('.bk-actions', body);
  const btn = (label, cls, fn) => {
    const el = document.createElement('button');
    el.className = `btn ${cls}`;
    el.textContent = label;
    if (fn) el.onclick = () => run(el, fn);
    return el;
  };
  const pair = (...els) => { const d = document.createElement('div'); d.className = 'pair'; els.forEach(e => d.appendChild(e)); return d; };
  const enrichBtn = btn('Fetch details', '', enrich);

  switch (b.status) {
    case 'library':
      actions.append(btn('Add to my shelf', 'primary block', async () => {
        await api(`/books/${b.id}/add-to-shelf`, { method: 'POST', body: '{}' });
        toast(`Added “${b.title}” to your shelf`);
        closeSheet('#book-scrim'); loadLibrary();
      }));
      break;
    case 'wanted':
      actions.append(btn('Find a download', 'primary block', () => openPicker(b)), enrichBtn);
      break;
    case 'grabbed': {
      const dl = btn('Downloading…', 'block', null); dl.disabled = true;
      actions.append(dl, pair(btn('Choose another', '', () => openPicker(b)), enrichBtn));
      break;
    }
    case 'imported':
      actions.append(btn('Start reading', 'primary block', () => openReader(b)),
        pair(btn('Mark finished', '', () => setStatus(b, 'read')), enrichBtn));
      break;
    case 'reading':
      actions.append(btn('Continue reading', 'primary block', () => openReader(b)),
        pair(btn('Mark finished', '', () => setStatus(b, 'read')), enrichBtn));
      break;
    case 'read':
      actions.append(btn('Read again', 'primary block', () => openReader(b)),
        pair(btn('Mark unread', '', () => setStatus(b, 'imported')), enrichBtn));
      break;
    default:
      actions.append(enrichBtn);
  }

  if (b.status !== 'library') {
    const rm = btn('Remove from my shelf', 'danger', null);
    rm.onclick = () => {
      if (!rm.classList.contains('armed')) {
        rm.classList.add('armed');
        rm.textContent = 'Tap again to remove';
        clearTimeout(rm._disarm);
        rm._disarm = setTimeout(() => { rm.classList.remove('armed'); rm.textContent = 'Remove from my shelf'; }, 6000);
        return;
      }
      clearTimeout(rm._disarm);
      run(rm, async () => {
        await api(`/books/${b.id}`, { method: 'DELETE' });
        toast(`Removed “${b.title}”. It's still in the household library.`);
        closeSheet('#book-scrim'); refreshCurrent();
      });
    };
    $('.bk-remove', body).appendChild(rm);
  }
}

async function run(el, fn) {
  if (el.disabled) return;
  const label = el.textContent;
  el.disabled = true;
  try { await fn(); }
  catch (e) { toast(e.message, 'error'); }
  finally { if (el.isConnected) { el.disabled = false; if (el.textContent === label) el.textContent = label; } }
}

async function enrich() {
  const btnEl = $$('#book-sheet-body .btn').find(x => x.textContent === 'Fetch details');
  if (btnEl) btnEl.textContent = 'Fetching…';
  try {
    const updated = await api(`/books/${sheetBook.id}/enrich`, { method: 'POST', body: '{}' });
    sheetBook = { ...sheetBook, ...updated };
    renderBookSheet();
    toast('Details updated');
  } catch (e) {
    if (btnEl) btnEl.textContent = 'Fetch details';
    throw new Error(`Couldn't fetch details. ${shelfmarkMessage(e)}`);
  }
}

async function setStatus(b, status) {
  await api(`/books/${b.id}/status`, { method: 'POST', body: JSON.stringify({ status }) });
  toast(status === 'read' ? `Marked “${b.title}” finished` : `Marked “${b.title}” unread`);
  closeSheet('#book-scrim');
  refreshCurrent();
}

async function openReader(b) {
  if (b.status !== 'reading') {
    try { await api(`/books/${b.id}/status`, { method: 'POST', body: JSON.stringify({ status: 'reading' }) }); } catch {}
  }
  location.href = `/reader?book=${b.id}`;
}

function refreshCurrent() {
  const v = VIEWS.find(n => !$(`#${n}-view`).hidden);
  if (v === 'home') loadHome();
  else if (v === 'library') loadLibrary();
}

// ---------- release picker ----------

let pickerBook = null;

async function openPicker(b) {
  pickerBook = b;
  closeSheet('#book-scrim');
  $('#picker-book-label').textContent = `${b.title}${b.author ? ' — ' + b.author : ''}`;
  $('#picker-count').textContent = '';
  $('#picker-list').innerHTML = '<div class="loading-line">Searching for downloads…</div>';
  openSheet('#picker-scrim');
  try {
    const { releases } = await api(`/books/${b.id}/releases`);
    renderReleases(releases || []);
  } catch (e) {
    $('#picker-list').innerHTML = `<div class="empty-state"><h3>Search didn't work</h3><p>${escapeHtml(shelfmarkMessage(e))}</p></div>`;
  }
}

function formatRank(f) {
  return ['cbz', 'cbr', 'djvu', 'fb2', 'pdf', 'azw3', 'mobi', 'epub'].indexOf(f);
}

function renderReleases(releases) {
  const list = $('#picker-list');
  list.innerHTML = '';
  $('#picker-count').textContent = `${releases.length} found`;
  if (!releases.length) {
    list.innerHTML = '<div class="empty-state"><h3>No downloads found</h3><p>Try again later, or paste an Anna\'s Archive link from the Find tab.</p></div>';
    return;
  }
  const sorted = [...releases].sort((a, b) => formatRank((b.format || '').toLowerCase()) - formatRank((a.format || '').toLowerCase()));
  for (const r of sorted) {
    const row = document.createElement('button');
    row.className = 'rel-row';
    row.innerHTML = `
      <span class="fmt">${escapeHtml(r.format || '—')}</span>
      <div class="rel-mid">
        <div class="rel-title">${escapeHtml(r.title || r.source_id)}</div>
        <div class="rel-sub">${escapeHtml([r.indexer, r.size, r.seeders != null ? `${r.seeders} seeders` : '', r.language].filter(Boolean).join(' · '))}</div>
      </div>`;
    row.onclick = () => run(row, async () => {
      await api(`/books/${pickerBook.id}/grab-release`, { method: 'POST', body: JSON.stringify(r) });
      afterGrab();
    });
    list.appendChild(row);
  }
}

function afterGrab() {
  toast(`Downloading “${pickerBook.title}”. It'll appear on your shelf when it's ready.`);
  closeSheet('#picker-scrim');
  pickerBook = null;
  refreshCurrent();
}

$('#picker-close').onclick = () => closeSheet('#picker-scrim');
$('#picker-autopick').onclick = () => run($('#picker-autopick'), async () => {
  await api(`/books/${pickerBook.id}/grab`, { method: 'POST' });
  afterGrab();
});

// ---------- find ----------

$('#searchform').onsubmit = async (e) => {
  e.preventDefault();
  const q = $('#searchbox').value.trim();
  if (!q) return;
  const btn = $('#searchform .btn');
  btn.disabled = true;
  btn.textContent = 'Searching…';
  $('#searchbox').blur();
  const grid = $('#results-grid');
  try {
    const results = (await api(`/search?q=${encodeURIComponent(q)}`)) || [];
    grid.innerHTML = '';
    if (!results.length) {
      grid.innerHTML = `<div class="empty-state"><h3>No results</h3><p>Nothing matched “${escapeHtml(q)}”. Try fewer words, or search by author.</p></div>`;
    }
    for (const r of results) grid.appendChild(resultTile(r));
  } catch (err) {
    grid.innerHTML = `<div class="empty-state"><h3>Search didn't work</h3><p>${escapeHtml(shelfmarkMessage(err))}</p></div>`;
  } finally {
    btn.disabled = false;
    btn.textContent = 'Search';
  }
};

function resultTile(r) {
  const el = document.createElement('div');
  el.className = 'book';
  el.style.cursor = 'default';
  el.appendChild(coverEl(r, { remote: true }));
  el.insertAdjacentHTML('beforeend', `<div class="bt">${escapeHtml(r.title || 'Untitled')}</div><div class="ba">${escapeHtml(r.author || '')}</div><div class="result-add"></div>`);
  const slot = $('.result-add', el);
  const render = () => {
    slot.innerHTML = '';
    const tracked = trackedProviders.has(`${r.provider}:${r.book_id}`);
    const b = document.createElement('button');
    b.className = `btn ${tracked ? 'quiet' : ''}`;
    b.textContent = tracked ? 'On your shelf' : 'Add to wanted';
    b.disabled = tracked;
    b.onclick = () => run(b, async () => {
      await api('/books', { method: 'POST', body: JSON.stringify({
        title: r.title, author: r.author, isbn: r.isbn,
        provider: r.provider, provider_id: r.book_id, cover_url: r.cover_url,
      }) });
      trackedProviders.add(`${r.provider}:${r.book_id}`);
      toast(`Added “${r.title}” to your wanted list`);
      render();
    });
    slot.appendChild(b);
  };
  render();
  return el;
}

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
    }) });
    $('#aa-result').textContent = `Downloaded. “${res.book.title}” will appear on your shelf in a few seconds.`;
    e.target.reset();
  } catch (err) {
    $('#aa-result').textContent = `Download failed: ${err.message}`;
  } finally {
    btn.disabled = false;
    btn.textContent = 'Download';
  }
};

// ---------- stats ----------

async function loadStats() {
  let st = null;
  try { st = await api('/stats'); } catch {}
  try { await refreshShelf(); } catch {}
  const reading = shelf.filter(b => b.status === 'reading').length;
  const finished = shelf.filter(b => b.status === 'read').length;
  const time = (sec) => {
    if (!sec) return '0<small>min</small>';
    if (sec >= 3600) return `${(sec / 3600).toFixed(1)}<small>h</small>`;
    return `${Math.max(1, Math.round(sec / 60))}<small>min</small>`;
  };
  const body = $('#stats-body');
  const streak = st?.streak_days ?? 0;
  let html = `
    <div class="figures">
      <div class="figure"><b>${st ? time(st.today_seconds_read) : '—'}</b><span>Today</span></div>
      <div class="figure"><b>${st ? streak : '—'}<small>${streak === 1 ? 'day' : 'days'}</small></b><span>Streak</span></div>
      <div class="figure"><b>${st ? time(st.total_seconds_read) : '—'}</b><span>All time</span></div>
      <div class="figure"><b>${reading}</b><span>Reading now</span></div>
      <div class="figure"><b>${finished}</b><span>Finished</span></div>
      <div class="figure"><b>${shelf.length}</b><span>On your shelf</span></div>
    </div>`;
  const daily = st?.daily || [];
  const anyReading = daily.some(d => d.minutes > 0);
  html += `<h2 class="shelf-title"><span>Last 14 days</span></h2>`;
  if (anyReading) {
    const max = Math.max(...daily.map(d => d.minutes), 1);
    html += `<div class="chart">${daily.map((d, i) => `
      <div class="col ${i === daily.length - 1 ? 'today' : ''}" title="${d.day}: ${d.minutes} min">
        <div class="bar ${d.minutes ? '' : 'zero'}" style="height:${Math.max(3, Math.round((d.minutes / max) * 100))}%"></div>
        <div class="d">${Number(d.day.slice(8))}</div>
      </div>`).join('')}</div>`;
  } else {
    html += `<div class="empty-state"><h3>No reading logged yet</h3><p>Time spent in the reader shows up here, day by day.</p></div>`;
  }
  if (st?.top_books?.length) {
    html += `<h2 class="shelf-title section-gap"><span>Most read</span></h2><div class="stat-list">${
      st.top_books.map(b => `<div><span>${escapeHtml(b.title)}</span><span>${b.minutes} min</span></div>`).join('')}</div>`;
  }
  body.innerHTML = html;
}

// ---------- account ----------

function renderAccount() {
  const initial = (me.username || '?').slice(0, 1).toUpperCase();
  $$('[data-account]').forEach(b => { b.textContent = initial; b.onclick = () => openSheet('#account-scrim'); });
  $('#account-menu').innerHTML = `
    <div class="who">Signed in as<b>${escapeHtml(me.username)}</b></div>
    ${me.role === 'admin' ? '<a href="/admin">Admin settings</a>' : ''}
    <button id="btn-logout">Sign out</button>`;
  $('#btn-logout').onclick = async () => {
    await api('/auth/logout', { method: 'POST' }).catch(() => {});
    location.href = '/login';
  };
}

// ---------- init ----------

(async () => {
  try { me = await api('/auth/me'); } catch { me = null; }
  if (!me) { location.href = '/login'; return; }
  renderAccount();
  showView((location.hash || '#home').slice(1), { push: false });
})();

window.addEventListener('hashchange', () => showView(location.hash.slice(1), { push: false }));

// service worker (PWA offline)
if ('serviceWorker' in navigator && location.protocol === 'https:') {
  navigator.serviceWorker.register('/static/sw.js').catch(e => console.warn('sw:', e));
}
