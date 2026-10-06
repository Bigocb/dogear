import '/vendor/view.js'
import { Overlayer } from '/vendor/overlayer.js'

const $ = (sel, el = document) => el.querySelector(sel)
const $$ = (sel, el = document) => [...el.querySelectorAll(sel)]

function fail(msg) {
  console.error('[dogear reader]', msg)
  $('#reader-loading').classList.remove('visible')
  const err = $('#reader-error')
  $('#reader-error-msg').textContent = String(msg)
  err.classList.add('visible')
}

// ---- state ----
const params = new URLSearchParams(location.search)
const bookId = Number(params.get('book'))
let view = null
let lastSaved = 0
let highlights = []
let bookmarks = []
let currentCFI = ''

// ---- config ----
const cfg = loadConfig()
function loadConfig() {
  try { return {
    fontsize: 100, lineheight: 1.6, theme: 'dark', font: '',
    fg: '', bg: '',
    ...JSON.parse(localStorage.getItem('dogear-reader') || '{}'),
  } } catch { return { fontsize: 100, lineheight: 1.6, theme: 'dark', font: '', fg: '', bg: '' } }
}
function saveConfig() { localStorage.setItem('dogear-reader', JSON.stringify(cfg)) }
function applyConfig() { document.documentElement.dataset.theme = cfg.theme }

const THEMES = {
  dark:  { fg: '#e8e8ea', bg: '#14161a' },
  sepia: { fg: '#4c3d2e', bg: '#f4ecd8' },
  light: { fg: '#222222', bg: '#ffffff' },
  black: { fg: '#b8b8b8', bg: '#000000' },
}
function themeColors() {
  return { fg: cfg.fg || THEMES[cfg.theme]?.fg || '#e8e8ea', bg: cfg.bg || THEMES[cfg.theme]?.bg || '#14161a' }
}
applyConfig()

window.onerror = () => { /* handled in main().catch; ignore stray Safari aborts */ }

const swallowAbort = (e) => {
  if (e && (e.name === 'AbortError' || /Load failed|aborted/i.test(e?.message || ''))) {
    console.warn('transient fetch abort:', e?.message || e)
    return
  }
  fail(e?.message || e)
}

// ---- highlight colors ----
const HL_COLORS = {
  yellow: '#fbd44f', green: '#7bd88f', blue: '#7aa2f7', pink: '#f7768e',
}

// ---- main ----
async function main() {
  const contentURL = `/api/books/${bookId}/content`
  const probe = await fetch(contentURL, { method: 'HEAD' })
  if (!probe.ok) throw new Error(`Book file unavailable (${probe.status})`)

  const container = $('#reader-container')
  const foliateView = document.createElement('foliate-view')
  container.appendChild(foliateView)

  // Fetch the book as a Blob first — a single plain GET is far more reliable
  // on iOS Safari than foliate's string-URL path (which re-fetches per entry).
  // Retry once on failure: Safari kills long-running fetches when tabs are
  // backgrounded or when CF round trips stall.
  let blob = null
  for (let attempt = 1; attempt <= 3; attempt++) {
    try {
      const res = await fetch(contentURL)
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      const raw = await res.blob()
      // must be a File (has .name/.type): foliate's isCBZ/isFBZ call
      // name.endsWith() on it — plain Blob makes those throw
      blob = new File([raw], `${bookId}.epub`, { type: raw.type || 'application/epub+zip' });
      break
    } catch (e) {
      console.warn(`fetch attempt ${attempt} failed:`, e)
      if (attempt === 3) throw new Error(`Could not download book: ${e?.message || e}`)
      await new Promise(r => setTimeout(r, 800 * attempt))
    }
  }
  if (!blob || !blob.size) throw new Error('Downloaded book file is empty')

  await foliateView.open(blob)  // Blob path: foliate detects zip/mobi/pdf from bytes
  view = foliateView

  // ask the service worker to keep this book for offline use
  if (navigator.serviceWorker?.controller && location.protocol === 'https:') {
    navigator.serviceWorker.controller.postMessage({ type: 'cache-book', path: contentURL });
  }

  view.addEventListener('relocate', e => onRelocate(e.detail))
  view.addEventListener('load', () => applyTypography())
  view.addEventListener('draw-annotation', e => {
    const { draw, annotation } = e.detail
    const color = HL_COLORS[annotation.color] || HL_COLORS.yellow
    draw(Overlayer.highlight, { color })
  })

  // restore progress
  const prog = await (await fetch(`/api/books/${bookId}/progress`)).json()
  if (prog.cfi) await view.init({ lastLocation: prog.cfi, showTextStart: false })
  else if (prog.percent > 0) await view.init({ lastLocation: { fraction: prog.percent / 100 }, showTextStart: false })
  else await view.init({ showTextStart: false })

  // wire selection popover
  wireSelection(view)

  $('#reader-loading').classList.remove('visible')
  $('#reader-topbar').hidden = false
  $('#reader-bottombar').hidden = false
  applyTypography()
  syncSettingsUI()

  // load annotations data
  await Promise.all([loadHighlights(), loadBookmarks()])
}

