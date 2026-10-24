package main

import (
	"bytes"
	"encoding/base64"
	"os"
	"regexp"
	"testing"
)

// The apple-touch-icon is embedded as a data URI in each page so iOS "Add to
// Home Screen" needs no separate fetch. Keep it in sync with the PNG file.
func TestEmbeddedTouchIconMatchesFile(t *testing.T) {
	want, err := os.ReadFile("web/static/touch-icon-180.png")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`<link rel="apple-touch-icon"[^>]*href="data:image/png;base64,([^"]+)"`)
	for _, page := range []string{"index", "reader", "admin", "login"} {
		html, err := os.ReadFile("web/" + page + ".html")
		if err != nil {
			t.Fatal(err)
		}
		m := re.FindSubmatch(html)
		if m == nil {
			t.Errorf("%s.html: no embedded apple-touch-icon", page)
			continue
		}
		got, err := base64.StdEncoding.DecodeString(string(m[1]))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s.html: embedded icon differs from touch-icon-180.png (regenerate it)", page)
		}
	}
}
