package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"github.com/jooservices/flickrdownloader/pkg/api"
	"github.com/jooservices/flickrdownloader/pkg/cache"
	"github.com/jooservices/flickrdownloader/pkg/config"
	"github.com/jooservices/flickrdownloader/pkg/download"
	"github.com/jooservices/flickrdownloader/pkg/quota"
	"github.com/jooservices/flickrdownloader/pkg/ui"
	"github.com/jooservices/flickrdownloader/pkg/update"
	"github.com/jooservices/flickrdownloader/pkg/watch"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// version is stamped at build time via
//
//	go build -ldflags "-X main.version=v1.2.3"
var version = "dev"

const uncategorizedID = "__uncategorized__"

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
	offlineFlag     bool

	verifyRefreshFlag bool

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
		Use:     "download -u <flickr-url>",
		Short:   "Download photos from a Flickr URL",
		PreRunE: func(cmd *cobra.Command, args []string) error { return rejectRefreshOffline(refreshFlag, offlineFlag) },
		RunE:    runDownload,
	}
	downloadCmd.Flags().StringVarP(&urlFlag, "url", "u", "", "Flickr URL (user / album / photo)")
	downloadCmd.Flags().StringVarP(&outDir, "out", "o", "", "Output directory (default: from config, normally ./photos)")
	downloadCmd.Flags().IntVarP(&workers, "workers", "w", 0, "Number of concurrent download workers (default: 20)")
	downloadCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview what would be downloaded (album tree, counts, estimated size) without downloading")
	downloadCmd.Flags().BoolVarP(&yesFlag, "yes", "y", false, "Skip the confirmation prompt and the album picker (download everything)")
	downloadCmd.Flags().StringVar(&albumsFlag, "albums", "", "Only download matching albums (comma-separated names, 'all' or 'none')")
	downloadCmd.Flags().BoolVar(&uncategorized, "uncategorized", true, "Include photos that are not in any album")
	downloadCmd.Flags().BoolVar(&refreshFlag, "refresh", false, "Bypass cached metadata and completion manifests, re-check everything against Flickr")
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

	watchCmd := &cobra.Command{
		Use:   "watch",
		Short: "Continuously poll a watchlist and download new photos, forever",
		Long: "Poll a watchlist file of Flickr URLs on an interval, downloading anything new and waiting\n" +
			"through quota exhaustion instead of exiting. Runs until interrupted (Ctrl-C / SIGTERM).",
		RunE: runWatch,
	}
	watchCmd.Flags().StringVar(&watchFile, "file", "", "Watchlist file (default: ~/.config/flickrdownloader/watchlist.yaml, then sources.txt)")
	watchCmd.Flags().DurationVar(&watchPollInterval, "poll-interval", 0, "Time between full cycles (default: from the watchlist file, or 30m)")
	watchCmd.Flags().StringVar(&watchOut, "out", "", "Output directory (default: from config, normally ./photos)")
	watchCmd.Flags().IntVar(&watchWorkersFlag, "workers", 0, "Worker count (default: from config)")
	watchCmd.Flags().StringVar(&watchLogFile, "log-file", "", "Append watch's log lines to this file in addition to stdout")
	watchCmd.Flags().BoolVar(&watchQuietFlag, "quiet", false, "Structured log lines instead of the progress bar (default: on automatically when stdout isn't a terminal)")

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
		Short: "Remove all cached responses, metadata, and completion manifests for this account",
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
	rootCmd.AddCommand(watchCmd)
	rootCmd.AddCommand(cacheCmd)
	rootCmd.AddCommand(updateCmd)
	rootCmd.AddCommand(quotaCmd)
	rootCmd.AddCommand(completionCmd)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
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

