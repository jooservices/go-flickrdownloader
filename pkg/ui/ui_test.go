package ui

import (
	"sync"
	"testing"
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
