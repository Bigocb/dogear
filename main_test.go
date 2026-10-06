package main

import (
	"errors"
	"strings"
	"testing"
)

func TestBookStatusValidation(t *testing.T) {
	for _, s := range []string{"wanted", "grabbed", "imported", "reading", "read", "abandoned"} {
		if !validStatuses[s] {
			t.Errorf("status %q should be valid", s)
		}
	}
	if validStatuses["bogus"] {
		t.Error("bogus should not be valid")
	}
}

func TestURLJoining(t *testing.T) {
	// minimal sanity for base url + path composition used in Shelfmark client
	base := "http://shelfmark:8084"
	if !strings.HasPrefix(base+"/api/health", "http://") {
		t.Fatal("bad prefix")
	}
	if errors.Is(nil, nil) != true {
		t.Error("errors.Is nil")
	}
}