func runAuth(cmd *cobra.Command, args []string) error {
	fmt.Printf("\n%s\n\n", ui.Bordered("Flickr Downloader", ui.ColorCyan))
	fmt.Printf("  %sStep 1%s  Get an API key from:\n", ui.ColorBold, ui.ColorReset)
	fmt.Println("          https://www.flickr.com/services/apps/create/apply/")
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)

	fmt.Printf("  %sAPI Key:%s    ", ui.ColorCyan, ui.ColorReset)
	apiKey, err := readLine(reader)
	if err != nil {
		return fmt.Errorf("read API key: %w", err)
	}

	fmt.Printf("  %sAPI Secret:%s ", ui.ColorCyan, ui.ColorReset)
	apiSecret, err := readSecret(reader)
	if err != nil {
		return fmt.Errorf("read API secret: %w", err)
	}

	if apiKey == "" || apiSecret == "" {
		return fmt.Errorf("API key and secret are required")
	}

	fmt.Printf("\n  %sStep 2%s  %sRequesting OAuth token...%s\n", ui.ColorBold, ui.ColorReset, ui.ColorDim, ui.ColorReset)
	reqToken, reqSecret, err := api.GetRequestToken(apiKey, apiSecret)
	if err != nil {
		return fmt.Errorf("get request token: %w", err)
	}

	authURL := api.GetAuthorizeURL(reqToken)
	fmt.Printf("\n  %sStep 3%s  Open this URL in your browser:\n", ui.ColorBold, ui.ColorReset)
	fmt.Printf("          %s%s%s\n", ui.ColorCyan, authURL, ui.ColorReset)
	fmt.Printf("\n  %sStep 4%s  After authorizing, paste the 9-digit code here: ", ui.ColorBold, ui.ColorReset)

	verifier, err := readLine(reader)
	if err != nil {
		return fmt.Errorf("read verifier: %w", err)
	}

	if verifier == "" {
		return fmt.Errorf("verifier is required")
	}

	fmt.Printf("\n  %sStep 5%s  %sExchanging for access token...%s\n", ui.ColorBold, ui.ColorReset, ui.ColorDim, ui.ColorReset)
	accessToken, accessSecret, userNSID, username, err := api.GetAccessToken(apiKey, apiSecret, reqToken, reqSecret, verifier)
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}

	cfg := &config.Config{
		APIKey:               apiKey,
		APISecret:            apiSecret,
		OAuthToken:           accessToken,
		OAuthSecret:          accessSecret,
		NSID:                 userNSID,
		WorkerCount:          20,
		OutputDir:            "./photos",
		APIHourlyLimit:       quota.DefaultHourlyLimit,
		APIRateMS:            int(quota.DefaultInterval / time.Millisecond),
		CacheListingTTLHours: config.DefaultCacheListingTTLHours,
	}

	if err := cfg.Save(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	fmt.Printf("\n  %s%s Authenticated as %s (NSID: %s)%s\n", ui.ColorGreen, ui.IconOk, username, userNSID, ui.ColorReset)
	fmt.Printf("  %sConfig saved.%s\n\n", ui.ColorDim, ui.ColorReset)
	fmt.Printf("  Next steps:\n")
	fmt.Printf("    %-42s %sone-time download%s\n", "flickrdownloader download -u <url>", ui.ColorDim, ui.ColorReset)
	fmt.Printf("    %-42s %sauto-download forever%s\n", "flickrdownloader watch --file <path>", ui.ColorDim, ui.ColorReset)
	return nil
}

func confirm(prompt string) bool {
	reader := bufio.NewReader(os.Stdin)
	fmt.Printf("\n  %s%s [Y/n]%s ", ui.ColorBold, prompt, ui.ColorReset)
	line, err := readLine(reader)
	if err != nil {
		return false
	}
	return line == "" || strings.EqualFold(line, "y") || strings.EqualFold(line, "yes")
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

// matchSets maps a --albums spec ("all", "none", or comma-separated names,
// exact or substring, case-insensitive) onto the fetched album list.
func matchSets(sets []api.PhotoSetInfo, spec string) ([]api.PhotoSetInfo, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("--albums requires a value (names, 'all' or 'none')")
	}
	if strings.EqualFold(spec, "all") {
		return sets, nil
	}
	if strings.EqualFold(spec, "none") {
		return []api.PhotoSetInfo{}, nil
	}

	names := []string{}
	for _, s := range sets {
		names = append(names, s.Title.Content)
	}

	var out []api.PhotoSetInfo
	seen := map[string]bool{}
	for _, name := range strings.Split(spec, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		found := false
		lower := strings.ToLower(name)
		for _, s := range sets {
			matches := strings.EqualFold(s.Title.Content, name) ||
				strings.Contains(strings.ToLower(s.Title.Content), lower)
			if matches && !seen[s.ID] {
				out = append(out, s)
				seen[s.ID] = true
			}
			if matches {
				found = true
			}
		}
		if !found {
			suggestions := closestAlbumNames(names, name, 3)
			if len(suggestions) > 0 {
				return nil, fmt.Errorf("no album matches '%s'\n\n  Did you mean:\n    %s\n\n  Run with --dry-run to see the full album list.",
					name, strings.Join(suggestions, "\n    "))
			}
			return nil, fmt.Errorf("no album matches '%s' — available albums:\n    %s",
				name, strings.Join(names, "\n    "))
		}
	}
	return out, nil
}

