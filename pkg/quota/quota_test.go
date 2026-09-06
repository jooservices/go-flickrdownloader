package quota

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
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

func writeQuotaLog(t *testing.T, path string, ts ...int64) {
	t.Helper()
	var b strings.Builder
	for _, v := range ts {
		fmt.Fprintf(&b, "%d\n", v)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func quotaLogLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func startTracker(t *testing.T, path string, limit, compactAfter int, clock *fakeClock) *Tracker {
	t.Helper()
	tk := newTracker(path, limit, 0)
	tk.compactAfter = compactAfter
	if clock != nil {
		tk.Now = clock.Now
		tk.Sleep = clock.Sleep
	}
	if err := tk.load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	return tk
}

func staleAndLive(now time.Time, stale, live int) []int64 {
	base := now.Unix()
	out := make([]int64, 0, stale+live)
	for i := 0; i < stale; i++ {
		out = append(out, base-7200)
	}
	for i := 0; i < live; i++ {
		out = append(out, base-int64(60+i))
	}
	return out
}

func TestCompactRewritesLiveEntriesOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.log")
	clock := newFakeClock(time.Now())
	writeQuotaLog(t, path, staleAndLive(clock.Now(), 5, 2)...)

	tk := startTracker(t, path, 100, 3, clock)
	if got := tk.Snapshot().Used; got != 2 {
		t.Fatalf("used = %d, want 2", got)
	}
	lines := quotaLogLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("log lines = %d, want 2 (stale residue should be gone): %v", len(lines), lines)
	}
	cutoff := clock.Now().Unix() - int64(window/time.Second)
	for _, line := range lines {
		var ts int64
		if _, err := fmt.Sscan(line, &ts); err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		if ts <= cutoff {
			t.Fatalf("stale timestamp %d left in compacted log", ts)
		}
	}
}

func TestCompactThenWaitAppendsToRewrittenLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.log")
	clock := newFakeClock(time.Now())
	writeQuotaLog(t, path, staleAndLive(clock.Now(), 4, 1)...)

	tk := startTracker(t, path, 100, 2, clock)
	if err := tk.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	lines := quotaLogLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("log lines = %d, want 2 (1 live + 1 new)", len(lines))
	}
	if got := tk.Snapshot().Used; got != 2 {
		t.Fatalf("used = %d, want 2", got)
	}
}

func TestCompactVisibleToSiblingTracker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.log")
	clock := newFakeClock(time.Now())
	writeQuotaLog(t, path, staleAndLive(clock.Now(), 6, 3)...)

	_ = startTracker(t, path, 100, 4, clock)

	tk2, err := New(path, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := tk2.Snapshot().Used; got != 3 {
		t.Fatalf("sibling used = %d, want 3", got)
	}
	if n := len(quotaLogLines(t, path)); n != 3 {
		t.Fatalf("sibling saw %d lines, want 3", n)
	}
}

func TestCompactFailureDoesNotBlockWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.log")
	clock := newFakeClock(time.Now())
	writeQuotaLog(t, path, staleAndLive(clock.Now(), 4, 1)...)

	tk := newTracker(path, 100, 0)
	tk.compactAfter = 2
	tk.Now = clock.Now
	tk.replaceFile = func(tmp, dest string) error {
		os.Remove(tmp)
		return errors.New("Access is denied.")
	}
	if err := tk.load(); err != nil {
		t.Fatal(err)
	}
	if err := tk.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after failed compact: %v", err)
	}
	if got := tk.Snapshot().Used; got != 2 {
		t.Fatalf("used = %d, want 2", got)
	}
}

func TestCompactNotRetriedDuringBackoff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.log")
	clock := newFakeClock(time.Now())
	writeQuotaLog(t, path, staleAndLive(clock.Now(), 4, 1)...)

	calls := 0
	tk := newTracker(path, 100, 0)
	tk.compactAfter = 2
	tk.Now = clock.Now
	tk.Sleep = clock.Sleep
	tk.replaceFile = func(tmp, dest string) error {
		calls++
		os.Remove(tmp)
		return errors.New("Access is denied.")
	}
	if err := tk.load(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("compact calls after load = %d, want 1", calls)
	}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := tk.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("compact retried during backoff: %d calls", calls)
	}
}

