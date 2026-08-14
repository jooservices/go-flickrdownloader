package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- helpers -----------------------------------------------------------------

type sleepRecorder struct {
	mu   sync.Mutex
	durs []time.Duration
}

func (s *sleepRecorder) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.durs = append(s.durs, d)
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (s *sleepRecorder) durations() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]time.Duration, len(s.durs))
	copy(out, s.durs)
	return out
}

func zeroJitter() float64 { return 0 }

type scriptedResponse struct {
	status         int
	body           string
	headers        map[string]string
	writeThenBlock string // if non-empty, write this then block until request ctx done
}

type scriptedHandler struct {
	mu        sync.Mutex
	responses []scriptedResponse
	idx       int
	reqs      []*http.Request
}

func (h *scriptedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	cloned := r.Clone(context.Background())
	h.reqs = append(h.reqs, cloned)
	var resp scriptedResponse
	if h.idx < len(h.responses) {
		resp = h.responses[h.idx]
		h.idx++
	} else {
		resp = scriptedResponse{status: http.StatusInternalServerError, body: "extra"}
	}
	h.mu.Unlock()

	for k, v := range resp.headers {
		w.Header().Set(k, v)
	}
	if resp.status == 0 {
		resp.status = http.StatusOK
	}
	if resp.writeThenBlock != "" {
		w.WriteHeader(resp.status)
		_, _ = w.Write([]byte(resp.writeThenBlock))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		return
	}
	w.WriteHeader(resp.status)
	_, _ = io.WriteString(w, resp.body)
}

func (h *scriptedHandler) requestCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.reqs)
}

func (h *scriptedHandler) requests() []*http.Request {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*http.Request, len(h.reqs))
	copy(out, h.reqs)
	return out
}

func newTestFetcher(t *testing.T, url, finalPath string, h *scriptedHandler, sleep *sleepRecorder) fetcher {
	t.Helper()
	return fetcher{
		url:       url,
		finalPath: finalPath,
		client:    h.client(),
		sleep:     sleep.sleep,
		jitter:    zeroJitter,
	}
}

func (h *scriptedHandler) client() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
}

func startScriptedServer(t *testing.T, responses ...scriptedResponse) (*httptest.Server, *scriptedHandler) {
	t.Helper()
	h := &scriptedHandler{responses: responses}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, h
}

func finalPathIn(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "photo.jpg")
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func exists(path string) bool {
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	panic(fmt.Sprintf("exists(%s): unexpected stat error: %v", path, err))
}

func assertNoFile(t *testing.T, path string) {
	t.Helper()
	if exists(path) {
		t.Fatalf("expected %s absent", path)
	}
}

func assertFileEquals(t *testing.T, path string, want []byte) {
	t.Helper()
	got := mustRead(t, path)
	if string(got) != string(want) {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func assertOutcome(t *testing.T, got, want outcome) {
	t.Helper()
	if got != want {
		t.Fatalf("outcome = %v, want %v", got, want)
	}
}

func assertSleeps(t *testing.T, sleep *sleepRecorder, want ...time.Duration) {
	t.Helper()
	got := sleep.durations()
	if len(got) != len(want) {
		t.Fatalf("sleep count = %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sleep[%d] = %v, want %v (all %v)", i, got[i], want[i], got)
		}
	}
}

func assertReqCount(t *testing.T, h *scriptedHandler, want int) {
	t.Helper()
	if got := h.requestCount(); got != want {
		t.Fatalf("request count = %d, want %d", got, want)
	}
}

func poisonBody(tag string) string { return "POISON-" + tag + "-BODY" }

func fileContains(path, substr string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(b), substr)
}

func makeImmutable(t *testing.T, path string) {
	t.Helper()
	switch runtime.GOOS {
	case "windows":
		t.Skip("immutable sibling requires chflags/chattr")
	case "darwin":
		if out, err := exec.Command("chflags", "uchg", path).CombinedOutput(); err != nil {
			t.Skipf("chflags uchg unavailable: %v (%s)", err, out)
		}
		t.Cleanup(func() { _ = exec.Command("chflags", "nouchg", path).Run() })
	default:
		if out, err := exec.Command("chattr", "+i", path).CombinedOutput(); err != nil {
			t.Skipf("chattr +i unavailable: %v (%s)", err, out)
		}
		t.Cleanup(func() { _ = exec.Command("chattr", "-i", path).Run() })
	}
}

// --- AC-001 / AC-002 / C15 ---------------------------------------------------

