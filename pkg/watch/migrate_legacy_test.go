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

// TestImportLegacyFilesMergesBothLegacyFiles is a regression test for a
// silent, unrecoverable data-loss bug: when both watchlist.yaml and
// sources.txt exist, only the first file's sources were ever loaded and
// persisted — yet both files were still renamed to .migrated and the
// one-shot import flag was still set, permanently discarding every URL in
// the second file with no error or warning. This asserts every source from
// both files survives the import, and both legacy files are still renamed.
func TestImportLegacyFilesMergesBothLegacyFiles(t *testing.T) {
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
`), 0o600); err != nil {
		t.Fatal(err)
	}
	textPath := filepath.Join(dir, "sources.txt")
	if err := os.WriteFile(textPath, []byte("https://www.flickr.com/photos/bob/\nhttps://www.flickr.com/photos/carol/\n"), 0o600); err != nil {
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

	if _, err := os.Stat(yamlPath + ".migrated"); err != nil {
		t.Fatalf("expected watchlist.yaml to be renamed: %v", err)
	}
	if _, err := os.Stat(textPath + ".migrated"); err != nil {
		t.Fatalf("expected sources.txt to be renamed: %v", err)
	}

	cfg, err := LoadFromStore(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	// Globals come from whichever file is found first (watchlist.yaml).
	if cfg.PollInterval != 20*time.Minute || cfg.OutputDir != "/tmp/legacy" || cfg.Workers != 3 {
		t.Fatalf("imported globals = %+v, want watchlist.yaml's", cfg)
	}
	if len(cfg.Sources) != 3 {
		t.Fatalf("imported sources = %d, want 3 (alice from yaml, bob+carol from sources.txt — none discarded)", len(cfg.Sources))
	}
	urls := map[string]bool{}
	for _, s := range cfg.Sources {
		urls[s.URL] = true
	}
	for _, want := range []string{
		"https://www.flickr.com/photos/alice/",
		"https://www.flickr.com/photos/bob/",
		"https://www.flickr.com/photos/carol/",
	} {
		if !urls[want] {
			t.Fatalf("imported sources = %v, missing %s", urls, want)
		}
	}
}

// TestImportLegacyFilesSkipsMalformedFileWithoutBlockingTheOther is a
// regression test for a side effect of the fix above: reading every found
// legacy file (instead of stopping at the first) means a malformed second
// file must not be able to abort the whole import and so permanently
// discard a valid first file's URLs too — that would trade one silent-data-
// loss bug for a "one bad file blocks the good one forever" bug. The
// malformed file must be skipped (left in place, not renamed) rather than
// merged or fatal.
func TestImportLegacyFilesSkipsMalformedFileWithoutBlockingTheOther(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "flickrdownloader")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	yamlPath := filepath.Join(dir, "watchlist.yaml")
	if err := os.WriteFile(yamlPath, []byte(`
sources:
  - url: https://www.flickr.com/photos/alice/
`), 0o600); err != nil {
		t.Fatal(err)
	}
	textPath := filepath.Join(dir, "sources.txt")
	if err := os.WriteFile(textPath, []byte("not a flickr url\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	if err := ImportLegacyFiles(ctx, store); err != nil {
		t.Fatalf("ImportLegacyFiles should not fail because of the malformed sources.txt: %v", err)
	}

	if _, err := os.Stat(yamlPath + ".migrated"); err != nil {
		t.Fatalf("expected the valid watchlist.yaml to still be imported and renamed: %v", err)
	}
	if _, err := os.Stat(textPath); err != nil {
		t.Fatalf("expected the malformed sources.txt to be left in place (not renamed) for the user to fix: %v", err)
	}
	if _, err := os.Stat(textPath + ".migrated"); !os.IsNotExist(err) {
		t.Fatalf("malformed sources.txt should not have been renamed to .migrated")
	}

	cfg, err := LoadFromStore(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sources) != 1 || cfg.Sources[0].URL != "https://www.flickr.com/photos/alice/" {
		t.Fatalf("imported sources = %+v, want only alice from the valid file", cfg.Sources)
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