// ---- progress ----
function onRelocate({ cfi, fraction, tocItem, pageItem }) {
  currentCFI = cfi || ''
  const pct = Math.round((fraction || 0) * 1000) / 10
  $('#progress-slider').value = pct
  $('#reader-progress-label').textContent = `${pct.toFixed(0)}%`
  const now = Date.now()
  if (now - lastSaved > 3000 && currentCFI) {
    lastSaved = now
    fetch(`/api/books/${bookId}/progress`, {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ cfi: currentCFI, percent: pct, device: 'web-pwa' }),
    }).catch(() => {})
  }
  if (tocItem?.label) {
    $('#reader-book-title').textContent = `${docTitle} — ${tocItem.label}`
  } else {
    $('#reader-book-title').textContent = docTitle
  }
}

// fetch the book title lazily from the /api/books list
let docTitle = 'Book';
(async () => {
  try {
    const b = await (await fetch(`/api/books/${bookId}`)).json()
    if (b?.title) {
      docTitle = b.title
      document.title = `${b.title} — Dogear`
      $('#reader-book-title').textContent = b.title
    }
  } catch {}
})()

// ---- selection popover ----
function wireSelection(foliateView) {
  let selectedText = ''
  let selectedRange = null

  const attachDoc = (doc) => {
    doc.addEventListener('selectionchange', () => {
      const sel = doc.getSelection()
      if (!sel || sel.isCollapsed || !sel.rangeCount) {
        hidePopover()
        return
      }
      selectedRange = sel.getRangeAt(0)
      selectedText = sel.toString().trim()
      if (!selectedText) { hidePopover(); return }
      showPopover(doc, sel)
    })
    // touchend needs slight delay after selectionchange on iOS
    doc.addEventListener('mouseup', () => setTimeout(checkSel, 30))
    function checkSel() {
      const sel = doc.getSelection()
      if (!sel || sel.isCollapsed) { hidePopover(); return }
    }
  }

  foliateView.addEventListener('load', ({ detail }) => attachDoc(detail.doc))

  // iOS: selectionchange fires while dragging handles; debounce
  let debounceTimer = null
  const origHandler = attachDoc
  // popover element
  const pop = document.createElement('div')
  pop.id = 'selection-popover'
  pop.hidden = true
  pop.innerHTML = `
    <button data-act="highlight">Highlight</button>
    <button data-act="note">Note</button>
    <div class="colors">
      ${Object.entries(HL_COLORS).map(([k, v]) => `<button class="swatch" data-color="${k}" style="background:${v}"></button>`).join('')}
    </div>
  `
  document.body.appendChild(pop)

  window.__showPop = (x, y) => {
    pop.hidden = false
    pop.style.left = `${x}px`
    pop.style.top = `${y}px`
  }
  function showPopover(doc, sel) {
    clearTimeout(debounceTimer)
    debounceTimer = setTimeout(() => {
      const rect = sel.getRangeAt(0).getBoundingClientRect()
      const frameRect = $('#reader-container').getBoundingClientRect()
      const x = frameRect.left + rect.left + rect.width / 2
      const y = frameRect.top + rect.top - 8
      window.__showPop(x, y)
    }, 150)
  }
  function hidePopover() { pop.hidden = true }

  pop.onclick = async (e) => {
    const { act, color: colorAttr } = e.target.dataset
    if (!selectedRange) return
    const color = colorAttr || 'yellow'
    // CFI for the selection range: anchor via resolveCFI on current index
    let cfi
    try {
      const index = view.lastLocation?.section?.current ?? 0
      cfi = view.getCFI(index, selectedRange)
    } catch { return }
    const doc = view.renderer.getContents().find(x => x.index === (view.lastLocation?.section?.current ?? 0))?.doc
    if (act === 'highlight' || colorAttr) {
      await saveHighlight(cfi, selectedText, color, null)
      view.addAnnotation({ value: cfi, color })
      doc?.getSelection()?.removeAllRanges()
      hidePopover()
    } else if (act === 'note') {
      const note = prompt('Note:')
      if (note == null) return
      await saveHighlight(cfi, selectedText, color, note || null)
      view.addAnnotation({ value: cfi, color, note })
      doc?.getSelection()?.removeAllRanges()
      hidePopover()
    }
  }
}

