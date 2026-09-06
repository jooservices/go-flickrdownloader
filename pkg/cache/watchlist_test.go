package cache

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jooservices/flickrdownloader/pkg/api"
)

func TestOpenCreatesWatchlistSchemaV8(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var version int
	if err := store.db.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, currentSchemaVersion)
	}
	var n int
	if err := store.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'watchlist'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("watchlist table count = %d err=%v", n, err)
	}
}

func TestWatchlistInsertListDeleteAndUniqueURL(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	ok, err := store.InsertWatchlist(ctx, WatchlistEntry{
		SourceURL:      "https://www.flickr.com/photos/alice/",
		Albums:         "all",
		IncludeOrphans: true,
		PollInterval:   time.Hour,
		Enabled:        true,
	})
	if err != nil || !ok {
		t.Fatalf("insert = %v err=%v, want inserted", ok, err)
	}
	dup, err := store.InsertWatchlist(ctx, WatchlistEntry{
		SourceURL: "https://www.flickr.com/photos/alice/",
		Enabled:   true,
	})
	if err != nil || dup {
		t.Fatalf("duplicate insert = %v err=%v, want skipped", dup, err)
	}

	entries, err := store.ListWatchlist(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].SourceURL != "https://www.flickr.com/photos/alice/" || !entries[0].Enabled || entries[0].PollInterval != time.Hour {
		t.Fatalf("entries = %+v", entries)
	}

	removed, err := store.DeleteWatchlist(ctx, []string{"https://www.flickr.com/photos/alice/", "https://www.flickr.com/photos/missing/"})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "https://www.flickr.com/photos/alice/" {
		t.Fatalf("removed = %v", removed)
	}
}

func TestClearLeavesWatchlistIntact(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	if err := store.PutResponse(ctx, "key", "flickr.photos.getInfo", []byte("body"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertWatchlist(ctx, WatchlistEntry{
		SourceURL:      "https://www.flickr.com/photos/alice/",
		IncludeOrphans: true,
		Enabled:        true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetWatchlistGlobals(ctx, WatchlistGlobals{PollInterval: 30 * time.Minute, Workers: 4}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutPhotoMetadata(ctx, api.PhotoMetadata{ID: "1", URL: "https://example.invalid/1.jpg"}); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := store.Get(ctx, "key"); err != nil || found {
		t.Fatalf("response found=%v err=%v, want cleared", found, err)
	}
	entries, err := store.ListWatchlist(ctx)
	if err != nil || len(entries) != 1 {
		t.Fatalf("watchlist after clear = %+v err=%v", entries, err)
	}
	g, err := store.GetWatchlistGlobals(ctx)
	if err != nil || g.Workers != 4 || g.PollInterval != 30*time.Minute {
		t.Fatalf("globals after clear = %+v err=%v", g, err)
	}
}
