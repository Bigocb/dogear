package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Importer organizes completed downloads from ingest dirs into the library.
type Importer struct {
	Ingests  []string // watch dirs (shelfmark output, /books, ...)
	Library  string   // library root (/library)
	CopyMode bool     // true = copy files instead of moving (keep originals)
	minAge   time.Duration
	store    *Store
	log      *log.Logger
}

func NewImporter(ingests []string, library string, store *Store, logf *log.Logger) *Importer {
	return &Importer{Ingests: ingests, Library: library, CopyMode: false, minAge: 30 * time.Second, store: store, log: logf}
}

// Run polls each ingest dir every interval until ctx is done.
// (Polling rather than inotify keeps this portable behind bind mounts.)
func (im *Importer) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	im.log.Printf("ingest watcher on %v -> %s (every %s, copy=%v)", im.Ingests, im.Library, interval, im.CopyMode)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, dir := range im.Ingests {
				if err := im.ScanOnce(ctx, dir); err != nil {
					im.log.Printf("scan %s: %v", dir, err)
				}
			}
		}
	}
}

var bookExts = map[string]bool{".epub": true, ".mobi": true, ".azw3": true, ".pdf": true, ".fb2": true, ".djvu": true}

type candidate struct {
	path    string
	name    string // filename without ext
	ext     string
	modTime time.Time
	size    int64
}

