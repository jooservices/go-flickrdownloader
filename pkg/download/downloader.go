package download

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jooservices/flickrdownloader/pkg/api"
	"github.com/jooservices/flickrdownloader/pkg/cache"
	"github.com/jooservices/flickrdownloader/pkg/ui"
	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"
)

// uncategorizedStatusID is the manifest key used for the "photos not in any
// album" bucket, distinct from any real photoset ID.
const uncategorizedStatusID = "__uncategorized__"

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

	// rootDir is the output root as given to New, independent of OutDir
	// (which is reassigned per-album during a run). It keys cache manifests
	// (pkg/cache.PhotosetStatus.RootDir) and the output-root lock so they
	// stay stable across the whole run.
	rootDir string

	// Cache is the optional response/manifest store (ADR-019/021). A nil
	// Cache disables photoset completion manifests and verify's offline
	// checks; downloads still work as before 1.3.0.
	Cache *cache.Store

	// Refresh bypasses completion-manifest reuse for this run so every
	// album is re-listed from Flickr (CLI --refresh / --force).
	Refresh bool

	// Quiet disables the interactive progress ticker in favor of periodic
	// structured summary lines, for non-TTY/service use (e.g. watch mode).
	Quiet bool

	// Logf, when set, receives quiet-mode progress lines (watch --log-file).
	// The function is expected to add its own timestamp/prefix.
	Logf func(format string, args ...any)

	// downloadedPaths records, per photo ID, the path of the first copy
	// written to disk. Later albums reuse it via hardlinks instead of
	// re-downloading the same file (feature: cross-album dedupe).
	downloadedPaths map[string]string
	pathMu          sync.Mutex

	// localFiles indexes every photo ID already present anywhere under
	// rootDir (populated by indexLocalFiles), enabling recursive local reuse
	// across albums and users and the presence checks verify relies on
	// (ADR-023). The first path seen for an ID is kept as the hardlink source.
	localFiles   map[string]string
	localByDir   map[string]map[string]string // abs directory → photo ID → path
	localFilesMu sync.Mutex

	// Live status state for the progress renderer: the album currently being
	// processed and the file currently being written by a worker.
	currAlbum  atomic.Value // string
	activeFile string
	activeMu   sync.Mutex

	// Per-album results for the final breakdown table.
	albumStats []ui.AlbumStat
	statsMu    sync.Mutex

	failMu sync.Mutex
}

var (
	safeNameRe    = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)
	winReservedRe = regexp.MustCompile(`(?i)^(CON|PRN|AUX|NUL|COM[1-9]|LPT[1-9])(?:\..*)?$`)
	allowedExt    = map[string]string{
		"jpg": "jpg", "jpeg": "jpg", "png": "png", "gif": "gif", "webp": "webp",
		"tif": "tif", "tiff": "tiff", "bmp": "bmp",
		"mp4": "mp4", "mov": "mov", "m4v": "m4v", "avi": "avi",
		"mkv": "mkv", "webm": "webm", "3gp": "3gp", "ogv": "ogv",
	}
)

const safeNameMaxRunes = 120

// SafeName turns a Flickr title into a directory name that is valid on
// Windows and Unix: strips reserved characters, trailing dots/spaces,
// Windows device names (CON, NUL, …), and truncates on a rune boundary.
func SafeName(name string) string {
	name = strings.TrimSpace(name)
	name = safeNameRe.ReplaceAllString(name, "_")
	name = strings.TrimRight(name, " .")
	if name == "" || name == "." || name == ".." {
		return "untitled"
	}
	if winReservedRe.MatchString(name) {
		name += "_"
	}
	if utf8.RuneCountInString(name) > safeNameMaxRunes {
		name = string([]rune(name)[:safeNameMaxRunes])
		name = strings.TrimRight(name, " .")
		if name == "" {
			return "untitled"
		}
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
		rootDir:         absolutePath(outDir),
		NumWorkers:      numWorkers,
		dlLimiter:       rate.NewLimiter(rate.Limit(numWorkers*2), numWorkers),
		httpClient:      newDownloadHTTPClient(),
		jitter:          func() float64 { return 0.2 * rand.Float64() }, // +0-20% jitter (AC-001)
		downloadedPaths: make(map[string]string),
	}
}

