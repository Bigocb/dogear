import '/vendor/view.js'
import { Overlayer } from '/vendor/overlayer.js'
import { textWalker } from '/vendor/text-walker.js'
import { compare as CFI_compare } from '/vendor/epubcfi.js'

// pdf.js v5 uses Uint8Array.prototype.toHex (a TC39 proposal not yet shipped
// in most browsers). Polyfill it so PDF books open.
if (!Uint8Array.prototype.toHex) {
  Object.defineProperty(Uint8Array.prototype, 'toHex', {
    value: function () {
      let s = ''
      for (let i = 0; i < this.length; i++) s += this[i].toString(16).padStart(2, '0')
      return s
    },
    writable: true, configurable: true, enumerable: false,
  })
}
if (!Uint8Array.prototype.toBase64) {
  Object.defineProperty(Uint8Array.prototype, 'toBase64', {
    value: function () {
      let bin = ''
      for (let i = 0; i < this.length; i++) bin += String.fromCharCode(this[i])
      return btoa(bin)
    },
    writable: true, configurable: true, enumerable: false,
  })
}

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
let isFixed = false
let highlights = []
let bookmarks = []
let currentCFI = ''

// ---- config ----
const cfg = loadConfig()
function loadConfig() {
  try { return {
    fontsize: 100, lineheight: 1.6, theme: 'dark', font: '',
    fg: '', bg: '',
    pdfZoom: 'fit-width',   // fit-width | fit-page | numeric scale
    ttsVoice: 'auto',       // 'auto' | 'dev:<id>' | 'piper:<id>'
    ...JSON.parse(localStorage.getItem('dogear-reader') || '{}'),
  } } catch { return { fontsize: 100, lineheight: 1.6, theme: 'dark', font: '', fg: '', bg: '', pdfZoom: 'fit-width', ttsVoice: 'auto' } }
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
applyPageColor()

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

  // PDFs (and other fixed-layout books) render as pages: no reflowable text,
  // so typography controls don't apply. Show page-fit controls instead.
  isFixed = !!view.isFixedLayout
  // the night-theme invert filter in reader.css is for PDF pages only
  document.documentElement.dataset.layout = isFixed ? 'fixed' : 'reflow'
  if (isFixed) applyPdfZoom()

  // ask the service worker to keep this book for offline use
  if (navigator.serviceWorker?.controller && location.protocol === 'https:') {
    navigator.serviceWorker.controller.postMessage({ type: 'cache-book', path: contentURL });
  }

  view.addEventListener('relocate', e => onRelocate(e.detail))
  view.addEventListener('load', ({ detail }) => { applyTypography(); wireTaps(detail.doc) })
  view.addEventListener('show-annotation', e => { annotationTapAt = Date.now(); openHighlight(e.detail.value) })
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
  // Only now is the reader at the saved spot. Relocations fired while opening
  // (foliate briefly lands on the first page) must never overwrite progress.
  savedCFI = prog.cfi || ''
  restored = true

  // wire selection popover
  wireSelection(view)

  $('#reader-loading').classList.remove('visible')
  $('#reader-topbar').hidden = false
  $('#reader-bottombar').hidden = false
  applyTypography()
  syncSettingsUI()
  // discover self-hosted voices, then refresh the picker when they land
  loadPiperVoices().then(() => { if ($('#set-voice')) renderVoicePicker() })
  // show the controls briefly so people know they exist, then get out of the way
  setTimeout(() => { if (!anySheetOpen()) setChrome(false) }, 2500)

  // load annotations data
  await Promise.all([loadHighlights(), loadBookmarks()])
}

// ---- progress ----
let annotationTapAt = 0
function onRelocate({ cfi, fraction, tocItem, time }) {
  currentCFI = cfi || ''
  const pct = Math.round((fraction || 0) * 1000) / 10
  $('#progress-slider').value = pct
  $('#reader-progress-label').textContent = `${pct.toFixed(0)}%`
  const mins = time?.section
  $('#reader-left').textContent = mins != null && isFinite(mins)
    ? (mins < 1 ? 'End of chapter' : `${Math.ceil(mins)} min left in chapter`) : ''
  $('#reader-chapter').textContent = tocItem?.label?.trim() || ''
  currentTocHref = tocItem?.href || null
  syncBookmarkButton()
  currentPct = pct
  if (restored) scheduleSave()
}

