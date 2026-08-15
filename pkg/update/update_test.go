package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestIsNewer(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"", "v1.0.0", true},
		{"dev", "v1.0.0", true},
		{"v1.0.0", "v1.0.0", false},
		{"1.0.0", "v1.0.1", true},
		{"v1.1.0", "v1.0.9", false},
		{"v1.9.9", "v2.0.0", true},
		{"v1.10.0", "v1.9.0", false},
		{"v1.2.3", "v1.2.3", false},
		{"v1.2.3-beta", "v1.2.3", false},
	}
	for _, c := range cases {
		if got := IsNewer(c.current, c.latest); got != c.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

func TestAssetFor(t *testing.T) {
	platformAsset := "flickrdownloader_v1.0.0_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	rel := &Release{
		TagName: "v1.0.0",
		Assets: []Asset{
			{Name: "flickrdownloader_v1.0.0_linux_amd64.tar.gz"},
			{Name: "flickrdownloader_v1.0.0_windows_amd64.tar.gz"},
			{Name: "flickrdownloader_v1.0.0_checksums.txt"},
			{Name: platformAsset},
		},
	}
	got := rel.AssetFor()
	if got == nil {
		t.Fatalf("no asset matched for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if got.Name != platformAsset {
		t.Fatalf("matched %q, want %q", got.Name, platformAsset)
	}

	if none := (&Release{TagName: "v1.0.0"}).AssetFor(); none != nil {
		t.Fatalf("expected nil for release without matching assets")
	}
}

func TestExpectedSHA256FromDigest(t *testing.T) {
	a := &Asset{Name: "flickrdownloader_linux_amd64.tar.gz", Digest: "sha256:ABCDEF0123"}
	got, err := expectedSHA256(context.Background(), &Release{}, a)
	if err != nil {
		t.Fatalf("expectedSHA256: %v", err)
	}
	if want := "abcdef0123"; got != want {
		t.Fatalf("expectedSHA256 = %q, want %q", got, want)
	}
}

func TestExpectedSHA256UnsupportedAlgorithm(t *testing.T) {
	a := &Asset{Name: "asset.tar.gz", Digest: "md5:deadbeef"}
	if _, err := expectedSHA256(context.Background(), &Release{}, a); err == nil {
		t.Fatal("expected an error for an unsupported digest algorithm")
	}
}

func TestExpectedSHA256FromChecksumsFile(t *testing.T) {
	const body = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  flickrdownloader_linux_amd64.tar.gz\n" +
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb  flickrdownloader_darwin_arm64.tar.gz\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	rel := &Release{Assets: []Asset{
		{Name: "checksums.txt", BrowserDownloadURL: srv.URL},
		{Name: "flickrdownloader_linux_amd64.tar.gz"},
	}}
	a := &rel.Assets[1]

	got, err := expectedSHA256(context.Background(), rel, a)
	if err != nil {
		t.Fatalf("expectedSHA256: %v", err)
	}
	if want := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"; got != want {
		t.Fatalf("expectedSHA256 = %q, want %q", got, want)
	}
}

func TestExpectedSHA256NotListedInChecksums(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("aaaa  some_other_asset.tar.gz\n"))
	}))
	defer srv.Close()

	rel := &Release{Assets: []Asset{
		{Name: "checksums.txt", BrowserDownloadURL: srv.URL},
		{Name: "flickrdownloader_linux_amd64.tar.gz"},
	}}
	a := &rel.Assets[1]

	if _, err := expectedSHA256(context.Background(), rel, a); err == nil {
		t.Fatal("expected an error when the asset isn't listed in the checksums file")
	}
}

func TestExpectedSHA256NoneAvailable(t *testing.T) {
	rel := &Release{Assets: []Asset{{Name: "flickrdownloader_linux_amd64.tar.gz"}}}
	a := &rel.Assets[0]

	if _, err := expectedSHA256(context.Background(), rel, a); err == nil {
		t.Fatal("expected Install to refuse when no checksum is available, not silently skip verification")
	}
}

func TestVerifyChecksum(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "archive")
	content := []byte("release archive contents")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	sum := sha256.Sum256(content)
	want := hex.EncodeToString(sum[:])

	if err := verifyChecksum(path, want); err != nil {
		t.Fatalf("verifyChecksum with matching hash: %v", err)
	}
	if err := verifyChecksum(path, "0000000000000000000000000000000000000000000000000000000000000000"); err == nil {
		t.Fatal("expected an error for a mismatched checksum")
	}
}
