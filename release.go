package main

import (
	"sort"
	"strings"
)

// Release is a normalized downloadable release from any acquisition source
// (libgen, prowlarr-direct, ...). The picker merges releases from every
// enabled source and grabs route back to the source that produced them.
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

// PickRelease auto-picks the best release: prefer epub, then bigger seeders,
// then smaller size among equal rank.
func PickRelease(releases []Release, preferFormat string) *Release {
	var best *Release
	bestScore := -1
	pref := strings.ToLower(preferFormat)
	for i := range releases {
		r := &releases[i]
		if r.ContentType != nil && *r.ContentType == "audiobook" {
			continue
		}
		format := ""
		if r.Format != nil {
			format = *r.Format
		}
		if pref != "" && format != "" && format == pref {
			return r
		}
		score := formatRank(format) * 100
		if r.Seeders != nil {
			score += min(*r.Seeders, 50)
		}
		if r.SizeBytes != nil {
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
