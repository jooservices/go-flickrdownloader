package download

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jooservices/go-flickrdownloader/pkg/api"
	"github.com/jooservices/go-flickrdownloader/pkg/cache"
)

// ScanResult describes one album (or the uncategorized bucket) after a disk scan.
type ScanResult struct {
	ID             string
	Title          string
	Directory      string
	Present        int
	ExpectedRemote int
	Complete       bool
	Unmatched      bool
}

// ScanReport is the outcome of ScanUser.
type ScanReport struct {
	Photosets     []ScanResult
	Uncategorized *ScanResult
	Unmatched     []ScanResult
}

// ScanUser indexes local files under the user directory and writes photoset
// completion manifests. When sets is non-nil it binds folders to Flickr album
// IDs and date_update (from photosets.getList) so a later download can skip
// listing. A nil sets slice is the disk-only path: manifests are keyed by
// folder name and cannot skip listing until a later run records date_update.
func (d *Downloader) ScanUser(ctx context.Context, userID string, sets []api.PhotoSetInfo) (*ScanReport, error) {
	userDir := filepath.Join(d.OutDir, userID)
	if err := d.indexLocalFiles(userDir); err != nil {
		return nil, fmt.Errorf("scan local files: %w", err)
	}

	report := &ScanReport{}
	knownDirs := make(map[string]bool, len(sets))

	if sets != nil {
		for _, set := range sets {
			setName := SafeName(set.Title.Content)
			setDir := filepath.Join(userDir, setName)
			absoluteSetDir := absolutePath(setDir)
			knownDirs[absoluteSetDir] = true

			ids, sizes := d.manifestFilesInDir(setDir)
			remote := int(set.Photos)
			complete := remote == len(ids) && int64(set.UpdatedAt) != 0
			d.savePhotosetStatus(ctx, cache.PhotosetStatus{
				RootDir:         d.rootDir,
				OwnerNSID:       userID,
				PhotosetID:      set.ID,
				Title:           setName,
				Directory:       absoluteSetDir,
				ExpectedIDs:     ids,
				Complete:        complete,
				SourceUpdatedAt: int64(set.UpdatedAt),
				FileSizes:       sizes,
			})
			report.Photosets = append(report.Photosets, ScanResult{
				ID:             set.ID,
				Title:          setName,
				Directory:      absoluteSetDir,
				Present:        len(ids),
				ExpectedRemote: remote,
				Complete:       complete,
			})
		}
	}

	entries, err := os.ReadDir(userDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read user directory: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := absolutePath(filepath.Join(userDir, entry.Name()))
		if knownDirs[dir] {
			continue
		}
		ids, sizes := d.manifestFilesInDir(dir)
		if len(ids) == 0 {
			continue
		}
		title := SafeName(entry.Name())
		result := ScanResult{
			ID:        localPhotosetID(title),
			Title:     title,
			Directory: dir,
			Present:   len(ids),
			Unmatched: sets != nil,
			Complete:  sets == nil,
		}
		if sets == nil {
			d.savePhotosetStatus(ctx, cache.PhotosetStatus{
				RootDir:     d.rootDir,
				OwnerNSID:   userID,
				PhotosetID:  result.ID,
				Title:       title,
				Directory:   dir,
				ExpectedIDs: ids,
				Complete:    true,
				FileSizes:   sizes,
			})
		}
		report.Unmatched = append(report.Unmatched, result)
	}
	sort.Slice(report.Unmatched, func(i, j int) bool {
		return report.Unmatched[i].Title < report.Unmatched[j].Title
	})

	uncatIDs, uncatSizes := d.manifestFilesInDir(userDir)
	if len(uncatIDs) > 0 || sets != nil {
		complete := false
		d.savePhotosetStatus(ctx, cache.PhotosetStatus{
			RootDir:     d.rootDir,
			OwnerNSID:   userID,
			PhotosetID:  uncategorizedStatusID,
			Title:       "Uncategorized photos",
			Directory:   absolutePath(userDir),
			ExpectedIDs: uncatIDs,
			Complete:    complete,
			FileSizes:   uncatSizes,
		})
		report.Uncategorized = &ScanResult{
			ID:        uncategorizedStatusID,
			Title:     "Uncategorized photos",
			Directory: absolutePath(userDir),
			Present:   len(uncatIDs),
			Complete:  complete,
		}
	}

	return report, nil
}
