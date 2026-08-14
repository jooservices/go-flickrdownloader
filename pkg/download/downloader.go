package download

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jooservices/flickrdownloader/pkg/api"
	"github.com/jooservices/flickrdownloader/pkg/ui"
	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"
)

type Stats struct {
	Total   int64
	Success int64
	Skipped int64
	Linked  int64
	Failed  int64
}

type Downloader struct {
	Client     *api.Client
	OutDir     string
	NumWorkers int
	progress   *ui.Progress
	currPage   int64
	totalPages int64
	dlLimiter  *rate.Limiter
	httpClient *http.Client
	sleep      sleeper
	jitter     func() float64

	// downloadedPaths records, per photo ID, the path of the first copy
	// written to disk. Later albums reuse it via hardlinks instead of
	// re-downloading the same file (feature: cross-album dedupe).
	downloadedPaths map[string]string
	pathMu          sync.Mutex
}

var safeNameRe = regexp.MustCompile(`[<>:"/\\|?*]`)

func SafeName(name string) string {
	name = strings.TrimSpace(name)
	name = safeNameRe.ReplaceAllString(name, "_")
	if name == "" || name == "." || name == ".." {
		return "untitled"
	}
	if len(name) > 120 {
		name = name[:120]
	}
	return name
}

func New(client *api.Client, outDir string, numWorkers int) *Downloader {
	if numWorkers <= 0 {
		numWorkers = 20
	}
	// The download limiter guards against hammering Flickr's CDN with bursts,
	// but should scale with -workers rather than capping it at a fixed rate —
	// otherwise a larger worker pool just means more goroutines blocked on
	// the same handful of tokens per second.
	return &Downloader{
		Client:          client,
		OutDir:          outDir,
		NumWorkers:      numWorkers,
		dlLimiter:       rate.NewLimiter(rate.Limit(numWorkers*2), numWorkers),
		httpClient:      &http.Client{Timeout: 60 * time.Second},
		jitter:          func() float64 { return 0.2 * rand.Float64() }, // +0-20% jitter (AC-001)
		downloadedPaths: make(map[string]string),
	}
}

// resolveDownloadURL avoids an extra flickr.photos.getSizes API call (which is
// the dominant bottleneck under the ~1req/sec API rate limit) by using the
// url_o extra returned alongside the photo listing whenever it's available.
// It falls back to getSizes for videos and for photos where Flickr omitted
// url_o (e.g. the owner disabled original-size downloads).
func (d *Downloader) resolveDownloadURL(ctx context.Context, photo api.Photo) (string, string, error) {
	if photo.Media != "video" && photo.URLOriginal != "" {
		return photo.URLOriginal, cleanExt(fileExt(photo.URLOriginal)), nil
	}
	return d.fetchSizesAndEnrich(ctx, photo)
}

func (d *Downloader) fetchSizesAndEnrich(ctx context.Context, photo api.Photo) (string, string, error) {
	sizes, err := d.Client.GetSizes(ctx, photo.ID)
	if err != nil {
		return "", "", fmt.Errorf("get sizes for %s: %w", photo.ID, err)
	}

	var downloadURL string

	for _, s := range sizes.Sizes.Size {
		if s.Media == "video" {
			switch s.Label {
			case "Video Original":
				downloadURL = s.Source
			case "HD MP4":
				if downloadURL == "" {
					downloadURL = s.Source
				}
			case "Site MP4":
				if downloadURL == "" {
					downloadURL = s.Source
				}
			case "Mobile MP4":
				if downloadURL == "" {
					downloadURL = s.Source
				}
			}
		} else {
			switch s.Label {
			case "Original":
				downloadURL = s.Source
			}
		}
	}

	if downloadURL == "" && len(sizes.Sizes.Size) > 0 {
		for i := len(sizes.Sizes.Size) - 1; i >= 0; i-- {
			s := sizes.Sizes.Size[i]
			if s.Source != "" && s.Media == "photo" {
				downloadURL = s.Source
				break
			}
		}
	}

	if downloadURL == "" {
		return "", "", fmt.Errorf("no sizes available for photo %s", photo.ID)
	}

	ext := fileExt(downloadURL)
	ext = cleanExt(ext)

	return downloadURL, ext, nil
}

func fileExt(source string) string {
	idx := strings.LastIndex(source, ".")
	if idx < 0 {
		return "jpg"
	}
	return strings.ToLower(source[idx+1:])
}

