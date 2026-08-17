package cache

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jooservices/flickrdownloader/pkg/api"
)

func TestStoreRoundTripAndDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "responses.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	fetchedAt := time.Unix(123, 0)
	body := []byte(`{"stat":"ok"}`)
	if err := store.Put(context.Background(), "key", body, fetchedAt); err != nil {
		t.Fatal(err)
	}

	got, gotAt, found, err := store.Get(context.Background(), "key")
	if err != nil {
		t.Fatal(err)
	}
	if !found || string(got) != string(body) || gotAt.Unix() != fetchedAt.Unix() {
		t.Fatalf("got body=%q fetched=%v found=%v", got, gotAt, found)
	}

	if err := store.Delete(context.Background(), "key"); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := store.Get(context.Background(), "key"); err != nil || found {
		t.Fatalf("after delete found=%v err=%v", found, err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("cache permissions = %o, want 600", got)
	}
}

func TestPhotosetStatusRoundTrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "responses.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	want := PhotosetStatus{
		RootDir:     "/photos",
		OwnerNSID:   "owner",
		PhotosetID:  "set-1",
		Title:       "Album",
		Directory:   "/photos/owner/Album",
		ExpectedIDs: []string{"2", "1"},
		Complete:    true,
		FileSizes:   map[string]int64{"1": 100},
		VerifiedAt:  time.Unix(456, 0),
	}
	if err := store.PutPhotosetStatus(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetPhotosetStatus(context.Background(), want.RootDir, want.OwnerNSID, want.PhotosetID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Title != want.Title || got.Directory != want.Directory ||
		!got.Complete || got.VerifiedAt.Unix() != want.VerifiedAt.Unix() ||
		len(got.ExpectedIDs) != 2 || got.ExpectedIDs[0] != "2" || got.ExpectedIDs[1] != "1" || got.FileSizes["1"] != 100 {
		t.Fatalf("status = %+v, want %+v", got, want)
	}
	bulk, err := store.GetPhotosetStatuses(context.Background(), want.RootDir, want.OwnerNSID)
	if err != nil || len(bulk) != 1 || bulk[want.PhotosetID].Title != want.Title {
		t.Fatalf("bulk statuses = %+v err=%v, want one status", bulk, err)
	}
}

func TestOpenMigratesLegacyResponseSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE responses (
			cache_key TEXT PRIMARY KEY,
			body BLOB NOT NULL,
			fetched_at INTEGER NOT NULL
		)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO responses(cache_key, body, fetched_at) VALUES('legacy', 'body', 123)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	body, fetchedAt, found, err := store.Get(context.Background(), "legacy")
	if err != nil || !found || string(body) != "body" || fetchedAt.Unix() != 123 {
		t.Fatalf("legacy response body=%q fetched=%v found=%v err=%v", body, fetchedAt, found, err)
	}
	if err := store.PutResponse(context.Background(), "new", "flickr.photos.getSizes", []byte("new-body"), time.Unix(456, 0)); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := store.db.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, currentSchemaVersion)
	}
}

