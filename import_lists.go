package main

import (
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// ImportSummary reports what an import did.
type ImportSummary struct {
	Source   string `json:"source"`
	Added    int    `json:"added"`
	Skipped  int    `json:"skipped"`  // already imported (by remote id)
	Existing int    `json:"existing"` // matched a book already in the library
	Failed   int    `json:"failed"`
}

// handleImportHardcover pulls the user's Hardcover shelf into their library.
// POST /api/import/hardcover {auto_download: bool, include_read: bool}
func (a *apiServer) handleImportHardcover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	user := userFromCtx(r)
	if user == nil {
		writeErr(w, http.StatusUnauthorized, "login required")
		return
	}
	if a.hardcover == nil || !a.hardcover.Configured() {
		writeErr(w, http.StatusPreconditionFailed, "Hardcover token not configured (admin → Acquisition sources)")
		return
	}
	var body struct {
		AutoDownload bool `json:"auto_download"`
		IncludeRead  bool `json:"include_read"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	items, err := a.hardcover.MyShelf(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "hardcover shelf: "+err.Error())
		return
	}
	sum := a.importShelf(user.ID, "hardcover", items, body.AutoDownload, body.IncludeRead)
	writeJSON(w, http.StatusOK, sum)
}

// importShelf turns shelf items into library entries for one user. Idempotent:
// items already imported (by remote id) are skipped, so re-syncing is safe.
func (a *apiServer) importShelf(userID int64, source string, items []ShelfItem, autoDownload, includeRead bool) ImportSummary {
	sum := ImportSummary{Source: source}
	for _, it := range items {
		if it.Title == "" {
			sum.Failed++
			continue
		}
		if a.store.importItemExists(userID, source, it.RemoteID) {
			sum.Skipped++
			continue
		}
		status := mapShelfStatus(it.StatusID, includeRead)
		if status == "" {
			// ignored / did-not-finish with include_read off -> skip entirely
			_ = a.store.recordImport(userID, source, it.RemoteID, 0, "skipped")
			sum.Skipped++
			continue
		}
		// already in the library? reuse it instead of creating a duplicate
		if existing := a.store.findBookByTitleAuthor(it.Title, it.Author); existing != nil {
			_ = a.store.shelfAdd(userID, existing.ID, status)
			if autoDownload {
				_ = a.store.setAutoGrab(existing.ID, true)
			}
			_ = a.store.recordImport(userID, source, it.RemoteID, existing.ID, status)
			sum.Existing++
			continue
		}
		provider := source
		provID := it.RemoteID
		var cover *string
		if it.CoverURL != "" {
			c := it.CoverURL
			cover = &c
		}
		owner := userID
		nb := &Book{
			Title: it.Title, Author: it.Author,
			Provider: &provider, ProviderID: &provID, CoverURL: cover,
			OwnerID: &owner, Private: true, AutoGrab: autoDownload,
		}
		id, err := a.store.addBook(nb)
		if err != nil {
			sum.Failed++
			continue
		}
		// addBook defaults to 'wanted'; set the mapped status on the shelf
		_ = a.store.shelfAdd(userID, id, status)
		_ = a.store.setStatus(id, status)
		if autoDownload {
			_ = a.store.setAutoGrab(id, true)
		}
		_ = a.store.recordImport(userID, source, it.RemoteID, id, status)
		sum.Added++
	}
	return sum
}

// mapShelfStatus maps a Hardcover status id to a Dogear status.
// 1=Want to Read, 2=Currently Reading, 3=Read, 5=Did Not Finish, 6=Ignored
func mapShelfStatus(statusID int, includeRead bool) string {
	switch statusID {
	case 1:
		return "wanted"
	case 2:
		return "reading"
	case 3:
		if includeRead {
			return "read"
		}
		return "" // treat already-read as "don't add" unless asked
	default:
		return "" // DNF / ignored
	}
}

// handleImportCSV imports a Goodreads (or compatible) CSV export.
// POST /api/import/csv  (multipart form: file, auto_download, include_read)
//
// Goodreads' export columns: Title, Author, ISBN, ISBN13, My Rating,
// Exclusive Shelf (read | currently-reading | to-read), ...
func (a *apiServer) handleImportCSV(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	user := userFromCtx(r)
	if user == nil {
		writeErr(w, http.StatusUnauthorized, "login required")
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "expected a multipart form with a 'file' field")
		return
	}
	autoDownload := r.FormValue("auto_download") == "true"
	includeRead := r.FormValue("include_read") == "true"

	f, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing file")
		return
	}
	defer f.Close()

	csvr := csv.NewReader(f)
	csvr.FieldsPerRecord = -1
	rows, err := csvr.ReadAll()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "couldn't read CSV: "+err.Error())
		return
	}
	if len(rows) < 2 {
		writeErr(w, http.StatusBadRequest, "CSV has no data rows")
		return
	}
	hdr := rows[0]
	col := func(name string) int {
		for i, h := range hdr {
			if strings.EqualFold(strings.TrimSpace(h), name) {
				return i
			}
		}
		return -1
	}
	cTitle := col("Title")
	cAuthor := col("Author")
	cShelf := col("Exclusive Shelf")
	if cShelf == -1 {
		cShelf = col("Bookshelves")
	}
	if cTitle == -1 {
		writeErr(w, http.StatusBadRequest, "CSV needs a Title column")
		return
	}

	sum := ImportSummary{Source: "goodreads"}
	for i, row := range rows[1:] {
		get := func(idx int) string {
			if idx >= 0 && idx < len(row) {
				return strings.TrimSpace(row[idx])
			}
			return ""
		}
		title := get(cTitle)
		if title == "" {
			continue
		}
		author := get(cAuthor)
		shelf := strings.ToLower(get(cShelf))
		if shelf == "" && len(row) > 0 {
			shelf = strings.ToLower(strings.TrimSpace(row[len(row)-1]))
		}
		status := csvShelfStatus(shelf, includeRead)
		if status == "" {
			sum.Skipped++
			continue
		}
		remoteID := "csv:" + strconv.Itoa(i) + ":" + strings.ToLower(title)
		if a.store.importItemExists(user.ID, "goodreads", remoteID) {
			sum.Skipped++
			continue
		}
		if existing := a.store.findBookByTitleAuthor(title, author); existing != nil {
			_ = a.store.shelfAdd(user.ID, existing.ID, status)
			if autoDownload {
				_ = a.store.setAutoGrab(existing.ID, true)
			}
			_ = a.store.recordImport(user.ID, "goodreads", remoteID, existing.ID, status)
			sum.Existing++
			continue
		}
		owner := user.ID
		src := "goodreads"
		nb := &Book{Title: title, Author: author, Provider: &src, ProviderID: &remoteID,
			OwnerID: &owner, Private: true, AutoGrab: autoDownload}
		id, err := a.store.addBook(nb)
		if err != nil {
			sum.Failed++
			continue
		}
		_ = a.store.shelfAdd(user.ID, id, status)
		_ = a.store.setStatus(id, status)
		_ = a.store.recordImport(user.ID, "goodreads", remoteID, id, status)
		sum.Added++
	}
	writeJSON(w, http.StatusOK, sum)
}

// csvShelfStatus maps a Goodreads/StoryGraph shelf string to a Dogear status.
func csvShelfStatus(shelf string, includeRead bool) string {
	switch {
	case strings.Contains(shelf, "currently-reading"), strings.Contains(shelf, "currently reading"):
		return "reading"
	case strings.Contains(shelf, "to-read"), strings.Contains(shelf, "want"):
		return "wanted"
	case shelf == "read":
		if includeRead {
			return "read"
		}
		return ""
	default:
		return ""
	}
}

var _ = io.Discard
