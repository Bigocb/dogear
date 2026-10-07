package main

import (
	"context"
	"log"
	"strings"
	"time"
)

// WantedWatcher periodically retries wanted books: it searches the acquisition
// sources and grabs the best ebook release when one appears (and the book has
// auto-grab enabled). This is the "set it and forget it" wanted list.
type WantedWatcher struct {
	api      *apiServer
	log      *log.Logger
	interval time.Duration
}

func NewWantedWatcher(api *apiServer, logf *log.Logger) *WantedWatcher {
	return &WantedWatcher{api: api, log: logf, interval: 6 * time.Hour}
}

// cfgWantedWatcherOn gates the whole feature (admin setting, default on).
const cfgWantedWatcherOn = "wanted_watcher_enabled"

func (w *WantedWatcher) enabled() bool {
	v := w.api.store.config(cfgWantedWatcherOn, "", "true")
	return v != "false" && v != "0"
}

func (w *WantedWatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	w.log.Printf("wanted watcher started (every %s)", w.interval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !w.enabled() {
				continue
			}
			if err := w.Tick(ctx); err != nil {
				w.log.Printf("wanted watcher: %v", err)
			}
		}
	}
}

// Tick retries every eligible wanted book once.
func (w *WantedWatcher) Tick(ctx context.Context) error {
	// wanted books that have no file yet, auto-grab on
	rows, err := w.api.store.db.Query(`
		SELECT ` + bookColsP("b.") + ` FROM books b
		WHERE b.status IN ('wanted') AND b.auto_grab != 0
		  AND NOT EXISTS (SELECT 1 FROM files f WHERE f.book_id = b.id)
		ORDER BY b.added_at ASC LIMIT 25`)
	if err != nil {
		return err
	}
	type job struct {
		id            int64
		title, author string
	}
	var jobs []job
	for rows.Next() {
		b, err := scanBook(rows)
		if err != nil {
			continue
		}
		jobs = append(jobs, job{b.ID, b.Title, b.Author})
	}
	rows.Close()

	for _, j := range jobs {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		w.tryOne(ctx, j.id, j.title, j.author)
		// pace lookups so we don't hammer the sources
		time.Sleep(3 * time.Second)
	}
	return nil
}

func (w *WantedWatcher) tryOne(ctx context.Context, id int64, title, author string) {
	query := title
	if author != "" {
		query += " " + author
	}
	// Prefer libgen (keyless search, md5 downloads via libgen or AA key).
	var pick *Release
	if w.api.libgen != nil {
		if rels, err := w.api.libgen.Search(ctx, query); err == nil {
			pick = bestEbook(rels)
		}
	}
	// Fall back to Prowlarr torrents only if nothing direct was found.
	if pick == nil && w.api.prowlarr != nil && w.api.prowlarr.Configured() {
		if rels, err := w.api.prowlarr.Search(ctx, query, 100); err == nil {
			var norm []Release
			for _, r := range rels {
				norm = append(norm, normalizeProwlarr(r))
			}
			pick = bestEbook(norm)
		}
	}
	if pick == nil {
		w.log.Printf("wanted: no release yet for %q", title)
		return
	}

	// direct source -> grab the md5 and import
	if pick.Source == "libgen" {
		dest, via, err := w.api.libgenDownloadAndImport(ctx, strings.ToLower(pick.SourceID), title, author)
		if err != nil {
			w.log.Printf("wanted: grab %q failed: %v", title, err)
			return
		}
		ext := strings.TrimPrefix(strings.ToLower(pathExt(dest)), ".")
		_, _ = w.api.importer.storeRecord(&Book{ID: id, Title: title, Author: author}, dest, ext, fileSize(dest), "")
		_ = w.api.store.setStatus(id, "imported")
		// put it on the owner's shelf (kept private per the personal-first model)
		if owner := w.api.store.ownerOf(id); owner != 0 {
			_ = w.api.store.shelfAdd(owner, id, "imported")
		}
		_ = w.api.store.addGrab(id, "autograb:libgen:"+pick.SourceID, "done")
		w.log.Printf("wanted: auto-grabbed %q via %s", title, via)
		return
	}
	// torrent source -> queue it, leave at 'grabbed'
	if pick.Source == "prowlarr-direct" {
		idxID := 0
		if pick.Extra != nil {
			if v, ok := pick.Extra["indexerId"].(float64); ok {
				idxID = int(v)
			}
		}
		if err := w.api.prowlarr.Grab(ctx, ProwlarrResult{GUID: pick.SourceID, IndexerID: idxID, Title: pick.Title}); err != nil {
			w.log.Printf("wanted: prowlarr grab %q failed: %v", title, err)
			return
		}
		_ = w.api.store.setStatus(id, "grabbed")
		_ = w.api.store.addGrab(id, "autograb:prowlarr:"+pick.SourceID, "queued")
		w.log.Printf("wanted: auto-queued torrent for %q", title)
	}
}

// bestEbook picks the highest-ranked ebook release, preferring epub.
func bestEbook(rels []Release) *Release {
	var best *Release
	bestScore := -1
	for i := range rels {
		r := &rels[i]
		if r.ContentType != nil && *r.ContentType == "audiobook" {
			continue
		}
		f := ""
		if r.Format != nil {
			f = strings.ToLower(*r.Format)
		}
		score := formatRank(f) * 100
		if r.Seeders != nil {
			score += min(*r.Seeders, 50)
		}
		if score > bestScore {
			bestScore = score
			best = r
		}
	}
	return best
}
