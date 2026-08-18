package watch

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jooservices/flickrdownloader/pkg/config"
	"gopkg.in/yaml.v3"
)

// ListSources returns the URLs currently listed in the watchlist at path. A
// missing file is not an error: it yields an empty list (handy for `watch add`
// creating the file from scratch).
func ListSources(path string) ([]string, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	urls := make([]string, 0, len(cfg.Sources))
	for _, s := range cfg.Sources {
		urls = append(urls, s.URL)
	}
	return urls, nil
}

// AddSources appends urls to the watchlist at path, creating the file when it
// does not exist. Already-present URLs and duplicates within the call are
// skipped. Returns the URLs that were actually added.
func AddSources(path string, urls []string) ([]string, error) {
	existing, err := ListSources(path)
	if err != nil {
		return nil, err
	}
	have := make(map[string]bool, len(existing)+len(urls))
	for _, u := range existing {
		have[u] = true
	}
	var added []string
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if !validFlickrURL(u) {
			return nil, fmt.Errorf("not a Flickr URL: %q", u)
		}
		if have[u] {
			continue
		}
		have[u] = true
		added = append(added, u)
	}
	if len(added) == 0 {
		return nil, nil
	}

	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		return added, appendYAMLSources(path, added)
	default:
		return added, appendTextSources(path, added)
	}
}

// RemoveSources removes urls from the watchlist at path. Only exact URL
// matches are removed. Returns the URLs that were actually removed.
func RemoveSources(path string, urls []string) ([]string, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("watchlist not found: %s", path)
	}
	remove := make(map[string]bool, len(urls))
	for _, u := range urls {
		remove[strings.TrimSpace(u)] = true
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		return removeYAMLSources(path, remove)
	default:
		return removeTextSources(path, remove)
	}
}

// appendYAMLSources loads the existing raw YAML (preserving global settings and
// per-source options), appends the new bare sources, and rewrites the file.
func appendYAMLSources(path string, urls []string) error {
	raw := rawConfig{}
	if _, err := os.Stat(path); err == nil {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("parse watchlist: %w", err)
		}
	}
	for _, u := range urls {
		raw.Sources = append(raw.Sources, rawSource{URL: u})
	}
	out, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, out, 0o600)
}

// appendTextSources appends the new URLs to a plain-text watchlist.
func appendTextSources(path string, urls []string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, u := range urls {
		if _, err := fmt.Fprintln(f, u); err != nil {
			return err
		}
	}
	return nil
}

// removeYAMLSources filters matching sources out of the YAML, preserving the
// options of the remaining ones.
func removeYAMLSources(path string, remove map[string]bool) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse watchlist: %w", err)
	}
	var kept []rawSource
	var removed []string
	for _, s := range raw.Sources {
		if remove[s.URL] {
			removed = append(removed, s.URL)
		} else {
			kept = append(kept, s)
		}
	}
	raw.Sources = kept
	out, err := yaml.Marshal(raw)
	if err != nil {
		return nil, err
	}
	return removed, writeFileAtomic(path, out, 0o600)
}

// removeTextSources drops matching lines from a plain-text watchlist, keeping
// comments and blank lines in place.
func removeTextSources(path string, remove map[string]bool) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var kept []string
	var removed []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") && remove[trimmed] {
			removed = append(removed, trimmed)
			continue
		}
		kept = append(kept, line)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	out := strings.Join(kept, "\n")
	if out != "" {
		out += "\n"
	}
	return removed, writeFileAtomic(path, []byte(out), 0o600)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	return config.WriteFileAtomic(path, data, mode)
}
