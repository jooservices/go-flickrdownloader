// Package watch implements the long-running watchlist feature: Flickr URLs
// that are re-downloaded on an interval, waiting through quota exhaustion
// instead of failing. The default store is the account cache database; YAML
// and plain-text files remain the --file escape hatch and the one-time
// legacy import format.
package watch

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Source is one watchlist entry: a Flickr URL plus download options. Zero
// values inherit the global default from the watchlist or the config file.
type Source struct {
	URL            string
	Albums         string // all | none | comma-separated names; "" inherits
	IncludeOrphans bool   // true inherits
	PollInterval   time.Duration
	OutputDir      string
	Workers        int
	Enabled        bool // false sources are skipped by the scheduler
}

// Config is a parsed watchlist file.
type Config struct {
	PollInterval time.Duration
	OutputDir    string
	Workers      int
	Sources      []Source
}

// DefaultPaths returns the legacy watchlist paths probed in order during
// one-time import: the YAML file first, then the plain-text fallback.
func DefaultPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	dir := filepath.Join(home, ".config", "flickrdownloader")
	return []string{
		filepath.Join(dir, "watchlist.yaml"),
		filepath.Join(dir, "sources.txt"),
	}
}

// Load parses a watchlist file. The format is chosen by extension: .yaml/.yml
// use the structured schema; everything else is treated as plain text (one
// URL per line, # comments and blank lines ignored).
func Load(path string) (*Config, error) {
	if path == "" {
		return nil, fmt.Errorf("watchlist path is empty")
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		return loadYAML(path)
	default:
		return loadText(path)
	}
}

// loadYAML parses the structured watchlist format.
func loadYAML(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read watchlist: %w", err)
	}
	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse watchlist: %w", err)
	}
	cfg := &Config{
		PollInterval: raw.PollInterval,
		OutputDir:    raw.OutputDir,
		Workers:      raw.Workers,
	}
	for i, s := range raw.Sources {
		src, err := normalizeSource(s, cfg)
		if err != nil {
			return nil, fmt.Errorf("watchlist source %d: %w", i+1, err)
		}
		cfg.Sources = append(cfg.Sources, src)
	}
	return cfg, nil
}

type rawConfig struct {
	PollInterval time.Duration `yaml:"poll_interval"`
	OutputDir    string        `yaml:"output_dir"`
	Workers      int           `yaml:"workers"`
	Sources      []rawSource   `yaml:"sources"`
}

type rawSource struct {
	URL           string        `yaml:"url"`
	Albums        string        `yaml:"albums"`
	Uncategorized *bool         `yaml:"uncategorized"`
	PollInterval  time.Duration `yaml:"poll_interval"`
	OutputDir     string        `yaml:"output_dir"`
	Workers       int           `yaml:"workers"`
}

func normalizeSource(s rawSource, cfg *Config) (Source, error) {
	urlStr := strings.TrimSpace(s.URL)
	if urlStr == "" {
		return Source{}, fmt.Errorf("missing url")
	}
	if !validFlickrURL(urlStr) {
		return Source{}, fmt.Errorf("not a Flickr URL: %q", urlStr)
	}
	src := Source{
		URL:          urlStr,
		Albums:       strings.TrimSpace(s.Albums),
		PollInterval: s.PollInterval,
		OutputDir:    s.OutputDir,
		Workers:      s.Workers,
		Enabled:      true,
	}
	if s.Uncategorized == nil {
		src.IncludeOrphans = true
	} else {
		src.IncludeOrphans = *s.Uncategorized
	}
	if src.Albums == "" {
		src.Albums = "all"
	}
	if src.PollInterval == 0 {
		src.PollInterval = cfg.PollInterval
	}
	if src.OutputDir == "" {
		src.OutputDir = cfg.OutputDir
	}
	return src, nil
}

// loadText parses the minimal one-URL-per-line format. Entries inherit the
// global defaults (albums all, uncategorized included).
func loadText(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read watchlist: %w", err)
	}
	defer f.Close()

	cfg := &Config{}
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if !validFlickrURL(text) {
			return nil, fmt.Errorf("line %d: not a Flickr URL: %q", line, text)
		}
		cfg.Sources = append(cfg.Sources, Source{
			URL:            text,
			Albums:         "all",
			IncludeOrphans: true,
			Enabled:        true,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read watchlist: %w", err)
	}
	return cfg, nil
}

// validFlickrURL performs a syntax-only check that urlStr is a Flickr URL of a
// supported shape. NSID resolution and network access are deferred to runtime.
func validFlickrURL(urlStr string) bool {
	u, err := url.Parse(urlStr)
	if err != nil {
		return false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	if host != "flickr.com" && host != "flic.kr" {
		return false
	}
	p := strings.Trim(strings.TrimPrefix(u.Path, "/"), "/")
	if p == "" {
		return false
	}
	parts := strings.Split(p, "/")
	switch parts[0] {
	case "photos", "people":
		return len(parts) >= 2
	default:
		return false
	}
}
