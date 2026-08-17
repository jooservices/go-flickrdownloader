package watch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func writeWatchlist(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "watchlist.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSchedulerRunsEverySourceEachCycle(t *testing.T) {
	path := writeWatchlist(t, `
poll_interval: 1ms
sources:
  - url: https://www.flickr.com/photos/alice/
  - url: https://www.flickr.com/photos/bob/
`)

	var mu sync.Mutex
	var seen []string
	ctx, cancel := context.WithCancel(context.Background())

	s := &Scheduler{
		Path: path,
		Logf: func(string, ...any) {},
		RunSource: func(_ context.Context, src Source) error {
			mu.Lock()
			seen = append(seen, src.URL)
			mu.Unlock()
			if len(seen) >= 2 {
				cancel()
			}
			return nil
		},
	}

	err := s.Loop(ctx)
	if err != nil {
		t.Fatalf("Loop returned an error on graceful shutdown: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("ran %d source(s) in one cycle, want at least 2", len(seen))
	}
	if seen[0] != "https://www.flickr.com/photos/alice/" || seen[1] != "https://www.flickr.com/photos/bob/" {
		t.Fatalf("sources ran out of order: %v", seen)
	}
}

// TestSchedulerContinuesAfterPerSourceError is the core "never stops on a
// per-source error" contract from the plan: watch mode must not let one bad
// URL kill the whole daemon.
func TestSchedulerContinuesAfterPerSourceError(t *testing.T) {
	path := writeWatchlist(t, `
poll_interval: 1ms
sources:
  - url: https://www.flickr.com/photos/alice/
  - url: https://www.flickr.com/photos/bob/
`)

	var mu sync.Mutex
	ran := map[string]int{}
	ctx, cancel := context.WithCancel(context.Background())

	s := &Scheduler{
		Path: path,
		Logf: func(string, ...any) {},
		RunSource: func(_ context.Context, src Source) error {
			mu.Lock()
			ran[src.URL]++
			done := ran["https://www.flickr.com/photos/alice/"] > 0 && ran["https://www.flickr.com/photos/bob/"] > 0
			mu.Unlock()
			if done {
				cancel()
			}
			if src.URL == "https://www.flickr.com/photos/alice/" {
				return errors.New("simulated failure")
			}
			return nil
		},
	}

	if err := s.Loop(ctx); err != nil {
		t.Fatalf("Loop returned an error on graceful shutdown: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if ran["https://www.flickr.com/photos/bob/"] == 0 {
		t.Fatal("bob's source never ran — a failure on alice's source should not stop the cycle")
	}
}

func TestSchedulerRequiresRunner(t *testing.T) {
	s := &Scheduler{Path: writeWatchlist(t, "sources:\n  - url: https://www.flickr.com/photos/alice/\n")}
	if err := s.Loop(context.Background()); err == nil {
		t.Fatal("expected an error when RunSource is nil")
	}
}

func TestSchedulerFailsOnEmptyWatchlist(t *testing.T) {
	path := writeWatchlist(t, "sources: []\n")
	s := &Scheduler{Path: path, RunSource: func(context.Context, Source) error { return nil }}
	if err := s.Loop(context.Background()); err == nil {
		t.Fatal("expected an error for a watchlist with no sources")
	}
}

func TestSchedulerFailsOnMissingWatchlist(t *testing.T) {
	s := &Scheduler{
		Path:      filepath.Join(t.TempDir(), "missing.yaml"),
		RunSource: func(context.Context, Source) error { return nil },
	}
	if err := s.Loop(context.Background()); err == nil {
		t.Fatal("expected an error for a missing watchlist")
	}
}

// TestSchedulerStopsPromptlyOnContextCancelDuringSleep ensures shutdown
// during the inter-cycle sleep is immediate, not a wait for the full
// poll_interval — critical for a process meant to be stopped by
// SIGINT/SIGTERM at any point in its cycle.
func TestSchedulerStopsPromptlyOnContextCancelDuringSleep(t *testing.T) {
	path := writeWatchlist(t, `
poll_interval: 1h
sources:
  - url: https://www.flickr.com/photos/alice/
`)
	ctx, cancel := context.WithCancel(context.Background())
	s := &Scheduler{
		Path: path,
		Logf: func(string, ...any) {},
		RunSource: func(context.Context, Source) error {
			return nil
		},
	}

	done := make(chan error, 1)
	go func() { done <- s.Loop(ctx) }()

	time.Sleep(20 * time.Millisecond) // let it enter the poll_interval sleep
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Loop returned an error on cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Loop did not exit promptly after context cancellation during sleep")
	}
}
