package main

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
)

func sPtr(s string) *string { return &s }
func iPtr(i int) *int       { return &i }

func TestFormatRank(t *testing.T) {
	if formatRank("epub") <= formatRank("mobi") {
		t.Error("epub should outrank mobi")
	}
	if formatRank("azw3") <= formatRank("pdf") {
		t.Error("azw3 should outrank pdf")
	}
	if formatRank("") != 0 || formatRank("weird") != 0 {
		t.Error("unknown formats rank 0")
	}
}

func TestPickReleasePrefersEpub(t *testing.T) {
	releases := []Release{
		{Source: "direct_download", SourceID: "pdf1", Title: "B", Format: sPtr("pdf")},
		{Source: "direct_download", SourceID: "mobi1", Title: "B", Format: sPtr("mobi")},
		{Source: "direct_download", SourceID: "epub1", Title: "B", Format: sPtr("epub")},
	}
	pick := PickRelease(releases, "")
	if pick == nil || *pick.Format != "epub" {
		t.Fatalf("expected epub picked, got %+v", pick)
	}
}

func TestPickReleaseSkipsAudiobooks(t *testing.T) {
	releases := []Release{
		{Source: "direct_download", SourceID: "ab", Title: "B", Format: sPtr("m4b"), ContentType: sPtr("audiobook")},
	}
	if pick := PickRelease(releases, ""); pick != nil {
		t.Error("audiobook-only releases should yield nil")
	}
}

func TestPickReleaseExplicitPreference(t *testing.T) {
	releases := []Release{
		{Source: "dd", SourceID: "1", Title: "B", Format: sPtr("epub")},
		{Source: "dd", SourceID: "2", Title: "B", Format: sPtr("pdf")},
	}
	pick := PickRelease(releases, "pdf")
	if pick == nil || *pick.Format != "pdf" {
		t.Fatalf("expected pdf explicit pick, got %+v", pick)
	}
}