func cleanExt(ext string) string {
	if idx := strings.Index(ext, "?"); idx != -1 {
		ext = ext[:idx]
	}
	switch ext {
	case "jpeg":
		return "jpg"
	case "":
		return "jpg"
	default:
		return ext
	}
}

// httpHint turns an HTTP status into actionable advice.
func httpHint(status int) string {
	switch status {
	case http.StatusForbidden:
		return "Flickr denied access — the photo may be private, or the owner disabled original downloads"
	case http.StatusNotFound:
		return "the photo no longer exists on Flickr"
	case http.StatusUnauthorized:
		return "authentication required — check that your account can view this photo"
	case http.StatusTooManyRequests:
		return "Flickr is rate-limiting downloads — retry later or lower -w"
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable:
		return "Flickr's servers had a problem — retrying usually helps"
	}
	return ""
}

func (d *Downloader) downloadFile(ctx context.Context, url, filePath string) (int64, error) {
	written, err := (fetcher{
		url:       url,
		finalPath: filePath,
		client:    d.httpClient,
		sleep:     d.sleep,
		jitter:    d.jitter,
		beforeAttempt: func(ctx context.Context) error {
			return d.dlLimiter.Wait(ctx)
		},
		maxTries: 3,
	}).run(ctx)
	if err != nil {
		return 0, err
	}
	return written, nil
}

// alreadyDownloaded reports whether a photo with this ID exists under any
// extension in OutDir (ignoring incomplete download artifacts), so resuming
// works regardless of the file's actual media type.
func (d *Downloader) alreadyDownloaded(photoID string) bool {
	matches, _ := filepath.Glob(filepath.Join(d.OutDir, photoID+".*"))
	for _, m := range matches {
		if !strings.HasSuffix(m, ".part") &&
			!strings.HasSuffix(m, ".cand") &&
			!strings.HasSuffix(m, ".tmp") {
			return true
		}
	}
	return false
}

func (d *Downloader) worker(ctx context.Context, photo api.Photo) {
	if d.alreadyDownloaded(photo.ID) {
		d.progress.AddSkipped()
		return
	}

	// Cross-album dedupe: if this photo was already downloaded for another
	// album, reuse it via a hardlink instead of downloading it again. On
	// filesystems without hardlink support (e.g. some network mounts) we
	// fall back to a regular download.
	d.pathMu.Lock()
	existing, ok := d.downloadedPaths[photo.ID]
	d.pathMu.Unlock()
	if ok {
		target := filepath.Join(d.OutDir, photo.ID+filepath.Ext(existing))
		if err := os.Link(existing, target); err == nil {
			d.progress.AddLinked()
			return
		}
	}

	downloadURL, ext, err := d.resolveDownloadURL(ctx, photo)
	if err != nil {
		d.progress.AddFailure(photo.ID, "", err.Error())
		return
	}

	filePath := filepath.Join(d.OutDir, photo.ID+"."+ext)

	n, err := d.downloadFile(ctx, downloadURL, filePath)
	if err != nil {
		if !errors.Is(err, errCancelled) {
			d.progress.AddFailure(photo.ID, downloadURL, err.Error())
		}
		return
	}

	d.pathMu.Lock()
	d.downloadedPaths[photo.ID] = filePath
	d.pathMu.Unlock()

	d.progress.AddSuccess()
	d.progress.AddBytes(n)
}

func (d *Downloader) renderProgressLine() {
	page := atomic.LoadInt64(&d.currPage)
	total := atomic.LoadInt64(&d.totalPages)
	fmt.Printf("\r\033[K%s", d.progress.Render())
	fmt.Printf("  %s▸ page %d/%d%s", ui.ColorDim, page, total, ui.ColorReset)
}

func (d *Downloader) startRenderer(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(150 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				d.renderProgressLine()
			}
		}
	}()
}

