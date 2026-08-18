package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"
)

type UserResponse struct {
	User struct {
		ID       string `json:"id"`
		Username struct {
			Content string `json:"_content"`
		} `json:"username"`
	} `json:"user"`
	Stat    string `json:"stat"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	baseURL       = "https://api.flickr.com/services/rest"
	photosPerPage = 500

	// listingMethods classifies which REST methods are "listing" calls for
	// cache TTL and pruning purposes: their results change as a user adds
	// photos, so they're kept fresh on a shorter, configurable TTL. Everything
	// else (getInfo, getSizes, ...) describes an existing photo/photoset and
	// rarely changes, so it gets the longer DefaultDetailTTL.
	methodGetPhotosByUser     = "flickr.people.getPhotos"
	methodGetPhotosByPhotoset = "flickr.photosets.getPhotos"
	methodGetPhotosets        = "flickr.photosets.getList"
	methodGetPhotosNotInSet   = "flickr.photos.getNotInSet"
	methodGetSizes            = "flickr.photos.getSizes"
	methodGetPhotosetInfo     = "flickr.photosets.getInfo"
	methodGetPhotoInfo        = "flickr.photos.getInfo"
)

// DefaultDetailTTL is how long cached detail responses (getSizes, getInfo,
// getPhotosetInfo — anything describing a single, rarely-changing object)
// are served without a live check.
const DefaultDetailTTL = 30 * 24 * time.Hour

var listingMethodSet = map[string]bool{
	methodGetPhotosByUser:     true,
	methodGetPhotosByPhotoset: true,
	methodGetPhotosets:        true,
	methodGetPhotosNotInSet:   true,
}

// RateLimiter gates REST API requests. A persisted quota.Tracker implements
// it; the default internal limiter paces at ~1 req/sec.
type RateLimiter interface {
	Wait(ctx context.Context) error
}

// ResponseCache persists successful REST responses across runs. pkg/cache.Store
// implements this; tests substitute an in-memory fake.
type ResponseCache interface {
	Get(ctx context.Context, key string) (body []byte, fetchedAt time.Time, found bool, err error)
	Put(ctx context.Context, key string, body []byte, fetchedAt time.Time) error
	PutResponse(ctx context.Context, key, method string, body []byte, fetchedAt time.Time) error
	Delete(ctx context.Context, key string) error
	DeletePhotoMetadata(ctx context.Context, photoID string) error
}

type Client struct {
	APIKey       string
	APISecret    string
	AccessToken  string
	AccessSecret string
	rateLimiter  RateLimiter
	apiCalls     atomic.Int64

	cache           ResponseCache
	cacheHits       atomic.Int64
	cacheListingTTL time.Duration
	refresh         bool
	offline         bool
	group           singleflight.Group

	// signedGet is overridden in tests to avoid real network calls.
	signedGet func(apiKey, apiSecret, accessToken, accessSecret, uri string, queryParams map[string]string) ([]byte, error)
	// sleep/jitter back the 429 retry loop; overridden in tests.
	sleep  retrySleep
	jitter retryJitter
}

func NewClient(apiKey, apiSecret, accessToken, accessSecret string) *Client {
	return &Client{
		APIKey:          apiKey,
		APISecret:       apiSecret,
		AccessToken:     accessToken,
		AccessSecret:    accessSecret,
		rateLimiter:     rate.NewLimiter(rate.Every(1050*time.Millisecond), 1),
		cacheListingTTL: 24 * time.Hour,
		signedGet:       SignedGet,
		sleep:           defaultRetrySleep,
		jitter:          func() float64 { return 0.1 * rand.Float64() },
	}
}

// SetRateLimiter replaces the default ~1 req/sec pacing with a custom gate,
// e.g. a persisted quota tracker enforcing Flickr's 3600/hour cap across runs.
func (c *Client) SetRateLimiter(rl RateLimiter) { c.rateLimiter = rl }

// SetResponseCache installs a persistent response cache. A nil cache (the
// default) makes every request a live REST call, as before 1.3.0.
func (c *Client) SetResponseCache(cache ResponseCache) { c.cache = cache }

// SetCacheListingTTL overrides how long listing responses (getPhotosByUser,
// getPhotosByPhotoset, getPhotosets, getPhotosNotInSet) are served from cache
// before a live re-check. Detail responses always use DefaultDetailTTL.
func (c *Client) SetCacheListingTTL(d time.Duration) { c.cacheListingTTL = d }

// SetCachePolicy controls whether a cache hit is bypassed (refresh) or a
// cache miss is forbidden from falling through to a live request (offline).
// Setting both is contradictory; callers should reject that combination
// before it reaches the client — offline takes priority here as the safer
// failure mode.
func (c *Client) SetCachePolicy(refresh, offline bool) {
	c.refresh = refresh
	c.offline = offline
}

// CacheHits reports how many requests were served from the response cache.
func (c *Client) CacheHits() int64 { return c.cacheHits.Load() }

// RequestCount reports how many REST API calls this client has made.
func (c *Client) RequestCount() int64 { return c.apiCalls.Load() }

// cacheTTL returns how long a cached response for method is considered
// fresh: the configured listing TTL for listing methods, DefaultDetailTTL
// for everything else.
func (c *Client) cacheTTL(method string) time.Duration {
	if listingMethodSet[method] {
		return c.cacheListingTTL
	}
	return DefaultDetailTTL
}

// cacheKey derives a cache key from the method and request params only —
// deliberately excluding oauth_token/oauth_signature/oauth_nonce/
// oauth_timestamp (and the format/nojsoncallback/method boilerplate already
// carried by the method prefix) so a signed credential can never end up
// persisted on disk inside the cache database.
func (c *Client) cacheKey(method string, params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		switch k {
		case "oauth_token", "oauth_signature", "oauth_nonce", "oauth_timestamp",
			"oauth_consumer_key", "oauth_signature_method", "oauth_version",
			"format", "nojsoncallback", "method":
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(method)
	for _, k := range keys {
		b.WriteByte('|')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(params[k])
	}
	return b.String()
}

// Quota reports hourly quota usage through the installed rate limiter when it
// exposes one (zero limit when no tracker is set).
func (c *Client) Quota() (used, limit int, resetAt time.Time) {
	if q, ok := c.rateLimiter.(interface {
		Usage() (int, int, time.Time)
	}); ok {
		return q.Usage()
	}
	return 0, 0, time.Time{}
}

// QuotaWaiting reports whether the rate limiter is currently blocked on the
// hourly cap, and when the next slot frees.
func (c *Client) QuotaWaiting() (blocked bool, until time.Time) {
	if q, ok := c.rateLimiter.(interface {
		Waiting() (bool, time.Time)
	}); ok {
		return q.Waiting()
	}
	return false, time.Time{}
}

// flickrHint maps known Flickr error codes to actionable advice.
func flickrHint(code int, msg string) string {
	switch code {
	case 1:
		return "check the URL — the user, album or photo may not exist anymore"
	case 96, 97:
		return "your OAuth token has expired — run 'flickrdownloader auth' again"
	case 98, 99:
		return "authentication problem — run 'flickrdownloader auth' again"
	case 100:
		return "invalid API key — run 'flickrdownloader auth' again"
	case 108, 111, 114:
		return "the API call was refused (bad parameter?) — check the URL and retry"
	case 112:
		return "this API method isn't available for your key — try updating flickrdownloader"
	case 429:
		return "Flickr rate limit reached — wait a minute and retry"
	}
	if strings.Contains(strings.ToLower(msg), "rate") ||
		strings.Contains(strings.ToLower(msg), "limit") {
		return "Flickr rate limit reached — wait a minute and retry"
	}
	return ""
}

func flickrErr(code int, msg string) error {
	if h := flickrHint(code, msg); h != "" {
		return fmt.Errorf("flickr error [%d]: %s — hint: %s", code, msg, h)
	}
	return fmt.Errorf("flickr error [%d]: %s", code, msg)
}

// apiGet fetches method, transparently serving from the response cache when
// possible: --offline serves any cached entry regardless of age (or fails if
// there is none), --refresh always bypasses the cache, and the default
// serves a cache hit younger than cacheTTL(method). Concurrent identical
// requests collapse onto a single in-flight REST call (singleflight,
// ADR-022) in addition to the persisted cross-process lease pkg/cache
// applies at a higher level.
func (c *Client) apiGet(ctx context.Context, method string, params map[string]string) ([]byte, error) {
	defaults := map[string]string{
		"method":         method,
		"format":         "json",
		"nojsoncallback": "1",
	}
	for k, v := range defaults {
		params[k] = v
	}

	key := c.cacheKey(method, params)

	if c.cache != nil {
		if body, fetchedAt, found, err := c.cache.Get(ctx, key); err == nil && found {
			if c.offline || (!c.refresh && time.Since(fetchedAt) < c.cacheTTL(method)) {
				c.cacheHits.Add(1)
				return body, nil
			}
		}
	}

	if c.offline {
		return nil, fmt.Errorf("offline mode: no cached response for %s", method)
	}

	v, err, _ := c.group.Do(key, func() (any, error) {
		if err := c.rateLimiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("rate limiter: %w", err)
		}
		c.apiCalls.Add(1)
		body, err := c.signedGetWithRetry(ctx, params)
		if err != nil {
			return nil, err
		}
		if c.cache != nil {
			_ = c.cache.PutResponse(ctx, key, method, body, time.Now())
		}
		return body, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}

// signedGetWithRetry retries a Flickr-side rate limit (JSON stat=fail,
// code=429, or a rate/limit message) with capped exponential backoff,
// unbounded in count — only context cancellation stops it. This lets a
// long-running watch process wait through rate limiting instead of failing,
// even when the proactive hourly tracker didn't catch it (e.g. a key shared
// across machines).
func (c *Client) signedGetWithRetry(ctx context.Context, params map[string]string) ([]byte, error) {
	attempt := 0
	for {
		body, err := c.signedGet(c.APIKey, c.APISecret, c.AccessToken, c.AccessSecret, baseURL, params)
		if err != nil {
			return nil, err
		}
		if !isRateLimitResponse(body) {
			return body, nil
		}
		d := rateLimitBackoff(attempt, c.jitter())
		if err := c.sleep(ctx, d); err != nil {
			return nil, fmt.Errorf("rate limited, retry cancelled: %w", err)
		}
		attempt++
	}
}

// InvalidatePhotoMetadata drops any cached getSizes/getInfo response and
// normalized metadata for photoID. Callers use this after a CDN 403/404
// proves a cached original URL is stale, so the next request re-resolves it
// via a live flickr.photos.getSizes instead of retrying the same dead URL.
func (c *Client) InvalidatePhotoMetadata(ctx context.Context, photoID string) {
	if c.cache == nil {
		return
	}
	_ = c.cache.Delete(ctx, c.cacheKey(methodGetSizes, map[string]string{"photo_id": photoID}))
	_ = c.cache.Delete(ctx, c.cacheKey(methodGetPhotoInfo, map[string]string{"photo_id": photoID}))
	_ = c.cache.DeletePhotoMetadata(ctx, photoID)
}

func (c *Client) GetPhotosByUser(ctx context.Context, userID string, page int) (*PhotosResponse, error) {
	params := map[string]string{
		"user_id":  userID,
		"page":     fmt.Sprintf("%d", page),
		"per_page": fmt.Sprintf("%d", photosPerPage),
		"extras":   "original_format,url_o,media,o_dims",
	}

	data, err := c.apiGet(ctx, methodGetPhotosByUser, params)
	if err != nil {
		return nil, err
	}

	var resp PhotosResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal photos: %w", err)
	}

	if resp.Stat != "ok" {
		return nil, flickrErr(resp.Code, resp.Message)
	}
	if err := validatePhotos(resp.Photos.Photo); err != nil {
		return nil, fmt.Errorf("photostream page %d: %w", page, err)
	}

	return &resp, nil
}

func (c *Client) GetPhotosByPhotoset(ctx context.Context, photosetID string, page int) (*PhotoSetResponse, error) {
	params := map[string]string{
		"photoset_id": photosetID,
		"page":        fmt.Sprintf("%d", page),
		"per_page":    fmt.Sprintf("%d", photosPerPage),
		"extras":      "original_format,url_o,media,o_dims",
	}

	data, err := c.apiGet(ctx, methodGetPhotosByPhotoset, params)
	if err != nil {
		return nil, err
	}

	var resp PhotoSetResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal photoset: %w", err)
	}

	if resp.Stat != "ok" {
		return nil, flickrErr(resp.Code, resp.Message)
	}
	if err := validatePhotos(resp.Photoset.Photo); err != nil {
		return nil, fmt.Errorf("photoset %s page %d: %w", photosetID, page, err)
	}
	if resp.Photoset.Owner != "" && !ValidNSID(resp.Photoset.Owner) {
		return nil, fmt.Errorf("photoset %s: invalid owner nsid %q", photosetID, resp.Photoset.Owner)
	}

	return &resp, nil
}

// GetPhotosNotInSet returns one page of the authenticated user's photos that
// are not in any album, used to discover/verify the "Uncategorized" bucket
// without paginating the full photostream and diffing album membership.
func (c *Client) GetPhotosNotInSet(ctx context.Context, page int) (*PhotosResponse, error) {
	params := map[string]string{
		"page":     fmt.Sprintf("%d", page),
		"per_page": fmt.Sprintf("%d", photosPerPage),
		"extras":   "original_format,url_o,media,o_dims",
	}

	data, err := c.apiGet(ctx, methodGetPhotosNotInSet, params)
	if err != nil {
		return nil, err
	}

	var resp PhotosResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal photos not in set: %w", err)
	}

	if resp.Stat != "ok" {
		return nil, flickrErr(resp.Code, resp.Message)
	}
	if err := validatePhotos(resp.Photos.Photo); err != nil {
		return nil, fmt.Errorf("photos not in set page %d: %w", page, err)
	}

	return &resp, nil
}

func (c *Client) GetSizes(ctx context.Context, photoID string) (*SizesResponse, error) {
	params := map[string]string{
		"photo_id": photoID,
	}

	data, err := c.apiGet(ctx, methodGetSizes, params)
	if err != nil {
		return nil, err
	}

	var resp SizesResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal sizes: %w", err)
	}

	if resp.Stat != "ok" {
		return nil, flickrErr(resp.Code, resp.Message)
	}

	return &resp, nil
}

func (c *Client) GetPhotosets(ctx context.Context, userID string) ([]PhotoSetInfo, error) {
	var all []PhotoSetInfo
	page := 1
	for {
		params := map[string]string{
			"user_id":  userID,
			"page":     fmt.Sprintf("%d", page),
			"per_page": "500",
		}
		data, err := c.apiGet(ctx, methodGetPhotosets, params)
		if err != nil {
			return nil, fmt.Errorf("photosets.getList (page %d): %w — raw: %s", page, err, string(data))
		}
		var resp PhotoSetsListResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, fmt.Errorf("unmarshal photosets list (page %d): %w — raw: %s", page, err, string(data))
		}
		if resp.Stat != "ok" {
			return nil, fmt.Errorf("%w — raw: %s", flickrErr(resp.Code, resp.Message), string(data))
		}
		if all == nil {
			all = make([]PhotoSetInfo, 0, resp.Photosets.Total)
		}
		if err := validatePhotoSets(resp.Photosets.Photoset); err != nil {
			return nil, fmt.Errorf("photosets list page %d: %w", page, err)
		}
		all = append(all, resp.Photosets.Photoset...)
		if page >= int(resp.Photosets.Pages) {
			break
		}
		page++
	}
	return all, nil
}

func (c *Client) GetPhotosetInfo(ctx context.Context, photosetID string) (*PhotoSetInfo, error) {
	params := map[string]string{
		"photoset_id": photosetID,
	}
	data, err := c.apiGet(ctx, methodGetPhotosetInfo, params)
	if err != nil {
		return nil, err
	}
	var resp PhotoSetInfoResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal photoset info: %w", err)
	}
	if resp.Stat != "ok" {
		return nil, flickrErr(resp.Code, resp.Message)
	}
	if err := validatePhotoSetInfo(resp.Photoset); err != nil {
		return nil, fmt.Errorf("photoset info: %w", err)
	}
	return &resp.Photoset, nil
}

func (c *Client) LookupUser(ctx context.Context, flickrURL string) (string, error) {
	params := map[string]string{
		"url": flickrURL,
	}
	data, err := c.apiGet(ctx, "flickr.urls.lookupUser", params)
	if err != nil {
		return "", fmt.Errorf("lookupUser: %w — raw: %s", err, string(data))
	}
	var resp UserResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("unmarshal user: %w — raw: %s", err, string(data))
	}
	if resp.Stat != "ok" {
		return "", fmt.Errorf("%w — raw: %s", flickrErr(resp.Code, resp.Message), string(data))
	}
	if !ValidNSID(resp.User.ID) {
		return "", fmt.Errorf("lookupUser: invalid nsid %q", resp.User.ID)
	}
	return resp.User.ID, nil
}

func (c *Client) GetPhotoInfo(ctx context.Context, photoID string) (*PhotoInfoResponse, error) {
	params := map[string]string{
		"photo_id": photoID,
	}
	data, err := c.apiGet(ctx, methodGetPhotoInfo, params)
	if err != nil {
		return nil, err
	}
	var resp PhotoInfoResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal photo info: %w", err)
	}
	if resp.Stat != "ok" {
		return nil, flickrErr(resp.Code, resp.Message)
	}
	if err := validatePhotoInfo(resp); err != nil {
		return nil, fmt.Errorf("photo info: %w", err)
	}
	return &resp, nil
}