func TestFetch_RetryBackoffAC001(t *testing.T) {
	t.Run("500_500_200", func(t *testing.T) {
		srv, h := startScriptedServer(t,
			scriptedResponse{status: 500, body: poisonBody("500a")},
			scriptedResponse{status: 500, body: poisonBody("500b")},
			scriptedResponse{status: 200, body: "ok-bytes", headers: map[string]string{"Content-Type": "image/jpeg"}},
		)
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		f := newTestFetcher(t, srv.URL, final, h, sleep)

		_, err := f.run(context.Background())
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		// Done: err==nil asserted above
		assertReqCount(t, h, 3)
		assertSleeps(t, sleep, 2*time.Second, 4*time.Second)
		assertFileEquals(t, final, []byte("ok-bytes"))
		assertNoFile(t, final+".part")
	})

	t.Run("500_500_500", func(t *testing.T) {
		srv, h := startScriptedServer(t,
			scriptedResponse{status: 500, body: poisonBody("x")},
			scriptedResponse{status: 500, body: poisonBody("y")},
			scriptedResponse{status: 500, body: poisonBody("z")},
		)
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		f := newTestFetcher(t, srv.URL, final, h, sleep)

		_, err := f.run(context.Background())
		if err == nil {
			t.Fatalf("expected exhausted-retry error, got nil")
		}
		assertReqCount(t, h, 3)
		assertSleeps(t, sleep, 2*time.Second, 4*time.Second)
		assertNoFile(t, final+".part")
		assertNoFile(t, final+".cand")
	})

	t.Run("429_on_try3_no_sleep", func(t *testing.T) {
		srv, h := startScriptedServer(t,
			scriptedResponse{status: 500, body: "a"},
			scriptedResponse{status: 500, body: "b"},
			scriptedResponse{status: 429, body: "c", headers: map[string]string{"Retry-After": "9"}},
		)
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		f := newTestFetcher(t, srv.URL, final, h, sleep)

		_, err := f.run(context.Background())
		if err == nil {
			t.Fatalf("expected incomplete outcome, got nil err")
		}
		var rae retryAfterError
		if !errors.As(err, &rae) {
			t.Fatalf("expected retryAfterError in err, got %v", err)
		}
		// Retry-After was parsed into the error but must not cause a post-final-try sleep.
		if rae.delay != 9*time.Second {
			t.Fatalf("Retry-After delay = %v, want 9s", rae.delay)
		}
		assertReqCount(t, h, 3)
		assertSleeps(t, sleep, 2*time.Second, 4*time.Second) // no third sleep; Retry-After discarded
		assertNoFile(t, final+".part")
		assertNoFile(t, final+".cand")
		assertNoFile(t, final)
	})

	t.Run("429_retry_after_5_on_try1", func(t *testing.T) {
		srv, h := startScriptedServer(t,
			scriptedResponse{status: 429, body: "wait", headers: map[string]string{"Retry-After": "5"}},
			scriptedResponse{status: 200, body: "done", headers: map[string]string{"Content-Type": "image/jpeg"}},
		)
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		f := newTestFetcher(t, srv.URL, final, h, sleep)

		_, err := f.run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// Done: err==nil asserted above
		assertReqCount(t, h, 2)
		assertSleeps(t, sleep, 5*time.Second)
		assertFileEquals(t, final, []byte("done"))
		assertNoFile(t, final+".part")
		assertNoFile(t, final+".cand")
	})
}

func TestFetch_RetryableStatusesAC002(t *testing.T) {
	for _, status := range []int{408, 429, 500, 502, 503} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv, h := startScriptedServer(t,
				scriptedResponse{status: status, body: "err"},
				scriptedResponse{status: status, body: "err"},
				scriptedResponse{status: status, body: "err"},
			)
			sleep := &sleepRecorder{}
			final := finalPathIn(t)
			f := newTestFetcher(t, srv.URL, final, h, sleep)

			_, err := f.run(context.Background())
			if err == nil {
				t.Fatalf("expected incomplete outcome, got nil err")
			}
			assertReqCount(t, h, 3)
			assertSleeps(t, sleep, 2*time.Second, 4*time.Second)
			assertNoFile(t, final+".part")
			assertNoFile(t, final+".cand")
			assertNoFile(t, final)
		})
	}
}

// --- AC-003 ------------------------------------------------------------------