async function saveHighlight(cfi, text, color, note) {
  const res = await fetch(`/api/books/${bookId}/highlights`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ cfi, text, color, note }),
  })
  if (!res.ok) throw new Error(await res.text())
  await loadHighlights()
}

async function loadHighlights() {
  highlights = await (await fetch(`/api/books/${bookId}/highlights`)).json()
  for (const hl of highlights) {
    try { view.addAnnotation({ value: hl.cfi, color: hl.color, note: hl.note, id: hl.id }) } catch {}
  }
  renderHighlightsTab()
}

async function loadBookmarks() {
  bookmarks = await (await fetch(`/api/books/${bookId}/bookmarks`)).json()
  renderHighlightsTab()
}

// ---- controls ----
$('#reader-back').onclick = () => { location.href = '/' }
$('#btn-toc').onclick = () => { togglePanel('toc'); buildToc() }
$('#btn-highlights').onclick = () => togglePanel('annotations')
$('#btn-bookmark').onclick = addBookmarkHere
$('#btn-settings').onclick = () => { $('#reader-settings').hidden = !$('#reader-settings').hidden }

$('#progress-slider').oninput = (e) => {
  const f = Number(e.target.value) / 100
  view?.goToFraction(f).catch(() => {})
}

$('#set-fontsize').oninput = e => { cfg.fontsize = Number(e.target.value); saveConfig(); applyTypography() }
$('#set-lineheight').oninput = e => { cfg.lineheight = Number(e.target.value); saveConfig(); applyTypography() }
$('#set-theme').onchange = e => {
  cfg.theme = e.target.value; cfg.fg = ''; cfg.bg = ''; saveConfig()
  applyConfig(); applyTypography(); syncSettingsUI()
}
$('#set-font').onchange = e => { cfg.font = e.target.value; saveConfig(); applyTypography() }
$('#set-fg').oninput = e => { cfg.fg = e.target.value; saveConfig(); applyTypography() }
$('#set-bg').oninput = e => { cfg.bg = e.target.value; saveConfig(); applyTypography() }

function syncSettingsUI() {
  const { fg, bg } = themeColors()
  $('#set-fg').value = fg
  $('#set-bg').value = bg
}

