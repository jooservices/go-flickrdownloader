package watch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jooservices/go-flickrdownloader/pkg/cache"
)

func TestImportLegacyFilesCopiesYAMLAndRenames(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "flickrdownloader")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	yamlPath := filepath.Join(dir, "watchlist.yaml")
	if err := os.WriteFile(yamlPath, []byte(`
poll_interval: 20m
output_dir: /tmp/legacy
workers: 3
sources:
  - url: https://www.flickr.com/photos/alice/
    albums: all
    uncategorized: false
  - url: https://www.flickr.com/photos/bob/
`), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	if err := ImportLegacyFiles(ctx, store); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(yamlPath); !os.IsNotExist(err) {
		t.Fatalf("original yaml still present: %v", err)
	}
	if _, err := os.Stat(yamlPath + ".migrated"); err != nil {
		t.Fatalf("expected .migrated file: %v", err)
	}

	cfg, err := LoadFromStore(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollInterval != 20*time.Minute || cfg.OutputDir != "/tmp/legacy" || cfg.Workers != 3 {
		t.Fatalf("imported globals = %+v", cfg)
	}
	if len(cfg.Sources) != 2 {
		t.Fatalf("imported sources = %d, want 2", len(cfg.Sources))
	}
	if cfg.Sources[0].IncludeOrphans {
		t.Fatal("alice should not include orphans")
	}

	if err := os.WriteFile(yamlPath, []byte(`
sources:
  - url: https://www.flickr.com/photos/carol/
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ImportLegacyFiles(ctx, store); err != nil {
		t.Fatal(err)
	}
	cfg, _ = LoadFromStore(ctx, store)
	if len(cfg.Sources) != 2 {
		t.Fatalf("second import should be a no-op, sources = %d", len(cfg.Sources))
	}
}

func TestImportLegacyFilesNoopWhenMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := ImportLegacyFiles(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFromStore(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sources) != 0 {
		t.Fatalf("sources = %+v, want empty", cfg.Sources)
	}
}