func TestFetch_ClientTimeoutAC003(t *testing.T) {
	srv, h := startScriptedServer(t,
		scriptedResponse{status: 200, writeThenBlock: "HALF", headers: map[string]string{
			"Content-Type":   "image/jpeg",
			"Content-Length": "8",
		}},
	)
	sleep := &sleepRecorder{}
	final := finalPathIn(t)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	f := fetcher{
		url:       srv.URL,
		finalPath: final,
		client:    &http.Client{Timeout: 150 * time.Millisecond},
		sleep:     sleep.sleep,
		jitter:    zeroJitter,
		maxTries:  1, // focus on the timeout path; avoid retrying the hang
	}

	_, err := f.run(parent)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	assertFileEquals(t, final+".part", []byte("HALF"))
	assertNoFile(t, final)
	assertNoFile(t, final+".cand")
	if parent.Err() != nil {
		t.Fatalf("parent ctx cancelled: %v", parent.Err())
	}
	// validateFile never invoked: prefix is short of Content-Length; had it
	// run and returned UNUSABLE for some other reason the file would be gone.
	// Presence of the prefix + INCOMPLETE is the C16 short-circuit.
	assertReqCount(t, h, 1)
	assertSleeps(t, sleep) // INCOMPLETE with maxTries=1: no retry sleep
	assertNoFile(t, final)
}

// --- AC-007 / AC-008 / AC-009 ------------------------------------------------

func TestFetch_Gated206AppendAC007(t *testing.T) {
	seed := []byte("AAAA")
	rest := "BBBB"
	srv, h := startScriptedServer(t, scriptedResponse{
		status: 206,
		body:   rest,
		headers: map[string]string{
			"Content-Type":  "image/jpeg",
			"Content-Range": "bytes 4-7/8",
		},
	})
	sleep := &sleepRecorder{}
	final := finalPathIn(t)
	writeFile(t, final+".part", seed)
	f := newTestFetcher(t, srv.URL, final, h, sleep)

	_, err := f.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Done: err==nil asserted above
	assertReqCount(t, h, 1)
	assertSleeps(t, sleep)
	assertFileEquals(t, final, []byte("AAAABBBB"))
	assertNoFile(t, final+".part")
}

func TestFetch_Gated206CapAC008(t *testing.T) {
	seed := []byte("AAAA")
	// Body exceeds remaining (4); copyCapped must stop at total.
	srv, h := startScriptedServer(t, scriptedResponse{
		status: 206,
		body:   "BBBBXXXXXXXX",
		headers: map[string]string{
			"Content-Type":  "image/jpeg",
			"Content-Range": "bytes 4-7/8",
		},
	})
	sleep := &sleepRecorder{}
	final := finalPathIn(t)
	writeFile(t, final+".part", seed)
	f := newTestFetcher(t, srv.URL, final, h, sleep)
	f.maxTries = 1

	_, err := f.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Done: err==nil asserted above
	assertFileEquals(t, final, []byte("AAAABBBB"))
	assertReqCount(t, h, 1)
	assertSleeps(t, sleep)
	assertNoFile(t, final+".part")
	assertNoFile(t, final+".cand")
	assertReqCount(t, h, 1)
}

func TestFetch_Gated206HTMLSniffAC009(t *testing.T) {
	seed := []byte("<html><body>") // poisoned from byte 0: sniff catches it
	html := "xxxxxx"
	srv, h := startScriptedServer(t, scriptedResponse{
		status: 206,
		body:   html,
		headers: map[string]string{
			"Content-Type":  "image/jpeg", // Gate B passes; sniff fails
			"Content-Range": "bytes 12-17/18",
		},
	})
	sleep := &sleepRecorder{}
	final := finalPathIn(t)
	writeFile(t, final+".part", seed)
	f := newTestFetcher(t, srv.URL, final, h, sleep)
	f.maxTries = 1

	_, err := f.run(context.Background())
	if err == nil {
		t.Fatalf("expected incomplete outcome, got nil err")
	}
	want := append(append([]byte{}, seed...), html...)
	assertFileEquals(t, final+".part", want)
	assertNoFile(t, final)
	assertReqCount(t, h, 1)
	assertSleeps(t, sleep)
}

// --- AC-010 ------------------------------------------------------------------

func TestFetch_GateBAndC_AC010(t *testing.T) {
	seed := []byte("SEEDPART")

	t.Run("html_content_type", func(t *testing.T) {
		srv, h := startScriptedServer(t, scriptedResponse{
			status: 206,
			body:   "xxxx",
			headers: map[string]string{
				"Content-Type":  "text/html",
				"Content-Range": "bytes 8-11/12",
			},
		})
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		writeFile(t, final+".part", seed)
		f := newTestFetcher(t, srv.URL, final, h, sleep)
		f.maxTries = 1

		_, err := f.run(context.Background())
		if err == nil {
			t.Fatalf("expected incomplete outcome, got nil err")
		}
		assertFileEquals(t, final+".part", seed)
		assertReqCount(t, h, 1)
		assertNoFile(t, final+".cand")
	})

	t.Run("end_plus_one_gt_total", func(t *testing.T) {
		srv, h := startScriptedServer(t, scriptedResponse{
			status: 206,
			body:   "xxxx",
			headers: map[string]string{
				"Content-Type":  "image/jpeg",
				"Content-Range": "bytes 8-20/12", // end+1 > total
			},
		})
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		writeFile(t, final+".part", seed)
		f := newTestFetcher(t, srv.URL, final, h, sleep)
		f.maxTries = 1

		_, err := f.run(context.Background())
		if err == nil {
			t.Fatalf("expected incomplete outcome, got nil err")
		}
		assertFileEquals(t, final+".part", seed)
		assertReqCount(t, h, 1)
	})
}

