package api

import (
	"context"
	"fmt"
	"testing"
	"time"
)

const okOwnerNSID = "123456789@N01"

func TestGetPhotosByPhotoset(t *testing.T) {
	const resp = `{"stat":"ok","photoset":{"id":"72157600000001","owner":"123456789@N01","page":1,"pages":1,"perpage":500,"total":1,"photo":[{"id":"11111111","owner":"123456789@N01"}]}}`
	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})
	client.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		return []byte(resp), nil
	}

	got, err := client.GetPhotosByPhotoset(context.Background(), "72157600000001", 1)
	if err != nil {
		t.Fatalf("GetPhotosByPhotoset: %v", err)
	}
	if len(got.Photoset.Photo) != 1 || got.Photoset.Photo[0].ID != "11111111" {
		t.Fatalf("got %+v", got)
	}
}

func TestGetPhotosByPhotosetRejectsInvalidOwner(t *testing.T) {
	const resp = `{"stat":"ok","photoset":{"id":"72157600000001","owner":"../evil","page":1,"pages":1,"perpage":500,"total":0,"photo":[]}}`
	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})
	client.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		return []byte(resp), nil
	}

	if _, err := client.GetPhotosByPhotoset(context.Background(), "72157600000001", 1); err == nil {
		t.Fatal("expected an error for an invalid owner nsid")
	}
}

func TestGetPhotosByPhotosetPropagatesFlickrError(t *testing.T) {
	const resp = `{"stat":"fail","code":1,"message":"Photoset not found"}`
	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})
	client.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		return []byte(resp), nil
	}

	if _, err := client.GetPhotosByPhotoset(context.Background(), "72157600000001", 1); err == nil {
		t.Fatal("expected an error for a Flickr-side failure")
	}
}

func TestGetPhotosNotInSet(t *testing.T) {
	const resp = `{"stat":"ok","photos":{"page":1,"pages":1,"perpage":500,"total":1,"photo":[{"id":"22222222"}]}}`
	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})
	client.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		return []byte(resp), nil
	}

	got, err := client.GetPhotosNotInSet(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetPhotosNotInSet: %v", err)
	}
	if len(got.Photos.Photo) != 1 || got.Photos.Photo[0].ID != "22222222" {
		t.Fatalf("got %+v", got)
	}
}

func TestGetPhotosNotInSetRejectsBadPhotoID(t *testing.T) {
	const resp = `{"stat":"ok","photos":{"page":1,"pages":1,"perpage":500,"total":1,"photo":[{"id":"not-an-id"}]}}`
	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})
	client.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		return []byte(resp), nil
	}

	if _, err := client.GetPhotosNotInSet(context.Background(), 1); err == nil {
		t.Fatal("expected an error for an invalid photo id")
	}
}

// TestGetPhotosetsPaginated drives the internal pagination loop across two
// pages, verifying it stops exactly at resp.Photosets.Pages and aggregates
// every page's entries.
func TestGetPhotosetsPaginated(t *testing.T) {
	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})
	calls := 0
	client.signedGet = func(_, _, _, _, _ string, params map[string]string) ([]byte, error) {
		calls++
		page := params["page"]
		switch page {
		case "1":
			return []byte(`{"stat":"ok","photosets":{"page":1,"pages":2,"perpage":500,"total":2,"photoset":[{"id":"72157600000001","owner":"123456789@N01"}]}}`), nil
		case "2":
			return []byte(`{"stat":"ok","photosets":{"page":2,"pages":2,"perpage":500,"total":2,"photoset":[{"id":"72157600000002","owner":"123456789@N01"}]}}`), nil
		default:
			return nil, fmt.Errorf("unexpected page %q", page)
		}
	}

	got, err := client.GetPhotosets(context.Background(), okOwnerNSID)
	if err != nil {
		t.Fatalf("GetPhotosets: %v", err)
	}
	if calls != 2 {
		t.Fatalf("signedGet calls = %d, want 2", calls)
	}
	if len(got) != 2 || got[0].ID != "72157600000001" || got[1].ID != "72157600000002" {
		t.Fatalf("got %+v", got)
	}
}

func TestGetPhotosetsPropagatesFlickrError(t *testing.T) {
	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})
	client.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		return []byte(`{"stat":"fail","code":1,"message":"User not found"}`), nil
	}

	if _, err := client.GetPhotosets(context.Background(), okOwnerNSID); err == nil {
		t.Fatal("expected an error for a Flickr-side failure")
	}
}

func TestGetPhotosetsRejectsInvalidEntry(t *testing.T) {
	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})
	client.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		return []byte(`{"stat":"ok","photosets":{"page":1,"pages":1,"perpage":500,"total":1,"photoset":[{"id":"not-an-id"}]}}`), nil
	}

	if _, err := client.GetPhotosets(context.Background(), okOwnerNSID); err == nil {
		t.Fatal("expected an error for an invalid photoset entry")
	}
}

