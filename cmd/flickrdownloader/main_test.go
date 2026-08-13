package main

import (
	"testing"

	"github.com/jooservices/flickrdownloader/pkg/api"
)

func testSets() []api.PhotoSetInfo {
	return []api.PhotoSetInfo{
		{ID: "1", Title: struct {
			Content string `json:"_content"`
		}{Content: "Wedding 2024"}},
		{ID: "2", Title: struct {
			Content string `json:"_content"`
		}{Content: "Honeymoon"}},
		{ID: "3", Title: struct {
			Content string `json:"_content"`
		}{Content: "Wedding Reception"}},
	}
}

func TestMatchSetsAllNone(t *testing.T) {
	sets := testSets()

	got, err := matchSets(sets, "all")
	if err != nil || len(got) != 3 {
		t.Fatalf("all: got %d sets, err %v", len(got), err)
	}

	got, err = matchSets(sets, "NONE")
	if err != nil || len(got) != 0 {
		t.Fatalf("none: got %d sets, err %v", len(got), err)
	}
}

func TestMatchSetsExactAndSubstring(t *testing.T) {
	sets := testSets()

	got, err := matchSets(sets, "Honeymoon")
	if err != nil || len(got) != 1 || got[0].ID != "2" {
		t.Fatalf("exact: got %+v, err %v", got, err)
	}

	got, err = matchSets(sets, "wedding") // case-insensitive substring
	if err != nil || len(got) != 2 {
		t.Fatalf("substring: got %d sets, err %v", len(got), err)
	}

	got, err = matchSets(sets, "  honeymoon , reception ") // trims whitespace
	if err != nil || len(got) != 2 {
		t.Fatalf("multi: got %d sets, err %v", len(got), err)
	}
}

func TestMatchSetsNoMatch(t *testing.T) {
	_, err := matchSets(testSets(), "does-not-exist")
	if err == nil {
		t.Fatal("expected error for unmatched name")
	}
}

func TestMatchSetsDedupes(t *testing.T) {
	// "wedding" matches two albums; a duplicate entry must not repeat them
	got, err := matchSets(testSets(), "wedding,wedding")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %d sets, err %v", len(got), err)
	}
}
