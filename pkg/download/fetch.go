package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"time"
)

// sleeper pauses between retries. It is injectable so tests can avoid real
// timers and observe cancellation without waiting out a backoff.
type sleeper func(ctx context.Context, d time.Duration) error

// jitter returns a fractional multiplier applied to the exponential backoff.
// Values outside [0, 0.2] are clamped by backoffFor.
type jitter func() float64

// retryAfterError carries the parsed Retry-After delay of a 429 so run can
// honor it instead of the default exponential backoff.
type retryAfterError struct {
	delay time.Duration
}

func (e retryAfterError) Error() string {
	return fmt.Sprintf("retry after %s", e.delay)
}

// attemptResult carries one TRY's resolution (design: attemptResult).
type attemptResult struct {
	outcome         outcome
	written         int64
	retryAfter      time.Duration // 429 with a usable Retry-After (may be 0, meaning retry now)
	retryAfterValid bool          // set when Retry-After was parsed
	appended        bool          // this try's own gated 206 append wrote to .part -> AC-009 downgrade
	err             error
}

// fetcher downloads url to finalPath with resume support. It is a value type:
// all fields are safe to copy, and each run holds at most one in-memory
// fetcher so concurrent downloads never share mutable state.
type fetcher struct {
	url           string
	finalPath     string
	client        *http.Client
	sleep         sleeper
	jitter        jitter
	beforeAttempt func(context.Context) error
	maxTries      int
}

func (f fetcher) partPath() string { return f.finalPath + ".part" }
func (f fetcher) candPath() string { return f.finalPath + ".cand" }

func (f fetcher) httpClient() *http.Client {
	if f.client != nil {
		return f.client
	}
	return http.DefaultClient
}

// run performs up to 3 tries (1 initial + 2 retries). Both INCOMPLETE and
// UNUSABLE consume a retry and continue the loop; only outcomeDone and
// outcomeFatal return immediately. Sleep happens only when another try will
// actually run, so 3 failures cost exactly 2 sleeps. A 429's parsed
// Retry-After (capped at 60s) overrides the backoff. On success the total
// bytes written to the final file are returned.
func (f fetcher) run(ctx context.Context) (written int64, err error) {
	tries := f.maxTries
	if tries <= 0 || tries > 3 {
		tries = 3
	}

	var lastErr error

	for attempt := 1; attempt <= tries; attempt++ {
		if f.beforeAttempt != nil {
			if err := f.beforeAttempt(ctx); err != nil {
				return 0, fmt.Errorf("%w: %w", errCancelled, err)
			}
		}

		res := f.attempt(ctx)
		lastErr = res.err

		if res.outcome == outcomeDone {
			info, err := os.Stat(f.finalPath)
			if err != nil {
				return 0, fmt.Errorf("stat final file: %w", err)
			}
			return info.Size(), nil
		}
		if res.outcome == outcomeFatal {
			return 0, res.err
		}
		if attempt == tries {
			if lastErr == nil {
				lastErr = fmt.Errorf("download failed: %v", res.outcome)
			}
			break
		}

		var delay time.Duration
		if res.retryAfterValid {
			delay = res.retryAfter
		} else {
			delay = backoffFor(attempt, f.jitterValue())
		}
		if err := f.sleepWith(ctx, delay); err != nil {
			return 0, fmt.Errorf("%w: %w", errCancelled, err)
		}
	}
	return 0, lastErr
}

// attempt resolves one try: it deletes any stray .cand, then either resumes
// the existing .part (rangeGet) or starts a fresh download (plainGet).
func (f fetcher) attempt(ctx context.Context) attemptResult {
	if err := os.Remove(f.candPath()); err != nil && !os.IsNotExist(err) {
		return attemptResult{outcome: outcomeFatal, err: err}
	}

	info, err := os.Stat(f.partPath())
	switch {
	case err == nil && info.Size() > 0:
		return f.rangeGet(ctx, info.Size())
	case err == nil || os.IsNotExist(err):
		return f.plainGet(ctx, f.partPath(), false)
	default:
		return attemptResult{outcome: outcomeFatal, err: err}
	}
}

// plainGet performs a single request with no Range header. The response is
// written to dst (the authoritative .part, or a disposable .cand for a
// same-try fallback). A 200 with an HTML Content-Type is rejected up front.
func (f fetcher) plainGet(ctx context.Context, dst string, isCandidate bool) attemptResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url, nil)
	if err != nil {
		return attemptResult{outcome: outcomeFatal, err: err}
	}

	resp, err := f.httpClient().Do(req)
	if err != nil {
		o, e := classifyTransport(ctx, err)
		return attemptResult{outcome: o, err: e}
	}
	if o, err, handled := classifyStatus(resp); handled {
		drainClose(resp)
		res := attemptResult{outcome: o, err: err}
		var rae retryAfterError
		if errors.As(err, &rae) {
			res.retryAfter = rae.delay
			res.retryAfterValid = true
		}
		return res
	}
	if resp.StatusCode != http.StatusOK {
		// 1xx/3xx/unsolicited 206 on a plain GET must never be written or
		// promoted. Not classified as fatal/retryable -> treat as retryable.
		drainClose(resp)
		return attemptResult{outcome: outcomeIncomplete, err: fmt.Errorf("unexpected status %d on plain GET", resp.StatusCode)}
	}
	return f.ingest(resp, dst, isCandidate, -1, resp.ContentLength, false)
}

