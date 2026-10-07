package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// buildStamp is set at process start; it versions static asset URLs so
// every restart/deploy produces new URLs, defeating any stale browser cache.
var buildStamp = strconv.FormatInt(time.Now().Unix(), 10)

// serveVersionedHTML serves an html file with asset URLs stamped with
// the current buildStamp.
func serveVersionedHTML(w http.ResponseWriter, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "missing "+path, http.StatusInternalServerError)
		return
	}
	html := string(data)
	html = strings.ReplaceAll(html, "/static/app.js\"", "/static/app.js?v="+buildStamp+"\"")
	html = strings.ReplaceAll(html, "/static/style.css\"", "/static/style.css?v="+buildStamp+"\"")
	html = strings.ReplaceAll(html, "/static/reader.js\"", "/static/reader.js?v="+buildStamp+"\"")
	html = strings.ReplaceAll(html, "/static/reader.css\"", "/static/reader.css?v="+buildStamp+"\"")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.WriteString(w, html)
}

type Book struct {
	ID          int64    `json:"id"`
	Title       string   `json:"title"`
	Author      string   `json:"author"`
	ISBN        string   `json:"isbn"`
	Provider    *string  `json:"provider"`
	ProviderID  *string  `json:"provider_id"`
	CoverURL    *string  `json:"cover_url"`
	CoverFile   *string  `json:"cover_file"`
	Status      string   `json:"status"`
	AddedAt     int64    `json:"added_at"`
	UpdatedAt   int64    `json:"updated_at"`
	Description *string  `json:"description,omitempty"`
	Publisher   *string  `json:"publisher,omitempty"`
	PublishYear *int     `json:"publish_year,omitempty"`
	Language    *string  `json:"language,omitempty"`
	SeriesName  *string  `json:"series_name,omitempty"`
	SeriesPos   *float64 `json:"series_position,omitempty"`
	Genres      *string  `json:"genres,omitempty"`
	PageCount   *int     `json:"page_count,omitempty"`
}

type Store struct {
	db *sql.DB
}

func openStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS books (
  id            INTEGER PRIMARY KEY,
  title         TEXT NOT NULL,
  author        TEXT,
  isbn          TEXT,
  provider      TEXT,
  provider_id   TEXT,
  cover_url     TEXT,
  cover_file    TEXT,
  status        TEXT NOT NULL DEFAULT 'wanted',
  added_at      INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  description    TEXT,
  publisher      TEXT,
  publish_year   INTEGER,
  language       TEXT,
  series_name    TEXT,
  series_position REAL,
  genres         TEXT,
  page_count     INTEGER,
  enriched_at    INTEGER
);
CREATE INDEX IF NOT EXISTS idx_books_status ON books(status);
CREATE UNIQUE INDEX IF NOT EXISTS idx_books_provider ON books(provider, provider_id) WHERE provider_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS grabs (
  id            INTEGER PRIMARY KEY,
  book_id       INTEGER NOT NULL REFERENCES books(id),
  release_ref   TEXT,
  state         TEXT NOT NULL DEFAULT 'queued',
  error         TEXT,
  requested_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_grabs_book ON grabs(book_id);

CREATE TABLE IF NOT EXISTS files (
  id            INTEGER PRIMARY KEY,
  book_id       INTEGER NOT NULL REFERENCES books(id),
  path          TEXT NOT NULL UNIQUE,
  format        TEXT,
  size          INTEGER,
  imported_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_files_book ON files(book_id);

CREATE TABLE IF NOT EXISTS progress (
  book_id       INTEGER PRIMARY KEY REFERENCES books(id),
  cfi           TEXT,
  percent       REAL,
  device        TEXT,
  updated_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS highlights (
  id            INTEGER PRIMARY KEY,
  book_id       INTEGER NOT NULL REFERENCES books(id),
  cfi           TEXT NOT NULL,        -- range CFI "start/end"
  text          TEXT NOT NULL,        -- excerpt for display/search
  note          TEXT,                 -- optional annotation
  color         TEXT NOT NULL DEFAULT 'yellow',
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_highlights_book ON highlights(book_id);

CREATE TABLE IF NOT EXISTS bookmarks (
  id            INTEGER PRIMARY KEY,
  book_id       INTEGER NOT NULL REFERENCES books(id),
  cfi           TEXT NOT NULL,
  label         TEXT,
  percent       REAL,
  created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_bookmarks_book ON bookmarks(book_id);

-- enrichment columns (nullable; filled from Hardcover/OpenLibrary via Shelfmark)
-- added via ALTER in migrateMeta() for existing DBs
`)
	return err
}

func (s *Store) close() error { return s.db.Close() }

// migrateMeta adds optional metadata-enrichment columns to books.
func (s *Store) migrateMeta() error {
	cols := []struct{ name, def string }{
		{"description", "TEXT"},
		{"publisher", "TEXT"},
		{"publish_year", "INTEGER"},
		{"language", "TEXT"},
		{"series_name", "TEXT"},
		{"series_position", "REAL"},
		{"genres", "TEXT"},
		{"page_count", "INTEGER"},
		{"enriched_at", "INTEGER"},
	}
	for _, c := range cols {
		var n int
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('books') WHERE name=?`, c.name).Scan(&n)
		if n == 0 {
			if _, err := s.db.Exec(`ALTER TABLE books ADD COLUMN ` + c.name + ` ` + c.def); err != nil {
				return err
			}
		}
	}
	return nil
}

const bookColNames = `id,title,author,isbn,provider,provider_id,cover_url,cover_file,status,added_at,updated_at,description,publisher,publish_year,language,series_name,series_position,genres,page_count`
const bookCols = bookColNames

// bookColsP returns the column list prefixed (e.g. "b." for JOINs).
func bookColsP(prefix string) string {
	fields := strings.Split(bookColNames, ",")
	for i, f := range fields {
		fields[i] = prefix + f
	}
	return strings.Join(fields, ",")
}

type rowScanner interface{ Scan(dest ...any) error }

func scanBook(sc rowScanner) (Book, error) {
	var b Book
	err := sc.Scan(&b.ID, &b.Title, &b.Author, &b.ISBN, &b.Provider, &b.ProviderID, &b.CoverURL, &b.CoverFile,
		&b.Status, &b.AddedAt, &b.UpdatedAt,
		&b.Description, &b.Publisher, &b.PublishYear, &b.Language, &b.SeriesName, &b.SeriesPos, &b.Genres, &b.PageCount)
	return b, err
}

// applyEnrichment writes metadata from a provider lookup onto a book.
func (s *Store) applyEnrichment(id int64, d *MetaDetail) error {
	var cover *string
	if d.CoverURL != nil && *d.CoverURL != "" {
		// keep the relative shelfmark path; handleCover proxies it
		cover = d.CoverURL
	}
	var genres *string
	if len(d.Genres) > 0 {
		g := strings.Join(d.Genres, ", ")
		genres = &g
	}
	_, err := s.db.Exec(`UPDATE books SET
		description = COALESCE(NULLIF(?,''), description),
		publisher   = COALESCE(?, publisher),
		publish_year= COALESCE(?, publish_year),
		language    = COALESCE(?, language),
		series_name = COALESCE(?, series_name),
		series_position = COALESCE(?, series_position),
		genres      = COALESCE(?, genres),
		cover_url   = COALESCE(cover_url, ?),
		enriched_at = strftime('%s','now'),
		updated_at  = strftime('%s','now')
		WHERE id=?`,
		d.Description, d.Publisher, d.PublishYear, d.Language, d.SeriesName, d.SeriesPosition, genres, cover, id)
	return err
}

func (s *Store) listBooks(status string, query string) ([]Book, error) {
	sqlBase := `SELECT ` + bookCols + ` FROM books`
	var conds []string
	var args []any
	if status != "" {
		conds = append(conds, "status=?")
		args = append(args, status)
	}
	if query != "" {
		q := "%" + strings.ToLower(query) + "%"
		conds = append(conds, "(lower(title) LIKE ? OR lower(coalesce(author,'')) LIKE ?)")
		args = append(args, q, q)
	}
	sqlQ := sqlBase
	if len(conds) > 0 {
		sqlQ += " WHERE " + strings.Join(conds, " AND ")
	}
	sqlQ += " ORDER BY added_at DESC"
	rows, err := s.db.Query(sqlQ, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Book
	for rows.Next() {
		b, err := scanBook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) getBook(id int64) (*Book, error) {
	b, err := scanBook(s.db.QueryRow(`SELECT `+bookCols+` FROM books WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *Store) findBookByProvider(provider, providerID string) (*Book, error) {
	b, err := scanBook(s.db.QueryRow(`SELECT `+bookCols+` FROM books WHERE provider=? AND provider_id=?`, provider, providerID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *Store) addBook(b *Book) (int64, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO books(title,author,isbn,provider,provider_id,cover_url,status,added_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		b.Title, b.Author, b.ISBN, b.Provider, b.ProviderID, b.CoverURL, "wanted", now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) setStatus(id int64, status string) error {
	res, err := s.db.Exec(`UPDATE books SET status=?, updated_at=? WHERE id=?`, status, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("book %d not found", id)
	}
	return nil
}

func (s *Store) deleteBook(id int64) error {
	for _, q := range []string{`DELETE FROM progress WHERE book_id=?`, `DELETE FROM files WHERE book_id=?`, `DELETE FROM grabs WHERE book_id=?`, `DELETE FROM books WHERE id=?`} {
		if _, err := s.db.Exec(q, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) setProgress(bookID int64, cfi string, percent float64, device string) error {
	_, err := s.db.Exec(`INSERT INTO progress(book_id,cfi,percent,device,updated_at) VALUES(?,?,?,?,?)
		ON CONFLICT(book_id) DO UPDATE SET cfi=excluded.cfi, percent=excluded.percent, device=excluded.device, updated_at=excluded.updated_at`,
		bookID, cfi, percent, device, time.Now().Unix())
	return err
}

func (s *Store) getProgress(bookID int64) (cfi string, percent float64, err error) {
	row := s.db.QueryRow(`SELECT cfi,percent FROM progress WHERE book_id=?`, bookID)
	err = row.Scan(&cfi, &percent)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	return
}

func (s *Store) addGrab(bookID int64, releaseRef, state string) error {
	_, err := s.db.Exec(`INSERT INTO grabs(book_id,release_ref,state,requested_at) VALUES(?,?,?,?)`,
		bookID, releaseRef, state, time.Now().Unix())
	return err
}

var bookStatuses = map[string]bool{"wanted": true, "grabbed": true, "imported": true, "reading": true, "read": true, "abandoned": true}

// fileExistsBySize reports whether a file row with this size is already imported.
// Used to avoid re-importing copies of the same book.
func (s *Store) fileExistsBySize(size int64) bool {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM files WHERE size=?`, size).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

func (s *Store) booksInStatuses(statuses ...string) ([]*Book, error) {
	args := make([]any, len(statuses))
	ph := make([]string, len(statuses))
	for i, st := range statuses {
		args[i] = st
		ph[i] = "?"
	}
	rows, err := s.db.Query(`SELECT id,title,author,isbn,provider,provider_id,cover_url,cover_file,status,added_at,updated_at
		FROM books WHERE status IN (`+strings.Join(ph, ",")+`) ORDER BY added_at ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Book
	for rows.Next() {
		var b Book
		if err := rows.Scan(&b.ID, &b.Title, &b.Author, &b.ISBN, &b.Provider, &b.ProviderID, &b.CoverURL, &b.CoverFile, &b.Status, &b.AddedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &b)
	}
	return out, rows.Err()
}

// handleHome returns rows for the Kindle-style home screen:
// continue (reading books w/ progress), recently-added, and counts.
func (a *apiServer) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	type homeBook struct {
		Book
		Percent float64 `json:"percent"`
	}
	mk := func(books []Book, percents map[int64]float64) []homeBook {
		out := make([]homeBook, 0, len(books))
		for _, b := range books {
			out = append(out, homeBook{Book: b, Percent: percents[b.ID]})
		}
		return out
	}
	all, err := a.store.shelfBooks(userFromCtx(r).ID, "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	percents := map[int64]float64{}
	for _, b := range all {
		if _, pct, err := a.store.getProgressForUser(userFromCtx(r).ID, b.ID); err == nil {
			percents[b.ID] = pct
		}
	}
	var reading, wantedCount int
	var continueRow []Book
	var recentRow []Book
	for i, b := range all {
		switch b.Status {
		case "reading":
			reading++
			if len(continueRow) < 12 {
				continueRow = append(continueRow, all[i])
			}
		case "imported":
			// "recently added" = latest imported (list is already desc by added_at)
			if len(recentRow) < 12 {
				recentRow = append(recentRow, b)
			}
		}
		if b.Status == "wanted" || b.Status == "grabbed" {
			wantedCount++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"continue":     mk(continueRow, percents),
		"recent":       mk(recentRow, percents),
		"count_read":   len(all) - reading - wantedCount,
		"count_all":    len(all),
		"count_wanted": wantedCount,
	})
}

// handleAdminStatus returns the admin panel data: counts, ingest health,
// shelfmark/AA status, and recent activity.
// handleEnrich fills in description/cover-series metadata for a book using
// Shelfmark's metadata providers (Hardcover/OpenLibrary).
// POST /api/books/{id}/enrich   (single)
// POST /api/admin/enrich        (batch: enrich books missing a description)
func (a *apiServer) handleEnrich(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	book, err := a.store.getBook(id)
	if err != nil || book == nil {
		writeErr(w, http.StatusNotFound, "book not found")
		return
	}
	query := book.Title
	if book.Author != "" {
		query += " " + book.Author
	}
	detail, err := a.sm.EnrichSearch(r.Context(), query, book.Author)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "enrich lookup: "+err.Error())
		return
	}
	if detail == nil {
		writeErr(w, http.StatusNotFound, "no metadata match found")
		return
	}
	if err := a.store.applyEnrichment(id, detail); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	b, _ := a.store.getBook(id)
	writeJSON(w, http.StatusOK, b)
}

// enrichJob tracks a running batch enrichment.
type enrichJob struct {
	Running bool  `json:"running"`
	Total   int   `json:"total"`
	Done    int   `json:"done"`
	Failed  int   `json:"failed"`
	Started int64 `json:"started"`
}

var currentEnrich = &enrichJob{}

// handleEnrichBatch starts enrichment in the background and returns immediately.
func (a *apiServer) handleEnrichBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if currentEnrich.Running {
		writeJSON(w, http.StatusOK, map[string]any{"already_running": true, "job": currentEnrich})
		return
	}
	books, err := a.store.listBooks("", "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	todo := make([]Book, 0, len(books))
	for i := range books {
		if books[i].Description == nil || *books[i].Description == "" {
			todo = append(todo, books[i])
		}
	}
	currentEnrich = &enrichJob{Running: true, Total: len(todo), Started: time.Now().Unix()}
	go func() {
		for i := range todo {
			b := todo[i]
			query := b.Title
			if b.Author != "" {
				query += " " + b.Author
			}
			detail, err := a.sm.EnrichSearch(context.Background(), query, b.Author)
			if err != nil || detail == nil {
				// retry once with title only
				if detail2, err2 := a.sm.EnrichSearch(context.Background(), b.Title, ""); err2 == nil && detail2 != nil {
					detail = detail2
				} else {
					currentEnrich.Failed++
					currentEnrich.Done++
					continue
				}
			}
			if err := a.store.applyEnrichment(b.ID, detail); err != nil {
				currentEnrich.Failed++
			}
			currentEnrich.Done++
		}
		currentEnrich.Running = false
	}()
	writeJSON(w, http.StatusOK, map[string]any{"started": true, "total": currentEnrich.Total})
}

// handleEnrichStatus reports progress of the running batch.
func (a *apiServer) handleEnrichStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, currentEnrich)
}

// handleReadingHeartbeat accumulates reading time for the session.
// POST /api/books/{id}/reading {seconds: n, pages: n}
func (a *apiServer) handleReadingHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	user := userFromCtx(r)
	if user == nil {
		writeErr(w, http.StatusUnauthorized, "login required")
		return
	}
	var body struct {
		Seconds int `json:"seconds"`
		Pages   int `json:"pages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := a.store.addReadingTime(user.ID, id, body.Seconds, body.Pages); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleStats returns the per-user reading stats dashboard.
// GET /api/stats
func (a *apiServer) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	user := userFromCtx(r)
	if user == nil {
		writeErr(w, http.StatusUnauthorized, "login required")
		return
	}
	stats, err := a.store.userStats(user.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (a *apiServer) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	type activity struct {
		Time   string `json:"time"`
		Event  string `json:"event"`
		Detail string `json:"detail"`
	}
	var bookCount, fileCount, grabCount int64
	_ = a.store.db.QueryRow(`SELECT COUNT(*) FROM books`).Scan(&bookCount)
	_ = a.store.db.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&fileCount)
	_ = a.store.db.QueryRow(`SELECT COUNT(*) FROM grabs`).Scan(&grabCount)

	rows, err := a.store.db.Query(`
		SELECT strftime('%s','now') - requested_at AS age, 'grab', coalesce(release_ref,'') FROM grabs
		UNION ALL
		SELECT strftime('%s','now') - imported_at AS age, 'import', path FROM files
		ORDER BY age ASC LIMIT 20`)
	acts := []activity{}
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var age int64
			var ev, det string
			if err := rows.Scan(&age, &ev, &det); err == nil {
				t := time.Now().Add(-time.Duration(age) * time.Second)
				acts = append(acts, activity{Time: t.Format(time.RFC3339), Event: ev, Detail: det})
			}
		}
	}

	// shelfmark status (best-effort)
	smStatus := map[string]any{"reachable": false}
	if resp, err := http.Get(a.sm.base + "/api/health"); err == nil {
		resp.Body.Close()
		smStatus["reachable"] = resp.StatusCode == http.StatusOK
		smStatus["status_code"] = resp.StatusCode
	}

	aaStatus := map[string]any{"key_configured": a.aa != nil && a.aa.key() != ""}

	writeJSON(w, http.StatusOK, map[string]any{
		"counts": map[string]any{
			"books": bookCount, "files": fileCount, "grabs": grabCount,
		},
		"activity":    acts,
		"shelfmark":   smStatus,
		"aa":          aaStatus,
		"ingest_dirs": a.importer.Ingests,
		"library_dir": a.importer.Library,
		"copy_mode":   a.importer.CopyMode,
	})
}

type Highlight struct {
	ID        int64   `json:"id"`
	BookID    int64   `json:"book_id"`
	CFI       string  `json:"cfi"`
	Text      string  `json:"text"`
	Note      *string `json:"note"`
	Color     string  `json:"color"`
	CreatedAt int64   `json:"created_at"`
	UpdatedAt int64   `json:"updated_at"`
}

type Bookmark struct {
	ID        int64   `json:"id"`
	BookID    int64   `json:"book_id"`
	CFI       string  `json:"cfi"`
	Label     *string `json:"label"`
	Percent   float64 `json:"percent"`
	CreatedAt int64   `json:"created_at"`
}

func (s *Store) listHighlights(bookID int64) ([]Highlight, error) {
	rows, err := s.db.Query(`SELECT id,book_id,cfi,text,note,color,created_at,updated_at FROM highlights WHERE book_id=? ORDER BY created_at DESC`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Highlight
	for rows.Next() {
		var hl Highlight
		if err := rows.Scan(&hl.ID, &hl.BookID, &hl.CFI, &hl.Text, &hl.Note, &hl.Color, &hl.CreatedAt, &hl.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, hl)
	}
	return out, rows.Err()
}

func (s *Store) addHighlight(bookID int64, cfi, text, color string, note *string) (int64, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO highlights(book_id,cfi,text,note,color,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`,
		bookID, cfi, text, note, color, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) updateHighlight(id int64, color string, note *string) error {
	res, err := s.db.Exec(`UPDATE highlights SET color=?, note=?, updated_at=? WHERE id=?`, color, note, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("highlight %d not found", id)
	}
	return nil
}

func (s *Store) deleteHighlight(id int64) error {
	res, err := s.db.Exec(`DELETE FROM highlights WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("highlight %d not found", id)
	}
	return nil
}

func (s *Store) listBookmarks(bookID int64) ([]Bookmark, error) {
	rows, err := s.db.Query(`SELECT id,book_id,cfi,label,percent,created_at FROM bookmarks WHERE book_id=? ORDER BY percent ASC`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bookmark
	for rows.Next() {
		var bm Bookmark
		if err := rows.Scan(&bm.ID, &bm.BookID, &bm.CFI, &bm.Label, &bm.Percent, &bm.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, bm)
	}
	return out, rows.Err()
}

func (s *Store) addBookmark(bookID int64, cfi string, label *string, percent float64) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO bookmarks(book_id,cfi,label,percent,created_at) VALUES(?,?,?,?,?)`,
		bookID, cfi, label, percent, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) deleteBookmark(id int64) error {
	res, err := s.db.Exec(`DELETE FROM bookmarks WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("bookmark %d not found", id)
	}
	return nil
}

// ---- HTTP handlers ----

var validStatuses = map[string]bool{"wanted": true, "grabbed": true, "imported": true, "reading": true, "read": true, "abandoned": true}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

type apiServer struct {
	store    *Store
	sm       *Shelfmark
	aa       *AAGrabber
	prowlarr *Prowlarr
	libgen   *Libgen
	importer *Importer
}

func (a *apiServer) handleBooks(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r)
	switch r.Method {
	case http.MethodGet:
		status := r.URL.Query().Get("status")
		if status != "" && !validStatuses[status] {
			writeErr(w, http.StatusBadRequest, "invalid status filter")
			return
		}
		q := r.URL.Query().Get("q")
		if user == nil {
			// anonymous = pre-login; no data
			writeErr(w, http.StatusUnauthorized, "login required")
			return
		}
		// household=1 -> browse shared library (books not on shelf)
		if r.URL.Query().Get("household") == "1" {
			books, err := a.store.unshelvedBooks(user.ID, q)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if books == nil {
				books = []Book{}
			}
			writeJSON(w, http.StatusOK, books)
			return
		}
		books, err := a.store.shelfBooks(user.ID, q)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// optional per-status filter (applied client-side too, but keep API parity)
		if status != "" {
			var filtered []Book
			for _, b := range books {
				if b.Status == status {
					filtered = append(filtered, b)
				}
			}
			books = filtered
		}
		if books == nil {
			books = []Book{}
		}
		writeJSON(w, http.StatusOK, books)
	case http.MethodPost:
		var b Book
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid json")
			return
		}
		if b.Title == "" {
			writeErr(w, http.StatusBadRequest, "title required")
			return
		}
		if user == nil {
			writeErr(w, http.StatusUnauthorized, "login required")
			return
		}
		// dedupe by provider id when present
		if b.Provider != nil && b.ProviderID != nil && *b.ProviderID != "" {
			existing, err := a.store.findBookByProvider(*b.Provider, *b.ProviderID)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			if existing != nil {
				writeJSON(w, http.StatusOK, existing)
				return
			}
		}
		id, err := a.store.addBook(&b)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := a.store.shelfAdd(user.ID, id, "wanted"); err != nil {
			writeErr(w, http.StatusInternalServerError, "shelf: "+err.Error())
			return
		}
		created, _ := a.store.getBook(id)
		created.Status = "wanted" // reflect the shelf status
		writeJSON(w, http.StatusCreated, created)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (a *apiServer) handleBook(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	switch r.Method {
	case http.MethodGet:
		// visibility: if the book is not on this user's shelf, treat as not found
		user := userFromCtx(r)
		if user != nil {
			if _, hidden, ok := a.store.shelfStatus(user.ID, id); !ok || hidden {
				writeErr(w, http.StatusNotFound, "book not on your shelf")
				return
			}
		}
		b, err := a.store.getBook(id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if b == nil {
			writeErr(w, http.StatusNotFound, "book not found")
			return
		}
		if user != nil {
			if status, _, ok := a.store.shelfStatus(user.ID, id); ok {
				b.Status = status
			}
		}
		writeJSON(w, http.StatusOK, b)
	case http.MethodDelete:
		// Netflix model: DELETE removes from YOUR shelf, not the shared library.
		user := userFromCtx(r)
		if user == nil {
			writeErr(w, http.StatusUnauthorized, "login required")
			return
		}
		if err := a.store.shelfRemove(user.ID, id); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (a *apiServer) handleBookStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !validStatuses[body.Status] {
		writeErr(w, http.StatusBadRequest, "invalid status")
		return
	}
	user := userFromCtx(r)
	if user == nil {
		writeErr(w, http.StatusUnauthorized, "login required")
		return
	}
	if err := a.store.shelfSetStatus(user.ID, id, body.Status); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	// mirror into core books table for admin/library-wide reporting
	_ = a.store.setStatus(id, body.Status)
	b, _ := a.store.getBook(id)
	if status, _, ok := a.store.shelfStatus(user.ID, id); ok {
		b.Status = status
	}
	writeJSON(w, http.StatusOK, b)
}

// handleShelfAdd puts a shared-library book onto the user's shelf.
// POST /api/books/{id}/add-to-shelf
func (a *apiServer) handleShelfAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	user := userFromCtx(r)
	if user == nil {
		writeErr(w, http.StatusUnauthorized, "login required")
		return
	}
	if err := a.store.shelfAdd(user.ID, id, "imported"); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	b, _ := a.store.getBook(id)
	writeJSON(w, http.StatusOK, b)
}

// handleGrabRelease queues a user-chosen release (from the picker UI).
func (a *apiServer) handleGrabRelease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var rel Release
	if err := json.NewDecoder(r.Body).Decode(&rel); err != nil || rel.Source == "" || rel.SourceID == "" {
		writeErr(w, http.StatusBadRequest, "invalid release payload")
		return
	}
	// route the grab to the source that produced the release
	if rel.Source == "prowlarr-direct" {
		if a.prowlarr == nil || !a.prowlarr.Configured() {
			writeErr(w, http.StatusPreconditionFailed, "prowlarr not configured")
			return
		}
		idxID := 0
		if rel.Extra != nil {
			if v, ok := rel.Extra["indexerId"].(float64); ok {
				idxID = int(v)
			}
		}
		if err := a.prowlarr.Grab(r.Context(), ProwlarrResult{
			GUID: rel.SourceID, IndexerID: idxID, Title: rel.Title,
		}); err != nil {
			writeErr(w, http.StatusBadGateway, "prowlarr grab: "+err.Error())
			return
		}
	} else if rel.Source == "libgen" {
		// search found the md5; AA's keyed API downloads it (no captcha)
		if a.aa == nil || a.aa.key() == "" {
			writeErr(w, http.StatusPreconditionFailed, "AA donator key not configured")
			return
		}
		md5 := strings.ToLower(rel.SourceID)
		if !md5Re.MatchString(md5) {
			writeErr(w, http.StatusBadRequest, "invalid libgen md5")
			return
		}
		b, _ := a.store.getBook(id)
		title, author := b.Title, b.Author
		dest, err := a.aaDownloadAndImport(r.Context(), md5, title, author)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		ext := strings.TrimPrefix(strings.ToLower(pathExt(dest)), ".")
		_, _ = a.importer.storeRecord(&Book{ID: id, Title: title, Author: author}, dest, ext, fileSize(dest), "")
		_ = a.store.setStatus(id, "imported")
		if u := userFromCtx(r); u != nil {
			_ = a.store.shelfAdd(u.ID, id, "imported")
		}
		_ = a.store.addGrab(id, "libgen:"+md5, "done")
		b2, _ := a.store.getBook(id)
		writeJSON(w, http.StatusOK, map[string]any{"book": b2})
		return
	} else {
		if err := a.sm.Grab(r.Context(), &rel); err != nil {
			writeErr(w, http.StatusBadGateway, "shelfmark grab: "+err.Error())
			return
		}
	}
	ref := rel.Source + ":" + rel.SourceID
	if err := a.store.addGrab(id, ref, "queued"); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = a.store.setStatus(id, "grabbed")
	_ = a.store.shelfAdd(userFromCtx(r).ID, id, "grabbed")
	b, _ := a.store.getBook(id)
	writeJSON(w, http.StatusOK, map[string]any{"book": b})
}

func (a *apiServer) handleProgress(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	user := userFromCtx(r)
	if user == nil {
		writeErr(w, http.StatusUnauthorized, "login required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		cfi, percent, err := a.store.getProgressForUser(user.ID, id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"book_id": id, "cfi": cfi, "percent": percent})
	case http.MethodPut:
		var body struct {
			CFI     string  `json:"cfi"`
			Percent float64 `json:"percent"`
			Device  string  `json:"device"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid json")
			return
		}
		if err := a.store.setProgressForUser(user.ID, id, body.CFI, body.Percent, body.Device); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// keep legacy single-user row in sync for admin stats
		_ = a.store.setProgress(id, body.CFI, body.Percent, body.Device)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (a *apiServer) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeErr(w, http.StatusBadRequest, "missing q")
		return
	}
	raw, err := a.sm.SearchMetadata(r.Context(), q)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "shelfmark search: "+err.Error())
		return
	}
	// map to a UI-friendly shape
	type result struct {
		Provider string  `json:"provider"`
		BookID   string  `json:"book_id"`
		Title    string  `json:"title"`
		Author   string  `json:"author"`
		ISBN     string  `json:"isbn"`
		CoverURL *string `json:"cover_url"`
	}
	results := make([]result, 0, len(raw))
	for _, m := range raw {
		var cov *string
		if m.CoverURL != nil && *m.CoverURL != "" {
			// stash the shelfmark cover path; the UI proxies via
			// /api/covers?src=<shelfmark-path>
			full := *m.CoverURL
			if strings.HasPrefix(full, "/") {
				full = "/api/shelfmark-cover?path=" + url.QueryEscape(full)
			}
			cov = &full
		}
		results = append(results, result{
			Provider: m.Provider,
			BookID:   m.BookID,
			Title:    m.Title,
			Author:   m.Author(),
			ISBN:     m.FullISBN(),
			CoverURL: cov,
		})
	}
	writeJSON(w, http.StatusOK, results)
}

// handleBookReleases lists downloadable releases for a book (manual pick UI).
func (a *apiServer) handleBookReleases(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	book, err := a.store.getBook(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeErr(w, http.StatusNotFound, "book not found")
		return
	}
	if book.Provider == nil || book.ProviderID == nil || *book.ProviderID == "" {
		if a.prowlarr == nil || !a.prowlarr.Configured() {
			writeErr(w, http.StatusBadRequest, "book has no provider id; add via search first, or enable Prowlarr")
			return
		}
	}
	// shelfmark releases (metadata-provider keyed) + prowlarr (title-search)
	releases := []Release{}
	type srcErr struct {
		Source string `json:"source"`
		Error  string `json:"error"`
	}
	errors := []srcErr{}

	// Libgen search (no captcha) -> md5s that download via the AA key.
	if a.libgen != nil {
		q := book.Title
		if book.Author != "" {
			q += " " + book.Author
		}
		if lg, err := a.libgen.Search(r.Context(), q); err != nil {
			errors = append(errors, srcErr{Source: "libgen", Error: err.Error()})
		} else {
			releases = append(releases, lg...)
		}
	}
	// Shelfmark releases are currently behind AA's captcha; skip unless the
	// caller explicitly opts in with ?shelfmark=1.
	if r.URL.Query().Get("shelfmark") == "1" && book.Provider != nil && book.ProviderID != nil && *book.ProviderID != "" && a.sm != nil {
		sm, err := a.sm.SearchReleases(r.Context(), *book.Provider, *book.ProviderID)
		if err != nil {
			errors = append(errors, srcErr{Source: "shelfmark", Error: err.Error()})
		} else {
			releases = append(releases, sm...)
		}
	}
	// Prowlarr: search by title+author; no metadata-provider id needed.
	if a.prowlarr != nil && a.prowlarr.Configured() {
		q := book.Title
		if book.Author != "" {
			q += " " + book.Author
		}
		pr, err := a.prowlarr.Search(r.Context(), q, 100)
		if err != nil {
			errors = append(errors, srcErr{Source: "prowlarr-direct", Error: err.Error()})
		} else {
			for _, pr := range pr {
				releases = append(releases, normalizeProwlarr(pr))
			}
		}
	}
	if releases == nil {
		releases = []Release{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"book": book, "releases": releases, "errors": errors})
}

func (a *apiServer) handleGrab(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	book, err := a.store.getBook(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeErr(w, http.StatusNotFound, "book not found")
		return
	}
	hasProvider := book.Provider != nil && *book.Provider != "" && book.ProviderID != nil && *book.ProviderID != ""
	if !hasProvider && (a.prowlarr == nil || !a.prowlarr.Configured()) {
		writeErr(w, http.StatusBadRequest, "book has no provider id; add via search first, or enable Prowlarr")
		return
	}
	// 1. search releases across all enabled sources
	releases := []Release{}
	if hasProvider {
		if sm, err := a.sm.SearchReleases(r.Context(), *book.Provider, *book.ProviderID); err == nil {
			releases = append(releases, sm...)
		}
	}
	if a.prowlarr != nil && a.prowlarr.Configured() {
		q := book.Title
		if book.Author != "" {
			q += " " + book.Author
		}
		if pr, err := a.prowlarr.Search(r.Context(), q, 100); err == nil {
			for _, r := range pr {
				releases = append(releases, normalizeProwlarr(r))
			}
		}
	}
	if len(releases) == 0 {
		writeErr(w, http.StatusNotFound, "no releases found")
		return
	}
	// 2. auto-pick best (respect preferred format query param ?format=epub)
	pick := PickRelease(releases, r.URL.Query().Get("format"))
	if pick == nil {
		writeErr(w, http.StatusNotFound, "no ebook-format releases (only audiobooks)")
		return
	}
	// 3. queue it via the source that produced it
	if pick.Source == "prowlarr-direct" {
		idxID := 0
		if pick.Extra != nil {
			if v, ok := pick.Extra["indexerId"].(float64); ok {
				idxID = int(v)
			}
		}
		if err := a.prowlarr.Grab(r.Context(), ProwlarrResult{GUID: pick.SourceID, IndexerID: idxID, Title: pick.Title}); err != nil {
			writeErr(w, http.StatusBadGateway, "prowlarr grab: "+err.Error())
			return
		}
	} else if err := a.sm.Grab(r.Context(), pick); err != nil {
		writeErr(w, http.StatusBadGateway, "shelfmark grab: "+err.Error())
		return
	}
	ref := pick.Source + ":" + pick.SourceID
	if err := a.store.addGrab(id, ref, "queued"); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = a.store.setStatus(id, "grabbed")
	_ = a.store.shelfAdd(userFromCtx(r).ID, id, "grabbed")
	b, _ := a.store.getBook(id)
	writeJSON(w, http.StatusOK, map[string]any{"book": b, "picked": pick})
}

// handleSettings (admin) reads/writes integration settings.
// GET  /api/admin/settings   -> current settings (secrets masked)
// PUT  /api/admin/settings   -> update; empty string clears, absent key = untouched
func (a *apiServer) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, a.store.settings())
	case http.MethodPut:
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid json")
			return
		}
		str := func(k string) (string, bool) {
			v, ok := body[k]
			if !ok {
				return "", false
			}
			s, _ := v.(string)
			return s, true
		}
		set := func(cfgKey, val string) {
			if strings.TrimSpace(val) == "" {
				_ = a.store.deleteConfig(cfgKey)
			} else {
				_ = a.store.setConfig(cfgKey, strings.TrimSpace(val))
			}
		}
		if v, ok := str("shelfmark_url"); ok {
			set(cfgShelfmarkURL, v)
		}
		if v, ok := str("shelfmark_user"); ok {
			set(cfgShelfmarkUser, v)
		}
		if v, ok := str("shelfmark_password"); ok {
			set(cfgShelfmarkPass, v)
		}
		if v, ok := str("prowlarr_url"); ok {
			set(cfgProwlarrURL, v)
		}
		if v, ok := str("prowlarr_api_key"); ok {
			set(cfgProwlarrKey, v)
		}
		if v, ok := str("aa_donator_key"); ok {
			set(cfgAAKey, v)
		}
		if v, ok := str("aa_base_url"); ok {
			set(cfgAABaseURL, v)
		}
		if v, ok := body["prowlarr_enabled"]; ok {
			b, _ := v.(bool)
			if b {
				_ = a.store.setConfig(cfgProwlarrOn, "true")
			} else {
				_ = a.store.setConfig(cfgProwlarrOn, "false")
			}
		}
		writeJSON(w, http.StatusOK, a.store.settings())
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleTestIntegration (admin) pings a configured service.
// POST /api/admin/test/{service}   service in {shelfmark, prowlarr, aa}
func (a *apiServer) handleTestIntegration(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	svc := r.PathValue("service")
	switch svc {
	case "prowlarr":
		if a.prowlarr == nil || !a.prowlarr.Configured() {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "detail": "Prowlarr URL or API key not set"})
			return
		}
		st, err := a.prowlarr.Status(r.Context())
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "detail": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "detail": fmt.Sprintf("%v %v", st["appName"], st["version"])})
	case "shelfmark":
		url := strings.TrimRight(a.store.config(cfgShelfmarkURL, "SHELFMARK_URL", "http://shelfmark:8084"), "/")
		resp, err := http.Get(url + "/api/health")
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "detail": err.Error()})
			return
		}
		defer resp.Body.Close()
		writeJSON(w, http.StatusOK, map[string]any{"ok": resp.StatusCode == 200, "detail": fmt.Sprintf("HTTP %d", resp.StatusCode)})
	case "aa":
		key := a.store.aaKey()
		if key == "" {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "detail": "Donator key not set"})
			return
		}
		base := strings.TrimRight(a.store.aaBaseURL(), "/")
		// cheap validity probe: invalid md5 => "Invalid secret key" means the key is bad
		u := base + "/dyn/api/fast_download.json?md5=00000000000000000000000000000000&key=" + url.QueryEscape(key)
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, u, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "detail": err.Error()})
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if strings.Contains(string(body), "Invalid secret key") {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "detail": "Invalid secret key"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "detail": "Key accepted"})
	default:
		writeErr(w, http.StatusNotFound, "unknown service")
	}
}

func (a *apiServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "time": time.Now().Format(time.RFC3339)})
}

func (a *apiServer) handleCover(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	b, err := a.store.getBook(id)
	if err != nil || b == nil {
		http.NotFound(w, r)
		return
	}
	// 1. local cover file (imported books)
	if b.CoverFile != nil && *b.CoverFile != "" {
		http.ServeFile(w, r, *b.CoverFile)
		return
	}
	// 2. fall back to proxying the provider cover
	if b.CoverURL == nil || *b.CoverURL == "" {
		http.NotFound(w, r)
		return
	}
	coverSrc := *b.CoverURL
	if strings.HasPrefix(coverSrc, "/") {
		coverSrc = a.sm.base + coverSrc
	}
	if !strings.HasPrefix(coverSrc, "http") {
		// a relative dogear path (shelfmark proxy) — not fetchable server-side
		http.NotFound(w, r)
		return
	}
	resp, err := http.Get(coverSrc)
	if err != nil {
		http.Error(w, "cover fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	_, _ = io.Copy(w, resp.Body)
}

func (a *apiServer) handleShelfmarkCover(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" || !strings.HasPrefix(path, "/api/covers/") {
		http.NotFound(w, r)
		return
	}
	resp, err := http.Get(a.sm.base + path)
	if err != nil {
		http.Error(w, "cover fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	_, _ = io.Copy(w, resp.Body)
}

// handleContent serves the book file itself to the web reader.
func (a *apiServer) handleContent(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if user := userFromCtx(r); user != nil {
		if _, hidden, ok := a.store.shelfStatus(user.ID, id); !ok || hidden {
			http.NotFound(w, r)
			return
		}
	}
	rows, err := a.store.db.Query(`SELECT path FROM files WHERE book_id=? ORDER BY id LIMIT 1`, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var path string
	if !rows.Next() {
		rows.Close()
		writeErr(w, http.StatusNotFound, "no file for book")
		return
	}
	if err := rows.Scan(&path); err != nil {
		rows.Close()
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	rows.Close()

	f, err := os.Open(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "file missing")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", epubMIME(filepath.Ext(path)))
	http.ServeContent(w, r, filepath.Base(path), modTime(path), f)
}

func epubMIME(ext string) string {
	switch strings.ToLower(ext) {
	case ".epub":
		return "application/epub+zip"
	case ".mobi":
		return "application/x-mobipocket-ebook"
	case ".azw3":
		return "application/vnd.amazon.ebook"
	case ".pdf":
		return "application/pdf"
	default:
		return "application/octet-stream"
	}
}

func modTime(path string) time.Time {
	if st, err := os.Stat(path); err == nil {
		return st.ModTime()
	}
	return time.Time{}
}

func staticHandler(dir string) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// short cache so updates deploy instantly; HTML never cached
		w.Header().Set("Cache-Control", "public, max-age=60, must-revalidate")
		fs.ServeHTTP(w, r)
	})
}

func noDirListing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *apiServer) handleHighlights(w http.ResponseWriter, r *http.Request) {
	bookID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	user := userFromCtx(r)
	if user == nil {
		writeErr(w, http.StatusUnauthorized, "login required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		hls, err := a.store.listHighlightsForUser(user.ID, bookID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if hls == nil {
			hls = []Highlight{}
		}
		writeJSON(w, http.StatusOK, hls)
	case http.MethodPost:
		var body struct {
			CFI   string  `json:"cfi"`
			Text  string  `json:"text"`
			Color string  `json:"color"`
			Note  *string `json:"note"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.CFI == "" || body.Text == "" {
			writeErr(w, http.StatusBadRequest, "cfi and text required")
			return
		}
		if body.Color == "" {
			body.Color = "yellow"
		}
		id, err := a.store.addHighlightForUser(user.ID, bookID, body.CFI, body.Text, body.Color, body.Note)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		hls, _ := a.store.listHighlightsForUser(user.ID, bookID)
		for _, hl := range hls {
			if hl.ID == id {
				writeJSON(w, http.StatusCreated, hl)
				return
			}
		}
		writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (a *apiServer) handleHighlight(w http.ResponseWriter, r *http.Request) {
	hlID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	switch r.Method {
	case http.MethodPut:
		var body struct {
			Color string  `json:"color"`
			Note  *string `json:"note"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid json")
			return
		}
		if err := a.store.updateHighlight(hlID, body.Color, body.Note); err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if err := a.store.deleteHighlight(hlID); err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (a *apiServer) handleBookmarks(w http.ResponseWriter, r *http.Request) {
	bookID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	user := userFromCtx(r)
	if user == nil {
		writeErr(w, http.StatusUnauthorized, "login required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		bms, err := a.store.listBookmarksForUser(user.ID, bookID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if bms == nil {
			bms = []Bookmark{}
		}
		writeJSON(w, http.StatusOK, bms)
	case http.MethodPost:
		var body struct {
			CFI     string  `json:"cfi"`
			Label   *string `json:"label"`
			Percent float64 `json:"percent"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.CFI == "" {
			writeErr(w, http.StatusBadRequest, "cfi required")
			return
		}
		id, err := a.store.addBookmarkForUser(user.ID, bookID, body.CFI, body.Label, body.Percent)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (a *apiServer) handleBookmarkDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	bmID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	a.store.deleteBookmark(bmID)
	w.WriteHeader(http.StatusNoContent)
}

// handleSeriesGrouping returns the library grouped into series (M4a).
// GET /api/series-group — derives groups by series keyword patterns in titles
// (e.g. "Dune 01-06", "Skulduggery 3: ..."), plus explicit series metadata from
// the books table when present.
func (a *apiServer) handleSeriesGrouping(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	user := userFromCtx(r)
	if user == nil {
		writeErr(w, http.StatusUnauthorized, "login required")
		return
	}
	books, err := a.store.shelfBooks(user.ID, "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type seriesBook struct {
		ID     int64   `json:"id"`
		Title  string  `json:"title"`
		Author string  `json:"author"`
		Status string  `json:"status"`
		Series string  `json:"series"`
		Order  float64 `json:"order"`
	}
	type seriesGroup struct {
		Name   string       `json:"name"`
		Author string       `json:"author"`
		Books  []seriesBook `json:"books"`
	}
	groups := map[string]*seriesGroup{}
	idsSeen := map[int64]bool{}
	for _, b := range books {
		name, order, isSeries := seriesFromTitle(b.Title)
		if !isSeries {
			continue
		}
		key := seriesKey(name, b.Author)
		g, ok := groups[key]
		if !ok {
			displayAuthor := b.Author
			if displayAuthor == "" {
				displayAuthor = "Unknown"
			}
			g = &seriesGroup{Name: name, Author: displayAuthor}
			groups[key] = g
		}
		g.Books = append(g.Books, seriesBook{ID: b.ID, Title: b.Title, Author: b.Author, Status: b.Status, Series: name, Order: order})
		idsSeen[b.ID] = true
	}
	out := make([]seriesGroup, 0, len(groups))
	for _, g := range groups {
		sort.Slice(g.Books, func(i, j int) bool { return g.Books[i].Order < g.Books[j].Order })
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}

// seriesFromTitle detects a series + order from common title patterns.
// Returns (seriesName, order, isSeries).
func seriesFromTitle(title string) (string, float64, bool) {
	low := strings.ToLower(title)
	// numbered forms: "Dune 3", "Skulduggery Pleasant Book 12", "Summoner #4"
	pats := []struct {
		re *regexp.Regexp
	}{
		{regexp.MustCompile(`^(.*\S)\s+book\s+(\d+)$`)},
		{regexp.MustCompile(`^(.*\S)\s*#(\d+)$`)},
		{regexp.MustCompile(`^(.*\D)(\d+)$`)},
	}
	for _, p := range pats {
		if m := p.re.FindStringSubmatch(low); m != nil {
			name := strings.TrimSpace(stripSeriesNoise(strings.Trim(m[1], " -:—_")))
			if len(name) < 3 {
				continue
			}
			n := 0
			fmt.Sscanf(m[2], "%d", &n)
			if n <= 0 || n > 100 {
				continue // nonsense ordering numbers (years, ISBNs)
			}
			return name, float64(n), true
		}
	}
	return "", 0, false
}

// stripSeriesNoise trims common suffix noise in series titles.
func stripSeriesNoise(s string) string {
	for _, suffix := range []string{"series", "trilogy", "collection", "omnibus"} {
		if strings.HasSuffix(strings.ToLower(s), " "+suffix) {
			return strings.TrimSpace(s[:len(s)-len(suffix)])
		}
	}
	return s
}

func seriesKey(name, author string) string {
	return strings.ToLower(name) + "|" + strings.ToLower(normalizeAuthor(author))
}

// normalizeAuthor merges author spelling variants ("Jacobs, Logan" ->
// "logan jacobs"; empty -> "unknown"; "," handling; initials kept).
func normalizeAuthor(a string) string {
	a = strings.TrimSpace(a)
	if a == "" {
		return "unknown"
	}
	// "Last, First" -> "First Last"
	if parts := strings.SplitN(a, ",", 2); len(parts) == 2 {
		first := strings.TrimSpace(parts[1])
		last := strings.TrimSpace(parts[0])
		if first != "" && last != "" {
			a = first + " " + last
		}
	}
	return strings.ToLower(strings.Join(strings.Fields(a), " "))
}

func main() {
	listen := os.Getenv("DOGEAR_LISTEN")
	if listen == "" {
		listen = ":8090"
	}
	dbPath := os.Getenv("DOGEAR_DB")
	if dbPath == "" {
		dbPath = "/data/dogear.db"
	}
	libraryPath := os.Getenv("DOGEAR_LIBRARY")
	if libraryPath == "" {
		libraryPath = "/library"
	}
	_ = libraryPath

	smURL := os.Getenv("SHELFMARK_URL")
	if smURL == "" {
		smURL = "http://shelfmark:8084"
	}
	sm, err := NewShelfmark(smURL)
	if err != nil {
		log.Fatalf("shelfmark client: %v", err)
	}
	if u := os.Getenv("SHELFMARK_USER"); u != "" {
		sm.user = u
		sm.pass = os.Getenv("SHELFMARK_PASSWORD")
	}

	store, err := openStore(dbPath)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer store.close()

	if err := store.ensureUsersSchema(); err != nil {
		log.Fatalf("users schema: %v", err)
	}
	if err := store.migrateMeta(); err != nil {
		log.Fatalf("meta schema: %v", err)
	}

	api := &apiServer{store: store, sm: sm}

	mux := http.NewServeMux()
	// public routes
	mux.HandleFunc("GET /api/health", api.handleHealth)
	mux.HandleFunc("POST /api/auth/login", api.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", api.handleLogout)
	mux.HandleFunc("GET /api/auth/me", api.handleMe)
	// admin routes
	mux.HandleFunc("GET /api/admin/users", api.requireAdmin(api.handleUsers))
	mux.HandleFunc("POST /api/admin/users", api.requireAdmin(api.handleUsers))
	mux.HandleFunc("PUT /api/admin/users/{id}", api.requireAdmin(api.handleUser))
	mux.HandleFunc("DELETE /api/admin/users/{id}", api.requireAdmin(api.handleUser))
	mux.HandleFunc("GET /api/admin/status", api.requireAdmin(api.handleAdminStatus))
	mux.HandleFunc("GET /api/admin/settings", api.requireAdmin(api.handleSettings))
	mux.HandleFunc("PUT /api/admin/settings", api.requireAdmin(api.handleSettings))
	mux.HandleFunc("POST /api/admin/test/{service}", api.requireAdmin(api.handleTestIntegration))

	// per-user data (must be behind auth)
	mux.HandleFunc("GET /api/books", api.requireUser(api.handleBooks))
	mux.HandleFunc("POST /api/books", api.requireUser(api.handleBooks))
	mux.HandleFunc("GET /api/books/{id}", api.requireUser(api.handleBook))
	mux.HandleFunc("DELETE /api/books/{id}", api.requireUser(api.handleBook))
	mux.HandleFunc("GET /api/books/{id}/releases", api.requireUser(api.handleBookReleases))
	mux.HandleFunc("POST /api/books/{id}/status", api.requireUser(api.handleBookStatus))
	mux.HandleFunc("POST /api/books/{id}/grab", api.requireUser(api.handleGrab))
	mux.HandleFunc("POST /api/books/{id}/grab-release", api.requireUser(api.handleGrabRelease))
	mux.HandleFunc("POST /api/books/{id}/grab-libgen", api.requireUser(api.handleLibgenGrab))
	mux.HandleFunc("POST /api/books/{id}/add-to-shelf", api.requireUser(api.handleShelfAdd))
	mux.HandleFunc("GET /api/books/{id}/progress", api.requireUser(api.handleProgress))
	mux.HandleFunc("PUT /api/books/{id}/progress", api.requireUser(api.handleProgress))
	mux.HandleFunc("GET /api/books/{id}/highlights", api.requireUser(api.handleHighlights))
	mux.HandleFunc("POST /api/books/{id}/highlights", api.requireUser(api.handleHighlights))
	mux.HandleFunc("PUT /api/highlights/{id}", api.requireUser(api.handleHighlight))
	mux.HandleFunc("DELETE /api/highlights/{id}", api.requireUser(api.handleHighlight))
	mux.HandleFunc("GET /api/books/{id}/bookmarks", api.requireUser(api.handleBookmarks))
	mux.HandleFunc("POST /api/books/{id}/bookmarks", api.requireUser(api.handleBookmarks))
	mux.HandleFunc("DELETE /api/bookmarks/{id}", api.requireUser(api.handleBookmarkDelete))
	mux.HandleFunc("GET /api/search", api.requireUser(api.handleSearch))
	mux.HandleFunc("GET /api/covers/{id}", api.requireUser(api.handleCover))
	mux.HandleFunc("GET /api/shelfmark-cover", api.requireUser(api.handleShelfmarkCover))
	mux.HandleFunc("GET /api/books/{id}/content", api.requireUser(api.handleContent))
	mux.HandleFunc("POST /api/aa-grab", api.requireUser(api.handleAAGrab))
	mux.HandleFunc("GET /api/series-group", api.requireUser(api.handleSeriesGrouping))
	mux.HandleFunc("GET /api/home", api.requireUser(api.handleHome))
	mux.HandleFunc("POST /api/books/{id}/reading", api.requireUser(api.handleReadingHeartbeat))
	mux.HandleFunc("GET /api/stats", api.requireUser(api.handleStats))
	mux.HandleFunc("POST /api/books/{id}/enrich", api.requireUser(api.handleEnrich))
	mux.HandleFunc("POST /api/admin/enrich", api.requireAdmin(api.handleEnrichBatch))
	mux.HandleFunc("GET /api/admin/enrich", api.requireAdmin(api.handleEnrichStatus))

	// static PWA assets
	webDir := os.Getenv("DOGEAR_WEB")
	if webDir == "" {
		webDir = "web"
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler(webDir+"/static")))
	mux.Handle("GET /vendor/", http.StripPrefix("/vendor/", staticHandler(webDir+"/vendor")))
	mux.HandleFunc("GET /reader", func(w http.ResponseWriter, r *http.Request) {
		serveVersionedHTML(w, webDir+"/reader.html")
	})
	mux.HandleFunc("GET /reader.html", func(w http.ResponseWriter, r *http.Request) {
		serveVersionedHTML(w, webDir+"/reader.html")
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		serveVersionedHTML(w, webDir+"/index.html")
	})
	mux.HandleFunc("GET /manifest.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/manifest+json")
		http.ServeFile(w, r, webDir+"/manifest.json")
	})
	// service worker must be served from root scope path
	mux.HandleFunc("GET /sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Service-Worker-Allowed", "/")
		http.ServeFile(w, r, webDir+"/static/sw.js")
	})
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		serveVersionedHTML(w, webDir+"/login.html")
	})
	mux.HandleFunc("GET /login.html", func(w http.ResponseWriter, r *http.Request) {
		serveVersionedHTML(w, webDir+"/login.html")
	})
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		serveVersionedHTML(w, webDir+"/admin.html")
	})

	addr := os.Getenv("DOGEAR_ADDR")
	if addr == "" {
		addr = ":8090"
	}
	_ = listen

	// ingest watcher: poll Shelfmark's output dir (plus extra sources) and import files
	ingestDir := os.Getenv("DOGEAR_INGEST")
	if ingestDir == "" {
		ingestDir = "/ingest"
	}
	ingests := []string{ingestDir}
	if extra := os.Getenv("DOGEAR_EXTRA_INGESTS"); extra != "" {
		for _, d := range strings.Split(extra, ":") {
			if d != "" {
				ingests = append(ingests, d)
			}
		}
	}
	copyMode := os.Getenv("DOGEAR_COPY_MODE") == "1" || os.Getenv("DOGEAR_COPY_MODE") == "true"
	importer := NewImporter(ingests, libraryPath, store, log.New(os.Stderr, "dogear/import ", log.LstdFlags))
	importer.CopyMode = copyMode
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go importer.Run(ctx, 20*time.Second)

	// AA direct grab (donator key; optional — feature disabled when unset)
	importerAPI := api
	importerAPI.importer = importer
	api.aa = NewAAGrabber(os.Getenv("AA_BASE_URL"), os.Getenv("AA_DONATOR_KEY"), log.New(os.Stderr, "dogear/aa ", log.LstdFlags))
	api.aa.store = store // so settings can override env later
	api.prowlarr = NewProwlarr(store, log.New(os.Stderr, "dogear/prowlarr ", log.LstdFlags))
	api.libgen = NewLibgen(nil, log.New(os.Stderr, "dogear/libgen ", log.LstdFlags))

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("dogear listening on %s (db=%s shelfmark=%s library=%s ingests=%v copy=%v)", addr, dbPath, smURL, libraryPath, ingests, copyMode)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
