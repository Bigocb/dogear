package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
)

func pathExt(p string) string { return filepath.Ext(p) }

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
	title := strings.TrimSpace(body.Title)
	if title == "" {
		title = "AA " + md5[:8]
	}
	bookID, err := a.findOrCreateBook(title, body.Author, userIDFromCtx(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = a.store.setStatus(bookID, "grabbed")
	_ = a.store.addGrab(bookID, "aa-fast:"+md5, "queued")

	dest, err := a.aaDownloadAndImport(r.Context(), md5, title, body.Author)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	ext := strings.TrimPrefix(strings.ToLower(pathExt(dest)), ".")
	_, _ = a.importer.storeRecord(&Book{ID: bookID, Title: title, Author: body.Author}, dest, ext, fileSize(dest), "")
	if u := userFromCtx(r); u != nil {
		_ = a.store.shelfAdd(u.ID, bookID, "imported")
	}
	_ = a.store.setStatus(bookID, "imported")
	b, _ := a.store.getBook(bookID)
	writeJSON(w, http.StatusOK, map[string]any{"book": b, "file": dest, "md5": md5})
}

// handleLibgenGrab downloads a libgen search result through the AA donator key.
// POST /api/books/{id}/grab-libgen {md5, title?, author?}
// This is the captcha-free path: libgen provides search + md5; AA provides the
// keyed fast download.
func (a *apiServer) handleLibgenGrab(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if a.aa == nil || a.aa.key() == "" {
		writeErr(w, http.StatusPreconditionFailed, "AA donator key not configured")
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
	var body struct {
		MD5    string `json:"md5"`
		Title  string `json:"title"`
		Author string `json:"author"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.MD5 == "" {
		writeErr(w, http.StatusBadRequest, "md5 required")
		return
	}
	if !md5Re.MatchString(body.MD5) {
		writeErr(w, http.StatusBadRequest, "invalid md5")
		return
	}
	title := body.Title
	if title == "" {
		title = book.Title
	}
	author := body.Author
	if author == "" {
		author = book.Author
	}
	_ = a.store.setStatus(id, "grabbed")
	_ = a.store.addGrab(id, "libgen:"+body.MD5, "queued")

	dest, via, err := a.libgenDownloadAndImport(r.Context(), strings.ToLower(body.MD5), title, author)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	ext := strings.TrimPrefix(strings.ToLower(pathExt(dest)), ".")
	_, _ = a.importer.storeRecord(&Book{ID: id, Title: title, Author: author}, dest, ext, fileSize(dest), "")
	if u := userFromCtx(r); u != nil {
		_ = a.store.shelfAdd(u.ID, id, "imported")
	}
	_ = a.store.setStatus(id, "imported")
	b, _ := a.store.getBook(id)
	writeJSON(w, http.StatusOK, map[string]any{"book": b, "file": dest, "via": via})
}
