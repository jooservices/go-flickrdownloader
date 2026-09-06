package watch

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/jooservices/go-flickrdownloader/pkg/cache"
)

// ImportLegacyFiles copies watchlist.yaml / sources.txt into the cache
// watchlist once, then renames the files to *.migrated. Missing files are a
// no-op. Subsequent calls are skipped when the cache records the import.
func ImportLegacyFiles(ctx context.Context, store *cache.Store) error {
	if store == nil {
		return fmt.Errorf("cache is not open")
	}
	done, err := store.WatchlistLegacyImported(ctx)
	if err != nil {
		return err
	}
	if done {
		return nil
	}

	cfg, found, err := loadAllLegacyWatchlists()
	if err != nil {
		return err
	}
	if cfg != nil {
		if err := persistLegacyWatchlist(ctx, store, cfg); err != nil {
			return err
		}
		if err := renameLegacyWatchlists(found); err != nil {
			return err
		}
	}
	return store.SetWatchlistLegacyImported(ctx)
}

// loadAllLegacyWatchlists loads every legacy watchlist file that exists, not
// just the first: every found file's Sources are merged into one Config,
// while whichever file is found first (DefaultPaths' order — watchlist.yaml
// before sources.txt) supplies the scalar globals (poll interval, output
// dir, workers). Duplicate URLs across files are harmless: InsertWatchlist
// no-ops on a conflicting source_url (UNIQUE constraint).
//
// A previous version loaded only the first found file, yet still renamed
// every found file to .migrated and marked the one-shot import done
// regardless — silently and permanently discarding every URL in a second
// legacy file whenever both watchlist.yaml and sources.txt existed, with no
// error, warning, or way to re-trigger the import.
//
// A file that exists but fails to parse is skipped with a warning rather
// than aborting the whole import: reading every found file (instead of
// stopping at the first) means one malformed file must not be able to block
// import of another, valid file found alongside it — the one-shot import
// flag would otherwise get set on failure, permanently discarding the good
// file's URLs too, with the only recourse being to find and fix or remove
// the malformed one by hand. Only successfully loaded files are reported in
// found (and so only those get renamed to *.migrated by the caller) — a
// skipped file is left at its original path, discoverable to fix.
func loadAllLegacyWatchlists() (*Config, []string, error) {
	var merged *Config
	var found []string
	for _, path := range DefaultPaths() {
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, nil, fmt.Errorf("stat legacy watchlist %s: %w", path, err)
		}
		cfg, err := Load(path)
		if err != nil {
			log.Printf("watch: skipping malformed legacy watchlist %s: %v", path, err)
			continue
		}
		found = append(found, path)
		if merged == nil {
			merged = &Config{PollInterval: cfg.PollInterval, OutputDir: cfg.OutputDir, Workers: cfg.Workers}
		}
		merged.Sources = append(merged.Sources, cfg.Sources...)
	}
	return merged, found, nil
}

func persistLegacyWatchlist(ctx context.Context, store *cache.Store, cfg *Config) error {
	if err := store.SetWatchlistGlobals(ctx, cache.WatchlistGlobals{
		PollInterval: cfg.PollInterval,
		OutputDir:    cfg.OutputDir,
		Workers:      cfg.Workers,
	}); err != nil {
		return err
	}
	for _, src := range cfg.Sources {
		if _, err := store.InsertWatchlist(ctx, cache.WatchlistEntry{
			SourceURL:      src.URL,
			Albums:         src.Albums,
			IncludeOrphans: src.IncludeOrphans,
			PollInterval:   src.PollInterval,
			OutputDir:      src.OutputDir,
			Workers:        src.Workers,
			Enabled:        true,
		}); err != nil {
			return err
		}
	}
	return nil
}

func renameLegacyWatchlists(paths []string) error {
	for _, path := range paths {
		if err := os.Rename(path, path+".migrated"); err != nil {
			return fmt.Errorf("rename legacy watchlist %s: %w", path, err)
		}
	}
	return nil
}
