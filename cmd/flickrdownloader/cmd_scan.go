package main

import (
	"fmt"

	"github.com/jooservices/flickrdownloader/pkg/api"
	"github.com/jooservices/flickrdownloader/pkg/config"
	"github.com/jooservices/flickrdownloader/pkg/download"
	"github.com/jooservices/flickrdownloader/pkg/ui"
	"github.com/spf13/cobra"
)

func runScan(cmd *cobra.Command, args []string) error {
	if err := requireURL(); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if outDir != "" {
		cfg.OutputDir = outDir
	}

	tr, err := newQuotaTracker(cfg)
	if err != nil {
		return err
	}
	defer flushQuotaOnExit(tr)()

	client, store, err := newClientWithCache(cfg, tr, false, scanOfflineFlag)
	if err != nil {
		return err
	}
	if store == nil {
		return fmt.Errorf("scan requires the response cache to be available — check that ~/.config/flickrdownloader is writable")
	}
	defer store.Close()

	unlock, err := lockOutputRoot(cfg.OutputDir)
	if err != nil {
		return err
	}
	defer unlock()

	ctx, stop := setupSignalContext()
	defer stop()

	parsed, err := api.ResolveURL(ctx, urlFlag, client)
	if err != nil {
		return fmt.Errorf("resolve URL '%s': %w", urlFlag, err)
	}
	if parsed.Type != api.TargetUser {
		return fmt.Errorf("scan currently supports user URLs only")
	}

	dl := download.New(client, cfg.OutputDir, cfg.WorkerCount)
	dl.Cache = store

	var sets []api.PhotoSetInfo
	if !scanOfflineFlag {
		listed, err := client.GetPhotosets(ctx, parsed.ID)
		if err != nil {
			return fmt.Errorf("list photosets: %w", err)
		}
		sets = listed
	}

	report, err := dl.ScanUser(ctx, parsed.ID, sets)
	if err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	printScanReport(report, scanOfflineFlag)
	return nil
}

func printScanReport(report *download.ScanReport, offline bool) {
	fmt.Printf("\n%s\n\n", ui.Bordered("Scan", ui.ColorCyan))
	if offline {
		fmt.Printf("  %sDisk-only — manifests cannot skip listing until Flickr date_update is recorded%s\n\n",
			ui.ColorYellow, ui.ColorReset)
	}

	for _, v := range report.Photosets {
		col, sym := ui.ColorGreen, "✓"
		detail := fmt.Sprintf("%d files on disk", v.Present)
		if v.ExpectedRemote > 0 || v.Present == 0 {
			detail = fmt.Sprintf("%d/%d files on disk", v.Present, v.ExpectedRemote)
		}
		if !v.Complete {
			col, sym = ui.ColorYellow, "⚠"
			if v.ExpectedRemote > v.Present {
				detail += fmt.Sprintf(" (%d missing vs Flickr)", v.ExpectedRemote-v.Present)
			}
		}
		fmt.Printf("  %s%s%s %-32s %s%s%s\n", col, sym, ui.ColorReset, v.Title, ui.ColorDim, detail, ui.ColorReset)
	}
	if report.Uncategorized != nil {
		fmt.Printf("  %s·%s %-32s %s%d uncategorized files%s\n",
			ui.ColorDim, ui.ColorReset, report.Uncategorized.Title, ui.ColorDim, report.Uncategorized.Present, ui.ColorReset)
	}
	for _, v := range report.Unmatched {
		fmt.Printf("  %s·%s %-32s %s%d files, folder not matched to a Flickr album%s\n",
			ui.ColorDim, ui.ColorReset, v.Title, ui.ColorDim, v.Present, ui.ColorReset)
	}
	if len(report.Photosets) == 0 && report.Uncategorized == nil && len(report.Unmatched) == 0 {
		fmt.Printf("  %sno downloaded photos found under the output directory%s\n", ui.ColorDim, ui.ColorReset)
	}
	fmt.Println()
}
