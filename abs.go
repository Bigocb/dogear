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
	"strings"
	"time"
)

// ABS is a lightweight client for Audiobookshelf. It only reads the user's
// library items and listening progress so Dogear can link to existing
// audiobooks and surface "Continue listening" when the user is further along
// in audio than in text.
type ABS struct {
	store  *Store
	client *http.Client
	log    *log.Logger
}

func NewABS(store *Store, logf *log.Logger) *ABS {
	return &ABS{
		store:  store,
		client: &http.Client{Timeout: 120 * time.Second},
		log:    logf,
	}
}

func (a *ABS) base() string { return strings.TrimRight(a.store.absURL(), "/") }

func (a *ABS) Configured() bool {
	return a.store.absEnabled() && a.base() != "" && a.store.absKey() != ""
}

func (a *ABS) getJSON(ctx context.Context, path string, out any) error {
	u, err := url.Parse(a.base() + path)
	if err != nil {
		return err
	}
	// ABS accepts the token either in the Authorization header or as a query
	// param. The header form is preferred for logs.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.store.absKey())
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("abs %d: %s", resp.StatusCode, string(body[:min(160, len(body))]))
	}
	return json.Unmarshal(body, out)
}

// ABSLibrary is the shape of /api/libraries entries.
type ABSLibrary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Icon string `json:"icon"`
}

// Libraries returns the ABS libraries the configured token can see.
func (a *ABS) Libraries(ctx context.Context) ([]ABSLibrary, error) {
	if !a.Configured() {
		return nil, fmt.Errorf("audiobookshelf not configured")
	}
	var out struct {
		Libraries []ABSLibrary `json:"libraries"`
	}
	if err := a.getJSON(ctx, "/api/libraries", &out); err != nil {
		return nil, err
	}
	return out.Libraries, nil
}

// ABSMediaMetadata is the nested metadata ABS returns for an audiobook.
type ABSMediaMetadata struct {
	Title         string `json:"title"`
	Author        string `json:"author"`
	Authors       []struct {
		Name string `json:"name"`
	} `json:"authors"`
	Narrator      string `json:"narrator"`
	Narrators     []string `json:"narrators"`
	Series        []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Sequence string `json:"sequence"`
	} `json:"series"`
	ISBN          string `json:"isbn"`
	ASIN          string `json:"asin"`
	PublishedYear string `json:"publishedYear"`
	Description   string `json:"description"`
}

type ABSMedia struct {
	Metadata       ABSMediaMetadata `json:"metadata"`
	Duration       float64          `json:"duration"`
	EbookFormat    *string          `json:"ebookFormat"`
}

type ABSMediaProgress struct {
	CurrentTime    float64 `json:"currentTime"`
	IsFinished     bool    `json:"isFinished"`
	LastUpdate     int64   `json:"lastUpdate"`
}

type ABSLibraryItem struct {
	ID            string            `json:"id"`
	LibraryID     string            `json:"libraryId"`
	Media         ABSMedia          `json:"media"`
	MediaProgress *ABSMediaProgress `json:"mediaProgress"`
}

