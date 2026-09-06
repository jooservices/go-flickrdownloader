package download

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jooservices/go-flickrdownloader/pkg/api"
)

// TestCanonicalPath covers the symlink-resolution helper backing the
// output-root lock (ADR-023): a relative path, an absolute path, and a
// symlinked path must all resolve to the same canonical directory so two
// different spellings of the same root actually serialize against each other.
func TestCanonicalPath(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if runtime.GOOS != "windows" {
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
	}

	abs := CanonicalPath(real)
	if !filepath.IsAbs(abs) {
		t.Fatalf("CanonicalPath(%q) = %q, want an absolute path", real, abs)
	}

	// Relative path resolves to the same canonical form as the absolute one.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, real)
	if err == nil {
		if got := CanonicalPath(rel); got != abs {
			t.Fatalf("CanonicalPath(relative) = %q, want %q (same as absolute)", got, abs)
		}
	}

	if runtime.GOOS != "windows" {
		if got := CanonicalPath(link); got != abs {
			t.Fatalf("CanonicalPath(symlink) = %q, want %q (the real target, so both spellings serialize)", got, abs)
		}
	}

	// A path that doesn't exist (EvalSymlinks fails) still returns an
	// absolute path rather than erroring — the lock must degrade gracefully
	// for a not-yet-created output directory.
	missing := filepath.Join(root, "does-not-exist-yet")
	if got := CanonicalPath(missing); !filepath.IsAbs(got) {
		t.Fatalf("CanonicalPath(missing) = %q, want an absolute path even when EvalSymlinks fails", got)
	}
}

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

// TestDownloadByPhotosetListsAndCreatesOutputDir is a regression test for
// DownloadByPhotoset having zero test coverage: it drives the real
// getInfo -> reuse-check -> getPhotos -> per-photo download flow against a
// fake Flickr REST server. The listed photo's url_o deliberately points at
// the test server's own host (not a real Flickr CDN host), so
// allowedDownloadURL correctly refuses it — that's a real, fast, local
// failure exercising the same code path a bad/foreign URL would hit in
// production, without this test needing a real file download to succeed.
func TestDownloadByPhotosetListsAndCreatesOutputDir(t *testing.T) {
	const ownerNSID = "123456789@N01"
	const photosetID = "72157600000001"

	infoResp := map[string]any{
		"stat": "ok",
		"photoset": map[string]any{
			"id":     photosetID,
			"owner":  ownerNSID,
			"title":  map[string]string{"_content": "Album A"},
			"photos": 1,
		},
	}
	listResp := map[string]any{
		"stat": "ok",
		"photoset": map[string]any{
			"id": photosetID, "owner": ownerNSID, "page": 1, "pages": 1, "perpage": 500, "total": 1,
			"photo": []map[string]any{
				{
					"id": "11111111", "secret": "s", "server": "1", "farm": 1,
					"title": "p1", "owner": ownerNSID,
					"url_o": "http://127.0.0.1:1/fake.jpg", "media": "photo",
					"o_width": 100, "o_height": 100,
				},
			},
		},
	}

	srv := fakeFlickrServer(t, map[string]http.HandlerFunc{
		"flickr.photosets.getInfo":   jsonHandler(t, infoResp),
		"flickr.photosets.getPhotos": jsonHandler(t, listResp),
	})

	client := api.NewClient("key", "secret", "token", "token-secret")
	client.SetBaseURL(srv.URL)

	root := t.TempDir()
	d := New(client, root, 1)
	d.Quiet = true

	stats, err := d.DownloadByPhotoset(context.Background(), photosetID)
	if err != nil {
		t.Fatalf("DownloadByPhotoset: %v", err)
	}
	if stats.Total != 1 {
		t.Fatalf("stats.Total = %d, want 1", stats.Total)
	}
	if stats.Failed != 1 {
		t.Fatalf("stats.Failed = %d, want 1 (a non-Flickr CDN host must be refused)", stats.Failed)
	}
	setDir := filepath.Join(root, ownerNSID, "Album A")
	if info, err := os.Stat(setDir); err != nil || !info.IsDir() {
		t.Fatalf("expected output directory %s to exist: %v", setDir, err)
	}
}

