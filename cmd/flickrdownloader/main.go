package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/jooservices/flickrdownloader/pkg/api"
	"github.com/jooservices/flickrdownloader/pkg/config"
	"github.com/jooservices/flickrdownloader/pkg/download"
	"github.com/jooservices/flickrdownloader/pkg/ui"
	"github.com/jooservices/flickrdownloader/pkg/update"
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
		RunE:  runDownload,
	}
	downloadCmd.Flags().StringVarP(&urlFlag, "url", "u", "", "Flickr URL (user / album / photo)")
	downloadCmd.Flags().StringVarP(&outDir, "out", "o", "", "Output directory (default: ./photos)")
	downloadCmd.Flags().IntVarP(&workers, "workers", "w", 0, "Number of concurrent download workers (default: 20)")
	downloadCmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview what would be downloaded (album tree, counts, estimated size) without downloading")
	downloadCmd.Flags().BoolVarP(&yesFlag, "yes", "y", false, "Skip the confirmation prompt and the album picker (download everything)")
	downloadCmd.Flags().StringVar(&albumsFlag, "albums", "", "Only download matching albums (comma-separated names, 'all' or 'none')")
	downloadCmd.Flags().BoolVar(&uncategorized, "uncategorized", true, "Include photos that are not in any album")

	updateCmd := &cobra.Command{
		Use:   "update",
		Short: "Check for updates and install the latest release",
		RunE:  runUpdate,
	}
	updateCmd.Flags().BoolVar(&updateCheckOnly, "check", false, "Only check for a newer version, do not install")

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
	rootCmd.AddCommand(updateCmd)
	rootCmd.AddCommand(completionCmd)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
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
	if err := update.Install(ctx, asset); err != nil {
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
		APIKey:      apiKey,
		APISecret:   apiSecret,
		OAuthToken:  accessToken,
		OAuthSecret: accessSecret,
		NSID:        userNSID,
		WorkerCount: 20,
		OutputDir:   "./photos",
	}

	if err := cfg.Save(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	fmt.Printf("\n  %s%s Authenticated as %s (NSID: %s)%s\n", ui.ColorGreen, ui.IconOk, username, userNSID, ui.ColorReset)
	fmt.Printf("  %sConfig saved. You can now use '%s'%s\n", ui.ColorDim, "flickrdownloader download -u <url>", ui.ColorReset)
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
			return nil, fmt.Errorf("no album matches '%s' — available albums:\n    %s",
				name, strings.Join(names, "\n    "))
		}
	}
	return out, nil
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

	client := api.NewClient(cfg.APIKey, cfg.APISecret, cfg.OAuthToken, cfg.OAuthSecret)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Printf("\n  %s%s Cancelling...%s\n", ui.ColorYellow, ui.IconErr, ui.ColorReset)
		cancel()
	}()

	fmt.Printf("  %sResolving URL...%s\n", ui.ColorDim, ui.ColorReset)
	parsed, err := api.ResolveURL(ctx, urlFlag, client)
	if err != nil {
		return fmt.Errorf("resolve URL '%s': %w", urlFlag, err)
	}

	fmt.Printf("\n%s\n\n", ui.Bordered("Flickr Downloader", ui.ColorCyan))
	fmt.Printf("  %sURL:%s    %s\n", ui.ColorDim, ui.ColorReset, urlFlag)

	dl := download.New(client, cfg.OutputDir, cfg.WorkerCount)

	switch parsed.Type {
	case api.TargetPhoto:
		info, err := client.GetPhotoInfo(ctx, parsed.ID)
		if err != nil {
			return fmt.Errorf("get photo info: %w", err)
		}
		fmt.Printf("  %sType:%s   Photo\n", ui.ColorDim, ui.ColorReset)
		fmt.Printf("  %sTitle:%s  %s\n", ui.ColorDim, ui.ColorReset, info.Photo.Title.Content)
		fmt.Printf("  %sOwner:%s  %s (%s)\n", ui.ColorDim, ui.ColorReset, info.Photo.Owner.Username, info.Photo.Owner.NSID)
		fmt.Printf("  %sMedia:%s  %s\n", ui.ColorDim, ui.ColorReset, info.Photo.Media)
		fmt.Printf("  %sOutput:%s %s/%s/\n\n", ui.ColorDim, ui.ColorReset, cfg.OutputDir, info.Photo.Owner.NSID)

		if dryRun {
			fmt.Printf("  %s%s Dry run — nothing downloaded.%s\n", ui.ColorYellow, ui.IconSpark, ui.ColorReset)
			return nil
		}

		return dl.DownloadPhoto(ctx, parsed.ID)

	case api.TargetPhotoset:
		psInfo, err := client.GetPhotosetInfo(ctx, parsed.ID)
		if err != nil {
			return fmt.Errorf("get photoset info: %w", err)
		}
		fmt.Printf("  %sType:%s   Album\n", ui.ColorDim, ui.ColorReset)
		fmt.Printf("  %sTitle:%s  %s\n", ui.ColorDim, ui.ColorReset, psInfo.Title.Content)
		fmt.Printf("  %sOwner:%s  %s\n", ui.ColorDim, ui.ColorReset, psInfo.Owner)
		fmt.Printf("  %sPhotos:%s %d\n", ui.ColorDim, ui.ColorReset, int(psInfo.Photos))
		fmt.Printf("  %sOutput:%s %s/%s/%s/\n\n", ui.ColorDim, ui.ColorReset, cfg.OutputDir, psInfo.Owner, download.SafeName(psInfo.Title.Content))

		if dryRun {
			fmt.Printf("  %s%s Dry run — nothing downloaded.%s\n", ui.ColorYellow, ui.IconSpark, ui.ColorReset)
			return nil
		}

		fmt.Printf("  %sDownloading...%s\n\n", ui.ColorDim, ui.ColorReset)
		_, err = dl.DownloadByPhotoset(ctx, parsed.ID)
		return err

	case api.TargetUser:
		return runUserDownload(ctx, dl, client, parsed.ID)
	}

	return nil
}