// absolutePath returns an absolute form of path for stable identity
// comparisons (manifest directories, stale-directory detection); it falls
// back to path unchanged if resolution fails.
func absolutePath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

// CanonicalPath resolves path to an absolute, symlink-resolved form, used to
// identify the output root consistently across relative paths, symlinks, and
// trailing slashes for the output-root lock (ADR-023).
func CanonicalPath(path string) string {
	abs := absolutePath(path)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// WarmLocalIndex recursively indexes every already-downloaded photo under
// the output root so later reuse/verification checks don't have to re-walk
// the filesystem per album (ADR-023). Best-effort: an indexing error only
// means reuse/verification degrade to their pre-1.3.0 behavior for this run,
// so it's returned rather than treated as fatal by callers that choose to
// ignore it.
func (d *Downloader) WarmLocalIndex() error {
	d.localFilesMu.Lock()
	d.localFiles = make(map[string]string)
	d.localByDir = make(map[string]map[string]string)
	d.localFilesMu.Unlock()
	return d.indexLocalFiles(d.rootDir)
}

// indexLocalFiles walks dir recursively and records every file named
// "<photoID>.<ext>" (skipping in-progress .part/.cand/.tmp artifacts), so
// verification and cross-album/cross-user reuse can check local presence
// without re-scanning per photoset.
func (d *Downloader) indexLocalFiles(dir string) error {
	d.localFilesMu.Lock()
	defer d.localFilesMu.Unlock()
	if d.localFiles == nil {
		d.localFiles = make(map[string]string)
	}
	if d.localByDir == nil {
		d.localByDir = make(map[string]map[string]string)
	}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".cand") || strings.HasSuffix(name, ".tmp") {
			return nil
		}
		id := strings.TrimSuffix(name, filepath.Ext(name))
		if !api.ValidPhotoID(id) {
			return nil
		}
		abs := absolutePath(path)
		parent := filepath.Dir(abs)
		if d.localByDir[parent] == nil {
			d.localByDir[parent] = make(map[string]string)
		}
		d.localByDir[parent][id] = abs
		if _, exists := d.localFiles[id]; !exists {
			d.localFiles[id] = abs
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (d *Downloader) noteLocalFile(id, path string) {
	abs := absolutePath(path)
	parent := filepath.Dir(abs)
	d.localFilesMu.Lock()
	defer d.localFilesMu.Unlock()
	if d.localFiles == nil {
		d.localFiles = make(map[string]string)
	}
	if d.localByDir == nil {
		d.localByDir = make(map[string]map[string]string)
	}
	if d.localByDir[parent] == nil {
		d.localByDir[parent] = make(map[string]string)
	}
	d.localByDir[parent][id] = abs
	if _, exists := d.localFiles[id]; !exists {
		d.localFiles[id] = abs
	}
}

// missingPhotosetIDs returns the subset of expected not present as an
// indexed local file inside dir (indexLocalFiles must have already run over
// a tree containing dir).
func (d *Downloader) missingPhotosetIDs(dir string, expected []string) []string {
	d.localFilesMu.Lock()
	defer d.localFilesMu.Unlock()
	want := absolutePath(dir)
	inDir := d.localByDir[want]
	var missing []string
	for _, id := range expected {
		if _, ok := inDir[id]; ok {
			continue
		}
		path, ok := d.localFiles[id]
		if !ok || filepath.Dir(path) != want {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	return missing
}

// missingPhotosetManifestIDs is missingPhotosetIDs against a saved manifest's
// own recorded directory, for the (!refresh) verification path.
func (d *Downloader) missingPhotosetManifestIDs(status *cache.PhotosetStatus) []string {
	return d.missingPhotosetIDs(status.Directory, status.ExpectedIDs)
}

// localPhotosetManifestComplete additionally checks file size against the
// manifest's recorded size (when one was recorded), catching a truncated or
// overwritten file that missingPhotosetManifestIDs' presence-only check
// would miss.
func (d *Downloader) localPhotosetManifestComplete(status *cache.PhotosetStatus) bool {
	d.localFilesMu.Lock()
	defer d.localFilesMu.Unlock()
	wantDir := absolutePath(status.Directory)
	inDir := d.localByDir[wantDir]
	for _, id := range status.ExpectedIDs {
		path, ok := inDir[id]
		if !ok {
			path, ok = d.localFiles[id]
			if !ok || filepath.Dir(path) != wantDir {
				return false
			}
		}
		if want, tracked := status.FileSizes[id]; tracked {
			info, err := os.Stat(path)
			if err != nil || info.Size() != want {
				return false
			}
		}
	}
	return true
}

// loadPhotosetStatuses bulk-loads every manifest for rootDir+ownerNSID in one
// query, avoiding a round trip per photoset. bulkLoaded is false (rather than
// an error) when there's no cache configured, so callers fall back cleanly.
func (d *Downloader) loadPhotosetStatuses(ctx context.Context, ownerNSID string) (statuses map[string]*cache.PhotosetStatus, bulkLoaded bool) {
	if d.Cache == nil {
		return nil, false
	}
	statuses, err := d.Cache.GetPhotosetStatuses(ctx, d.rootDir, ownerNSID)
	if err != nil {
		return nil, false
	}
	return statuses, true
}

// lookupPhotosetStatus resolves one manifest, preferring an already bulk
// loaded map when available.
func (d *Downloader) lookupPhotosetStatus(ctx context.Context, ownerNSID, photosetID string, statuses map[string]*cache.PhotosetStatus, bulkLoaded bool) *cache.PhotosetStatus {
	if bulkLoaded {
		return statuses[photosetID]
	}
	if d.Cache == nil {
		return nil
	}
	status, err := d.Cache.GetPhotosetStatus(ctx, d.rootDir, ownerNSID, photosetID)
	if err != nil {
		return nil
	}
	return status
}

// savePhotosetStatus persists status; a write failure is reported but never
// fatal since the manifest is an optimization, not the source of truth.
func (d *Downloader) savePhotosetStatus(ctx context.Context, status cache.PhotosetStatus) {
	if d.Cache == nil {
		return
	}
	if err := d.Cache.PutPhotosetStatus(ctx, status); err != nil {
		fmt.Fprintf(os.Stderr, "  %s%s save manifest for %s: %v%s\n",
			ui.ColorRed, ui.IconErr, status.Title, err, ui.ColorReset)
	}
}

// collectPhotosetMembership pages through photosetID's full listing and
// marks every photo ID as a member, used to keep deselected albums' photos
// out of the "uncategorized" bucket during a refresh.
func (d *Downloader) collectPhotosetMembership(ctx context.Context, photosetID string, membership map[string]bool) error {
	first, err := d.Client.GetPhotosByPhotoset(ctx, photosetID, 1)
	if err != nil {
		return err
	}
	for _, p := range first.Photoset.Photo {
		membership[p.ID] = true
	}
	pages := int(first.Photoset.Pages)
	for page := 2; page <= pages; page++ {
		resp, err := d.Client.GetPhotosByPhotoset(ctx, photosetID, page)
		if err != nil {
			return fmt.Errorf("photoset %s page %d: %w", photosetID, page, err)
		}
		for _, p := range resp.Photoset.Photo {
			membership[p.ID] = true
		}
	}
	return nil
}

// resolveDownloadURL avoids an extra flickr.photos.getSizes API call (which is
// the dominant bottleneck under the ~1req/sec API rate limit) by using the
// url_o extra returned alongside the photo listing whenever it's available.
// It falls back to getSizes for videos and for photos where Flickr omitted
// url_o (e.g. the owner disabled original-size downloads).
func (d *Downloader) resolveDownloadURL(ctx context.Context, photo api.Photo) (string, string, error) {
	if photo.Media != "video" && photo.URLOriginal != "" {
		return acceptDownloadURL(photo.URLOriginal)
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

	return acceptDownloadURL(downloadURL)
}

func fileExt(source string) string {
	idx := strings.LastIndex(source, ".")
	if idx < 0 {
		return "jpg"
	}
	return strings.ToLower(source[idx+1:])
}

func acceptDownloadURL(raw string) (string, string, error) {
	if !allowedDownloadURL(raw) {
		return "", "", fmt.Errorf("refusing download from non-Flickr host")
	}
	return raw, cleanExt(fileExt(raw)), nil
}

func allowedDownloadURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	return allowedDownloadHost(u.Hostname())
}

func allowedDownloadHost(host string) bool {
	host = strings.ToLower(strings.TrimPrefix(host, "www."))
	return host == "flickr.com" || strings.HasSuffix(host, ".flickr.com") ||
		host == "staticflickr.com" || strings.HasSuffix(host, ".staticflickr.com")
}

func cleanExt(ext string) string {
	if idx := strings.Index(ext, "?"); idx != -1 {
		ext = ext[:idx]
	}
	// Defense-in-depth: ext becomes the tail of a filepath.Join'd path, so it
	// must never carry a path separator or other filesystem-significant
	// character even if a future response source is less trustworthy than
	// today's Flickr CDN.
	for i, r := range ext {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			ext = ext[:i]
			break
		}
	}
	ext = strings.ToLower(ext)
	if mapped, ok := allowedExt[ext]; ok {
		return mapped
	}
	return "jpg"
}

// newDownloadHTTPClient keeps the 60s per-attempt Timeout required by
// ADR-005 / AC-003, adds a header timeout so a hung CDN does not burn the
// whole minute before retry, and refuses redirects off Flickr's CDN.
func newDownloadHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{
		Timeout:   60 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			if !allowedDownloadHost(req.URL.Hostname()) {
				return fmt.Errorf("refusing redirect to non-Flickr host %s", req.URL.Host)
			}
			return nil
		},
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
	wantDir := absolutePath(d.OutDir)
	d.localFilesMu.Lock()
	inDir := d.localByDir[wantDir]
	if inDir != nil {
		_, ok := inDir[photoID]
		d.localFilesMu.Unlock()
		if ok {
			return true
		}
	} else {
		d.localFilesMu.Unlock()
	}
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

// setAlbum records the album currently being processed, for the live status
// line. Called before each album's batch starts.
func (d *Downloader) setAlbum(name string) { d.currAlbum.Store(name) }

func (d *Downloader) currentAlbum() string {
	if v, ok := d.currAlbum.Load().(string); ok {
		return v
	}
	return ""
}

func (d *Downloader) setActiveFile(path string) {
	d.activeMu.Lock()
	d.activeFile = filepath.Base(path)
	d.activeMu.Unlock()
}

func (d *Downloader) clearActiveFile() {
	d.activeMu.Lock()
	d.activeFile = ""
	d.activeMu.Unlock()
}

func (d *Downloader) currentFile() string {
	d.activeMu.Lock()
	defer d.activeMu.Unlock()
	return d.activeFile
}

// recordAlbum snapshots the completed batch into the per-album breakdown.
func (d *Downloader) recordAlbum(name string, p *ui.Progress) ui.AlbumStat {
	st := p.Stats()
	stat := ui.AlbumStat{
		Name:    name,
		Success: st.Success,
		Skipped: st.Skipped,
		Linked:  st.Linked,
		Failed:  st.Failed,
		Bytes:   atomic.LoadInt64(&st.Bytes),
	}
	d.statsMu.Lock()
	d.albumStats = append(d.albumStats, stat)
	d.statsMu.Unlock()
	return stat
}

// AlbumStats returns a copy of the per-album results accumulated so far.
func (d *Downloader) AlbumStats() []ui.AlbumStat {
	d.statsMu.Lock()
	defer d.statsMu.Unlock()
	out := make([]ui.AlbumStat, len(d.albumStats))
	copy(out, d.albumStats)
	return out
}

func (d *Downloader) worker(ctx context.Context, photo api.Photo) {
	if d.alreadyDownloaded(photo.ID) {
		d.recordPhotoSuccess(photo.ID)
		d.progress.AddSkipped()
		return
	}

	// Cross-album dedupe: if this photo was already downloaded for another
	// album (this run, or a prior run indexed via WarmLocalIndex — ADR-023),
	// reuse it via a hardlink instead of downloading it again. On
	// filesystems without hardlink support (e.g. some network mounts) we
	// fall back to a regular download.
	d.pathMu.Lock()
	existing, ok := d.downloadedPaths[photo.ID]
	d.pathMu.Unlock()
	if !ok {
		d.localFilesMu.Lock()
		existing, ok = d.localFiles[photo.ID]
		d.localFilesMu.Unlock()
	}
	if ok {
		target := filepath.Join(d.OutDir, photo.ID+filepath.Ext(existing))
		if err := os.Link(existing, target); err == nil {
			d.noteLocalFile(photo.ID, target)
			d.recordPhotoSuccess(photo.ID)
			d.progress.AddLinked()
			return
		}
	}

	downloadURL, ext, err := d.resolveDownloadURL(ctx, photo)
	if err != nil {
		d.recordPhotoFailure(photo.ID, "", err.Error())
		return
	}

	filePath := filepath.Join(d.OutDir, photo.ID+"."+ext)

	d.setActiveFile(filePath)
	defer d.clearActiveFile()

	n, err := d.downloadFile(ctx, downloadURL, filePath)
	if err != nil {
		if !errors.Is(err, errCancelled) {
			d.recordPhotoFailure(photo.ID, downloadURL, err.Error())
		}
		return
	}

	d.pathMu.Lock()
	d.downloadedPaths[photo.ID] = filePath
	d.pathMu.Unlock()
	d.noteLocalFile(photo.ID, filePath)
	d.recordPhotoSuccess(photo.ID)

	d.progress.AddSuccess()
	d.progress.AddBytes(n)
}

func (d *Downloader) renderProgressLine() {
	page := atomic.LoadInt64(&d.currPage)
	total := atomic.LoadInt64(&d.totalPages)
	fmt.Printf("\r\033[K%s", d.progress.Render())

	// Quota indicators: a countdown while blocked on the hourly cap, and a
	// compact usage readout once the hour is nearly spent.
	if d.Client != nil {
		if blocked, until := d.Client.QuotaWaiting(); blocked && !until.IsZero() {
			fmt.Printf("  %s⏳ quota full — next slot in %s%s",
				ui.ColorYellow, ui.FormatDuration(time.Until(until)), ui.ColorReset)
		} else if used, limit, _ := d.Client.Quota(); limit > 0 && used*10 >= limit*9 {
			fmt.Printf("  %sAPI %d/%d%s", ui.ColorYellow, used, limit, ui.ColorReset)
		}
	}

	if album := d.currentAlbum(); album != "" {
		fmt.Printf("  %s%s%s", ui.ColorCyan, truncateRunes(album, 40), ui.ColorReset)
	}
	if file := d.currentFile(); file != "" {
		fmt.Printf("  %s▸ %s%s", ui.ColorDim, truncateRunes(file, 40), ui.ColorReset)
	}

	fmt.Printf("  %s▸ page %d/%d%s", ui.ColorDim, page, total, ui.ColorReset)
}

func truncateRunes(s string, w int) string {
	if utf8.RuneCountInString(s) <= w {
		return s
	}
	runes := []rune(s)
	return string(runes[:max(w-1, 1)]) + "…"
}

// startRenderer starts the progress-line ticker and returns a stop func that
// blocks until the goroutine has actually exited. Callers must call stop
// before mutating any field the renderer reads (d.progress, d.OutDir, ...)
// — ctx cancellation alone only requests the exit, it doesn't wait for it,
// so a caller that reassigns those fields right after cancelling can still
// race the renderer's last in-flight tick.
func (d *Downloader) startRenderer(ctx context.Context) (stop func()) {
	done := make(chan struct{})
	if d.Quiet {
		go func() {
			defer close(done)
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					d.logQuietProgress()
				}
			}
		}()
		return func() { <-done }
	}
	go func() {
		defer close(done)
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
	return func() { <-done }
}

// logQuietProgress emits one structured, greppable summary line in place of
// the interactive progress bar, for non-TTY/service use (watch mode).
func (d *Downloader) logQuietProgress() {
	if d.progress == nil {
		return
	}
	st := d.progress.Stats()
	done := atomic.LoadInt64(&st.Success) + atomic.LoadInt64(&st.Skipped) +
		atomic.LoadInt64(&st.Linked) + atomic.LoadInt64(&st.Failed)
	msg := fmt.Sprintf("progress album=%q done=%d/%d ok=%d skip=%d link=%d fail=%d",
		d.currentAlbum(), done, st.Total,
		atomic.LoadInt64(&st.Success), atomic.LoadInt64(&st.Skipped),
		atomic.LoadInt64(&st.Linked), atomic.LoadInt64(&st.Failed))
	if d.Logf != nil {
		d.Logf("%s", msg)
		return
	}
	fmt.Printf("%s INFO %s\n", time.Now().UTC().Format(time.RFC3339), msg)
}

func (d *Downloader) downloadPhotosFromPages(
	ctx context.Context,
	totalPhotos int,
	totalPages int,
	allowIDAbsentSweep bool,
	printCompletion bool,
	fetchPage func(ctx context.Context, page int) ([]api.Photo, error),
) Stats {
	d.progress = ui.NewProgress(totalPhotos)
	atomic.StoreInt64(&d.currPage, 0)
	atomic.StoreInt64(&d.totalPages, int64(totalPages))

	// seenIDs records every photo ID at enqueue time so the ID-absent sweep
	// never removes an artifact for a photo this run actually handled, even
	// if its worker never got to process the job (C10).
	seenIDs := make(map[string]bool)
	allPagesOK := true

	jobs := make(chan api.Photo, d.NumWorkers*2)
	parentCtx := ctx
	g, ctx := errgroup.WithContext(ctx)

	stopRenderer := d.startRenderer(ctx)

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
				timer := time.NewTimer(100 * time.Millisecond)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			photos, err := fetchPage(ctx, page)
			if err != nil {
				allPagesOK = false
				fmt.Fprintf(os.Stderr, "\r\033[K  %s%s API page %d: %v%s\n",
					ui.ColorRed, ui.IconErr, page, err, ui.ColorReset)
				continue
			}

			for _, photo := range photos {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case jobs <- photo:
					seenIDs[photo.ID] = true
				}
			}
			atomic.StoreInt64(&d.currPage, int64(page))
		}
		return nil
	})

	err := g.Wait()
	// The renderer goroutine only checks ctx.Done() between ticks, so it can
	// still be mid-render here even though ctx is already cancelled; block
	// until it has actually exited before returning, since the caller may
	// immediately reassign d.progress/d.OutDir for the next album.
	stopRenderer()
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return Stats{}
	}

	// Sweep only inside a per-album directory whose listing fully succeeded;
	// a partial listing or a cancelled run must never delete .part/.cand
	// files (AC-017, BR8).
	if allowIDAbsentSweep && allPagesOK && parentCtx.Err() == nil {
		sweepStaleParts(d.OutDir, seenIDs)
	}

	if !d.Quiet {
		fmt.Print("\r\033[K")
	}
	stat := d.recordAlbum(d.currentAlbum(), d.progress)
	if printCompletion && !d.Quiet {
		fmt.Print(ui.CompletionLine(stat))
	}

	return Stats{
		Total:   int64(totalPhotos),
		Success: d.progress.Stats().Success,
		Skipped: d.progress.Stats().Skipped,
		Linked:  d.progress.Stats().Linked,
		Failed:  d.progress.Stats().Failed,
	}
}

