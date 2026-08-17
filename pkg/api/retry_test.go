package api

import (
	"testing"
	"time"
)

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
