package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// OpenLibrary provides keyless book metadata (search + detail). No account,
// no API key -- this is the default metadata source so external installs work
// with zero configuration.
type OpenLibrary struct {
	client *http.Client
	log    *log.Logger
}

func NewOpenLibrary(logf *log.Logger) *OpenLibrary {
	return &OpenLibrary{client: &http.Client{Timeout: 30 * time.Second}, log: logf}
}

// OLDoc is the subset of OpenLibrary's search docs we use.
type OLDoc struct {
	Key            string   `json:"key"`
	Title          string   `json:"title"`
	AuthorName     []string `json:"author_name"`
	FirstPublishYr *int     `json:"first_publish_year"`
	CoverID        *int     `json:"cover_i"`
	ISBN           []string `json:"isbn"`
	Subject        []string `json:"subject"`
	Language       []string `json:"language"`
	Publisher      []string `json:"publisher"`
	PageCount      *int     `json:"number_of_pages_median"`

	// populated for the detail endpoint
	Description    *string  `json:"-"`
	SeriesName     *string  `json:"-"`
	SeriesPosition *float64 `json:"-"`
}

// MetaResult is the canonical search-hit shape across all metadata sources
// (OpenLibrary, Hardcover, Shelfmark). The UI consumes this directly.
type MetaResult struct {
	Provider string  `json:"provider"`
	BookID   string  `json:"book_id"`
	Title    string  `json:"title"`
	Author   string  `json:"author"`
	ISBN     string  `json:"isbn"`
	CoverURL *string `json:"cover_url"`
}

// MetadataResult is the UI-facing shape (shared by all metadata sources).
type MetadataResult = MetaResult

// Search returns normalized results across OpenLibrary.
func (o *OpenLibrary) Search(ctx context.Context, query string) ([]MetadataResult, error) {
	u := "https://openlibrary.org/search.json?limit=25&fields=key,title,author_name,first_publish_year,cover_i,isbn,subject,language,publisher,number_of_pages_median&q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Dogear/1.0 (self-hosted ebook library)")
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openlibrary search: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Docs []OLDoc `json:"docs"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	res := make([]MetadataResult, 0, len(out.Docs))
	for _, d := range out.Docs {
		author := ""
		if len(d.AuthorName) > 0 {
			author = d.AuthorName[0]
		}
		isbn := ""
		if len(d.ISBN) > 0 {
			isbn = d.ISBN[0]
		}
		var cover *string
		if d.CoverID != nil {
			c := fmt.Sprintf("https://covers.openlibrary.org/b/id/%d-M.jpg", *d.CoverID)
			cover = &c
		}
		id := OLWorkID(d.Key)
		res = append(res, MetadataResult{
			Provider: "openlibrary",
			BookID:   id,
			Title:    d.Title,
			Author:   author,
			ISBN:     isbn,
			CoverURL: cover,
		})
	}
	return res, nil
}

// Detail fetches full metadata (description, series, subjects) for a work id.
func (o *OpenLibrary) Detail(ctx context.Context, workID string) (*MetaDetail, error) {
	u := "https://openlibrary.org/works/" + url.PathEscape(workID) + ".json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Dogear/1.0 (self-hosted ebook library)")
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openlibrary work: HTTP %d", resp.StatusCode)
	}
	var w struct {
		Title       string          `json:"title"`
		Description json.RawMessage `json:"description"`
		Subjects    []string        `json:"subjects"`
		Series      []string        `json:"series"`
		Covers      []int           `json:"covers"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, err
	}
	md := &MetaDetail{Title: w.Title, Genres: w.Subjects}
	// description is either a string or {"type":..,"value":..}
	if len(w.Description) > 0 {
		var s string
		if json.Unmarshal(w.Description, &s) == nil {
			md.Description = s
		} else {
			var obj struct {
				Value string `json:"value"`
			}
			if json.Unmarshal(w.Description, &obj) == nil {
				md.Description = obj.Value
			}
		}
	}
	if len(w.Series) > 0 {
		md.SeriesName = &w.Series[0]
	}
	if len(w.Covers) > 0 {
		c := fmt.Sprintf("https://covers.openlibrary.org/b/id/%d-L.jpg", w.Covers[0])
		md.CoverURL = &c
	}
	return md, nil
}

// OLWorkID extracts a work id from a provider book id (which may already be one).
func OLWorkID(bookID string) string {
	return strings.TrimPrefix(strings.TrimPrefix(bookID, "/works/"), "works/")
}

var _ = strconv.Itoa
