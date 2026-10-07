package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// aaDownloadAndImport resolves an md5 through the AA donator API, downloads it,
// and files it in the library under <Author>/<Title>/. Shared by the paste-link
// flow and the libgen search→AA grab. Returns the dest path.
func (a *apiServer) aaDownloadAndImport(ctx context.Context, md5, title, author string) (string, error) {
	if a.aa == nil || a.aa.key() == "" {
		return "", errString("AA donator key not configured")
	}
	dl, err := a.aa.Resolve(ctx, md5)
	if err != nil {
		return "", errString("AA resolve: " + err.Error())
	}
	stagingDir := os.Getenv("DOGEAR_AA_STAGING")
	if stagingDir == "" {
		stagingDir = "/data/staging"
	}
	base := title
	if author != "" {
		base = author + " - " + title
	}
	staged, err := a.aa.Download(ctx, dl, stagingDir, base)
	if err != nil {
		return "", errString("AA download: " + err.Error())
	}
	if author == "" {
		author = "AA Grabs"
	}
	destDir := filepath.Join(a.importer.Library, sanitize(author), sanitize(title))
	if err := os.MkdirAll(destDir, 0o775); err != nil {
		return "", err
	}
	ext := strings.ToLower(filepath.Ext(staged))
	dest := filepath.Join(destDir, sanitize(title)+ext)
	if err := os.Rename(staged, dest); err != nil {
		if err := copyFile(staged, dest); err != nil {
			return "", errString("library move: " + err.Error())
		}
		os.Remove(staged)
	}
	if ext == ".epub" {
		if coverData, ctype, cerr := epubCover(dest); cerr == nil && coverData != nil {
			coverExt := ".jpg"
			if strings.Contains(ctype, "png") {
				coverExt = ".png"
			}
			_ = os.WriteFile(filepath.Join(destDir, "cover"+coverExt), coverData, 0o644)
		}
	}
	return dest, nil
}

type errString string

func (e errString) Error() string { return string(e) }

// findOrCreateBook dedupes on (title, author) so repeat grabs of the same book
// don't create duplicates. New books are owned by ownerID (0 = unknown).
func (a *apiServer) findOrCreateBook(title, author string, ownerID int64) (int64, error) {
	existing, _ := a.store.listBooks("", "")
	for _, b := range existing {
		if strings.EqualFold(b.Title, title) && strings.EqualFold(b.Author, author) {
			return b.ID, nil
		}
	}
	nb := &Book{Title: title, Author: author}
	if ownerID != 0 {
		nb.OwnerID = &ownerID
	}
	return a.store.addBook(nb)
}

// libgenDownloadAndImport tries libgen's own download first (no quota, no key),
// then falls back to the AA donator key. Files into <Author>/<Title>/.
func (a *apiServer) libgenDownloadAndImport(ctx context.Context, md5, title, author string) (string, string, error) {
	stagingDir := os.Getenv("DOGEAR_AA_STAGING")
	if stagingDir == "" {
		stagingDir = "/data/staging"
	}
	base := title
	if author != "" {
		base = author + " - " + title
	}
	var staged string
	var err error
	if a.libgen != nil {
		staged, err = a.libgen.Download(ctx, md5, stagingDir, base)
		if err == nil {
			return a.finalizeImport(staged, title, author), "libgen", nil
		}
		a.libgen.log.Printf("libgen direct failed (%v); falling back to AA key", err)
	}
	// fallback: AA keyed fast download
	dest, aerr := a.aaDownloadAndImport(ctx, md5, title, author)
	if aerr != nil {
		if err != nil {
			return "", "", errString("libgen: " + err.Error() + "; AA: " + aerr.Error())
		}
		return "", "", aerr
	}
	return dest, "aa", nil
}

// finalizeImport moves a staged file into the library and extracts a cover.
func (a *apiServer) finalizeImport(staged, title, author string) string {
	if author == "" {
		author = "AA Grabs"
	}
	destDir := filepath.Join(a.importer.Library, sanitize(author), sanitize(title))
	if err := os.MkdirAll(destDir, 0o775); err != nil {
		return staged
	}
	ext := strings.ToLower(filepath.Ext(staged))
	dest := filepath.Join(destDir, sanitize(title)+ext)
	if err := os.Rename(staged, dest); err != nil {
		if err := copyFile(staged, dest); err != nil {
			return staged
		}
		os.Remove(staged)
	}
	if ext == ".epub" {
		if coverData, ctype, cerr := epubCover(dest); cerr == nil && coverData != nil {
			coverExt := ".jpg"
			if strings.Contains(ctype, "png") {
				coverExt = ".png"
			}
			_ = os.WriteFile(filepath.Join(destDir, "cover"+coverExt), coverData, 0o644)
		}
	}
	return dest
}
