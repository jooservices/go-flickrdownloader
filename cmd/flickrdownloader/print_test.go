package main

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/jooservices/flickrdownloader/pkg/api"
	"github.com/jooservices/flickrdownloader/pkg/download"
)

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = orig
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestPluralIES(t *testing.T) {
	if got := pluralIES(1); got != "y" {
		t.Fatalf("pluralIES(1) = %q, want %q", got, "y")
	}
	if got := pluralIES(0); got != "ies" {
		t.Fatalf("pluralIES(0) = %q, want %q", got, "ies")
	}
	if got := pluralIES(2); got != "ies" {
		t.Fatalf("pluralIES(2) = %q, want %q", got, "ies")
	}
}

func TestVerificationSymbol(t *testing.T) {
	cases := []struct {
		state  download.VerificationState
		symbol string
	}{
		{download.VerificationComplete, "✓"},
		{download.VerificationIncomplete, "⚠"},
		{download.VerificationStale, "✗"},
		{download.VerificationError, "✗"},
		{download.VerificationNotScanned, "·"},
	}
	for _, c := range cases {
		_, sym := verificationSymbol(c.state)
		if sym != c.symbol {
			t.Errorf("verificationSymbol(%v) symbol = %q, want %q", c.state, sym, c.symbol)
		}
	}
}

func TestSetByID(t *testing.T) {
	sets := []api.PhotoSetInfo{{ID: "1"}, {ID: "2"}}
	got := setByID(sets, "2")
	if got.ID != "2" {
		t.Fatalf("setByID = %+v, want id 2", got)
	}
	if missing := setByID(sets, "missing"); missing.ID != "" {
		t.Fatalf("setByID(missing) = %+v, want zero value", missing)
	}
}

func TestBuildPickerItems(t *testing.T) {
	sets := []api.PhotoSetInfo{{ID: "1", Photos: 3, Title: struct {
		Content string `json:"_content"`
	}{Content: "Album A"}}}
	firstPage := &api.PhotosResponse{}
	firstPage.Photos.Total = 5

	items := buildPickerItems(sets, firstPage, 5)
	if len(items) != 2 {
		t.Fatalf("items = %+v, want 2 (1 album + uncategorized)", items)
	}
	if items[0].ID != "1" || items[0].Label != "Album A" {
		t.Fatalf("items[0] = %+v", items[0])
	}
	if items[1].ID != uncategorizedID {
		t.Fatalf("items[1] = %+v, want the uncategorized bucket", items[1])
	}
	if !strings.Contains(items[1].Detail, "2 photos") {
		t.Fatalf("uncategorized detail = %q, want 2 photos (5 total - 3 in album)", items[1].Detail)
	}
}

func TestPrintQuotaHeaderSkippedWithoutLimiter(t *testing.T) {
	client := api.NewClient("key", "secret", "token", "token-secret")
	out := captureStdout(t, func() { printQuotaHeader(client) })
	if out != "" {
		t.Fatalf("printQuotaHeader with no quota limiter = %q, want empty (limit<=0 is a no-op)", out)
	}
}

func TestPrintRunSummarySkippedWithoutRequests(t *testing.T) {
	client := api.NewClient("key", "secret", "token", "token-secret")
	out := captureStdout(t, func() { printRunSummary(client) })
	if out != "" {
		t.Fatalf("printRunSummary with 0 requests = %q, want empty", out)
	}
}

func TestPrintVerificationReport(t *testing.T) {
	report := &download.VerificationReport{
		Photosets: []download.PhotosetVerification{
			{Title: "Album A", State: download.VerificationComplete, Present: 3, Expected: 3},
			{Title: "Album B", State: download.VerificationIncomplete, Present: 1, Expected: 3, Missing: []string{"1", "2"}},
		},
		Uncategorized: &download.PhotosetVerification{Title: "Uncategorized photos", State: download.VerificationNotScanned},
	}
	out := captureStdout(t, func() { printVerificationReport(report) })
	if !strings.Contains(out, "Album A") || !strings.Contains(out, "Album B") || !strings.Contains(out, "Uncategorized photos") {
		t.Fatalf("printVerificationReport output = %q, want all three entries", out)
	}
	if !strings.Contains(out, "not scanned") {
		t.Fatalf("output = %q, want the not-scanned hint", out)
	}
}