// --- AC-011 ------------------------------------------------------------------

func TestFetch_GateAFallbackAC011(t *testing.T) {
	seed := []byte("SEEDPART")
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"gap_start", map[string]string{"Content-Type": "image/jpeg", "Content-Range": "bytes 10-15/20"}},
		{"overlap_start", map[string]string{"Content-Type": "image/jpeg", "Content-Range": "bytes 4-9/20"}}, // start 4 < partSize 8
		{"missing_content_range", map[string]string{"Content-Type": "image/jpeg"}},
		{"star_total", map[string]string{"Content-Type": "image/jpeg", "Content-Range": "bytes 0-5/*"}},
		{"unparseable", map[string]string{"Content-Type": "image/jpeg", "Content-Range": "bytes junk"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv, h := startScriptedServer(t,
				scriptedResponse{status: 206, body: "CHUNK", headers: tc.headers},
				scriptedResponse{status: 500, body: poisonBody("cand")}, // candidate fallback INCOMPLETE
				scriptedResponse{status: 500, body: "again"},
			)
			sleep := &sleepRecorder{}
			final := finalPathIn(t)
			writeFile(t, final+".part", seed)
			f := newTestFetcher(t, srv.URL, final, h, sleep)
			f.maxTries = 2

			_, err := f.run(context.Background())
			if err == nil {
				t.Fatalf("expected incomplete outcome, got nil err")
			}
			assertFileEquals(t, final+".part", seed)
			assertNoFile(t, final+".cand")
			// try1: 2 requests; 1 sleep; try2: 1 request
			assertReqCount(t, h, 3)
			assertSleeps(t, sleep, 2*time.Second)
			reqs := h.requests()
			if reqs[0].Header.Get("Range") == "" {
				t.Fatal("first request should be Range")
			}
			if reqs[1].Header.Get("Range") != "" {
				t.Fatal("fallback candidate GET must not send Range")
			}
		})
	}
}

// --- AC-012 ------------------------------------------------------------------

func TestFetch_RangeIgnored200AC012(t *testing.T) {
	seed := []byte("OLDPART!")

	t.Run("valid_candidate_promoted", func(t *testing.T) {
		body := "NEWphoto"
		srv, h := startScriptedServer(t, scriptedResponse{
			status: 200,
			body:   body,
			headers: map[string]string{
				"Content-Type":   "image/jpeg",
				"Content-Length": "8",
			},
		})
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		writeFile(t, final+".part", seed)
		f := newTestFetcher(t, srv.URL, final, h, sleep)

		_, err := f.run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// Done: err==nil asserted above
		assertReqCount(t, h, 1)
		assertSleeps(t, sleep)
		assertFileEquals(t, final, []byte(body))
		assertNoFile(t, final+".part")
		assertNoFile(t, final+".cand")
		if h.requests()[0].Header.Get("Range") == "" {
			t.Fatal("expected Range on request")
		}
	})

	t.Run("invalid_candidate_deleted", func(t *testing.T) {
		srv, h := startScriptedServer(t, scriptedResponse{
			status: 200,
			body:   "<html>nope",
			headers: map[string]string{
				"Content-Type":   "image/jpeg",
				"Content-Length": "10",
			},
		})
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		writeFile(t, final+".part", seed)
		f := newTestFetcher(t, srv.URL, final, h, sleep)
		f.maxTries = 1

		_, err := f.run(context.Background())
		if err == nil {
			t.Fatalf("expected unusable outcome, got nil err")
		} // sniff -> UNUSABLE, try consumed, no retry
		assertReqCount(t, h, 1)
		assertSleeps(t, sleep)
		assertFileEquals(t, final+".part", seed)
		assertNoFile(t, final+".cand")
		assertNoFile(t, final)
	})
}

// --- AC-013 ------------------------------------------------------------------