// collectOrphanPhotos walks every page of a user's photostream — the first
// page's photos are supplied directly to avoid a duplicate API call — and
// returns those not already covered by a photoset. A page fetch error is
// reported to stderr with the page number and cause, and a closing warning
// is printed once discovery finishes, rather than silently dropping that
// page's photos with no indication anything was missed.
func collectOrphanPhotos(pages int, firstPagePhotos []api.Photo, downloaded map[string]bool, fetchPage func(page int) ([]api.Photo, error)) []api.Photo {
	var orphans []api.Photo
	incomplete := false
	for page := 1; page <= pages; page++ {
		photos := firstPagePhotos
		if page > 1 {
			p, err := fetchPage(page)
			if err != nil {
				incomplete = true
				fmt.Fprintf(os.Stderr, "  %s%s uncategorized photos page %d: %v%s\n",
					ui.ColorRed, ui.IconErr, page, err, ui.ColorReset)
				continue
			}
			photos = p
		}
		for _, p := range photos {
			if !downloaded[p.ID] {
				orphans = append(orphans, p)
			}
		}
	}
	if incomplete {
		fmt.Fprintf(os.Stderr, "  %s%s some uncategorized photos may be missing from this run — rerun to retry%s\n",
			ui.ColorYellow, ui.IconErr, ui.ColorReset)
	}
	return orphans
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
	d.RetryFailedFirst(ctx)
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

	statuses, bulkLoaded := map[string]*cache.PhotosetStatus(nil), false
	if !d.Refresh {
		statuses, bulkLoaded = d.loadPhotosetStatuses(ctx, userID)
	}

	for _, set := range sets {
		setName := SafeName(set.Title.Content)
		setDir := filepath.Join(userDir, setName)

		if !d.Refresh {
			status := d.lookupPhotosetStatusForDir(ctx, userID, set.ID, setDir, statuses, bulkLoaded)
			if d.photosetListingReusable(status, set, setDir) {
				d.skipCompletedPhotoset(setName, status, downloaded)
				totalSkipped += int64(len(status.ExpectedIDs))
				continue
			}
		}

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
		d.setAlbum(setName)

		var listedIDs []string
		listingOK := true
		stats := d.downloadPhotosFromPages(ctx, total, pages, true, false,
			func(ctx context.Context, page int) ([]api.Photo, error) {
				var photos []api.Photo
				if page == 1 {
					photos = firstPage.Photoset.Photo
				} else {
					resp, err := d.Client.GetPhotosByPhotoset(ctx, set.ID, page)
					if err != nil {
						listingOK = false
						return nil, err
					}
					photos = resp.Photoset.Photo
				}
				for _, p := range photos {
					downloaded[p.ID] = true
					listedIDs = append(listedIDs, p.ID)
				}
				return photos, nil
			})

		if listingOK && stats.Failed == 0 && ctx.Err() == nil && len(listedIDs) == total {
			d.saveCompletePhotoset(ctx, userID, set.ID, setName, setDir, listedIDs, int64(set.UpdatedAt))
		}

		totalSuccess += stats.Success
		totalSkipped += stats.Skipped
		totalLinked += stats.Linked
		totalFailed += stats.Failed
	}

	// Download remaining photos not in any photoset
	includeOrphans := opts.IncludeOrphans || opts.Sets == nil
	firstPage := opts.FirstPage
	if firstPage == nil && includeOrphans {
		fp, err := d.Client.GetPhotosByUser(ctx, userID, 1)
		if err != nil {
			return Stats{}, fmt.Errorf("get first page: %w", err)
		}
		firstPage = fp
	}

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
		orphanPhotos = collectOrphanPhotos(int(firstPage.Photos.Pages), firstPage.Photos.Photo, downloaded,
			func(page int) ([]api.Photo, error) {
				resp, err := d.Client.GetPhotosByUser(ctx, userID, page)
				if err != nil {
					return nil, err
				}
				return resp.Photos.Photo, nil
			})
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
		d.setAlbum("Uncategorized photos")

		stats := d.downloadPhotosFromPages(ctx, orphans, 1, false, false,
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

	totalPhotos := int64(0)
	if firstPage != nil {
		totalPhotos = int64(int(firstPage.Photos.Total))
	}
	return Stats{
		Total:   totalPhotos,
		Success: totalSuccess,
		Skipped: totalSkipped,
		Linked:  totalLinked,
		Failed:  totalFailed,
	}, nil
}

func (d *Downloader) DownloadByPhotoset(ctx context.Context, photosetID string) (Stats, error) {
	d.RetryFailedFirst(ctx)
	info, infoErr := d.Client.GetPhotosetInfo(ctx, photosetID)
	ownerNSID := ""
	setName := photosetID
	if infoErr == nil {
		ownerNSID = info.Owner
		setName = SafeName(info.Title.Content)
		setDir := filepath.Join(d.OutDir, ownerNSID, setName)
		if !d.Refresh && ownerNSID != "" {
			_ = d.indexLocalFiles(filepath.Join(d.rootDir, ownerNSID))
			status := d.lookupPhotosetStatus(ctx, ownerNSID, photosetID, nil, false)
			if d.photosetListingReusable(status, *info, setDir) {
				d.skipCompletedPhotoset(setName, status, map[string]bool{})
				return Stats{Total: int64(len(status.ExpectedIDs)), Skipped: int64(len(status.ExpectedIDs))}, nil
			}
		}
	}

	firstPage, err := d.Client.GetPhotosByPhotoset(ctx, photosetID, 1)
	if err != nil {
		return Stats{}, fmt.Errorf("get first page: %w", err)
	}
	if ownerNSID == "" {
		ownerNSID = firstPage.Photoset.Owner
	}
	setDir := filepath.Join(d.OutDir, ownerNSID, setName)

	if err := os.MkdirAll(setDir, 0755); err != nil {
		return Stats{}, fmt.Errorf("create output dir: %w", err)
	}

	d.OutDir = setDir
	d.setAlbum(setName)

	total := int(firstPage.Photoset.Total)
	pages := int(firstPage.Photoset.Pages)

	var listedIDs []string
	listingOK := true
	stats := d.downloadPhotosFromPages(ctx, total, pages, true, true,
		func(ctx context.Context, page int) ([]api.Photo, error) {
			var photos []api.Photo
			if page == 1 {
				photos = firstPage.Photoset.Photo
			} else {
				resp, err := d.Client.GetPhotosByPhotoset(ctx, photosetID, page)
				if err != nil {
					listingOK = false
					return nil, err
				}
				photos = resp.Photoset.Photo
			}
			for _, p := range photos {
				listedIDs = append(listedIDs, p.ID)
			}
			return photos, nil
		})

	if listingOK && stats.Failed == 0 && ctx.Err() == nil && len(listedIDs) == total {
		updatedAt := int64(0)
		if info != nil {
			updatedAt = int64(info.UpdatedAt)
		}
		d.saveCompletePhotoset(ctx, ownerNSID, photosetID, setName, setDir, listedIDs, updatedAt)
	}

	return stats, nil
}

func (d *Downloader) DownloadPhoto(ctx context.Context, photoID string) error {
	d.RetryFailedFirst(ctx)
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
	d.setAlbum("Photo")

	photo := api.Photo{
		ID:     info.Photo.ID,
		Secret: info.Photo.Secret,
		Server: info.Photo.Server,
		Farm:   api.FlexInt(info.Photo.Farm),
		Title:  info.Photo.Title.Content,
		Owner:  owner,
	}

	d.worker(ctx, photo)
	stat := d.recordAlbum(d.currentAlbum(), d.progress)
	if !d.Quiet {
		fmt.Print("\r\033[K")
		fmt.Print(ui.CompletionLine(stat))
	}
	return nil
}
