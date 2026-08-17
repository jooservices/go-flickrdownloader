package download

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jooservices/flickrdownloader/pkg/api"
	"github.com/jooservices/flickrdownloader/pkg/cache"
)

type VerificationState string

const (
	VerificationComplete   VerificationState = "complete"
	VerificationIncomplete VerificationState = "incomplete"
	VerificationNotScanned VerificationState = "not-scanned"
	VerificationStale      VerificationState = "stale"
	VerificationError      VerificationState = "error"
)

// PhotosetVerification describes one photoset without downloading anything.
type PhotosetVerification struct {
	ID        string
	Title     string
	Directory string
	State     VerificationState
	Expected  int
	Present   int
	Missing   []string
	Error     string
	Refreshed bool
}

type VerificationReport struct {
	Photosets        []PhotosetVerification
	StaleDirectories []string
	Uncategorized    *PhotosetVerification
}

// VerifyUserOpts configures VerifyUser. AllSets is the full album list used
// for orphan membership during refresh; when nil it defaults to Sets.
type VerifyUserOpts struct {
	AuthenticatedNSID string
	AllSets           []api.PhotoSetInfo
}

// VerifyUser compares photoset manifests and local files. Without refresh it
// never asks Flickr for photo pages; refresh rebuilds each expected-ID list.
func (d *Downloader) VerifyUser(ctx context.Context, userID string, sets []api.PhotoSetInfo, refresh bool, opts ...VerifyUserOpts) (*VerificationReport, error) {
	var opt VerifyUserOpts
	if len(opts) > 0 {
		opt = opts[0]
	}
	allSets := opt.AllSets
	if allSets == nil {
		allSets = sets
	}

	userDir := filepath.Join(d.OutDir, userID)
	if err := d.indexLocalFiles(userDir); err != nil {
		return nil, fmt.Errorf("scan local files: %w", err)
	}

	statuses, bulkLoaded := map[string]*cache.PhotosetStatus(nil), false
	if !refresh {
		statuses, bulkLoaded = d.loadPhotosetStatuses(ctx, userID)
	}
	report := &VerificationReport{}
	knownDirs := make(map[string]bool, len(sets))
	albumPhotoIDs := make(map[string]bool)

	for _, set := range sets {
		setName := SafeName(set.Title.Content)
		setDir := filepath.Join(userDir, setName)
		absoluteSetDir := absolutePath(setDir)
		knownDirs[absoluteSetDir] = true

		status := (*cache.PhotosetStatus)(nil)
		if !refresh {
			status = d.lookupPhotosetStatus(ctx, userID, set.ID, statuses, bulkLoaded)
		}

		verification := PhotosetVerification{ID: set.ID, Title: setName, Directory: absoluteSetDir, Refreshed: refresh}
		if refresh {
			expected, total, err := d.refreshPhotosetIDs(ctx, set.ID)
			if err != nil {
				verification.State = VerificationError
				verification.Error = err.Error()
				report.Photosets = append(report.Photosets, verification)
				continue
			}
			verification.Expected = len(expected)
			for _, photoID := range expected {
				albumPhotoIDs[photoID] = true
			}
			verification.Missing = d.missingPhotosetIDs(setDir, expected)
			verification.Present = verification.Expected - len(verification.Missing)
			if len(expected) < total {
				verification.State = VerificationError
				verification.Error = fmt.Sprintf("listing returned %d of %d photos", len(expected), total)
			} else if len(verification.Missing) == 0 {
				verification.State = VerificationComplete
			} else {
				verification.State = VerificationIncomplete
			}
			if verification.State == VerificationComplete || verification.State == VerificationIncomplete {
				d.savePhotosetStatus(ctx, cache.PhotosetStatus{
					RootDir: d.rootDir, OwnerNSID: userID, PhotosetID: set.ID,
					Title: setName, Directory: absoluteSetDir, ExpectedIDs: expected,
					Complete: verification.State == VerificationComplete, SourceUpdatedAt: int64(set.UpdatedAt),
				})
			}
		} else if status == nil {
			verification.State = VerificationNotScanned
		} else if absolutePath(status.Directory) != absoluteSetDir {
			verification.State = VerificationStale
			verification.Expected = len(status.ExpectedIDs)
			verification.Missing = append([]string(nil), status.ExpectedIDs...)
		} else {
			verification.Expected = len(status.ExpectedIDs)
			verification.Missing = d.missingPhotosetManifestIDs(status)
			verification.Present = verification.Expected - len(verification.Missing)
			if status.Complete && len(verification.Missing) == 0 && int(set.Photos) == verification.Expected &&
				status.SourceUpdatedAt == int64(set.UpdatedAt) &&
				d.localPhotosetManifestComplete(status) {
				verification.State = VerificationComplete
			} else {
				verification.State = VerificationIncomplete
			}
		}
		report.Photosets = append(report.Photosets, verification)
	}

	entries, err := os.ReadDir(userDir)
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			path := absolutePath(filepath.Join(userDir, entry.Name()))
			if !knownDirs[path] {
				report.StaleDirectories = append(report.StaleDirectories, path)
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("scan stale directories: %w", err)
	}
	sort.Strings(report.StaleDirectories)

	if !refresh {
		status := d.lookupPhotosetStatus(ctx, userID, uncategorizedStatusID, statuses, bulkLoaded)
		if status == nil {
			report.Uncategorized = &PhotosetVerification{
				Title: "Uncategorized photos", Directory: absolutePath(userDir), State: VerificationNotScanned,
			}
		} else {
			missing := d.missingPhotosetManifestIDs(status)
			state := VerificationIncomplete
			if status.Complete && len(missing) == 0 {
				state = VerificationComplete
			}
			report.Uncategorized = &PhotosetVerification{
				ID: uncategorizedStatusID, Title: "Uncategorized photos", Directory: absolutePath(userDir),
				State: state, Expected: len(status.ExpectedIDs), Present: len(status.ExpectedIDs) - len(missing), Missing: missing,
			}
		}
	} else {
		scope := newOrphanMembershipScope(sets, allSets)
		if err := d.markDeselectedAlbumPhotos(ctx, scope, albumPhotoIDs); err != nil {
			return nil, err
		}
		expected, err := d.refreshUncategorizedIDs(ctx, userID, albumPhotoIDs, opt.AuthenticatedNSID)
		verification := &PhotosetVerification{ID: uncategorizedStatusID, Title: "Uncategorized photos", Directory: absolutePath(userDir), Refreshed: true}
		if err != nil {
			verification.State = VerificationError
			verification.Error = err.Error()
		} else {
			verification.Expected = len(expected)
			verification.Missing = d.missingPhotosetIDs(userDir, expected)
			verification.Present = verification.Expected - len(verification.Missing)
			if len(verification.Missing) == 0 {
				verification.State = VerificationComplete
			} else {
				verification.State = VerificationIncomplete
			}
			if verification.State == VerificationComplete || verification.State == VerificationIncomplete {
				d.savePhotosetStatus(ctx, cache.PhotosetStatus{
					RootDir: d.rootDir, OwnerNSID: userID, PhotosetID: uncategorizedStatusID,
					Title: verification.Title, Directory: verification.Directory, ExpectedIDs: expected,
					Complete: verification.State == VerificationComplete,
				})
			}
		}
		report.Uncategorized = verification
	}
	return report, nil
}

