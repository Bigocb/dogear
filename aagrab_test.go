package main

import (
	"testing"
)

func TestParseAALink(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"d6e1dc51a50726f00ec438af21952a45", "d6e1dc51a50726f00ec438af21952a45", true},
		{"https://annas-archive.gd/md5/d6e1dc51a50726f00ec438af21952a45", "d6e1dc51a50726f00ec438af21952a45", true},
		{"https://annas-archive.gd/fast_download/d6e1dc51a50726f00ec438af21952a45/0/3", "d6e1dc51a50726f00ec438af21952a45", true},
		{"annas-archive.gd/md5/D6E1DC51A50726F00EC438AF21952A45", "d6e1dc51a50726f00ec438af21952a45", true},
		{"", "", false},
		{"not an md5", "", false},
		{"https://example.com/book/123", "", false},
	}
	for _, c := range cases {
		got, ok := ParseAALink(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseAALink(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestSeriesFromTitle(t *testing.T) {
	name, order, ok := seriesFromTitle("Dune 3")
	if !ok || name != "dune" || order != 3 {
		t.Fatalf("Dune 3 -> %q %v %v", name, order, ok)
	}
	name, order, ok = seriesFromTitle("Skulduggery Pleasant Book 12")
	if !ok || order != 12 {
		t.Fatalf("Book 12 -> %q %v %v", name, order, ok)
	}
	_, _, ok = seriesFromTitle("Project Hail Mary")
	if ok {
		t.Fatal("non-numbered title flagged as series")
	}
}

func TestLooksLikePersonNameStillGood(t *testing.T) {
	if !looksLikePersonName("Andy Weir") {
		t.Error("Andy Weir should be a person")
	}
	if looksLikePersonName("Project Hail Mary") {
		t.Error("title should not be a person")
	}
}