// closestAlbumNames returns up to n names ranked by edit distance to query,
// used to suggest a fix for a typo'd --albums name instead of dumping the
// full album list.
func closestAlbumNames(names []string, query string, n int) []string {
	type scored struct {
		name string
		dist int
	}
	q := strings.ToLower(query)
	scores := make([]scored, 0, len(names))
	for _, name := range names {
		scores = append(scores, scored{name, levenshtein(strings.ToLower(name), q)})
	}
	sort.Slice(scores, func(i, j int) bool { return scores[i].dist < scores[j].dist })
	out := make([]string, 0, n)
	for i := 0; i < len(scores) && i < n; i++ {
		out = append(out, scores[i].name)
	}
	return out
}

func levenshtein(a, b string) int {
	la, lb := len(a), len(b)
	d := make([]int, lb+1)
	for j := range d {
		d[j] = j
	}
	for i := 1; i <= la; i++ {
		prev := d[0]
		d[0] = i
		for j := 1; j <= lb; j++ {
			temp := d[j]
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d[j] = minInt(d[j]+1, minInt(d[j-1]+1, prev+cost))
			prev = temp
		}
	}
	return d[lb]
}

func minInt(a, b int) int {
	if b < a {
		return b
	}
	return a
}

// downloadOnceOptions controls one download pass, shared by the `download`
// command's single run and each `watch` cycle's per-source run.
type downloadOnceOptions struct {
	Albums        string // "" triggers the interactive picker when eligible; otherwise a matchSets spec
	Uncategorized bool
	DryRun        bool
	Yes           bool // skip the picker/confirmation (always true for watch)
	Quiet         bool // suppress interactive/TTY-only output (always true for watch's default)
}

// runDownloadOnce resolves urlStr and downloads it, sharing the resolve +
// dispatch + report logic between the `download` command and every watch
// cycle's per-source run instead of duplicating it.
func runDownloadOnce(ctx context.Context, urlStr string, cfg *config.Config, client *api.Client, dl *download.Downloader, opts downloadOnceOptions) error {
	dl.Quiet = opts.Quiet

	parsed, err := api.ResolveURL(ctx, urlStr, client)
	if err != nil {
		return fmt.Errorf("resolve URL '%s': %w", urlStr, err)
	}

	if !opts.Quiet {
		fmt.Printf("\n  %sURL:%s    %s\n", ui.ColorDim, ui.ColorReset, urlStr)
		printQuotaHeader(client)
	}

	var runErr error
	switch parsed.Type {
	case api.TargetPhoto:
		info, err := client.GetPhotoInfo(ctx, parsed.ID)
		if err != nil {
			return fmt.Errorf("get photo info: %w", err)
		}
		if !opts.Quiet {
			fmt.Printf("  %sType:%s   Photo\n", ui.ColorDim, ui.ColorReset)
			fmt.Printf("  %sTitle:%s  %s\n", ui.ColorDim, ui.ColorReset, info.Photo.Title.Content)
			fmt.Printf("  %sOwner:%s  %s (%s)\n", ui.ColorDim, ui.ColorReset, info.Photo.Owner.Username, info.Photo.Owner.NSID)
			fmt.Printf("  %sMedia:%s  %s\n", ui.ColorDim, ui.ColorReset, info.Photo.Media)
			fmt.Printf("  %sOutput:%s %s/%s/\n\n", ui.ColorDim, ui.ColorReset, dl.OutDir, info.Photo.Owner.NSID)
		}
		if opts.DryRun {
			if !opts.Quiet {
				fmt.Printf("  %s%s Dry run — nothing downloaded.%s\n", ui.ColorYellow, ui.IconSpark, ui.ColorReset)
			}
			return nil
		}
		runErr = dl.DownloadPhoto(ctx, parsed.ID)

	case api.TargetPhotoset:
		psInfo, err := client.GetPhotosetInfo(ctx, parsed.ID)
		if err != nil {
			return fmt.Errorf("get photoset info: %w", err)
		}
		if !opts.Quiet {
			fmt.Printf("  %sType:%s   Album\n", ui.ColorDim, ui.ColorReset)
			fmt.Printf("  %sTitle:%s  %s\n", ui.ColorDim, ui.ColorReset, psInfo.Title.Content)
			fmt.Printf("  %sOwner:%s  %s\n", ui.ColorDim, ui.ColorReset, psInfo.Owner)
			fmt.Printf("  %sPhotos:%s %d\n", ui.ColorDim, ui.ColorReset, int(psInfo.Photos))
			fmt.Printf("  %sOutput:%s %s/%s/%s/\n\n", ui.ColorDim, ui.ColorReset, dl.OutDir, psInfo.Owner, download.SafeName(psInfo.Title.Content))
		}
		if opts.DryRun {
			if !opts.Quiet {
				fmt.Printf("  %s%s Dry run — nothing downloaded.%s\n", ui.ColorYellow, ui.IconSpark, ui.ColorReset)
			}
			return nil
		}
		if !opts.Quiet {
			fmt.Printf("  %sDownloading...%s\n", ui.ColorDim, ui.ColorReset)
		}
		_, runErr = dl.DownloadByPhotoset(ctx, parsed.ID)

	case api.TargetUser:
		runErr = runUserDownload(ctx, dl, client, parsed.ID, cfg, opts)
	}

	if !opts.Quiet {
		fmt.Print(ui.Breakdown(dl.AlbumStats()))
		printRunSummary(client)
	}
	return runErr
}

