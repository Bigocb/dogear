// Offline downloads: books the user explicitly taps "Download" on are stored in
// the Cache API under their /content URL; the service worker serves them
// cache-first. Metadata (library, progress, notes, covers) is cached by the
// service worker as it is browsed, and refreshed here when a book is downloaded.
//
// Books live in a cache whose name is NOT versioned, so deploying a new service
// worker never wipes downloads. Removal is manual only.
const Offline = (() => {
  const BOOKS = 'dogear-books';
  const supported = typeof caches !== 'undefined' && window.isSecureContext;
  const contentPath = (id) => `/api/books/${id}/content`;
  const downloading = new Map(); // id -> fraction 0..1

  async function ids() {
    if (!supported) return new Set();
    const cache = await caches.open(BOOKS);
    const out = new Set();
    for (const req of await cache.keys()) {
      const m = new URL(req.url).pathname.match(/^\/api\/books\/(\d+)\/content$/);
      if (m) out.add(Number(m[1]));
    }
    return out;
  }

  async function has(id) {
    if (!supported) return false;
    return !!(await (await caches.open(BOOKS)).match(contentPath(id)));
  }

  // Ask the browser not to evict our storage. iOS honours this for installed
  // web apps; failure is harmless.
  async function persist() {
    try { if (navigator.storage?.persist) await navigator.storage.persist(); } catch {}
  }

  async function download(id, onProgress) {
    if (!supported) throw new Error('Offline storage needs the installed app over HTTPS');
    if (downloading.has(id)) return;
    downloading.set(id, 0);
    try {
      await persist();
      const res = await fetch(contentPath(id), { cache: 'no-store' });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const total = Number(res.headers.get('Content-Length')) || 0;
      const chunks = [];
      let got = 0;
      const reader = res.body.getReader();
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        chunks.push(value);
        got += value.length;
        const f = total ? Math.min(1, got / total) : 0;
        downloading.set(id, f);
        onProgress?.(f, got);
      }
      if (!got) throw new Error('empty file');
      const type = res.headers.get('Content-Type') || 'application/octet-stream';
      const cache = await caches.open(BOOKS);
      await cache.put(contentPath(id), new Response(new Blob(chunks, { type }), {
        status: 200,
        headers: { 'Content-Type': type, 'Content-Length': String(got) },
      }));
      // warm the metadata the reader needs so the book opens fully offline
      await Promise.allSettled([
        `/api/books/${id}`, `/api/books/${id}/progress`,
        `/api/books/${id}/highlights`, `/api/books/${id}/bookmarks`,
        `/api/covers/${id}`,
      ].map(u => fetch(u)));
    } finally {
      downloading.delete(id);
    }
  }

  async function remove(id) {
    if (!supported) return;
    await (await caches.open(BOOKS)).delete(contentPath(id));
  }

  // Sign-out on a shared device: drop everything we stored.
  async function clearAll() {
    if (!supported) return;
    for (const n of await caches.keys()) if (n === BOOKS || n.startsWith('dogear-data-')) await caches.delete(n);
  }

  async function usage() {
    try {
      const e = await navigator.storage.estimate();
      return { used: e.usage || 0, quota: e.quota || 0 };
    } catch { return null; }
  }

  const fmt = (n) => n >= 1e9 ? (n / 1e9).toFixed(1) + ' GB' : n >= 1e6 ? Math.round(n / 1e6) + ' MB' : Math.round(n / 1e3) + ' KB';

  return { supported, ids, has, download, remove, clearAll, usage, fmt, downloading };
})();