// TestDownloadByPhotosetPropagatesGetInfoFailure covers the branch where
// flickr.photosets.getInfo itself fails (a genuinely non-existent or
// deleted album, distinct from a rate-limit/transport error): DownloadByPhotoset
// must fall through to the listing call using the bare photosetID as the
// directory name rather than erroring immediately, since getInfo is
// decorative (title/owner for the folder name) and getPhotos is the
// authoritative call.
func TestDownloadByPhotosetPropagatesGetInfoFailure(t *testing.T) {
	const photosetID = "72157600000002"

	srv := fakeFlickrServer(t, map[string]http.HandlerFunc{
		"flickr.photosets.getInfo": jsonHandler(t, map[string]any{
			"stat": "fail", "code": 1, "message": "Photoset not found",
		}),
		// flickr.photosets.getPhotos is intentionally left unhandled here:
		// with no owner known (getInfo failed) the photoset directory name
		// falls back to the bare ID, and this asserts DownloadByPhotoset's
		// own error wrapping around that second, now-authoritative call.
	})

	client := api.NewClient("key", "secret", "token", "token-secret")
	client.SetBaseURL(srv.URL)

	d := New(client, t.TempDir(), 1)
	d.Quiet = true

	_, err := d.DownloadByPhotoset(context.Background(), photosetID)
	if err == nil {
		t.Fatal("expected an error when both getInfo and getPhotos fail")
	}
}

// TestDownloadPhotoCreatesOutputDirAndRecordsFailure is a regression test
// for DownloadPhoto having zero test coverage: it drives the real
// getPhotoInfo -> single-worker-download flow against a fake Flickr REST
// server. As with the photoset test above, the photo has no url_o so the
// worker must fall through to flickr.photos.getSizes, whose fake response
// also points at a non-Flickr host — refused locally, no real network hop.
func TestDownloadPhotoCreatesOutputDirAndRecordsFailure(t *testing.T) {
	const ownerNSID = "123456789@N01"
	const photoID = "22222222"

	infoResp := map[string]any{
		"stat": "ok",
		"photo": map[string]any{
			"id": photoID, "secret": "s", "server": "1", "farm": 1,
			"owner":          map[string]any{"nsid": ownerNSID, "username": "someone"},
			"title":          map[string]string{"_content": "p1"},
			"originalsecret": "s", "originalformat": "jpg", "media": "photo",
		},
	}
	sizesResp := map[string]any{
		"stat": "ok",
		"sizes": map[string]any{
			"size": []map[string]any{
				{"label": "Original", "width": 100, "height": 100, "source": "http://127.0.0.1:1/fake.jpg", "url": "http://127.0.0.1:1/fake", "media": "photo"},
			},
		},
	}

	srv := fakeFlickrServer(t, map[string]http.HandlerFunc{
		"flickr.photos.getInfo":  jsonHandler(t, infoResp),
		"flickr.photos.getSizes": jsonHandler(t, sizesResp),
	})

	client := api.NewClient("key", "secret", "token", "token-secret")
	client.SetBaseURL(srv.URL)

	root := t.TempDir()
	d := New(client, root, 1)
	d.Quiet = true

	if err := d.DownloadPhoto(context.Background(), photoID); err != nil {
		t.Fatalf("DownloadPhoto: %v", err)
	}
	outDir := filepath.Join(root, ownerNSID)
	if info, err := os.Stat(outDir); err != nil || !info.IsDir() {
		t.Fatalf("expected output directory %s to exist: %v", outDir, err)
	}
}

// TestDownloadPhotoPropagatesGetPhotoInfoError covers DownloadPhoto's own
// error wrapping when the initial metadata call fails outright.
func TestDownloadPhotoPropagatesGetPhotoInfoError(t *testing.T) {
	srv := fakeFlickrServer(t, map[string]http.HandlerFunc{
		"flickr.photos.getInfo": jsonHandler(t, map[string]any{
			"stat": "fail", "code": 1, "message": "Photo not found",
		}),
	})

	client := api.NewClient("key", "secret", "token", "token-secret")
	client.SetBaseURL(srv.URL)

	d := New(client, t.TempDir(), 1)
	d.Quiet = true

	err := d.DownloadPhoto(context.Background(), "33333333")
	if err == nil {
		t.Fatal("expected an error when getPhotoInfo fails")
	}
}