func TestFetch_416AC013(t *testing.T) {
	t.Run("partSize_equals_total_promoted", func(t *testing.T) {
		seed := []byte("COMPLETE")
		srv, h := startScriptedServer(t, scriptedResponse{
			status: 416,
			body:   poisonBody("416"),
			headers: map[string]string{
				"Content-Range": "bytes */8",
			},
		})
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		writeFile(t, final+".part", seed)
		f := newTestFetcher(t, srv.URL, final, h, sleep)

		_, err := f.run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// Done: err==nil asserted above
		assertReqCount(t, h, 1)
		assertFileEquals(t, final, seed)
		assertNoFile(t, final+".part")
	})

	t.Run("mismatch_two_requests", func(t *testing.T) {
		seed := []byte("PARTIAL!")
		srv, h := startScriptedServer(t,
			scriptedResponse{status: 416, body: "x", headers: map[string]string{"Content-Range": "bytes */100"}},
			scriptedResponse{status: 200, body: "FULLFILE", headers: map[string]string{
				"Content-Type": "image/jpeg", "Content-Length": "8",
			}},
		)
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		writeFile(t, final+".part", seed)
		f := newTestFetcher(t, srv.URL, final, h, sleep)

		_, err := f.run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// Done: err==nil asserted above
		assertReqCount(t, h, 2)
		assertFileEquals(t, final, []byte("FULLFILE"))
		assertNoFile(t, final+".part")
	})

	t.Run("unparseable_two_requests", func(t *testing.T) {
		seed := []byte("PARTIAL!")
		srv, h := startScriptedServer(t,
			scriptedResponse{status: 416, body: "x", headers: map[string]string{"Content-Range": "bytes 0-1/2"}},
			scriptedResponse{status: 500, body: poisonBody("fb")},
		)
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		writeFile(t, final+".part", seed)
		f := newTestFetcher(t, srv.URL, final, h, sleep)
		f.maxTries = 1

		_, err := f.run(context.Background())
		if err == nil {
			t.Fatalf("expected incomplete outcome, got nil err")
		}
		assertReqCount(t, h, 2)
		assertFileEquals(t, final+".part", seed)
		assertNoFile(t, final+".cand")
	})
}

// --- AC-014 ------------------------------------------------------------------

func TestFetch_ZeroByteAC014(t *testing.T) {
	t.Run("zero_length_200", func(t *testing.T) {
		srv, h := startScriptedServer(t, scriptedResponse{
			status: 200,
			body:   "",
			headers: map[string]string{
				"Content-Type":   "image/jpeg",
				"Content-Length": "0",
			},
		})
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		f := newTestFetcher(t, srv.URL, final, h, sleep)
		f.maxTries = 1

		_, err := f.run(context.Background())
		if err == nil {
			t.Fatalf("expected unusable outcome, got nil err")
		}
		assertReqCount(t, h, 1)
		assertNoFile(t, final+".part")
		assertNoFile(t, final)
	})

	t.Run("preexisting_zero_part_plain_get", func(t *testing.T) {
		srv, h := startScriptedServer(t, scriptedResponse{
			status:  200,
			body:    "fresh",
			headers: map[string]string{"Content-Type": "image/jpeg", "Content-Length": "5"},
		})
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		writeFile(t, final+".part", nil) // zero-byte .part
		f := newTestFetcher(t, srv.URL, final, h, sleep)

		_, err := f.run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// Done: err==nil asserted above
		assertReqCount(t, h, 1)
		if h.requests()[0].Header.Get("Range") != "" {
			t.Fatal("zero-byte .part must not trigger Range; plain GET expected")
		}
		assertFileEquals(t, final, []byte("fresh"))
	})
}

// --- AC-015 ------------------------------------------------------------------

