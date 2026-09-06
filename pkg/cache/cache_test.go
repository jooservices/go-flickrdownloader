package cache

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestWALSidecarPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "responses.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), "k", []byte(`{}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		p := path + suffix
		info, err := os.Stat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s permissions = %o, want 600", p, got)
		}
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

// TestRenewResponseLease covers ADR-022's cross-process cache-miss
// coordination guarantee (dead owner's lease expires within its TTL): only
// the current owner can extend an active lease, a wrong owner is refused,
// and an already-expired lease can no longer be renewed by anyone —
// matching TryResponseLease's own "an expired lease can be claimed" rule,
// so a stale lease doesn't linger past its TTL just because something still
// calls Renew on it.
func TestRenewResponseLease(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "responses.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	if ok, err := store.TryResponseLease(ctx, "key", "owner-1", time.Minute); err != nil || !ok {
		t.Fatalf("initial lease = %v err=%v, want acquired", ok, err)
	}

	// Wrong owner cannot renew.
	if ok, err := store.RenewResponseLease(ctx, "key", "owner-2", time.Minute); err != nil || ok {
		t.Fatalf("renew by wrong owner = %v err=%v, want refused", ok, err)
	}

	// Correct owner extends it.
	if ok, err := store.RenewResponseLease(ctx, "key", "owner-1", time.Hour); err != nil || !ok {
		t.Fatalf("renew by owner = %v err=%v, want extended", ok, err)
	}
	// A competing owner must still be blocked — proves the renewal actually
	// pushed expires_at forward rather than being a no-op success.
	if ok, err := store.TryResponseLease(ctx, "key", "owner-2", time.Minute); err != nil || ok {
		t.Fatalf("lease after renewal = %v err=%v, want still held by owner-1", ok, err)
	}

	// No lease at all for this key.
	if ok, err := store.RenewResponseLease(ctx, "missing-key", "owner-1", time.Minute); err != nil || ok {
		t.Fatalf("renew with no lease = %v err=%v, want refused", ok, err)
	}

	// An already-expired lease cannot be renewed by its former owner —
	// TryResponseLease already lets a new owner claim it once expired
	// (TestPhotoMetadataRoundTripAndLease/TestClearRemovesResponsesAndStatuses
	// cover that side); Renew must not let the old owner resurrect it
	// instead.
	if _, err := store.db.ExecContext(ctx,
		"UPDATE response_leases SET expires_at = ? WHERE cache_key = ?",
		time.Now().Add(-time.Minute).Unix(), "key"); err != nil {
		t.Fatalf("force-expire lease: %v", err)
	}
	if ok, err := store.RenewResponseLease(ctx, "key", "owner-1", time.Minute); err != nil || ok {
		t.Fatalf("renew of expired lease = %v err=%v, want refused", ok, err)
	}

	if ok, err := store.RenewResponseLease(ctx, "key", "owner-1", 0); err == nil || ok {
		t.Fatalf("renew with non-positive ttl = %v err=%v, want an error", ok, err)
	}
}

