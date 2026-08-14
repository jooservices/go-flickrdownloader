package download

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStatusPredicates(t *testing.T) {
	tests := []struct {
		status           int
		retryable, fatal bool
	}{
		{400, false, true}, {401, false, true}, {403, false, true}, {404, false, true},
		{408, true, false}, {416, false, false}, {429, true, false},
		{500, true, false}, {502, true, false}, {503, true, false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.status), func(t *testing.T) {
			if got := isRetryableStatus(tt.status); got != tt.retryable {
				t.Errorf("isRetryableStatus(%d) = %v, want %v", tt.status, got, tt.retryable)
			}
			if got := isFatalStatus(tt.status); got != tt.fatal {
				t.Errorf("isFatalStatus(%d) = %v, want %v", tt.status, got, tt.fatal)
			}
		})
	}
}

func TestClassifyTransport(t *testing.T) {
	timeoutErr := &url.Error{Op: "Get", URL: "https://example.test", Err: context.DeadlineExceeded}
	tests := []struct {
		name      string
		parent    context.Context
		want      outcome
		cancelled bool
	}{
		{"child deadline with clean parent", context.Background(), outcomeIncomplete, false},
		func() struct {
			name      string
			parent    context.Context
			want      outcome
			cancelled bool
		} {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return struct {
				name      string
				parent    context.Context
				want      outcome
				cancelled bool
			}{"cancelled parent", ctx, outcomeFatal, true}
		}(),
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := classifyTransport(tt.parent, timeoutErr)
			if got != tt.want {
				t.Fatalf("outcome = %v, want %v", got, tt.want)
			}
			if tt.cancelled && !errors.Is(err, errCancelled) {
				t.Errorf("error %v does not wrap errCancelled", err)
			}
		})
	}
}

func TestBackoffFor(t *testing.T) {
	tests := []struct {
		attempt int
		jitter  float64
		want    time.Duration
	}{
		{1, 0, 2 * time.Second}, {1, 0.2, 2400 * time.Millisecond},
		{2, 0, 4 * time.Second}, {2, 0.2, 4800 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("try_%d_jitter_%g", tt.attempt, tt.jitter), func(t *testing.T) {
			if got := backoffFor(tt.attempt, tt.jitter); got != tt.want {
				t.Errorf("backoffFor(%d, %g) = %v, want %v", tt.attempt, tt.jitter, got, tt.want)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name, value string
		want        time.Duration
		ok          bool
	}{
		{"delta seconds", "12", 12 * time.Second, true},
		{"overflow huge delta", "9223372037", 60 * time.Second, true},
		{"http date", now.Add(17 * time.Second).Format(http.TimeFormat), 17 * time.Second, true},
		{"capped", "3600", 60 * time.Second, true},
		{"negative", "-1", 0, false}, {"garbage", "soon", 0, false}, {"missing", "", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseRetryAfter(tt.value, now)
			if got != tt.want || ok != tt.ok {
				t.Errorf("parseRetryAfter(%q) = (%v, %v), want (%v, %v)", tt.value, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestParseUnsatisfiedRange(t *testing.T) {
	tests := []struct {
		value string
		want  int64
		ok    bool
	}{
		{"bytes */300", 300, true}, {"bytes */*", 0, false}, {"", 0, false}, {"bytes 0-299/300", 0, false}, {"bytes */nope", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got, ok := parseUnsatisfiedRange(tt.value)
			if got != tt.want || ok != tt.ok {
				t.Errorf("parseUnsatisfiedRange(%q) = (%d, %v), want (%d, %v)", tt.value, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestParseContentRange(t *testing.T) {
	tests := []struct {
		value             string
		start, end, total int64
		ok                bool
	}{
		{"bytes 12-34/100", 12, 34, 100, true}, {"bytes */100", 0, 0, 0, false}, {"", 0, 0, 0, false}, {"bytes 12-x/100", 0, 0, 0, false}, {"items 12-34/100", 0, 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			start, end, total, ok := parseContentRange(tt.value)
			if start != tt.start || end != tt.end || total != tt.total || ok != tt.ok {
				t.Errorf("parseContentRange(%q) = (%d, %d, %d, %v), want (%d, %d, %d, %v)", tt.value, start, end, total, ok, tt.start, tt.end, tt.total, tt.ok)
			}
		})
	}
}

func TestHTMLDetection(t *testing.T) {
	type sniffCase struct {
		name, body string
		want       bool
	}
	sniffCases := []sniffCase{
		{"leading whitespace", "\n\t <!DOCTYPE html><html></html>", true}, {"bom", "\ufeff <html></html>", true},
		{"html tag", "<HTML><body>x", true}, {"jpeg", "\xff\xd8\xff\xe0JFIF", false}, {"short non html", "not markup", false}, {"empty", "", false},
	}
	for _, tt := range sniffCases {
		t.Run("sniff_"+tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "body")
			if err := os.WriteFile(path, []byte(tt.body), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := sniffHTML(path)
			if err != nil || got != tt.want {
				t.Errorf("sniffHTML() = (%v, %v), want (%v, nil)", got, err, tt.want)
			}
		})
	}

	typeCases := []struct {
		value string
		want  bool
	}{
		{"text/html", true}, {"text/html; charset=utf-8", true}, {"application/xhtml+xml", true}, {"application/octet-stream", false}, {"image/jpeg", false}, {"", false},
	}
	for _, tt := range typeCases {
		t.Run("type_"+tt.value, func(t *testing.T) {
			if got := isHTMLType(tt.value); got != tt.want {
				t.Errorf("isHTMLType(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestValidateFile(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		total int64
		want  outcome
	}{
		{"zero unknown", "", -1, outcomeUnusable}, {"zero declared", "", 0, outcomeUnusable}, {"zero known", "", 3, outcomeUnusable},
		{"unknown skips completeness", "ok", -1, outcomeDone}, {"short", "ok", 3, outcomeIncomplete}, {"long", "four", 3, outcomeUnusable}, {"html", " <html>x", -1, outcomeUnusable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(path, []byte(tt.body), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := validateFile(path, tt.total)
			if err != nil || got != tt.want {
				t.Errorf("validateFile() = (%v, %v), want (%v, nil)", got, err, tt.want)
			}
		})
	}

	got, err := validateFile(filepath.Join(t.TempDir(), "missing"), -1)
	if got != outcomeFatal || err == nil {
		t.Errorf("missing validateFile() = (%v, %v), want (outcomeFatal, non-nil)", got, err)
	}
}
