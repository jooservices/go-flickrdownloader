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
	Path         string // watchlist file path, reloaded each cycle when Load is nil
	Load         func() (*Config, error)
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
		cfg, err := s.load()
		if err != nil {
			return fmt.Errorf("load watchlist: %w", err)
		}
		sources := enabledSources(cfg.Sources)
		if len(sources) == 0 {
			label := s.Path
			if label == "" {
				label = "database"
			}
			return fmt.Errorf("watchlist %s has no sources", label)
		}
		if cfg.PollInterval > 0 {
			poll = cfg.PollInterval
		}
		if cfg.Workers > 0 || cfg.OutputDir != "" {
			for i := range sources {
				if sources[i].Workers == 0 {
					sources[i].Workers = cfg.Workers
				}
				if sources[i].OutputDir == "" {
					sources[i].OutputDir = cfg.OutputDir
				}
			}
		}

		logf("watch: cycle start — %d source(s)", len(sources))
		cycleStart := time.Now()
		for _, src := range sources {
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

func (s *Scheduler) load() (*Config, error) {
	if s.Load != nil {
		return s.Load()
	}
	return Load(s.Path)
}

func enabledSources(sources []Source) []Source {
	out := make([]Source, 0, len(sources))
	for _, src := range sources {
		if src.Enabled {
			out = append(out, src)
		}
	}
	return out
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