function applyTypography() {
  try { view.renderer.setStyles?.(getStyles()) } catch {}
}
function getStyles() {
  const { fg, bg } = themeColors()
  const font = cfg.font ? `;font-family:${cfg.font}` : ''
  const base = (cfg.fontsize / 100).toFixed(2)
  const lh = cfg.lineheight
  return `
    html{background:${bg} !important;line-height:${lh} !important}
    body{background:${bg} !important;color:${fg} !important;line-height:${lh} !important;font-size:${base}em !important${font}}
    p,div,span,h1,h2,h3,h4,h5,h6,li,td,blockquote,section,article{color:${fg};font-size:inherit;line-height:${lh} !important}
    a{color:${cfg.theme === 'dark' || cfg.theme === 'black' ? '#7aa2f7' : '#3a6ea5'}}
  `
}

// ---- panels ----
function togglePanel(kind) {
  const toc = $('#toc-panel'), ann = $('#ann-panel')
  if (kind === 'toc') {
    ann.hidden = true
    toc.hidden = !toc.hidden
  } else {
    toc.hidden = true
    ann.hidden = !ann.hidden
  }
}

// explicit close buttons in both panels
$$('.panel-close').forEach(btn => btn.onclick = () => {
  $('#' + btn.dataset.panel).hidden = true
})

// backdrop click closes any open panel
document.addEventListener('click', (e) => {
  for (const id of ['toc-panel', 'ann-panel']) {
    const panel = $('#' + id)
    if (!panel.hidden && !panel.contains(e.target) && !e.target.closest('#btn-toc') && !e.target.closest('#btn-highlights')) {
      panel.hidden = true
    }
  }
})

let tocBuilt = false
async function buildToc() {
  if (tocBuilt && $('#toc-panel').hidden) return
  const panel = $('#toc-panel')
  const content = $('#toc-content')
  if (!tocBuilt) {
    const toc = view?.book?.toc
    if (!toc || !toc.length) {
      content.innerHTML = '<div class="empty" style="padding:1rem 0; color:var(--muted);">No table of contents</div>'
      tocBuilt = true
      return
    }
    const ul = document.createElement('ul')
    const walk = (items, parent) => {
      for (const it of items || []) {
        const li = document.createElement('li')
        const a = document.createElement('a')
        a.textContent = it.label?.trim() || '(untitled)'
        a.onclick = () => { view.goTo(it.href).catch(() => {}); panel.hidden = true }
        li.appendChild(a)
        if (it.subitems?.length) { const child = document.createElement('ul'); walk(it.subitems, child); li.appendChild(child) }
        parent.appendChild(li)
      }
    }
    walk(toc, ul)
    content.innerHTML = ''
    content.appendChild(ul)
    tocBuilt = true
  }
}

// ---- annotations panel ----
function renderHighlightsTab() {
  const panel = $('#ann-panel')
  if (!panel) return
  const content = $('#ann-content')
  const hls = highlights.length
    ? highlights.map(hl => `
        <div class="ann-item" style="border-left: 3px solid ${HL_COLORS[hl.color] || '#888'}">
          <div class="ann-text">${escapeHtml(hl.text.slice(0, 240))}</div>
          ${hl.note ? `<div class="ann-note">${escapeHtml(hl.note)}</div>` : ''}
          <div class="ann-actions">
            <button data-del-hl="${hl.id}">Delete</button>
          </div>
        </div>`).join('')
    : '<div class="empty">No highlights yet.</div>'
  const bms = bookmarks.length
    ? bookmarks.map(bm => `
        <div class="ann-item bookmark">
          <div class="ann-text">${escapeHtml(bm.label || `Bookmark (${(bm.percent || 0).toFixed(0)}%)`)}</div>
          <div class="ann-actions">
            <button data-go-bm="${bm.id}">Go</button>
            <button data-del-bm="${bm.id}">Delete</button>
          </div>
        </div>`).join('')
    : '<div class="empty">No bookmarks yet.</div>'
  content.innerHTML = `
    <h3>Highlights <span class="muted">${highlights.length}</span></h3>
    ${hls}
    <h3>Bookmarks <span class="muted">${bookmarks.length}</span></h3>
    ${bms}
  `
  content.onclick = async (e) => {
    const delHl = e.target.dataset.delHl
    const goBm = e.target.dataset.goBm
    const delBm = e.target.dataset.delBm
    if (delHl) {
      await fetch(`/api/highlights/${delHl}`, { method: 'DELETE' })
      try { view.deleteAnnotation({ value: highlights.find(h => String(h.id) === String(delHl))?.cfi }) } catch {}
      await loadHighlights()
    } else if (goBm) {
      const bm = bookmarks.find(b => String(b.id) === String(goBm))
      if (bm) { view.goTo(bm.cfi).catch(() => {}); panel.hidden = true; $('#ann-panel').hidden = true }
    } else if (delBm) {
      await fetch(`/api/bookmarks/${delBm}`, { method: 'DELETE' })
      await loadBookmarks()
    }
  }
}

