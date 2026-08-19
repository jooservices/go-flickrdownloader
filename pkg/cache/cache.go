// Package cache stores successful Flickr REST responses on disk. It is an
// optimization only: callers can fall back to the network when the database
// is unavailable, while cache-only callers receive an explicit miss.
package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jooservices/flickrdownloader/pkg/api"
	_ "modernc.org/sqlite"
)

// Store is a small SQLite-backed response store. A single connection keeps
// SQLite locking predictable while REST requests are already rate-limited.
type Store struct {
	db   *sql.DB
	path string
}

const currentSchemaVersion = 8

const authFingerprintKey = "auth_fingerprint"

const createPhotosetStatusTable = `
	CREATE TABLE IF NOT EXISTS photoset_status (
		root_dir     TEXT NOT NULL,
		owner_nsid   TEXT NOT NULL,
		photoset_id  TEXT NOT NULL,
		title        TEXT NOT NULL,
		directory    TEXT NOT NULL,
		expected_ids TEXT NOT NULL,
		complete     INTEGER NOT NULL,
		source_updated_at INTEGER NOT NULL DEFAULT 0,
		file_sizes   TEXT NOT NULL DEFAULT '{}',
		verified_at  INTEGER NOT NULL,
		PRIMARY KEY (root_dir, owner_nsid, photoset_id)
	)`

const createPhotoMetadataTable = `
	CREATE TABLE IF NOT EXISTS photo_metadata (
		photo_id        TEXT PRIMARY KEY,
		url             TEXT NOT NULL,
		media           TEXT NOT NULL,
		original_format TEXT NOT NULL,
		extension       TEXT NOT NULL,
		o_width         INTEGER NOT NULL,
		o_height        INTEGER NOT NULL,
		size_bytes      INTEGER NOT NULL,
		updated_at      INTEGER NOT NULL
	)`

const createResponseLeasesTable = `
	CREATE TABLE IF NOT EXISTS response_leases (
		cache_key  TEXT PRIMARY KEY,
		owner      TEXT NOT NULL,
		expires_at INTEGER NOT NULL
	)`

const createCacheMetaTable = `
	CREATE TABLE IF NOT EXISTS cache_meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`

const createWatchlistTable = `
	CREATE TABLE IF NOT EXISTS watchlist (
		id                INTEGER PRIMARY KEY AUTOINCREMENT,
		source_url        TEXT NOT NULL UNIQUE,
		albums            TEXT NOT NULL DEFAULT 'all',
		include_orphans   INTEGER NOT NULL DEFAULT 1,
		poll_interval_ns  INTEGER NOT NULL DEFAULT 0,
		output_dir        TEXT NOT NULL DEFAULT '',
		workers           INTEGER NOT NULL DEFAULT 0,
		enabled           INTEGER NOT NULL DEFAULT 1,
		created_at        INTEGER NOT NULL,
		updated_at        INTEGER NOT NULL
	)`

// PhotosetStatus is the persisted local verification manifest for one
// photoset. ExpectedIDs lets a later run validate local files without asking
// Flickr for the photoset listing again.
type PhotosetStatus struct {
	RootDir         string
	OwnerNSID       string
	PhotosetID      string
	Title           string
	Directory       string
	ExpectedIDs     []string
	Complete        bool
	SourceUpdatedAt int64
	FileSizes       map[string]int64
	VerifiedAt      time.Time
}

// Open creates or opens a response cache at path.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("cache path is empty")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create cache directory: %w", err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open cache database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	closeOnError := func(err error) (*Store, error) {
		_ = db.Close()
		return nil, err
	}
	for _, pragma := range []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			return closeOnError(fmt.Errorf("configure cache database: %w", err))
		}
	}
	if err := migrate(db); err != nil {
		return closeOnError(err)
	}

	if path != ":memory:" {
		if err := protectCacheFiles(path); err != nil {
			return closeOnError(fmt.Errorf("protect cache database: %w", err))
		}
	}
	return &Store{db: db, path: path}, nil
}

