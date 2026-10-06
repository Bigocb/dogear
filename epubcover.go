package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"regexp"
	"strings"
)

// epubCover extracts the highest-value raster cover from an epub.
// Returns (imageBytes, contentType, error).
func epubCover(path string) ([]byte, string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, "", err
	}
	defer zr.Close()

	// 1. find OPF via META-INF/container.xml
	var opfPath string
	for _, f := range zr.File {
		if strings.EqualFold(f.Name, "META-INF/container.xml") {
			rc, err := f.Open()
			if err != nil {
				break
			}
			data, _ := io.ReadAll(rc)
			rc.Close()
			opfPath = strings.TrimSpace(containerRootfile(string(data)))
			break
		}
	}
	if opfPath == "" {
		// fallback: any .opf
		for _, f := range zr.File {
			if strings.HasSuffix(strings.ToLower(f.Name), ".opf") {
				opfPath = f.Name
				break
			}
		}
	}
	if opfPath == "" {
		return nil, "", io.ErrUnexpectedEOF
	}
	opfDir := ""
	if i := strings.LastIndex(opfPath, "/"); i >= 0 {
		opfDir = opfPath[:i+1]
	}

	var coverHref string
	for _, f := range zr.File {
		if f.Name != opfPath {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, "", err
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		coverHref = opfCoverHref(string(data))
		break
	}
	// try manifest-recognized cover first
	if coverHref != "" {
		prefix := opfDir
		hrefFull := coverHref
		if !strings.HasPrefix(hrefFull, "http") {
			hrefFull = prefix + hrefFull
		}
		for _, f := range zr.File {
			if f.Name == hrefFull {
				rc, err := f.Open()
				if err != nil {
					break
				}
				data, _ := io.ReadAll(rc)
				rc.Close()
				ct := "image/jpeg"
				if strings.HasSuffix(strings.ToLower(hrefFull), ".png") {
					ct = "image/png"
				}
				return data, ct, nil
			}
		}
	}
	// 2. fallback: largest image file inside
	var best zip.File
	found := false
	var bestLen uint64
	for _, f := range zr.File {
		lo := strings.ToLower(f.Name)
		if !strings.Contains(lo, "cover") {
			continue
		}
		if strings.HasSuffix(lo, ".jpg") || strings.HasSuffix(lo, ".jpeg") || strings.HasSuffix(lo, ".png") {
			if f.CompressedSize64 > bestLen {
				best = *f
				bestLen = f.CompressedSize64
				found = true
			}
		}
	}
	if found {
		rc, err := best.Open()
		if err != nil {
			return nil, "", err
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		ct := "image/jpeg"
		if strings.HasSuffix(strings.ToLower(best.Name), ".png") {
			ct = "image/png"
		}
		return data, ct, nil
	}
	return nil, "", errNoCover
}

var errNoCover = io.EOF

// EpubMeta holds title/author from an epub's OPF metadata.
type EpubMeta struct {
	Title  string
	Author string
}

// epubMeta extracts dc:title and dc:creator from the epub's OPF.
func epubMeta(path string) (EpubMeta, error) {
	var meta EpubMeta
	zr, err := zip.OpenReader(path)
	if err != nil {
		return meta, err
	}
	defer zr.Close()

	var opfPath string
	for _, f := range zr.File {
		if strings.EqualFold(f.Name, "META-INF/container.xml") {
			rc, err := f.Open()
			if err != nil {
				break
			}
			data, _ := io.ReadAll(rc)
			rc.Close()
			opfPath = strings.TrimSpace(containerRootfile(string(data)))
			break
		}
	}
	if opfPath == "" {
		for _, f := range zr.File {
			if strings.HasSuffix(strings.ToLower(f.Name), ".opf") {
				opfPath = f.Name
				break
			}
		}
	}
	if opfPath == "" {
		return meta, io.ErrUnexpectedEOF
	}
	for _, f := range zr.File {
		if f.Name != opfPath {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return meta, err
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		meta.Title = xmlTagText(string(data), "dc:title")
		meta.Author = xmlTagText(string(data), "dc:creator")
		return meta, nil
	}
	return meta, io.ErrUnexpectedEOF
}

func xmlTagText(xmlText, tag string) string {
	re := regexp.MustCompile(`<` + tag + `[^>]*>([^<]*)</` + tag + `>`)
	if m := re.FindStringSubmatch(xmlText); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func containerRootfile(xmlText string) string {
	type container struct {
		Rootfiles []struct {
			FullPath string `xml:"full-path,attr"`
		} `xml:"rootfiles>rootfile"`
	}
	var c container
	if err := xml.Unmarshal([]byte(xmlText), &c); err != nil {
		return ""
	}
	if len(c.Rootfiles) > 0 {
		return c.Rootfiles[0].FullPath
	}
	return ""
}

func opfCoverHref(opfXML string) string {
	// find <meta name="cover" content="cover-image-id"/>
	reMeta := regexp.MustCompile(`<meta[^>]+name=["\']cover["\'][^>]+content=["\']([^"\']+)["\']`)
	id := ""
	if m := reMeta.FindStringSubmatch(opfXML); m != nil {
		id = m[1]
	}
	// find item with that id OR properties="cover-image"
	if id != "" {
		reItem := regexp.MustCompile(`<item[^>]+id=["\']` + regexp.QuoteMeta(id) + `["\'][^>]*>`)
		if m := reItem.FindStringSubmatch(opfXML); m != nil {
			reHref := regexp.MustCompile(`href=["\']([^"\']+)["\']`)
			if hm := reHref.FindStringSubmatch(m[0]); hm != nil {
				return strings.TrimPrefix(strings.TrimPrefix(hm[1], "./"), "/")
			}
		}
	}
	reProps := regexp.MustCompile(`<item[^>]+properties=["\'][^"\']*cover-image[^>]+>`)
	if m := reProps.FindStringSubmatch(opfXML); m != nil {
		reHref := regexp.MustCompile(`href=["\']([^"\']+)["\']`)
		if hm := reHref.FindStringSubmatch(m[0]); hm != nil {
			return strings.TrimPrefix(strings.TrimPrefix(hm[1], "./"), "/")
		}
	}
	return ""
}

var _ = bytes.TrimSpace
