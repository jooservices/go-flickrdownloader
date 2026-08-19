package watch

import (
	"context"
	"fmt"
	"strings"

	"github.com/jooservices/flickrdownloader/pkg/cache"
)

// LoadFromStore reads the watchlist from the account cache database.
func LoadFromStore(ctx context.Context, store *cache.Store) (*Config, error) {
	if store == nil {
		return nil, fmt.Errorf("cache is not open")
	}
	globals, err := store.GetWatchlistGlobals(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := store.ListWatchlist(ctx)
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		PollInterval: globals.PollInterval,
		OutputDir:    globals.OutputDir,
		Workers:      globals.Workers,
	}
	for _, e := range entries {
		src := Source{
			URL:            e.SourceURL,
			Albums:         e.Albums,
			IncludeOrphans: e.IncludeOrphans,
			PollInterval:   e.PollInterval,
			OutputDir:      e.OutputDir,
			Workers:        e.Workers,
			Enabled:        e.Enabled,
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
		cfg.Sources = append(cfg.Sources, src)
	}
	return cfg, nil
}

// AddToStore appends urls to the cache watchlist. Duplicates and already-present
// URLs are skipped. Returns the URLs that were actually inserted.
func AddToStore(ctx context.Context, store *cache.Store, urls []string) ([]string, error) {
	if store == nil {
		return nil, fmt.Errorf("cache is not open")
	}
	var added []string
	seen := make(map[string]bool, len(urls))
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if !validFlickrURL(u) {
			return nil, fmt.Errorf("not a Flickr URL: %q", u)
		}
		if seen[u] {
			continue
		}
		seen[u] = true
		ok, err := store.InsertWatchlist(ctx, cache.WatchlistEntry{
			SourceURL:      u,
			Albums:         "all",
			IncludeOrphans: true,
			Enabled:        true,
		})
		if err != nil {
			return nil, err
		}
		if ok {
			added = append(added, u)
		}
	}
	return added, nil
}

// RemoveFromStore deletes exact URL matches from the cache watchlist.
func RemoveFromStore(ctx context.Context, store *cache.Store, urls []string) ([]string, error) {
	if store == nil {
		return nil, fmt.Errorf("cache is not open")
	}
	return store.DeleteWatchlist(ctx, urls)
}

// SetEnabled updates the enabled bit for one source URL.
func SetEnabled(ctx context.Context, store *cache.Store, url string, enabled bool) error {
	if store == nil {
		return fmt.Errorf("cache is not open")
	}
	return store.SetWatchlistEnabled(ctx, url, enabled)
}