func protectCacheFiles(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func migrate(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin cache migration: %w", err)
	}
	rollback := func(err error) error {
		_ = tx.Rollback()
		return err
	}

	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return rollback(fmt.Errorf("create schema version table: %w", err))
	}
	var version int
	err = tx.QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&version)
	if err == sql.ErrNoRows {
		version, err = detectSchemaVersion(tx)
		if err != nil {
			return rollback(err)
		}
		if _, err := tx.Exec("INSERT INTO schema_version(version) VALUES(?)", version); err != nil {
			return rollback(fmt.Errorf("record schema version: %w", err))
		}
	} else if err != nil {
		return rollback(fmt.Errorf("read schema version: %w", err))
	}
	if version > currentSchemaVersion {
		return rollback(fmt.Errorf("cache schema version %d is newer than supported version %d", version, currentSchemaVersion))
	}

	responsesExists, err := tableExists(tx, "responses")
	if err != nil {
		return rollback(fmt.Errorf("inspect response schema: %w", err))
	}
	if !responsesExists {
		if _, err := tx.Exec(`
			CREATE TABLE responses (
				cache_key  TEXT PRIMARY KEY,
				body       BLOB NOT NULL,
				fetched_at INTEGER NOT NULL,
				method     TEXT NOT NULL DEFAULT ''
			)`); err != nil {
			return rollback(fmt.Errorf("create response schema: %w", err))
		}
	}
	methodExists, err := columnExists(tx, "responses", "method")
	if err != nil {
		return rollback(fmt.Errorf("inspect response columns: %w", err))
	}
	if !methodExists {
		if _, err := tx.Exec("ALTER TABLE responses ADD COLUMN method TEXT NOT NULL DEFAULT ''"); err != nil {
			return rollback(fmt.Errorf("migrate response method column: %w", err))
		}
	}
	if _, err := tx.Exec(createPhotosetStatusTable); err != nil {
		return rollback(fmt.Errorf("create photoset status schema: %w", err))
	}
	statusUpdatedExists, err := columnExists(tx, "photoset_status", "source_updated_at")
	if err != nil {
		return rollback(fmt.Errorf("inspect photoset status columns: %w", err))
	}
	if !statusUpdatedExists {
		if _, err := tx.Exec("ALTER TABLE photoset_status ADD COLUMN source_updated_at INTEGER NOT NULL DEFAULT 0"); err != nil {
			return rollback(fmt.Errorf("migrate photoset source timestamp: %w", err))
		}
	}
	statusFileSizesExists, err := columnExists(tx, "photoset_status", "file_sizes")
	if err != nil {
		return rollback(fmt.Errorf("inspect photoset file size columns: %w", err))
	}
	if !statusFileSizesExists {
		if _, err := tx.Exec("ALTER TABLE photoset_status ADD COLUMN file_sizes TEXT NOT NULL DEFAULT '{}'"); err != nil {
			return rollback(fmt.Errorf("migrate photoset file sizes: %w", err))
		}
	}
	if _, err := tx.Exec(createPhotoMetadataTable); err != nil {
		return rollback(fmt.Errorf("create photo metadata schema: %w", err))
	}
	if _, err := tx.Exec(createResponseLeasesTable); err != nil {
		return rollback(fmt.Errorf("create response lease schema: %w", err))
	}
	if _, err := tx.Exec(createCacheMetaTable); err != nil {
		return rollback(fmt.Errorf("create cache meta schema: %w", err))
	}
	if _, err := tx.Exec(createWatchlistTable); err != nil {
		return rollback(fmt.Errorf("create watchlist schema: %w", err))
	}
	for table, columns := range map[string][]string{
		"responses":       {"cache_key", "body", "fetched_at", "method"},
		"photoset_status": {"root_dir", "owner_nsid", "photoset_id", "title", "directory", "expected_ids", "complete", "source_updated_at", "file_sizes", "verified_at"},
		"photo_metadata":  {"photo_id", "url", "media", "original_format", "extension", "o_width", "o_height", "size_bytes", "updated_at"},
		"response_leases": {"cache_key", "owner", "expires_at"},
		"cache_meta":      {"key", "value"},
		"watchlist":       {"id", "source_url", "albums", "include_orphans", "poll_interval_ns", "output_dir", "workers", "enabled", "created_at", "updated_at"},
	} {
		if err := requireColumns(tx, table, columns); err != nil {
			return rollback(err)
		}
	}
	if version != currentSchemaVersion {
		if _, err := tx.Exec("UPDATE schema_version SET version = ?", currentSchemaVersion); err != nil {
			return rollback(fmt.Errorf("update schema version: %w", err))
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit cache migration: %w", err)
	}
	return nil
}