func TestFetch_FatalAndRangeStatusAC015(t *testing.T) {
	seed := []byte("KEEPME!!")

	t.Run("404_fatal_preserves_part", func(t *testing.T) {
		// Plain GET 404 with a pre-existing zero-byte .part (no resume base).
		srv, h := startScriptedServer(t, scriptedResponse{
			status: 404, body: poisonBody("404"),
		})
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		writeFile(t, final+".part", nil)
		f := newTestFetcher(t, srv.URL, final, h, sleep)

		_, err := f.run(context.Background())

		if err == nil {
			t.Fatal("expected error")
		}
		assertReqCount(t, h, 1)
		assertSleeps(t, sleep)
		assertFileEquals(t, final+".part", nil)
		assertNoFile(t, final+".cand")
	})

	t.Run("408_retried", func(t *testing.T) {
		srv, h := startScriptedServer(t,
			scriptedResponse{status: 408, body: "t"},
			scriptedResponse{status: 200, body: "okok!", headers: map[string]string{"Content-Type": "image/jpeg"}},
		)
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		f := newTestFetcher(t, srv.URL, final, h, sleep)

		_, err := f.run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// Done: err==nil asserted above
		assertReqCount(t, h, 2)
		assertSleeps(t, sleep, 2*time.Second)
		assertFileEquals(t, final, []byte("okok!"))
		assertNoFile(t, final+".part")
		assertNoFile(t, final+".cand")
	})

	t.Run("range_500_no_fallback", func(t *testing.T) {
		srv, h := startScriptedServer(t, scriptedResponse{
			status: 500, body: poisonBody("r500"),
		})
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		writeFile(t, final+".part", seed)
		f := newTestFetcher(t, srv.URL, final, h, sleep)
		f.maxTries = 1

		_, err := f.run(context.Background())
		if err == nil {
			t.Fatalf("expected incomplete outcome, got nil err")
		}
		assertReqCount(t, h, 1)
		assertFileEquals(t, final+".part", seed)
		assertNoFile(t, final+".cand")
		if fileContains(final+".part", "POISON") {
			t.Fatal("500 body leaked into .part")
		}
	})

	t.Run("range_404_fatal_no_fallback", func(t *testing.T) {
		srv, h := startScriptedServer(t, scriptedResponse{
			status: 404, body: poisonBody("r404"),
		})
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		writeFile(t, final+".part", seed)
		f := newTestFetcher(t, srv.URL, final, h, sleep)

		_, err := f.run(context.Background())
		if err == nil {
			t.Fatal("expected error")
		}

		assertReqCount(t, h, 1)
		assertFileEquals(t, final+".part", seed)
		assertNoFile(t, final+".cand")
	})
}

// --- AC-016 ------------------------------------------------------------------

func TestFetch_SniffRejectAC016(t *testing.T) {
	html := "<html>fake"

	t.Run("direct_plain_get", func(t *testing.T) {
		srv, h := startScriptedServer(t, scriptedResponse{
			status: 200,
			body:   html,
			headers: map[string]string{
				"Content-Type":   "image/jpeg",
				"Content-Length": "10",
			},
		})
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		f := newTestFetcher(t, srv.URL, final, h, sleep)
		f.maxTries = 1

		_, err := f.run(context.Background())
		if err == nil {
			t.Fatalf("expected unusable outcome, got nil err")
		}
		assertReqCount(t, h, 1)
		assertNoFile(t, final+".part")
		assertNoFile(t, final)
	})

	t.Run("candidate_promotion_path", func(t *testing.T) {
		seed := []byte("REALPART")
		srv, h := startScriptedServer(t, scriptedResponse{
			status: 200,
			body:   html,
			headers: map[string]string{
				"Content-Type":   "image/jpeg",
				"Content-Length": "10",
			},
		})
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		writeFile(t, final+".part", seed)
		f := newTestFetcher(t, srv.URL, final, h, sleep)
		f.maxTries = 1

		_, err := f.run(context.Background())
		if err == nil {
			t.Fatalf("expected unusable outcome, got nil err")
		}
		assertReqCount(t, h, 1)
		assertFileEquals(t, final+".part", seed)
		assertNoFile(t, final+".cand")
		assertNoFile(t, final)
	})
}

// --- AC-020 ------------------------------------------------------------------

func TestFetch_StrayCandDeletedAC020(t *testing.T) {
	seed := []byte("RESUME!")
	srv, h := startScriptedServer(t, scriptedResponse{
		status: 206,
		body:   "X",
		headers: map[string]string{
			"Content-Type":  "image/jpeg",
			"Content-Range": "bytes 7-7/8",
		},
	})
	sleep := &sleepRecorder{}
	final := finalPathIn(t)
	writeFile(t, final+".part", seed)
	writeFile(t, final+".cand", []byte("stale-candidate-valid-looking"))

	f := newTestFetcher(t, srv.URL, final, h, sleep)
	_, err := f.run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Done: err==nil asserted above
	assertReqCount(t, h, 1)
	if h.requests()[0].Header.Get("Range") != "bytes=7-" {
		t.Fatalf("expected resume Range bytes=7-, got %q", h.requests()[0].Header.Get("Range"))
	}
	assertNoFile(t, final+".cand")
	assertFileEquals(t, final, []byte("RESUME!X"))
}

// --- C4 / C11 / C14 ----------------------------------------------------------

