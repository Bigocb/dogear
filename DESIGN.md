# Dogear — Design Document

A self-hosted ebook library manager ("Sonarr for books") with a built-in
Kindle-like web reader, powered by Shelfmark for acquisition.

**Status:** Design draft v0.1 (2026-09-30). Not yet implemented.

---

## 1. Problem

There is no single self-hosted app that combines:

1. **Library management** — track owned/wanted books like Sonarr
   (wanted → grabbed → downloaded → read), with metadata lookup.
2. **Acquisition** — find and download books from Anna's Archive /
   Libgen / torrents with Cloudflare-bypass handling.
3. **Reading** — a polished, Kindle-like web reader on iOS (PWA),
   with reading-progress sync.

Existing tools cover slices:

| Tool | Management | Acquisition | Reading | Gap |
|---|---|---|---|---|
| Readarr (retired) | ✅ | ✅ | ❌ | Archived 2025, no fork |
| Calibre-Web(-Automated) | ➖ | ❌ | ➖ | User dislikes; reading UX weak |
| Komga | ❌ (read-only serve) | ❌ | ➖ | No wanted/arr flow; comics-first |
| Kavita | ➖ | ❌ | ➖ | OPDS/web reader; no acquisition |
| Tome / BookHeaven / bookarr | ➖ | ❌ | ➖ | Very early (⭐<200), no AA integration |

**Conclusion:** build the thin manager + reader, reuse Shelfmark for the
hard acquisition parts.

## 2. Name

**Dogear** — the folded corner mark of a read page. (Working title.)

## 3. Architecture

```
┌───────────────────────────────────┐
│  Dogear (new app)                 │
│                                   │
│  ├─ Library DB (SQLite)           │
│  │    books, wanted, progress     │
│  ├─ Watcher (ingest folder)       │
│  ├─ Shelfmark client              │────→ Shelfmark existing API
│  │    (search / grab / status)    │       ├─ /api/metadata/search
│  ├─ OPDS v2 feed                 │       ├─ /api/releases
│  ├─ Web reader (epub.js PWA)     │       └─ /api/releases/download
│  └─ Progress API                 │
└───────────────────────────────────┘
        │           ↑ ingests completed files
        │           │  from Shelfmark output folder
        ▼           │
   [library folder on disk]
        │
        ▼  (optional, existing)
   Audiobookshelf  ← audiobooks keep flowing to ABS as today
```

- **Dogear never downloads books itself.** It requests Shelfmark to
  acquire, then imports whatever Shelfmark delivers to the ingestion
  folder. Acquisition complexity (AA, zlib, Cloudflare, torrents,
  queueing) stays in Shelfmark.
- Audiobooks remain an Audiobookshelf concern (Shelfmark already
  integrates with ABS). Dogear is **ebooks only** (epub, mobi, azw3,
  pdf, fb2).

## 4. Shelfmark integration (the contract)

Shelfmark (running at `http://shelfmark:8084`) exposes a JSON API
(authenticated via session cookie or API token — see §4.4).

### 4.1 Search metadata

```
GET /api/metadata/search?query=...&provider=hardcover
GET /api/metadata/book/{provider}/{book_id}
```
Returns title, author, ISBN, cover, identifiers. Used to build wanted
entries.

### 4.2 Find releases