func detectSchemaVersion(tx *sql.Tx) (int, error) {
	responses, err := tableExists(tx, "responses")
	if err != nil {
		return 0, fmt.Errorf("inspect response schema: %w", err)
	}
	if !responses {
		return 0, nil
	}
	method, err := columnExists(tx, "responses", "method")
	if err != nil {
		return 0, fmt.Errorf("inspect response columns: %w", err)
	}
	status, err := tableExists(tx, "photoset_status")
	if err != nil {
		return 0, fmt.Errorf("inspect photoset status schema: %w", err)
	}
	if method && status {
		metadata, err := tableExists(tx, "photo_metadata")
		if err != nil {
			return 0, fmt.Errorf("inspect photo metadata schema: %w", err)
		}
		if metadata {
			leases, err := tableExists(tx, "response_leases")
			if err != nil {
				return 0, fmt.Errorf("inspect response lease schema: %w", err)
			}
			if leases {
				sourceUpdated, err := columnExists(tx, "photoset_status", "source_updated_at")
				if err != nil {
					return 0, fmt.Errorf("inspect photoset source timestamp: %w", err)
				}
				if !sourceUpdated {
					return 4, nil
				}
				fileSizes, err := columnExists(tx, "photoset_status", "file_sizes")
				if err != nil {
					return 0, fmt.Errorf("inspect photoset file sizes: %w", err)
				}
				if fileSizes {
					meta, err := tableExists(tx, "cache_meta")
					if err != nil {
						return 0, fmt.Errorf("inspect cache meta schema: %w", err)
					}
					if meta {
						watchlist, err := tableExists(tx, "watchlist")
						if err != nil {
							return 0, fmt.Errorf("inspect watchlist schema: %w", err)
						}
						if watchlist {
							return currentSchemaVersion, nil
						}
						return 7, nil
					}
					return 6, nil
				}
				return 5, nil
			}
			return 3, nil
		}
		return 2, nil
	}
	return 1, nil
}

func tableExists(tx *sql.Tx, name string) (bool, error) {
	var count int
	err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&count)
	return count > 0, err
}

var knownCacheTables = map[string]struct{}{
	"responses":       {},
	"photoset_status": {},
	"photo_metadata":  {},
	"response_leases": {},
	"cache_meta":      {},
	"watchlist":       {},
}

func tableColumns(tx *sql.Tx, table string) (map[string]bool, error) {
	if _, ok := knownCacheTables[table]; !ok {
		return nil, fmt.Errorf("unknown cache table %q", table)
	}
	rows, err := tx.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

func columnExists(tx *sql.Tx, table, column string) (bool, error) {
	columns, err := tableColumns(tx, table)
	if err != nil {
		return false, err
	}
	return columns[column], nil
}

func requireColumns(tx *sql.Tx, table string, columns []string) error {
	existing, err := tableColumns(tx, table)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", table, err)
	}
	for _, column := range columns {
		if !existing[column] {
			return fmt.Errorf("cache schema missing %s.%s", table, column)
		}
	}
	return nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	err := s.db.Close()
	if s.path != "" && s.path != ":memory:" {
		_ = protectCacheFiles(s.path)
	}
	return err
}

// BindAuth associates this cache with the current OAuth access token. A
// changed fingerprint clears REST responses, photo metadata, and leases so
// private listings cannot be reused after re-authentication. Photoset
// completion manifests are retained because they describe local files.
func (s *Store) BindAuth(ctx context.Context, fingerprint string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("cache is not open")
	}
	var stored string
	err := s.db.QueryRowContext(ctx,
		"SELECT value FROM cache_meta WHERE key = ?", authFingerprintKey,
	).Scan(&stored)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("read auth fingerprint: %w", err)
	}
	if err == nil && stored == fingerprint {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin auth bind: %w", err)
	}
	rollback := func(err error) error {
		_ = tx.Rollback()
		return err
	}
	for _, table := range []string{"responses", "photo_metadata", "response_leases"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return rollback(fmt.Errorf("clear %s after auth change: %w", table, err))
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO cache_meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		authFingerprintKey, fingerprint,
	); err != nil {
		return rollback(fmt.Errorf("store auth fingerprint: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit auth bind: %w", err)
	}
	return nil
}