// rangeGet resumes an existing .part of partSize bytes. A 206 is appended
// only when all three gates pass; on any gate failure the try either falls
// back to plainGet-into-candidate (Gate A, 416) or resolves INCOMPLETE
// without touching .part (Gate B/C).
func (f fetcher) rangeGet(ctx context.Context, partSize int64) attemptResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url, nil)
	if err != nil {
		return attemptResult{outcome: outcomeFatal, err: err}
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", partSize))

	resp, err := f.httpClient().Do(req)
	if err != nil {
		o, e := classifyTransport(ctx, err)
		return attemptResult{outcome: o, err: e}
	}
	if o, err, handled := classifyStatus(resp); handled {
		drainClose(resp)
		res := attemptResult{outcome: o, err: err}
		var rae retryAfterError
		if errors.As(err, &rae) {
			res.retryAfter = rae.delay
			res.retryAfterValid = true
		}
		return res
	}

	switch resp.StatusCode {
	case http.StatusPartialContent:
		start, end, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok || start != partSize || total <= 0 || end < start {
			drainClose(resp)
			return f.plainGet(ctx, f.candPath(), true)
		}
		if isHTMLType(resp.Header.Get("Content-Type")) {
			drainClose(resp)
			return attemptResult{outcome: outcomeIncomplete}
		}
		if end >= total { // overflow-safe form of end+1 > total
			drainClose(resp)
			return attemptResult{outcome: outcomeIncomplete}
		}
		// Cap by the declared range width (end-start+1). Gate C guarantees
		// end < total (overflow-safe) and start >= 0, so end-start+1 is
		// bounded by total and cannot overflow (AC-008).
		res := f.ingest(resp, f.partPath(), false, end-start+1, total, true)
		if res.outcome == outcomeIncomplete || res.outcome == outcomeUnusable {
			res.appended = true
		}
		return res
	case http.StatusOK:
		return f.ingest(resp, f.candPath(), true, -1, resp.ContentLength, false)
	case http.StatusRequestedRangeNotSatisfiable:
		drainClose(resp)
		total, ok := parseUnsatisfiedRange(resp.Header.Get("Content-Range"))
		if ok && partSize == total {
			// AC-013: size == complete-length -> finish locally via BR4,
			// NO new request (second-opinion ruling). A validation failure
			// is UNUSABLE: delete the bad .part (BR1) so the next try is a
			// fresh plain GET instead of looping on 416. FATAL is returned
			// untouched (AC-015 forbids retrying it).
			res := f.promoteExisting(f.partPath(), total)
			if res.outcome == outcomeDone {
				return res
			}
			if res.outcome == outcomeUnusable {
				if err := os.Remove(f.partPath()); err != nil && !os.IsNotExist(err) {
					return attemptResult{outcome: outcomeFatal, err: err}
				}
				return res
			}
			return res
		}
		return f.plainGet(ctx, f.candPath(), true)
	default:
		// Only 206/200/416 are handled; 1xx/3xx/other must never be written.
		drainClose(resp)
		return attemptResult{outcome: outcomeIncomplete, err: fmt.Errorf("unexpected status %d on range GET", resp.StatusCode)}
	}
}

// ingest writes resp's body to dst, validates it, and decides the outcome.
// A read-side failure short-circuits to INCOMPLETE without validateFile; the
// .part keeps its already-written bytes while a .cand is discarded. Only a
// clean copy+Close leads to validation and promotion.
func (f fetcher) ingest(resp *http.Response, dst string, isCandidate bool, limit, expectedTotal int64, preserveOnUnusable bool) attemptResult {
	defer drainClose(resp)

	if isHTMLType(resp.Header.Get("Content-Type")) {
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			return attemptResult{outcome: outcomeFatal, err: err}
		}
		return attemptResult{outcome: outcomeUnusable}
	}

	out, err := openForWrite(dst, isCandidate)
	if err != nil {
		return attemptResult{outcome: outcomeFatal, err: err}
	}

	n, readErr, writeErr := copyCapped(out, resp.Body, limit)
	closeErr := out.Close()

	// C4/AC-015: a local filesystem failure (write or close) is FATAL and
	// must never be masked by a read-side error. Check writeErr/closeErr
	// FIRST — a read error only short-circuits to INCOMPLETE when the write
	// side stayed clean.
	if writeErr != nil {
		return attemptResult{outcome: outcomeFatal, written: n, err: writeErr}
	}
	if closeErr != nil {
		return attemptResult{outcome: outcomeFatal, written: n, err: closeErr}
	}
	if readErr != nil {
		if isCandidate {
			if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
				return attemptResult{outcome: outcomeFatal, err: err}
			}
		}
		return attemptResult{outcome: outcomeIncomplete, written: n, err: readErr}
	}

	o, err := validateFile(dst, expectedTotal)
	if err != nil {
		return attemptResult{outcome: outcomeFatal, written: n, err: err}
	}
	switch o {
	case outcomeUnusable:
		if preserveOnUnusable {
			return attemptResult{outcome: outcomeIncomplete, written: n, err: err}
		}
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			return attemptResult{outcome: outcomeFatal, written: n, err: err}
		}
		return attemptResult{outcome: outcomeUnusable, written: n, err: err}
	case outcomeIncomplete:
		if isCandidate {
			if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
				return attemptResult{outcome: outcomeFatal, written: n, err: err}
			}
		}
		return attemptResult{outcome: outcomeIncomplete, written: n, err: err}
	default:
		if err := f.promote(dst); err != nil {
			return attemptResult{outcome: outcomeFatal, written: n, err: err}
		}
		return attemptResult{outcome: outcomeDone, written: n}
	}
}

