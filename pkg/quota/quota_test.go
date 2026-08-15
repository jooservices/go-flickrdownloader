package quota

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func newFakeClock(base time.Time) *fakeClock { return &fakeClock{now: base} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Sleep records the requested delay and advances the fake clock by it.
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	c.mu.Unlock()
	c.advance(d)
	return nil
}

func newTestTracker(t *testing.T, limit int) (*Tracker, *fakeClock, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "quota.log")
	clock := newFakeClock(time.Now())
	tk, err := New(path, limit, 0) // interval 0 disables pacing
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tk.Now = clock.Now
	tk.Sleep = clock.Sleep
	return tk, clock, path
}

func TestWaitRecordsAndPersists(t *testing.T) {
	tk, _, path := newTestTracker(t, 100)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := tk.Wait(ctx); err != nil {
			t.Fatalf("Wait %d: %v", i, err)
		}
	}
	if got := tk.Snapshot().Used; got != 3 {
		t.Fatalf("used = %d, want 3", got)
	}
	if err := tk.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("log lines = %d, want 3", len(lines))
	}
}

func TestLoadRoundTrip(t *testing.T) {
	tk, _, path := newTestTracker(t, 100)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := tk.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := tk.Flush(); err != nil {
		t.Fatal(err)
	}

	tk2, err := New(path, 100, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := tk2.Snapshot().Used; got != 5 {
		t.Fatalf("reloaded used = %d, want 5", got)
	}
}

func TestBlockedUntilSlotFrees(t *testing.T) {
	tk, clock, _ := newTestTracker(t, 2)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := tk.Wait(ctx); err != nil {
			t.Fatal(err)
		}
		clock.advance(2 * time.Second) // keep entries at distinct timestamps
	}
	if err := tk.Wait(ctx); err != nil {
		t.Fatalf("Wait after cap: %v", err)
	}
	if len(clock.sleeps) != 1 {
		t.Fatalf("sleeps = %d, want 1", len(clock.sleeps))
	}
	if got := clock.sleeps[0]; got != time.Hour-4*time.Second {
		t.Fatalf("sleep = %v, want %v", got, time.Hour-4*time.Second)
	}
	// The oldest entry (t0) expired during the block; the rollover slot was
	// reused, so the window holds the two newer requests.
	if got := tk.Snapshot().Used; got != 2 {
		t.Fatalf("used = %d, want 2", got)
	}
}

func TestWaitingStateVisible(t *testing.T) {
	tk, clock, _ := newTestTracker(t, 2)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := tk.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}

	release := make(chan struct{})
	entered := make(chan struct{})
	tk.Sleep = func(ctx context.Context, d time.Duration) error {
		close(entered)
		select {
		case <-release:
			clock.advance(d)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	done := make(chan error, 1)
	go func() { done <- tk.Wait(ctx) }()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not block")
	}

	blocked, until := tk.Waiting()
	if !blocked {
		t.Fatal("expected Waiting()=true while blocked")
	}
	if !until.After(time.Now()) {
		t.Fatal("expected waitUntil in the future")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if blocked, _ := tk.Waiting(); blocked {
		t.Fatal("expected Waiting()=false after block cleared")
	}
}

func TestWaitCancel(t *testing.T) {
	tk, _, _ := newTestTracker(t, 1)
	ctx := context.Background()
	if err := tk.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	// Replace the fake sleep with one that honours cancellation, like the
	// production default.
	tk.Sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }

	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := tk.Wait(cctx)
	if err == nil {
		t.Fatal("expected error on cancelled wait")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancelled wait should return immediately")
	}
	if blocked, _ := tk.Waiting(); blocked {
		t.Fatal("waiting should be cleared after cancel")
	}
}

func TestExpiredEntriesTrimmedOnLoad(t *testing.T) {
	now := time.Now().Unix()
	data := fmt.Sprintf("%d\n%d\n%d\n", now-7200, now-3600, now-60)
	path := filepath.Join(t.TempDir(), "quota.log")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	tk, err := New(path, 100, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := tk.Snapshot().Used; got != 1 {
		t.Fatalf("used = %d, want 1", got)
	}
}

func TestCorruptLinesIgnored(t *testing.T) {
	now := time.Now().Unix()
	data := fmt.Sprintf("garbage\nnot-a-number\n%d\n\n", now-10)
	path := filepath.Join(t.TempDir(), "quota.log")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	tk, err := New(path, 100, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := tk.Snapshot().Used; got != 1 {
		t.Fatalf("used = %d, want 1", got)
	}
}

func TestSnapshotResetAt(t *testing.T) {
	tk, _, _ := newTestTracker(t, 100)
	ctx := context.Background()
	if err := tk.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tk.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	s := tk.Snapshot()
	want := tk.entries[0] + int64(window/time.Second)
	if s.ResetAt.Unix() != want {
		t.Fatalf("ResetAt = %d, want %d", s.ResetAt.Unix(), want)
	}
}

// TestWriteThroughPerRequest verifies each accepted request is persisted
// immediately (not batched), since sibling processes sharing the log must
// see it on their very next check.
func TestWriteThroughPerRequest(t *testing.T) {
	tk, _, path := newTestTracker(t, 1000)
	ctx := context.Background()
	if err := tk.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("log should be written through after a single request: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("log lines = %d, want 1", len(lines))
	}

	for i := 0; i < 59; i++ {
		if err := tk.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	tk2, err := New(path, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := tk2.Snapshot().Used; got != 60 {
		t.Fatalf("used = %d, want 60", got)
	}
}

// TestInterprocessSharedBudget verifies two Trackers pointed at the same log
// path share the hourly budget, catching the case where a second Tracker
// only sees its own in-memory usage instead of reloading combined usage from
// disk on every request.
func TestInterprocessSharedBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.log")
	clock := newFakeClock(time.Now())
	ctx := context.Background()

	tk1, err := New(path, 3, 0)
	if err != nil {
		t.Fatalf("New tk1: %v", err)
	}
	tk1.Now = clock.Now
	tk1.Sleep = clock.Sleep

	tk2, err := New(path, 3, 0)
	if err != nil {
		t.Fatalf("New tk2: %v", err)
	}
	tk2.Now = clock.Now
	tk2.Sleep = clock.Sleep

	// tk1 consumes 2 of the shared budget of 3.
	if err := tk1.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tk1.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	// tk2 should see only 1 slot left, not a fresh budget of 3.
	if err := tk2.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	release := make(chan struct{})
	entered := make(chan struct{})
	tk2.Sleep = func(ctx context.Context, d time.Duration) error {
		close(entered)
		select {
		case <-release:
			clock.advance(d)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	done := make(chan error, 1)
	go func() { done <- tk2.Wait(ctx) }()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("tk2's 4th request should have blocked on tk1's shared usage")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Wait: %v", err)
	}
}
