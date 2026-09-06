package api

import "testing"

func TestValidPhotoID(t *testing.T) {
	if !ValidPhotoID("12345678") {
		t.Fatal("expected valid photo id")
	}
	if ValidPhotoID("abc") || ValidPhotoID("123") || ValidPhotoID("../x") {
		t.Fatal("expected invalid photo ids to fail")
	}
}

func TestValidNSID(t *testing.T) {
	if !ValidNSID("123456789@N01") {
		t.Fatal("expected valid nsid")
	}
	if ValidNSID("owner/name") || ValidNSID("123") {
		t.Fatal("expected invalid nsids to fail")
	}
}

func TestValidatePhotosRejectsBadID(t *testing.T) {
	err := validatePhotos([]Photo{{ID: "../evil", Owner: "123456789@N01"}})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidatePhotoSetInfoRejectsBadID(t *testing.T) {
	if err := validatePhotoSetInfo(PhotoSetInfo{ID: "not-an-id"}); err == nil {
		t.Fatal("expected invalid photoset id to fail")
	}
	if err := validatePhotoSetInfo(PhotoSetInfo{ID: "72157600000001", Owner: "../evil"}); err == nil {
		t.Fatal("expected invalid owner nsid to fail")
	}
	if err := validatePhotoSetInfo(PhotoSetInfo{ID: "72157600000001", Owner: "123456789@N01"}); err != nil {
		t.Fatalf("valid photoset info rejected: %v", err)
	}
}

func TestValidatePhotoSetsRejectsAnyBadEntry(t *testing.T) {
	sets := []PhotoSetInfo{
		{ID: "72157600000001", Owner: "123456789@N01"},
		{ID: "not-an-id"},
	}
	if err := validatePhotoSets(sets); err == nil {
		t.Fatal("expected validation error for the second entry")
	}
	sets[1] = PhotoSetInfo{ID: "72157600000002", Owner: "123456789@N01"}
	if err := validatePhotoSets(sets); err != nil {
		t.Fatalf("valid photoset list rejected: %v", err)
	}
}

func TestValidatePhotoInfoRejectsBadOwner(t *testing.T) {
	var resp PhotoInfoResponse
	resp.Photo.ID = "12345678"
	resp.Photo.Owner.NSID = "../evil"
	if err := validatePhotoInfo(resp); err == nil {
		t.Fatal("expected invalid owner nsid to fail")
	}
	resp.Photo.Owner.NSID = "123456789@N01"
	if err := validatePhotoInfo(resp); err != nil {
		t.Fatalf("valid photo info rejected: %v", err)
	}
}