// Items returns all audiobook library items for a given library, including
// per-user progress when the token belongs to a user.
func (a *ABS) Items(ctx context.Context, libraryID string) ([]ABSLibraryItem, error) {
	if !a.Configured() {
		return nil, fmt.Errorf("audiobookshelf not configured")
	}
	var out struct {
		Results []ABSLibraryItem `json:"results"`
	}
	path := "/api/libraries/" + url.PathEscape(libraryID) + "/items?limit=100000&sort=addedAt&include=progress"
	if err := a.getJSON(ctx, path, &out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// Sync pulls ABS items for the configured library, stores them, and attempts
// to match each audiobook to a Dogear book. Returns counts.
func (a *ABS) Sync(ctx context.Context) (synced, matched int, err error) {
	if !a.Configured() {
		return 0, 0, fmt.Errorf("audiobookshelf not configured")
	}
	libID := a.store.absLibrary()
	if libID == "" {
		// If no library selected, try to find a library named "Audiobooks" or
		// fall back to the first one.
		libs, e := a.Libraries(ctx)
		if e != nil {
			return 0, 0, e
		}
		for _, l := range libs {
			if strings.EqualFold(l.Name, "Audiobooks") {
				libID = l.ID
				break
			}
		}
		if libID == "" && len(libs) > 0 {
			libID = libs[0].ID
		}
		if libID == "" {
			return 0, 0, fmt.Errorf("no audiobookshelf libraries found")
		}
		_ = a.store.setConfig(cfgABSLibrary, libID)
	}

	items, err := a.Items(ctx, libID)
	if err != nil {
		return 0, 0, err
	}
	now := time.Now().Unix()
	for _, it := range items {
		m := it.Media.Metadata
		author := m.Author
		if author == "" && len(m.Authors) > 0 {
			author = m.Authors[0].Name
		}
		seriesName := ""
		seriesSeq := ""
		if len(m.Series) > 0 {
			seriesName = m.Series[0].Name
			seriesSeq = m.Series[0].Sequence
		}
		progress := float64(0)
		current := float64(0)
		finished := false
		if it.MediaProgress != nil {
			current = it.MediaProgress.CurrentTime
			finished = it.MediaProgress.IsFinished
			if it.Media.Duration > 0 {
				progress = current / it.Media.Duration
			}
		}
		// Duration of 0 or ebook-only media means this isn't a useful audio
		// item; skip it so we don't clutter the mapping table.
		if it.Media.Duration <= 0 {
			continue
		}

		id, err := a.store.upsertABSItem(absItemRecord{
			libraryID:     libID,
			itemID:        it.ID,
			title:         m.Title,
			author:        author,
			seriesName:    seriesName,
			seriesPos:     seriesSeq,
			isbn:          m.ISBN,
			duration:      it.Media.Duration,
			currentTime:   current,
			progress:      progress,
			isFinished:    finished,
			lastSync:      now,
		})
		if err != nil {
			a.log.Printf("abs sync: upsert failed for %s: %v", m.Title, err)
			continue
		}
		synced++
		if _, err := a.store.matchABSItem(id, libID); err != nil {
			a.log.Printf("abs sync: match failed for %s: %v", m.Title, err)
		} else {
			matched++
		}
	}
	a.log.Printf("abs sync: %d items, %d matched", synced, matched)
	return synced, matched, nil
}

// Status verifies connectivity and returns the configured library name.
func (a *ABS) Status(ctx context.Context) (map[string]any, error) {
	if !a.Configured() {
		return nil, fmt.Errorf("audiobookshelf not configured")
	}
	libs, err := a.Libraries(ctx)
	if err != nil {
		return nil, err
	}
	var name string
	libID := a.store.absLibrary()
	for _, l := range libs {
		if l.ID == libID {
			name = l.Name
			break
		}
	}
	if name == "" && len(libs) > 0 {
		name = libs[0].Name
	}
	return map[string]any{
		"connected": true,
		"libraries": len(libs),
		"library":   name,
	}, nil
}

// absItemRecord is the storage shape for an ABS library item.
type absItemRecord struct {
	id            int64
	libraryID     string
	itemID        string
	bookID        *int64
	title         string
	author        string
	seriesName    string
	seriesPos     string
	isbn          string
	duration      float64
	currentTime   float64
	progress      float64
	isFinished    bool
	lastSync      int64
}

func (s *Store) migrateABS() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS abs_items (
  id            INTEGER PRIMARY KEY,
  library_id    TEXT NOT NULL,
  item_id       TEXT NOT NULL UNIQUE,
  book_id       INTEGER REFERENCES books(id) ON DELETE SET NULL,
  title         TEXT,
  author        TEXT,
  series_name   TEXT,
  series_pos    TEXT,
  isbn          TEXT,
  duration      REAL,
  current_time  REAL,
  progress      REAL,
  is_finished   INTEGER NOT NULL DEFAULT 0,
  last_sync     INTEGER,
  created_at    INTEGER NOT NULL DEFAULT (strftime('%s','now'))
);
CREATE INDEX IF NOT EXISTS idx_abs_book ON abs_items(book_id);
CREATE INDEX IF NOT EXISTS idx_abs_item ON abs_items(item_id);
`)
	return err
}

func (s *Store) upsertABSItem(r absItemRecord) (int64, error) {
	res, err := s.db.Exec(`
INSERT INTO abs_items(library_id,item_id,title,author,series_name,series_pos,isbn,duration,current_time,progress,is_finished,last_sync)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(item_id) DO UPDATE SET
  library_id=excluded.library_id,
  title=excluded.title,
  author=excluded.author,
  series_name=excluded.series_name,
  series_pos=excluded.series_pos,
  isbn=excluded.isbn,
  duration=excluded.duration,
  current_time=excluded.current_time,
  progress=excluded.progress,
  is_finished=excluded.is_finished,
  last_sync=excluded.last_sync`,
		r.libraryID, r.itemID, r.title, r.author, r.seriesName, r.seriesPos, r.isbn,
		r.duration, r.currentTime, r.progress, boolInt(r.isFinished), r.lastSync)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) matchABSItem(absItemID int64, libraryID string) (int64, error) {
	row := s.db.QueryRow(`SELECT title, author, isbn FROM abs_items WHERE id=?`, absItemID)
	var title, author, isbn string
	if err := row.Scan(&title, &author, &isbn); err != nil {
		return 0, err
	}
	var bookID *int64
	// Try ISBN first.
	if strings.TrimSpace(isbn) != "" {
		var bid int64
		err := s.db.QueryRow(`SELECT id FROM books WHERE isbn=? COLLATE NOCASE LIMIT 1`, isbn).Scan(&bid)
		if err == nil {
			bookID = &bid
		}
	}
	// Then exact title+author.
	if bookID == nil && title != "" {
		if b := s.findBookByTitleAuthor(title, author); b != nil {
			bookID = &b.ID
		}
	}
	// Then normalized title only.
	if bookID == nil && title != "" {
		var bid int64
		err := s.db.QueryRow(`SELECT id FROM books WHERE lower(title)=lower(?) LIMIT 1`, title).Scan(&bid)
		if err == nil {
			bookID = &bid
		}
	}
	if bookID != nil {
		_, err := s.db.Exec(`UPDATE abs_items SET book_id=? WHERE id=?`, *bookID, absItemID)
		return *bookID, err
	}
	_, err := s.db.Exec(`UPDATE abs_items SET book_id=NULL WHERE id=?`, absItemID)
	return 0, err
}

// absItemForBook returns the linked ABS item and progress for a book, if any.
func (s *Store) absItemForBook(bookID int64) (*absItemRecord, error) {
	var r absItemRecord
	var finished int
	err := s.db.QueryRow(`
SELECT id,library_id,item_id,book_id,title,author,series_name,series_pos,isbn,duration,current_time,progress,is_finished,last_sync
FROM abs_items WHERE book_id=? LIMIT 1`, bookID).Scan(
		&r.id, &r.libraryID, &r.itemID, &r.bookID, &r.title, &r.author, &r.seriesName, &r.seriesPos,
		&r.isbn, &r.duration, &r.currentTime, &r.progress, &finished, &r.lastSync)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.isFinished = finished != 0
	return &r, nil
}

// attachABSAudio enriches a slice of books with their linked ABS audiobooks.
func (a *apiServer) attachABSAudio(books []Book) []Book {
	ids := make([]int64, 0, len(books))
	for _, b := range books {
		if b.ID != 0 {
			ids = append(ids, b.ID)
		}
	}
	if len(ids) == 0 {
		return books
	}
	items, err := a.store.absItemsForBooks(ids)
	if err != nil {
		return books
	}
	for i := range books {
		if it, ok := items[books[i].ID]; ok && a.abs != nil {
			books[i].ABSAudio = &ABSAudio{
				ItemID:   it.itemID,
				Library:  it.libraryID,
				URL:      absItemURL(a.abs.base(), it.libraryID, it.itemID),
				Duration: it.duration,
				Current:  it.currentTime,
				Progress: it.progress,
				Finished: it.isFinished,
				LastSync: it.lastSync,
			}
		}
	}
	return books
}

func absItemURL(base, libraryID, itemID string) string {
	return strings.TrimRight(base, "/") + "/library/" + url.PathEscape(libraryID) + "/book/" + url.PathEscape(itemID)
}

// absItemsForBooks returns linked ABS items for many book IDs in one query.
func (s *Store) absItemsForBooks(bookIDs []int64) (map[int64]*absItemRecord, error) {
	if len(bookIDs) == 0 {
		return map[int64]*absItemRecord{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(bookIDs)), ",")
	args := make([]any, len(bookIDs))
	for i, id := range bookIDs {
		args[i] = id
	}
	rows, err := s.db.Query(`
SELECT id,library_id,item_id,book_id,title,author,series_name,series_pos,isbn,duration,current_time,progress,is_finished,last_sync
FROM abs_items WHERE book_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]*absItemRecord{}
	for rows.Next() {
		var r absItemRecord
		var finished int
		if err := rows.Scan(
			&r.id, &r.libraryID, &r.itemID, &r.bookID, &r.title, &r.author, &r.seriesName, &r.seriesPos,
			&r.isbn, &r.duration, &r.currentTime, &r.progress, &finished, &r.lastSync); err != nil {
			continue
		}
		r.isFinished = finished != 0
		if r.bookID != nil {
			out[*r.bookID] = &r
		}
	}
	return out, rows.Err()
}

