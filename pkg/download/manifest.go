package download

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jooservices/flickrdownloader/pkg/api"
	"github.com/jooservices/flickrdownloader/pkg/cache"
	"github.com/jooservices/flickrdownloader/pkg/ui"
)

const localPhotosetIDPrefix = "__local__:"

func localPhotosetID(folder string) string {
	return localPhotosetIDPrefix + folder
}

// photosetListingReusable reports whether this album can skip flickr.photosets.getPhotos
// (ADR-021): complete manifest, same directory, same Flickr date_update, same
// expected count, and every recorded file still present at the recorded size.
// A zero source timestamp never skips — that is the disk-only scan case, and
// skipping on count alone would miss a same-count membership change.
func (d *Downloader) photosetListingReusable(status *cache.PhotosetStatus, set api.PhotoSetInfo, setDir string) bool {
	if status == nil || !status.Complete {
		return false
	}
	if status.SourceUpdatedAt == 0 || status.SourceUpdatedAt != int64(set.UpdatedAt) {
		return false
	}
	if absolutePath(status.Directory) != absolutePath(setDir) {
		return false
	}
	if int(set.Photos) != len(status.ExpectedIDs) {
		return false
	}
	if len(d.missingPhotosetManifestIDs(status)) > 0 {
		return false
	}
	return d.localPhotosetManifestComplete(status)
}

func (d *Downloader) lookupPhotosetStatusForDir(ctx context.Context, ownerNSID, photosetID, setDir string, statuses map[string]*cache.PhotosetStatus, bulkLoaded bool) *cache.PhotosetStatus {
	if status := d.lookupPhotosetStatus(ctx, ownerNSID, photosetID, statuses, bulkLoaded); status != nil {
		return status
	}
	if !bulkLoaded {
		return nil
	}
	want := absolutePath(setDir)
	for _, status := range statuses {
		if status != nil && absolutePath(status.Directory) == want {
			return status
		}
	}
	if name := filepath.Base(want); name != "" && name != "." {
		if status := statuses[localPhotosetID(name)]; status != nil {
			return status
		}
	}
	return nil
}

func (d *Downloader) recordSkippedAlbum(name string, n int) {
	stat := ui.AlbumStat{Name: name, Skipped: int64(n)}
	d.statsMu.Lock()
	d.albumStats = append(d.albumStats, stat)
	d.statsMu.Unlock()
}

func (d *Downloader) skipCompletedPhotoset(setName string, status *cache.PhotosetStatus, downloaded map[string]bool) {
	for _, id := range status.ExpectedIDs {
		downloaded[id] = true
	}
	d.setAlbum(setName)
	d.recordSkippedAlbum(setName, len(status.ExpectedIDs))
	if d.Quiet {
		return
	}
	fmt.Printf("\n  %s%s %s %s(%d photos)%s\n",
		ui.ColorCyan, ui.IconPhoto, setName, ui.ColorDim, len(status.ExpectedIDs), ui.ColorReset)
	fmt.Printf("  %s%s already complete — skipped listing%s\n",
		ui.ColorGreen, ui.IconOk, ui.ColorReset)
}

func (d *Downloader) manifestFilesInDir(dir string) (ids []string, sizes map[string]int64) {
	d.localFilesMu.Lock()
	inDir := d.localByDir[absolutePath(dir)]
	d.localFilesMu.Unlock()
	sizes = make(map[string]int64, len(inDir))
	for id, path := range inDir {
		ids = append(ids, id)
		if info, err := os.Stat(path); err == nil {
			sizes[id] = info.Size()
		}
	}
	sort.Strings(ids)
	return ids, sizes
}

func (d *Downloader) fileSizesInDir(dir string) map[string]int64 {
	_, sizes := d.manifestFilesInDir(dir)
	if sizes == nil {
		return map[string]int64{}
	}
	return sizes
}

func (d *Downloader) saveCompletePhotoset(ctx context.Context, userID, photosetID, title, setDir string, listedIDs []string, updatedAt int64) {
	if d.Cache == nil {
		return
	}
	_ = d.indexLocalFiles(setDir)
	sizes := d.fileSizesInDir(setDir)
	ids := append([]string(nil), listedIDs...)
	sort.Strings(ids)
	d.savePhotosetStatus(ctx, cache.PhotosetStatus{
		RootDir:         d.rootDir,
		OwnerNSID:       userID,
		PhotosetID:      photosetID,
		Title:           title,
		Directory:       absolutePath(setDir),
		ExpectedIDs:     ids,
		Complete:        true,
		SourceUpdatedAt: updatedAt,
		FileSizes:       sizes,
	})
}