func (im *Importer) ScanOnce(ctx context.Context, ingestDir string) error {
	// recursive walk so nested acquisitions (author folders, series packs) import too
	var cands []candidate
	err := filepath.WalkDir(ingestDir, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return nil // unreadable entry: skip
		}
		if d.IsDir() {
			// skip hidden dirs
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if !bookExts[ext] || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		// skip very fresh files (still downloading); tests pass modTime
		if time.Since(info.ModTime()) > im.minAge && info.Size() > 0 {
			stem := strings.TrimSuffix(d.Name(), filepath.Ext(d.Name()))
			cands = append(cands, candidate{path: p, name: stem, ext: ext, modTime: info.ModTime(), size: info.Size()})
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk ingest %s: %w", ingestDir, err)
	}
	// oldest first
	sort.Slice(cands, func(i, j int) bool { return cands[i].modTime.Before(cands[j].modTime) })
	for _, c := range cands {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		if err := im.importFile(c, cands); err != nil {
			im.log.Printf("import %s: %v", c.path, err)
		}
	}
	return nil
}

// importFile matches a file to a book, moves+organizes it, records the file row.
func (im *Importer) importFile(c candidate, all []candidate) error {
	// dedupe: skip if this exact source path or same-size file already recorded
	if im.store.fileExistsBySize(c.size) {
		return nil // already imported (size match = same book copy)
	}
	book := im.matchBook(c)
	title, author := c.name, ""
	if book != nil {
		title, author = book.Title, book.Author
	} else {
		// prefer the epub's own embedded metadata
		if c.ext == ".epub" {
			if em, err := epubMeta(c.path); err == nil && em.Title != "" {
				title, author = em.Title, em.Author
			}
		}
		if title == c.name {
			// fall back to "Author - Title" dash convention; if the LAST
			// segment looks like a person's name (First Last), assume
			// "Title - Author" (common Calibre-like layout)
			if parts := strings.SplitN(c.name, " - ", 2); len(parts) == 2 && parts[0] != "" && parts[1] != "" {
				if looksLikePersonName(parts[len(parts)-1]) && !looksLikePersonName(parts[0]) {
					title, author = parts[0], parts[1]
				} else {
					author, title = parts[0], parts[1]
				}
			}
		}
	}
	if strings.TrimSpace(title) == "" {
		return fmt.Errorf("no title for %s", c.name)
	}

	destDir := filepath.Join(im.Library, sanitize(author), sanitize(title))
	if err := os.MkdirAll(destDir, 0o775); err != nil {
		return err
	}
	dest := filepath.Join(destDir, sanitize(title)+c.ext)
	// keep a unique suffix if dest already exists (same book, different copy)
	if _, err := os.Stat(dest); err == nil {
		base := strings.TrimSuffix(sanitize(title)+c.ext, c.ext)
		ext := c.ext
		for n := 2; ; n++ {
			dest = filepath.Join(destDir, fmt.Sprintf("%s (%d)%s", base, n, ext))
			if _, err := os.Stat(dest); err != nil {
				break
			}
		}
	}
	if err := im.moveOrCopy(c.path, dest); err != nil {
		return fmt.Errorf("move: %w", err)
	}

	// cover: epub internal or sidecar jpg/png with same stem
	coverDest := ""
	if c.ext == ".epub" {
		if coverData, ctype, err := epubCover(dest); err == nil && coverData != nil {
			ext := ".jpg"
			if strings.Contains(ctype, "png") {
				ext = ".png"
			}
			coverDest = filepath.Join(destDir, "cover"+ext)
			if err := os.WriteFile(coverDest, coverData, 0o644); err != nil {
				coverDest = ""
			}
		}
	}
	if coverDest == "" {
		for _, other := range all {
			base := strings.TrimSuffix(filepath.Base(other.path), filepath.Ext(other.path))
			if base == c.name {
				switch strings.ToLower(filepath.Ext(other.path)) {
				case ".jpg", ".jpeg", ".png":
					cv := filepath.Join(destDir, "cover"+strings.ToLower(filepath.Ext(other.path)))
					if im.moveOrCopy(other.path, cv) == nil {
						coverDest = cv
					}
				}
			}
		}
	}
	// sidecar .cue: bring along if sibling of source
	if _, err := os.Stat(filepath.Join(filepath.Dir(c.path), c.name+".cue")); err == nil {
		im.moveOrCopy(filepath.Join(filepath.Dir(c.path), c.name+".cue"), filepath.Join(destDir, sanitize(title)+".cue"))
	}

	if book == nil {
		book = &Book{Title: title, Author: author}
	}
	id, err := im.storeRecord(book, dest, strings.TrimPrefix(c.ext, "."), c.size, coverDest)
	if err != nil {
		return err
	}
	if err := im.store.setStatus(id, "imported"); err != nil {
		return err
	}
	im.log.Printf("imported: %q / %q -> %s (book %d)", author, title, dest, id)
	return nil
}

func (im *Importer) moveOrCopy(src, dst string) error {
	if im.CopyMode {
		// copy: originals stay in the ingest dir (bulk-import mode)
		return copyFile(src, dst)
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// matchBook finds a grabbed book whose title matches the filename
// (fuzzy containment, normalized).
func (im *Importer) matchBook(c candidate) *Book {
	cands, err := im.store.booksInStatuses("grabbed")
	if err != nil {
		return nil
	}
	cname := normalizeTitle(c.name)
	for _, b := range cands {
		bt := normalizeTitle(b.Title)
		if bt == "" {
			continue
		}
		if strings.Contains(cname, bt) || strings.Contains(bt, cname) {
			return b
		}
		if b.Author != "" {
			ba := normalizeTitle(b.Author)
			if ba != "" && strings.Contains(cname, ba) {
				return b
			}
		}
	}
	return nil
}

func (im *Importer) storeRecord(book *Book, path, format string, size int64, coverPath string) (int64, error) {
	now := time.Now().Unix()
	var id int64
	// If the caller knows the book (a grab targeting an existing wanted entry),
	// attach the file to THAT book. Never create a duplicate from a title
	// mismatch -- that orphaned the file from its shelf entry.
	if book.ID != 0 {
		id = book.ID
		if _, err := im.store.db.Exec(`UPDATE books SET status='imported',
			cover_file=coalesce(nullif(?,''), cover_file), updated_at=? WHERE id=?`,
			coverPath, now, id); err != nil {
			return 0, err
		}
	} else {
		err := im.store.db.QueryRow(`SELECT id FROM books WHERE lower(title)=lower(?) AND lower(coalesce(author,''))=lower(?)`,
			book.Title, book.Author).Scan(&id)
		if err != nil {
			res, err := im.store.db.Exec(`INSERT INTO books(title,author,isbn,status,cover_file,added_at,updated_at,owner_id) VALUES(?,?,?,?,?,?,?,?)`,
				book.Title, book.Author, book.ISBN, "imported", nonEmpty(coverPath), now, now, book.OwnerID)
			if err != nil {
				return 0, err
			}
			id, _ = res.LastInsertId()
		} else {
			if _, err := im.store.db.Exec(`UPDATE books SET status='imported', cover_file=coalesce(nullif(?,''), cover_file), updated_at=? WHERE id=?`,
				coverPath, now, id); err != nil {
				return 0, err
			}
		}
	}
	if _, err := im.store.db.Exec(`INSERT INTO files(book_id,path,format,size,imported_at) VALUES(?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET format=excluded.format, size=excluded.size, imported_at=excluded.imported_at`,
		id, path, format, size, now); err != nil {
		return 0, err
	}
	return id, nil
}

// ---- helpers ----

func sanitize(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "Unknown"
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		if r < 32 {
			return -1
		}
		return r
	}, s)
}

var wsRe = regexp.MustCompile(`\s+`)
var trailingPunctRe = regexp.MustCompile(`[\s_\-()\[\]]+$`)

func normalizeTitle(s string) string {
	s = strings.ToLower(s)
	s = strings.TrimSuffix(s, filepath.Ext(s))
	s = wsRe.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	s = trailingPunctRe.ReplaceAllString(s, "")
	return s
}

// looksLikePersonName heuristically detects "Firstname Lastname" style
// author strings vs book titles. Titles rarely look like capitalized words.
func looksLikePersonName(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || strings.Contains(s, " - ") || strings.Contains(s, ":") {
		return false
	}
	words := strings.Fields(s)
	if len(words) < 2 || len(words) > 4 {
		return false
	}
	// reject obvious title markers
	low := strings.ToLower(s)
	titleMarkers := []string{"book", "part", "volume", "omnibus", "series", "novel", "chapter", "trilogy", "collection", "edition", "world", "the ", "a ", "of ", "quest", "dungeon", "king", "mage", "project", "guide", "introduction", "complete", "essential"}
	for _, m := range titleMarkers {
		if strings.Contains(low, m) {
			return false
		}
	}
	// each word capitalized?
	caps := 0
	for _, w := range words {
		if len(w) > 0 && strings.ToUpper(w[:1]) == w[:1] {
			caps++
		}
	}
	return caps == len(words)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
