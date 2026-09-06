package main

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// version is stamped at build time via
//
//	go build -ldflags "-X main.version=v1.2.3"
var version = "dev"

const uncategorizedID = "__uncategorized__"

var (
	urlFlag         string
	outDir          string
	workers         int
	dryRun          bool
	yesFlag         bool
	albumsFlag      string
	uncategorized   bool
	updateCheckOnly bool
	refreshFlag     bool
	forceFlag       bool
	offlineFlag     bool

	verifyRefreshFlag bool
	scanOfflineFlag   bool

	watchFile         string
	watchPollInterval time.Duration
	watchOut          string
	watchWorkersFlag  int
	watchLogFile      string
	watchQuietFlag    bool
)

func main() {
	rootCmd := &cobra.Command{
		Use:     "flickrdownloader",
		Short:   "Download all photos from a Flickr user or photoset",
		Version: version,
	}

	authCmd := &cobra.Command{
		Use:   "auth",
		Short: "Authenticate with Flickr (one-time setup)",
		RunE:  runAuth,
	}

	downloadCmd := &cobra.Command{
		Use:   "download -u <flickr-url>",
		Short: "Download photos from a Flickr URL",
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return rejectRefreshOffline(refreshFlag || forceFlag, offlineFlag)
		},
		RunE: runDownload,
	}
	downloadCmd.Flags().StringVarP(&urlFlag, "url", "u", "", "Flickr URL (user / album / photo)")
	downloadCmd.Flags().StringVarP(&outDir, "out", "o", "", "Output directory (default: from config, normally ./photos)")
	downloadCmd.Flags().IntVarP(&workers, "workers", "w", 0, "Number of concurrent download workers (default: 20)")
	downloadCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview what would be downloaded (album tree, counts, estimated size) without downloading")
	downloadCmd.Flags().BoolVarP(&yesFlag, "yes", "y", false, "Skip the confirmation prompt and the album picker (download everything)")
	downloadCmd.Flags().StringVar(&albumsFlag, "albums", "", "Only download matching albums (comma-separated names, 'all' or 'none')")
	downloadCmd.Flags().BoolVar(&uncategorized, "uncategorized", true, "Include photos that are not in any album")
	downloadCmd.Flags().BoolVar(&refreshFlag, "refresh", false, "Bypass cached metadata and completion manifests, re-list every album from Flickr")
	downloadCmd.Flags().BoolVar(&forceFlag, "force", false, "Alias for --refresh")
	downloadCmd.Flags().BoolVar(&offlineFlag, "offline", false, "Use cached Flickr responses only; never make a live request")

	verifyCmd := &cobra.Command{
		Use:     "verify -u <flickr-url>",
		Short:   "Check downloaded photos against local completion manifests without downloading",
		PreRunE: func(cmd *cobra.Command, args []string) error { return rejectRefreshOffline(verifyRefreshFlag, false) },
		RunE:    runVerify,
	}
	verifyCmd.Flags().StringVarP(&urlFlag, "url", "u", "", "Flickr URL (user)")
	verifyCmd.Flags().StringVarP(&outDir, "out", "o", "", "Output directory (default: from config, normally ./photos)")
	verifyCmd.Flags().BoolVar(&verifyRefreshFlag, "refresh", false, "Re-check the current Flickr listing instead of trusting cached manifests")

	scanCmd := &cobra.Command{
		Use:   "scan -u <flickr-url>",
		Short: "Index already-downloaded files into completion manifests without downloading",
		Long: "Walk the output directory and write photoset completion manifests so a later download\n" +
			"can skip re-listing albums that still match disk. By default this calls photosets.getList\n" +
			"(cheap) to bind folders to album IDs and date_update. --offline writes disk-only manifests\n" +
			"and cannot skip listing until a later run records Flickr's date_update.",
		RunE: runScan,
	}
	scanCmd.Flags().StringVarP(&urlFlag, "url", "u", "", "Flickr URL (user)")
	scanCmd.Flags().StringVarP(&outDir, "out", "o", "", "Output directory (default: from config, normally ./photos)")
	scanCmd.Flags().BoolVar(&scanOfflineFlag, "offline", false, "Index disk only; do not call Flickr (manifests will not skip listing until date_update is known)")

	watchCmd := &cobra.Command{
		Use:   "watch",
		Short: "Continuously poll a watchlist and download new photos, forever",
		Long: "Poll a watchlist of Flickr URLs on an interval, downloading anything new and waiting\n" +
			"through quota exhaustion instead of exiting. Runs until interrupted (Ctrl-C / SIGTERM).\n" +
			"The default watchlist lives in the account cache database; pass --file to use a YAML or text file.",
		RunE: runWatch,
	}
	watchCmd.Flags().StringVar(&watchFile, "file", "", "Watchlist file (optional YAML/text override; default: the account cache database)")
	watchCmd.Flags().DurationVar(&watchPollInterval, "poll-interval", 0, "Time between full cycles (default: from the watchlist (DB or --file), or 30m)")
	watchCmd.Flags().StringVar(&watchOut, "out", "", "Output directory (default: from config, normally ./photos)")
	watchCmd.Flags().IntVar(&watchWorkersFlag, "workers", 0, "Worker count (default: from config)")
	watchCmd.Flags().StringVar(&watchLogFile, "log-file", "", "Append watch's log lines to this file in addition to stdout")
	watchCmd.Flags().BoolVar(&watchQuietFlag, "quiet", false, "Structured log lines instead of the progress bar (default: on automatically when stdout isn't a terminal)")

	watchListCmd := &cobra.Command{
		Use:   "list",
		Short: "List the URLs in the watchlist",
		RunE:  runWatchList,
	}
	watchListCmd.Flags().StringVar(&watchFile, "file", "", "Watchlist file (optional YAML/text override; default: the account cache database)")

	watchAddCmd := &cobra.Command{
		Use:     "add <flickr-url> [more...]",
		Short:   "Add Flickr URLs to the watchlist",
		Args:    cobra.MinimumNArgs(1),
		Example: "  flickrdownloader watch add https://www.flickr.com/photos/bob/",
		RunE:    runWatchAdd,
	}
	watchAddCmd.Flags().StringVar(&watchFile, "file", "", "Watchlist file (optional YAML/text override; default: the account cache database)")

	watchRemoveCmd := &cobra.Command{
		Use:     "remove <flickr-url> [more...]",
		Aliases: []string{"rm"},
		Short:   "Remove Flickr URLs from the watchlist",
		Args:    cobra.MinimumNArgs(1),
		Example: "  flickrdownloader watch remove https://www.flickr.com/photos/bob/",
		RunE:    runWatchRemove,
	}
	watchRemoveCmd.Flags().StringVar(&watchFile, "file", "", "Watchlist file (optional YAML/text override; default: the account cache database)")

	watchCmd.AddCommand(watchListCmd, watchAddCmd, watchRemoveCmd)

	cacheCmd := &cobra.Command{
		Use:   "cache",
		Short: "Manage the local Flickr response cache",
	}
	cachePruneCmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove expired cache entries (photoset completion manifests are kept)",
		RunE:  runCachePrune,
	}
	cacheClearCmd := &cobra.Command{
		Use:   "clear",
		Short: "Remove all cached responses, metadata, and completion manifests for this account (the watchlist is kept)",
		RunE:  runCacheClear,
	}
	cacheCmd.AddCommand(cachePruneCmd, cacheClearCmd)

	updateCmd := &cobra.Command{
		Use:   "update",
		Short: "Check for updates and install the latest release",
		RunE:  runUpdate,
	}
	updateCmd.Flags().BoolVar(&updateCheckOnly, "check", false, "Only check for a newer version, do not install")

	quotaCmd := &cobra.Command{
		Use:   "quota",
		Short: "Show Flickr API quota usage for this hour",
		RunE:  runQuota,
	}

	completionCmd := &cobra.Command{
		Use:       "completion [bash|zsh|fish|powershell]",
		Short:     "Generate shell completion script",
		Long:      "Generate a shell completion script for flickrdownloader.\n\n  bash:       source <(flickrdownloader completion bash)\n  zsh:        source <(flickrdownloader completion zsh)\n  fish:       flickrdownloader completion fish | source\n  powershell: flickrdownloader completion powershell | Out-String | Invoke-Expression",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return rootCmd.GenBashCompletionV2(os.Stdout, true)
			case "zsh":
				return rootCmd.GenZshCompletion(os.Stdout)
			case "fish":
				return rootCmd.GenFishCompletion(os.Stdout, true)
			case "powershell":
				return rootCmd.GenPowerShellCompletionWithDesc(os.Stdout)
			}
			return fmt.Errorf("unsupported shell: %s (supported: bash, zsh, fish, powershell)", args[0])
		},
	}

	rootCmd.AddCommand(authCmd)
	rootCmd.AddCommand(downloadCmd)
	rootCmd.AddCommand(verifyCmd)
	rootCmd.AddCommand(scanCmd)
	rootCmd.AddCommand(watchCmd)
	rootCmd.AddCommand(cacheCmd)
	rootCmd.AddCommand(updateCmd)
	rootCmd.AddCommand(quotaCmd)
	rootCmd.AddCommand(completionCmd)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
