package main

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/jooservices/go-flickrdownloader/pkg/config"
	"github.com/jooservices/go-flickrdownloader/pkg/ui"
	"github.com/jooservices/go-flickrdownloader/pkg/update"
	"github.com/spf13/cobra"
)

func runUpdate(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	fmt.Printf("  %sChecking for updates...%s\n", ui.ColorDim, ui.ColorReset)
	rel, err := update.LatestRelease(ctx)
	if err != nil {
		return fmt.Errorf("check for updates: %w\n  hint: check your connection; GitHub allows 60 unauthenticated requests/hour", err)
	}
	latest := rel.TagName
	if latest == "" {
		return fmt.Errorf("latest release has no version tag")
	}

	fmt.Printf("  Current: %s\n  Latest:  %s\n", version, latest)

	if !update.IsNewer(version, latest) {
		fmt.Printf("  %s%s Already up to date.%s\n", ui.ColorGreen, ui.IconOk, ui.ColorReset)
		return nil
	}

	if updateCheckOnly {
		fmt.Printf("  %sA newer version is available — run '%s' to install it.%s\n",
			ui.ColorYellow, "flickrdownloader update", ui.ColorReset)
		return nil
	}

	asset := rel.AssetFor()
	if asset == nil {
		return fmt.Errorf("no prebuilt binary for %s/%s in release %s — build from source instead",
			runtime.GOOS, runtime.GOARCH, latest)
	}

	fmt.Printf("  Downloading %s (%.1f MB)...\n", asset.Name, float64(asset.Size)/1e6)
	if err := update.Install(ctx, rel, asset); err != nil {
		return err
	}
	fmt.Printf("  %s%s Updated to %s%s\n", ui.ColorGreen, ui.IconOk, latest, ui.ColorReset)
	return nil
}

func runQuota(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	tr, err := newQuotaTracker(cfg)
	if err != nil {
		return err
	}

	s := tr.Snapshot()
	pct := 0.0
	if s.Limit > 0 {
		pct = float64(s.Used) / float64(s.Limit) * 100
	}
	col := ui.ColorGreen
	if s.Used*10 >= s.Limit*8 {
		col = ui.ColorYellow
	}
	if s.Used >= s.Limit {
		col = ui.ColorRed
	}

	fmt.Printf("\n%s\n\n", ui.Bordered("API Quota", ui.ColorCyan))
	fmt.Printf("  %sUsed:%s      %d / %d  (%.0f%%)\n", ui.ColorDim, ui.ColorReset, s.Used, s.Limit, pct)
	if s.Waiting && !s.WaitUntil.IsZero() {
		fmt.Printf("  %sBlocked:%s   %squota full — next slot in %s%s\n",
			ui.ColorDim, ui.ColorReset, ui.ColorYellow, ui.FormatDuration(time.Until(s.WaitUntil)), ui.ColorReset)
	}
	if s.ResetAt.IsZero() {
		fmt.Printf("  %sWindow:%s    empty — full budget available\n", ui.ColorDim, ui.ColorReset)
	} else if wait := time.Until(s.ResetAt); wait > 0 {
		fmt.Printf("  %sRolls:%s     oldest request expires in %s%s%s\n",
			ui.ColorDim, ui.ColorReset, col, ui.FormatDuration(wait), ui.ColorReset)
	}
	fmt.Printf("\n  %s● green <80%%   ● yellow 80–99%%   ● red at cap, requests pause%s\n", ui.ColorDim, ui.ColorReset)
	return nil
}