// Progress is saved a moment after the reader settles on a page, and flushed
// when the page is hidden or closed, so the last page read is never dropped.
let restored = false
let savedCFI = ''
let currentPct = 0
let saveTimer = null
function scheduleSave() {
  clearTimeout(saveTimer)
  saveTimer = setTimeout(saveProgress, 1500)
}
function saveProgress() {
  clearTimeout(saveTimer)
  if (!restored || !currentCFI || currentCFI === savedCFI) return
  savedCFI = currentCFI
  fetch(`/api/books/${bookId}/progress`, {
    method: 'PUT', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ cfi: currentCFI, percent: currentPct, device: 'web-pwa' }),
    keepalive: true,
  }).catch(() => { savedCFI = '' })
}
document.addEventListener('visibilitychange', () => { if (document.hidden) saveProgress() })
window.addEventListener('pagehide', saveProgress)
let currentTocHref = null

// fetch the book title for the top bar
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

function toast(msg) {
  const t = $('#toast')
  t.textContent = msg
  t.hidden = false
  clearTimeout(toast._t)
  toast._t = setTimeout(() => { t.hidden = true }, 1800)
}

// ---- chrome (top/bottom bars) ----
// publish the bottom bar's height so floating controls (read aloud) can sit above it
new ResizeObserver(([e]) => {
  document.documentElement.style.setProperty('--bb-h', `${Math.round(e.target.getBoundingClientRect().height)}px`)
}).observe($('#reader-bottombar'))
function setChrome(on) { $('#reader-root').classList.toggle('chrome', on) }
const chromeOn = () => $('#reader-root').classList.contains('chrome')

// Tap zones inside the book: left third = back, right third = forward, middle = toggle controls.
function wireTaps(doc) {
  doc.addEventListener('click', (e) => {
    if (e.button !== 0 || e.target.closest?.('a[href]')) return
    const sel = doc.getSelection()
    if (sel && !sel.isCollapsed) return
    const frame = doc.defaultView?.frameElement
    const x = (frame ? frame.getBoundingClientRect().left : 0) + e.clientX
    setTimeout(() => {
      if (Date.now() - annotationTapAt < 400) return // tapped a highlight
      const w = window.innerWidth
      if (chromeOn() && x > w * 0.25 && x < w * 0.75) return setChrome(false)
      if (x < w * 0.25) { setChrome(false); view.prev() }
      else if (x > w * 0.75) { setChrome(false); view.next() }
      else setChrome(true)
    }, 0)
  })
}

// ---- sheets ----
function openSheet(id) { setChrome(false); $(id).hidden = false }
function closeSheet(id) { $(id).hidden = true }
const anySheetOpen = () => $$('.scrim').some(s => !s.hidden)
$$('.scrim').forEach(s => s.addEventListener('click', e => { if (e.target === s) closeSheet('#' + s.id) }))

// ---- selection popover ----
let pendingNote = null // { cfi, text, color } for a new note, or { id } when editing

function wireSelection(foliateView) {
  let selectedText = ''
  let selectedRange = null
  let selectedDoc = null
  let debounceTimer = null

  const pop = document.createElement('div')
  pop.id = 'selection-popover'
  pop.hidden = true
  pop.innerHTML = `
    <div class="colors">
      ${Object.entries(HL_COLORS).map(([k, v]) => `<button class="swatch" data-color="${k}" style="background:${v}" aria-label="Highlight ${k}"></button>`).join('')}
    </div>
    <span class="sep"></span>
    <button data-act="note">Note</button>
    <button data-act="copy">Copy</button>`
  document.body.appendChild(pop)
  const hide = () => { pop.hidden = true }

  // attach to sections loaded from now on, and to the one already on screen
  // (wireSelection runs after view.init, so its first 'load' event has passed)
  const attach = (doc) => {
    if (!doc || doc.__dogearSel) return
    doc.__dogearSel = true
    doc.addEventListener('selectionchange', () => {
      const sel = doc.getSelection()
      if (!sel || sel.isCollapsed || !sel.rangeCount || !sel.toString().trim()) { clearTimeout(debounceTimer); hide(); return }
      selectedRange = sel.getRangeAt(0)
      selectedText = sel.toString().trim()
      selectedDoc = doc
      clearTimeout(debounceTimer)
      debounceTimer = setTimeout(() => {
        const rect = selectedRange.getBoundingClientRect()
        const frameRect = doc.defaultView.frameElement.getBoundingClientRect()
        const half = (pop.offsetWidth || 260) / 2 + 8
        const x = Math.min(window.innerWidth - half, Math.max(half, frameRect.left + rect.left + rect.width / 2))
        let y = frameRect.top + rect.top - 10
        pop.hidden = false
        if (y - pop.offsetHeight < 8) y = frameRect.top + rect.bottom + pop.offsetHeight + 14 // flip below
        pop.style.left = `${x}px`
        pop.style.top = `${y}px`
      }, 200)
    })
  }
  foliateView.addEventListener('load', ({ detail: { doc } }) => attach(doc))
  for (const { doc } of foliateView.renderer.getContents?.() || []) attach(doc)

  pop.onclick = async (e) => {
    const t = e.target.closest('button')
    if (!t || !selectedRange) return
    let cfi
    try { cfi = view.getCFI(view.lastLocation?.section?.current ?? 0, selectedRange) } catch { return }
    const clear = () => { selectedDoc?.getSelection()?.removeAllRanges(); hide() }
    if (t.dataset.color) {
      await saveHighlight(cfi, selectedText, t.dataset.color, null)
      clear()
    } else if (t.dataset.act === 'note') {
      pendingNote = { cfi, text: selectedText, color: 'yellow' }
      clear()
      openNoteEditor(selectedText, '')
    } else if (t.dataset.act === 'copy') {
      try { await navigator.clipboard.writeText(selectedText); toast('Copied') } catch {}
      clear()
    }
  }
}

