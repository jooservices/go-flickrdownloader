package download

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jooservices/flickrdownloader/pkg/api"
	"github.com/jooservices/flickrdownloader/pkg/cache"
)

func newTestDownloader(t *testing.T, rootDir string) *Downloader {
	t.Helper()
	store, err := cache.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	d := New(&api.Client{}, rootDir, 1)
	d.Cache = store
	return d
}

func writePhoto(t *testing.T, dir, id string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jpg"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestIndexLocalFilesFindsPhotosRecursively(t *testing.T) {
	root := t.TempDir()
	d := newTestDownloader(t, root)
	writePhoto(t, filepath.Join(root, "user1", "Album A"), "11111111")
	writePhoto(t, filepath.Join(root, "user1", "Album B"), "22222222")
	// In-progress artifacts must never count as present.
	if err := os.MkdirAll(filepath.Join(root, "user1", "Album A"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "user1", "Album A", "33333333.jpg.part"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := d.indexLocalFiles(root); err != nil {
		t.Fatal(err)
	}

	d.localFilesMu.Lock()
	defer d.localFilesMu.Unlock()
	if len(d.localFiles) != 2 {
		t.Fatalf("indexed %d files, want 2 (in-progress .part must be excluded): %v", len(d.localFiles), d.localFiles)
	}
	if _, ok := d.localFiles["11111111"]; !ok {
		t.Fatal("expected photo 11111111 to be indexed")
	}
	if _, ok := d.localFiles["33333333"]; ok {
		t.Fatal(".part artifact should not be indexed as present")
	}
}

func TestIndexLocalFilesIgnoresNonPhotoFilenames(t *testing.T) {
	root := t.TempDir()
	d := newTestDownloader(t, root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.indexLocalFiles(root); err != nil {
		t.Fatal(err)
	}
	d.localFilesMu.Lock()
	defer d.localFilesMu.Unlock()
	if len(d.localFiles) != 0 {
		t.Fatalf("indexed %d non-photo files, want 0: %v", len(d.localFiles), d.localFiles)
	}
}

func TestMissingPhotosetIDsReportsAbsentFiles(t *testing.T) {
	root := t.TempDir()
	setDir := filepath.Join(root, "user1", "Album A")
	d := newTestDownloader(t, root)
	writePhoto(t, setDir, "11111111")
	if err := d.indexLocalFiles(root); err != nil {
		t.Fatal(err)
	}

	missing := d.missingPhotosetIDs(setDir, []string{"11111111", "22222222"})
	if len(missing) != 1 || missing[0] != "22222222" {
		t.Fatalf("missing = %v, want [22222222]", missing)
	}
}

func TestMissingPhotosetIDsRejectsFileInWrongDirectory(t *testing.T) {
	root := t.TempDir()
	d := newTestDownloader(t, root)
	// Photo 11111111 exists, but under a different album than expected —
	// a stale manifest pointing at a renamed/moved directory should not
	// count it as present there.
	writePhoto(t, filepath.Join(root, "user1", "Album A"), "11111111")
	if err := d.indexLocalFiles(root); err != nil {
		t.Fatal(err)
	}

	otherDir := filepath.Join(root, "user1", "Album B")
	missing := d.missingPhotosetIDs(otherDir, []string{"11111111"})
	if len(missing) != 1 {
		t.Fatalf("missing = %v, want [11111111] (file is in a different directory)", missing)
	}
}

func TestLocalPhotosetManifestCompleteChecksFileSize(t *testing.T) {
	root := t.TempDir()
	setDir := filepath.Join(root, "user1", "Album A")
	d := newTestDownloader(t, root)
	writePhoto(t, setDir, "11111111") // writes 1 byte ("x")
	if err := d.indexLocalFiles(root); err != nil {
		t.Fatal(err)
	}

	complete := &cache.PhotosetStatus{
		ExpectedIDs: []string{"11111111"},
		FileSizes:   map[string]int64{"11111111": 1},
	}
	if !d.localPhotosetManifestComplete(complete) {
		t.Fatal("expected manifest to be complete: file present with matching size")
	}

	truncated := &cache.PhotosetStatus{
		ExpectedIDs: []string{"11111111"},
		FileSizes:   map[string]int64{"11111111": 999}, // recorded size doesn't match disk
	}
	if d.localPhotosetManifestComplete(truncated) {
		t.Fatal("expected manifest to be incomplete: on-disk size does not match the recorded size")
	}
}

func TestSaveAndLoadPhotosetStatusRoundTrips(t *testing.T) {
	root := t.TempDir()
	d := newTestDownloader(t, root)
	ctx := context.Background()

	status := cache.PhotosetStatus{
		RootDir: d.rootDir, OwnerNSID: "123@N01", PhotosetID: "999",
		Title: "Album A", Directory: filepath.Join(root, "123@N01", "Album A"),
		ExpectedIDs: []string{"11111111", "22222222"}, Complete: true,
	}
	d.savePhotosetStatus(ctx, status)

	statuses, ok := d.loadPhotosetStatuses(ctx, "123@N01")
	if !ok {
		t.Fatal("expected bulk load to succeed with a cache configured")
	}
	got, ok := statuses["999"]
	if !ok {
		t.Fatal("expected photoset 999 in the bulk-loaded statuses")
	}
	if !got.Complete || len(got.ExpectedIDs) != 2 {
		t.Fatalf("round-tripped status = %+v, want Complete=true with 2 expected IDs", got)
	}

	single := d.lookupPhotosetStatus(ctx, "123@N01", "999", nil, false)
	if single == nil || !single.Complete {
		t.Fatal("expected single lookup (non-bulk path) to also find the saved status")
	}
}

func TestLoadPhotosetStatusesWithoutCacheReturnsNotBulkLoaded(t *testing.T) {
	d := New(&api.Client{}, t.TempDir(), 1) // no Cache configured
	_, ok := d.loadPhotosetStatuses(context.Background(), "123@N01")
	if ok {
		t.Fatal("expected bulkLoaded=false when no cache is configured")
	}
}
