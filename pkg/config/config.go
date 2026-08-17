package config

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultCacheListingTTLHours is how long cached photo/photoset listing
// responses are considered fresh before a non-offline run re-fetches them.
const DefaultCacheListingTTLHours = 24

type Config struct {
	APIKey               string `json:"api_key"`
	APISecret            string `json:"api_secret"`
	OAuthToken           string `json:"oauth_token"`
	OAuthSecret          string `json:"oauth_token_secret"`
	NSID                 string `json:"nsid,omitempty"`
	OutputDir            string `json:"output_dir,omitempty"`
	WorkerCount          int    `json:"worker_count,omitempty"`
	APIHourlyLimit       int    `json:"api_hourly_limit,omitempty"`
	APIRateMS            int    `json:"api_interval_ms,omitempty"`
	CacheListingTTLHours int    `json:"cache_listing_ttl_hours,omitempty"`
}

func configDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	dir := filepath.Join(home, ".config", "flickrdownloader")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create config dir: %w", err)
	}
	return dir, nil
}

func configPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

func Load() (*Config, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("not authenticated. Run 'flickrdownloader auth' first")
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.WorkerCount <= 0 {
		cfg.WorkerCount = 20
	}
	if cfg.OutputDir == "" {
		cfg.OutputDir = "./photos"
	}
	if cfg.APIHourlyLimit <= 0 {
		cfg.APIHourlyLimit = 3600
	}
	if cfg.APIRateMS <= 0 {
		cfg.APIRateMS = 1050
	}
	if cfg.CacheListingTTLHours <= 0 {
		cfg.CacheListingTTLHours = DefaultCacheListingTTLHours
	}
	return &cfg, nil
}

// QuotaPath returns the per-API-key quota log path. Separate keys get
// separate logs so the hourly budget is tracked independently per key.
func QuotaPath(apiKey string) (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(apiKey))
	return filepath.Join(dir, fmt.Sprintf("quota-%x.log", sum[:4])), nil
}

// CachePath returns the per-account (API key + authenticated NSID) response
// cache database path. Separate accounts get separate databases so cached
// private listings can never leak across accounts sharing this machine.
func CachePath(apiKey, nsid string) (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(apiKey + "|" + nsid))
	return filepath.Join(dir, fmt.Sprintf("cache-%x.db", sum[:8])), nil
}

// AuthFingerprint derives a stable, non-reversible identifier for an OAuth
// access token, used to detect re-authentication as a different account so
// the response cache can be invalidated (see pkg/cache.Store.BindAuth).
func AuthFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", sum)
}

func (c *Config) Save() error {
	path, err := configPath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}