func TestFetch_ReadOnlyDirC4(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("chmod-based read-only dir test skipped on Windows/root")
	}
	seed := []byte("KEEP")
	dir := t.TempDir()
	final := filepath.Join(dir, "photo.jpg")
	writeFile(t, final+".part", seed)
	writeFile(t, final+".cand", []byte("stray"))

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	srv, h := startScriptedServer(t, scriptedResponse{status: 200, body: "nope"})
	sleep := &sleepRecorder{}
	f := newTestFetcher(t, srv.URL, final, h, sleep)

	f.maxTries = 1
	_, err := f.run(context.Background())

	if err == nil || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("expected permission error, got %v", err)
	}
	assertFileEquals(t, final+".part", seed)            // .part never deleted on FS error
	assertFileEquals(t, final+".cand", []byte("stray")) // startup .cand removal failed; file untouched
	assertReqCount(t, h, 0)                             // failed before any request
	assertSleeps(t, sleep)                              // FATAL: no retry, no sleep
}

func TestFetch_WritePathEACCES(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("chmod-based read-only dir test skipped on Windows/root")
	}
	dir := t.TempDir()
	final := filepath.Join(dir, "photo.jpg")
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	srv, h := startScriptedServer(t, scriptedResponse{status: 200, body: "PAYLOAD", headers: map[string]string{"Content-Type": "image/jpeg", "Content-Length": "7"}})
	sleep := &sleepRecorder{}
	f := newTestFetcher(t, srv.URL, final, h, sleep)
	f.maxTries = 1

	_, err := f.run(context.Background())
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("expected permission error, got %v", err)
	}
	assertReqCount(t, h, 1)
	assertSleeps(t, sleep) // FATAL: no retry
	assertNoFile(t, final)
	assertNoFile(t, final+".part") // create failed; nothing written
	assertNoFile(t, final+".cand")
}

func TestFetch_PromoteSiblingCleanupC14(t *testing.T) {
	t.Run("rename_ok_sibling_remove_fails", func(t *testing.T) {
		seed := []byte("OLDPART!")
		body := "NEWphoto"
		srv, h := startScriptedServer(t, scriptedResponse{
			status:  200,
			body:    body,
			headers: map[string]string{"Content-Type": "image/jpeg", "Content-Length": "8"},
		})
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		writeFile(t, final+".part", seed)
		makeImmutable(t, final+".part")
		partPath := final + ".part"

		f := newTestFetcher(t, srv.URL, final, h, sleep)
		_, err := f.run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// Done: err==nil asserted above
		assertFileEquals(t, final, []byte(body))
		// The immutable sibling .part removal failed best-effort (C14):
		// the outcome is still Done and the .part may remain on disk.
		if !exists(partPath) {
			t.Log("sibling .part removed despite immutability (fs allowed it)")
		}
	})

	t.Run("rename_failure", func(t *testing.T) {
		dir := t.TempDir()
		final := filepath.Join(dir, "photo.jpg")
		// Make finalPath a non-empty directory so Rename(src, final) fails.
		if err := os.MkdirAll(filepath.Join(final, "nested"), 0o755); err != nil {
			t.Fatal(err)
		}
		srv, h := startScriptedServer(t, scriptedResponse{
			status:  200,
			body:    "payload!",
			headers: map[string]string{"Content-Type": "image/jpeg", "Content-Length": "8"},
		})
		sleep := &sleepRecorder{}
		f := newTestFetcher(t, srv.URL, final, h, sleep)

		_, err := f.run(context.Background())

		if err == nil {
			t.Fatal("expected rename error")
		}
		assertFileEquals(t, final+".part", []byte("payload!"))
		assertNoFile(t, final+".cand")
	})
}

// --- C5 / C13 ----------------------------------------------------------------

