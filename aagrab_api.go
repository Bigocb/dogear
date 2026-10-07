package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// handleAAGrab grabs a book directly from Anna's Archive via donator key.
// POST /api/aa-grab {link: "<md5 or AA URL>", title, author}
// Downloads synchronously and saves STRAIGHT INTO THE LIBRARY (not the shared
// ingest dir — CWA watches that too and steals the files). Then records the
// book + file rows itself so no watcher roundtrip is needed.
func (a *apiServer) handleAAGrab(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if a.aa == nil || a.aa.key() == "" {
		writeErr(w, http.StatusPreconditionFailed, "AA_DONATOR_KEY not configured")
		return
	}
	var body struct {
		Link   string `json:"link"`
		Title  string `json:"title"`
		Author string `json:"author"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Link == "" {
		writeErr(w, http.StatusBadRequest, "link (md5 or AA URL) required")
		return
	}
	md5, ok := ParseAALink(body.Link)
	if !ok {
		writeErr(w, http.StatusBadRequest, "could not parse an md5/AA link from input")
		return
	}

	// find existing book (dedupe by title+author) or create
	title := strings.TrimSpace(body.Title)
	if title == "" {
		title = "AA " + md5[:8]
	}
	bookID := int64(0)
	existing, _ := a.store.listBooks("", "")
	for _, b := range existing {
		if strings.EqualFold(b.Title, title) && strings.EqualFold(b.Author, body.Author) {
			bookID = b.ID
			break
		}
	}
	if bookID == 0 {
		id, err := a.store.addBook(&Book{Title: title, Author: body.Author})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		bookID = id
	}
	_ = a.store.setStatus(bookID, "grabbed")
	_ = a.store.addGrab(bookID, "aa-fast:"+md5, "queued")

	dl, err := a.aa.Resolve(r.Context(), md5)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "AA resolve: "+err.Error())
		return
	}

	// download to a staging file FIRST (need content/size + maybe cover from
	// the epub), then move into the library with author/title organization
	stagingDir := os.Getenv("DOGEAR_AA_STAGING")
	if stagingDir == "" {
		stagingDir = "/data/staging"
	}
	base := title
	if body.Author != "" {
		base = body.Author + " - " + title
	}
	staged, err := a.aa.Download(r.Context(), dl, stagingDir, base)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "AA download: "+err.Error())
		return
	}

	// organize into library: <Author>/<Title>/
	author := body.Author
	if author == "" {
		author = "AA Grabs"
	}
	destDir := filepath.Join(a.importer.Library, sanitize(author), sanitize(title))
	if err := os.MkdirAll(destDir, 0o775); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ext := strings.ToLower(filepath.Ext(staged))
	dest := filepath.Join(destDir, sanitize(title)+ext)
	if err := os.Rename(staged, dest); err != nil {
		if err := copyFile(staged, dest); err != nil {
			writeErr(w, http.StatusInternalServerError, "library move: "+err.Error())
			return
		}
		os.Remove(staged)
	}
	// extract cover if epub
	if ext == ".epub" {
		if coverData, ctype, cerr := epubCover(dest); cerr == nil && coverData != nil {
			coverExt := ".jpg"
			if strings.Contains(ctype, "png") {
				coverExt = ".png"
			}
			_ = os.WriteFile(filepath.Join(destDir, "cover"+coverExt), coverData, 0o644)
		}
	}
	_, _ = a.importer.storeRecord(&Book{ID: bookID, Title: title, Author: author}, dest, strings.TrimPrefix(ext, "."), fileSize(dest), "")
	// the grabber's shelf gets the book (Netflix model)
	if u := userFromCtx(r); u != nil {
		_ = a.store.shelfAdd(u.ID, bookID, "imported")
	}
	_ = a.store.setStatus(bookID, "imported")
	b, _ := a.store.getBook(bookID)
	writeJSON(w, http.StatusOK, map[string]any{"book": b, "file": dest, "md5": md5})
}
