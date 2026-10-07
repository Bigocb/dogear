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
// don't create duplicates.
func (a *apiServer) findOrCreateBook(title, author string) (int64, error) {
	existing, _ := a.store.listBooks("", "")
	for _, b := range existing {
		if strings.EqualFold(b.Title, title) && strings.EqualFold(b.Author, author) {
			return b.ID, nil
		}
	}
	return a.store.addBook(&Book{Title: title, Author: author})
}