func TestNormalizeTitle(t *testing.T) {
	cases := map[string]string{
		"Project Hail Mary.epub":         "project hail mary",
		"  Project - Hail   Mary ":       "project - hail mary",
		"We Are Legion (We Are Bob).m4b": "we are legion (we are bob",
	}
	for in, want := range cases {
		if got := normalizeTitle(in); got != want {
			t.Errorf("normalizeTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitize(t *testing.T) {
	if got := sanitize("A/B: C?"); got != "A_B_ C_" {
		t.Errorf("sanitize = %q", got)
	}
	if sanitize("") != "Unknown" {
		t.Error("empty -> Unknown")
	}
}

func TestEpubCover(t *testing.T) {
	// build a minimal epub with cover
	buf := new(bytes.Buffer)
	zw := zip.NewWriter(buf)
	// container.xml
	cxml := `<?xml version="1.0"?><container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`
	f1, _ := zw.Create("META-INF/container.xml")
	f1.Write([]byte(cxml))
	opf := `<?xml version="1.0"?><package><metadata><meta name="cover" content="cv"/></metadata><manifest><item id="cv" href="images/cover.jpg" media-type="image/jpeg"/></manifest></package>`
	f2, _ := zw.Create("OEBPS/content.opf")
	f2.Write([]byte(opf))
	img := bytes.Repeat([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00}, 100) // jpeg magic
	f3, _ := zw.Create("OEBPS/images/cover.jpg")
	f3.Write(img)
	zw.Close()

	path := filepath.Join(t.TempDir(), "test.epub")
	os.WriteFile(path, buf.Bytes(), 0o644)

	data, ct, err := epubCover(path)
	if err != nil {
		t.Fatalf("epubCover: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("no cover data")
	}
	if ct != "image/jpeg" {
		t.Errorf("ctype = %q", ct)
	}
}

func TestImporterMatchAndStore(t *testing.T) {
	// temp db
	s, err := openStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	id, err := s.addBook(&Book{Title: "Project Hail Mary", Author: "Andy Weir"})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.setStatus(id, "grabbed")

	libDir := t.TempDir()
	ingestDir := t.TempDir()
	im := NewImporter([]string{ingestDir}, libDir, s, testLogger())
	im.minAge = 0 // tests: import immediately

	// place a file whose name embeds the title
	src := filepath.Join(ingestDir, "Andy Weir - Project Hail Mary.epub")
	os.WriteFile(src, []byte("fake"), 0o644)

	if err := im.ScanOnce(context.Background(), ingestDir); err != nil {
		t.Fatal(err)
	}
	books, _ := s.listBooks("", "")
	if len(books) != 1 || books[0].Status != "imported" {
		t.Fatalf("expected 1 imported book, got %+v", books)
	}
	files, err := s.db.Query(`SELECT path FROM files WHERE book_id=?`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	if !files.Next() {
		t.Fatal("no file row")
	}
	// dest exists
	var p string
	files.Scan(&p)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("moved file missing: %v", p)
	}
}

func TestReconcileFilesRestoresWantedBug(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	owner := int64(1)
	if err := s.ensureUsersSchema(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO users(id,username,role) VALUES(1,'u','admin')`); err != nil {
		t.Fatal(err)
	}
	id, err := s.addBook(&Book{Title: "Monster Menu", Author: "Terrell Garrett", OwnerID: &owner})
	if err != nil {
		t.Fatal(err)
	}
	// simulate the bug: a file was recorded but the book stayed 'wanted'
	// with an outstanding grab and no shelf row.
	if _, err := s.db.Exec(`INSERT INTO files(book_id,path,format,size,imported_at) VALUES(?,?,?,?,1)`,
		id, "/library/x.epub", "epub", 100); err != nil {
		t.Fatal(err)
	}
	_ = s.addGrab(id, "libgen:abc", "queued")

	n, err := s.reconcileFiles()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reconciled %d, want 1", n)
	}
	b, _ := s.getBook(id)
	if b.Status != "imported" {
		t.Fatalf("status = %q, want imported", b.Status)
	}
	var state string
	s.db.QueryRow(`SELECT state FROM grabs WHERE book_id=?`, id).Scan(&state)
	if state != "done" {
		t.Fatalf("grab state = %q, want done", state)
	}
	if _, _, ok := s.shelfStatus(owner, id); !ok {
		t.Fatal("owner not shelved after reconcile")
	}

	// the guard: a later 'wanted' write must not hide a file-backed book
	if err := s.setStatus(id, "wanted"); err != nil {
		t.Fatal(err)
	}
	b, _ = s.getBook(id)
	if b.Status != "imported" {
		t.Fatalf("status = %q after guarded setStatus, want imported", b.Status)
	}
}

func TestHasFileFlag(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	id, err := s.addBook(&Book{Title: "No Copy"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.getBook(id)
	if b.HasFile {
		t.Fatal("book without a file should report has_file=false")
	}
	if _, err := s.db.Exec(`INSERT INTO files(book_id,path,format,size,imported_at) VALUES(?,?,?,?,1)`,
		id, "/library/x.epub", "epub", 5); err != nil {
		t.Fatal(err)
	}
	b, _ = s.getBook(id)
	if !b.HasFile {
		t.Fatal("book with a file should report has_file=true")
	}
	books, err := s.listBooks("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(books) != 1 || !books[0].HasFile {
		t.Fatalf("listBooks has_file = %+v", books)
	}
}

func TestLooksLikePersonName(t *testing.T) {
	yes := []string{"Logan Jacobs", "Andy Weir", "Frank Herbert", "J. R. R. Tolkien"}
	no := []string{"Dinosaur World 2", "The Divine Comedy", "Book 3", "Cultivation: Battle Mage Farmer"}
	for _, s := range yes {
		if !looksLikePersonName(s) {
			t.Errorf("expected %q to look like a person", s)
		}
	}
	for _, s := range no {
		if looksLikePersonName(s) {
			t.Errorf("expected %q NOT to look like a person", s)
		}
	}
}

func TestEpubMeta(t *testing.T) {
	buf := new(bytes.Buffer)
	zw := zip.NewWriter(buf)
	cxml := `<?xml version="1.0"?><container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`
	f1, _ := zw.Create("META-INF/container.xml")
	f1.Write([]byte(cxml))
	opf := `<?xml version="1.0"?><package><metadata><dc:title>Project Hail Mary</dc:title><dc:creator>Andy Weir</dc:creator></metadata></package>`
	f2, _ := zw.Create("OEBPS/content.opf")
	f2.Write([]byte(opf))
	zw.Close()

	path := filepath.Join(t.TempDir(), "t.epub")
	os.WriteFile(path, buf.Bytes(), 0o644)
	em, err := epubMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if em.Title != "Project Hail Mary" || em.Author != "Andy Weir" {
		t.Fatalf("meta = %+v", em)
	}
}

func TestImporterBackwardsDashNames(t *testing.T) {
	// "Title - Author" style file should parse correctly when importing fresh
	s, err := openStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	libDir := t.TempDir()
	ingestDir := t.TempDir()
	im := NewImporter([]string{ingestDir}, libDir, s, testLogger())
	im.minAge = 0
	im.CopyMode = false

	src := filepath.Join(ingestDir, "Dinosaur World 2 - Logan Jacobs.epub")
	os.WriteFile(src, []byte("fake"), 0o644)
	if err := im.ScanOnce(context.Background(), ingestDir); err != nil {
		t.Fatal(err)
	}
	books, _ := s.listBooks("", "")
	if len(books) != 1 {
		t.Fatalf("want 1 book, got %d", len(books))
	}
	b := books[0]
	if b.Title != "Dinosaur World 2" || b.Author != "Logan Jacobs" {
		t.Fatalf("parsed title=%q author=%q — wrong split", b.Title, b.Author)
	}
}

func testLogger() *log.Logger { return log.New(io.Discard, "", 0) }
