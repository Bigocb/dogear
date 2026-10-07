// Dogear service worker
// - App shell: precache core assets, network-first with cache fallback
// - Book files: cached on successful read, LRU-ish eviction (keep N most recent)
// - Covers/api: network-first (cheap, tiny)

const VERSION = 'v5'
const SHELL_CACHE = `dogear-shell-${VERSION}`
const BOOK_CACHE = `dogear-books-${VERSION}`
const KEEP_BOOKS = 5

const SHELL_ASSETS = [
  '/', '/static/app.js', '/static/style.css',
  '/static/reader.js', '/static/reader.css', '/static/viewport.js',
  '/vendor/view.js', '/vendor/epub.js', '/vendor/paginator.js',
  '/vendor/epubcfi.js', '/vendor/overlayer.js',
  '/manifest.json',
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
    // drop old-version caches
    const names = await caches.keys()
    await Promise.all(names.filter(n => !n.endsWith(VERSION)).map(n => caches.delete(n)))
    await self.clients.claim()
  })())
})

self.addEventListener('message', (event) => {
  // reader asks us to cache a book once fully downloaded
  if (event.data?.type === 'cache-book' && event.data.path) {
    event.waitUntil(cacheBook(event.data.path))
  }
  if (event.data?.type === 'evict-books') {
    event.waitUntil(evictBooks())
  }
})

async function cacheBook(path) {
  const cache = await caches.open(BOOK_CACHE)
  // store keyed by path (e.g. /api/books/8/content)
  const existing = await cache.match(path)
  if (existing) {
    await touch(path)
    return
  }
  const res = await fetch(path)
  if (!res.ok) return
  await cache.put(path, res.clone())
  await evictBooks()
}

// last-touched index inside the cache response's header bag
async function touch(path) {
  const cache = await caches.open(BOOK_CACHE)
  const res = await cache.match(path)
  if (!res) return
  const headers = new Headers(res.headers)
  headers.set('X-Dogear-Touched', String(Date.now()))
  await cache.put(path, new Response(await res.blob(), { status: res.status, headers }))
}

async function evictBooks() {
  const cache = await caches.open(BOOK_CACHE)
  const keys = await cache.keys()
  const tagged = []
  for (const req of keys) {
    const res = await cache.match(req)
    const t = Number(res?.headers.get('X-Dogear-Touched') || 0)
    tagged.push({ req, t: t || (res?.headers.get('Date') ? 0 : 0) })
  }
  if (tagged.length <= KEEP_BOOKS) return
  // sort oldest first; delete beyond KEEP_BOOKS
  tagged.sort((a, b) => a.t - b.t)
  for (let i = 0; i < tagged.length - KEEP_BOOKS; i++) {
    await cache.delete(tagged[i].req)
  }
}

self.addEventListener('fetch', (event) => {
  const url = new URL(event.request.url)
  if (url.origin !== self.location.origin) return
  // auth + admin APIs: never touch (stale caches caused login loops)
  if (url.pathname.startsWith('/api/auth/') || url.pathname.startsWith('/api/admin/')) return

  // book content: cache-first when present, else network + save
  if (url.pathname.endsWith('/content')) {
    event.respondWith((async () => {
      const cache = await caches.open(BOOK_CACHE)
      const cached = await cache.match(event.request)
      if (cached) {
        touch(url.pathname)
        return cached
      }
      try {
        const res = await fetch(event.request)
        if (res.ok && event.request.method === 'GET') {
          // only cache full 200 responses; skip 206 partials
          if (res.status === 200) await cache.put(url.pathname, res.clone())
        }
        return res
      } catch (e) {
        return new Response('offline', { status: 504 })
      }
    })())
    return
  }

  // everything else: network-first, fallback to shell cache
  event.respondWith((async () => {
    const cache = await caches.open(SHELL_CACHE)
    try {
      const res = await fetch(event.request)
      return res
    } catch (e) {
      const cached = await cache.match(event.request, { ignoreSearch: true })
      if (cached) return cached
      // SPA fallback for navigation requests
      if (event.request.mode === 'navigate') {
        const index = await cache.match('/')
        if (index) return index
      }
      return new Response('offline', { status: 504 })
    }
  })())
})