function openNoteEditor(quote, note) {
  $('#note-quote').textContent = quote
  $('#note-text').value = note || ''
  openSheet('#note-scrim')
  setTimeout(() => $('#note-text').focus(), 250)
}
$('#note-cancel').onclick = () => { pendingNote = null; closeSheet('#note-scrim') }
$('#note-form').onsubmit = async (e) => {
  e.preventDefault()
  const note = $('#note-text').value.trim() || null
  try {
    if (pendingNote?.id) {
      const hl = highlights.find(h => h.id === pendingNote.id)
      const res = await fetch(`/api/highlights/${pendingNote.id}`, {
        method: 'PUT', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ color: hl?.color || 'yellow', note }),
      })
      if (!res.ok) throw new Error(await res.text())
      await loadHighlights()
    } else if (pendingNote) {
      await saveHighlight(pendingNote.cfi, pendingNote.text, pendingNote.color, note)
    }
    toast('Note saved')
    pendingNote = null
    closeSheet('#note-scrim')
  } catch (err) { toast(`Couldn't save: ${err.message}`) }
}

// tapping a highlight in the text opens its note
function openHighlight(cfi) {
  const hl = highlights.find(h => h.cfi === cfi)
  if (!hl) return
  pendingNote = { id: hl.id }
  openNoteEditor(hl.text, hl.note)
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
  for (const hl of highlights) { try { view.deleteAnnotation({ value: hl.cfi }) } catch {} }
  highlights = await (await fetch(`/api/books/${bookId}/highlights`)).json()
  for (const hl of highlights) {
    try { view.addAnnotation({ value: hl.cfi, color: hl.color, note: hl.note, id: hl.id }) } catch {}
  }
  renderHighlights()
}

async function loadBookmarks() {
  bookmarks = await (await fetch(`/api/books/${bookId}/bookmarks`)).json()
  renderBookmarks()
  syncBookmarkButton()
}

// ---- controls ----
$('#reader-back').onclick = () => { saveProgress(); stopTTS(); location.href = '/#library' }
$('#btn-toc').onclick = () => openNav('toc')
$('#btn-highlights').onclick = () => openNav(highlights.length || !bookmarks.length ? 'hl' : 'bm')
$('#btn-bookmark').onclick = toggleBookmark
$('#btn-settings').onclick = () => openSheet('#type-scrim')

$('#progress-slider').oninput = (e) => {
  view?.goToFraction(Number(e.target.value) / 100).catch(() => {})
}

// ---- read-aloud (TTS) ----
// Sentence-level read-aloud. Text is segmented into sentences with
// Intl.Segmenter (via foliate's text walker, which returns DOM Ranges), then
// each sentence is spoken either by the browser SpeechSynthesis (device
// voices) or by the self-hosted Piper engine (nicer, consistent voices).
const tts = {
  on: false, paused: false, rate: 1, list: [], i: -1, gen: 0, doc: null, errs: 0,
  deviceVoices: [], piper: [], piperAvailable: false,
}
const TTS_RATES = [0.75, 1, 1.25, 1.5, 2, 2.5, 3]
const TTS_AUDIO = typeof Audio !== 'undefined' ? new Audio() : null
let ttsAudioUrl = null
if (TTS_AUDIO) TTS_AUDIO.preload = 'auto'