// Get returns a cached response and the time it was fetched.
func (s *Store) Get(ctx context.Context, key string) (body []byte, fetchedAt time.Time, found bool, err error) {
	if s == nil || s.db == nil {
		return nil, time.Time{}, false, fmt.Errorf("cache is not open")
	}
	var unix int64
	err = s.db.QueryRowContext(ctx,
		"SELECT body, fetched_at FROM responses WHERE cache_key = ?", key,
	).Scan(&body, &unix)
	if err == sql.ErrNoRows {
		return nil, time.Time{}, false, nil
	}
	if err != nil {
		return nil, time.Time{}, false, fmt.Errorf("read cache response: %w", err)
	}
	return body, time.Unix(unix, 0), true, nil
}

// Put stores a successful response, replacing an older entry for the same
// request key.
func (s *Store) Put(ctx context.Context, key string, body []byte, fetchedAt time.Time) error {
	return s.PutResponse(ctx, key, "", body, fetchedAt)
}

// PutResponse stores a successful response and records its Flickr method so
// maintenance can apply the configured listing/detail retention policy.
func (s *Store) PutResponse(ctx context.Context, key, method string, body []byte, fetchedAt time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("cache is not open")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO responses(cache_key, body, fetched_at, method)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(cache_key) DO UPDATE SET
			body = excluded.body,
			fetched_at = excluded.fetched_at,
			method = excluded.method`,
		key, body, fetchedAt.Unix(), method,
	)
	if err != nil {
		return fmt.Errorf("write cache response: %w", err)
	}
	return nil
}

// Prune removes expired response entries. Photoset completion manifests are
// intentionally retained because they describe local files, not API freshness.
func (s *Store) Prune(ctx context.Context, now time.Time, listingTTL, detailTTL time.Duration) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("cache is not open")
	}
	if listingTTL <= 0 || detailTTL <= 0 {
		return 0, fmt.Errorf("cache retention durations must be positive")
	}
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM responses
		WHERE (method IN (
			'flickr.people.getPhotos',
			'flickr.photosets.getPhotos',
			'flickr.photosets.getList',
			'flickr.photos.getNotInSet'
		) AND fetched_at < ?)
		OR (method NOT IN (
			'flickr.people.getPhotos',
			'flickr.photosets.getPhotos',
			'flickr.photosets.getList',
			'flickr.photos.getNotInSet'
		) AND fetched_at < ?)`,
		now.Add(-listingTTL).Unix(), now.Add(-detailTTL).Unix(),
	)
	if err != nil {
		return 0, fmt.Errorf("prune response cache: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count pruned responses: %w", err)
	}
	metadataResult, err := s.db.ExecContext(ctx,
		"DELETE FROM photo_metadata WHERE updated_at < ?",
		now.Add(-detailTTL).Unix(),
	)
	if err != nil {
		return 0, fmt.Errorf("prune photo metadata: %w", err)
	}
	metadataCount, err := metadataResult.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count pruned photo metadata: %w", err)
	}
	count += metadataCount
	leaseResult, err := s.db.ExecContext(ctx, "DELETE FROM response_leases WHERE expires_at <= ?", now.Unix())
	if err != nil {
		return 0, fmt.Errorf("prune response leases: %w", err)
	}
	leaseCount, err := leaseResult.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count pruned response leases: %w", err)
	}
	count += leaseCount
	return count, nil
}