func (d *Downloader) refreshPhotosetIDs(ctx context.Context, photosetID string) ([]string, int, error) {
	first, err := d.Client.GetPhotosByPhotoset(ctx, photosetID, 1)
	if err != nil {
		return nil, 0, err
	}
	total := int(first.Photoset.Total)
	pages := int(first.Photoset.Pages)
	ids := make(map[string]bool)
	for _, photo := range first.Photoset.Photo {
		ids[photo.ID] = true
	}
	for page := 2; page <= pages; page++ {
		resp, err := d.Client.GetPhotosByPhotoset(ctx, photosetID, page)
		if err != nil {
			return nil, total, fmt.Errorf("photoset page %d: %w", page, err)
		}
		for _, photo := range resp.Photoset.Photo {
			ids[photo.ID] = true
		}
	}
	expected := make([]string, 0, len(ids))
	for id := range ids {
		expected = append(expected, id)
	}
	sort.Strings(expected)
	return expected, total, nil
}

func (d *Downloader) refreshUncategorizedIDs(ctx context.Context, userID string, albumIDs map[string]bool, authenticatedNSID string) ([]string, error) {
	if authenticatedNSID == userID {
		first, err := d.Client.GetPhotosNotInSet(ctx, 1)
		if err != nil {
			return nil, err
		}
		ids := make(map[string]bool)
		for _, photo := range first.Photos.Photo {
			ids[photo.ID] = true
		}
		for page := 2; page <= int(first.Photos.Pages); page++ {
			resp, err := d.Client.GetPhotosNotInSet(ctx, page)
			if err != nil {
				return nil, fmt.Errorf("uncategorized page %d: %w", page, err)
			}
			for _, photo := range resp.Photos.Photo {
				ids[photo.ID] = true
			}
		}
		return sortedIDs(ids), nil
	}

	first, err := d.Client.GetPhotosByUser(ctx, userID, 1)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]bool)
	for _, photo := range first.Photos.Photo {
		if !albumIDs[photo.ID] {
			ids[photo.ID] = true
		}
	}
	for page := 2; page <= int(first.Photos.Pages); page++ {
		resp, err := d.Client.GetPhotosByUser(ctx, userID, page)
		if err != nil {
			return nil, fmt.Errorf("photostream page %d: %w", page, err)
		}
		for _, photo := range resp.Photos.Photo {
			if !albumIDs[photo.ID] {
				ids[photo.ID] = true
			}
		}
	}
	return sortedIDs(ids), nil
}

func sortedIDs(ids map[string]bool) []string {
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