// printQuotaHeader shows the persisted hourly usage at the start of a run.
func printQuotaHeader(client *api.Client) {
	used, limit, resetAt := client.Quota()
	if limit <= 0 {
		return
	}
	col := ui.ColorGreen
	if used*10 >= limit*8 {
		col = ui.ColorYellow
	}
	fmt.Printf("  %sAPI quota:%s %d/%d used this hour", col, ui.ColorReset, used, limit)
	if used > 0 && !resetAt.IsZero() {
		if wait := time.Until(resetAt); wait > 0 {
			fmt.Printf(" · %soldest request expires in %s%s", ui.ColorDim, ui.FormatDuration(wait), ui.ColorReset)
		}
	}
	fmt.Println()
}

// printRunSummary reports the API cost of the finished run.
func printRunSummary(client *api.Client) {
	n := client.RequestCount()
	if n == 0 {
		return
	}
	line := fmt.Sprintf("\n  %sAPI calls this run:%s %d", ui.ColorDim, ui.ColorReset, n)
	if used, limit, _ := client.Quota(); limit > 0 {
		line += fmt.Sprintf(" · %d/%d used this hour (%d left)",
			used, limit, max(limit-used, 0))
	}
	if hits := client.CacheHits(); hits > 0 {
		line += fmt.Sprintf(" · %d served from cache", hits)
	}
	fmt.Println(line)
}

