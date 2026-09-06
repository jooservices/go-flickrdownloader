package cache

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

const (
	metaWatchPollIntervalNs = "watch.poll_interval_ns"
	metaWatchOutputDir      = "watch.output_dir"
	metaWatchWorkers        = "watch.workers"
	metaWatchLegacyImported = "watch.legacy_imported"
)

// WatchlistEntry is one persisted watch source. A zero PollInterval, OutputDir,
// or Workers means "inherit the watchlist global".
type WatchlistEntry struct {
	ID             int64
	SourceURL      string
	Albums         string
	IncludeOrphans bool
	PollInterval   time.Duration
	OutputDir      string
	Workers        int
	Enabled        bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// WatchlistGlobals are file-level defaults stored in cache_meta.
type WatchlistGlobals struct {
	PollInterval time.Duration
	OutputDir    string
	Workers      int
}

func (s *Store) metaGet(ctx context.Context, key string) (string, bool, error) {
	if s == nil || s.db == nil {
		return "", false, fmt.Errorf("cache is not open")
	}
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM cache_meta WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read cache meta %s: %w", key, err)
	}
	return value, true, nil
}

func (s *Store) metaSet(ctx context.Context, key, value string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("cache is not open")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO cache_meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	if err != nil {
		return fmt.Errorf("write cache meta %s: %w", key, err)
	}
	return nil
}

// WatchlistLegacyImported reports whether the one-time YAML/text import ran.
func (s *Store) WatchlistLegacyImported(ctx context.Context) (bool, error) {
	value, ok, err := s.metaGet(ctx, metaWatchLegacyImported)
	if err != nil {
		return false, err
	}
	return ok && value == "1", nil
}

// SetWatchlistLegacyImported records that the YAML/text import has finished.
func (s *Store) SetWatchlistLegacyImported(ctx context.Context) error {
	return s.metaSet(ctx, metaWatchLegacyImported, "1")
}

// GetWatchlistGlobals returns the stored watchlist-wide defaults.
func (s *Store) GetWatchlistGlobals(ctx context.Context) (WatchlistGlobals, error) {
	var g WatchlistGlobals
	if ns, ok, err := s.metaGet(ctx, metaWatchPollIntervalNs); err != nil {
		return g, err
	} else if ok && ns != "" {
		n, err := strconv.ParseInt(ns, 10, 64)
		if err != nil {
			return g, fmt.Errorf("parse watch poll interval: %w", err)
		}
		g.PollInterval = time.Duration(n)
	}
	if dir, ok, err := s.metaGet(ctx, metaWatchOutputDir); err != nil {
		return g, err
	} else if ok {
		g.OutputDir = dir
	}
	if workers, ok, err := s.metaGet(ctx, metaWatchWorkers); err != nil {
		return g, err
	} else if ok && workers != "" {
		n, err := strconv.Atoi(workers)
		if err != nil {
			return g, fmt.Errorf("parse watch workers: %w", err)
		}
		g.Workers = n
	}
	return g, nil
}

// SetWatchlistGlobals stores watchlist-wide defaults.
func (s *Store) SetWatchlistGlobals(ctx context.Context, g WatchlistGlobals) error {
	if err := s.metaSet(ctx, metaWatchPollIntervalNs, strconv.FormatInt(int64(g.PollInterval), 10)); err != nil {
		return err
	}
	if err := s.metaSet(ctx, metaWatchOutputDir, g.OutputDir); err != nil {
		return err
	}
	return s.metaSet(ctx, metaWatchWorkers, strconv.Itoa(g.Workers))
}

// ListWatchlist returns every watch source in insertion order.
func (s *Store) ListWatchlist(ctx context.Context) ([]WatchlistEntry, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("cache is not open")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, source_url, albums, include_orphans, poll_interval_ns, output_dir,
		       workers, enabled, created_at, updated_at
		FROM watchlist
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list watchlist: %w", err)
	}
	defer rows.Close()

	var entries []WatchlistEntry
	for rows.Next() {
		var e WatchlistEntry
		var orphans, enabled int
		var pollNs, createdAt, updatedAt int64
		if err := rows.Scan(
			&e.ID, &e.SourceURL, &e.Albums, &orphans, &pollNs, &e.OutputDir,
			&e.Workers, &enabled, &createdAt, &updatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan watchlist: %w", err)
		}
		e.IncludeOrphans = orphans != 0
		e.Enabled = enabled != 0
		e.PollInterval = time.Duration(pollNs)
		e.CreatedAt = time.Unix(createdAt, 0)
		e.UpdatedAt = time.Unix(updatedAt, 0)
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list watchlist rows: %w", err)
	}
	return entries, nil
}

// InsertWatchlist adds a source if the URL is not already present. Existing
// URLs are left unchanged. Returns whether a row was inserted.
func (s *Store) InsertWatchlist(ctx context.Context, entry WatchlistEntry) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("cache is not open")
	}
	if entry.Albums == "" {
		entry.Albums = "all"
	}
	now := time.Now().Unix()
	orphans := 0
	if entry.IncludeOrphans {
		orphans = 1
	}
	enabled := 0
	if entry.Enabled {
		enabled = 1
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO watchlist(
			source_url, albums, include_orphans, poll_interval_ns, output_dir,
			workers, enabled, created_at, updated_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_url) DO NOTHING`,
		entry.SourceURL, entry.Albums, orphans, int64(entry.PollInterval),
		entry.OutputDir, entry.Workers, enabled, now, now,
	)
	if err != nil {
		return false, fmt.Errorf("insert watchlist: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count inserted watchlist: %w", err)
	}
	return n > 0, nil
}

// DeleteWatchlist removes exact URL matches. Returns the URLs that were deleted.
func (s *Store) DeleteWatchlist(ctx context.Context, urls []string) ([]string, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("cache is not open")
	}
	var removed []string
	for _, u := range urls {
		if u == "" {
			continue
		}
		result, err := s.db.ExecContext(ctx, "DELETE FROM watchlist WHERE source_url = ?", u)
		if err != nil {
			return nil, fmt.Errorf("delete watchlist %s: %w", u, err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("count deleted watchlist %s: %w", u, err)
		}
		if n > 0 {
			removed = append(removed, u)
		}
	}
	return removed, nil
}

// SetWatchlistEnabled flips the enabled bit for one source URL.
func (s *Store) SetWatchlistEnabled(ctx context.Context, url string, enabled bool) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("cache is not open")
	}
	flag := 0
	if enabled {
		flag = 1
	}
	result, err := s.db.ExecContext(ctx,
		"UPDATE watchlist SET enabled = ?, updated_at = ? WHERE source_url = ?",
		flag, time.Now().Unix(), url,
	)
	if err != nil {
		return fmt.Errorf("set watchlist enabled: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count watchlist enabled update: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("watchlist source not found: %s", url)
	}
	return nil
}
