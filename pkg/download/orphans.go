package download

import (
	"context"
	"fmt"

	"github.com/jooservices/flickrdownloader/pkg/api"
)

// orphanMembershipScope describes which albums determine whether a photo counts
// as "in an album" when detecting uncategorized/orphan photos.
type orphanMembershipScope struct {
	selectedSets []api.PhotoSetInfo
	allSets      []api.PhotoSetInfo
}

func newOrphanMembershipScope(selectedSets, allSets []api.PhotoSetInfo) orphanMembershipScope {
	if allSets == nil {
		allSets = selectedSets
	}
	return orphanMembershipScope{selectedSets: selectedSets, allSets: allSets}
}

func (s orphanMembershipScope) deselectedSets() []api.PhotoSetInfo {
	selected := make(map[string]bool, len(s.selectedSets))
	for _, set := range s.selectedSets {
		selected[set.ID] = true
	}
	var out []api.PhotoSetInfo
	for _, set := range s.allSets {
		if !selected[set.ID] {
			out = append(out, set)
		}
	}
	return out
}

// markDeselectedAlbumPhotos records membership for albums excluded from the
// current operation so their photos are not treated as uncategorized.
func (d *Downloader) markDeselectedAlbumPhotos(ctx context.Context, scope orphanMembershipScope, membership map[string]bool) error {
	for _, set := range scope.deselectedSets() {
		if err := d.collectPhotosetMembership(ctx, set.ID, membership); err != nil {
			return fmt.Errorf("discover membership for photoset %s: %w", set.ID, err)
		}
	}
	return nil
}
