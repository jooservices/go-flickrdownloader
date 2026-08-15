package update

import (
	"archive/tar"
	"compress/gzip"
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

func TestIsBinaryAssetName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"flickrdownloader", true},
		{"flickrdownloader.exe", true},
		// Real release archives name the entry after the asset itself
		// (see AssetFor), not the bare binary name.
		{"flickrdownloader_darwin_arm64", true},
		{"flickrdownloader_windows_amd64", true},
		{"flickrdownloader_windows_amd64.exe", true},
		{"flickrdownloader-old", false},
		{"README.md", false},
		{"LICENSE", false},
	}
	for _, c := range cases {
		if got := isBinaryAssetName(c.name); got != c.want {
			t.Errorf("isBinaryAssetName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestExtractBinaryMatchesRealAssetLayout pins extractBinary against the
// actual archive layout every published release uses (verified against a
// real v1.2.0 asset): the entry inside the tar.gz is named after the asset
// itself, e.g. "flickrdownloader_linux_amd64", not the bare binary name.
// Matching only the bare name here previously made self-update fail on
// every release with "binary not found in archive".
func TestExtractBinaryMatchesRealAssetLayout(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "release.tar.gz")

	const want = "fake binary contents"
	writeTarGz(t, archivePath, map[string]string{
		"flickrdownloader_linux_amd64": want,
	})

	binPath, err := extractBinary(archivePath, dir)
	if err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	got, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read extracted binary: %v", err)
	}
	if string(got) != want {
		t.Fatalf("extracted contents = %q, want %q", got, want)
	}
}

func writeTarGz(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o755,
			Size: int64(len(content)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
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
