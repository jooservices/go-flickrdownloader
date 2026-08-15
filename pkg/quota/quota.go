// Package quota tracks Flickr's REST API quota (3600 requests/hour) across
// separate runs of the tool. It persists a rolling window of request
// timestamps per API key in an append-only log, and blocks requests once the
// hourly cap is reached until the oldest entry expires.
//
// Concurrent processes sharing the same log path (e.g. a manual run and a
// cron job using the same API key) coordinate through an OS-level advisory
// lock (flock) on a sibling "<path>.lock" file: every accept/deny decision
// re-reads the shared log under that lock and, if the request is accepted,
// writes it through immediately. That keeps the hourly cap correct across
// all of them, at the cost of a small file read/write per request — cheap
// next to the ~1/sec pacing already applied. flock is held for the current
// process's lifetime only, so a crash or kill releases it automatically;
// there's no stale-lock case to clean up. A torn trailing line from a hard
// kill mid-write is simply ignored on the next read.
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

	"github.com/gofrs/flock"
	"golang.org/x/time/rate"
)

// DefaultHourlyLimit is Flickr's documented REST API cap.
const DefaultHourlyLimit = 3600

// DefaultInterval paces requests at ~1/sec, matching the pacing the API
// client previously applied internally.
const DefaultInterval = 1050 * time.Millisecond

// window is the rolling window of the hourly quota.
const window = time.Hour

// compactStaleThreshold rewrites the log at load time once this many lines
// were older than the window (crash residue / long-lived installs).
const compactStaleThreshold = 10000

// lockRetryDelay is how often TryLockContext polls for the interprocess
// lock while waiting for another process to release it.
const lockRetryDelay = 25 * time.Millisecond

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
	flock   *flock.Flock // interprocess lock guarding path; nil when path == ""
	entries []int64      // epoch seconds inside the rolling window
	flushed int          // number of entries already written to disk
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
	if path != "" {
		t.flock = flock.New(path + ".lock")
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
// until the oldest entry expires (rolling window). Each attempt re-reads the
// shared log under the interprocess lock (see the package doc), so the cap
// is enforced across every process sharing this path, not just this one.
// ctx cancellation aborts the wait. It is safe for concurrent use.
func (t *Tracker) Wait(ctx context.Context) error {
	if t.rate != nil {
		if err := t.rate.Wait(ctx); err != nil {
			return err
		}
	}

	for {
		now := t.Now()
		recorded, wait, err := t.tryRecord(ctx, now)
		if err != nil {
			return err
		}
		if recorded {
			return nil
		}
		if wait <= 0 {
			// The blocking entry expired between the check and now (clock
			// rounding) — retry immediately rather than sleeping for 0.
			continue
		}

		t.mu.Lock()
		t.waiting = true
		t.waitUntil = now.Add(wait)
		t.mu.Unlock()

		err = t.sleepWith(ctx, wait)
		t.mu.Lock()
		t.waiting = false
		t.mu.Unlock()
		if err != nil {
			return err
		}
	}
}

// tryRecord attempts, under the interprocess lock, to record one request
// against the shared quota log: it reloads the authoritative on-disk state,
// and either accepts the request (writing it through immediately) or reports
// how long until the oldest entry frees a slot.
func (t *Tracker) tryRecord(ctx context.Context, now time.Time) (recorded bool, wait time.Duration, err error) {
	if t.flock != nil {
		locked, lockErr := t.flock.TryLockContext(ctx, lockRetryDelay)
		if lockErr != nil {
			return false, 0, fmt.Errorf("lock quota log: %w", lockErr)
		}
		if !locked {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return false, 0, ctxErr
			}
			return false, 0, fmt.Errorf("lock quota log: timed out")
		}
		defer t.flock.Unlock()
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.path != "" {
		if err := t.rawLoadLocked(now); err != nil {
			return false, 0, err
		}
	} else {
		t.trimLocked(now)
	}

	if len(t.entries) < t.limit {
		t.entries = append(t.entries, now.Unix())
		t.waiting = false
		if err := t.flushLocked(); err != nil {
			return false, 0, err
		}
		return true, 0, nil
	}

	next := t.entries[0] + int64(window/time.Second) - now.Unix()
	if next < 0 {
		next = 0
	}
	return false, time.Duration(next) * time.Second, nil
}

// Flush writes any unflushed request timestamps to the log. Wait already
// writes through synchronously, so under normal operation there is nothing
// pending; this remains as a safety net for callers that mutate entries
// outside Wait.
func (t *Tracker) Flush() error {
	if t.flock != nil {
		locked, err := t.flock.TryLockContext(context.Background(), lockRetryDelay)
		if err != nil {
			return fmt.Errorf("lock quota log: %w", err)
		}
		if !locked {
			return fmt.Errorf("lock quota log: timed out")
		}
		defer t.flock.Unlock()
	}
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

// load performs the initial read of any persisted usage at startup, under
// the interprocess lock so it never races a sibling process's write.
func (t *Tracker) load() error {
	if t.path == "" {
		return nil
	}
	if t.flock != nil {
		locked, err := t.flock.TryLockContext(context.Background(), lockRetryDelay)
		if err != nil {
			return fmt.Errorf("lock quota log: %w", err)
		}
		if !locked {
			return fmt.Errorf("lock quota log: timed out")
		}
		defer t.flock.Unlock()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rawLoadLocked(t.Now())
}

// rawLoadLocked replaces the in-memory entry set with what's live on disk as
// of now, discarding anything outside the rolling window. The caller holds
// mu and, other than at construction, the interprocess flock — so this
// always reflects the combined usage of every process sharing the log.
func (t *Tracker) rawLoadLocked(now time.Time) error {
	f, err := os.Open(t.path)
	if err != nil {
		if os.IsNotExist(err) {
			t.entries = nil
			t.flushed = 0
			return nil
		}
		return fmt.Errorf("open quota log: %w", err)
	}
	defer f.Close()

	cutoff := now.Unix() - int64(window/time.Second)
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
