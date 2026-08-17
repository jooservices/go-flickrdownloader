package api

import "fmt"

// ValidPhotoID reports whether id matches Flickr's numeric photo/photoset id shape.
func ValidPhotoID(id string) bool {
	return photoIDRe.MatchString(id)
}

// ValidNSID reports whether id matches Flickr's owner NSID shape (digits@Nxx).
func ValidNSID(id string) bool {
	return nsidRe.MatchString(id)
}

func validatePhoto(p Photo) error {
	if !ValidPhotoID(p.ID) {
		return fmt.Errorf("invalid photo id %q", p.ID)
	}
	if p.Owner != "" && !ValidNSID(p.Owner) {
		return fmt.Errorf("invalid owner nsid %q for photo %s", p.Owner, p.ID)
	}
	return nil
}

func validatePhotos(photos []Photo) error {
	for _, p := range photos {
		if err := validatePhoto(p); err != nil {
			return err
		}
	}
	return nil
}

func validatePhotoSetInfo(s PhotoSetInfo) error {
	if !ValidPhotoID(s.ID) {
		return fmt.Errorf("invalid photoset id %q", s.ID)
	}
	if s.Owner != "" && !ValidNSID(s.Owner) {
		return fmt.Errorf("invalid owner nsid %q for photoset %s", s.Owner, s.ID)
	}
	return nil
}

func validatePhotoSets(sets []PhotoSetInfo) error {
	for _, s := range sets {
		if err := validatePhotoSetInfo(s); err != nil {
			return err
		}
	}
	return nil
}
