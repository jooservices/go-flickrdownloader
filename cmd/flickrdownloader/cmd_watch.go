package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jooservices/go-flickrdownloader/pkg/api"
	"github.com/jooservices/go-flickrdownloader/pkg/cache"
	"github.com/jooservices/go-flickrdownloader/pkg/config"
	"github.com/jooservices/go-flickrdownloader/pkg/download"
	"github.com/jooservices/go-flickrdownloader/pkg/watch"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// openWatchStore opens the account cache (hard-fail) and runs the one-time
// YAML/text import. Unlike newClientWithCache, a missing cache is an error
// because the watchlist itself is stored there.
func openWatchStore(cfg *config.Config) (*cache.Store, error) {
	path, err := config.CachePath(cfg.APIKey, cfg.NSID)
	if err != nil {
		return nil, err
	}
	store, err := cache.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open cache: %w", err)
	}
	if err := store.BindAuth(context.Background(), config.AuthFingerprint(cfg.OAuthToken)); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("bind cache to account: %w", err)
	}
	if err := watch.ImportLegacyFiles(context.Background(), store); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("import legacy watchlist: %w", err)
	}
	return store, nil
}

func runWatch(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	watchPath := watchFile

	quiet := watchQuietFlag || !term.IsTerminal(int(os.Stdout.Fd()))

	writers := []io.Writer{os.Stdout}
	if watchLogFile != "" {
		f, err := os.OpenFile(watchLogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return fmt.Errorf("open log file: %w", err)
		}
		defer f.Close()
		writers = append(writers, f)
	}
	logWriteWarned := false
	logf := func(format string, args ...any) {
		line := fmt.Sprintf("%s INFO %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
		for _, w := range writers {
			if _, err := fmt.Fprint(w, line); err != nil && !logWriteWarned {
				logWriteWarned = true
				fmt.Fprintf(os.Stderr, "  warning: write log: %v\n", err)
			}
		}
	}

	outDirDefault := cfg.OutputDir
	if watchOut != "" {
		outDirDefault = watchOut
	}
	workerDefault := cfg.WorkerCount
	if watchWorkersFlag > 0 {
		workerDefault = watchWorkersFlag
	}

	unlock, err := lockOutputRoot(outDirDefault)
	if err != nil {
		return err
	}
	defer unlock()
	lockedRoots := map[string]bool{download.CanonicalPath(outDirDefault): true}

	tr, err := newQuotaTracker(cfg)
	if err != nil {
		return err
	}
	defer flushQuotaOnExit(tr)()

	var client *api.Client
	var store *cache.Store
	if watchFile != "" {
		client, store, err = newClientWithCache(cfg, tr, false, false)
		if err != nil {
			return err
		}
	} else {
		store, err = openWatchStore(cfg)
		if err != nil {
			return err
		}
		client = api.NewClient(cfg.APIKey, cfg.APISecret, cfg.OAuthToken, cfg.OAuthSecret)
		client.SetRateLimiter(tr)
		client.SetResponseCache(store)
		client.SetCacheListingTTL(time.Duration(cfg.CacheListingTTLHours) * time.Hour)
		client.SetCachePolicy(false, false)
	}
	if store != nil {
		defer store.Close()
	}

	ctx, stop := setupSignalContext()
	defer stop()

	sched := &watch.Scheduler{
		Path:         watchPath,
		PollInterval: watchPollInterval,
		Logf:         logf,
		RunSource: func(ctx context.Context, src watch.Source) error {
			srcOut := outDirDefault
			if src.OutputDir != "" {
				srcOut = src.OutputDir
			}
			if canon := download.CanonicalPath(srcOut); !lockedRoots[canon] {
				unlockSrc, lockErr := lockOutputRoot(srcOut)
				if lockErr != nil {
					return lockErr
				}
				defer unlockSrc()
			}
			srcWorkers := workerDefault
			if src.Workers > 0 {
				srcWorkers = src.Workers
			}

			dl := download.New(client, srcOut, srcWorkers)
			dl.Cache = store
			dl.Logf = logf
			if err := dl.WarmLocalIndex(); err != nil {
				logf("source=%s status=index_warning err=%q", src.URL, err.Error())
			}

			albums := src.Albums
			if albums == "" {
				albums = "all"
			}

			return runDownloadOnce(ctx, src.URL, cfg, client, dl, downloadOnceOptions{
				Albums:        albums,
				Uncategorized: src.IncludeOrphans,
				Yes:           true,
				Quiet:         quiet,
			})
		},
	}

	if watchFile == "" {
		storeRef := store
		sched.Load = func() (*watch.Config, error) {
			return watch.LoadFromStore(ctx, storeRef)
		}
		logf("watch starting watchlist=db")
	} else {
		logf("watch starting watchlist=%s", watchPath)
	}
	if err := sched.Loop(ctx); err != nil {
		return fmt.Errorf("watch: %w", err)
	}
	logf("watch stopped")
	return nil
}

func runWatchList(cmd *cobra.Command, args []string) error {
	if watchFile != "" {
		path := watchFile
		urls, err := watch.ListSources(path)
		if err != nil {
			return fmt.Errorf("list watchlist: %w", err)
		}
		if len(urls) == 0 {
			fmt.Printf("watchlist %s has no sources\n", path)
			return nil
		}
		for _, u := range urls {
			fmt.Println(u)
		}
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := openWatchStore(cfg)
	if err != nil {
		return err
	}
	defer store.Close()
	wl, err := watch.LoadFromStore(context.Background(), store)
	if err != nil {
		return fmt.Errorf("list watchlist: %w", err)
	}
	if len(wl.Sources) == 0 {
		fmt.Println("watchlist has no sources")
		return nil
	}
	for _, src := range wl.Sources {
		if src.Enabled {
			fmt.Println(src.URL)
			continue
		}
		fmt.Printf("%s (disabled)\n", src.URL)
	}
	return nil
}

func runWatchAdd(cmd *cobra.Command, args []string) error {
	if watchFile != "" {
		path := watchFile
		added, err := watch.AddSources(path, args)
		if err != nil {
			return fmt.Errorf("add to watchlist: %w", err)
		}
		if len(added) == 0 {
			fmt.Printf("no new URLs added to %s (all already present)\n", path)
			return nil
		}
		for _, u := range added {
			fmt.Printf("added %s\n", u)
		}
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := openWatchStore(cfg)
	if err != nil {
		return err
	}
	defer store.Close()
	added, err := watch.AddToStore(context.Background(), store, args)
	if err != nil {
		return fmt.Errorf("add to watchlist: %w", err)
	}
	if len(added) == 0 {
		fmt.Println("no new URLs added (all already present)")
		return nil
	}
	for _, u := range added {
		fmt.Printf("added %s\n", u)
	}
	return nil
}

func runWatchRemove(cmd *cobra.Command, args []string) error {
	if watchFile != "" {
		path := watchFile
		removed, err := watch.RemoveSources(path, args)
		if err != nil {
			return fmt.Errorf("remove from watchlist: %w", err)
		}
		if len(removed) == 0 {
			fmt.Printf("no URLs removed from %s (none matched)\n", path)
			return nil
		}
		for _, u := range removed {
			fmt.Printf("removed %s\n", u)
		}
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := openWatchStore(cfg)
	if err != nil {
		return err
	}
	defer store.Close()
	removed, err := watch.RemoveFromStore(context.Background(), store, args)
	if err != nil {
		return fmt.Errorf("remove from watchlist: %w", err)
	}
	if len(removed) == 0 {
		fmt.Println("no URLs removed (none matched)")
		return nil
	}
	for _, u := range removed {
		fmt.Printf("removed %s\n", u)
	}
	return nil
}
