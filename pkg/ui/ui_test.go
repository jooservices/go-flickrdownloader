package ui

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestProgressConcurrentAccess exercises AddSuccess/AddSkipped/AddLinked/
// AddFailure from worker goroutines concurrently with Render (which reads
// completed() via percent()/eta()), the same pattern the downloader uses
// between its worker pool and the 150ms renderer tick. Run with -race: a
// plain (non-atomic) read of the Stats fields here would be flagged as a
// data race against the atomic writers.
func TestProgressConcurrentAccess(t *testing.T) {
	const workers = 20
	const perWorker = 200

	p := NewProgress(workers * perWorker)

	stop := make(chan struct{})
	rendererDone := make(chan struct{})
	go func() {
		defer close(rendererDone)
		for {
			select {
			case <-stop:
				return
			default:
				_ = p.Render()
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				switch j % 4 {
				case 0:
					p.AddSuccess()
				case 1:
					p.AddSkipped()
				case 2:
					p.AddLinked()
				case 3:
					p.AddFailure("id", "url", "err")
				}
				p.AddBytes(1)
			}
		}(i)
	}
	wg.Wait()
	close(stop)
	<-rendererDone

	want := int64(workers * perWorker / 4)
	if got := p.Stats().Success; got != want {
		t.Fatalf("Success = %d, want %d", got, want)
	}
	if got := p.completed(); got != int64(workers*perWorker) {
		t.Fatalf("completed = %d, want %d", got, workers*perWorker)
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Second, "5s"},
		{90 * time.Second, "1m30s"},
		{2*time.Hour + 5*time.Minute, "2h05m"},
	}
	for _, c := range cases {
		if got := FormatDuration(c.d); got != c.want {
			t.Errorf("FormatDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{500, "500 B"},
		{2048, "2.0 KB"},
		{5 * 1024 * 1024, "5.0 MB"},
		{3 * 1024 * 1024 * 1024, "3.0 GB"},
	}
	for _, c := range cases {
		if got := FormatBytes(c.n); got != c.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestBordered(t *testing.T) {
	got := Bordered("hello", ColorGreen)
	if !strings.Contains(got, "hello") || !strings.Contains(got, "╔") || !strings.Contains(got, "╚") {
		t.Fatalf("Bordered = %q, want a bordered box containing the text", got)
	}
}

func TestCompletionLine(t *testing.T) {
	singular := CompletionLine(AlbumStat{Success: 1})
	if !strings.Contains(singular, "1 photo") || strings.Contains(singular, "1 photos") {
		t.Fatalf("CompletionLine(1) = %q, want singular %q", singular, "photo")
	}
	withFailures := CompletionLine(AlbumStat{Success: 2, Failed: 3, Bytes: 1024})
	if !strings.Contains(withFailures, "2 photos") || !strings.Contains(withFailures, "3 failed") || !strings.Contains(withFailures, "1.0 KB") {
		t.Fatalf("CompletionLine = %q, want it to mention count, failures, and size", withFailures)
	}
}

func TestBreakdownEmptyStats(t *testing.T) {
	if got := Breakdown(nil); got != "" {
		t.Fatalf("Breakdown(nil) = %q, want empty", got)
	}
}

func TestBreakdownRendersRowsAndTotals(t *testing.T) {
	got := Breakdown([]AlbumStat{
		{Name: "Album A", Success: 3, Skipped: 1, Bytes: 2048},
		{Name: "Album B", Failed: 2},
	})
	if !strings.Contains(got, "Album A") || !strings.Contains(got, "Album B") {
		t.Fatalf("Breakdown = %q, want both album names", got)
	}
	if !strings.Contains(got, "Total") {
		t.Fatalf("Breakdown = %q, want a totals row", got)
	}
}

func TestSummaryIncludesFailuresAndStats(t *testing.T) {
	p := NewProgress(5)
	p.AddSuccess()
	p.AddSuccess()
	p.AddSkipped()
	p.AddFailure("123", "https://example.invalid/123.jpg", "timeout")
	p.AddFailure("124", "https://example.invalid/124.jpg", "timeout")

	got := p.Summary()
	if !strings.Contains(got, "Download Complete") {
		t.Fatalf("Summary = %q, want the completion banner", got)
	}
	if !strings.Contains(got, "2×") || !strings.Contains(got, "timeout") {
		t.Fatalf("Summary = %q, want the grouped failure count and reason", got)
	}
	if !strings.Contains(got, "123") || !strings.Contains(got, "124") {
		t.Fatalf("Summary = %q, want both failed photo IDs listed", got)
	}
}

func TestSummaryGroupsManyFailedIDs(t *testing.T) {
	p := NewProgress(20)
	for i := 0; i < 10; i++ {
		p.AddFailure(fmt.Sprintf("%d", i), "https://example.invalid/x.jpg", "timeout")
	}
	got := p.Summary()
	if !strings.Contains(got, "more") {
		t.Fatalf("Summary = %q, want the ID list truncated with a '+N more' suffix past 8 IDs", got)
	}
}

func TestFormatSpeed(t *testing.T) {
	if got, want := formatSpeed(2048), "2.0 KB/s"; got != want {
		t.Fatalf("formatSpeed = %q, want %q", got, want)
	}
}

func TestProgressPercentAndETAZeroCases(t *testing.T) {
	p := NewProgress(0)
	if got := p.percent(); got != 0 {
		t.Fatalf("percent with zero total = %v, want 0", got)
	}
	if got := p.eta(); got != 0 {
		t.Fatalf("eta with nothing done = %v, want 0", got)
	}
}

func TestProgressSpeedAfterElapsedTimeWithBytes(t *testing.T) {
	p := NewProgress(10)
	p.start = time.Now().Add(-time.Second)
	p.AddBytes(2048)
	if got := p.speed(); got <= 0 {
		t.Fatalf("speed = %v, want > 0 after elapsed time with bytes transferred", got)
	}
	rendered := p.Render()
	if !strings.Contains(rendered, "/s") {
		t.Fatalf("Render = %q, want it to include a speed readout", rendered)
	}
}