// promoteExisting validates an already-complete file and promotes it when the
// verdict is Done, without issuing a new request (the 416 partSize==total path).
func (f fetcher) promoteExisting(dst string, total int64) attemptResult {
	o, err := validateFile(dst, total)
	if err != nil {
		return attemptResult{outcome: outcomeFatal, err: err}
	}
	if o != outcomeDone {
		return attemptResult{outcome: o, err: err}
	}
	if err := f.promote(dst); err != nil {
		return attemptResult{outcome: outcomeFatal, err: err}
	}
	return attemptResult{outcome: outcomeDone}
}

// promote renames src to finalPath. It returns nil IFF the rename succeeded.
// The BR7 sibling .part/.cand removal that follows is BEST-EFFORT: a failure
// there is logged and ignored, never converted into an error (C14).
func (f fetcher) promote(src string) error {
	if err := os.Rename(src, f.finalPath); err != nil {
		return err
	}
	for _, sibling := range []string{f.partPath(), f.candPath()} {
		if sibling == src {
			continue
		}
		if err := os.Remove(sibling); err != nil && !os.IsNotExist(err) {
			log.Printf("download: remove sibling %s: %v", sibling, err)
		}
	}
	return nil
}

// copyCapped copies at most limit bytes from src to dst. limit < 0 copies
// everything. readErr and writeErr are reported separately so the caller can
// tell a server-side failure (retryable, keep .part) from a disk failure
// (FATAL). A clean EOF is not an error.
func copyCapped(dst *os.File, src io.Reader, limit int64) (n int64, readErr, writeErr error) {
	if limit < 0 {
		limit = math.MaxInt64
	}
	buf := make([]byte, 32*1024)
	for n < limit {
		want := int64(len(buf))
		if remaining := limit - n; remaining < want {
			want = remaining
		}
		nr, rerr := src.Read(buf[:want])
		if nr > 0 {
			nw, werr := dst.Write(buf[:nr])
			n += int64(nw)
			if werr != nil {
				return n, nil, werr
			}
			if nw < nr {
				return n, nil, io.ErrShortWrite
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return n, nil, nil
			}
			return n, rerr, nil
		}
	}
	return n, nil, nil
}

// drainClose reads up to 64KB of the body to allow connection reuse, then
// closes it. Used whenever the body is not going to be written to disk.
func drainClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
	resp.Body.Close()
}

// classifyStatus maps any status that is not a 200/206/416 to its outcome.
func classifyStatus(resp *http.Response) (outcome, error, bool) {
	if isFatalStatus(resp.StatusCode) {
		return outcomeFatal, fmt.Errorf("http status %d", resp.StatusCode), true
	}
	if isRetryableStatus(resp.StatusCode) {
		if resp.StatusCode == http.StatusTooManyRequests {
			if d, ok := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
				return outcomeIncomplete, retryAfterError{delay: d}, true
			}
		}
		return outcomeIncomplete, nil, true
	}
	return 0, nil, false
}

// openForWrite returns the destination file for a body being written.
// Candidates and fresh .part files are truncated; an existing .part with
// bytes is opened append-only so a resume never loses already-written data.
func openForWrite(dst string, isCandidate bool) (*os.File, error) {
	if isCandidate {
		return os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	}
	info, err := os.Stat(dst)
	if err == nil && info.Size() > 0 {
		return os.OpenFile(dst, os.O_WRONLY|os.O_APPEND, 0o644)
	}
	return os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
}

func (f fetcher) jitterValue() float64 {
	if f.jitter != nil {
		return f.jitter()
	}
	return 0
}

func (f fetcher) sleepWith(ctx context.Context, d time.Duration) error {
	if f.sleep != nil {
		return f.sleep(ctx, d)
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