func (d *Downloader) downloadPhotosFromPages(
	ctx context.Context,
	totalPhotos int,
	totalPages int,
	fetchPage func(ctx context.Context, page int) ([]api.Photo, error),
) Stats {
	d.progress = ui.NewProgress(totalPhotos)
	atomic.StoreInt64(&d.currPage, 0)
	atomic.StoreInt64(&d.totalPages, int64(totalPages))

	jobs := make(chan api.Photo, d.NumWorkers*2)
	g, ctx := errgroup.WithContext(ctx)

	d.startRenderer(ctx)

	for i := 0; i < d.NumWorkers; i++ {
		g.Go(func() error {
			for photo := range jobs {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
				d.worker(ctx, photo)
			}
			return nil
		})
	}

	g.Go(func() error {
		defer close(jobs)
		for page := 1; page <= totalPages; page++ {
			if page > 1 {
				time.Sleep(100 * time.Millisecond)
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			photos, err := fetchPage(ctx, page)
			if err != nil {
				fmt.Fprintf(os.Stderr, "\r\033[K  %s%s API page %d: %v%s\n",
					ui.ColorRed, ui.IconErr, page, err, ui.ColorReset)
				continue
			}

			for _, photo := range photos {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case jobs <- photo:
				}
			}
			atomic.StoreInt64(&d.currPage, int64(page))
		}
		return nil
	})

	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return Stats{}
	}

	fmt.Print("\r\033[K")
	fmt.Print(d.progress.Summary())

	return Stats{
		Total:   int64(totalPhotos),
		Success: d.progress.Stats().Success,
		Skipped: d.progress.Stats().Skipped,
		Linked:  d.progress.Stats().Linked,
		Failed:  d.progress.Stats().Failed,
	}
}

// UserDownloadOptions controls DownloadByUser. A nil Sets slice fetches all
// albums from the API (legacy behavior); an empty non-nil slice downloads no
// albums. FirstPage may be supplied to avoid a duplicate API call (e.g. when
// the caller already scanned the photostream for a dry-run plan).
type UserDownloadOptions struct {
	Sets           []api.PhotoSetInfo
	FirstPage      *api.PhotosResponse
	IncludeOrphans bool
}

func (d *Downloader) DownloadByUser(ctx context.Context, userID string, opts UserDownloadOptions) (Stats, error) {
	userDir := filepath.Join(d.OutDir, userID)

	sets := opts.Sets
	if sets == nil {
		fmt.Printf("  %sDiscovering photosets...%s\n", ui.ColorDim, ui.ColorReset)
		fetched, err := d.Client.GetPhotosets(ctx, userID)
		if err != nil {
			return Stats{}, fmt.Errorf("list photosets: %w", err)
		}
		sets = fetched
	}

	downloaded := make(map[string]bool)
	var totalSuccess, totalSkipped, totalLinked, totalFailed int64

	if len(sets) > 0 {
		fmt.Printf("  %sFound %d photoset(s)%s\n", ui.ColorCyan, len(sets), ui.ColorReset)
	}

	for _, set := range sets {
		setName := SafeName(set.Title.Content)
		setDir := filepath.Join(userDir, setName)

		if err := os.MkdirAll(setDir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "  %s%s create dir %s: %v%s\n",
				ui.ColorRed, ui.IconErr, setName, err, ui.ColorReset)
			continue
		}

		d.OutDir = setDir

		firstPage, err := d.Client.GetPhotosByPhotoset(ctx, set.ID, 1)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s%s photoset '%s': %v%s\n",
				ui.ColorRed, ui.IconErr, setName, err, ui.ColorReset)
			continue
		}

		total := int(firstPage.Photoset.Total)
		pages := int(firstPage.Photoset.Pages)
		fmt.Printf("\n  %s%s %s %s(%d photos)%s\n",
			ui.ColorCyan, ui.IconPhoto, setName, ui.ColorDim, total, ui.ColorReset)

		stats := d.downloadPhotosFromPages(ctx, total, pages,
			func(ctx context.Context, page int) ([]api.Photo, error) {
				var photos []api.Photo
				if page == 1 {
					photos = firstPage.Photoset.Photo
				} else {
					resp, err := d.Client.GetPhotosByPhotoset(ctx, set.ID, page)
					if err != nil {
						return nil, err
					}
					photos = resp.Photoset.Photo
				}
				for _, p := range photos {
					downloaded[p.ID] = true
				}
				return photos, nil
			})

		totalSuccess += stats.Success
		totalSkipped += stats.Skipped
		totalLinked += stats.Linked
		totalFailed += stats.Failed
	}

	// Download remaining photos not in any photoset
	firstPage := opts.FirstPage
	if firstPage == nil {
		fp, err := d.Client.GetPhotosByUser(ctx, userID, 1)
		if err != nil {
			return Stats{}, fmt.Errorf("get first page: %w", err)
		}
		firstPage = fp
	}

	includeOrphans := opts.IncludeOrphans || opts.Sets == nil

	orphans := 0
	var orphanPhotos []api.Photo

	if includeOrphans {
		if firstPage.Photos.Total == 0 && len(sets) == 0 {
			fmt.Printf("\n  %s%s No photos or photosets found for this user. Check that the NSID is correct.%s\n",
				ui.ColorYellow, ui.IconErr, ui.ColorReset)
			fmt.Printf("  %sNSID used: %s%s\n\n", ui.ColorDim, userID, ui.ColorReset)
			fmt.Printf("  %sThis may mean:%s\n", ui.ColorBold, ui.ColorReset)
			fmt.Printf("    %s- The user's content is private (OAuth is for a different user)%s\n", ui.ColorDim, ui.ColorReset)
			fmt.Printf("    %s- The NSID resolved incorrectly%s\n", ui.ColorDim, ui.ColorReset)
		}
		for page := 1; page <= int(firstPage.Photos.Pages); page++ {
			if page > 1 {
				resp, err := d.Client.GetPhotosByUser(ctx, userID, page)
				if err != nil {
					continue
				}
				for _, p := range resp.Photos.Photo {
					if !downloaded[p.ID] {
						orphanPhotos = append(orphanPhotos, p)
					}
				}
			} else {
				for _, p := range firstPage.Photos.Photo {
					if !downloaded[p.ID] {
						orphanPhotos = append(orphanPhotos, p)
					}
				}
			}
		}
	}

	if len(orphanPhotos) > 0 {
		orphans = len(orphanPhotos)
		orphanDir := userDir
		if err := os.MkdirAll(orphanDir, 0755); err != nil {
			return Stats{}, fmt.Errorf("create dir: %w", err)
		}
		d.OutDir = orphanDir

		fmt.Printf("\n  %s%s Uncategorized (%d photos)%s\n",
			ui.ColorCyan, ui.IconPhoto, orphans, ui.ColorReset)

		stats := d.downloadPhotosFromPages(ctx, orphans, 1,
			func(ctx context.Context, page int) ([]api.Photo, error) {
				return orphanPhotos, nil
			})

		totalSuccess += stats.Success
		totalSkipped += stats.Skipped
		totalLinked += stats.Linked
		totalFailed += stats.Failed
	}

	fmt.Printf("\n  %s%s Total: %d photosets + %d uncategorized%s\n",
		ui.ColorGreen, ui.IconSpark, len(sets), orphans, ui.ColorReset)

	return Stats{
		Total:   int64(int(firstPage.Photos.Total)),
		Success: totalSuccess,
		Skipped: totalSkipped,
		Linked:  totalLinked,
		Failed:  totalFailed,
	}, nil
}

