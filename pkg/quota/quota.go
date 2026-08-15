// Package quota tracks Flickr's REST API quota (3600 requests/hour) across
// separate runs of the tool. It persists a rolling window of request
// timestamps per API key in an append-only log, and blocks requests once the
// hourly cap is reached until the oldest entry expires.
//
// The log is written with O_APPEND in batches (never rewritten during normal
// operation), so a hard kill loses at most flushBatch unrecorded requests and
// a torn trailing line is simply ignored on the next load.
package quota

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// DefaultHourlyLimit is Flickr's documented REST API cap.
const DefaultHourlyLimit = 3600

// DefaultInterval paces requests at ~1/sec, matching the pacing the API
// client previously applied internally.
const DefaultInterval = 1050 * time.Millisecond

// window is the rolling window of the hourly quota.
const window = time.Hour

// flushBatch flushes the quota log at most once per this many requests, so a
// hard kill loses at most 60 unrecorded requests (~1.7% of the hourly cap).
const flushBatch = 60

// compactStaleThreshold rewrites the log at load time once this many lines
// were older than the window (crash residue / long-lived installs).
const compactStaleThreshold = 10000

// Snapshot is a point-in-time view of quota usage for the UI.
type Snapshot struct {
	Used      int
	Limit     int
	ResetAt   time.Time // when the oldest live entry expires; zero when unused
	Waiting   bool      // a request is currently blocked on the hourly cap
	WaitUntil time.Time // when the current block ends (valid while Waiting)
}

// Tracker gates REST API requests against both a per-request interval and
// Flickr's hourly cap, persisting usage in a per-API-key log file so separate
// runs share the same hourly budget.
type Tracker struct {
	mu      sync.Mutex
	path    string
	entries []int64 // epoch seconds inside the rolling window
	flushed int     // number of entries already written to disk
	limit   int
	rate    *rate.Limiter // per-request pacing; nil disables it

	waiting   bool
	waitUntil time.Time

	Now   func() time.Time // injectable for tests
	Sleep func(context.Context, time.Duration) error
}

// New loads any persisted usage for the given log path and returns a Tracker.
// hourlyLimit <= 0 uses DefaultHourlyLimit; interval <= 0 disables the
// per-request pacing.
func New(path string, hourlyLimit int, interval time.Duration) (*Tracker, error) {
	if hourlyLimit <= 0 {
		hourlyLimit = DefaultHourlyLimit
	}
	t := &Tracker{
		path:  path,
		limit: hourlyLimit,
		Now:   time.Now,
	}
	if interval > 0 {
		t.rate = rate.NewLimiter(rate.Every(interval), 1)
	}
	if err := t.load(); err != nil {
		return nil, err
	}
	return t, nil
}

// Wait blocks until a request is allowed by both the per-request interval and
// the hourly cap, then records the request. When the cap is reached it waits
// until the oldest entry expires (rolling window). ctx cancellation aborts
// the wait. It is safe for concurrent use.
func (t *Tracker) Wait(ctx context.Context) error {
	if t.rate != nil {
		if err := t.rate.Wait(ctx); err != nil {
			return err
		}
	}

	for {
		now := t.Now()
		t.mu.Lock()
		t.trimLocked(now)
		if len(t.entries) < t.limit {
			ts := now.Unix()
			t.entries = append(t.entries, ts)
			t.waiting = false
			err := t.maybeFlushLocked()
			t.mu.Unlock()
			return err
		}

		next := t.entries[0] + int64(window/time.Second) - now.Unix()
		if next <= 0 {
			t.mu.Unlock()
			continue
		}
		t.waiting = true
		t.waitUntil = now.Add(time.Duration(next) * time.Second)
		t.mu.Unlock()

		err := t.sleepWith(ctx, time.Duration(next)*time.Second)
		t.mu.Lock()
		t.waiting = false
		t.mu.Unlock()
		if err != nil {
			return err
		}
	}
}

