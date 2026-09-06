package api

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestClientRetriesRateLimitedResponseThenSucceeds verifies apiGet's retry
// wiring: a Flickr-side rate-limit response (stat=fail, code=429) is retried
// with the injected sleep rather than surfaced as an error, and a subsequent
// success is returned normally.
func TestClientRetriesRateLimitedResponseThenSucceeds(t *testing.T) {
	const limited = `{"stat":"fail","code":429,"message":"rate limit"}`
	const ok = `{"stat":"ok","photos":{"page":1,"pages":1,"perpage":500,"total":0,"photo":[]}}`

	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})

	var mu sync.Mutex
	calls := 0
	client.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls < 3 {
			return []byte(limited), nil
		}
		return []byte(ok), nil
	}

	var slept []time.Duration
	client.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	client.jitter = func() float64 { return 0 }

	if _, err := client.GetPhotosByUser(context.Background(), "owner", 1); err != nil {
		t.Fatalf("GetPhotosByUser: %v", err)
	}
	if calls != 3 {
		t.Fatalf("signedGet calls = %d, want 3 (2 rate-limited + 1 success)", calls)
	}
	if len(slept) != 2 {
		t.Fatalf("retry sleeps = %d, want 2", len(slept))
	}
	// Request count only reflects one logical request even though the
	// underlying transport was hit 3 times, since apiCalls is incremented
	// once per apiGet call (the singleflight-wrapped unit), not per retry.
	if client.RequestCount() != 1 {
		t.Fatalf("RequestCount() = %d, want 1", client.RequestCount())
	}
}

// TestClientRetryStopsOnContextCancellation ensures an unbounded retry loop
// (by design, for watch mode) still exits promptly when ctx is cancelled,
// rather than retrying forever.
func TestClientRetryStopsOnContextCancellation(t *testing.T) {
	const limited = `{"stat":"fail","code":429,"message":"rate limit"}`

	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})
	client.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		return []byte(limited), nil
	}
	client.sleep = func(ctx context.Context, _ time.Duration) error {
		return ctx.Err()
	}
	client.jitter = func() float64 { return 0 }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.GetPhotosByUser(ctx, "owner", 1)
	if err == nil {
		t.Fatal("expected an error when the retry loop's context is already cancelled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected a context.Canceled-wrapping error, got: %v", err)
	}
}

// TestClientRetriesHTTPStatusRateLimitThenSucceeds is a regression test for
// the gap where signedGetWithRetry only inspected the JSON response body for
// a rate-limit signal (Flickr's usual HTTP-200-with-stat=fail convention)
// and never looked at the actual HTTP status: a real transport-level 429
// (an edge/WAF throttle, distinct from Flickr's own quota) made signedGet
// return a plain error, which the retry loop propagated immediately with no
// retry, no backoff, and no wait — defeating watch mode's "wait through
// quota exhaustion instead of exiting" promise for anything but Flickr's own
// in-band signal.
func TestClientRetriesHTTPStatusRateLimitThenSucceeds(t *testing.T) {
	const ok = `{"stat":"ok","photos":{"page":1,"pages":1,"perpage":500,"total":0,"photo":[]}}`

	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})

	var mu sync.Mutex
	calls := 0
	client.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls < 3 {
			return nil, &httpStatusError{status: 429, body: []byte("Too Many Requests")}
		}
		return []byte(ok), nil
	}

	var slept []time.Duration
	client.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	client.jitter = func() float64 { return 0 }

	if _, err := client.GetPhotosByUser(context.Background(), "owner", 1); err != nil {
		t.Fatalf("GetPhotosByUser: %v", err)
	}
	if calls != 3 {
		t.Fatalf("signedGet calls = %d, want 3 (2 transport-429 + 1 success)", calls)
	}
	if len(slept) != 2 {
		t.Fatalf("retry sleeps = %d, want 2", len(slept))
	}
}

// TestClientRetriesHTTPStatus503ThenSucceeds covers a transient 5xx from
// Flickr's infrastructure — also invisible to isRateLimitResponse since the
// body is never Flickr's JSON envelope.
func TestClientRetriesHTTPStatus503ThenSucceeds(t *testing.T) {
	const ok = `{"stat":"ok","photos":{"page":1,"pages":1,"perpage":500,"total":0,"photo":[]}}`

	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})

	calls := 0
	client.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		calls++
		if calls < 2 {
			return nil, &httpStatusError{status: 503, body: []byte("Service Unavailable")}
		}
		return []byte(ok), nil
	}
	client.sleep = func(_ context.Context, _ time.Duration) error { return nil }
	client.jitter = func() float64 { return 0 }

	if _, err := client.GetPhotosByUser(context.Background(), "owner", 1); err != nil {
		t.Fatalf("GetPhotosByUser: %v", err)
	}
	if calls != 2 {
		t.Fatalf("signedGet calls = %d, want 2", calls)
	}
}

