package download

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestDownloadPhotosFromPagesSweepGates(t *testing.T) {
	tests := []struct {
		name       string
		ctx        context.Context
		allowSweep bool
		fetchPage  func(context.Context, int) ([]api.Photo, error)
		wantStale  bool
	}{
		{
			name:       "clean album listing sweeps absent artifacts",
			ctx:        context.Background(),
			allowSweep: true,
			fetchPage: func(context.Context, int) ([]api.Photo, error) {
				return nil, nil
			},
		},
		{
			name:       "fetch error leaves artifacts intact",
			ctx:        context.Background(),
			allowSweep: true,
			fetchPage: func(context.Context, int) ([]api.Photo, error) {
				return nil, errors.New("page failed")
			},
			wantStale: true,
		},
		{
			name:       "cancelled listing leaves artifacts intact",
			ctx:        cancelledContext(),
			allowSweep: true,
			fetchPage: func(context.Context, int) ([]api.Photo, error) {
				t.Fatal("cancelled run must not fetch a page")
				return nil, nil
			},
			wantStale: true,
		},
		{
			name:       "owner root batch never sweeps",
			ctx:        context.Background(),
			allowSweep: false,
			fetchPage: func(context.Context, int) ([]api.Photo, error) {
				return nil, nil
			},
			wantStale: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			part := filepath.Join(dir, "absent.jpg.part")
			cand := filepath.Join(dir, "also-absent.jpg.cand")
			writeFile(t, part, []byte("partial"))
			writeFile(t, cand, []byte("candidate"))

			d := New(nil, dir, 1)
			d.downloadPhotosFromPages(tt.ctx, 0, 1, tt.allowSweep, false, tt.fetchPage)

			for _, path := range []string{part, cand} {
				if got := exists(path); got != tt.wantStale {
					t.Errorf("artifact %s exists = %v, want %v", filepath.Base(path), got, tt.wantStale)
				}
			}
		})
	}
}

// TestStartRendererStopWaitsForExit verifies startRenderer's stop() blocks
// until its goroutine has actually returned, not just until ctx is
// cancelled. Without that guarantee, mutating fields the renderer reads
// (d.progress, d.OutDir) right after stop() — as downloadPhotosFromPages
// does for the next album — races the goroutine's last in-flight tick.
func TestStartRendererStopWaitsForExit(t *testing.T) {
	d := New(nil, t.TempDir(), 1)
	d.progress = ui.NewProgress(1)

	ctx, cancel := context.WithCancel(context.Background())
	stop := d.startRenderer(ctx)
	cancel()
	stop()

	// If stop() returned before the goroutine exited, the renderer could
	// still be reading the old d.progress/d.OutDir here; -race would flag
	// these writes as racing that read.
	d.progress = ui.NewProgress(2)
	d.OutDir = t.TempDir()
}

// TestConsecutiveAlbumsDoNotRaceRenderer mirrors DownloadByUser's per-album
// loop: d.OutDir is reassigned and downloadPhotosFromPages is called again
// immediately after the previous call returns. The renderer goroutine
// started inside downloadPhotosFromPages must have fully exited by the time
// it returns, or this reassignment (and the next call's d.progress
// reassignment) races its reads. The artificial delay in fetchPage ensures
// the renderer's ticker actually fires at least once per call, so the race
// window is real rather than skipped by a fast return. Run with -race.
func TestConsecutiveAlbumsDoNotRaceRenderer(t *testing.T) {
	d := New(nil, t.TempDir(), 2)
	ctx := context.Background()
	fetchPage := func(context.Context, int) ([]api.Photo, error) {
		time.Sleep(200 * time.Millisecond)
		return nil, nil
	}

	for i := 0; i < 3; i++ {
		d.OutDir = t.TempDir()
		d.downloadPhotosFromPages(ctx, 0, 1, false, false, fetchPage)
	}
}

func TestCollectOrphanPhotosSkipsAlreadyDownloaded(t *testing.T) {
	firstPage := []api.Photo{{ID: "1"}, {ID: "2"}}
	downloaded := map[string]bool{"2": true}

	got := collectOrphanPhotos(1, firstPage, downloaded, func(page int) ([]api.Photo, error) {
		t.Fatalf("fetchPage should not be called for a single-page listing")
		return nil, nil
	})

	if len(got) != 1 || got[0].ID != "1" {
		t.Fatalf("got %+v, want only photo 1", got)
	}
}

// TestCollectOrphanPhotosReportsFailedPage verifies a page fetch error is
// surfaced (stderr message with the page number and cause, plus a closing
// warning) instead of being silently swallowed, while photos from pages
// that did succeed are still returned.
func TestCollectOrphanPhotosReportsFailedPage(t *testing.T) {
	firstPage := []api.Photo{{ID: "1"}}
	wantErr := errors.New("boom")

	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w

	got := collectOrphanPhotos(3, firstPage, nil, func(page int) ([]api.Photo, error) {
		switch page {
		case 2:
			return nil, wantErr
		case 3:
			return []api.Photo{{ID: "3"}}, nil
		default:
			t.Fatalf("unexpected page %d", page)
			return nil, nil
		}
	})

	w.Close()
	os.Stderr = origStderr
	out, _ := io.ReadAll(r)
	stderr := string(out)

	if len(got) != 2 || got[0].ID != "1" || got[1].ID != "3" {
		t.Fatalf("got %+v, want photos 1 and 3 (page 2 failed but shouldn't block the rest)", got)
	}
	if !strings.Contains(stderr, "page 2") || !strings.Contains(stderr, "boom") {
		t.Fatalf("stderr = %q, want it to mention the failed page and cause", stderr)
	}
	if !strings.Contains(stderr, "may be missing") {
		t.Fatalf("stderr = %q, want a closing warning that discovery was incomplete", stderr)
	}
}

