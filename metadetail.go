package main

import (
	"strconv"
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
	parsed, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err == nil && parsed != 0 {
		n = parsed
		f.v = &n
	}
	return nil
}

// MetaDetail is the normalized rich metadata returned by any metadata source
// (Hardcover, OpenLibrary).
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