func runUserDownload(ctx context.Context, dl *download.Downloader, client *api.Client, nsid string, cfg *config.Config, opts downloadOnceOptions) error {
	if !opts.Quiet {
		fmt.Printf("  %sType:%s   User\n", ui.ColorDim, ui.ColorReset)
		fmt.Printf("  %sNSID:%s   %s\n", ui.ColorDim, ui.ColorReset, nsid)
		fmt.Printf("  %sOutput:%s %s/%s/\n", ui.ColorDim, ui.ColorReset, dl.OutDir, nsid)
		fmt.Printf("\n  %sScanning photos & albums...%s\n", ui.ColorDim, ui.ColorReset)
	}
	sets, err := client.GetPhotosets(ctx, nsid)
	if err != nil {
		return fmt.Errorf("discover photosets: %w", err)
	}

	firstPage, err := client.GetPhotosByUser(ctx, nsid, 1)
	if err != nil {
		return fmt.Errorf("get photos: %w", err)
	}
	totalPhotos := int(firstPage.Photos.Total)

	// Decide which albums to download.
	var selected []api.PhotoSetInfo
	includeOrphans := opts.Uncategorized
	showSelections := false

	switch {
	case opts.Albums != "":
		selected, err = matchSets(sets, opts.Albums)
		if err != nil {
			return err
		}
		showSelections = true
	case !opts.Quiet && !opts.Yes && !opts.DryRun && term.IsTerminal(int(os.Stdin.Fd())):
		items := buildPickerItems(sets, firstPage, totalPhotos)
		picked, err := ui.PickMulti("Select albums to download", items)
		if err != nil {
			if err == ui.ErrPickerCancelled {
				fmt.Printf("  %sCancelled — nothing downloaded.%s\n", ui.ColorYellow, ui.ColorReset)
				return nil
			}
			return fmt.Errorf("album picker: %w", err)
		}
		selected = []api.PhotoSetInfo{}
		for _, it := range picked {
			if it.ID == uncategorizedID {
				includeOrphans = it.Selected
				continue
			}
			if it.Selected {
				selected = append(selected, setByID(sets, it.ID))
			}
		}
		showSelections = true
	default:
		selected = sets
	}

	// Show the plan: album tree with selections and estimated sizes.
	if !opts.Quiet {
		printPlan(client, sets, selected, includeOrphans, firstPage, totalPhotos, showSelections, cfg.APIRateMS)
	}

	if opts.DryRun {
		if !opts.Quiet {
			fmt.Printf("\n  %s%s Dry run — nothing downloaded.%s\n", ui.ColorYellow, ui.IconSpark, ui.ColorReset)
		}
		return nil
	}

	if !opts.Yes && !opts.Quiet && term.IsTerminal(int(os.Stdin.Fd())) {
		if !confirm("Start download?") {
			fmt.Printf("  %sCancelled — nothing downloaded.%s\n", ui.ColorYellow, ui.ColorReset)
			return nil
		}
	}

	if !opts.Quiet {
		fmt.Printf("\n  %sDownloading albums...%s\n", ui.ColorDim, ui.ColorReset)
	}
	_, err = dl.DownloadByUser(ctx, nsid, download.UserDownloadOptions{
		Sets:           selected,
		FirstPage:      firstPage,
		IncludeOrphans: includeOrphans,
	})
	return err
}

func setByID(sets []api.PhotoSetInfo, id string) api.PhotoSetInfo {
	for _, s := range sets {
		if s.ID == id {
			return s
		}
	}
	return api.PhotoSetInfo{}
}

func buildPickerItems(sets []api.PhotoSetInfo, firstPage *api.PhotosResponse, totalPhotos int) []ui.PickerItem {
	avg := download.AvgPhotoBytes(firstPage.Photos.Photo)
	items := make([]ui.PickerItem, 0, len(sets)+1)
	for _, s := range sets {
		detail := fmt.Sprintf("%d photos", int(s.Photos))
		if avg > 0 {
			detail += fmt.Sprintf(" · ~%s", ui.FormatBytes(int64(s.Photos)*avg))
		}
		items = append(items, ui.PickerItem{
			ID:       s.ID,
			Label:    s.Title.Content,
			Detail:   detail,
			Selected: true,
		})
	}

	inSets := int64(0)
	for _, s := range sets {
		inSets += int64(s.Photos)
	}
	orphanCount := totalPhotos - int(inSets)
	if orphanCount < 0 {
		orphanCount = 0
	}
	items = append(items, ui.PickerItem{
		ID:       uncategorizedID,
		Label:    "Uncategorized photos",
		Detail:   fmt.Sprintf("%d photos", orphanCount),
		Selected: true,
	})
	return items
}

