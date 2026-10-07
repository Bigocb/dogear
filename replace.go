package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// handleReplace grabs a different release for a book and swaps out the file.
// POST /api/books/{id}/replace  {release: Release}
//
// Direct sources (libgen/AA) are swapped cleanly: download the new file, then
// delete the file(s) it replaces. Torrent sources can't be swapped safely while
// seeding, so the new file is added and the reader prefers the newest copy;
// the old torrent's data is left to seed.
func (a *apiServer) handleReplace(w http.ResponseWriter, r *http.Request) {
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
	book, err := a.store.getBook(id)
	if err != nil || book == nil {
		writeErr(w, http.StatusNotFound, "book not found")
		return
	}
	var body struct {
		Release *Release `json:"release"`
		MD5     string   `json:"md5"`
		Title   string   `json:"title"`
		Author  string   `json:"author"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	// snapshot the files we're about to replace
	oldPaths, _ := a.store.bookFilePaths(id)

	title := body.Title
	if title == "" {
		title = book.Title
	}
	author := body.Author
	if author == "" {
		author = book.Author
	}

	// Direct download paths (md5 given, or a libgen release) -> clean swap.
	isDirect := body.MD5 != "" || (body.Release != nil && body.Release.Source == "libgen")
	if isDirect {
		md5 := strings.ToLower(body.MD5)
		if md5 == "" && body.Release != nil {
			md5 = strings.ToLower(body.Release.SourceID)
		}
		if !md5Re.MatchString(md5) {
			writeErr(w, http.StatusBadRequest, "invalid md5")
			return
		}
		dest, via, err := a.libgenDownloadAndImport(r.Context(), md5, title, author)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		ext := strings.TrimPrefix(strings.ToLower(pathExt(dest)), ".")
		// record new file first, then remove the old ones
		if _, err := a.importer.storeRecord(&Book{ID: id, Title: title, Author: author}, dest, ext, fileSize(dest), ""); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		removed := a.deleteOldFiles(id, dest, oldPaths)
		_ = a.store.setStatus(id, "imported")
		if u := userFromCtx(r); u != nil {
			_ = a.store.shelfAdd(u.ID, id, "imported")
		}
		_ = a.store.addGrab(id, "replace:"+via+":"+md5, "done")
		b, _ := a.store.getBook(id)
		writeJSON(w, http.StatusOK, map[string]any{"book": b, "via": via, "replaced_files": removed})
		return
	}

	// Torrent path: queue the new release, keep old files seeding.
	if body.Release == nil {
		writeErr(w, http.StatusBadRequest, "release or md5 required")
		return
	}
	rel := *body.Release
	if rel.Source == "prowlarr-direct" {
		idxID := 0
		if rel.Extra != nil {
			if v, ok := rel.Extra["indexerId"].(float64); ok {
				idxID = int(v)
			}
		}
		if err := a.prowlarr.Grab(r.Context(), ProwlarrResult{GUID: rel.SourceID, IndexerID: idxID, Title: rel.Title}); err != nil {
			writeErr(w, http.StatusBadGateway, "prowlarr grab: "+err.Error())
			return
		}
	} else {
		if err := a.sm.Grab(r.Context(), &rel); err != nil {
			writeErr(w, http.StatusBadGateway, "shelfmark grab: "+err.Error())
			return
		}
	}
	_ = a.store.addGrab(id, "replace:"+rel.Source+":"+rel.SourceID, "queued")
	b, _ := a.store.getBook(id)
	writeJSON(w, http.StatusOK, map[string]any{
		"book": b, "via": rel.Source,
		"note": "New torrent queued. The previous file stays until the replacement lands (it may still be seeding).",
	})
}

// deleteOldFiles removes a book's earlier files after a replacement, and cleans
// up now-empty directories. The new dest is never deleted.
func (a *apiServer) deleteOldFiles(bookID int64, keep string, oldPaths []string) int {
	removed := 0
	dirs := map[string]bool{}
	for _, p := range oldPaths {
		if p == keep {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			// already gone on disk; just drop the row
			_, _ = a.store.db.Exec(`DELETE FROM files WHERE book_id=? AND path=?`, bookID, p)
			continue
		}
		if os.Remove(p) == nil {
			removed++
			dirs[filepath.Dir(p)] = true
		}
		_, _ = a.store.db.Exec(`DELETE FROM files WHERE book_id=? AND path=?`, bookID, p)
	}
	for d := range dirs {
		if a.importer != nil && d != a.importer.Library {
			_ = os.Remove(d) // only succeeds if empty
		}
	}
	return removed
}

var _ = context.Background
