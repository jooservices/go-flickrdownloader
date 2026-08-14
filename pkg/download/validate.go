package download

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// outcome is the result taxonomy for one download attempt. HTTP 416 is a
// signal handled by the resume path, rather than an outcome of its own.
type outcome int

const (
	outcomeDone outcome = iota
	outcomeIncomplete
	outcomeUnusable
	outcomeFatal
)

var errCancelled = errors.New("download cancelled")

func isRetryableStatus(status int) bool {
	return status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests ||
		(status >= http.StatusInternalServerError && status < 600)
}

func isFatalStatus(status int) bool {
	return status >= http.StatusBadRequest && status < http.StatusInternalServerError &&
		status != http.StatusRequestTimeout &&
		status != http.StatusTooManyRequests &&
		status != http.StatusRequestedRangeNotSatisfiable
}

// classifyTransport intentionally consults the parent context first. A client
// timeout may cancel its child request context while the parent remains valid;
// that is retryable and must preserve the partial file.
func classifyTransport(parent context.Context, err error) (outcome, error) {
	if parent != nil && parent.Err() != nil {
		return outcomeFatal, fmt.Errorf("%w: %w", errCancelled, parent.Err())
	}
	return outcomeIncomplete, err
}

func isHTMLType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	switch strings.ToLower(mediaType) {
	case "text/html", "application/xhtml+xml":
		return true
	default:
		return false
	}
}

func sniffHTML(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	buf := make([]byte, 512)
	n, err := f.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	buf = bytes.TrimSpace(buf[:n])
	buf = bytes.TrimPrefix(buf, []byte{0xef, 0xbb, 0xbf})
	buf = bytes.TrimSpace(buf)
	buf = bytes.ToLower(buf)
	return bytes.HasPrefix(buf, []byte("<!doctype html")) || bytes.HasPrefix(buf, []byte("<html")), nil
}

func parseContentRange(v string) (start, end, total int64, ok bool) {
	fields := strings.Fields(v)
	if len(fields) != 2 || fields[0] != "bytes" {
		return 0, 0, 0, false
	}
	parts := strings.Split(fields[1], "/")
	if len(parts) != 2 || parts[0] == "*" || parts[1] == "*" {
		return 0, 0, 0, false
	}
	rangeParts := strings.Split(parts[0], "-")
	if len(rangeParts) != 2 {
		return 0, 0, 0, false
	}
	start, ok = parseNonNegativeInt64(rangeParts[0])
	if !ok {
		return 0, 0, 0, false
	}
	end, ok = parseNonNegativeInt64(rangeParts[1])
	if !ok || end < start {
		return 0, 0, 0, false
	}
	total, ok = parseNonNegativeInt64(parts[1])
	if !ok {
		return 0, 0, 0, false
	}
	return start, end, total, true
}

func parseUnsatisfiedRange(v string) (total int64, ok bool) {
	fields := strings.Fields(v)
	if len(fields) != 2 || fields[0] != "bytes" || !strings.HasPrefix(fields[1], "*/") {
		return 0, false
	}
	value := strings.TrimPrefix(fields[1], "*/")
	if value == "" || value == "*" || strings.Contains(value, "/") {
		return 0, false
	}
	total, ok = parseNonNegativeInt64(value)
	if !ok {
		return 0, false
	}
	return total, true
}

func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}

	if seconds, isDelta := parseNonNegativeInt64(v); isDelta {
		if seconds > int64((maxRetryAfter / time.Second)) {
			return maxRetryAfter, true
		}
		return capRetryAfter(time.Duration(seconds) * time.Second), true
	}

	when, err := http.ParseTime(v)
	if err != nil {
		return 0, false
	}
	delay := when.Sub(now)
	if delay < 0 {
		return 0, false
	}
	return capRetryAfter(delay), true
}

const maxRetryAfter = 60 * time.Second

func capRetryAfter(delay time.Duration) time.Duration {
	if delay > maxRetryAfter {
		return maxRetryAfter
	}
	return delay
}

func parseNonNegativeInt64(v string) (int64, bool) {
	if v == "" {
		return 0, false
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil
}

func backoffFor(attempt int, jitter float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if jitter < 0 {
		jitter = 0
	} else if jitter > 0.2 {
		jitter = 0.2
	}
	base := time.Duration(1<<uint(attempt)) * time.Second
	return time.Duration(float64(base) * (1 + jitter))
}

func validateFile(path string, total int64) (outcome, error) {
	info, err := os.Stat(path)
	if err != nil {
		return outcomeFatal, err
	}
	size := info.Size()
	if size == 0 {
		return outcomeUnusable, nil
	}
	if total >= 0 {
		if size < total {
			return outcomeIncomplete, nil
		}
		if size > total {
			return outcomeUnusable, nil
		}
	}
	html, err := sniffHTML(path)
	if err != nil {
		return outcomeFatal, err
	}
	if html {
		return outcomeUnusable, nil
	}
	return outcomeDone, nil
}
