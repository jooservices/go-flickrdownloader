package download

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jooservices/flickrdownloader/pkg/api"
)

func TestScanUserWritesCompleteManifest(t *testing.T) {
	root := t.TempDir()
	owner := "123@N01"
	setDir := filepath.Join(root, owner, "Album A")
	d := newTestDownloader(t, root)
	writePhoto(t, setDir, "11111111")

	ctx := context.Background()
	sets := []api.PhotoSetInfo{{
		ID: "72157600000001",
		Title: struct {
			Content string `json:"_content"`
		}{Content: "Album A"},
		Photos:    1,
		UpdatedAt: 42,
	}}
	report, err := d.ScanUser(ctx, owner, sets)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Photosets) != 1 || !report.Photosets[0].Complete || report.Photosets[0].Present != 1 {
		t.Fatalf("report = %+v", report.Photosets)
	}

	got := d.lookupPhotosetStatus(ctx, owner, "72157600000001", nil, false)
	if got == nil || !got.Complete || got.SourceUpdatedAt != 42 || len(got.ExpectedIDs) != 1 {
		t.Fatalf("saved status = %+v", got)
	}
}

func TestScanUserIncompleteWhenCountsDiffer(t *testing.T) {
	root := t.TempDir()
	owner := "123@N01"
	d := newTestDownloader(t, root)
	writePhoto(t, filepath.Join(root, owner, "Album A"), "11111111")

	report, err := d.ScanUser(context.Background(), owner, []api.PhotoSetInfo{{
		ID: "72157600000001",
		Title: struct {
			Content string `json:"_content"`
		}{Content: "Album A"},
		Photos:    3,
		UpdatedAt: 42,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if report.Photosets[0].Complete {
		t.Fatal("expected incomplete when disk has fewer files than Flickr")
	}
}

func TestScanUserOfflineKeysByFolder(t *testing.T) {
	root := t.TempDir()
	owner := "123@N01"
	d := newTestDownloader(t, root)
	writePhoto(t, filepath.Join(root, owner, "Album A"), "11111111")

	report, err := d.ScanUser(context.Background(), owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Unmatched) != 1 || !report.Unmatched[0].Complete {
		t.Fatalf("unmatched = %+v", report.Unmatched)
	}
	got := d.lookupPhotosetStatus(context.Background(), owner, localPhotosetID("Album A"), nil, false)
	if got == nil || got.SourceUpdatedAt != 0 || !got.Complete {
		t.Fatalf("offline status = %+v, want complete with no date_update", got)
	}
}