function ttsSegments(doc) {
  const lang = doc.documentElement?.lang || 'en'
  const segmenter = new Intl.Segmenter(lang, { granularity: 'sentence' })
  const out = []
  const gen = (strs, makeRange) => {
    const str = strs.join('')
    let sum = 0, si = -1
    return (function* () {
      for (const { index, segment } of segmenter.segment(str)) {
        const text = segment.replace(/\s+/g, ' ').trim()
        if (!text) continue
        while (sum <= index) sum += strs[++si].length
        const sIdx = si, sOff = index - (sum - strs[si].length)
        const end = index + segment.length - 1
        if (end < str.length) while (sum <= end) sum += strs[++si].length
        const eIdx = si, eOff = end - (sum - strs[si].length) + 1
        yield [text, makeRange(sIdx, sOff, eIdx, eOff)]
      }
    })()
  }
  for (const [text, range] of textWalker(doc, gen)) out.push([text, range])
  return out
}

function ttsEnsure() {
  const doc = view?.renderer?.getContents?.()[0]?.doc
  if (!doc) return false
  if (tts.doc === doc && tts.list.length) return true
  tts.doc = doc
  tts.list = ttsSegments(doc)
  tts.i = -1
  return tts.list.length > 0
}

function ttsOverlayer() { return view?.renderer?.getContents?.()[0]?.overlayer }
function ttsHighlight(range) {
  const o = ttsOverlayer()
  if (!o) return
  try { o.add('__tts__', range, Overlayer.highlight, { color: HL_COLORS.blue }) } catch {}
  try { view.renderer.scrollToAnchor?.(range, false) } catch {}
}
function clearTTSHighlight() { try { ttsOverlayer()?.remove('__tts__') } catch {} }

function refreshTTSVoices() {
  tts.deviceVoices = speechSynthesis.getVoices?.() || []
  if ($('#set-voice')) renderVoicePicker()
}
if ('speechSynthesis' in window) {
  refreshTTSVoices()
  speechSynthesis.addEventListener?.('voiceschanged', refreshTTSVoices)
}

// Fetch the self-hosted Piper voice list once; degrade silently when absent.
async function loadPiperVoices() {
  try {
    const r = await fetch('/api/tts/voices')
    if (!r.ok) return
    const d = await r.json()
    tts.piperAvailable = !!d.available
    tts.piper = d.voices || []
  } catch { tts.piperAvailable = false }
}

// buildVoiceOptions returns select options: device voices first, then Piper.
function buildVoiceOptions() {
  const opts = [{ id: 'auto', label: 'Automatic' }]
  if (tts.deviceVoices.length) {
    opts.push({ group: 'This device' })
    for (const v of tts.deviceVoices) {
      opts.push({ id: 'dev:' + v.voiceURI, label: `${v.name} (${v.lang})` })
    }
  }
  if (tts.piperAvailable && tts.piper.length) {
    opts.push({ group: 'Dogear voices (self-hosted)' })
    for (const v of tts.piper) {
      opts.push({ id: 'piper:' + v.id, label: `${v.name} · ${v.lang}` })
    }
  }
  return opts
}

// resolveVoice maps the saved selection id to what we actually use.
function resolveVoice() {
  const pref = cfg.ttsVoice || 'auto'
  if (pref.startsWith('piper:')) {
    const id = pref.slice(6)
    if (tts.piperAvailable && tts.piper.some(v => v.id === id)) {
      return { kind: 'piper', id }
    }
  }
  if (pref.startsWith('dev:')) {
    const uri = pref.slice(4)
    const v = tts.deviceVoices.find(x => x.voiceURI === uri)
    if (v) return { kind: 'device', voice: v }
  }
  // automatic: prefer a self-hosted voice, else any device voice matching lang
  const lang = tts.doc?.documentElement?.lang || 'en'
  if (tts.piperAvailable && tts.piper.length) {
    const p = tts.piper.find(v => v.lang?.startsWith(lang.slice(0, 2))) || tts.piper[0]
    return { kind: 'piper', id: p.id }
  }
  const dv = tts.deviceVoices.find(x => x.lang === lang) ||
             tts.deviceVoices.find(x => x.lang?.startsWith(lang.slice(0, 2)))
  return { kind: 'device', voice: dv || null, id: 'auto' }
}

function ttsFindStart() {
  const r = view?.lastLocation?.range
  if (!r) return 0
  for (let k = 0; k < tts.list.length; k++) {
    try { if (r.compareBoundaryPoints(Range.END_TO_START, tts.list[k][1]) <= 0) return k } catch {}
  }
  return 0
}

function ttsSpeak() {
  if (!tts.on) return
  const item = tts.list[tts.i]
  if (!item) { stopTTS(); return }
  const [text, range] = item
  ttsHighlight(range)
  const gen = ++tts.gen
  const sel = resolveVoice()
  if (sel.kind === 'piper') speakPiper(text, sel.id, gen)
  else speakDevice(text, sel.voice, gen)
}

