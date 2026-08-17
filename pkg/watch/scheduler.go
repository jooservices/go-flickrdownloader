package watch

import (
	"context"
	"fmt"
	"log"
	"time"
)

// DefaultPollInterval is used when a watchlist specifies none.
const DefaultPollInterval = 30 * time.Minute

// Runner executes one watchlist source and returns whether it succeeded.
// The scheduler never stops on a per-source error: it logs and continues.
type Runner func(ctx context.Context, src Source) error

// Scheduler owns the forever loop: reload the watchlist each cycle, run every
// source, sleep the poll interval (context-aware), and stop cleanly on
// SIGINT/SIGTERM via the context.
type Scheduler struct {
	Path         string // watchlist file path, reloaded each cycle
	PollInterval time.Duration
	RunSource    Runner
	Logf         func(format string, args ...any)
}

// Loop blocks until ctx is cancelled. Startup errors (missing watchlist,
// no sources) are fatal; per-source errors are logged and skipped.
func (s *Scheduler) Loop(ctx context.Context) error {
	if s.RunSource == nil {
		return fmt.Errorf("watch scheduler has no runner")
	}
	logf := s.Logf
	if logf == nil {
		logf = log.Printf
	}

	poll := s.PollInterval
	if poll <= 0 {
		poll = DefaultPollInterval
	}

	for {
		cfg, err := Load(s.Path)
		if err != nil {
			return fmt.Errorf("load watchlist: %w", err)
		}
		if len(cfg.Sources) == 0 {
			return fmt.Errorf("watchlist %s has no sources", s.Path)
		}
		if cfg.PollInterval > 0 {
			poll = cfg.PollInterval
		}
		if cfg.Workers > 0 || cfg.OutputDir != "" {
			for i := range cfg.Sources {
				if cfg.Sources[i].Workers == 0 {
					cfg.Sources[i].Workers = cfg.Workers
				}
				if cfg.Sources[i].OutputDir == "" {
					cfg.Sources[i].OutputDir = cfg.OutputDir
				}
			}
		}

		logf("watch: cycle start — %d source(s)", len(cfg.Sources))
		cycleStart := time.Now()
		for _, src := range cfg.Sources {
			if ctx.Err() != nil {
				break
			}
			started := time.Now()
			if err := s.RunSource(ctx, src); err != nil {
				logf("watch: source %s: %v", src.URL, err)
				continue
			}
			logf("watch: source %s done in %s", src.URL, time.Since(started).Round(time.Second))
		}

		elapsed := time.Since(cycleStart)
		wait := poll - elapsed
		if wait < 0 {
			wait = 0
		}
		if wait > 0 {
			logf("watch: cycle done in %s — next cycle in %s", elapsed.Round(time.Second), wait.Round(time.Second))
			if err := sleepCtx(ctx, wait); err != nil {
				return nil // graceful shutdown
			}
		}
	}
}

// sleepCtx waits d or returns immediately when ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