func runUserDownload(ctx context.Context, dl *download.Downloader, client *api.Client, nsid string) error {
	fmt.Printf("  %sType:%s   User\n", ui.ColorDim, ui.ColorReset)
	fmt.Printf("  %sNSID:%s   %s\n", ui.ColorDim, ui.ColorReset, nsid)
	fmt.Printf("  %sOutput:%s %s/%s/\n", ui.ColorDim, ui.ColorReset, dl.OutDir, nsid)

	fmt.Printf("\n  %sScanning photos & albums...%s\n", ui.ColorDim, ui.ColorReset)
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
	includeOrphans := uncategorized
	showSelections := false

	switch {
	case albumsFlag != "":
		selected, err = matchSets(sets, albumsFlag)
		if err != nil {
			return err
		}
		showSelections = true
	case term.IsTerminal(int(os.Stdin.Fd())) && !dryRun && !yesFlag:
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
	printPlan(sets, selected, includeOrphans, firstPage, totalPhotos, showSelections)

	if dryRun {
		fmt.Printf("\n  %s%s Dry run — nothing downloaded.%s\n", ui.ColorYellow, ui.IconSpark, ui.ColorReset)
		return nil
	}

	if !yesFlag && term.IsTerminal(int(os.Stdin.Fd())) {
		if !confirm("Start download?") {
			fmt.Printf("  %sCancelled — nothing downloaded.%s\n", ui.ColorYellow, ui.ColorReset)
			return nil
		}
	}

	fmt.Printf("\n  %sDownloading...%s\n\n", ui.ColorDim, ui.ColorReset)
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

func printPlan(sets, selected []api.PhotoSetInfo, includeOrphans bool, firstPage *api.PhotosResponse, totalPhotos int, showSelections bool) {
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
}