func TestAlreadyDownloadedIgnoresIncompleteArtifacts(t *testing.T) {
	dir := t.TempDir()
	d := New(nil, dir, 1)

	for _, suffix := range []string{"jpg.part", "jpg.cand", "jpg.tmp"} {
		writeFile(t, filepath.Join(dir, "123."+suffix), []byte("incomplete"))
		if d.alreadyDownloaded("123") {
			t.Fatalf("123 should not be considered downloaded with only %s", suffix)
		}
		if err := os.Remove(filepath.Join(dir, "123."+suffix)); err != nil {
			t.Fatal(err)
		}
	}

	writeFile(t, filepath.Join(dir, "123.jpg"), []byte("complete"))
	if !d.alreadyDownloaded("123") {
		t.Fatal("123 should be considered downloaded when its final file exists")
	}
}

func TestWorkerRegistersPathOnlyAfterSuccessfulRenameAndRemovesSiblingPart(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "123.jpg.part")
	writeFile(t, part, []byte("stale partial"))

	d := New(nil, dir, 1)
	d.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Range") == "" {
			t.Error("expected resume request for existing .part")
		}
		return testHTTPResponse(r, http.StatusOK, "image/jpeg", "final bytes"), nil
	})}
	d.progress = ui.NewProgress(1)
	d.worker(context.Background(), api.Photo{ID: "123", URLOriginal: "https://example.invalid/123.jpg", Media: "photo"})

	final := filepath.Join(dir, "123.jpg")
	if !exists(final) {
		t.Fatal("expected successful download to be renamed to its final path")
	}
	if exists(part) {
		t.Fatal("expected sibling .part to be removed after candidate promotion")
	}
	if got := d.downloadedPaths["123"]; got != final {
		t.Fatalf("downloaded path = %q, want final path %q", got, final)
	}

	failDir := t.TempDir()
	failed := New(nil, failDir, 1)
	failed.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return testHTTPResponse(r, http.StatusForbidden, "", ""), nil
	})}
	failed.progress = ui.NewProgress(1)
	failed.worker(context.Background(), api.Photo{ID: "456", URLOriginal: "https://example.invalid/456.jpg", Media: "photo"})
	if _, ok := failed.downloadedPaths["456"]; ok {
		t.Fatal("failed download must not be registered for cross-album dedupe")
	}
}

func TestCancelledRunKeepsPartialCountsAndDoesNotReportFailure(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "skip.jpg")
	writeFile(t, existing, []byte("already downloaded"))

	linkedSource := filepath.Join(t.TempDir(), "link.jpg")
	writeFile(t, linkedSource, []byte("source"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := New(nil, dir, 1)
	d.downloadedPaths["link"] = linkedSource

	stats := d.downloadPhotosFromPages(ctx, 2, 2, true, false, func(_ context.Context, page int) ([]api.Photo, error) {
		if page == 1 {
			return []api.Photo{{ID: "skip"}, {ID: "link"}}, nil
		}
		cancel()
		return nil, context.Canceled
	})

	if stats.Skipped != 1 || stats.Linked != 1 || stats.Success != 0 {
		t.Fatalf("partial stats = %+v, want one skip and one link", stats)
	}
	if stats.Failed != 0 || len(d.progress.Stats().Failures) != 0 {
		t.Fatalf("cancelled run recorded failures: %+v", d.progress.Stats().Failures)
	}
}

func TestSweepPreservesEnqueuedIDArtifacts(t *testing.T) {
	dir := t.TempDir()
	queuedPart := filepath.Join(dir, "queued.jpg.part")
	writeFile(t, queuedPart, []byte("keep me"))
	// Canary: a .part whose photo ID is NOT in this run's listing must be
	// deleted by the sweep — proving the sweep actually ran (AC-017).
	canaryPart := filepath.Join(dir, "canary.jpg.part")
	writeFile(t, canaryPart, []byte("stale"))

	d := New(nil, dir, 1)
	d.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "active.jpg") {
			return &http.Response{
				StatusCode: 200,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": {"image/jpeg"}},
				Body:       io.NopCloser(strings.NewReader("PHOTO")),
				ContentLength: 5,
				Request:    r,
			}, nil
		}
		return &http.Response{
			StatusCode: 404,
			Status:     "404 Not Found",
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("gone")),
			Request:    r,
		}, nil
	})}

	stats := d.downloadPhotosFromPages(context.Background(), 2, 1, true, false, func(context.Context, int) ([]api.Photo, error) {
		return []api.Photo{
			{ID: "active", URLOriginal: "https://example.invalid/active.jpg", Media: "photo"},
			{ID: "queued", URLOriginal: "https://example.invalid/queued.jpg", Media: "photo"},
		}, nil
	})

	// Clean run (ctx not cancelled, allPagesOK): the sweep executes, but both
	// IDs were recorded at enqueue time, so neither artifact may be deleted.
	if !exists(queuedPart) {
		t.Fatal("artifact for an enqueued ID must survive the sweep (seenIDs recorded at enqueue)")
	}
	if exists(canaryPart) {
		t.Fatal("canary .part for a non-enqueued ID must be deleted by the sweep")
	}
	if stats.Success != 1 {
		t.Fatalf("stats.Success = %d, want 1", stats.Success)
	}
}

func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testHTTPResponse(r *http.Request, status int, contentType, body string) *http.Response {
	h := make(http.Header)
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	return &http.Response{
		StatusCode:    status,
		Header:        h,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: -1,
		Request:       r,
	}
}