// TestBindAuthSameFingerprintIsNoop covers the short-circuit: binding the
// same fingerprint twice in a row must not re-clear private data — only an
// actual account change should trigger that.
func TestBindAuthSameFingerprintIsNoop(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "responses.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	if err := store.BindAuth(ctx, "fp-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.PutResponse(ctx, "key", "flickr.photos.getInfo", []byte("body"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAuth(ctx, "fp-a"); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := store.Get(ctx, "key"); err != nil || !found {
		t.Fatalf("found=%v err=%v, want retained across a same-fingerprint rebind", found, err)
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

// TestDetectSchemaVersion covers every branch of detectSchemaVersion
// directly: Open/migrate always converges to currentSchemaVersion in the
// same transaction that calls it (any version below current gets
// immediately updated), so the specific number detectSchemaVersion returns
// for a legacy database is not observable through Open's public behavior —
// only a direct, white-box call can verify it. Each step below adds one
// more table/column that a real historical version of this schema
// introduced, in order.
// makeReadOnly forces every write on store's connection to fail
// deterministically and portably (no OS file-permission quirks), via
// SQLite's own query_only pragma, so the write-error branches throughout
// this file can be exercised without a fault-injecting driver.
func makeReadOnly(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.db.Exec("PRAGMA query_only = 1"); err != nil {
		t.Fatalf("set query_only: %v", err)
	}
}

func TestPruneRejectsNonPositiveTTL(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "responses.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.Prune(ctx, time.Now(), 0, time.Hour); err == nil {
		t.Fatal("expected an error for a non-positive listing TTL")
	}
	if _, err := store.Prune(ctx, time.Now(), time.Hour, 0); err == nil {
		t.Fatal("expected an error for a non-positive detail TTL")
	}
}

// TestWriteErrorsPropagate covers the write-failure branch of every
// mutating Store method (as opposed to the "cache is not open" nil-receiver
// guard, already covered separately): a real SQLite write error must
// surface as a Go error, never be swallowed or panic.
func TestWriteErrorsPropagate(t *testing.T) {
	newStore := func(t *testing.T) *Store {
		t.Helper()
		store, err := Open(filepath.Join(t.TempDir(), "responses.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		return store
	}
	ctx := context.Background()

	t.Run("Put", func(t *testing.T) {
		store := newStore(t)
		makeReadOnly(t, store)
		if err := store.Put(ctx, "k", []byte("v"), time.Now()); err == nil {
			t.Fatal("expected a write error")
		}
	})
	t.Run("Prune", func(t *testing.T) {
		store := newStore(t)
		makeReadOnly(t, store)
		if _, err := store.Prune(ctx, time.Now(), time.Hour, time.Hour); err == nil {
			t.Fatal("expected a write error")
		}
	})
	t.Run("Clear", func(t *testing.T) {
		store := newStore(t)
		makeReadOnly(t, store)
		if _, err := store.Clear(ctx); err == nil {
			t.Fatal("expected a write error")
		}
	})
	t.Run("TryResponseLease", func(t *testing.T) {
		store := newStore(t)
		makeReadOnly(t, store)
		if _, err := store.TryResponseLease(ctx, "k", "o", time.Minute); err == nil {
			t.Fatal("expected a write error")
		}
	})
	t.Run("ReleaseResponseLease", func(t *testing.T) {
		store := newStore(t)
		makeReadOnly(t, store)
		if err := store.ReleaseResponseLease(ctx, "k", "o"); err == nil {
			t.Fatal("expected a write error")
		}
	})
	t.Run("RenewResponseLease", func(t *testing.T) {
		store := newStore(t)
		makeReadOnly(t, store)
		if _, err := store.RenewResponseLease(ctx, "k", "o", time.Minute); err == nil {
			t.Fatal("expected a write error")
		}
	})
	t.Run("PutPhotoMetadataBatch", func(t *testing.T) {
		store := newStore(t)
		makeReadOnly(t, store)
		if err := store.PutPhotoMetadataBatch(ctx, []api.PhotoMetadata{{ID: "1"}}); err == nil {
			t.Fatal("expected a write error")
		}
	})
	t.Run("Delete", func(t *testing.T) {
		store := newStore(t)
		makeReadOnly(t, store)
		if err := store.Delete(ctx, "k"); err == nil {
			t.Fatal("expected a write error")
		}
	})
	t.Run("DeletePhotoMetadata", func(t *testing.T) {
		store := newStore(t)
		makeReadOnly(t, store)
		if err := store.DeletePhotoMetadata(ctx, "1"); err == nil {
			t.Fatal("expected a write error")
		}
	})
	t.Run("PutPhotosetStatus", func(t *testing.T) {
		store := newStore(t)
		makeReadOnly(t, store)
		err := store.PutPhotosetStatus(ctx, PhotosetStatus{
			RootDir: "/photos", OwnerNSID: "owner", PhotosetID: "set", Directory: "/photos/owner/set",
		})
		if err == nil {
			t.Fatal("expected a write error")
		}
	})
	t.Run("BindAuth", func(t *testing.T) {
		store := newStore(t)
		if err := store.BindAuth(ctx, "fp-a"); err != nil {
			t.Fatal(err)
		}
		makeReadOnly(t, store)
		// A different fingerprint forces the write path (the same
		// fingerprint short-circuits before any write, covered separately).
		if err := store.BindAuth(ctx, "fp-b"); err == nil {
			t.Fatal("expected a write error")
		}
	})
}

// TestMigrateFailsWritingSchemaVersionTable covers migrate's very first
// write (creating the schema_version bookkeeping table itself) failing.
func TestMigrateFailsWritingSchemaVersionTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ro.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// Force file creation on disk before going read-only.
	if _, err := db.Exec("CREATE TABLE x(a)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}

	roDB, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer roDB.Close()
	if err := migrate(roDB); err == nil {
		t.Fatal("expected migrate to fail creating schema_version on a read-only database")
	}
}

// TestMigrateFailsUpdatingSchemaVersion covers migrate's final write: a
// structurally complete but version-stale database (every CREATE TABLE IF
// NOT EXISTS and column-exists check is a no-op requiring no write) still
// needs one real write — bumping schema_version — which this proves fails
// cleanly when the database is read-only, rather than silently reporting
// success at a version the on-disk row was never actually updated to.
func TestMigrateFailsUpdatingSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ro.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE schema_version SET version = 6"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}

	roDB, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer roDB.Close()
	if err := migrate(roDB); err == nil {
		t.Fatal("expected migrate to fail updating a stale schema_version on a read-only database")
	}
}

// TestMigrateFailsAtEachWriteStep drives migrate through each of its
// write statements in turn: pre-stage the database exactly one step short
// of that write (every earlier write already applied, so it won't be
// reached), make it read-only, and confirm that specific write's failure
// surfaces with the expected wrapped message rather than a later step's.
func TestMigrateFailsAtEachWriteStep(t *testing.T) {
	cases := []struct {
		name    string
		wantErr string
		setup   []string // applied on top of schema_version(version) via a writable connection
	}{
		{
			name:    "create responses table",
			wantErr: "create response schema",
			setup:   nil,
		},
		{
			name:    "add method column",
			wantErr: "migrate response method column",
			setup: []string{
				`CREATE TABLE responses (cache_key TEXT PRIMARY KEY, body BLOB NOT NULL, fetched_at INTEGER NOT NULL)`,
			},
		},
		{
			name:    "create photoset_status table",
			wantErr: "create photoset status schema",
			setup: []string{
				`CREATE TABLE responses (cache_key TEXT PRIMARY KEY, body BLOB NOT NULL, fetched_at INTEGER NOT NULL, method TEXT NOT NULL DEFAULT '')`,
			},
		},
		{
			name:    "add source_updated_at column",
			wantErr: "migrate photoset source timestamp",
			setup: []string{
				`CREATE TABLE responses (cache_key TEXT PRIMARY KEY, body BLOB NOT NULL, fetched_at INTEGER NOT NULL, method TEXT NOT NULL DEFAULT '')`,
				`CREATE TABLE photoset_status (root_dir TEXT NOT NULL, owner_nsid TEXT NOT NULL, photoset_id TEXT NOT NULL, title TEXT NOT NULL, directory TEXT NOT NULL, expected_ids TEXT NOT NULL, complete INTEGER NOT NULL, verified_at INTEGER NOT NULL)`,
			},
		},
		{
			name:    "create photo_metadata table",
			wantErr: "create photo metadata schema",
			setup: []string{
				`CREATE TABLE responses (cache_key TEXT PRIMARY KEY, body BLOB NOT NULL, fetched_at INTEGER NOT NULL, method TEXT NOT NULL DEFAULT '')`,
				`CREATE TABLE photoset_status (root_dir TEXT NOT NULL, owner_nsid TEXT NOT NULL, photoset_id TEXT NOT NULL, title TEXT NOT NULL, directory TEXT NOT NULL, expected_ids TEXT NOT NULL, complete INTEGER NOT NULL, source_updated_at INTEGER NOT NULL DEFAULT 0, file_sizes TEXT NOT NULL DEFAULT '{}', verified_at INTEGER NOT NULL)`,
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ro.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("CREATE TABLE schema_version (version INTEGER NOT NULL)"); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("INSERT INTO schema_version(version) VALUES(1)"); err != nil {
				t.Fatal(err)
			}
			for _, stmt := range c.setup {
				if _, err := db.Exec(stmt); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o444); err != nil {
				t.Fatal(err)
			}

			roDB, err := sql.Open("sqlite", path+"?mode=ro")
			if err != nil {
				t.Fatal(err)
			}
			defer roDB.Close()
			err = migrate(roDB)
			if err == nil {
				t.Fatal("expected a write error")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, c.wantErr)
			}
		})
	}
}

func TestOpenFailsWhenDatabaseFileIsUnwritable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unwritable.db")
	if err := os.WriteFile(path, nil, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("expected an error opening an unwritable database file")
	}
}

// TestOpenFailsWhenParentDirIsAFile covers Open's MkdirAll error path: a
// path component that already exists as a regular file, not a directory.
func TestOpenFailsWhenParentDirIsAFile(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(blocker, "cache.db")); err == nil {
		t.Fatal("expected an error when a path component is a file, not a directory")
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("expected an error for an empty path")
	}
}

func TestNilStoreCloseIsNoop(t *testing.T) {
	var store *Store
	if err := store.Close(); err != nil {
		t.Fatalf("Close on a nil store: %v, want nil", err)
	}
}

// TestNilStoreMethodsReturnClosedError covers the "cache is not open" guard
// present at the top of every Store method — a nil *Store (an unopened or
// failed-to-open cache, which callers throughout this codebase treat as an
// optional optimization rather than a fatal error) must return a clear
// error, never panic.
func TestNilStoreMethodsReturnClosedError(t *testing.T) {
	var store *Store
	ctx := context.Background()

	if _, _, _, err := store.Get(ctx, "k"); err == nil {
		t.Error("Get: expected an error on a nil store")
	}
	if err := store.Put(ctx, "k", nil, time.Now()); err == nil {
		t.Error("Put: expected an error")
	}
	if err := store.PutResponse(ctx, "k", "m", nil, time.Now()); err == nil {
		t.Error("PutResponse: expected an error")
	}
	if _, err := store.Prune(ctx, time.Now(), time.Hour, time.Hour); err == nil {
		t.Error("Prune: expected an error")
	}
	if _, err := store.Clear(ctx); err == nil {
		t.Error("Clear: expected an error")
	}
	if _, err := store.TryResponseLease(ctx, "k", "o", time.Minute); err == nil {
		t.Error("TryResponseLease: expected an error")
	}
	if err := store.ReleaseResponseLease(ctx, "k", "o"); err == nil {
		t.Error("ReleaseResponseLease: expected an error")
	}
	if _, err := store.RenewResponseLease(ctx, "k", "o", time.Minute); err == nil {
		t.Error("RenewResponseLease: expected an error")
	}
	if err := store.PutPhotoMetadata(ctx, api.PhotoMetadata{ID: "1"}); err == nil {
		t.Error("PutPhotoMetadata: expected an error")
	}
	if err := store.PutPhotoMetadataBatch(ctx, []api.PhotoMetadata{{ID: "1"}}); err == nil {
		t.Error("PutPhotoMetadataBatch: expected an error")
	}
	if _, err := store.GetPhotoMetadata(ctx, []string{"1"}); err == nil {
		t.Error("GetPhotoMetadata: expected an error")
	}
	if err := store.Delete(ctx, "k"); err == nil {
		t.Error("Delete: expected an error")
	}
	if err := store.DeletePhotoMetadata(ctx, "1"); err == nil {
		t.Error("DeletePhotoMetadata: expected an error")
	}
	if _, err := store.GetPhotosetStatus(ctx, "/root", "owner", "set"); err == nil {
		t.Error("GetPhotosetStatus: expected an error")
	}
	if _, err := store.GetPhotosetStatuses(ctx, "/root", "owner"); err == nil {
		t.Error("GetPhotosetStatuses: expected an error")
	}
	if err := store.PutPhotosetStatus(ctx, PhotosetStatus{}); err == nil {
		t.Error("PutPhotosetStatus: expected an error")
	}
	if err := store.BindAuth(ctx, "fp"); err == nil {
		t.Error("BindAuth: expected an error")
	}
}

// TestDeleteAndDeletePhotoMetadata covers the direct delete paths (as
// opposed to bulk removal via Clear, already covered elsewhere).
func TestDeleteAndDeletePhotoMetadata(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "responses.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	if err := store.Put(ctx, "key", []byte(`{}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "key"); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := store.Get(ctx, "key"); err != nil || found {
		t.Fatalf("found=%v err=%v, want deleted", found, err)
	}

	if err := store.PutPhotoMetadata(ctx, api.PhotoMetadata{ID: "1", URL: "https://example.invalid/1.jpg"}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePhotoMetadata(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	meta, err := store.GetPhotoMetadata(ctx, []string{"1"})
	if err != nil || len(meta) != 0 {
		t.Fatalf("metadata = %+v err=%v, want empty after delete", meta, err)
	}
}

// TestPutPhotoMetadataBatchMergesWithoutClobbering covers the upsert's CASE
// WHEN merge logic: a later write with blank/zero fields must not overwrite
// already-recorded good values, and an empty batch is a no-op.
func TestPutPhotoMetadataBatchMergesWithoutClobbering(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "responses.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	if err := store.PutPhotoMetadataBatch(ctx, nil); err != nil {
		t.Fatalf("empty batch: %v", err)
	}

	full := api.PhotoMetadata{
		ID: "1", URL: "https://example.invalid/1.jpg", Media: "photo",
		OriginalFormat: "jpg", Extension: "jpg", OWidth: 100, OHeight: 80, SizeBytes: 1234,
	}
	if err := store.PutPhotoMetadataBatch(ctx, []api.PhotoMetadata{full}); err != nil {
		t.Fatal(err)
	}

	// A later write for the same ID with blank/zero fields (e.g. a partial
	// re-enrichment) must preserve the previously recorded good values.
	sparse := api.PhotoMetadata{ID: "1", URL: "https://example.invalid/1.jpg"}
	if err := store.PutPhotoMetadataBatch(ctx, []api.PhotoMetadata{sparse}); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetPhotoMetadata(ctx, []string{"1"})
	if err != nil {
		t.Fatal(err)
	}
	if got["1"].Media != "photo" || got["1"].SizeBytes != 1234 || got["1"].OWidth != 100 {
		t.Fatalf("metadata after sparse merge = %+v, want the original values preserved", got["1"])
	}
}

// TestGetPhotoMetadataPaginatesOverFiveHundredIDs covers GetPhotoMetadata's
// internal chunking for a query too large for one SQLite IN() clause.
func TestGetPhotoMetadataPaginatesOverFiveHundredIDs(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "responses.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	const n = 600
	ids := make([]string, n)
	records := make([]api.PhotoMetadata, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%08d", i)
		ids[i] = id
		records[i] = api.PhotoMetadata{ID: id, URL: "https://example.invalid/" + id + ".jpg", Media: "photo"}
	}
	if err := store.PutPhotoMetadataBatch(ctx, records); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetPhotoMetadata(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("got %d records, want %d (pagination must not drop any)", len(got), n)
	}
}

func TestTableColumnsRejectsUnknownTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cols.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tableColumns(tx, "not_a_real_table"); err == nil {
		t.Fatal("expected an error for a table outside the known allowlist")
	}
}

