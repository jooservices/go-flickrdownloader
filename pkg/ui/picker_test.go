package ui

import (
	"io"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/term"
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

func TestReadKeyPlainByte(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := w.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	w.Close()

	key, err := readKey(r)
	if err != nil {
		t.Fatalf("readKey: %v", err)
	}
	if key != 'a' {
		t.Fatalf("key = %d, want %d ('a')", key, 'a')
	}
}

func TestReadKeyArrowUp(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := w.Write([]byte{keyEsc, '[', 'A'}); err != nil {
		t.Fatal(err)
	}
	w.Close()

	key, err := readKey(r)
	if err != nil {
		t.Fatalf("readKey: %v", err)
	}
	if key != keyUp {
		t.Fatalf("key = %d, want keyUp (%d)", key, keyUp)
	}
}

func TestReadKeyEscAloneTimesOut(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := w.Write([]byte{keyEsc}); err != nil {
		t.Fatal(err)
	}
	// Deliberately not closing w: readKey must not block waiting for more
	// bytes past its 30ms deadline when only ESC arrived.
	defer w.Close()

	key, err := readKey(r)
	if err != nil {
		t.Fatalf("readKey: %v", err)
	}
	if key != keyEsc {
		t.Fatalf("key = %d, want keyEsc (%d)", key, keyEsc)
	}
}

func TestReadKeyEOF(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	defer r.Close()

	_, err = readKey(r)
	if err == nil {
		t.Fatal("expected an error (EOF) reading from a closed pipe")
	}
}

func TestPickMultiEmptyItemsReturnsUnchanged(t *testing.T) {
	got, err := PickMulti("title", nil)
	if err != nil {
		t.Fatalf("PickMulti: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want empty", got)
	}
}

func TestPickMultiNonTTYReturnsItemsUnchanged(t *testing.T) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		t.Skip("stdin is a real terminal in this environment; PickMulti would block on input")
	}
	items := []PickerItem{{ID: "1", Label: "Album A"}}
	got, err := PickMulti("title", items)
	if err != nil {
		t.Fatalf("PickMulti: %v", err)
	}
	if len(got) != 1 || got[0].ID != "1" {
		t.Fatalf("got %+v, want the input unchanged", got)
	}
}

func TestPickerRedraw(t *testing.T) {
	p := &picker{
		items: []PickerItem{
			{ID: "1", Label: "Album A", Detail: "10 photos", Selected: true},
			{ID: "2", Label: "Album B"},
		},
		title:  "Pick albums",
		height: 10,
		width:  80,
	}

	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	p.redraw()
	w.Close()
	os.Stdout = origStdout

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "Pick albums") || !strings.Contains(got, "Album A") || !strings.Contains(got, "Album B") {
		t.Fatalf("redraw output = %q, want the title and both album labels", got)
	}
	if !strings.Contains(got, "1/2 selected") {
		t.Fatalf("redraw output = %q, want the selected count", got)
	}
}

func TestPickerRedrawNoMatches(t *testing.T) {
	p := &picker{
		items:  []PickerItem{{ID: "1", Label: "Album A"}},
		filter: "nonexistent",
		height: 10,
		width:  80,
	}

	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	p.redraw()
	w.Close()
	os.Stdout = origStdout

	out, _ := io.ReadAll(r)
	if !strings.Contains(string(out), "no matches") {
		t.Fatalf("redraw output = %q, want a no-matches message", out)
	}
}
