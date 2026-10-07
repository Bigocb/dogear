package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const lgUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"

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

// resolveDirectLink scrapes the keyed get.php link from libgen's ads.php page.
func (l *Libgen) resolveDirectLink(ctx context.Context, base, md5 string) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, base+"/ads.php?md5="+md5, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", lgUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := l.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("libgen ads %d", resp.StatusCode)
	}
	m := regexp.MustCompile(`get\.php\?md5=[a-f0-9]{32}&key=[A-Za-z0-9]+`).FindString(string(body))
	if m == "" {
		return "", fmt.Errorf("no download link on libgen page")
	}
	if strings.HasPrefix(m, "http") {
		return m, nil
	}
	return base + "/" + m, nil
}

// Download fetches the file directly from libgen (no AA key needed). libgen's
// DB is frequently overloaded, so this validates the response is a real file,
// bounds each attempt with a timeout, and returns an error otherwise so the
// caller can fall back to AA.
func (l *Libgen) Download(ctx context.Context, md5, destDir, baseName string) (string, error) {
	var lastErr error
	for _, base := range l.mirrors {
		link, err := l.resolveDirectLink(ctx, base, md5)
		if err != nil {
			lastErr = err
			continue
		}
		for attempt := 0; attempt < 2; attempt++ {
			// bound each download attempt so a stalled libgen never blocks the
			// caller's fallback path
			actx, cancel := context.WithTimeout(ctx, 45*time.Second)
			req, err := http.NewRequestWithContext(actx, http.MethodGet, link, nil)
			if err != nil {
				cancel()
				lastErr = err
				break
			}
			req.Header.Set("User-Agent", lgUA)
			req.Header.Set("Referer", base+"/ads.php?md5="+md5)
			resp, err := l.client.Do(req)
			if err != nil {
				cancel()
				lastErr = err
				continue
			}
			ct := resp.Header.Get("Content-Type")
			cd := resp.Header.Get("Content-Disposition")
			if strings.Contains(ct, "text/html") && resp.StatusCode >= 400 {
				resp.Body.Close()
				cancel()
				lastErr = fmt.Errorf("libgen %s: HTTP %d (overloaded?)", base, resp.StatusCode)
				continue
			}
			name := baseName
			if m := regexp.MustCompile(`filename="?([^";]+)"?`).FindStringSubmatch(cd); m != nil {
				name = m[1]
			}
			ext := strings.ToLower(filepath.Ext(name))
			if !bookExts[ext] {
				ext = ".epub"
				name = strings.TrimSuffix(name, filepath.Ext(name)) + ext
			}
			if err := os.MkdirAll(destDir, 0o775); err != nil {
				resp.Body.Close()
				cancel()
				return "", err
			}
			dest := filepath.Join(destDir, sanitize(strings.TrimSuffix(name, ext))+ext)
			out, err := os.Create(dest)
			if err != nil {
				resp.Body.Close()
				cancel()
				return "", err
			}
			size, cerr := io.Copy(out, resp.Body)
			out.Close()
			resp.Body.Close()
			cancel()
			if cerr != nil || size < 1000 {
				os.Remove(dest)
				lastErr = fmt.Errorf("libgen %s: short/empty download", base)
				continue
			}
			l.log.Printf("libgen direct download ok: %s (%d bytes)", dest, size)
			return dest, nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("libgen download failed")
	}
	return "", lastErr
}
