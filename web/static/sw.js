// Dogear service worker
// - App shell: precache core assets, network-first with cache fallback
// - Books: only the ones the user downloaded (static/offline.js stores them in
//   BOOK_CACHE); served cache-first, never saved automatically
// - Library data (lists, progress, notes, covers): network-first, last good
//   response kept so the app browses offline

const VERSION = 'v12'
const SHELL_CACHE = `dogear-shell-${VERSION}`
const DATA_CACHE = `dogear-data-${VERSION}`
// NOT versioned: user downloads must survive service-worker updates
const BOOK_CACHE = 'dogear-books'

const SHELL_ASSETS = [
  '/', '/static/app.js', '/static/offline.js', '/static/style.css',
  '/static/reader.js', '/static/reader.css', '/static/viewport.js',
  '/vendor/view.js', '/vendor/epub.js', '/vendor/paginator.js',
  '/vendor/epubcfi.js', '/vendor/overlayer.js', '/vendor/mobi.js', '/vendor/fb2.js',
  '/vendor/footnotes.js', '/vendor/text-walker.js', '/vendor/progress.js', '/vendor/search.js',
  '/vendor/tts.js', '/vendor/dict.js', '/vendor/quote-image.js', '/vendor/pdf.js',
  '/vendor/vendor/zip.js', '/vendor/fixed-layout.js', '/vendor/comic-book.js',
  '/manifest.json', '/reader', '/login',
]

// GET API responses worth keeping for offline browsing
const DATA_PATHS = [
  /^\/api\/auth\/me$/,
  /^\/api\/books$/,
  /^\/api\/books\/\d+$/,
  /^\/api\/books\/\d+\/(progress|highlights|bookmarks|series-next)$/,
  /^\/api\/home$/,
  /^\/api\/stats$/,
  /^\/api\/series-group$/,
  /^\/api\/covers\/\d+$/,
]

self.addEventListener('install', (event) => {
  event.waitUntil((async () => {
    const cache = await caches.open(SHELL_CACHE)
    await Promise.allSettled(SHELL_ASSETS.map(url => cache.add(url)))
    await self.skipWaiting()
  })())
})

self.addEventListener('activate', (event) => {
  event.waitUntil((async () => {
    // drop old-version caches, but never the downloaded books
    const names = await caches.keys()
    await Promise.all(names.filter(n => n !== BOOK_CACHE && !n.endsWith(VERSION)).map(n => caches.delete(n)))
    await self.clients.claim()
  })())
})

self.addEventListener('message', (event) => {
  if (event.data?.type === 'skip-waiting') {
    self.skipWaiting()
  }
})

function isDataRequest(url) {
  return DATA_PATHS.some(re => re.test(url.pathname))
}

self.addEventListener('fetch', (event) => {
  const url = new URL(event.request.url)
  if (url.origin !== self.location.origin) return
  // login/logout and admin APIs: never touch (stale caches caused login loops)
  if (url.pathname.startsWith('/api/admin/')) return
  if (url.pathname.startsWith('/api/auth/') && url.pathname !== '/api/auth/me') return

  // downloaded book: cache-first (HEAD probes included); otherwise straight to
  // the network. Books are saved only by an explicit download, never here.
  if (/^\/api\/books\/\d+\/content$/.test(url.pathname)) {
    event.respondWith((async () => {
      const cache = await caches.open(BOOK_CACHE)
      const cached = await cache.match(url.pathname, { ignoreMethod: true })
      if (cached) return cached
      try {
        return await fetch(event.request)
      } catch (e) {
        return new Response('offline', { status: 504 })
      }
    })())
    return
  }

  if (event.request.method !== 'GET') return

  // library data: network-first, remember the last good answer. Only a network
  // failure falls back to the cache; a 401 etc. passes through untouched.
  if (isDataRequest(url)) {
    event.respondWith((async () => {
      const cache = await caches.open(DATA_CACHE)
      try {
        const res = await fetch(event.request)
        if (res.ok) cache.put(event.request, res.clone())
        return res
      } catch (e) {
        const cached = await cache.match(event.request, { ignoreSearch: url.pathname.startsWith('/api/covers/') })
        return cached || new Response(JSON.stringify({ error: 'offline' }), {
          status: 504, headers: { 'Content-Type': 'application/json' },
        })
      }
    })())
    return
  }

  // everything else (app shell): network-first, fallback to shell cache
  event.respondWith((async () => {
    const cache = await caches.open(SHELL_CACHE)
    try {
      const res = await fetch(event.request)
      // keep what the reader actually loads (modules, formats) for offline use
      if (res.ok && (url.pathname.startsWith('/static/') || url.pathname.startsWith('/vendor/') ||
          ['/', '/reader', '/login'].includes(url.pathname))) {
        cache.put(url.pathname, res.clone())
      }
      return res
    } catch (e) {
      const cached = await cache.match(event.request, { ignoreSearch: true })
      if (cached) return cached
      // SPA fallback for navigation requests
      if (event.request.mode === 'navigate') {
        const index = await cache.match(url.pathname === '/reader' ? '/reader' : '/')
        if (index) return index
      }
      return new Response('offline', { status: 504 })
    }
  })())
})