func printPlan(client *api.Client, sets, selected []api.PhotoSetInfo, includeOrphans bool, firstPage *api.PhotosResponse, totalPhotos int, showSelections bool, paceMS int) {
	avg := download.AvgPhotoBytes(firstPage.Photos.Photo)
	selectedByID := map[string]bool{}
	for _, s := range selected {
		selectedByID[s.ID] = true
	}

	inSets := int64(0)
	var totalEst int64

	if len(sets) > 0 {
		fmt.Printf("\n  %sAlbums:%s\n", ui.ColorBold, ui.ColorReset)
	}
	for _, s := range sets {
		mark := " "
		col := ""
		if showSelections {
			if selectedByID[s.ID] {
				mark = ui.IconOk
				col = ui.ColorGreen
			} else {
				mark = ui.IconSkip
				col = ui.ColorDim
			}
		}
		est := int64(0)
		if avg > 0 {
			est = int64(s.Photos) * avg
		}
		totalEst += est
		inSets += int64(s.Photos)
		line := fmt.Sprintf("    %s%s%s %s", col, mark, ui.ColorReset, s.Title.Content)
		detail := fmt.Sprintf("%d photos", int(s.Photos))
		if est > 0 {
			detail += fmt.Sprintf(" · ~%s", ui.FormatBytes(est))
		}
		fmt.Printf("%s  %s%s\n", line, ui.ColorDim, detail+ui.ColorReset)
	}

	orphanCount := totalPhotos - int(inSets)
	if orphanCount < 0 {
		orphanCount = 0
	}
	if showSelections {
		mark := ui.IconSkip
		col := ui.ColorDim
		if includeOrphans {
			mark = ui.IconOk
			col = ui.ColorGreen
		}
		line := fmt.Sprintf("    %s%s%s Uncategorized photos", col, mark, ui.ColorReset)
		fmt.Printf("%s  %s%d photos%s\n", line, ui.ColorDim, orphanCount, ui.ColorReset)
	}

	estTotal, estKnown := download.EstimatePhotosBytes(firstPage.Photos.Photo)
	total := fmt.Sprintf("%d photos", totalPhotos)
	if estKnown > 0 {
		extrapolated := estTotal
		if int(estKnown) < totalPhotos {
			extrapolated = estTotal * int64(totalPhotos) / estKnown
		}
		total += fmt.Sprintf(" · ~%s estimated", ui.FormatBytes(extrapolated))
	}
	fmt.Printf("\n  %sTotal:%s %s\n", ui.ColorBold, ui.ColorReset, total)
	printAPIEstimate(client, selected, includeOrphans, totalPhotos, firstPage, paceMS)
}

// estimateAPICalls predicts the REST requests still needed to list and
// download the selected albums and orphans, using only data already fetched
// during discovery. Listing pages are exact (counts come from the photoset
// list and the photostream total); getSizes fallbacks are estimated from the
// sampled fraction of photos lacking url_o (videos or originals disabled),
// which is what forces a sizes call.
func estimateAPICalls(selected []api.PhotoSetInfo, includeOrphans bool, totalPhotos int, firstPage *api.PhotosResponse) (listing, sizes int) {
	const perPage = 500
	var toDownload int
	for _, s := range selected {
		n := int(s.Photos)
		toDownload += n
		listing += (n + perPage - 1) / perPage
	}
	if includeOrphans {
		if orphans := totalPhotos - toDownload; orphans > 0 {
			toDownload += orphans
		}
		// The orphan scan re-paginates the photostream; page 1 is cached.
		if pages := (totalPhotos + perPage - 1) / perPage; pages > 1 {
			listing += pages - 1
		}
	}

	frac, known := 0.0, 0
	for _, p := range firstPage.Photos.Photo {
		known++
		if p.Media == "video" || p.URLOriginal == "" {
			frac++
		}
	}
	if known > 0 {
		sizes = int(float64(toDownload) * frac / float64(known))
	}
	return listing, sizes
}

// printAPIEstimate shows the predicted API cost of the plan against the
// remaining hourly budget, plus a wall-clock estimate at paceMS (the
// configured interval limiter — passed in rather than hardcoded, so this
// estimate can't silently drift from the actual pace when APIRateMS is
// non-default), including pauses at the cap when the plan exceeds the budget.
func printAPIEstimate(client *api.Client, selected []api.PhotoSetInfo, includeOrphans bool, totalPhotos int, firstPage *api.PhotosResponse, paceMS int) {
	used, limit, _ := client.Quota()
	if limit <= 0 {
		return
	}
	listing, sizes := estimateAPICalls(selected, includeOrphans, totalPhotos, firstPage)
	total := listing + sizes
	remaining := limit - used

	var status string
	col := ui.ColorGreen
	if total > remaining {
		col = ui.ColorYellow
		status = fmt.Sprintf("✗ exceeds remaining — will pause at the %d cap", limit)
	} else {
		status = "✓ fits within the hourly quota"
	}

	pace := float64(paceMS) // ms per request — matches the interval limiter
	wall := time.Duration(float64(total) * pace * float64(time.Millisecond))
	if total > remaining {
		pauses := (total - remaining + limit - 1) / limit
		wall += time.Duration(pauses) * time.Hour
	}

	fmt.Printf("\n  %sEstimated API calls:%s ~%d (%d listing + ~%d sizes)\n",
		ui.ColorBold, ui.ColorReset, total, listing, sizes)
	fmt.Printf("  %sRemaining this hour:%s %d · %s%s%s\n",
		ui.ColorBold, ui.ColorReset, remaining, col, status, ui.ColorReset)
	if total > 0 {
		fmt.Printf("  %sEst. wall time:%s %s\n",
			ui.ColorBold, ui.ColorReset, ui.FormatDuration(wall))
	}
}

