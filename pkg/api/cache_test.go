package api

import (
	"context"
	"sync"
	"testing"
	"time"
)

type immediateLimiter struct{}

func (immediateLimiter) Wait(context.Context) error { return nil }

type memoryResponseCache struct {
	mu      sync.Mutex
	body    map[string][]byte
	fetched map[string]time.Time
}

func newMemoryResponseCache() *memoryResponseCache {
	return &memoryResponseCache{
		body:    make(map[string][]byte),
		fetched: make(map[string]time.Time),
	}
}

func (m *memoryResponseCache) Get(_ context.Context, key string) ([]byte, time.Time, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	body, ok := m.body[key]
	if !ok {
		return nil, time.Time{}, false, nil
	}
	return append([]byte(nil), body...), m.fetched[key], true, nil
}

func (m *memoryResponseCache) Put(_ context.Context, key string, body []byte, fetchedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.body[key] = append([]byte(nil), body...)
	m.fetched[key] = fetchedAt
	return nil
}

func (m *memoryResponseCache) PutResponse(ctx context.Context, key, _ string, body []byte, fetchedAt time.Time) error {
	return m.Put(ctx, key, body, fetchedAt)
}

func (m *memoryResponseCache) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.body, key)
	delete(m.fetched, key)
	return nil
}

func (m *memoryResponseCache) DeletePhotoMetadata(_ context.Context, _ string) error {
	return nil
}

func TestClientUsesConfiguredListingTTL(t *testing.T) {
	client := NewClient("key", "secret", "token", "token-secret")
	client.SetCacheListingTTL(3 * time.Hour)
	if got := client.cacheTTL("flickr.photosets.getPhotos"); got != 3*time.Hour {
		t.Fatalf("listing TTL = %v, want 3h", got)
	}
	if got := client.cacheTTL("flickr.photos.getSizes"); got != DefaultDetailTTL {
		t.Fatalf("detail TTL = %v, want %v", got, DefaultDetailTTL)
	}
}

func TestClientUsesResponseCacheBeforeRESTRequest(t *testing.T) {
	const response = `{"stat":"ok","photos":{"page":1,"pages":1,"perpage":500,"total":0,"photo":[]}}`
	calls := 0
	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})
	client.SetResponseCache(newMemoryResponseCache())
	client.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		calls++
		return []byte(response), nil
	}

	if _, err := client.GetPhotosByUser(context.Background(), "owner", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetPhotosByUser(context.Background(), "owner", 1); err != nil {
		t.Fatal(err)
	}

	if calls != 1 || client.RequestCount() != 1 {
		t.Fatalf("REST calls = %d, request count = %d, want one", calls, client.RequestCount())
	}
	if client.CacheHits() != 1 {
		t.Fatalf("cache hits = %d, want 1", client.CacheHits())
	}
}

func TestClientSingleflightCollapsesConcurrentMisses(t *testing.T) {
	const response = `{"stat":"ok","photos":{"page":1,"pages":1,"perpage":500,"total":0,"photo":[]}}`
	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})
	client.SetResponseCache(newMemoryResponseCache())
	var mu sync.Mutex
	calls := 0
	client.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(25 * time.Millisecond)
		return []byte(response), nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := client.GetPhotosByUser(context.Background(), "owner", 1); err != nil {
				t.Errorf("GetPhotosByUser: %v", err)
			}
		}()
	}
	wg.Wait()
	if calls != 1 || client.RequestCount() != 1 {
		t.Fatalf("REST calls = %d request count = %d, want one", calls, client.RequestCount())
	}
}

func TestClientCacheOnlyDoesNotIssueRESTRequest(t *testing.T) {
	const response = `{"stat":"ok","photos":{"page":1,"pages":1,"perpage":500,"total":0,"photo":[]}}`
	store := newMemoryResponseCache()
	seed := NewClient("key", "secret", "token", "token-secret")
	seed.SetRateLimiter(immediateLimiter{})
	seed.SetResponseCache(store)
	seed.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		return []byte(response), nil
	}
	if _, err := seed.GetPhotosByUser(context.Background(), "owner", 1); err != nil {
		t.Fatal(err)
	}

	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})
	client.SetResponseCache(store)
	client.SetCachePolicy(false, true)
	client.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		t.Fatal("cache-only client issued a REST request")
		return nil, nil
	}
	if _, err := client.GetPhotosByUser(context.Background(), "owner", 1); err != nil {
		t.Fatal(err)
	}
}

func TestClientCacheOnlyServesExpiredEntries(t *testing.T) {
	const response = `{"stat":"ok","photos":{"page":1,"pages":1,"perpage":500,"total":0,"photo":[]}}`
	store := newMemoryResponseCache()
	seed := NewClient("key", "secret", "token", "token-secret")
	seed.SetRateLimiter(immediateLimiter{})
	seed.SetResponseCache(store)
	seed.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		return []byte(response), nil
	}
	if _, err := seed.GetPhotosByUser(context.Background(), "owner", 1); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	for key := range store.fetched {
		store.fetched[key] = time.Now().Add(-48 * time.Hour)
	}
	store.mu.Unlock()

	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})
	client.SetResponseCache(store)
	client.SetCachePolicy(false, true)
	client.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		t.Fatal("cache-only client issued a REST request for an expired entry")
		return nil, nil
	}
	if _, err := client.GetPhotosByUser(context.Background(), "owner", 1); err != nil {
		t.Fatal(err)
	}
	if client.CacheHits() != 1 {
		t.Fatalf("cache hits = %d, want 1", client.CacheHits())
	}
}

const testAPIPhotoID = "123456789"

func TestInvalidatePhotoMetadataDeletesInfoAndSizes(t *testing.T) {
	store := newMemoryResponseCache()
	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})
	client.SetResponseCache(store)
	calls := 0
	client.signedGet = func(_, _, _, _, _ string, params map[string]string) ([]byte, error) {
		calls++
		switch params["method"] {
		case "flickr.photos.getSizes":
			return []byte(`{"stat":"ok","sizes":{"candownload":1,"size":[{"label":"Original","source":"https://example.invalid/1.jpg","media":"photo","width":1,"height":1}]}}`), nil
		case "flickr.photos.getInfo":
			return []byte(`{"stat":"ok","photo":{"id":"123456789","secret":"s","server":"1","farm":1,"owner":{"nsid":"123456789@N01","username":"u"},"title":{"_content":"t"},"originalsecret":"os","originalformat":"jpg","media":"photo"}}`), nil
		default:
			t.Fatalf("unexpected method %s", params["method"])
			return nil, nil
		}
	}
	if _, err := client.GetSizes(context.Background(), testAPIPhotoID); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetPhotoInfo(context.Background(), testAPIPhotoID); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("seed calls = %d, want 2", calls)
	}
	client.InvalidatePhotoMetadata(context.Background(), testAPIPhotoID)
	if _, err := client.GetSizes(context.Background(), testAPIPhotoID); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetPhotoInfo(context.Background(), testAPIPhotoID); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatalf("calls after invalidate = %d, want 4", calls)
	}
}
