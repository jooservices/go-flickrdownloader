package ui

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPickerVisibleFilter(t *testing.T) {
	p := &picker{
		items: []PickerItem{
			{ID: "1", Label: "Wedding 2024"},
			{ID: "2", Label: "Honeymoon"},
			{ID: "3", Label: "wedding reception"},
		},
	}

	vis := p.visible()
	if len(vis) != 3 {
		t.Fatalf("no filter: expected 3 visible, got %d", len(vis))
	}

	p.filter = "wedding" // case-insensitive substring
	vis = p.visible()
	if len(vis) != 2 || p.items[vis[0]].ID != "1" || p.items[vis[1]].ID != "3" {
		t.Fatalf("filter: got %v", vis)
	}

	p.filter = "xyz"
	if vis = p.visible(); len(vis) != 0 {
		t.Fatalf("expected no matches, got %d", len(vis))
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 20); got != "short" {
		t.Fatalf("got %q", got)
	}
	got := truncate("a very long label indeed", 8)
	if utf8.RuneCountInString(got) > 8 || !strings.HasSuffix(got, "…") {
		t.Fatalf("got %q", got)
	}
}
