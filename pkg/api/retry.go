package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// retrySleep is the injectable wait used between 429 retries. Production
// binds a context-aware timer; tests substitute a fake to avoid wall-clock
// waits.
type retrySleep func(ctx context.Context, d time.Duration) error

// retryJitter returns the additive jitter fraction applied to each backoff.
type retryJitter func() float64

// isRateLimitResponse reports whether a raw REST envelope is a Flickr-side
// rate limit: stat=fail with code 429, or a message mentioning rate/limit.
// It is deliberately strict: only clearly rate-limit responses are retried,
// everything else surfaces immediately.
func isRateLimitResponse(body []byte) bool {
	var envelope struct {
		Stat    string `json:"stat"`
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return false
	}
	if envelope.Stat != "fail" {
		return false
	}
	if envelope.Code == 429 {
		return true
	}
	msg := strings.ToLower(envelope.Message)
	return strings.Contains(msg, "rate") || strings.Contains(msg, "limit")
}

// rateLimitBackoff returns the delay before the nth retry: 1s, 2s, 4s, ...
// capped at 60s, plus the jitter fraction. Retries are unbounded in count and
// only stop when the context is cancelled, so a long-running watch process
// waits through quota exhaustion instead of failing.
//
// attempt is clamped before the shift: left-shifting a 64-bit int by 64 or
// more overflows to 0, which would otherwise make the cap check below a
// no-op and drop backoff straight to 0s once a retry loop runs past ~64
// attempts — very reachable for a process designed to retry forever through
// a multi-hour outage, and exactly the wrong failure mode (a zero-delay hot
// loop hammering an already rate-limited API).
func rateLimitBackoff(attempt int, jitter float64) time.Duration {
	const maxShift = 6 // 1<<6s = 64s, already past the 60s cap
	if attempt > maxShift {
		attempt = maxShift
	}
	d := time.Duration(1<<uint(attempt)) * time.Second
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	return d + time.Duration(float64(d)*jitter)
}

// rateLimitedError marks a response that exhausted the retry loop without a
// resolution (normally only via context cancellation).
type rateLimitedError struct {
	code    int
	message string
}

func (e *rateLimitedError) Error() string {
	return fmt.Sprintf("flickr rate limit [%d]: %s", e.code, e.message)
}

// httpStatusError marks a Flickr REST response whose HTTP status itself (as
// opposed to the JSON body isRateLimitResponse inspects) signals the
// outcome: a transport-level 429/5xx from Flickr's edge, or an ordinary
// 4xx failure. signedGet returns this instead of a plain error so
// signedGetWithRetry can tell a transient status worth retrying from a
// fatal one, and honor a Retry-After header when the server sent one.
type httpStatusError struct {
	status        int
	body          []byte
	retryAfter    time.Duration
	hasRetryAfter bool
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("flickr api error %d: %s", e.status, string(e.body))
}

// isRetryableHTTPStatus reports whether an HTTP status returned by Flickr's
// REST endpoint itself signals a transient condition worth retrying: 429 Too
// Many Requests, 408 Request Timeout, or a 5xx server error. This is
// distinct from isRateLimitResponse, which classifies Flickr's usual
// convention of returning HTTP 200 with a JSON stat=fail/code=429 envelope —
// a real non-200 status (an edge/WAF throttle, or a transient outage) never
// reaches that check because the body isn't Flickr's REST envelope at all.
func isRetryableHTTPStatus(status int) bool {
	return status == http.StatusTooManyRequests ||
		status == http.StatusRequestTimeout ||
		(status >= http.StatusInternalServerError && status < 600)
}

// maxHTTPRetryAfter caps a parsed Retry-After header, matching the cap
// rateLimitBackoff already applies to its own computed backoff.
const maxHTTPRetryAfter = 60 * time.Second

// parseRetryAfter parses a Retry-After header value — delta-seconds or an
// HTTP-date — capped at maxHTTPRetryAfter. Duplicated from pkg/download's
// copy rather than shared: pkg/api sits below pkg/download in the module's
// layering (cmd -> pkg/{api,config,download,...}), so api must not import
// download.
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(v, 10, 64); err == nil && seconds >= 0 {
		d := time.Duration(seconds) * time.Second
		if d > maxHTTPRetryAfter {
			return maxHTTPRetryAfter, true
		}
		return d, true
	}
	when, err := http.ParseTime(v)
	if err != nil {
		return 0, false
	}
	delay := when.Sub(now)
	if delay < 0 {
		return 0, false
	}
	if delay > maxHTTPRetryAfter {
		return maxHTTPRetryAfter, true
	}
	return delay, true
}

// defaultRetrySleep is the production retrySleep: a context-aware timer.
func defaultRetrySleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