// speakDevice: browser SpeechSynthesis (device/OS voices).
function speakDevice(text, voice, gen) {
  const u = new SpeechSynthesisUtterance(text)
  const lang = tts.doc?.documentElement?.lang
  if (lang) u.lang = lang
  u.rate = tts.rate
  if (voice) u.voice = voice
  tts.errs = 0
  u.onend = () => { if (gen === tts.gen && tts.on && !tts.paused) { tts.i++; ttsSpeak() } }
  u.onerror = () => {
    if (gen !== tts.gen || !tts.on) return
    if (++tts.errs >= 3) { stopTTS(); toast('No voice available for read-aloud'); return }
    tts.i++; ttsSpeak()
  }
  try { speechSynthesis.speak(u) } catch {
    if (++tts.errs >= 3) { stopTTS(); toast('No voice available for read-aloud'); return }
    tts.i++; ttsSpeak()
  }
}

// speakPiper: fetch WAV from the self-hosted engine and play it. A sentence is
// prefetched while the current one plays so playback stays gapless.
function ttsPiperUrl(text, voiceId) {
  const ls = (1 / tts.rate).toFixed(3)
  return `/api/tts?text=${encodeURIComponent(text)}&voice=${encodeURIComponent(voiceId)}&length_scale=${ls}`
}

function speakPiper(text, voiceId, gen) {
  // prefetch the next sentence (best effort, ignored if playback moves on)
  const nxt = tts.list[tts.i + 1]
  if (nxt) { try { new Audio(ttsPiperUrl(nxt[0], voiceId)).preload = 'auto' } catch {} }
  fetch(ttsPiperUrl(text, voiceId))
    .then(res => { if (!res.ok) throw new Error('tts ' + res.status); return res.blob() })
    .then(blob => {
      if (gen !== tts.gen || !tts.on) return
      if (ttsAudioUrl) URL.revokeObjectURL(ttsAudioUrl)
      ttsAudioUrl = URL.createObjectURL(blob)
      TTS_AUDIO.src = ttsAudioUrl
      TTS_AUDIO.onended = () => { if (gen === tts.gen && tts.on && !tts.paused) { tts.i++; ttsSpeak() } }
      TTS_AUDIO.onerror = () => { if (gen === tts.gen && tts.on) { stopTTS(); toast('Read-aloud playback failed') } }
      if (tts.paused) return
      TTS_AUDIO.play().catch(() => { if (gen === tts.gen) { stopTTS(); toast('Tap Listen again to allow audio') } })
    })
    .catch(() => {
      if (gen !== tts.gen || !tts.on) return
      if (++tts.errs >= 3) { stopTTS(); toast('Read-aloud engine unavailable'); return }
      tts.i++; ttsSpeak()
    })
}

function ttsNext() {
  if (!tts.on) return
  try { speechSynthesis.cancel() } catch {}
  tts.i = Math.min(tts.i + 1, tts.list.length - 1)
  tts.paused = false
  $('#tts-bar').classList.remove('paused')
  ttsSpeak()
}
function ttsPrev() {
  if (!tts.on) return
  try { speechSynthesis.cancel() } catch {}
  tts.i = Math.max(tts.i - 1, 0)
  tts.paused = false
  $('#tts-bar').classList.remove('paused')
  ttsSpeak()
}

function stopTTS() {
  tts.on = false; tts.paused = false
  tts.gen++
  try { speechSynthesis.cancel() } catch {}
  try { TTS_AUDIO?.pause() } catch {}
  clearTTSHighlight()
  $('#tts-bar').hidden = true
  $('#btn-tts').setAttribute('aria-pressed', 'false')
}

function startTTS() {
  if (!('speechSynthesis' in window) && !tts.piperAvailable) { toast('This browser can’t read aloud'); return }
  if (!ttsEnsure()) { toast('Listen works with reflowable books'); return }
  tts.on = true; tts.paused = false
  tts.i = ttsFindStart()
  $('#tts-bar').hidden = false
  $('#tts-bar').classList.remove('paused')
  $('#btn-tts').setAttribute('aria-pressed', 'true')
  ttsSpeak()
}

