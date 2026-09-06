package download

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jooservices/go-flickrdownloader/pkg/ui"
)

func TestFailureLogRoundTripAndClear(t *testing.T) {
	root := t.TempDir()
	d := New(nil, root, 1)
	album := filepath.Join(root, "nsid", "Album")
	if err := os.MkdirAll(album, 0o755); err != nil {
		t.Fatal(err)
	}
	d.OutDir = album
	d.persistFailure("12345678", "https://live.staticflickr.com/a.jpg", "timeout")

	recs := d.loadFailures()
	if len(recs) != 1 || recs[0].ID != "12345678" || recs[0].Rel != "nsid/Album" {
		t.Fatalf("load = %+v", recs)
	}

	d.recordPhotoSuccess("12345678")
	if recs := d.loadFailures(); len(recs) != 0 {
		t.Fatalf("cleared log still has %v", recs)
	}
	if _, err := os.Stat(d.failuresPath()); !os.IsNotExist(err) {
		t.Fatal("empty log file should be removed")
	}
}

func TestLoadFailuresDropsCompletedAndEscaping(t *testing.T) {
	root := t.TempDir()
	d := New(nil, root, 1)
	writeFile(t, filepath.Join(root, "12345678.jpg"), []byte("ok"))
	lines := []failedRecord{
		{ID: "12345678", Rel: "", Err: "old"},
		{ID: "87654321", Rel: "../outside", Err: "bad"},
		{ID: "not-an-id", Rel: "", Err: "bad"},
		{ID: "11223344", Rel: "keep", Err: "timeout"},
	}
	var raw strings.Builder
	for _, rec := range lines {
		b, _ := json.Marshal(rec)
		raw.Write(b)
		raw.WriteByte('\n')
	}
	if err := os.WriteFile(d.failuresPath(), []byte(raw.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	recs := d.loadFailures()
	if len(recs) != 1 || recs[0].ID != "11223344" {
		t.Fatalf("load = %+v, want only 11223344", recs)
	}
}

func TestRetryFailedFirstDownloadsPersisted(t *testing.T) {
	root := t.TempDir()
	d := New(nil, root, 1)
	d.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return testHTTPResponse(r, http.StatusOK, "image/jpeg", "photo-bytes"), nil
	})}
	d.OutDir = root
	d.persistFailure("12345678", "https://live.staticflickr.com/12345678.jpg", "timeout")

	d.RetryFailedFirst(context.Background())

	got, err := os.ReadFile(filepath.Join(root, "12345678.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "photo-bytes" {
		t.Fatalf("got %q", got)
	}
	if recs := d.loadFailures(); len(recs) != 0 {
		t.Fatalf("success should clear log: %v", recs)
	}
}

func TestFormatFailuresGroupsReasons(t *testing.T) {
	got := ui.FormatFailures([]ui.FailEntry{
		{ID: "1", Err: "timeout"},
		{ID: "2", Err: "timeout"},
		{ID: "3", Err: "403"},
	})
	if !strings.Contains(got, "2×") || !strings.Contains(got, "timeout") || !strings.Contains(got, "403") {
		t.Fatalf("report = %q", got)
	}
}
