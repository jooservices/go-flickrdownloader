package download

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jooservices/flickrdownloader/pkg/api"
	"github.com/jooservices/flickrdownloader/pkg/ui"
)

func TestWorkerHardlinkDedupe(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	existing := filepath.Join(dir1, "123.jpg")
	if err := os.WriteFile(existing, []byte("photo-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := New(nil, dir2, 1)
	d.downloadedPaths = map[string]string{"123": existing}
	d.progress = ui.NewProgress(1)

	d.worker(context.Background(), api.Photo{
		ID:          "123",
		URLOriginal: "https://example.invalid/never-fetched.jpg",
		Media:       "photo",
	})

	target := filepath.Join(dir2, "123.jpg")
	st1, err1 := os.Stat(existing)
	st2, err2 := os.Stat(target)
	if err1 != nil || err2 != nil {
		t.Fatalf("expected both files to exist: %v %v", err1, err2)
	}
	if !os.SameFile(st1, st2) {
		t.Fatalf("expected %s to be a hardlink of %s", target, existing)
	}
	if got := d.progress.Stats().Linked; got != 1 {
		t.Fatalf("expected 1 linked, got %d", got)
	}
	if got := d.progress.Stats().Success; got != 0 {
		t.Fatalf("expected 0 downloads, got %d", got)
	}
}

func TestWorkerSkipsExisting(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "123.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := New(nil, dir, 1)
	d.progress = ui.NewProgress(1)

	d.worker(context.Background(), api.Photo{ID: "123", URLOriginal: "https://example.invalid/x.jpg", Media: "photo"})

	if got := d.progress.Stats().Skipped; got != 1 {
		t.Fatalf("expected 1 skipped, got %d", got)
	}
}