func TestCompactRetriesAfterBackoff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.log")
	clock := newFakeClock(time.Now())
	writeQuotaLog(t, path, staleAndLive(clock.Now(), 4, 1)...)

	calls := 0
	tk := newTracker(path, 100, 0)
	tk.compactAfter = 2
	tk.Now = clock.Now
	tk.Sleep = clock.Sleep
	tk.replaceFile = func(tmp, dest string) error {
		calls++
		if calls == 1 {
			os.Remove(tmp)
			return errors.New("Access is denied.")
		}
		return os.Rename(tmp, dest)
	}
	if err := tk.load(); err != nil {
		t.Fatal(err)
	}
	if err := tk.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("compact calls before backoff elapsed = %d, want 1", calls)
	}

	clock.advance(compactRetryAfter)
	if err := tk.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("compact calls after backoff = %d, want 2", calls)
	}
	lines := quotaLogLines(t, path)
	cutoff := clock.Now().Unix() - int64(window/time.Second)
	for _, line := range lines {
		var ts int64
		if _, err := fmt.Sscan(line, &ts); err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		if ts <= cutoff {
			t.Fatalf("stale timestamp %d left after successful retry", ts)
		}
	}
}

func TestCompactFailureLoggedOnce(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	path := filepath.Join(t.TempDir(), "quota.log")
	clock := newFakeClock(time.Now())
	writeQuotaLog(t, path, staleAndLive(clock.Now(), 4, 1)...)

	tk := newTracker(path, 100, 0)
	tk.compactAfter = 2
	tk.Now = clock.Now
	tk.Sleep = clock.Sleep
	tk.replaceFile = func(tmp, dest string) error {
		os.Remove(tmp)
		return errors.New("Access is denied.")
	}
	if err := tk.load(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := tk.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	clock.advance(compactRetryAfter)
	if err := tk.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	got := buf.String()
	if n := strings.Count(got, "quota: compact"); n != 1 {
		t.Fatalf("compact log lines = %d, want 1:\n%s", n, got)
	}
	if !strings.Contains(got, "will retry later") {
		t.Fatalf("missing backoff hint in log: %q", got)
	}
}

func TestReplaceQuotaLogOverwritesDest(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "quota.log")
	tmp := dest + ".tmp"
	if err := os.WriteFile(dest, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceQuotaLog(tmp, dest); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new\n" {
		t.Fatalf("dest = %q, want %q", data, "new\n")
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("tmp should be gone after replace")
	}
}

func TestReplaceFileRetriesThenRemoveFallback(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "quota.log")
	tmp := dest + ".tmp"
	if err := os.WriteFile(dest, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	renames := 0
	err := replaceFileWith(tmp, dest,
		func(oldpath, newpath string) error {
			renames++
			if renames <= replaceAttempts {
				return errors.New("Access is denied.")
			}
			return os.Rename(oldpath, newpath)
		},
		os.Remove,
		func(time.Duration) {},
	)
	if err != nil {
		t.Fatal(err)
	}
	if renames != replaceAttempts+1 {
		t.Fatalf("renames = %d, want %d (retries + fallback)", renames, replaceAttempts+1)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new\n" {
		t.Fatalf("dest = %q, want %q", data, "new\n")
	}
}

func TestReplaceFileGivesUpWhenDestStuck(t *testing.T) {
	err := replaceFileWith("tmp", "dest",
		func(string, string) error { return errors.New("Access is denied.") },
		func(string) error { return errors.New("busy") },
		func(time.Duration) {},
	)
	if err == nil || err.Error() != "Access is denied." {
		t.Fatalf("err = %v, want original rename error", err)
	}
}

func TestReplaceFileLeavesDestWhenOpen(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("rename-over-open-file only fails on Windows")
	}
	dir := t.TempDir()
	dest := filepath.Join(dir, "quota.log")
	tmp := dest + ".tmp"
	if err := os.WriteFile(dest, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	err = replaceFileWith(tmp, dest, os.Rename, os.Remove, func(time.Duration) {})
	if err == nil {
		t.Fatal("expected replace to fail while dest is open")
	}
	data, readErr := os.ReadFile(dest)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != "old\n" {
		t.Fatalf("dest mutated while open: %q", data)
	}
}