function toggleTTS() {
  if (!tts.on) { startTTS(); return }
  if (tts.paused) {
    tts.paused = false
    $('#tts-bar').classList.remove('paused')
    const sel = resolveVoice()
    if (sel.kind === 'piper') {
      if (TTS_AUDIO?.paused) TTS_AUDIO.play().catch(() => {})
      else ttsSpeak()
    } else {
      try { speechSynthesis.resume() } catch {}
      if (!speechSynthesis.speaking) ttsSpeak()
    }
  } else {
    tts.paused = true
    $('#tts-bar').classList.add('paused')
    const sel = resolveVoice()
    if (sel.kind === 'piper') { try { TTS_AUDIO?.pause() } catch {} }
    else { try { speechSynthesis.pause() } catch {} }
  }
}

function cycleTTSRate() {
  const i = TTS_RATES.indexOf(tts.rate)
  tts.rate = TTS_RATES[(i + 1) % TTS_RATES.length]
  $('#tts-rate').textContent = `${tts.rate}×`
  if (tts.on && !tts.paused) { try { speechSynthesis.cancel() } catch {}; ttsSpeak() }
}

$('#btn-tts').onclick = () => { if (tts.on) stopTTS(); else toggleTTS() }
$('#tts-play').onclick = toggleTTS
$('#tts-next').onclick = ttsNext
$('#tts-prev').onclick = ttsPrev
$('#tts-rate').onclick = cycleTTSRate
$('#tts-close').onclick = stopTTS

// ---- typography ----
const SIZE_STEPS = [70, 80, 90, 100, 110, 120, 135, 150, 170, 200]
function stepSize(dir) {
  const i = SIZE_STEPS.findIndex(s => s >= cfg.fontsize)
  const cur = i === -1 ? SIZE_STEPS.length - 1 : i
  const next = SIZE_STEPS[Math.min(SIZE_STEPS.length - 1, Math.max(0, cur + dir))]
  cfg.fontsize = next
  saveConfig(); applyTypography(); syncSettingsUI()
}
$('#font-smaller').onclick = () => stepSize(-1)
$('#font-larger').onclick = () => stepSize(1)
$$('#set-theme button').forEach(b => b.onclick = () => {
  cfg.theme = b.dataset.theme; cfg.fg = ''; cfg.bg = ''
  saveConfig(); applyConfig(); applyTypography(); syncSettingsUI()
})
$$('#set-font button').forEach(b => b.onclick = () => { cfg.font = b.dataset.font; saveConfig(); applyTypography(); syncSettingsUI() })
$$('#set-lineheight button').forEach(b => b.onclick = () => { cfg.lineheight = Number(b.dataset.lh); saveConfig(); applyTypography(); syncSettingsUI() })
$('#set-fg').oninput = e => { cfg.fg = e.target.value; saveConfig(); applyTypography() }
$$('#pdf-fit button').forEach(b => b.onclick = () => {
  cfg.pdfZoom = b.dataset.fit; saveConfig(); applyPdfZoom(); syncSettingsUI()
})
$('#set-bg').oninput = e => { cfg.bg = e.target.value; saveConfig(); applyTypography() }
$('#reset-colors').onclick = () => { cfg.fg = ''; cfg.bg = ''; saveConfig(); applyTypography(); syncSettingsUI() }

function syncSettingsUI() {
  // PDFs get page-fit controls; reflowable books get typography controls.
  $('#pdf-controls').hidden = !isFixed
  $('#reflow-controls').hidden = isFixed
  // read-aloud needs reflowable text; hide the control for fixed-layout books
  $('#btn-tts').hidden = isFixed
  $$('#pdf-fit button').forEach(b => b.classList.toggle('on', b.dataset.fit === String(cfg.pdfZoom)))

  const { fg, bg } = themeColors()
  $('#set-fg').value = fg
  $('#set-bg').value = bg
  $('#font-size-val').textContent = `${cfg.fontsize}%`
  $('#font-smaller').disabled = cfg.fontsize <= SIZE_STEPS[0]
  $('#font-larger').disabled = cfg.fontsize >= SIZE_STEPS[SIZE_STEPS.length - 1]
  $$('#set-theme button').forEach(b => b.classList.toggle('on', b.dataset.theme === cfg.theme))
  $$('#set-font button').forEach(b => b.classList.toggle('on', b.dataset.font === (cfg.font || '')))
  const lhs = $$('#set-lineheight button').map(b => Number(b.dataset.lh))
  const nearest = lhs.reduce((a, b) => Math.abs(b - cfg.lineheight) < Math.abs(a - cfg.lineheight) ? b : a)
  $$('#set-lineheight button').forEach(b => b.classList.toggle('on', Number(b.dataset.lh) === nearest))
  $('meta[name=theme-color]')?.setAttribute('content', bg)
  renderVoicePicker()
}

