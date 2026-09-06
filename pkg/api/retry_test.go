package api

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestIsRetryableHTTPStatus(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{200, false},
		{400, false},
		{401, false},
		{403, false},
		{404, false},
		{408, true},
		{429, true},
		{500, true},
		{502, true},
		{503, true},
		{504, true},
		{599, true},
		{600, false},
	}
	for _, c := range cases {
		if got := isRetryableHTTPStatus(c.status); got != c.want {
			t.Errorf("isRetryableHTTPStatus(%d) = %v, want %v", c.status, got, c.want)
		}
	}
}

func TestAPIParseRetryAfter(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		value string
		want  time.Duration
		ok    bool
	}{
		{"empty", "", 0, false},
		{"seconds", "5", 5 * time.Second, true},
		{"zero seconds", "0", 0, true},
		{"negative rejected", "-1", 0, false},
		{"leading plus rejected", "+30", 0, false},
		{"over cap clamps", "3600", maxHTTPRetryAfter, true},
		// Regression test: seconds this large overflows int64 when naively
		// converted to a time.Duration (multiplying by 1e9) before the cap
		// check runs, wrapping negative — time.NewTimer(negative) fires
		// immediately, turning an already-throttled endpoint into a
		// zero-delay hot retry loop instead of respecting the 60s cap.
		{"huge value overflow clamps instead of going negative", "18446744070", maxHTTPRetryAfter, true},
		{"max int64 clamps instead of overflowing", "9223372036854775807", maxHTTPRetryAfter, true},
		{"garbage rejected", "not-a-number", 0, false},
		{"http date in future", now.Add(10 * time.Second).Format(http.TimeFormat), 10 * time.Second, true},
		{"http date in past rejected", now.Add(-10 * time.Second).Format(http.TimeFormat), 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseRetryAfter(c.value, now)
			if ok != c.ok {
				t.Fatalf("parseRetryAfter(%q) ok = %v, want %v", c.value, ok, c.ok)
			}
			if ok && got != c.want {
				t.Fatalf("parseRetryAfter(%q) = %v, want %v", c.value, got, c.want)
			}
		})
	}
}

// TestRateLimitBackoffCapsEvenAtHighAttempts is a regression test for a
// backoff overflow: computing 1<<attempt before checking the 60s cap let a
// long-running retry loop (attempt >= 64) overflow an int64 shift to 0,
// silently dropping backoff from 60s to 0s instead of staying capped — a
// serious problem for watch mode, which retries 429s unbounded in count.
func TestRateLimitBackoffCapsEvenAtHighAttempts(t *testing.T) {
	for _, attempt := range []int{0, 1, 6, 7, 10, 64, 65, 1000} {
		d := rateLimitBackoff(attempt, 0)
		if d <= 0 {
			t.Fatalf("attempt %d: backoff = %v, want > 0 (capped at 60s, never 0)", attempt, d)
		}
		if d > 60*time.Second {
			t.Fatalf("attempt %d: backoff = %v, want <= 60s", attempt, d)
		}
	}
}

func TestRateLimitBackoffGrowsThenCaps(t *testing.T) {
	got := rateLimitBackoff(0, 0)
	if got != 1*time.Second {
		t.Fatalf("attempt 0 backoff = %v, want 1s", got)
	}
	got = rateLimitBackoff(2, 0)
	if got != 4*time.Second {
		t.Fatalf("attempt 2 backoff = %v, want 4s", got)
	}
	got = rateLimitBackoff(10, 0)
	if got != 60*time.Second {
		t.Fatalf("attempt 10 backoff = %v, want capped 60s", got)
	}
}

func TestRateLimitBackoffAppliesJitter(t *testing.T) {
	base := rateLimitBackoff(0, 0)
	jittered := rateLimitBackoff(0, 0.5)
	if jittered <= base {
		t.Fatalf("jittered backoff %v should exceed base %v", jittered, base)
	}
}

func TestIsRateLimitResponse(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"code 429", `{"stat":"fail","code":429,"message":"rate limit"}`, true},
		{"message mentions limit", `{"stat":"fail","code":1,"message":"Too many requests, rate exceeded"}`, true},
		{"unrelated failure", `{"stat":"fail","code":1,"message":"Photo not found"}`, false},
		{"success", `{"stat":"ok"}`, false},
		{"garbage", `not json`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isRateLimitResponse([]byte(c.body)); got != c.want {
				t.Fatalf("isRateLimitResponse(%q) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}

func TestDefaultRetrySleepCompletesNormally(t *testing.T) {
	start := time.Now()
	if err := defaultRetrySleep(context.Background(), 10*time.Millisecond); err != nil {
		t.Fatalf("defaultRetrySleep: %v", err)
	}
	if time.Since(start) < 10*time.Millisecond {
		t.Fatal("defaultRetrySleep returned before the requested duration elapsed")
	}
}

func TestDefaultRetrySleepStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := defaultRetrySleep(ctx, time.Hour); err == nil {
		t.Fatal("expected an error when the context is already cancelled")
	}
}
