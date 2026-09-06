package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jooservices/go-flickrdownloader/pkg/cache"
	"github.com/jooservices/go-flickrdownloader/pkg/config"
)

// fakeFlickrServer answers the REST methods a test needs, dispatching on the
// "method" query parameter the way the real Flickr REST endpoint does.
func fakeFlickrServer(t *testing.T, handlers map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Query().Get("method")
		h, ok := handlers[method]
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"stat":"fail","code":1,"message":"unhandled method %s"}`, method)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func jsonHandler(t *testing.T, v any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(v); err != nil {
			t.Errorf("encode fake response: %v", err)
		}
	}
}

// TestRunWatchAddListRemove drives the local (network-free) watchlist
// commands end-to-end against a --file watchlist in a temp dir.
func TestRunWatchAddListRemove(t *testing.T) {
	origWatchFile := watchFile
	defer func() { watchFile = origWatchFile }()
	watchFile = filepath.Join(t.TempDir(), "watchlist.yaml")

	const url = "https://www.flickr.com/photos/alice/"

	out := captureStdout(t, func() {
		if err := runWatchAdd(nil, []string{url}); err != nil {
			t.Fatalf("runWatchAdd: %v", err)
		}
	})
	if !strings.Contains(out, "added") {
		t.Fatalf("runWatchAdd output = %q, want it to report the addition", out)
	}

	out = captureStdout(t, func() {
		if err := runWatchList(nil, nil); err != nil {
			t.Fatalf("runWatchList: %v", err)
		}
	})
	if !strings.Contains(out, "alice") {
		t.Fatalf("runWatchList output = %q, want the added URL listed", out)
	}

	out = captureStdout(t, func() {
		if err := runWatchAdd(nil, []string{url}); err != nil {
			t.Fatalf("runWatchAdd (duplicate): %v", err)
		}
	})
	if !strings.Contains(out, "no new URLs") {
		t.Fatalf("runWatchAdd duplicate output = %q, want the no-new-URLs message", out)
	}

	out = captureStdout(t, func() {
		if err := runWatchRemove(nil, []string{url}); err != nil {
			t.Fatalf("runWatchRemove: %v", err)
		}
	})
	if !strings.Contains(out, "removed") {
		t.Fatalf("runWatchRemove output = %q, want it to report the removal", out)
	}

	out = captureStdout(t, func() {
		if err := runWatchRemove(nil, []string{url}); err != nil {
			t.Fatalf("runWatchRemove (already gone): %v", err)
		}
	})
	if !strings.Contains(out, "none matched") {
		t.Fatalf("runWatchRemove (already gone) output = %q, want the none-matched message", out)
	}

	out = captureStdout(t, func() {
		if err := runWatchList(nil, nil); err != nil {
			t.Fatalf("runWatchList (empty): %v", err)
		}
	})
	if !strings.Contains(out, "no sources") {
		t.Fatalf("runWatchList (empty) output = %q, want the no-sources message", out)
	}
}

func newTestConfigForRunCmds(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	cfg := &config.Config{APIKey: "key", APISecret: "secret", OAuthToken: "token", OAuthSecret: "token-secret"}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
}

// TestRunCachePruneAndClear drives both cache-management commands against a
// real (temp, HOME-redirected) cache database seeded with an expired entry.
func TestRunCachePruneAndClear(t *testing.T) {
	newTestConfigForRunCmds(t)

	loaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	path, err := config.CachePath(loaded.APIKey, loaded.NSID)
	if err != nil {
		t.Fatal(err)
	}
	store, err := cache.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutResponse(context.Background(), "old", "flickr.photos.getSizes", []byte("x"), time.Now().Add(-40*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if err := runCachePrune(nil, nil); err != nil {
			t.Fatalf("runCachePrune: %v", err)
		}
	})
	if !strings.Contains(out, "Pruned") {
		t.Fatalf("runCachePrune output = %q, want it to report what was pruned", out)
	}

	out = captureStdout(t, func() {
		if err := runCacheClear(nil, nil); err != nil {
			t.Fatalf("runCacheClear: %v", err)
		}
	})
	if !strings.Contains(out, "Cleared") {
		t.Fatalf("runCacheClear output = %q, want it to report what was cleared", out)
	}
}

func TestRunQuotaReportsUsage(t *testing.T) {
	newTestConfigForRunCmds(t)

	out := captureStdout(t, func() {
		if err := runQuota(nil, nil); err != nil {
			t.Fatalf("runQuota: %v", err)
		}
	})
	if !strings.Contains(out, "3600") {
		t.Fatalf("runQuota output = %q, want it to mention the hourly limit", out)
	}
}

// TestRunDownloadDryRunPhotoset drives runDownload end-to-end for the
// photoset-URL case (no LookupUser network call needed, unlike a user URL)
// against a fake Flickr server, in --dry-run mode so no actual file
// download is attempted.
func TestRunDownloadDryRunPhotoset(t *testing.T) {
	newTestConfigForRunCmds(t)

	srv := fakeFlickrServer(t, map[string]http.HandlerFunc{
		"flickr.photosets.getInfo": jsonHandler(t, map[string]any{
			"stat": "ok",
			"photoset": map[string]any{
				"id": "72157600000001", "owner": "123456789@N01",
				"title": map[string]string{"_content": "Album A"}, "photos": 1,
			},
		}),
	})
	origBaseURL := testAPIBaseURL
	defer func() { testAPIBaseURL = origBaseURL }()
	testAPIBaseURL = srv.URL

	origURL, origOutDir, origDryRun, origYes := urlFlag, outDir, dryRun, yesFlag
	defer func() { urlFlag, outDir, dryRun, yesFlag = origURL, origOutDir, origDryRun, origYes }()
	urlFlag = "https://www.flickr.com/photos/someuser/albums/72157600000001"
	outDir = t.TempDir()
	dryRun = true
	yesFlag = true

	out := captureStdout(t, func() {
		if err := runDownload(nil, nil); err != nil {
			t.Fatalf("runDownload: %v", err)
		}
	})
	if !strings.Contains(out, "Album A") {
		t.Fatalf("runDownload output = %q, want the resolved album title", out)
	}
	if !strings.Contains(out, "Dry run") {
		t.Fatalf("runDownload output = %q, want the dry-run notice", out)
	}
}

func TestRunDownloadRequiresURLFlag(t *testing.T) {
	origURL := urlFlag
	defer func() { urlFlag = origURL }()
	urlFlag = ""

	if err := runDownload(nil, nil); err == nil {
		t.Fatal("expected an error when -u is not provided")
	}
}

// TestRunVerifyUserURL drives runVerify end-to-end for a user URL (which
// needs LookupUser, unlike the photoset-URL runDownload test above)
// against a fake Flickr server, with no albums so it reports quickly.
func TestRunVerifyUserURL(t *testing.T) {
	newTestConfigForRunCmds(t)

	srv := fakeFlickrServer(t, map[string]http.HandlerFunc{
		"flickr.urls.lookupUser": jsonHandler(t, map[string]any{
			"stat": "ok",
			"user": map[string]any{"id": "123456789@N01", "username": map[string]string{"_content": "someuser"}},
		}),
		"flickr.photosets.getList": jsonHandler(t, map[string]any{
			"stat":      "ok",
			"photosets": map[string]any{"page": 1, "pages": 1, "perpage": 500, "total": 0, "photoset": []map[string]any{}},
		}),
	})
	origBaseURL := testAPIBaseURL
	defer func() { testAPIBaseURL = origBaseURL }()
	testAPIBaseURL = srv.URL

	origURL, origOutDir := urlFlag, outDir
	defer func() { urlFlag, outDir = origURL, origOutDir }()
	urlFlag = "https://www.flickr.com/photos/someuser/"
	outDir = t.TempDir()

	out := captureStdout(t, func() {
		if err := runVerify(nil, nil); err != nil {
			t.Fatalf("runVerify: %v", err)
		}
	})
	if !strings.Contains(out, "Verify") {
		t.Fatalf("runVerify output = %q, want the Verify banner", out)
	}
}

func TestRunVerifyRequiresURLFlag(t *testing.T) {
	origURL := urlFlag
	defer func() { urlFlag = origURL }()
	urlFlag = ""

	if err := runVerify(nil, nil); err == nil {
		t.Fatal("expected an error when -u is not provided")
	}
}

func TestRunScanUserURL(t *testing.T) {
	newTestConfigForRunCmds(t)

	srv := fakeFlickrServer(t, map[string]http.HandlerFunc{
		"flickr.urls.lookupUser": jsonHandler(t, map[string]any{
			"stat": "ok",
			"user": map[string]any{"id": "123456789@N01", "username": map[string]string{"_content": "someuser"}},
		}),
		"flickr.photosets.getList": jsonHandler(t, map[string]any{
			"stat":      "ok",
			"photosets": map[string]any{"page": 1, "pages": 1, "perpage": 500, "total": 0, "photoset": []map[string]any{}},
		}),
	})
	origBaseURL := testAPIBaseURL
	defer func() { testAPIBaseURL = origBaseURL }()
	testAPIBaseURL = srv.URL

	origURL, origOutDir := urlFlag, outDir
	defer func() { urlFlag, outDir = origURL, origOutDir }()
	urlFlag = "https://www.flickr.com/photos/someuser/"
	outDir = t.TempDir()

	out := captureStdout(t, func() {
		if err := runScan(nil, nil); err != nil {
			t.Fatalf("runScan: %v", err)
		}
	})
	if !strings.Contains(out, "Scan") {
		t.Fatalf("runScan output = %q, want the Scan banner", out)
	}
}

func TestRunScanRequiresURLFlag(t *testing.T) {
	origURL := urlFlag
	defer func() { urlFlag = origURL }()
	urlFlag = ""

	if err := runScan(nil, nil); err == nil {
		t.Fatal("expected an error when -u is not provided")
	}
}