function escapeHtml(s) {
  const d = document.createElement('div')
  d.textContent = s
  return d.innerHTML
}

async function addBookmarkHere() {
  if (!view || !currentCFI) return
  const frac = $('#progress-slider').value / 100
  const label = `Page ${(frac * 100).toFixed(0)}%`
  await fetch(`/api/books/${bookId}/bookmarks`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ cfi: currentCFI, label, percent: frac }),
  })
  await loadBookmarks()
}

// ---- reading-time tracking ----
const readTrack = { lastBeat: Date.now(), pageTurns: 0 }

document.addEventListener('visibilitychange', () => {
  // on return to the tab, flush accumulated time
  if (!document.hidden) readTrack.lastBeat = Date.now()
})
window.addEventListener('pagehide', () => flushReadingTime(true))

setInterval(() => {
  if (document.hidden || !view || !currentCFI) {
    readTrack.lastBeat = Date.now()
    return
  }
  const elapsed = Math.round((Date.now() - readTrack.lastBeat) / 1000)
  readTrack.lastBeat = Date.now()
  if (elapsed >= 10) sendHeartbeat(elapsed)
}, 30000)

function sendHeartbeat(seconds) {
  const pages = readTrack.pageTurns
  readTrack.pageTurns = 0
  fetch(`/api/books/${bookId}/reading`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ seconds, pages }),
    keepalive: true,
  }).catch(() => {})
}
function flushReadingTime() {
  const elapsed = Math.round((Date.now() - readTrack.lastBeat) / 1000)
  if (elapsed >= 10) sendHeartbeat(elapsed)
}

// count page turns for the stats
const _origNext = view?.next?.bind(view)
const _origPrev = view?.prev?.bind(view)
if (_origNext && _origPrev) {
  view.next = (...a) => { readTrack.pageTurns++; return _origNext(...a) }
  view.prev = (...a) => { readTrack.pageTurns++; return _origPrev(...a) }
}

// ---- gesture nav ----
let touchStartX = 0
$('#reader-container').addEventListener('touchstart', e => { touchStartX = e.touches[0].clientX }, { passive: true })
$('#reader-container').addEventListener('touchend', e => {
  const dx = e.changedTouches[0].clientX - touchStartX
  if (Math.abs(dx) > 60) dx < 0 ? view.next() : view.prev()
}, { passive: true })

document.addEventListener('keydown', e => {
  if (e.key === 'ArrowRight') view.next()
  if (e.key === 'ArrowLeft') view.prev()
})

let uiVisible = true
$('#reader-container').addEventListener('dblclick', () => {
  uiVisible = !uiVisible
  $('#reader-topbar').hidden = !uiVisible
  $('#reader-bottombar').hidden = !uiVisible
})


main().then(() => {
  const _n = view.next.bind(view), _p = view.prev.bind(view);
  view.next = (...a) => { readTrack.pageTurns++; return _n(...a) };
  view.prev = (...a) => { readTrack.pageTurns++; return _p(...a) };
}).catch(swallowAbort)