func TestPruneUsesListingRetention(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "responses.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Unix(1_000_000, 0)
	entries := []struct {
		key, method string
		age         time.Duration
	}{
		{"old-listing", "flickr.photosets.getPhotos", 48 * time.Hour},
		{"fresh-listing", "flickr.photosets.getPhotos", 23 * time.Hour},
		{"old-detail", "flickr.photos.getSizes", 31 * 24 * time.Hour},
	}
	for _, entry := range entries {
		if err := store.PutResponse(context.Background(), entry.key, entry.method, []byte(entry.key), now.Add(-entry.age)); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := store.Prune(context.Background(), now, 24*time.Hour, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if _, _, found, err := store.Get(context.Background(), "fresh-listing"); err != nil || !found {
		t.Fatalf("fresh listing found=%v err=%v, want retained", found, err)
	}
	for _, key := range []string{"old-listing", "old-detail"} {
		if _, _, found, err := store.Get(context.Background(), key); err != nil || found {
			t.Fatalf("%s found=%v err=%v, want pruned", key, found, err)
		}
	}
}

func TestClearRemovesResponsesAndStatuses(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "responses.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.PutResponse(context.Background(), "key", "flickr.photos.getInfo", []byte("body"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.PutPhotosetStatus(context.Background(), PhotosetStatus{
		RootDir: "/photos", OwnerNSID: "owner", PhotosetID: "set", Title: "Album",
		Directory: "/photos/owner/Album", ExpectedIDs: []string{"1"}, Complete: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutPhotoMetadata(context.Background(), api.PhotoMetadata{ID: "1", URL: "https://example.invalid/1.jpg"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.TryResponseLease(context.Background(), "lease", "owner-1", time.Hour); err != nil || !ok {
		t.Fatalf("lease = %v err=%v, want acquired", ok, err)
	}

	removed, err := store.Clear(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 4 {
		t.Fatalf("removed = %d, want 4", removed)
	}
	if _, _, found, err := store.Get(context.Background(), "key"); err != nil || found {
		t.Fatalf("response found=%v err=%v, want cleared", found, err)
	}
	status, err := store.GetPhotosetStatus(context.Background(), "/photos", "owner", "set")
	if err != nil || status != nil {
		t.Fatalf("status = %+v err=%v, want cleared", status, err)
	}
	meta, err := store.GetPhotoMetadata(context.Background(), []string{"1"})
	if err != nil || len(meta) != 0 {
		t.Fatalf("metadata = %+v err=%v, want cleared", meta, err)
	}
	if ok, err := store.TryResponseLease(context.Background(), "lease", "owner-2", time.Minute); err != nil || !ok {
		t.Fatalf("lease after clear = %v err=%v, want free", ok, err)
	}
}

func TestPhotoMetadataRoundTripAndLease(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "responses.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	want := api.PhotoMetadata{
		ID: "123", URL: "https://static.example/123.jpg", Media: "photo",
		OriginalFormat: "jpg", Extension: "jpg", OWidth: 100, OHeight: 80,
		SizeBytes: 1234, UpdatedAt: time.Unix(789, 0),
	}
	if err := store.PutPhotoMetadata(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	if err := store.PutPhotoMetadata(context.Background(), api.PhotoMetadata{ID: "123", URL: want.URL, Media: "photo", Extension: "jpg"}); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetPhotoMetadata(context.Background(), []string{"123", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if got["123"].URL != want.URL || got["123"].SizeBytes != want.SizeBytes || len(got) != 1 {
		t.Fatalf("metadata = %+v, want one matching record", got)
	}

	first, err := store.TryResponseLease(context.Background(), "key", "owner-1", time.Minute)
	if err != nil || !first {
		t.Fatalf("first lease = %v err=%v, want acquired", first, err)
	}
	second, err := store.TryResponseLease(context.Background(), "key", "owner-2", time.Minute)
	if err != nil || second {
		t.Fatalf("second lease = %v err=%v, want blocked", second, err)
	}
	if err := store.ReleaseResponseLease(context.Background(), "key", "owner-1"); err != nil {
		t.Fatal(err)
	}
	third, err := store.TryResponseLease(context.Background(), "key", "owner-2", time.Minute)
	if err != nil || !third {
		t.Fatalf("third lease = %v err=%v, want acquired after release", third, err)
	}
}

func TestBindAuthClearsPrivateDataKeepsManifests(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "responses.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.BindAuth(ctx, "fp-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.PutResponse(ctx, "key", "flickr.photos.getNotInSet", []byte("private"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.PutPhotoMetadata(ctx, api.PhotoMetadata{ID: "1", URL: "https://example.invalid/1.jpg"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.TryResponseLease(ctx, "lease", "owner-1", time.Hour); err != nil || !ok {
		t.Fatalf("lease = %v err=%v, want acquired", ok, err)
	}
	if err := store.PutPhotosetStatus(ctx, PhotosetStatus{
		RootDir: "/photos", OwnerNSID: "owner", PhotosetID: "set", Title: "Album",
		Directory: "/photos/owner/Album", ExpectedIDs: []string{"1"}, Complete: true,
	}); err != nil {
		t.Fatal(err)
	}

	if err := store.BindAuth(ctx, "fp-a"); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := store.Get(ctx, "key"); err != nil || !found {
		t.Fatalf("same-token response found=%v err=%v, want retained", found, err)
	}

	if err := store.BindAuth(ctx, "fp-b"); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := store.Get(ctx, "key"); err != nil || found {
		t.Fatalf("response found=%v err=%v, want cleared", found, err)
	}
	meta, err := store.GetPhotoMetadata(ctx, []string{"1"})
	if err != nil || len(meta) != 0 {
		t.Fatalf("metadata = %+v err=%v, want empty", meta, err)
	}
	if ok, err := store.TryResponseLease(ctx, "lease", "owner-2", time.Minute); err != nil || !ok {
		t.Fatalf("lease after bind = %v err=%v, want free", ok, err)
	}
	status, err := store.GetPhotosetStatus(ctx, "/photos", "owner", "set")
	if err != nil || status == nil || status.Title != "Album" {
		t.Fatalf("status = %+v err=%v, want retained manifest", status, err)
	}
}

func TestOpenMigratesV6ToV7(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v6.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		`INSERT INTO schema_version(version) VALUES(6)`,
		`CREATE TABLE responses (cache_key TEXT PRIMARY KEY, body BLOB NOT NULL, fetched_at INTEGER NOT NULL, method TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO responses(cache_key, body, fetched_at, method) VALUES('legacy', 'body', 123, 'flickr.photos.getInfo')`,
		`CREATE TABLE photoset_status (
			root_dir TEXT NOT NULL, owner_nsid TEXT NOT NULL, photoset_id TEXT NOT NULL,
			title TEXT NOT NULL, directory TEXT NOT NULL, expected_ids TEXT NOT NULL,
			complete INTEGER NOT NULL, source_updated_at INTEGER NOT NULL DEFAULT 0,
			file_sizes TEXT NOT NULL DEFAULT '{}', verified_at INTEGER NOT NULL,
			PRIMARY KEY (root_dir, owner_nsid, photoset_id)
		)`,
		`CREATE TABLE photo_metadata (
			photo_id TEXT PRIMARY KEY, url TEXT NOT NULL, media TEXT NOT NULL,
			original_format TEXT NOT NULL, extension TEXT NOT NULL,
			o_width INTEGER NOT NULL, o_height INTEGER NOT NULL,
			size_bytes INTEGER NOT NULL, updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE response_leases (cache_key TEXT PRIMARY KEY, owner TEXT NOT NULL, expires_at INTEGER NOT NULL)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	body, fetchedAt, found, err := store.Get(context.Background(), "legacy")
	if err != nil || !found || string(body) != "body" || fetchedAt.Unix() != 123 {
		t.Fatalf("migrated response body=%q fetched=%v found=%v err=%v", body, fetchedAt, found, err)
	}
	var version int
	if err := store.db.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, currentSchemaVersion)
	}
	var metaCount int
	if err := store.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'cache_meta'").Scan(&metaCount); err != nil || metaCount != 1 {
		t.Fatalf("cache_meta count = %d err=%v, want table", metaCount, err)
	}
}