func TestGetPhotosetInfo(t *testing.T) {
	const resp = `{"stat":"ok","photoset":{"id":"72157600000001","owner":"123456789@N01","title":{"_content":"Album A"}}}`
	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})
	client.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		return []byte(resp), nil
	}

	got, err := client.GetPhotosetInfo(context.Background(), "72157600000001")
	if err != nil {
		t.Fatalf("GetPhotosetInfo: %v", err)
	}
	if got.Title.Content != "Album A" || got.Owner != okOwnerNSID {
		t.Fatalf("got %+v", got)
	}
}

func TestGetPhotosetInfoPropagatesFlickrError(t *testing.T) {
	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})
	client.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		return []byte(`{"stat":"fail","code":1,"message":"Photoset not found"}`), nil
	}

	if _, err := client.GetPhotosetInfo(context.Background(), "72157600000001"); err == nil {
		t.Fatal("expected an error for a Flickr-side failure")
	}
}

func TestGetPhotosetInfoRejectsInvalidInfo(t *testing.T) {
	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})
	client.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		return []byte(`{"stat":"ok","photoset":{"id":"not-an-id"}}`), nil
	}

	if _, err := client.GetPhotosetInfo(context.Background(), "72157600000001"); err == nil {
		t.Fatal("expected an error for an invalid photoset id in the response")
	}
}

func TestLookupUser(t *testing.T) {
	const resp = `{"stat":"ok","user":{"id":"123456789@N01","username":{"_content":"alice"}}}`
	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})
	client.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		return []byte(resp), nil
	}

	got, err := client.LookupUser(context.Background(), "https://www.flickr.com/photos/alice/")
	if err != nil {
		t.Fatalf("LookupUser: %v", err)
	}
	if got != okOwnerNSID {
		t.Fatalf("LookupUser = %q, want %q", got, okOwnerNSID)
	}
}

func TestLookupUserRejectsInvalidNSID(t *testing.T) {
	const resp = `{"stat":"ok","user":{"id":"not-an-nsid","username":{"_content":"alice"}}}`
	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})
	client.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		return []byte(resp), nil
	}

	if _, err := client.LookupUser(context.Background(), "https://www.flickr.com/photos/alice/"); err == nil {
		t.Fatal("expected an error for an invalid nsid")
	}
}

func TestLookupUserPropagatesFlickrError(t *testing.T) {
	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})
	client.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		return []byte(`{"stat":"fail","code":1,"message":"User not found"}`), nil
	}

	if _, err := client.LookupUser(context.Background(), "https://www.flickr.com/photos/alice/"); err == nil {
		t.Fatal("expected an error for a Flickr-side failure")
	}
}

// fakeQuotaLimiter implements RateLimiter plus the duck-typed Usage/Waiting
// interfaces Quota/QuotaWaiting probe for, exercising the "has one" branch;
// the default rate.Limiter (used when no tracker is installed) exercises
// the "doesn't have one" branch.
type fakeQuotaLimiter struct {
	used, limit int
	resetAt     time.Time
	waiting     bool
	waitUntil   time.Time
}

func (f fakeQuotaLimiter) Wait(context.Context) error { return nil }
func (f fakeQuotaLimiter) Usage() (int, int, time.Time) {
	return f.used, f.limit, f.resetAt
}
func (f fakeQuotaLimiter) Waiting() (bool, time.Time) { return f.waiting, f.waitUntil }

func TestQuotaWithAndWithoutTracker(t *testing.T) {
	client := NewClient("key", "secret", "token", "token-secret")
	if used, limit, _ := client.Quota(); used != 0 || limit != 0 {
		t.Fatalf("Quota with no tracker = %d/%d, want 0/0", used, limit)
	}

	resetAt := time.Now().Add(time.Hour)
	client.SetRateLimiter(fakeQuotaLimiter{used: 5, limit: 3600, resetAt: resetAt})
	used, limit, got := client.Quota()
	if used != 5 || limit != 3600 || !got.Equal(resetAt) {
		t.Fatalf("Quota = %d/%d reset=%v, want 5/3600 reset=%v", used, limit, got, resetAt)
	}
}

func TestQuotaWaitingWithAndWithoutTracker(t *testing.T) {
	client := NewClient("key", "secret", "token", "token-secret")
	if blocked, _ := client.QuotaWaiting(); blocked {
		t.Fatal("QuotaWaiting with no tracker should report unblocked")
	}

	until := time.Now().Add(time.Minute)
	client.SetRateLimiter(fakeQuotaLimiter{waiting: true, waitUntil: until})
	blocked, got := client.QuotaWaiting()
	if !blocked || !got.Equal(until) {
		t.Fatalf("QuotaWaiting = blocked=%v until=%v, want true/%v", blocked, got, until)
	}
}
