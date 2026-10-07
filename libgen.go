package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Libgen searches Library Genesis (a fork mirror that exposes md5s) and returns
// releases whose ID is the file md5. Those md5s are shared with Anna's Archive,
// so a libgen search result can be downloaded through the AA donator key --
// which bypasses AA's captcha'd search page entirely.
type Libgen struct {
	mirrors []string
	client  *http.Client
	log     *log.Logger
}

func NewLibgen(mirrors []string, logf *log.Logger) *Libgen {
	if len(mirrors) == 0 {
		mirrors = []string{
			"https://libgen.vg",
			"https://libgen.la",
			"https://libgen.bz",
			"https://libgen.gs",
		}
	}
	return &Libgen{mirrors: mirrors, client: &http.Client{Timeout: 45 * time.Second}, log: logf}
}

// searchURL builds a libgen search request. The md5-bearing table lives on
// /index.php?req=<query> for the .vg/.la/.bz forks.
func (l *Libgen) searchURL(base, query string) string {
	return base + "/index.php?req=" + url.QueryEscape(query) + "&res=100"
}

var (
	lgRowRe  = regexp.MustCompile(`(?is)<tr[^>]*>(.*?)</tr>`)
	lgCellRe = regexp.MustCompile(`(?is)<td[^>]*>(.*?)</td>`)
	lgMD5Re  = regexp.MustCompile(`(?i)md5=([a-f0-9]{32})`)
	lgTagRe  = regexp.MustCompile(`(?is)<[^>]+>`)
	lgExtRe  = regexp.MustCompile(`(?i)\b(pdf|epub|mobi|azw3|djvu|fb2|cbz|cbr)\b`)
	lgWSRe   = regexp.MustCompile(`\s+`)
)

func lgText(s string) string {
	return strings.TrimSpace(lgWSRe.ReplaceAllString(lgTagRe.ReplaceAllString(s, " "), " "))
}

// Search returns normalized releases (Source "libgen", SourceID = md5).
func (l *Libgen) Search(ctx context.Context, query string) ([]Release, error) {
	var lastErr error
	for _, base := range l.mirrors {
		rels, err := l.searchMirror(ctx, base, query)
		if err != nil {
			lastErr = err
			continue
		}
		if len(rels) > 0 {
			return rels, nil
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, nil
}

func (l *Libgen) searchMirror(ctx context.Context, base, query string) ([]Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.searchURL(base, query), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36")
	resp, err := l.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("libgen %s: HTTP %d", base, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	html := string(body)

	var out []Release
	seen := map[string]bool{}
	for _, row := range lgRowRe.FindAllStringSubmatch(html, -1) {
		rowHTML := row[1]
		m := lgMD5Re.FindStringSubmatch(rowHTML)
		if m == nil {
			continue
		}
		md5 := strings.ToLower(m[1])
		if seen[md5] {
			continue
		}
		cells := lgCellRe.FindAllStringSubmatch(rowHTML, -1)
		if len(cells) < 2 {
			continue
		}
		title := lgText(cells[0][1])
		author := lgText(cells[1][1])
		ext := ""
		if em := lgExtRe.FindStringSubmatch(rowHTML); em != nil {
			ext = strings.ToLower(em[1])
		}
		// size column is usually index 6 ("26 MB", "781 kB")
		size := ""
		for _, c := range cells {
			t := lgText(c[1])
			if regexp.MustCompile(`^\d+(\.\d+)?\s*(kB|MB|GB|B)$`).MatchString(t) {
				size = t
				break
			}
		}
		// trim a trailing " href=..." artifact from title if any
		if i := strings.Index(title, `"`); i > 0 {
			title = strings.TrimSpace(title[:i])
		}
		seen[md5] = true
		out = append(out, Release{
			Source:      "libgen",
			SourceID:    md5,
			Title:       title,
			Format:      strOrNil(ext),
			Size:        strOrNil(size),
			ContentType: strOrNil("book"),
			Indexer:     &base,
			Extra:       map[string]any{"md5": md5, "author": author, "libgen_base": base},
		})
	}
	return out, nil
}