// TestRequireColumnsRejectsMissingColumn is a white-box test for a defect
// requireColumns exists to catch: a table present but missing a column
// migrate is supposed to guarantee. Not reachable through Open/migrate in
// this codebase's normal operation (migrate always adds what it checks
// immediately before checking it) — this exercises requireColumns directly.
func TestRequireColumnsRejectsMissingColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cols.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE responses (cache_key TEXT PRIMARY KEY, body BLOB NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := requireColumns(tx, "responses", []string{"cache_key", "body", "fetched_at"}); err == nil {
		t.Fatal("expected an error for a missing column")
	}
	if err := requireColumns(tx, "responses", []string{"cache_key", "body"}); err != nil {
		t.Fatalf("present columns should not error: %v", err)
	}
}

func TestDetectSchemaVersion(t *testing.T) {
	steps := []string{
		`CREATE TABLE responses (cache_key TEXT PRIMARY KEY, body BLOB NOT NULL, fetched_at INTEGER NOT NULL)`,
		`ALTER TABLE responses ADD COLUMN method TEXT NOT NULL DEFAULT ''`,
		`CREATE TABLE photoset_status (root_dir TEXT NOT NULL, owner_nsid TEXT NOT NULL, photoset_id TEXT NOT NULL, title TEXT NOT NULL, directory TEXT NOT NULL, expected_ids TEXT NOT NULL, complete INTEGER NOT NULL, verified_at INTEGER NOT NULL)`,
		`CREATE TABLE photo_metadata (photo_id TEXT PRIMARY KEY, url TEXT NOT NULL, media TEXT NOT NULL, original_format TEXT NOT NULL, extension TEXT NOT NULL, o_width INTEGER NOT NULL, o_height INTEGER NOT NULL, size_bytes INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE response_leases (cache_key TEXT PRIMARY KEY, owner TEXT NOT NULL, expires_at INTEGER NOT NULL)`,
		`ALTER TABLE photoset_status ADD COLUMN source_updated_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE photoset_status ADD COLUMN file_sizes TEXT NOT NULL DEFAULT '{}'`,
		`CREATE TABLE cache_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	}
	// wantAfterStep[i] is detectSchemaVersion's result after applying
	// steps[:i] — i.e. wantAfterStep[0] is the fresh-database case.
	wantAfterStep := []int{0, 1, 1, 2, 3, 4, 5, 6, currentSchemaVersion}

	for i, want := range wantAfterStep {
		t.Run(fmt.Sprintf("after_%d_steps", i), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "detect.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			defer db.Close()
			for _, stmt := range steps[:i] {
				if _, err := db.Exec(stmt); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
			}
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			got, err := detectSchemaVersion(tx)
			if err != nil {
				t.Fatalf("detectSchemaVersion: %v", err)
			}
			if got != want {
				t.Fatalf("after %d step(s): version = %d, want %d", i, got, want)
			}
		})
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