// Clear removes cached responses, photo metadata, leases, and photoset
// manifests for the account. The watchlist table and watch.* cache_meta
// keys are retained so `cache clear` cannot drop the user's sources.
func (s *Store) Clear(ctx context.Context) (int64, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("cache is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin cache clear: %w", err)
	}
	rollback := func(err error) (int64, error) {
		_ = tx.Rollback()
		return 0, err
	}
	var removed int64
	for _, table := range []string{"responses", "photoset_status", "photo_metadata"} {
		result, err := tx.ExecContext(ctx, "DELETE FROM "+table)
		if err != nil {
			return rollback(fmt.Errorf("clear %s: %w", table, err))
		}
		count, err := result.RowsAffected()
		if err != nil {
			return rollback(fmt.Errorf("count cleared %s: %w", table, err))
		}
		removed += count
	}
	leaseResult, err := tx.ExecContext(ctx, "DELETE FROM response_leases")
	if err != nil {
		return rollback(fmt.Errorf("clear response leases: %w", err))
	}
	leaseCount, err := leaseResult.RowsAffected()
	if err != nil {
		return rollback(fmt.Errorf("count cleared response leases: %w", err))
	}
	removed += leaseCount
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit cache clear: %w", err)
	}
	return removed, nil
}

// TryResponseLease coordinates a cache miss across processes. It returns true
// only when this owner acquired the lease; an expired lease can be claimed.
func (s *Store) TryResponseLease(ctx context.Context, key, owner string, ttl time.Duration) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("cache is not open")
	}
	if ttl <= 0 {
		return false, fmt.Errorf("response lease duration must be positive")
	}
	now := time.Now().Unix()
	expiresAt := time.Now().Add(ttl).Unix()
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO response_leases(cache_key, owner, expires_at)
		VALUES(?, ?, ?)
		ON CONFLICT(cache_key) DO UPDATE SET
			owner = excluded.owner,
			expires_at = excluded.expires_at
		WHERE response_leases.expires_at <= ?`,
		key, owner, expiresAt, now,
	)
	if err != nil {
		return false, fmt.Errorf("acquire response lease: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count response lease: %w", err)
	}
	return count > 0, nil
}

// ReleaseResponseLease releases a lease only when it is still owned by owner.
func (s *Store) ReleaseResponseLease(ctx context.Context, key, owner string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("cache is not open")
	}
	if _, err := s.db.ExecContext(ctx,
		"DELETE FROM response_leases WHERE cache_key = ? AND owner = ?", key, owner); err != nil {
		return fmt.Errorf("release response lease: %w", err)
	}
	return nil
}

// RenewResponseLease extends an active lease held by owner.
func (s *Store) RenewResponseLease(ctx context.Context, key, owner string, ttl time.Duration) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("cache is not open")
	}
	if ttl <= 0 {
		return false, fmt.Errorf("response lease duration must be positive")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE response_leases
		SET expires_at = ?
		WHERE cache_key = ? AND owner = ? AND expires_at > ?`,
		time.Now().Add(ttl).Unix(), key, owner, time.Now().Unix(),
	)
	if err != nil {
		return false, fmt.Errorf("renew response lease: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count renewed response lease: %w", err)
	}
	return count > 0, nil
}

// PutPhotoMetadata stores one normalized photo record.
func (s *Store) PutPhotoMetadata(ctx context.Context, metadata api.PhotoMetadata) error {
	return s.PutPhotoMetadataBatch(ctx, []api.PhotoMetadata{metadata})
}