// renderVoicePicker fills the Reading voice select and its hint. Options merge
// the device's own voices and the self-hosted Piper voices (grouped).
function renderVoicePicker() {
  const sel = $('#set-voice')
  if (!sel) return
  const opts = buildVoiceOptions()
  sel.innerHTML = ''
  let current = null
  for (const o of opts) {
    if (o.group) {
      const g = document.createElement('optgroup')
      g.label = o.group
      sel.appendChild(g)
      current = g
    } else {
      const el = document.createElement('option')
      el.value = o.id
      el.textContent = o.label
      ;(current || sel).appendChild(el)
    }
  }
  // keep the saved choice selectable even if its voice disappeared
  if (cfg.ttsVoice && !opts.some(o => o.id === cfg.ttsVoice)) {
    const el = document.createElement('option')
    el.value = cfg.ttsVoice
    el.textContent = `${cfg.ttsVoice} (unavailable)`
    sel.appendChild(el)
  }
  sel.value = cfg.ttsVoice || 'auto'
  const hint = $('#voice-hint')
  if (hint) {
    const rv = resolveVoice()
    hint.textContent = !tts.piperAvailable
      ? 'Using your device’s voices. Self-hosted voices are offline right now.'
      : rv.kind === 'piper'
        ? 'Self-hosted neural voice — natural and private.'
        : 'Using a voice built into this device.'
  }
}
$('#set-voice').onchange = e => { cfg.ttsVoice = e.target.value; saveConfig(); renderVoicePicker() }

// the area around the book (margins, status-bar strip) takes the page colour;
// the reader's controls keep the app palette from reader.css
function applyPageColor() {
  document.documentElement.style.setProperty('--page', themeColors().bg)
}

function applyTypography() {
  applyPageColor()
  if (isFixed) return // fixed-layout (PDF): pages don't reflow
  try { view.renderer.setStyles?.(getStyles()) } catch {}
}