```
GET /api/releases?provider=hardcover&book_id=...
```
Returns downloadable releases across configured release sources
(Anna's Archive, Libgen, …).

### 4.3 Queue download

```
POST /api/releases/download   (payload: chosen release)
GET  /api/downloads/active    (poll grab status)
```
Shelfmark downloads → moves to its configured output
(`/home/bigocb/media-stack/configs/calibre-web-automated/ingest`).

### 4.4 Auth

Reuse Shelfmark user auth (`POST /api/auth/login`) with a dedicated
`dogear` service user, or an API key if Shelfmark gains one. Config in
`.env`.

## 5. Data model (SQLite)

```sql
-- A book we know about (from metadata or import)
CREATE TABLE books (
  id            INTEGER PRIMARY KEY,
  title         TEXT NOT NULL,
  author        TEXT,
  isbn          TEXT,
  provider      TEXT,          -- hardcover/openlibrary (source of metadata)
  provider_id   TEXT,          -- provider's book id
  cover_url     TEXT,
  cover_file    TEXT,          -- local cached cover
  status        TEXT NOT NULL DEFAULT 'wanted',
      -- wanted | grabbed | imported | reading | read | abandoned
  added_at      INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);

-- Links into Shelfmark
CREATE TABLE grabs (
  id            INTEGER PRIMARY KEY,
  book_id       INTEGER REFERENCES books(id),
  release_ref   TEXT,          -- shelfmark release identifier
  requested_at  INTEGER,
  state         TEXT,          -- queued | downloading | done | failed
  error         TEXT
);

-- Which file(s) represent this book in the library
CREATE TABLE files (
  id            INTEGER PRIMARY KEY,
  book_id       INTEGER REFERENCES books(id),
  path          TEXT NOT NULL, -- absolute path in library folder
  format        TEXT,          -- epub / pdf / mobi ...
  size          INTEGER,
  imported_at   INTEGER
);

-- Reading progress (one per book per user; single-user for v1)
CREATE TABLE progress (
  book_id       INTEGER PRIMARY KEY REFERENCES books(id),
  cfis          REAL,   -- canonical fragment id (epub CFI)
  percent       REAL,
  device        TEXT,
  updated_at    INTEGER
);

-- Wanted-search cache so results come back fast
CREATE TABLE search_cache (
  query         TEXT,
  provider      TEXT,
  payload       TEXT,          -- raw JSON
  fetched_at    INTEGER
);
```

## 6. State machine

```
        metadata search / manual add
                    │
                    ▼
                 WANTED ────┐  grab requested
                    │       ▼
                    │   GRABBED (queued in Shelfmark)
                    │       │ ingest watcher sees file
                    ▼       ▼
                 IMPORTED ───▶ READING ──▶ READ
                    │                      │
                    └──────── abandoned ◀──┘
```

- `wanted → grabbed`: Dogear picks the best release automatically
  (prefer epub > mobi > pdf; prefer lowest waitlist source) then calls
  the download endpoint. Manual pick optional.
- `grabbed → imported`: filesystem watcher (fsnotify/inotify) on the
  Shelfmark ingest dir; on new file, match to the grab (by
  title/author/ISBN fuzzy match), move/organize into
  `<library>/<Author>/<Title>/`, extract cover + metadata (epub has
  OPF), write `files` row.
- Reader marks `reading` on first open, `read` on 98%+ or manual mark.

## 7. Library layout

```
/mnt/media/media/ebooks/
  David Cloutier/
    Project Hail Mary/
      Project Hail Mary.epub
      cover.jpg
      metadata.json        <- dogear's own book id inside
```

Owned copy of metadata in `metadata.json` keeps the directory
self-describing and re-importable without the DB.

## 8. Reader (the actual product)

Web-based, PWA installed on iOS ("Add to Home Screen" → standalone).

- **Rendering:** epub.js (or foliate-js — lighter, better maintained)
  rendering paginated two-column-less single page, CFIs for position.
- **Navigation:** swipe, tap zones, volume buttons not possible on
  iOS web — swipe + chapter list.
- **Typography:** font choice (system serif/sans + a few packaged),
  size, line height, margins, day/sepia/night themes, brightness
  dimmer.
- **Progress:** store CFI + percent per book on every page turn
  (throttled), restore exactly. Sync across devices via the API.
- **Offline:** PWA service worker caches the reader app + currently
  opened book file. Read on the subway; progress syncs later.
- **Kindle feel:** page-turn animation optional, "location" counter
  (CFI→approx page), time-to-finish estimate, per-book position ring
  on covers.
- **Pdf** files: pdf.js embedded, separate code path, minimal.

## 9. API surface (Dogear's own)

```
GET    /api/books?status=...            list/filter
POST   /api/books                       add manual (title/author)
POST   /api/books/wanted                add from metadata provider
DELETE /api/books/:id                   remove (and optionally files)
POST   /api/books/:id/grab              trigger shelfmark grab
GET    /api/books/:id/progress          reader position
PUT    /api/books/:id/progress          save position
GET    /api/books/:id/content           (reader) open file stream
GET    /api/covers/:id                  cover image
GET    /opds                            OPDS v2 catalog feed
GET    /opds/:id/manifest               readium webpub manifest (v2?)
GET    /api/health
```

Auth: single-user passcode (config), session cookie; no user system
in v1.

## 10. Stack

| Piece | Choice | Why |
|---|---|---|
| Backend | Go (chi/echo) or Node Fastify | matches other homelab apps; small |
| DB | SQLite | fits scale (<10k books) |
| Reader UI | foliate-js + Vite + vanilla/preact | light, iOS-safe |
| OPDS | self-generate (simple XML/JSON) | trivial |
| Watcher | fsnotify (Go) / chokidar (node) | stdlib-ish |
| Epub metadata | golang epub parser or epub.js on client | only need title/author/ISBN/cover |
| Container | multi-stage docker, compose block in media-stack | consistent |
| UI | Vite PWA, service worker precache | offline reading |

## 11. Non-goals (v1)

- Multi-user (future: household sharing).
- Audiobook hosting/playing (ABS covers it).
- Annotations/highlights sync (future; CFI makes it feasible).
- Comic/manga formats.
- Kobo/Kindle device sync (reading is in-browser PWA).
- Prowlarr/torrent direct integration (Shelfmark owns acquisition).

## 12. Milestones

1. **M1 — Skeleton:** Go/Node service + SQLite + `books` CRUD +
   basic web UI (library grid, add/search via Shelfmark metadata).
2. **M2 — Acquisition:** wanted → grab via Shelfmark API → ingest
   watcher → import to library folder → state updates.
3. **M3 — Reader:** open an epub in foliate-js, paginated, progress
   saved/restored. PWA manifest. OPDS feed.
4. **M4 — Polish:** themes, fonts, offline caching, reading streak
   stats, series grouping, duplicate detection at import.

## 13. Deployment

- `docker-compose` block in `/home/bigocb/media-stack/docker-compose.yml`:
  `dogear` (the app) + mount `/mnt/media/media/ebooks` + Shelfmark's
  ingest dir (read: watch, move-out) + reverse proxy hostname
  `books.cloutier.work` (tunnel route + Traefik router, same recipe
  as other services).
- Single container, no DB service (SQLite file in `configs/dogear/`).

## 14. Risks / open questions

- **Shelfmark API stability:** it's a small project (⭐19); endpoints
  may change. Mitigation: thin client module, pin its version.
- **Shelfmark search DDoS-Guard issue** (see AGENTS.md): search
  currently fails unless patched; the app must surface "search
  unavailable" gracefully.
- **iOS PWA quirks:** storage limits for offline books (~large books
  may not cache); test early. 200MB per-book cap fallback: stream
  only when online.
- **epub variants:** DRM-free assumption; pdf via pdf.js is second
  priority.