// PutPhotoMetadataBatch stores normalized records in one transaction so a
// 500-photo listing page does not cause 500 independent SQLite commits.
func (s *Store) PutPhotoMetadataBatch(ctx context.Context, records []api.PhotoMetadata) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("cache is not open")
	}
	if len(records) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin photo metadata write: %w", err)
	}
	rollback := func(err error) error {
		_ = tx.Rollback()
		return err
	}
	for _, metadata := range records {
		updatedAt := metadata.UpdatedAt
		if updatedAt.IsZero() {
			updatedAt = time.Now()
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO photo_metadata(
				photo_id, url, media, original_format, extension,
				o_width, o_height, size_bytes, updated_at
			) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(photo_id) DO UPDATE SET
				url = CASE WHEN excluded.url <> '' THEN excluded.url ELSE photo_metadata.url END,
				media = CASE WHEN excluded.media <> '' THEN excluded.media ELSE photo_metadata.media END,
				original_format = CASE WHEN excluded.original_format <> '' THEN excluded.original_format ELSE photo_metadata.original_format END,
				extension = CASE WHEN excluded.extension <> '' THEN excluded.extension ELSE photo_metadata.extension END,
				o_width = CASE WHEN excluded.o_width > 0 THEN excluded.o_width ELSE photo_metadata.o_width END,
				o_height = CASE WHEN excluded.o_height > 0 THEN excluded.o_height ELSE photo_metadata.o_height END,
				size_bytes = CASE WHEN excluded.size_bytes > 0 THEN excluded.size_bytes ELSE photo_metadata.size_bytes END,
				updated_at = CASE WHEN excluded.url <> photo_metadata.url OR
					excluded.media <> photo_metadata.media OR
					excluded.original_format <> photo_metadata.original_format OR
					excluded.size_bytes > 0 THEN excluded.updated_at ELSE photo_metadata.updated_at END`,
			metadata.ID, metadata.URL, metadata.Media, metadata.OriginalFormat,
			metadata.Extension, metadata.OWidth, metadata.OHeight, metadata.SizeBytes,
			updatedAt.Unix(),
		); err != nil {
			return rollback(fmt.Errorf("write photo metadata %s: %w", metadata.ID, err))
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit photo metadata: %w", err)
	}
	return nil
}

// GetPhotoMetadata returns normalized records for the requested IDs.
func (s *Store) GetPhotoMetadata(ctx context.Context, ids []string) (map[string]api.PhotoMetadata, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("cache is not open")
	}
	result := make(map[string]api.PhotoMetadata, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	if len(ids) > 500 {
		for start := 0; start < len(ids); start += 500 {
			end := min(start+500, len(ids))
			part, err := s.GetPhotoMetadata(ctx, ids[start:end])
			if err != nil {
				return nil, err
			}
			for id, metadata := range part {
				result[id] = metadata
			}
		}
		return result, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, "SELECT photo_id, url, media, original_format, extension, o_width, o_height, size_bytes, updated_at FROM photo_metadata WHERE photo_id IN ("+placeholders+")", args...)
	if err != nil {
		return nil, fmt.Errorf("read photo metadata: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var metadata api.PhotoMetadata
		var updatedAt int64
		if err := rows.Scan(&metadata.ID, &metadata.URL, &metadata.Media, &metadata.OriginalFormat, &metadata.Extension, &metadata.OWidth, &metadata.OHeight, &metadata.SizeBytes, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan photo metadata: %w", err)
		}
		metadata.UpdatedAt = time.Unix(updatedAt, 0)
		result[metadata.ID] = metadata
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read photo metadata rows: %w", err)
	}
	return result, nil
}

// Delete removes one cached response, usually after a CDN or API response
// proves that the cached URL or metadata is no longer valid.
func (s *Store) Delete(ctx context.Context, key string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("cache is not open")
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM responses WHERE cache_key = ?", key); err != nil {
		return fmt.Errorf("delete cache response: %w", err)
	}
	return nil
}

// DeletePhotoMetadata removes the normalized record for one photo.
func (s *Store) DeletePhotoMetadata(ctx context.Context, photoID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("cache is not open")
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM photo_metadata WHERE photo_id = ?", photoID); err != nil {
		return fmt.Errorf("delete photo metadata: %w", err)
	}
	return nil
}

// GetPhotosetStatus returns the manifest for one output root and photoset.
func (s *Store) GetPhotosetStatus(ctx context.Context, rootDir, ownerNSID, photosetID string) (*PhotosetStatus, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("cache is not open")
	}
	var status PhotosetStatus
	var expectedJSON string
	var complete int
	var sourceUpdatedAt, verifiedAt int64
	var fileSizesJSON string
	err := s.db.QueryRowContext(ctx, `
		SELECT title, directory, expected_ids, complete, source_updated_at, file_sizes, verified_at
		FROM photoset_status
		WHERE root_dir = ? AND owner_nsid = ? AND photoset_id = ?`,
		rootDir, ownerNSID, photosetID,
	).Scan(&status.Title, &status.Directory, &expectedJSON, &complete, &sourceUpdatedAt, &fileSizesJSON, &verifiedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read photoset status: %w", err)
	}
	if err := json.Unmarshal([]byte(expectedJSON), &status.ExpectedIDs); err != nil {
		return nil, fmt.Errorf("decode photoset status: %w", err)
	}
	if err := json.Unmarshal([]byte(fileSizesJSON), &status.FileSizes); err != nil {
		return nil, fmt.Errorf("decode photoset file sizes: %w", err)
	}
	status.RootDir = rootDir
	status.OwnerNSID = ownerNSID
	status.PhotosetID = photosetID
	status.Complete = complete != 0
	status.SourceUpdatedAt = sourceUpdatedAt
	status.VerifiedAt = time.Unix(verifiedAt, 0)
	return &status, nil
}

// GetPhotosetStatuses loads every manifest for one output root and owner in a
// single query, which avoids one SQLite round trip per photoset.
func (s *Store) GetPhotosetStatuses(ctx context.Context, rootDir, ownerNSID string) (map[string]*PhotosetStatus, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("cache is not open")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT photoset_id, title, directory, expected_ids, complete, source_updated_at, file_sizes, verified_at
		FROM photoset_status
		WHERE root_dir = ? AND owner_nsid = ?`, rootDir, ownerNSID)
	if err != nil {
		return nil, fmt.Errorf("read photoset statuses: %w", err)
	}
	defer rows.Close()
	statuses := make(map[string]*PhotosetStatus)
	for rows.Next() {
		var status PhotosetStatus
		var expectedJSON string
		var complete int
		var sourceUpdatedAt, verifiedAt int64
		var fileSizesJSON string
		if err := rows.Scan(&status.PhotosetID, &status.Title, &status.Directory, &expectedJSON, &complete, &sourceUpdatedAt, &fileSizesJSON, &verifiedAt); err != nil {
			return nil, fmt.Errorf("scan photoset status: %w", err)
		}
		if err := json.Unmarshal([]byte(expectedJSON), &status.ExpectedIDs); err != nil {
			return nil, fmt.Errorf("decode photoset status %s: %w", status.PhotosetID, err)
		}
		if err := json.Unmarshal([]byte(fileSizesJSON), &status.FileSizes); err != nil {
			return nil, fmt.Errorf("decode photoset file sizes %s: %w", status.PhotosetID, err)
		}
		status.RootDir = rootDir
		status.OwnerNSID = ownerNSID
		status.Complete = complete != 0
		status.SourceUpdatedAt = sourceUpdatedAt
		status.VerifiedAt = time.Unix(verifiedAt, 0)
		statuses[status.PhotosetID] = &status
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read photoset status rows: %w", err)
	}
	return statuses, nil
}