func (d *Downloader) DownloadByPhotoset(ctx context.Context, photosetID string) (Stats, error) {
	firstPage, err := d.Client.GetPhotosByPhotoset(ctx, photosetID, 1)
	if err != nil {
		return Stats{}, fmt.Errorf("get first page: %w", err)
	}

	// Owner NSID always comes from the photoset listing itself, so the output
	// directory layout stays consistent even if the title lookup below fails.
	ownerNSID := firstPage.Photoset.Owner
	setName := photosetID
	if info, err := d.Client.GetPhotosetInfo(ctx, photosetID); err == nil {
		setName = SafeName(info.Title.Content)
	}

	setDir := filepath.Join(d.OutDir, ownerNSID, setName)
	if err := os.MkdirAll(setDir, 0755); err != nil {
		return Stats{}, fmt.Errorf("create output dir: %w", err)
	}

	d.OutDir = setDir

	total := int(firstPage.Photoset.Total)
	pages := int(firstPage.Photoset.Pages)

	stats := d.downloadPhotosFromPages(ctx, total, pages,
		func(ctx context.Context, page int) ([]api.Photo, error) {
			if page == 1 {
				return firstPage.Photoset.Photo, nil
			}
			resp, err := d.Client.GetPhotosByPhotoset(ctx, photosetID, page)
			if err != nil {
				return nil, err
			}
			return resp.Photoset.Photo, nil
		})

	return stats, nil
}

func (d *Downloader) DownloadPhoto(ctx context.Context, photoID string) error {
	info, err := d.Client.GetPhotoInfo(ctx, photoID)
	if err != nil {
		return fmt.Errorf("get photo info: %w", err)
	}

	owner := info.Photo.Owner.NSID
	dir := filepath.Join(d.OutDir, owner)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	d.OutDir = dir
	d.progress = ui.NewProgress(1)

	photo := api.Photo{
		ID:     info.Photo.ID,
		Secret: info.Photo.Secret,
		Server: info.Photo.Server,
		Farm:   api.FlexInt(info.Photo.Farm),
		Title:  info.Photo.Title.Content,
		Owner:  owner,
	}

	d.worker(ctx, photo)
	fmt.Print("\r\033[K")
	fmt.Print(d.progress.Summary())
	return nil
}
