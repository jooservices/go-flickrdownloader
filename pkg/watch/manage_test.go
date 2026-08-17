package watch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddSourcesCreatesYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchlist.yaml")
	added, err := AddSources(path, []string{"https://www.flickr.com/photos/alice/"})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 {
		t.Fatalf("added = %v, want 1 URL", added)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sources) != 1 || cfg.Sources[0].URL != "https://www.flickr.com/photos/alice/" {
		t.Fatalf("loaded sources = %+v", cfg.Sources)
	}
}

func TestAddSourcesSkipsDuplicatesAndBadURLs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "watchlist.yaml")
	if _, err := AddSources(path, []string{"https://www.flickr.com/photos/alice/"}); err != nil {
		t.Fatal(err)
	}
	added, err := AddSources(path, []string{
		"https://www.flickr.com/photos/alice/", // duplicate
		"https://www.flickr.com/photos/bob/",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 || added[0] != "https://www.flickr.com/photos/bob/" {
		t.Fatalf("added = %v, want only bob", added)
	}
	if _, err := AddSources(path, []string{"https://example.com/not-flickr"}); err == nil {
		t.Fatal("expected an error for a non-Flickr URL")
	}
	cfg, _ := Load(path)
	if len(cfg.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(cfg.Sources))
	}
}

func TestAddSourcesPreservesExistingGlobalAndSourceOptions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "watchlist.yaml")
	content := `
poll_interval: 1h
sources:
  - url: https://www.flickr.com/photos/alice/
    albums: wedding
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := AddSources(path, []string{"https://www.flickr.com/photos/bob/"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollInterval.String() != "1h0m0s" {
		t.Fatalf("global poll interval lost: %v", cfg.PollInterval)
	}
	if cfg.Sources[0].Albums != "wedding" {
		t.Fatalf("existing source options lost: %+v", cfg.Sources[0])
	}
	if len(cfg.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(cfg.Sources))
	}
}

func TestAddSourcesTextFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sources.txt")
	if _, err := AddSources(path, []string{"https://www.flickr.com/photos/alice/"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "https://www.flickr.com/photos/alice/") {
		t.Fatalf("text file missing URL: %q", data)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sources) != 1 {
		t.Fatalf("sources = %d, want 1", len(cfg.Sources))
	}
}

func TestRemoveSourcesYAMLKeepsRemainingOptions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "watchlist.yaml")
	content := `
sources:
  - url: https://www.flickr.com/photos/alice/
    albums: wedding
  - url: https://www.flickr.com/photos/bob/
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveSources(path, []string{"https://www.flickr.com/photos/alice/"})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "https://www.flickr.com/photos/alice/" {
		t.Fatalf("removed = %v", removed)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sources) != 1 || cfg.Sources[0].URL != "https://www.flickr.com/photos/bob/" {
		t.Fatalf("sources after removal = %+v", cfg.Sources)
	}
}

func TestRemoveSourcesTextKeepsComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sources.txt")
	content := "# keep me\nhttps://www.flickr.com/photos/alice/\n\nhttps://www.flickr.com/photos/bob/\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveSources(path, []string{"https://www.flickr.com/photos/alice/"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "# keep me") {
		t.Fatalf("comment lost: %q", got)
	}
	if strings.Contains(got, "alice") {
		t.Fatalf("alice still present: %q", got)
	}
	if !strings.Contains(got, "bob") {
		t.Fatalf("bob lost: %q", got)
	}
}

func TestRemoveSourcesMissingFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchlist.yaml")
	if _, err := RemoveSources(path, []string{"https://www.flickr.com/photos/alice/"}); err == nil {
		t.Fatal("expected an error for a missing watchlist")
	}
}

func TestListSourcesMissingFileIsEmpty(t *testing.T) {
	urls, err := ListSources(filepath.Join(t.TempDir(), "watchlist.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 0 {
		t.Fatalf("urls = %v, want none", urls)
	}
}

func TestAddSourcesAtomicNoPartiallyWrittenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "watchlist.yaml")
	if err := os.WriteFile(path, []byte("sources:\n  - url: https://www.flickr.com/photos/alice/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := AddSources(path, []string{"https://www.flickr.com/photos/bob/"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
}
