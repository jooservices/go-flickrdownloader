package watch

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jooservices/go-flickrdownloader/pkg/cache"
)

func TestStoreRoundTripAndSkipDuplicates(t *testing.T) {
	store, err := cache.Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	if err := store.SetWatchlistGlobals(ctx, cache.WatchlistGlobals{
		PollInterval: 45 * time.Minute,
		OutputDir:    "/tmp/out",
		Workers:      6,
	}); err != nil {
		t.Fatal(err)
	}

	added, err := AddToStore(ctx, store, []string{
		"https://www.flickr.com/photos/alice/",
		"https://www.flickr.com/photos/alice/",
		"https://www.flickr.com/photos/bob/",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 2 {
		t.Fatalf("added = %v, want alice and bob", added)
	}
	if _, err := AddToStore(ctx, store, []string{"https://example.com/not-flickr"}); err == nil {
		t.Fatal("expected an error for a non-Flickr URL")
	}

	cfg, err := LoadFromStore(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollInterval != 45*time.Minute || cfg.OutputDir != "/tmp/out" || cfg.Workers != 6 {
		t.Fatalf("globals = %+v", cfg)
	}
	if len(cfg.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(cfg.Sources))
	}
	if cfg.Sources[0].PollInterval != 45*time.Minute || cfg.Sources[0].OutputDir != "/tmp/out" {
		t.Fatalf("inherited source = %+v", cfg.Sources[0])
	}
	if !cfg.Sources[0].Enabled || !cfg.Sources[0].IncludeOrphans {
		t.Fatalf("defaults = %+v", cfg.Sources[0])
	}

	removed, err := RemoveFromStore(ctx, store, []string{"https://www.flickr.com/photos/alice/"})
	if err != nil || len(removed) != 1 {
		t.Fatalf("removed = %v err=%v", removed, err)
	}
	cfg, _ = LoadFromStore(ctx, store)
	if len(cfg.Sources) != 1 || cfg.Sources[0].URL != "https://www.flickr.com/photos/bob/" {
		t.Fatalf("after remove = %+v", cfg.Sources)
	}
}