// Flush writes any unflushed request timestamps to the log. Call it on
// graceful exit (e.g. defer) so a hard kill loses at most flushBatch entries.
func (t *Tracker) Flush() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.flushLocked()
}

// Snapshot returns the current usage state.
func (t *Tracker) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := Snapshot{
		Used:      len(t.entries),
		Limit:     t.limit,
		Waiting:   t.waiting,
		WaitUntil: t.waitUntil,
	}
	if len(t.entries) > 0 {
		s.ResetAt = time.Unix(t.entries[0]+int64(window/time.Second), 0)
	}
	return s
}

// Usage is the duck-typed view used by the API client's UI helpers.
func (t *Tracker) Usage() (used, limit int, resetAt time.Time) {
	s := t.Snapshot()
	return s.Used, s.Limit, s.ResetAt
}

// Waiting reports whether a request is currently blocked on the hourly cap,
// and when the next slot frees.
func (t *Tracker) Waiting() (blocked bool, until time.Time) {
	s := t.Snapshot()
	return s.Waiting, s.WaitUntil
}

func (t *Tracker) load() error {
	if t.path == "" {
		return nil
	}
	f, err := os.Open(t.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open quota log: %w", err)
	}
	defer f.Close()

	cutoff := t.Now().Unix() - int64(window/time.Second)
	var kept []int64
	var stale int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		ts, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			continue // torn or corrupt line — ignore
		}
		if ts <= cutoff {
			stale++
		} else {
			kept = append(kept, ts)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read quota log: %w", err)
	}
	t.entries = kept
	t.flushed = len(kept)
	if stale > compactStaleThreshold {
		if err := t.compactLocked(); err != nil {
			log.Printf("quota: compact %s: %v", t.path, err)
		}
	}
	return nil
}

// trimLocked drops entries older than the rolling window, adjusting the flush
// cursor so already-persisted lines are never re-written.
func (t *Tracker) trimLocked(now time.Time) {
	cutoff := now.Unix() - int64(window/time.Second)
	keep := 0
	for keep < len(t.entries) && t.entries[keep] <= cutoff {
		keep++
	}
	if keep > 0 {
		t.entries = t.entries[keep:]
		if t.flushed <= keep {
			t.flushed = 0
		} else {
			t.flushed -= keep
		}
	}
}

func (t *Tracker) maybeFlushLocked() error {
	if len(t.entries)-t.flushed < flushBatch {
		return nil
	}
	return t.flushLocked()
}

// flushLocked appends every entry after the flush cursor. The caller holds mu.
func (t *Tracker) flushLocked() error {
	if t.path == "" || len(t.entries) == t.flushed {
		return nil
	}
	f, err := os.OpenFile(t.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open quota log: %w", err)
	}
	w := bufio.NewWriter(f)
	for i := t.flushed; i < len(t.entries); i++ {
		if _, err := fmt.Fprintf(w, "%d\n", t.entries[i]); err != nil {
			f.Close()
			return fmt.Errorf("write quota log: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return fmt.Errorf("flush quota log: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close quota log: %w", err)
	}
	t.flushed = len(t.entries)
	return nil
}

// compactLocked rewrites the log with only live entries via a temp file +
// rename, so the rewrite is atomic. The caller holds mu.
func (t *Tracker) compactLocked() error {
	if t.path == "" {
		return nil
	}
	tmp := t.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create temp quota log: %w", err)
	}
	abort := func() {
		f.Close()
		os.Remove(tmp)
	}
	w := bufio.NewWriter(f)
	for _, ts := range t.entries {
		if _, err := fmt.Fprintf(w, "%d\n", ts); err != nil {
			abort()
			return fmt.Errorf("write temp quota log: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		abort()
		return fmt.Errorf("flush temp quota log: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close temp quota log: %w", err)
	}
	if err := os.Rename(tmp, t.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace quota log: %w", err)
	}
	return nil
}

func (t *Tracker) sleepWith(ctx context.Context, d time.Duration) error {
	if t.Sleep != nil {
		return t.Sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
