package update

import (
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