func TestUsageReflectsSnapshot(t *testing.T) {
	tk, _, _ := newTestTracker(t, 2)
	if used, limit, _ := tk.Usage(); used != 0 || limit != 2 {
		t.Fatalf("Usage = %d/%d, want 0/2", used, limit)
	}
	if err := tk.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if used, limit, _ := tk.Usage(); used != 1 || limit != 2 {
		t.Fatalf("Usage = %d/%d, want 1/2", used, limit)
	}
}

// TestTrimLockedDropsExpiredEntriesInMemoryOnly covers trimLocked, the
// non-persisted counterpart to rawLoadLocked's file-backed trimming: a
// Tracker built with an empty path (in-memory only) still has to expire old
// entries out of its rolling window, just without a log to re-read.
func TestTrimLockedDropsExpiredEntriesInMemoryOnly(t *testing.T) {
	clock := newFakeClock(time.Now())
	tk, err := New("", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	tk.Now = clock.Now
	tk.Sleep = clock.Sleep
	ctx := context.Background()
	if err := tk.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tk.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	// The cap (2) is now reached. Advance the clock past the rolling
	// window so trimLocked drops both entries on the next attempt, rather
	// than blocking.
	clock.advance(time.Hour + time.Second)
	if err := tk.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	used, _, _ := tk.Usage()
	if used != 1 {
		t.Fatalf("used = %d, want 1 (two expired entries trimmed, one fresh recorded)", used)
	}
}

func TestSleepWithUsesRealTimerWhenNoSleepInjected(t *testing.T) {
	tk := &Tracker{Now: time.Now}
	start := time.Now()
	if err := tk.sleepWith(context.Background(), 10*time.Millisecond); err != nil {
		t.Fatalf("sleepWith: %v", err)
	}
	if time.Since(start) < 10*time.Millisecond {
		t.Fatal("sleepWith returned before the requested duration elapsed")
	}
}

func TestSleepWithRealTimerRespectsCancellation(t *testing.T) {
	tk := &Tracker{Now: time.Now}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tk.sleepWith(ctx, time.Hour); err == nil {
		t.Fatal("expected an error when the context is already cancelled")
	}
}

func TestFlushLockedFailsWhenDirMissing(t *testing.T) {
	tk := newTracker(filepath.Join(t.TempDir(), "missing-dir", "quota.log"), 0, 0)
	tk.entries = []int64{time.Now().Unix()}
	if err := tk.flushLocked(); err == nil {
		t.Fatal("expected an error when the log's directory doesn't exist")
	}
}

func TestCompactLockedFailsWhenDirMissing(t *testing.T) {
	tk := newTracker(filepath.Join(t.TempDir(), "missing-dir", "quota.log"), 0, 0)
	tk.entries = []int64{time.Now().Unix()}
	if err := tk.compactLocked(); err == nil {
		t.Fatal("expected an error when the log's directory doesn't exist")
	}
}

func TestNewFailsWhenLogUnreadable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: file permissions don't block reads")
	}
	path := filepath.Join(t.TempDir(), "quota.log")
	if err := os.WriteFile(path, []byte("123\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path, 0, 0); err == nil {
		t.Fatal("expected an error when the log file is unreadable")
	}
}

func TestCompactLockedFailsWhenReplaceFails(t *testing.T) {
	tk := newTracker(filepath.Join(t.TempDir(), "quota.log"), 0, 0)
	tk.entries = []int64{time.Now().Unix()}
	tk.replaceFile = func(tmp, dest string) error { return fmt.Errorf("boom") }
	if err := tk.compactLocked(); err == nil {
		t.Fatal("expected an error when replaceFile fails")
	}
}

func TestFlushPropagatesFlushLockedFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "quota.log")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	tk, err := New(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	tk.entries = append(tk.entries, time.Now().Unix()) // unflushed
	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if err := tk.Flush(); err == nil {
		t.Fatal("expected Flush to propagate a flushLocked failure")
	}
}