// apply the PDF page-fit/zoom setting to foliate's fixed-layout renderer
function applyPdfZoom() {
  if (!isFixed || !view?.renderer) return
  try { view.renderer.setAttribute('zoom', String(cfg.pdfZoom)) } catch {}
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
    a{color:${cfg.theme === 'dark' || cfg.theme === 'black' ? '#d1ad66' : '#94682a'}}
  `
}

// ---- contents / highlights / bookmarks sheet ----
function openNav(tab) {
  buildToc()
  showTab(tab)
  openSheet('#nav-scrim')
  if (tab === 'toc') $('#toc-content a.current')?.scrollIntoView({ block: 'center' })
}
function showTab(tab) {
  $$('#nav-scrim .tabs button').forEach(b => b.classList.toggle('on', b.dataset.tab === tab))
  $$('#nav-scrim [data-pane]').forEach(p => { p.hidden = p.dataset.pane !== tab })
}
$$('#nav-scrim .tabs button').forEach(b => b.onclick = () => showTab(b.dataset.tab))

let tocBuilt = false
function buildToc() {
  const content = $('#toc-content')
  if (!tocBuilt) {
    const toc = view?.book?.toc
    if (!toc || !toc.length) {
      content.innerHTML = '<div class="empty"><b>No contents</b>This book doesn\'t include a table of contents.</div>'
      tocBuilt = true
      return
    }
    const ul = document.createElement('ul')
    const walk = (items, parent) => {
      for (const it of items || []) {
        const li = document.createElement('li')
        const a = document.createElement('a')
        a.textContent = it.label?.trim() || 'Untitled'
        a.dataset.href = it.href
        a.onclick = () => { view.goTo(it.href).catch(() => {}); closeSheet('#nav-scrim') }
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
  $$('#toc-content a').forEach(a => a.classList.toggle('current', !!currentTocHref && a.dataset.href === currentTocHref))
}

const ICON_NOTE = '<svg viewBox="0 0 24 24"><path d="M4 20h4L19 9l-4-4L4 16z"/></svg>'
const ICON_TRASH = '<svg viewBox="0 0 24 24"><path d="M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13"/></svg>'

function renderHighlights() {
  $('#hl-count').textContent = highlights.length || ''
  $('#hl-content').innerHTML = highlights.length
    ? highlights.map(hl => `
        <div class="ann">
          <button class="ann-go" data-go-hl="${hl.id}">
            <div class="ann-text" style="--sw:${HL_COLORS[hl.color] || HL_COLORS.yellow}">${escapeHtml(hl.text.slice(0, 280))}</div>
            ${hl.note ? `<div class="ann-note">${escapeHtml(hl.note)}</div>` : ''}
          </button>
          <div class="ann-tools">
            <button data-note-hl="${hl.id}" aria-label="Edit note">${ICON_NOTE}</button>
            <button data-del-hl="${hl.id}" aria-label="Delete highlight">${ICON_TRASH}</button>
          </div>
        </div>`).join('')
    : '<div class="empty"><b>No highlights yet</b>Select text while reading to highlight it or add a note.</div>'
}

function renderBookmarks() {
  $('#bm-count').textContent = bookmarks.length || ''
  $('#bm-content').innerHTML = bookmarks.length
    ? bookmarks.map(bm => `
        <div class="ann">
          <button class="ann-go" data-go-bm="${bm.id}">
            <div class="ann-text">${escapeHtml(bm.label || 'Bookmark')}</div>
          </button>
          <div class="ann-tools"><button data-del-bm="${bm.id}" aria-label="Delete bookmark">${ICON_TRASH}</button></div>
        </div>`).join('')
    : '<div class="empty"><b>No bookmarks yet</b>Tap the bookmark at the top of the page to save your place.</div>'
}

$('#nav-scrim .sheet-body').onclick = async (e) => {
  const t = e.target.closest('[data-go-hl],[data-note-hl],[data-del-hl],[data-go-bm],[data-del-bm]')
  if (!t) return
  const d = t.dataset
  if (d.goHl) {
    const hl = highlights.find(h => String(h.id) === d.goHl)
    if (hl) { closeSheet('#nav-scrim'); view.goTo(hl.cfi).catch(() => {}) }
  } else if (d.noteHl) {
    const hl = highlights.find(h => String(h.id) === d.noteHl)
    if (hl) { closeSheet('#nav-scrim'); pendingNote = { id: hl.id }; openNoteEditor(hl.text, hl.note) }
  } else if (d.delHl) {
    await fetch(`/api/highlights/${d.delHl}`, { method: 'DELETE' })
    await loadHighlights()
    toast('Highlight deleted')
  } else if (d.goBm) {
    const bm = bookmarks.find(b => String(b.id) === d.goBm)
    if (bm) { closeSheet('#nav-scrim'); view.goTo(bm.cfi).catch(() => {}) }
  } else if (d.delBm) {
    await fetch(`/api/bookmarks/${d.delBm}`, { method: 'DELETE' })
    await loadBookmarks()
  }
}

function escapeHtml(s) {
  const d = document.createElement('div')
  d.textContent = s
  return d.innerHTML
}

// ---- bookmarks ----
// a bookmark "covers" the current page if its CFI falls inside the visible range
function bookmarkHere() {
  const loc = view?.lastLocation
  if (!loc?.range || !bookmarks.length) return null
  const index = loc.section?.current
  const edge = (atStart) => { const r = loc.range.cloneRange(); r.collapse(atStart); return view.getCFI(index, r) }
  let start, end
  try { start = edge(true); end = edge(false) } catch { return null }
  return bookmarks.find(bm => {
    try { return CFI_compare(bm.cfi, start) >= 0 && CFI_compare(bm.cfi, end) <= 0 } catch { return false }
  }) || null
}
function syncBookmarkButton() {
  $('#btn-bookmark').setAttribute('aria-pressed', bookmarkHere() ? 'true' : 'false')
}

async function toggleBookmark() {
  if (!view || !currentCFI) return
  const existing = bookmarkHere()
  if (existing) {
    await fetch(`/api/bookmarks/${existing.id}`, { method: 'DELETE' })
    await loadBookmarks()
    toast('Bookmark removed')
    return
  }
  const frac = $('#progress-slider').value / 100
  const chapter = $('#reader-chapter').textContent
  const label = `${chapter ? chapter + ' · ' : ''}${(frac * 100).toFixed(0)}%`
  await fetch(`/api/books/${bookId}/bookmarks`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ cfi: currentCFI, label, percent: frac }),
  })
  await loadBookmarks()
  toast('Page bookmarked')
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

// ---- keyboard ----
document.addEventListener('keydown', e => {
  if (e.target.closest?.('input, textarea')) return
  if (e.key === 'Escape') { const open = $$('.scrim').find(s => !s.hidden); if (open) return closeSheet('#' + open.id); return setChrome(!chromeOn()) }
  if (anySheetOpen()) return
  if (e.key === 'ArrowRight' || e.key === ' ') { e.preventDefault(); view.next() }
  if (e.key === 'ArrowLeft') view.prev()
})

main().then(() => {
  const _n = view.next.bind(view), _p = view.prev.bind(view);
  view.next = (...a) => { readTrack.pageTurns++; return _n(...a) };
  view.prev = (...a) => { readTrack.pageTurns++; return _p(...a) };
}).catch(swallowAbort)