package download

import (
	"testing"

	"github.com/jooservices/flickrdownloader/pkg/api"
)

func TestEstimatePhotoBytes(t *testing.T) {
	photo := api.Photo{OWidth: 6000, OHeight: 4000, Media: "photo"}
	got := EstimatePhotoBytes(photo)
	want := int64(6000 * 4000 * 0.35)
	if got != want {
		t.Fatalf("expected %d, got %d", want, got)
	}

	video := api.Photo{OWidth: 1920, OHeight: 1080, Media: "video"}
	if got := EstimatePhotoBytes(video); got < minVideoBytes {
		t.Fatalf("video estimate %d below minimum %d", got, minVideoBytes)
	}

	unknown := api.Photo{Media: "photo"}
	if got := EstimatePhotoBytes(unknown); got != 0 {
		t.Fatalf("expected 0 for unknown dimensions, got %d", got)
	}
}

func TestEstimatePhotosBytes(t *testing.T) {
	photos := []api.Photo{
		{OWidth: 1000, OHeight: 1000, Media: "photo"},
		{OWidth: 1000, OHeight: 1000, Media: "photo"},
		{Media: "photo"}, // unknown dimensions
	}
	total, known := EstimatePhotosBytes(photos)
	if known != 2 {
		t.Fatalf("expected 2 known, got %d", known)
	}
	if want := int64(2 * 1000 * 1000 * 0.35); total != want {
		t.Fatalf("expected %d, got %d", want, total)
	}

	if avg := AvgPhotoBytes(photos); avg <= 0 {
		t.Fatalf("expected positive average, got %d", avg)
	}
}
