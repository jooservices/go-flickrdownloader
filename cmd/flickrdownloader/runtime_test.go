package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jooservices/flickrdownloader/pkg/config"
)

// TestRejectRefreshOffline covers the guard that stops the contradictory
// --refresh/--offline combination from ever reaching the client, rather than
// leaving the outcome to whichever cache check happens to run first.
func TestRejectRefreshOffline(t *testing.T) {
	if err := rejectRefreshOffline(false, false); err != nil {
		t.Fatalf("neither flag set: %v, want nil", err)
	}
	if err := rejectRefreshOffline(true, false); err != nil {
		t.Fatalf("refresh only: %v, want nil", err)
	}
	if err := rejectRefreshOffline(false, true); err != nil {
		t.Fatalf("offline only: %v, want nil", err)
	}
	if err := rejectRefreshOffline(true, true); err == nil {
		t.Fatal("both flags set: expected an error")
	}
}

// TestLockOutputRoot covers the ADR-023 output-root advisory lock: a second
// process (or a re-entrant call) must be refused while the first holds it,
// and releasing must let a subsequent lock succeed.
func TestLockOutputRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "photos")

	unlock, err := lockOutputRoot(root)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}

	if _, err := lockOutputRoot(root); err == nil {
		t.Fatal("second concurrent lock on the same root: expected an error")
	}

	if err := unlock(); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	unlock2, err := lockOutputRoot(root)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	if err := unlock2(); err != nil {
		t.Fatalf("second unlock: %v", err)
	}
}

// TestLockOutputRootDifferentSpellingsCollide proves the lock uses
// CanonicalPath (ADR-023): a relative and an absolute spelling of the same
// directory must serialize against each other, not silently coexist.
func TestLockOutputRootDifferentSpellingsCollide(t *testing.T) {
	base := t.TempDir()
	abs := filepath.Join(base, "photos")

	unlock, err := lockOutputRoot(abs)
	if err != nil {
		t.Fatalf("first lock (absolute): %v", err)
	}
	defer unlock()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	rel, err := filepath.Rel(wd, abs)
	if err != nil {
		t.Skip("cwd is not on the same volume as the temp dir; skipping relative-path case")
	}
	if _, err := lockOutputRoot(rel); err == nil {
		t.Fatal("lock via a relative spelling of the same directory: expected an error (same canonical root)")
	}
}

// newTestConfig builds a minimal, valid Config for the quota/client
// wiring tests below, redirecting the config/quota/cache paths under a
// per-test HOME so nothing touches the real ~/.config/flickrdownloader.
func newTestConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return &config.Config{
		APIKey:               "test-key",
		APISecret:            "test-secret",
		OAuthToken:           "test-token",
		OAuthSecret:          "test-token-secret",
		NSID:                 "123456789@N01",
		APIHourlyLimit:       3600,
		APIRateMS:            1,
		CacheListingTTLHours: 24,
	}
}

// TestNewQuotaTracker covers the quota.Tracker wiring: it must persist to
// the per-API-key path config.QuotaPath derives, and a second tracker built
// from the same config must see usage the first one recorded (proving it's
// reading the same on-disk log, not an independent in-memory one).
func TestNewQuotaTracker(t *testing.T) {
	cfg := newTestConfig(t)

	tr, err := newQuotaTracker(cfg)
	if err != nil {
		t.Fatalf("newQuotaTracker: %v", err)
	}
	if err := tr.Wait(t.Context()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	flushQuotaOnExit(tr)()

	used, limit, _ := tr.Usage()
	if used != 1 || limit != cfg.APIHourlyLimit {
		t.Fatalf("usage = %d/%d, want 1/%d", used, limit, cfg.APIHourlyLimit)
	}

	tr2, err := newQuotaTracker(cfg)
	if err != nil {
		t.Fatalf("second newQuotaTracker: %v", err)
	}
	used2, _, _ := tr2.Usage()
	if used2 != 1 {
		t.Fatalf("second tracker usage = %d, want 1 (shared log path, ADR-015)", used2)
	}
}

// TestNewClientWithCache covers the client wiring: the quota tracker must be
// installed as the rate limiter, and the response cache must open and bind
// at the per-account path config.CachePath derives.
func TestNewClientWithCache(t *testing.T) {
	cfg := newTestConfig(t)

	tr, err := newQuotaTracker(cfg)
	if err != nil {
		t.Fatalf("newQuotaTracker: %v", err)
	}
	defer flushQuotaOnExit(tr)()

	client, store, err := newClientWithCache(cfg, tr, false, false)
	if err != nil {
		t.Fatalf("newClientWithCache: %v", err)
	}
	if client == nil {
		t.Fatal("client is nil")
	}
	if store == nil {
		t.Fatal("expected a response cache store to open successfully")
	}
	defer store.Close()

	blocked, _ := client.QuotaWaiting()
	if blocked {
		t.Fatal("quota should not report blocked immediately after wiring")
	}
}