func TestPrintScanReport(t *testing.T) {
	report := &download.ScanReport{
		Photosets: []download.ScanResult{
			{Title: "Album A", Present: 5, ExpectedRemote: 5, Complete: true},
			{Title: "Album B", Present: 2, ExpectedRemote: 5, Complete: false},
		},
		Uncategorized: &download.ScanResult{Title: "Uncategorized photos", Present: 3},
		Unmatched:     []download.ScanResult{{Title: "Random Folder", Present: 1}},
	}
	out := captureStdout(t, func() { printScanReport(report, false) })
	if !strings.Contains(out, "Album A") || !strings.Contains(out, "Album B") || !strings.Contains(out, "missing vs Flickr") {
		t.Fatalf("printScanReport output = %q, want both albums and the incomplete-album detail", out)
	}
	if !strings.Contains(out, "Random Folder") {
		t.Fatalf("output = %q, want the unmatched folder listed", out)
	}

	empty := captureStdout(t, func() { printScanReport(&download.ScanReport{}, false) })
	if !strings.Contains(empty, "no downloaded photos found") {
		t.Fatalf("empty report output = %q, want the no-photos message", empty)
	}

	offline := captureStdout(t, func() { printScanReport(&download.ScanReport{}, true) })
	if !strings.Contains(offline, "Disk-only") {
		t.Fatalf("offline report output = %q, want the disk-only notice", offline)
	}
}

func TestEstimateAPICalls(t *testing.T) {
	selected := []api.PhotoSetInfo{{ID: "1", Photos: 600}}
	firstPage := &api.PhotosResponse{}
	firstPage.Photos.Photo = []api.Photo{
		{ID: "1", URLOriginal: "https://live.staticflickr.com/1.jpg"},
		{ID: "2", Media: "video"},
	}
	listing, sizes := estimateAPICalls(selected, false, 600, firstPage)
	if listing != 2 {
		t.Fatalf("listing = %d, want 2 (600 photos / 500 per page, rounded up)", listing)
	}
	if sizes <= 0 {
		t.Fatalf("sizes = %d, want > 0 (one sampled photo needs a getSizes fallback)", sizes)
	}
}

func TestReadLine(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("hello world\n"))
	got, err := readLine(r)
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if got != "hello world" {
		t.Fatalf("readLine = %q, want %q", got, "hello world")
	}
}

func TestReadLineNoTrailingNewline(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("no newline"))
	got, err := readLine(r)
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if got != "no newline" {
		t.Fatalf("readLine = %q, want %q", got, "no newline")
	}
}

func TestReadLineEmptyInputErrors(t *testing.T) {
	r := bufio.NewReader(strings.NewReader(""))
	if _, err := readLine(r); err == nil {
		t.Fatal("expected an error (EOF) for empty input")
	}
}

// TestReadSecretNonTTYFallsBackToReadLine covers readSecret's non-terminal
// path (piped input, as in tests/scripts) — go test's stdin is never a TTY,
// so this always exercises that branch.
func TestReadSecretNonTTYFallsBackToReadLine(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("s3cr3t\n"))
	got, err := readSecret(r)
	if err != nil {
		t.Fatalf("readSecret: %v", err)
	}
	if got != "s3cr3t" {
		t.Fatalf("readSecret = %q, want %q", got, "s3cr3t")
	}
}

func TestConfirm(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"\n", true},
		{"y\n", true},
		{"Y\n", true},
		{"yes\n", true},
		{"n\n", false},
		{"no\n", false},
		{"garbage\n", false},
	}
	for _, c := range cases {
		origStdin := os.Stdin
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.WriteString(c.input); err != nil {
			t.Fatal(err)
		}
		w.Close()
		os.Stdin = r

		out := captureStdout(t, func() {
			if got := confirm("Continue?"); got != c.want {
				t.Errorf("confirm(%q input) = %v, want %v", c.input, got, c.want)
			}
		})
		os.Stdin = origStdin
		if !strings.Contains(out, "Continue?") {
			t.Errorf("confirm output = %q, want the prompt echoed", out)
		}
	}
}

func TestSetupSignalContextStopCancelsContext(t *testing.T) {
	ctx, stop := setupSignalContext()
	select {
	case <-ctx.Done():
		t.Fatal("context cancelled before stop() was called")
	default:
	}
	stop()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("expected ctx to be cancelled after stop()")
	}
	if !errorsIsContextCanceled(ctx) {
		t.Fatal("expected context.Canceled after stop()")
	}
}

func errorsIsContextCanceled(ctx context.Context) bool {
	return ctx.Err() == context.Canceled
}
