package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"github.com/jooservices/go-flickrdownloader/pkg/api"
	"github.com/jooservices/go-flickrdownloader/pkg/cache"
	"github.com/jooservices/go-flickrdownloader/pkg/config"
	"github.com/jooservices/go-flickrdownloader/pkg/download"
	"github.com/jooservices/go-flickrdownloader/pkg/quota"
	"github.com/jooservices/go-flickrdownloader/pkg/ui"
	"golang.org/x/term"
)

// readLine reads one line from r, trimming surrounding whitespace/newline.
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// readSecret reads one line without echoing it to the terminal when stdin is
// a TTY, falling back to a plain read (e.g. piped input in tests/scripts).
func readSecret(r *bufio.Reader) (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	return readLine(r)
}

// rejectRefreshOffline rejects the contradictory combination of forcing a
// live check (--refresh) and forbidding one (--offline) before either flag
// reaches the client, instead of leaving the outcome to whichever cache
// check happens to run first.
func rejectRefreshOffline(refresh, offline bool) error {
	if refresh && offline {
		return fmt.Errorf("--refresh and --offline can't be used together\n" +
			"  --refresh forces a live check against Flickr.\n" +
			"  --offline forbids any live request.\n" +
			"  Pick one.")
	}
	return nil
}

// setupSignalContext returns a context cancelled on SIGINT/SIGTERM and a
// stop func callers must defer to release the signal handler — without it,
// the goroutine below (and the OS-level signal registration) outlives the
// command that started it.
func setupSignalContext() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-sigCh:
			fmt.Printf("\n  %s%s Cancelling — waiting for in-flight downloads to finish...%s\n",
				ui.ColorYellow, ui.IconErr, ui.ColorReset)
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		signal.Stop(sigCh)
		cancel()
	}
}

// lockOutputRoot acquires an advisory flock on rootDir so at most one
// flickrdownloader process (download/verify/watch) operates on it at a time,
// preventing two concurrent runs from racing manifest writes or partial
// downloads (ADR-023).
func lockOutputRoot(rootDir string) (func() error, error) {
	if err := os.MkdirAll(rootDir, 0755); err != nil {
		return nil, fmt.Errorf("create output directory: %w", err)
	}
	rootDir = download.CanonicalPath(rootDir)
	fl := flock.New(filepath.Join(rootDir, ".flickrdownloader.lock"))
	locked, err := fl.TryLock()
	if err != nil {
		return nil, fmt.Errorf("lock output directory: %w", err)
	}
	if !locked {
		return nil, fmt.Errorf("output directory %s is already in use by another flickrdownloader process", rootDir)
	}
	return fl.Unlock, nil
}

// newQuotaTracker builds the persisted per-API-key quota tracker from cfg.
func newQuotaTracker(cfg *config.Config) (*quota.Tracker, error) {
	qPath, err := config.QuotaPath(cfg.APIKey)
	if err != nil {
		return nil, err
	}
	tr, err := quota.New(qPath, cfg.APIHourlyLimit, time.Duration(cfg.APIRateMS)*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("init quota tracker: %w", err)
	}
	return tr, nil
}

func flushQuotaOnExit(tr *quota.Tracker) func() {
	return func() {
		if err := tr.Flush(); err != nil {
			fmt.Fprintf(os.Stderr, "  %s%s flush quota log: %v%s\n", ui.ColorRed, ui.IconErr, err, ui.ColorReset)
		}
	}
}

// newClientWithCache builds an API client wired to the persisted quota
// tracker and, when available, the account-scoped response cache (ADR-019).
// The cache is an optimization only: if it can't be opened (e.g. read-only
// filesystem), the run continues without one rather than failing.
func newClientWithCache(cfg *config.Config, tr *quota.Tracker, refresh, offline bool) (*api.Client, *cache.Store, error) {
	client := api.NewClient(cfg.APIKey, cfg.APISecret, cfg.OAuthToken, cfg.OAuthSecret)
	client.SetRateLimiter(tr)

	cachePath, err := config.CachePath(cfg.APIKey, cfg.NSID)
	if err != nil {
		return client, nil, fmt.Errorf("resolve cache path: %w", err)
	}
	store, err := cache.Open(cachePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  %s%s response cache unavailable, continuing without it: %v%s\n",
			ui.ColorYellow, ui.IconErr, err, ui.ColorReset)
		return client, nil, nil
	}
	if err := store.BindAuth(context.Background(), config.AuthFingerprint(cfg.OAuthToken)); err != nil {
		fmt.Fprintf(os.Stderr, "  %s%s bind cache to account: %v%s\n", ui.ColorYellow, ui.IconErr, err, ui.ColorReset)
	}
	client.SetResponseCache(store)
	client.SetCacheListingTTL(time.Duration(cfg.CacheListingTTLHours) * time.Hour)
	client.SetCachePolicy(refresh, offline)
	return client, store, nil
}
