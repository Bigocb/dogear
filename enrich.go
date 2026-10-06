package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// flexInt accepts a JSON number or string and yields an *int.
type flexInt struct{ v *int }

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	n := 0
	for _, ch := range s {
		if ch >= '0' && ch <= '9' {
			n = n*10 + int(ch-'0')
		} else if n > 0 {
			break
		}
	}
	if n > 0 {
		f.v = &n
	}
	return nil
}

// flexFloat accepts number or string.
type flexFloat struct{ v *float64 }

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	var n float64
	_, err := fmt.Sscanf(s, "%g", &n)
	if err == nil && n != 0 {
		f.v = &n
	}
	return nil
}

// MetaDetail is the rich metadata Shelfmark returns (Hardcover/OpenLibrary).
type MetaDetail struct {
	Title          string    `json:"title"`
	Authors        []string  `json:"authors"`
	Description    string    `json:"description"`
	PublishYear    *int      `json:"-"`
	Publisher      *string   `json:"publisher"`
	SeriesName     *string   `json:"series_name"`
	SeriesPosition *float64  `json:"-"`
	Language       *string   `json:"language"`
	ISBN13         *string   `json:"isbn_13"`
	CoverURL       *string   `json:"cover_url"`
	Genres         []string  `json:"genres"`
	RawYear        flexInt   `json:"publish_year"`
	RawSeriesPos   flexFloat `json:"series_position"`
}

// EnrichSearch finds the best-matching book's full metadata in Shelfmark's
// providers (Hardcover/OpenLibrary), returning the detail payload.
func (s *Shelfmark) EnrichSearch(ctx context.Context, query string, wantAuthor string) (*MetaDetail, error) {
	var out struct {
		Books []MetaDetail `json:"books"`
	}
	if err := s.getJSON(ctx, "/api/metadata/search?query="+url.QueryEscape(query), &out); err != nil {
		return nil, err
	}
	if len(out.Books) == 0 {
		return nil, nil
	}
	// choose: highest preference to a title/author overlap
	best := &out.Books[0]
	bestScore := -1
	q := strings.ToLower(query)
	wa := strings.ToLower(wantAuthor)
	for i := range out.Books {
		b := &out.Books[i]
		score := 0
		if strings.Contains(strings.ToLower(b.Title), q) || strings.Contains(q, strings.ToLower(b.Title)) {
			score += 2
		}
		for _, a := range b.Authors {
			if wa != "" && strings.Contains(strings.ToLower(a), wa) {
				score += 3
			}
		}
		// richer records preferred
		if b.Description != "" {
			score++
		}
		if b.CoverURL != nil && *b.CoverURL != "" {
			score++
		}
		if score > bestScore {
			bestScore = score
			best = b
		}
	}
	// year may arrive as string; normalize
	if best.RawYear.v != nil {
		best.PublishYear = best.RawYear.v
	}
	if best.SeriesPosition == nil && best.RawSeriesPos.v != nil {
		best.SeriesPosition = best.RawSeriesPos.v
	}
	return best, nil
}

var _ = json.Marshal
