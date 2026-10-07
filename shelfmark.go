package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Release mirrors shelfmark.release_sources.Release (dataclass serialization).
type Release struct {
	Source      string         `json:"source"`
	SourceID    string         `json:"source_id"`
	Title       string         `json:"title"`
	Format      *string        `json:"format"`
	Language    *string        `json:"language"`
	Size        *string        `json:"size"`
	SizeBytes   *int64         `json:"size_bytes"`
	DownloadURL *string        `json:"download_url"`
	InfoURL     *string        `json:"info_url"`
	Indexer     *string        `json:"indexer"`
	Seeders     *int           `json:"seeders"`
	ContentType *string        `json:"content_type"`
	Extra       map[string]any `json:"extra"`
}

// smBook is Shelfmark's raw metadata hit shape.
type smBook struct {
	Provider string   `json:"provider"`
	BookID   string   `json:"provider_id"`
	Title    string   `json:"title"`
	Authors  []string `json:"authors"`
	ISBN13   *string  `json:"isbn_13"`
	ISBN10   *string  `json:"isbn_10"`
	CoverURL *string  `json:"cover_url"`
}

func (m smBook) author() string {
	if len(m.Authors) > 0 {
		return m.Authors[0]
	}
	return ""
}

func (m smBook) fullISBN() string {
	if m.ISBN13 != nil && *m.ISBN13 != "" {
		return *m.ISBN13
	}
	if m.ISBN10 != nil && *m.ISBN10 != "" {
		return *m.ISBN10
	}
	return ""
}

type Shelfmark struct {
	base     string
	user     string
	pass     string
	client   *http.Client
	loggedIn time.Time
	logger   *log.Logger
}

func NewShelfmark(baseURL string) (*Shelfmark, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &Shelfmark{
		base:   baseURL,
		client: &http.Client{Jar: jar, Timeout: 90 * time.Second},
		logger: log.New(os.Stderr, "dogear/shelfmark ", log.LstdFlags|log.Lmsgprefix),
	}, nil
}

func (s *Shelfmark) login(ctx context.Context) error {
	form := url.Values{}
	form.Set("username", s.user)
	form.Set("password", s.pass)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/api/auth/login", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("login status %d", resp.StatusCode)
	}
	s.loggedIn = time.Now()
	return nil
}

func (s *Shelfmark) authed(ctx context.Context, method, path, contentType string, body []byte) (*http.Response, error) {
	do := func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, s.base+path, nil)
		if err != nil {
			return nil, err
		}
		if body != nil {
			// re-attach body (request bodies cannot be replayed)
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		return s.client.Do(req)
	}
	resp, err := do()
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		if s.user == "" {
			return resp, nil // anonymous mode; return the 401 as-is
		}
		if lerr := s.login(ctx); lerr != nil {
			return nil, fmt.Errorf("relogin: %w", lerr)
		}
		resp, err = do()
	}
	return resp, err
}

func (s *Shelfmark) getJSON(ctx context.Context, path string, out any) error {
	resp, err := s.authed(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s status %d: %s", path, resp.StatusCode, string(body[:min(200, len(body))]))
	}
	return json.Unmarshal(body, out)
}

func (s *Shelfmark) SearchMetadata(ctx context.Context, query string) ([]MetaResult, error) {
	var out struct {
		Books []smBook `json:"books"`
	}
	if err := s.getJSON(ctx, "/api/metadata/search?query="+url.QueryEscape(query), &out); err != nil {
		return nil, err
	}
	res := make([]MetaResult, 0, len(out.Books))
	for _, b := range out.Books {
		res = append(res, MetaResult{
			Provider: b.Provider, BookID: b.BookID, Title: b.Title,
			Author: b.author(), ISBN: b.fullISBN(), CoverURL: b.CoverURL,
		})
	}
	return res, nil
}

func (s *Shelfmark) SearchReleases(ctx context.Context, provider, bookID string) ([]Release, error) {
	var out struct {
		Releases []Release `json:"releases"`
	}
	path := fmt.Sprintf("/api/releases?provider=%s&book_id=%s", url.QueryEscape(provider), url.QueryEscape(bookID))
	if err := s.getJSON(ctx, path, &out); err != nil {
		return nil, err
	}
	return out.Releases, nil
}

// formatRank ranks book formats; higher is better.
func formatRank(f string) int {
	order := []string{"epub", "mobi", "azw3", "pdf", "fb2", "djvu", "cbz", "cbr"}
	f = strings.ToLower(strings.TrimSpace(f))
	for i, o := range order {
		if f == o {
			return len(order) - i
		}
	}
	return 0
}

// PickRelease auto-picks the best release: prefer epub, then bigger
// seeders, then smaller size among equal rank.
func PickRelease(releases []Release, preferFormat string) *Release {
	var best *Release
	bestScore := -1
	pref := strings.ToLower(preferFormat)
	for i := range releases {
		r := &releases[i]
		if r.ContentType != nil && *r.ContentType == "audiobook" {
			continue // Dogear is ebooks-only
		}
		format := ""
		if r.Format != nil {
			format = *r.Format
		}
		if pref != "" && format != "" && format == pref {
			// preferred format wins immediately
			return r
		}
		score := formatRank(format) * 100
		if r.Seeders != nil {
			score += min(*r.Seeders, 50) // cap seeders influence
		}
		if r.SizeBytes != nil {
			// slight bias toward smaller files at equal format
			score += int(min(10, int64(10-(*r.SizeBytes)/(200<<20))))
		}
		if score > bestScore {
			bestScore = score
			best = r
		}
	}
	return best
}

// SortReleasesByPreference is a helper for tests / manual pick UI.
func SortReleasesByPreference(releases []Release, preferFormat string) {
	prefer := strings.ToLower(preferFormat)
	sort.SliceStable(releases, func(i, j int) bool {
		fi, fj := "", ""
		if releases[i].Format != nil {
			fi = *releases[i].Format
		}
		if releases[j].Format != nil {
			fj = *releases[j].Format
		}
		pi, pj := 0, 0
		if fi == prefer {
			pi = 2
		} else {
			pi = formatRank(fi)
		}
		if fj == prefer {
			pj = 2
		} else {
			pj = formatRank(fj)
		}
		return pi > pj
	})
}

// Grab queues the chosen release on Shelfmark.
func (s *Shelfmark) Grab(ctx context.Context, r *Release) error {
	payload := map[string]any{
		"source":    r.Source,
		"source_id": r.SourceID,
		"title":     r.Title,
	}
	if r.Format != nil {
		payload["format"] = *r.Format
	}
	if r.Size != nil {
		payload["size"] = *r.Size
	}
	if r.ContentType != nil {
		payload["content_type"] = *r.ContentType
	}
	if r.Extra != nil {
		payload["extra"] = r.Extra
	}
	if r.DownloadURL != nil {
		payload["download_url"] = *r.DownloadURL
	}
	buf, _ := json.Marshal(payload)
	resp, err := s.authed(ctx, http.MethodPost, "/api/releases/download", "application/json", buf)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("download status %d: %s", resp.StatusCode, string(body[:min(200, len(body))]))
	}
	return nil
}

// DownloadActive mirrors /api/downloads/active item shape (subset).
func (s *Shelfmark) DownloadActive(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	if err := s.getJSON(ctx, "/api/downloads/active", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// unused import guard
var _ = strconv.Itoa
