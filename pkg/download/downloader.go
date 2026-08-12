package download

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
}

var safeNameRe = regexp.MustCompile(`[<>:"/\\|?*]`)

var downloadHTTPClient = &http.Client{Timeout: 60 * time.Second}

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
		Client:     client,
		OutDir:     outDir,
		NumWorkers: numWorkers,
		dlLimiter:  rate.NewLimiter(rate.Limit(numWorkers*2), numWorkers),
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

func (d *Downloader) downloadFile(ctx context.Context, url, filePath string) (int64, error) {
	if err := d.dlLimiter.Wait(ctx); err != nil {
		return 0, fmt.Errorf("rate limiter: %w", err)
	}

	tmpPath := filePath + ".tmp"

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return 0, fmt.Errorf("create request: %w", err)
		}

		resp, err := downloadHTTPClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("http get: %w", err)
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			lastErr = fmt.Errorf("http status %d", resp.StatusCode)
			time.Sleep(2 * time.Second)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return 0, fmt.Errorf("http status %d", resp.StatusCode)
		}

		out, err := os.Create(tmpPath)
		if err != nil {
			resp.Body.Close()
			return 0, fmt.Errorf("create file: %w", err)
		}

		written, err := io.Copy(out, resp.Body)
		resp.Body.Close()
		out.Close()

		if err != nil {
			os.Remove(tmpPath)
			return 0, fmt.Errorf("write file: %w", err)
		}

		if err := os.Rename(tmpPath, filePath); err != nil {
			os.Remove(tmpPath)
			return 0, fmt.Errorf("rename temp file: %w", err)
		}

		return written, nil
	}

	return 0, fmt.Errorf("retry exhausted: %w", lastErr)
}

// alreadyDownloaded reports whether a photo with this ID exists under any
// extension in OutDir (ignoring stale .tmp files left by an interrupted
// download), so resuming works regardless of the file's actual media type.
func (d *Downloader) alreadyDownloaded(photoID string) bool {
	matches, _ := filepath.Glob(filepath.Join(d.OutDir, photoID+".*"))
	for _, m := range matches {
		if !strings.HasSuffix(m, ".tmp") {
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

	downloadURL, ext, err := d.resolveDownloadURL(ctx, photo)
	if err != nil {
		d.progress.AddFailure(photo.ID, "", err.Error())
		return
	}

	filePath := filepath.Join(d.OutDir, photo.ID+"."+ext)

	n, err := d.downloadFile(ctx, downloadURL, filePath)
	if err != nil {
		d.progress.AddFailure(photo.ID, downloadURL, err.Error())
		return
	}

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

	if err := g.Wait(); err != nil && err != context.Canceled {
		return Stats{}
	}

	fmt.Print("\r\033[K")
	fmt.Print(d.progress.Summary())

	return Stats{
		Total:   int64(totalPhotos),
		Success: d.progress.Stats().Success,
		Skipped: d.progress.Stats().Skipped,
		Failed:  d.progress.Stats().Failed,
	}
}

func (d *Downloader) DownloadByUser(ctx context.Context, userID string) (Stats, error) {
	userDir := filepath.Join(d.OutDir, userID)

	fmt.Printf("  %sDiscovering photosets...%s\n", ui.ColorDim, ui.ColorReset)
	sets, err := d.Client.GetPhotosets(ctx, userID)
	if err != nil {
		return Stats{}, fmt.Errorf("list photosets: %w", err)
	}

	downloaded := make(map[string]bool)
	var totalSuccess, totalSkipped, totalFailed int64

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
		totalFailed += stats.Failed
	}

	// Download remaining photos not in any photoset
	firstPage, err := d.Client.GetPhotosByUser(ctx, userID, 1)
	if err != nil {
		return Stats{}, fmt.Errorf("get first page: %w", err)
	}

	orphans := 0
	var orphanPhotos []api.Photo

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
		totalFailed += stats.Failed
	}

	fmt.Printf("\n  %s%s Total: %d photosets + %d uncategorized%s\n",
		ui.ColorGreen, ui.IconSpark, len(sets), orphans, ui.ColorReset)

	return Stats{
		Total:   int64(int(firstPage.Photos.Total)),
		Success: totalSuccess,
		Skipped: totalSkipped,
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