func TestFetch_ErrorBodiesNeverOnDiskC5(t *testing.T) {
	seed := []byte("SEEDDATA")

	t.Run("plain_5xx", func(t *testing.T) {
		p := poisonBody("p5xx")
		srv, h := startScriptedServer(t, scriptedResponse{status: 503, body: p})
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		f := newTestFetcher(t, srv.URL, final, h, sleep)
		f.maxTries = 1
		_, err := f.run(context.Background())
		if err == nil {
			t.Fatalf("expected incomplete outcome, got nil err")
		}
		assertNoFile(t, final+".part")
		assertNoFile(t, final+".cand")
		assertNoFile(t, final)
	})

	t.Run("plain_429", func(t *testing.T) {
		p := poisonBody("p429")
		srv, h := startScriptedServer(t, scriptedResponse{status: 429, body: p})
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		f := newTestFetcher(t, srv.URL, final, h, sleep)
		f.maxTries = 1
		_, _ = f.run(context.Background())
		assertNoFile(t, final+".part")
		assertNoFile(t, final+".cand")
		assertNoFile(t, final)
	})

	t.Run("plain_404", func(t *testing.T) {
		p := poisonBody("p404")
		srv, h := startScriptedServer(t, scriptedResponse{status: 404, body: p})
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		f := newTestFetcher(t, srv.URL, final, h, sleep)
		_, _ = f.run(context.Background())
		assertNoFile(t, final+".part")
		assertNoFile(t, final+".cand")
		assertNoFile(t, final)
	})

	t.Run("range_5xx", func(t *testing.T) {
		p := poisonBody("r5xx")
		srv, h := startScriptedServer(t, scriptedResponse{status: 502, body: p})
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		writeFile(t, final+".part", seed)
		f := newTestFetcher(t, srv.URL, final, h, sleep)
		f.maxTries = 1
		_, err := f.run(context.Background())
		if err == nil {
			t.Fatal("expected incomplete outcome, got nil err")
		}
		assertReqCount(t, h, 1)
		assertSleeps(t, sleep) // maxTries=1: no retry sleep
		assertFileEquals(t, final+".part", seed)
		assertNoFile(t, final+".cand")
		assertNoFile(t, final)
		if fileContains(final+".part", "POISON") {
			t.Fatal("poison in .part")
		}
	})

	t.Run("range_429", func(t *testing.T) {
		p := poisonBody("r429")
		srv, h := startScriptedServer(t, scriptedResponse{status: 429, body: p})
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		writeFile(t, final+".part", seed)
		f := newTestFetcher(t, srv.URL, final, h, sleep)
		f.maxTries = 1
		_, err := f.run(context.Background())
		if err == nil {
			t.Fatal("expected incomplete outcome, got nil err")
		}
		assertReqCount(t, h, 1)
		assertSleeps(t, sleep)
		assertFileEquals(t, final+".part", seed)
		assertNoFile(t, final+".cand")
		assertNoFile(t, final)
	})

	t.Run("range_404", func(t *testing.T) {
		p := poisonBody("r404")
		srv, h := startScriptedServer(t, scriptedResponse{status: 404, body: p})
		sleep := &sleepRecorder{}
		final := finalPathIn(t)
		writeFile(t, final+".part", seed)
		f := newTestFetcher(t, srv.URL, final, h, sleep)
		_, err := f.run(context.Background())
		if err == nil {
			t.Fatal("expected fatal outcome, got nil err")
		}
		assertReqCount(t, h, 1)
		assertSleeps(t, sleep)
		assertFileEquals(t, final+".part", seed)
		assertNoFile(t, final+".cand")
		assertNoFile(t, final)
	})
}

// --- C7 / C13 cancellation ---------------------------------------------------

func TestFetch_CancellationC13(t *testing.T) {
	seed := []byte("PRESERVE")

	t.Run("cancel_during_backoff", func(t *testing.T) {
		srv, h := startScriptedServer(t,
			scriptedResponse{status: 500, body: "x"},
			scriptedResponse{status: 200, body: "should-not-run"},
		)
		final := finalPathIn(t)
		writeFile(t, final+".part", seed)

		ctx, cancel := context.WithCancel(context.Background())
		sleep := &sleepRecorder{}
		f := fetcher{
			url:       srv.URL,
			finalPath: final,
			client:    h.client(),
			jitter:    zeroJitter,
			sleep: func(c context.Context, d time.Duration) error {
				sleep.mu.Lock()
				sleep.durs = append(sleep.durs, d)
				sleep.mu.Unlock()
				cancel()
				return c.Err()
			},
		}

		_, err := f.run(ctx)

		if !errors.Is(err, errCancelled) {
			t.Fatalf("err %v does not wrap errCancelled", err)
		}
		assertFileEquals(t, final+".part", seed)
		assertReqCount(t, h, 1)
		assertSleeps(t, sleep, 2*time.Second)
	})

	t.Run("cancel_beforeAttempt", func(t *testing.T) {
		srv, h := startScriptedServer(t, scriptedResponse{status: 200, body: "x"})
		final := finalPathIn(t)
		writeFile(t, final+".part", seed)
		sleep := &sleepRecorder{}
		f := fetcher{
			url:       srv.URL,
			finalPath: final,
			client:    h.client(),
			sleep:     sleep.sleep,
			jitter:    zeroJitter,
			beforeAttempt: func(ctx context.Context) error {
				return context.Canceled
			},
		}

		_, err := f.run(context.Background())

		if !errors.Is(err, errCancelled) {
			t.Fatalf("err %v does not wrap errCancelled", err)
		}
		assertFileEquals(t, final+".part", seed)
		assertReqCount(t, h, 0)
		assertSleeps(t, sleep)
	})
}
