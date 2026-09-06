package download

import "github.com/jooservices/go-flickrdownloader/pkg/api"

// Size estimation constants. Flickr original JPEGs typically compress to
// roughly 0.25–0.5 bytes per pixel; videos are much denser per frame pixel.
const (
	photoBytesPerPixel = 0.35
	videoBytesPerPixel = 1.20
	minVideoBytes      = 10 << 20
)

// EstimatePhotoBytes guesses the original-file size of a photo or video from
// its original dimensions. Returns 0 when no dimensions are known.
func EstimatePhotoBytes(p api.Photo) int64 {
	w, h := int64(p.OWidth), int64(p.OHeight)
	if w <= 0 || h <= 0 {
		return 0
	}
	if p.Media == "video" {
		n := int64(float64(w) * float64(h) * videoBytesPerPixel)
		if n < minVideoBytes {
			n = minVideoBytes
		}
		return n
	}
	return int64(float64(w) * float64(h) * photoBytesPerPixel)
}

// EstimatePhotosBytes sums the estimates for photos whose dimensions are
// known and reports how many photos contributed to the total.
func EstimatePhotosBytes(photos []api.Photo) (total, known int64) {
	for _, p := range photos {
		if n := EstimatePhotoBytes(p); n > 0 {
			total += n
			known++
		}
	}
	return
}

// AvgPhotoBytes returns the mean estimated size across photos with known
// dimensions, or 0 when none are known. Useful for extrapolating a total
// size from a sampled page of the photostream.
func AvgPhotoBytes(photos []api.Photo) int64 {
	total, known := EstimatePhotosBytes(photos)
	if known == 0 {
		return 0
	}
	return total / known
}