func runDownload(cmd *cobra.Command, args []string) error {
	if urlFlag == "" {
		return fmt.Errorf("provide -u <flickr-url>")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if workers > 0 {
		cfg.WorkerCount = workers
	}
	if outDir != "" {
		cfg.OutputDir = outDir
	}

	tr, err := newQuotaTracker(cfg)
	if err != nil {
		return err
	}
	defer flushQuotaOnExit(tr)()

	client, store, err := newClientWithCache(cfg, tr, refreshFlag, offlineFlag)
	if err != nil {
		return err
	}
	if store != nil {
		defer store.Close()
	}

	unlock, err := lockOutputRoot(cfg.OutputDir)
	if err != nil {
		return err
	}
	defer unlock()

	ctx, stop := setupSignalContext()
	defer stop()

	fmt.Printf("\n%s\n\n", ui.Bordered("Flickr Downloader", ui.ColorCyan))
	fmt.Printf("  %sResolving URL...%s\n", ui.ColorDim, ui.ColorReset)

	dl := download.New(client, cfg.OutputDir, cfg.WorkerCount)
	dl.Cache = store
	if err := dl.WarmLocalIndex(); err != nil {
		fmt.Fprintf(os.Stderr, "  %s%s index local files: %v%s\n", ui.ColorYellow, ui.IconErr, err, ui.ColorReset)
	}

	return runDownloadOnce(ctx, urlFlag, cfg, client, dl, downloadOnceOptions{
		Albums:        albumsFlag,
		Uncategorized: uncategorized,
		DryRun:        dryRun,
		Yes:           yesFlag,
		Quiet:         false,
	})
}

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
	if urlFlag == "" {
		return fmt.Errorf("provide -u <flickr-url>")
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

func runWatch(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	path := watchFile
	if path == "" {
		for _, candidate := range watch.DefaultPaths() {
			if _, err := os.Stat(candidate); err == nil {
				path = candidate
				break
			}
		}
		if path == "" {
			return fmt.Errorf("no watchlist found — create ~/.config/flickrdownloader/watchlist.yaml (or sources.txt), or pass --file")
		}
	}

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
	logf := func(format string, args ...any) {
		line := fmt.Sprintf("%s INFO %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
		for _, w := range writers {
			fmt.Fprint(w, line)
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

	tr, err := newQuotaTracker(cfg)
	if err != nil {
		return err
	}
	defer flushQuotaOnExit(tr)()

	client, store, err := newClientWithCache(cfg, tr, false, false)
	if err != nil {
		return err
	}
	if store != nil {
		defer store.Close()
	}

	ctx, stop := setupSignalContext()
	defer stop()

	sched := &watch.Scheduler{
		Path:         path,
		PollInterval: watchPollInterval,
		Logf:         logf,
		RunSource: func(ctx context.Context, src watch.Source) error {
			srcOut := outDirDefault
			if src.OutputDir != "" {
				srcOut = src.OutputDir
			}
			srcWorkers := workerDefault
			if src.Workers > 0 {
				srcWorkers = src.Workers
			}

			dl := download.New(client, srcOut, srcWorkers)
			dl.Cache = store
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

	logf("watch starting watchlist=%s", path)
	if err := sched.Loop(ctx); err != nil {
		return fmt.Errorf("watch: %w", err)
	}
	logf("watch stopped")
	return nil
}

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
