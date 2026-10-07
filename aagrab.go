package main

import (
	"context"
	"encoding/json"
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

// AAGrabber downloads books straight from Anna's Archive using a donator key.
type AAGrabber struct {
	Base   string
	Key    string
	store  *Store // when set, resolve base/key from settings (UI-configurable)
	client *http.Client
	log    *log.Logger
}

func (a *AAGrabber) base() string {
	if a.store != nil {
		if v := a.store.aaBaseURL(); v != "" {
			return strings.TrimRight(v, "/")
		}
	}
	return strings.TrimRight(a.Base, "/")
}

func (a *AAGrabber) key() string {
	if a.store != nil {
		if v := a.store.aaKey(); v != "" {
			return v
		}
	}
	return a.Key
}

func NewAAGrabber(base, key string, logf *log.Logger) *AAGrabber {
	if base == "" {
		base = "https://annas-archive.gd"
	}
	return &AAGrabber{
		Base:   base,
		Key:    key,
		client: &http.Client{Timeout: 120 * time.Second},
		log:    logf,
	}
}

var md5Re = regexp.MustCompile(`^[a-f0-9]{32}$|^[A-F0-9]{32}$`)

var md5InURL = regexp.MustCompile(`(?i)[?&/](?:md5=|md5/|fast_download/)([a-f0-9]{32})`)

// ParseAALink accepts a raw md5 or an AA URL (md5/<md5>, fast_download/<md5>, ?md5=...).
func ParseAALink(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if md5Re.MatchString(s) {
		return strings.ToLower(s), true
	}
	if m := md5InURL.FindStringSubmatch(s); m != nil {
		return strings.ToLower(m[1]), true
	}
	return "", false
}

type fastDownloadResp struct {
	DownloadURL string `json:"download_url"`
	Error       string `json:"error"`
}

// Resolve asks AA for the fast download URL for an md5.
func (a *AAGrabber) Resolve(ctx context.Context, md5 string) (string, error) {
	if a.key() == "" {
		return "", fmt.Errorf("AA donator key not configured")
	}
	u := fmt.Sprintf("%s/dyn/api/fast_download.json?md5=%s&key=%s", a.base(), url.QueryEscape(md5), url.QueryEscape(a.key()))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36")
	resp, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out fastDownloadResp
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("AA api %d: %s", resp.StatusCode, string(body[:min(160, len(body))]))
	}
	if out.DownloadURL == "" {
		return "", fmt.Errorf("AA: %s", orDefault(out.Error, resp.Status))
	}
	return out.DownloadURL, nil
}

// Download fetches the resolved URL and stores it in the ingest dir.
// Returns the stored file path.
func (a *AAGrabber) Download(ctx context.Context, downloadURL, destDir, baseName string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36")
	resp, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", fmt.Errorf("download %d: %s", resp.StatusCode, string(body))
	}

	// detect format from content-disposition or content-type
	name := baseName
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if m := regexp.MustCompile(`filename="?([^";]+)"?`).FindStringSubmatch(cd); m != nil {
			name = m[1]
		}
	}
	ext := strings.ToLower(filepath.Ext(name))
	if !bookExts[ext] {
		switch {
		case strings.Contains(resp.Header.Get("Content-Type"), "pdf"):
			ext = ".pdf"
		case strings.Contains(resp.Header.Get("Content-Type"), "epub"):
			ext = ".epub"
		default:
			ext = ".epub"
		}
		name = strings.TrimSuffix(name, filepath.Ext(name)) + ext
	}

	if err := os.MkdirAll(destDir, 0o775); err != nil {
		return "", err
	}
	// stage the download in the ingest dir; the importer handles organization
	dest := filepath.Join(destDir, sanitize(strings.TrimSuffix(name, ext))+ext)
	out, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	size, err := io.Copy(out, resp.Body)
	out.Close()
	if err != nil {
		os.Remove(dest)
		return "", err
	}
	if size < 1000 {
		os.Remove(dest)
		return "", fmt.Errorf("downloaded file suspiciously small (%d bytes)", size)
	}
	a.log.Printf("AA fast download stored: %s (%d bytes)", dest, size)
	return dest, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
