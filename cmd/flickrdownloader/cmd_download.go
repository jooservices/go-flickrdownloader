package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jooservices/flickrdownloader/pkg/api"
	"github.com/jooservices/flickrdownloader/pkg/config"
	"github.com/jooservices/flickrdownloader/pkg/download"
	"github.com/jooservices/flickrdownloader/pkg/ui"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func confirm(prompt string) bool {
	reader := bufio.NewReader(os.Stdin)
	fmt.Printf("\n  %s%s [Y/n]%s ", ui.ColorBold, prompt, ui.ColorReset)
	line, err := readLine(reader)
	if err != nil {
		return false
	}
	return line == "" || strings.EqualFold(line, "y") || strings.EqualFold(line, "yes")
}

// matchSets maps an --albums spec (comma-separated names, 'all', 'none',
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
			d[j] = min(d[j]+1, min(d[j-1]+1, prev+cost))
			prev = temp
		}
	}
	return d[lb]
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
	if report := dl.FormatFailures(); report != "" {
		fmt.Fprint(os.Stderr, report)
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
	if err := requireURL(); err != nil {
		return err
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
	dl.Refresh = refreshFlag || forceFlag
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
