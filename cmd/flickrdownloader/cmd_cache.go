package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jooservices/flickrdownloader/pkg/api"
	"github.com/jooservices/flickrdownloader/pkg/cache"
	"github.com/jooservices/flickrdownloader/pkg/config"
	"github.com/jooservices/flickrdownloader/pkg/ui"
	"github.com/spf13/cobra"
)

func pluralIES(n int64) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

func runCachePrune(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	path, err := config.CachePath(cfg.APIKey, cfg.NSID)
	if err != nil {
		return err
	}
	store, err := cache.Open(path)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}
	defer store.Close()

	listingTTL := time.Duration(cfg.CacheListingTTLHours) * time.Hour
	removed, err := store.Prune(context.Background(), time.Now(), listingTTL, api.DefaultDetailTTL)
	if err != nil {
		return fmt.Errorf("prune cache: %w", err)
	}
	fmt.Printf("  %s%s Pruned %d expired cache entr%s%s\n", ui.ColorGreen, ui.IconOk, removed, pluralIES(removed), ui.ColorReset)
	return nil
}

func runCacheClear(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	path, err := config.CachePath(cfg.APIKey, cfg.NSID)
	if err != nil {
		return err
	}
	store, err := cache.Open(path)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}
	defer store.Close()

	removed, err := store.Clear(context.Background())
	if err != nil {
		return fmt.Errorf("clear cache: %w", err)
	}
	fmt.Printf("  %s%s Cleared %d cache entr%s%s\n", ui.ColorGreen, ui.IconOk, removed, pluralIES(removed), ui.ColorReset)
	return nil
}
