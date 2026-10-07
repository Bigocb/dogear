package main

import (
	"bytes"
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

// Prowlarr searches indexers (via a Prowlarr instance) and grabs by handing the
// release to Prowlarr's configured download client (Deluge/SAB/etc.).
type Prowlarr struct {
	store  *Store
	client *http.Client
	log    *log.Logger
}

func NewProwlarr(store *Store, logf *log.Logger) *Prowlarr {
	return &Prowlarr{
		store:  store,
		client: &http.Client{Timeout: 90 * time.Second},
		log:    logf,
	}
}

func (p *Prowlarr) base() string { return strings.TrimRight(p.store.prowlarrURL(), "/") }

// Configured reports whether a URL and API key are available and enabled.
func (p *Prowlarr) Configured() bool {
	return p.store.prowlarrEnabled() && p.base() != "" && p.store.prowlarrKey() != ""
}

// Status hits /system/status to verify connectivity.
func (p *Prowlarr) Status(ctx context.Context) (map[string]any, error) {
	out := map[string]any{}
	if err := p.getJSON(ctx, "/api/v1/system/status", &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *Prowlarr) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base()+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", p.store.prowlarrKey())
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("prowlarr %d: %s", resp.StatusCode, string(body[:min(160, len(body))]))
	}
	return json.Unmarshal(body, out)
}

// ProwlarrResult mirrors the subset of /api/v1/search results we need.
type ProwlarrResult struct {
	Title       string  `json:"title"`
	Indexer     string  `json:"indexer"`
	IndexerID   int     `json:"indexerId"`
	Size        int64   `json:"size"`
	Seeders     *int    `json:"seeders"`
	Leechers    *int    `json:"leechers"`
	DownloadURL *string `json:"downloadUrl"`
	MagnetURL   *string `json:"magnetUrl"`
	GUID        string  `json:"guid"`
	Categories  []any   `json:"categories"`
	PublishDate string  `json:"publishDate"`
}

// Search queries Prowlarr. Ebook-ish results are preferred when categories is empty.
func (p *Prowlarr) Search(ctx context.Context, query string, limit int) ([]ProwlarrResult, error) {
	if !p.Configured() {
		return nil, fmt.Errorf("prowlarr not configured")
	}
	if limit <= 0 {
		limit = 100
	}
	var out []ProwlarrResult
	path := "/api/v1/search?query=" + url.QueryEscape(query) + "&limit=" + strconv.Itoa(limit)
	if err := p.getJSON(ctx, path, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Grab hands a search result to Prowlarr, which routes it to its download client.
func (p *Prowlarr) Grab(ctx context.Context, r ProwlarrResult) error {
	if !p.Configured() {
		return fmt.Errorf("prowlarr not configured")
	}
	payload, _ := json.Marshal(map[string]any{
		"guid":      r.GUID,
		"indexerId": r.IndexerID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base()+"/api/v1/search", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", p.store.prowlarrKey())
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("prowlarr grab %d: %s", resp.StatusCode, string(body))
	}
	p.log.Printf("prowlarr grab queued: %s (%s)", r.Title, r.Indexer)
	return nil
}

// normalizeProwlarr maps a Prowlarr result to the app's Release shape so the
// existing release picker/grab UI works unchanged.
func normalizeProwlarr(r ProwlarrResult) Release {
	format := ""
	low := strings.ToLower(r.Title)
	for _, f := range []string{"epub", "mobi", "azw3", "pdf", "fb2", "cbz", "cbr"} {
		if strings.Contains(low, f) {
			format = f
			break
		}
	}
	var size string
	if r.Size > 0 {
		size = humanSize(r.Size)
	}
	seeders := r.Seeders
	idx := r.Indexer
	title := r.Title
	// Prowlarr results are torrents; the UI's Release struct reuses these fields.
	// Source name is "prowlarr-direct" so it never collides with Shelfmark's own
	// internal "prowlarr" source (which has a different grab API).
	return Release{
		Source:      "prowlarr-direct",
		SourceID:    r.GUID,
		Title:       title,
		Format:      strOrNil(format),
		Size:        strOrNil(size),
		SizeBytes:   &r.Size,
		Seeders:     seeders,
		Indexer:     &idx,
		ContentType: strOrNil("book"),
		Extra:       map[string]any{"indexerId": r.IndexerID, "magnet": r.MagnetURL, "dogear_direct": true},
	}
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func strOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
