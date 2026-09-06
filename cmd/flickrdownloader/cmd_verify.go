package main

import (
	"fmt"

	"github.com/jooservices/flickrdownloader/pkg/api"
	"github.com/jooservices/flickrdownloader/pkg/config"
	"github.com/jooservices/flickrdownloader/pkg/download"
	"github.com/jooservices/flickrdownloader/pkg/ui"
	"github.com/spf13/cobra"
)

func verificationSymbol(state download.VerificationState) (color, symbol string) {
	switch state {
	case download.VerificationComplete:
		return ui.ColorGreen, "✓"
	case download.VerificationIncomplete:
		return ui.ColorYellow, "⚠"
	case download.VerificationStale, download.VerificationError:
		return ui.ColorRed, "✗"
	default: // not-scanned
		return ui.ColorDim, "·"
	}
}

func printVerificationReport(report *download.VerificationReport) {
	fmt.Printf("\n%s\n\n", ui.Bordered("Verify", ui.ColorCyan))

	all := append([]download.PhotosetVerification{}, report.Photosets...)
	if report.Uncategorized != nil {
		all = append(all, *report.Uncategorized)
	}
	for _, v := range all {
		col, sym := verificationSymbol(v.State)
		var detail string
		switch v.State {
		case download.VerificationComplete:
			detail = fmt.Sprintf("%d/%d photos, matches disk", v.Present, v.Expected)
		case download.VerificationIncomplete:
			detail = fmt.Sprintf("%d/%d (%d missing)", v.Present, v.Expected, len(v.Missing))
		case download.VerificationNotScanned:
			detail = "not scanned — run with --refresh for a live check"
		case download.VerificationStale:
			detail = fmt.Sprintf("%d local file(s) no longer match the current source", len(v.Missing))
		case download.VerificationError:
			detail = v.Error
		}
		fmt.Printf("  %s%s%s %-32s %s%s%s\n", col, sym, ui.ColorReset, v.Title, ui.ColorDim, detail, ui.ColorReset)
	}

	if len(report.StaleDirectories) > 0 {
		fmt.Printf("\n  %sStale directories (not in the current album list):%s\n", ui.ColorYellow, ui.ColorReset)
		for _, d := range report.StaleDirectories {
			fmt.Printf("    %s%s%s\n", ui.ColorDim, d, ui.ColorReset)
		}
	}

	fmt.Printf("\n  %s%s%s complete   %s%s%s incomplete   %s· not scanned   %s%s%s stale/error%s\n",
		ui.ColorGreen, "✓", ui.ColorReset,
		ui.ColorYellow, "⚠", ui.ColorReset,
		ui.ColorDim,
		ui.ColorRed, "✗", ui.ColorReset, ui.ColorReset)
}

func runVerify(cmd *cobra.Command, args []string) error {
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

	client, store, err := newClientWithCache(cfg, tr, false, false)
	if err != nil {
		return err
	}
	if store == nil {
		return fmt.Errorf("verify requires the response cache to be available — check that ~/.config/flickrdownloader is writable")
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
		return fmt.Errorf("verify currently supports user URLs only")
	}

	dl := download.New(client, cfg.OutputDir, cfg.WorkerCount)
	dl.Cache = store

	sets, err := client.GetPhotosets(ctx, parsed.ID)
	if err != nil {
		return fmt.Errorf("list photosets: %w", err)
	}

	report, err := dl.VerifyUser(ctx, parsed.ID, sets, verifyRefreshFlag, download.VerifyUserOpts{
		AuthenticatedNSID: cfg.NSID,
	})
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}

	printVerificationReport(report)
	return nil
}