// TestClientHTTPStatusRetryHonorsRetryAfter checks that a Retry-After
// delivered on the transport-level error overrides the computed exponential
// backoff, matching the CDN download path's existing behavior.
func TestClientHTTPStatusRetryHonorsRetryAfter(t *testing.T) {
	const ok = `{"stat":"ok","photos":{"page":1,"pages":1,"perpage":500,"total":0,"photo":[]}}`

	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})

	calls := 0
	client.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		calls++
		if calls < 2 {
			return nil, &httpStatusError{status: 429, body: []byte("slow down"), retryAfter: 7 * time.Second, hasRetryAfter: true}
		}
		return []byte(ok), nil
	}
	var slept []time.Duration
	client.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	client.jitter = func() float64 { return 0.5 } // would inflate a computed backoff if used

	if _, err := client.GetPhotosByUser(context.Background(), "owner", 1); err != nil {
		t.Fatalf("GetPhotosByUser: %v", err)
	}
	if len(slept) != 1 || slept[0] != 7*time.Second {
		t.Fatalf("slept = %v, want a single 7s sleep from Retry-After", slept)
	}
}

// TestClientDoesNotRetryFatalHTTPStatus ensures an ordinary 4xx (not a
// rate-limit/transient signal) still fails immediately rather than being
// swept into the new retry path.
func TestClientDoesNotRetryFatalHTTPStatus(t *testing.T) {
	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})

	calls := 0
	client.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		calls++
		return nil, &httpStatusError{status: 404, body: []byte("not found")}
	}
	client.sleep = func(_ context.Context, _ time.Duration) error {
		t.Fatal("must not sleep/retry a fatal 404")
		return nil
	}

	_, err := client.GetPhotosByUser(context.Background(), "owner", 1)
	if err == nil {
		t.Fatal("expected an error for a fatal HTTP status")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("error = %v, want it to mention 404", err)
	}
	if calls != 1 {
		t.Fatalf("signedGet calls = %d, want 1 (no retry)", calls)
	}
}

// TestClientHTTPStatusRetryStopsOnContextCancellation mirrors
// TestClientRetryStopsOnContextCancellation for the transport-status retry
// path: unbounded in count, but still exits promptly on cancellation.
func TestClientHTTPStatusRetryStopsOnContextCancellation(t *testing.T) {
	client := NewClient("key", "secret", "token", "token-secret")
	client.SetRateLimiter(immediateLimiter{})
	client.signedGet = func(_, _, _, _, _ string, _ map[string]string) ([]byte, error) {
		return nil, &httpStatusError{status: 429, body: []byte("Too Many Requests")}
	}
	client.sleep = func(ctx context.Context, _ time.Duration) error {
		return ctx.Err()
	}
	client.jitter = func() float64 { return 0 }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.GetPhotosByUser(ctx, "owner", 1)
	if err == nil {
		t.Fatal("expected an error when the retry loop's context is already cancelled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected a context.Canceled-wrapping error, got: %v", err)
	}
}

// TestCacheKeyExcludesOAuthCredentials is a regression test for keeping
// signed-request secrets out of the on-disk response cache: the cache key
// must be derived only from the method and non-oauth params, so a cached row
// never carries an oauth_token/oauth_signature that could leak the account's
// credentials if the cache database were copied or inspected.
func TestCacheKeyExcludesOAuthCredentials(t *testing.T) {
	client := NewClient("key", "secret", "token", "token-secret")
	withSig := client.cacheKey("flickr.people.getPhotos", map[string]string{
		"user_id":         "123@N01",
		"oauth_token":     "secret-token",
		"oauth_signature": "secret-signature",
		"oauth_nonce":     "abc123",
		"format":          "json",
	})
	withoutSig := client.cacheKey("flickr.people.getPhotos", map[string]string{
		"user_id": "123@N01",
	})
	if withSig != withoutSig {
		t.Fatalf("cache key changed when oauth params were added: %q vs %q — oauth credentials are leaking into the cache key", withSig, withoutSig)
	}
	for _, secret := range []string{"secret-token", "secret-signature", "abc123"} {
		if strings.Contains(withSig, secret) {
			t.Fatalf("cache key %q contains OAuth secret %q", withSig, secret)
		}
	}
}