// PutPhotosetStatus stores or replaces one local verification manifest.
func (s *Store) PutPhotosetStatus(ctx context.Context, status PhotosetStatus) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("cache is not open")
	}
	expectedJSON, err := json.Marshal(status.ExpectedIDs)
	if err != nil {
		return fmt.Errorf("encode photoset status: %w", err)
	}
	fileSizesJSON, err := json.Marshal(status.FileSizes)
	if err != nil {
		return fmt.Errorf("encode photoset file sizes: %w", err)
	}
	verifiedAt := status.VerifiedAt
	if verifiedAt.IsZero() {
		verifiedAt = time.Now()
	}
	complete := 0
	if status.Complete {
		complete = 1
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO photoset_status(
			root_dir, owner_nsid, photoset_id, title, directory,
			expected_ids, complete, source_updated_at, file_sizes, verified_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(root_dir, owner_nsid, photoset_id) DO UPDATE SET
			title = excluded.title,
			directory = excluded.directory,
			expected_ids = excluded.expected_ids,
			complete = excluded.complete,
			source_updated_at = excluded.source_updated_at,
			file_sizes = excluded.file_sizes,
			verified_at = excluded.verified_at`,
		status.RootDir, status.OwnerNSID, status.PhotosetID, status.Title,
		status.Directory, string(expectedJSON), complete, status.SourceUpdatedAt, string(fileSizesJSON), verifiedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("write photoset status: %w", err)
	}
	return nil
}
