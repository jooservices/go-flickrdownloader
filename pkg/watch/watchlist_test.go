package watch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadYAMLParsesGlobalsAndSources(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "watchlist.yaml", `
poll_interval: 45m
output_dir: /tmp/out
workers: 8
sources:
  - url: https://www.flickr.com/photos/alice/
    albums: all
    uncategorized: true
  - url: https://www.flickr.com/photos/bob/albums/72177720123456789
    poll_interval: 1h
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollInterval != 45*time.Minute {
		t.Fatalf("poll interval = %v, want 45m", cfg.PollInterval)
	}
	if len(cfg.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(cfg.Sources))
	}
	if cfg.Sources[0].URL != "https://www.flickr.com/photos/alice/" {
		t.Fatalf("source 0 url = %q", cfg.Sources[0].URL)
	}
	if !cfg.Sources[0].IncludeOrphans {
		t.Fatal("source 0 should include orphans")
	}
	if cfg.Sources[1].PollInterval != time.Hour {
		t.Fatalf("source 1 poll interval = %v, want 1h (per-source override)", cfg.Sources[1].PollInterval)
	}
	if cfg.Sources[0].PollInterval != 45*time.Minute {
		t.Fatalf("source 0 poll interval = %v, want inherited 45m", cfg.Sources[0].PollInterval)
	}
}

func TestLoadYAMLUncategorizedDefaultsTrue(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "watchlist.yaml", `
sources:
  - url: https://www.flickr.com/photos/alice/
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Sources[0].IncludeOrphans {
		t.Fatal("uncategorized should default to true when omitted")
	}
	if cfg.Sources[0].Albums != "all" {
		t.Fatalf("albums default = %q, want \"all\"", cfg.Sources[0].Albums)
	}
}

func TestLoadYAMLUncategorizedExplicitFalse(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "watchlist.yaml", `
sources:
  - url: https://www.flickr.com/photos/alice/
    uncategorized: false
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sources[0].IncludeOrphans {
		t.Fatal("uncategorized: false should be honored, not overridden by the default")
	}
}

func TestLoadYAMLRejectsNonFlickrURL(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "watchlist.yaml", `
sources:
  - url: https://example.com/not-flickr
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for a non-Flickr URL")
	}
}

func TestLoadTextParsesCommentsAndBlankLines(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "sources.txt", `
# a comment
https://www.flickr.com/photos/alice/

# another comment
https://www.flickr.com/photos/bob/albums/72177720123456789
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(cfg.Sources))
	}
	for _, s := range cfg.Sources {
		if s.Albums != "all" || !s.IncludeOrphans {
			t.Fatalf("text source should inherit defaults (albums=all, orphans=true), got %+v", s)
		}
	}
}

func TestLoadTextRejectsBadURLWithLineNumber(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "sources.txt", "https://www.flickr.com/photos/alice/\nnot a url\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for an invalid line")
	}
	if got := err.Error(); !strings.Contains(got, "line 2") {
		t.Fatalf("error %q should mention the offending line number", got)
	}
}

func TestLoadPicksFormatByExtension(t *testing.T) {
	dir := t.TempDir()
	ymlPath := writeFile(t, dir, "list.yml", "sources:\n  - url: https://www.flickr.com/photos/alice/\n")
	if _, err := Load(ymlPath); err != nil {
		t.Fatalf(".yml should parse as YAML: %v", err)
	}
	txtPath := writeFile(t, dir, "list.other", "https://www.flickr.com/photos/alice/\n")
	if _, err := Load(txtPath); err != nil {
		t.Fatalf("non-.yaml/.yml extension should fall back to text: %v", err)
	}
}

func TestLoadEmptyPathIsAnError(t *testing.T) {
	if _, err := Load(""); err == nil {
		t.Fatal("expected an error for an empty path")
	}
}

func TestLoadMissingFileIsAnError(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestDefaultPathsOrdersYAMLBeforeText(t *testing.T) {
	paths := DefaultPaths()
	if len(paths) != 2 {
		t.Fatalf("DefaultPaths() = %v, want 2 entries", paths)
	}
	if filepath.Ext(paths[0]) != ".yaml" {
		t.Fatalf("first default path %q should be the YAML file", paths[0])
	}
	if filepath.Base(paths[1]) != "sources.txt" {
		t.Fatalf("second default path %q should be sources.txt", paths[1])
	}
}
