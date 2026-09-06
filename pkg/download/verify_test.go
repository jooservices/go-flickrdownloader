package download

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/jooservices/go-flickrdownloader/pkg/api"
	"github.com/jooservices/go-flickrdownloader/pkg/cache"
)

const okOwnerNSID = "123456789@N01"

func TestSortedIDs(t *testing.T) {
	got := sortedIDs(map[string]bool{"33333333": true, "11111111": true, "22222222": true})
	want := []string{"11111111", "22222222", "33333333"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestNewOrphanMembershipScopeDefaultsAllSetsToSelected(t *testing.T) {
	selected := []api.PhotoSetInfo{{ID: "1"}}
	scope := newOrphanMembershipScope(selected, nil)
	if len(scope.allSets) != 1 || scope.allSets[0].ID != "1" {
		t.Fatalf("allSets = %+v, want it to default to selectedSets", scope.allSets)
	}
}

func TestDeselectedSets(t *testing.T) {
	selected := []api.PhotoSetInfo{{ID: "1"}, {ID: "2"}}
	all := []api.PhotoSetInfo{{ID: "1"}, {ID: "2"}, {ID: "3"}}
	scope := newOrphanMembershipScope(selected, all)
	got := scope.deselectedSets()
	if len(got) != 1 || got[0].ID != "3" {
		t.Fatalf("deselectedSets = %+v, want only id 3", got)
	}
}

func TestCollectPhotosetMembership(t *testing.T) {
	srv := fakeFlickrServer(t, map[string]http.HandlerFunc{
		"flickr.photosets.getPhotos": jsonHandler(t, map[string]any{
			"stat": "ok",
			"photoset": map[string]any{
				"id": "72157600000001", "owner": okOwnerNSID, "page": 1, "pages": 1, "perpage": 500, "total": 1,
				"photo": []map[string]any{{"id": "11111111", "owner": okOwnerNSID}},
			},
		}),
	})
	client := api.NewClient("key", "secret", "token", "token-secret")
	client.SetBaseURL(srv.URL)
	d := New(client, t.TempDir(), 1)

	membership := map[string]bool{}
	if err := d.collectPhotosetMembership(context.Background(), "72157600000001", membership); err != nil {
		t.Fatalf("collectPhotosetMembership: %v", err)
	}
	if !membership["11111111"] {
		t.Fatalf("membership = %v, want 11111111 present", membership)
	}
}

func TestCollectPhotosetMembershipPropagatesError(t *testing.T) {
	srv := fakeFlickrServer(t, map[string]http.HandlerFunc{
		"flickr.photosets.getPhotos": jsonHandler(t, map[string]any{"stat": "fail", "code": 1, "message": "not found"}),
	})
	client := api.NewClient("key", "secret", "token", "token-secret")
	client.SetBaseURL(srv.URL)
	d := New(client, t.TempDir(), 1)

	if err := d.collectPhotosetMembership(context.Background(), "72157600000001", map[string]bool{}); err == nil {
		t.Fatal("expected an error for a Flickr-side failure")
	}
}

func TestMarkDeselectedAlbumPhotos(t *testing.T) {
	srv := fakeFlickrServer(t, map[string]http.HandlerFunc{
		"flickr.photosets.getPhotos": jsonHandler(t, map[string]any{
			"stat": "ok",
			"photoset": map[string]any{
				"id": "72157600000002", "owner": okOwnerNSID, "page": 1, "pages": 1, "perpage": 500, "total": 1,
				"photo": []map[string]any{{"id": "22222222", "owner": okOwnerNSID}},
			},
		}),
	})
	client := api.NewClient("key", "secret", "token", "token-secret")
	client.SetBaseURL(srv.URL)
	d := New(client, t.TempDir(), 1)

	scope := newOrphanMembershipScope(
		[]api.PhotoSetInfo{{ID: "72157600000001"}},
		[]api.PhotoSetInfo{{ID: "72157600000001"}, {ID: "72157600000002"}},
	)
	membership := map[string]bool{}
	if err := d.markDeselectedAlbumPhotos(context.Background(), scope, membership); err != nil {
		t.Fatalf("markDeselectedAlbumPhotos: %v", err)
	}
	if !membership["22222222"] {
		t.Fatalf("membership = %v, want the deselected album's photo present", membership)
	}
}

func TestRefreshPhotosetIDs(t *testing.T) {
	srv := fakeFlickrServer(t, map[string]http.HandlerFunc{
		"flickr.photosets.getPhotos": jsonHandler(t, map[string]any{
			"stat": "ok",
			"photoset": map[string]any{
				"id": "72157600000001", "owner": okOwnerNSID, "page": 1, "pages": 1, "perpage": 500, "total": 1,
				"photo": []map[string]any{{"id": "11111111", "owner": okOwnerNSID}},
			},
		}),
	})
	client := api.NewClient("key", "secret", "token", "token-secret")
	client.SetBaseURL(srv.URL)
	d := New(client, t.TempDir(), 1)

	ids, total, err := d.refreshPhotosetIDs(context.Background(), "72157600000001")
	if err != nil {
		t.Fatalf("refreshPhotosetIDs: %v", err)
	}
	if total != 1 || len(ids) != 1 || ids[0] != "11111111" {
		t.Fatalf("ids=%v total=%d, want [11111111]/1", ids, total)
	}
}

func TestRefreshUncategorizedIDsAuthenticatedUser(t *testing.T) {
	srv := fakeFlickrServer(t, map[string]http.HandlerFunc{
		"flickr.photos.getNotInSet": jsonHandler(t, map[string]any{
			"stat":   "ok",
			"photos": map[string]any{"page": 1, "pages": 1, "perpage": 500, "total": 1, "photo": []map[string]any{{"id": "33333333"}}},
		}),
	})
	client := api.NewClient("key", "secret", "token", "token-secret")
	client.SetBaseURL(srv.URL)
	d := New(client, t.TempDir(), 1)

	ids, err := d.refreshUncategorizedIDs(context.Background(), okOwnerNSID, map[string]bool{}, okOwnerNSID)
	if err != nil {
		t.Fatalf("refreshUncategorizedIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != "33333333" {
		t.Fatalf("ids = %v, want [33333333]", ids)
	}
}

func TestRefreshUncategorizedIDsOtherUserExcludesAlbumMembers(t *testing.T) {
	srv := fakeFlickrServer(t, map[string]http.HandlerFunc{
		"flickr.people.getPhotos": jsonHandler(t, map[string]any{
			"stat": "ok",
			"photos": map[string]any{"page": 1, "pages": 1, "perpage": 500, "total": 2, "photo": []map[string]any{
				{"id": "11111111"}, {"id": "44444444"},
			}},
		}),
	})
	client := api.NewClient("key", "secret", "token", "token-secret")
	client.SetBaseURL(srv.URL)
	d := New(client, t.TempDir(), 1)

	// Not the authenticated NSID, so this takes the getPhotosByUser branch;
	// 11111111 is already an album member and must be excluded.
	ids, err := d.refreshUncategorizedIDs(context.Background(), okOwnerNSID, map[string]bool{"11111111": true}, "someone-else@N01")
	if err != nil {
		t.Fatalf("refreshUncategorizedIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != "44444444" {
		t.Fatalf("ids = %v, want [44444444] (11111111 excluded as an album member)", ids)
	}
}

func TestVerifyUserNotScannedWithoutManifest(t *testing.T) {
	client := api.NewClient("key", "secret", "token", "token-secret")
	d := New(client, t.TempDir(), 1)
	store, err := cache.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	d.Cache = store

	sets := []api.PhotoSetInfo{{ID: "72157600000001", Title: struct {
		Content string `json:"_content"`
	}{Content: "Album A"}}}
	report, err := d.VerifyUser(context.Background(), okOwnerNSID, sets, false)
	if err != nil {
		t.Fatalf("VerifyUser: %v", err)
	}
	if len(report.Photosets) != 1 || report.Photosets[0].State != VerificationNotScanned {
		t.Fatalf("photosets = %+v, want a single not-scanned entry", report.Photosets)
	}
	if report.Uncategorized == nil || report.Uncategorized.State != VerificationNotScanned {
		t.Fatalf("uncategorized = %+v, want not-scanned", report.Uncategorized)
	}
}

func TestVerifyUserCompleteWithManifestAndFiles(t *testing.T) {
	root := t.TempDir()
	client := api.NewClient("key", "secret", "token", "token-secret")
	d := New(client, root, 1)
	store, err := cache.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	d.Cache = store

	setDir := filepath.Join(root, okOwnerNSID, "Album A")
	writePhoto(t, setDir, "11111111")
	if err := d.indexLocalFiles(root); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	d.savePhotosetStatus(ctx, cache.PhotosetStatus{
		RootDir: d.rootDir, OwnerNSID: okOwnerNSID, PhotosetID: "72157600000001",
		Title: "Album A", Directory: absolutePath(setDir), ExpectedIDs: []string{"11111111"},
		Complete: true, SourceUpdatedAt: 42,
	})

	sets := []api.PhotoSetInfo{{ID: "72157600000001", Photos: 1, UpdatedAt: 42, Title: struct {
		Content string `json:"_content"`
	}{Content: "Album A"}}}
	report, err := d.VerifyUser(ctx, okOwnerNSID, sets, false)
	if err != nil {
		t.Fatalf("VerifyUser: %v", err)
	}
	if len(report.Photosets) != 1 || report.Photosets[0].State != VerificationComplete {
		t.Fatalf("photosets = %+v, want complete", report.Photosets)
	}
}

func TestVerifyUserStaleDirectory(t *testing.T) {
	root := t.TempDir()
	client := api.NewClient("key", "secret", "token", "token-secret")
	d := New(client, root, 1)
	store, err := cache.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	d.Cache = store
	ctx := context.Background()

	// Manifest recorded against a different directory than the album
	// currently resolves to (e.g. the title changed).
	d.savePhotosetStatus(ctx, cache.PhotosetStatus{
		RootDir: d.rootDir, OwnerNSID: okOwnerNSID, PhotosetID: "72157600000001",
		Title: "Old Name", Directory: absolutePath(filepath.Join(root, okOwnerNSID, "Old Name")),
		ExpectedIDs: []string{"11111111"}, Complete: true,
	})

	sets := []api.PhotoSetInfo{{ID: "72157600000001", Title: struct {
		Content string `json:"_content"`
	}{Content: "New Name"}}}
	report, err := d.VerifyUser(ctx, okOwnerNSID, sets, false)
	if err != nil {
		t.Fatalf("VerifyUser: %v", err)
	}
	if len(report.Photosets) != 1 || report.Photosets[0].State != VerificationStale {
		t.Fatalf("photosets = %+v, want stale", report.Photosets)
	}
}

func TestVerifyUserRefreshComplete(t *testing.T) {
	root := t.TempDir()
	setDir := filepath.Join(root, okOwnerNSID, "Album A")
	writePhoto(t, setDir, "11111111")

	srv := fakeFlickrServer(t, map[string]http.HandlerFunc{
		"flickr.photosets.getPhotos": jsonHandler(t, map[string]any{
			"stat": "ok",
			"photoset": map[string]any{
				"id": "72157600000001", "owner": okOwnerNSID, "page": 1, "pages": 1, "perpage": 500, "total": 1,
				"photo": []map[string]any{{"id": "11111111", "owner": okOwnerNSID}},
			},
		}),
		"flickr.photos.getNotInSet": jsonHandler(t, map[string]any{
			"stat":   "ok",
			"photos": map[string]any{"page": 1, "pages": 1, "perpage": 500, "total": 0, "photo": []map[string]any{}},
		}),
	})
	client := api.NewClient("key", "secret", "token", "token-secret")
	client.SetBaseURL(srv.URL)
	d := New(client, root, 1)
	store, err := cache.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	d.Cache = store
	if err := d.indexLocalFiles(root); err != nil {
		t.Fatal(err)
	}

	sets := []api.PhotoSetInfo{{ID: "72157600000001", Title: struct {
		Content string `json:"_content"`
	}{Content: "Album A"}}}
	report, err := d.VerifyUser(context.Background(), okOwnerNSID, sets, true, VerifyUserOpts{AuthenticatedNSID: okOwnerNSID})
	if err != nil {
		t.Fatalf("VerifyUser: %v", err)
	}
	if len(report.Photosets) != 1 || report.Photosets[0].State != VerificationComplete {
		t.Fatalf("photosets = %+v, want complete", report.Photosets)
	}
	if !report.Photosets[0].Refreshed {
		t.Fatal("expected Refreshed=true")
	}
	if report.Uncategorized == nil || report.Uncategorized.State != VerificationComplete {
		t.Fatalf("uncategorized = %+v, want complete (no photos not in a set)", report.Uncategorized)
	}
}

func TestVerifyUserRefreshErrorState(t *testing.T) {
	srv := fakeFlickrServer(t, map[string]http.HandlerFunc{
		"flickr.photosets.getPhotos": jsonHandler(t, map[string]any{"stat": "fail", "code": 1, "message": "not found"}),
	})
	client := api.NewClient("key", "secret", "token", "token-secret")
	client.SetBaseURL(srv.URL)
	d := New(client, t.TempDir(), 1)

	sets := []api.PhotoSetInfo{{ID: "72157600000001", Title: struct {
		Content string `json:"_content"`
	}{Content: "Album A"}}}
	report, err := d.VerifyUser(context.Background(), okOwnerNSID, sets, true)
	if err != nil {
		t.Fatalf("VerifyUser: %v", err)
	}
	if len(report.Photosets) != 1 || report.Photosets[0].State != VerificationError {
		t.Fatalf("photosets = %+v, want error state", report.Photosets)
	}
}

// TestDownloadByUserDiscoversSetsAndDownloads drives DownloadByUser's full
// happy path: opts.Sets is nil (forcing the flickr.photosets.getList
// discovery call), one photoset with one photo, and orphans disabled to
// keep the fixture minimal. The photo's url_o points at a non-Flickr host
// so the download itself is refused locally and fast (as in the
// DownloadByPhotoset tests), while every listing/orchestration branch
// still runs for real.
func TestDownloadByUserDiscoversSetsAndDownloads(t *testing.T) {
	srv := fakeFlickrServer(t, map[string]http.HandlerFunc{
		"flickr.photosets.getList": jsonHandler(t, map[string]any{
			"stat": "ok",
			"photosets": map[string]any{
				"page": 1, "pages": 1, "perpage": 500, "total": 1,
				"photoset": []map[string]any{{
					"id": "72157600000001", "owner": okOwnerNSID,
					"title": map[string]string{"_content": "Album A"}, "photos": 1,
				}},
			},
		}),
		"flickr.photosets.getPhotos": jsonHandler(t, map[string]any{
			"stat": "ok",
			"photoset": map[string]any{
				"id": "72157600000001", "owner": okOwnerNSID, "page": 1, "pages": 1, "perpage": 500, "total": 1,
				"photo": []map[string]any{{
					"id": "11111111", "owner": okOwnerNSID, "url_o": "http://127.0.0.1:1/fake.jpg", "media": "photo",
				}},
			},
		}),
		// opts.Sets == nil forces includeOrphans regardless of
		// opts.IncludeOrphans, so the orphan first-page lookup runs too.
		"flickr.people.getPhotos": jsonHandler(t, map[string]any{
			"stat":   "ok",
			"photos": map[string]any{"page": 1, "pages": 1, "perpage": 500, "total": 0, "photo": []map[string]any{}},
		}),
	})
	client := api.NewClient("key", "secret", "token", "token-secret")
	client.SetBaseURL(srv.URL)
	root := t.TempDir()
	d := New(client, root, 1)
	d.Quiet = true

	stats, err := d.DownloadByUser(context.Background(), okOwnerNSID, UserDownloadOptions{IncludeOrphans: false})
	if err != nil {
		t.Fatalf("DownloadByUser: %v", err)
	}
	if stats.Failed != 1 {
		t.Fatalf("stats.Failed = %d, want 1 (non-Flickr CDN host refused)", stats.Failed)
	}
	setDir := filepath.Join(root, okOwnerNSID, "Album A")
	if info, err := os.Stat(setDir); err != nil || !info.IsDir() {
		t.Fatalf("expected output directory %s to exist: %v", setDir, err)
	}
}

func TestDownloadByUserPropagatesDiscoveryError(t *testing.T) {
	srv := fakeFlickrServer(t, map[string]http.HandlerFunc{
		"flickr.photosets.getList": jsonHandler(t, map[string]any{"stat": "fail", "code": 1, "message": "user not found"}),
	})
	client := api.NewClient("key", "secret", "token", "token-secret")
	client.SetBaseURL(srv.URL)
	d := New(client, t.TempDir(), 1)
	d.Quiet = true

	_, err := d.DownloadByUser(context.Background(), okOwnerNSID, UserDownloadOptions{})
	if err == nil {
		t.Fatal("expected an error when photoset discovery fails")
	}
}

func TestVerifyUserDetectsStaleDirectories(t *testing.T) {
	root := t.TempDir()
	client := api.NewClient("key", "secret", "token", "token-secret")
	d := New(client, root, 1)

	// A directory under the user root that isn't among the current sets.
	if err := os.MkdirAll(filepath.Join(root, okOwnerNSID, "Orphaned Folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	report, err := d.VerifyUser(context.Background(), okOwnerNSID, nil, false)
	if err != nil {
		t.Fatalf("VerifyUser: %v", err)
	}
	if len(report.StaleDirectories) != 1 {
		t.Fatalf("stale directories = %v, want 1", report.StaleDirectories)
	}
}
