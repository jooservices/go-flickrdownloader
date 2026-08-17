package api

import (
	"context"
	"encoding/json"
	"fmt"
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
