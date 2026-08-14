# Design — Improve Download Robustness in `pkg/download` (v4 — final)

**Feature:** `robust-downloads` · **Requirements:** `docs/features/robust-downloads/requirements.md` (APPROVED, immutable) · **Status:** FINAL (consensus loop complete: C1–C16 resolved in rounds 1–2, T1–T3 applied in final revision; approved by Debater contract)

## Overview

`pkg/download` currently downloads via `downloadFile` (`downloader.go:184-254`), which truncates its temp file on every retry, treats all transport errors alike, performs no response validation, and cannot resume. This design replaces the per-photo download path with a state machine resolving each **try** to exactly one of INCOMPLETE / UNUSABLE / FATAL, resuming from a `.part` via HTTP Range, validating every completed file before promotion, and never deleting an authoritative `.part` unless a validated replacement has been promoted.

Scope is confined to `package download`. No API, CLI-flag, config, or dependency changes.

## Architecture

1. **Orchestration** (`downloader.go`, modified) — worker pool, dedupe/hardlink, progress, per-album sweep. Owns policy defaults and injects them downward, including the rate limiter (C8).
2. **Per-photo state machine** (`fetch.go`, new) — a `fetcher` value type owning one photo's URL, paths, HTTP client, and injected sleep/jitter/pre-try hook. Implements BR2/BR3 (try loop) and BR5 (try dispatch). No dependency on `api.Client`, `ui.Progress`, `Downloader`, or `rate.Limiter`.
3. **Pure predicates** (`validate.go`, new) — status dispatch, header parsing, backoff arithmetic, file validation. Only `sniffHTML` and `validateFile` touch the filesystem.
4. **Sweep** (`sweep.go`, new) — BR8 stale-file removal, filesystem-only.

**Request layering (C12).** Response *handling* is factored out of response *fetching*. `ingest` owns everything downstream of a received response — HTML header check → `copyCapped` → `validateFile` → promote-or-delete — and issues no requests. `plainGet` = one `Do` + `ingest`. `rangeGet`'s 200 branch calls `ingest` **directly on the response already in hand**, so a Range-ignored 200 costs exactly **one** request, not two.

**Try vs. request (C2).** AC-001 budgets three **tries**, not three `Do` calls. Exactly two paths issue a second request within one try: a 206 failing the identity gate (AC-011) and a 416 whose length disagrees with `.part` (AC-013). Both drain-and-close the first body first. Every other path is one request per try. Backoff, the try counter, and `beforeAttempt` (the limiter token) advance once per try.

## Alternatives considered and rejected

| Alternative | Why rejected |
|---|---|
| Grow `downloadFile` in place | The BR5 matrix pushes one function past 300 lines, still coupled to `*Downloader`; untestable at AC granularity. |
| Full interface decomposition (`Transport`/`Validator`/`RetryPolicy`) | One real implementation of each in a single-binary CLI. Indirection without coverage. |
| **`rangeGet`'s 200 branch delegating to `plainGet`** | Would re-issue the request, spending two `Do` calls and two limiter-free round trips for a server that simply ignored `Range` — and would race a second response against the first. Extracting `ingest` gives the same code reuse at one request (C12). |
| BR8 sweep as a startup pass | Needs a second full listing purely to learn the ID set; under `api/flickr.go:44`'s ~1 req/sec limiter that doubles time-to-first-byte, and needs explicit dry-run gating. End-of-album satisfies AC-017/AC-018 at zero API cost and inherits dry-run exclusion (`main.go:346,364,437`). |
| Sweep eligibility inferred from state | `downloadPhotosFromPages` also drives the orphan/owner-root batch (`:545`). An explicit caller flag is not fragile (C1). |
| A single `classifyStatus` covering 416 | 416 is a *signal*, not an outcome (BR1/AC-004); folding it in forces a fake outcome at the one call site that must branch (C3). |
| `validateFile` returning a bare `outcome` | Cannot separate "provably wrong" (UNUSABLE, delete) from "could not determine" (FATAL, touch nothing); conflating them deletes a resumable `.part` on transient EACCES (C4). |
| **`promote` treating sibling cleanup failure as FATAL** | The rename already succeeded — the final file exists and is correct. Reporting FATAL would re-download a complete photo and mark it failed. Cleanup is best-effort (C14). |
| **Validating after a read-side copy failure** | A truncated body is INCOMPLETE by definition (BR1); running `validateFile` on it risks a size/sniff verdict of UNUSABLE that would delete a resumable `.part` (C16). |
| Coercing an absent `Content-Length` to 0 | Collides with a declared empty body; `http.Response.ContentLength` already uses -1 (C6). |
| `Clock` interface / fake-clock dependency | Larger surface than two func fields; `go.mod` stays at 4 direct deps. |
| Per-request `context.WithTimeout` replacing `http.Client.Timeout` | AC-003 requires the timeout unchanged. |
| `.part` keyed on photo ID alone | Contradicts BR6's approved `{finalPath}.part`. Accepted as a known limitation instead. |
| Error-string matching for the client timeout | `*url.Error`'s wrapping has shifted across Go releases; parent-`ctx` inspection is version-stable. |

## Public contracts

All identifiers package-private; `package download`'s exported surface is unchanged.

### `fetch.go`

```go
// outcome is the BR1 taxonomy. 416 is NOT a member — it is a signal (AC-004).
type outcome int

const (
    outcomeDone       outcome = iota // promoted; returned ONLY after rename succeeded (C3, C14)
    outcomeIncomplete                // preserve .part; consumes a try
    outcomeUnusable                  // delete the offending file; consumes a try
    outcomeFatal                     // no retry
)

// sleeper waits d or returns early when ctx is done, wrapping errCancelled (C7).
type sleeper func(ctx context.Context, d time.Duration) error

// errCancelled marks any FATAL rooted in parent-context cancellation —
// transport, sleeper, or beforeAttempt. BR9 excludes it from per-photo
// failure reporting. Always tested with errors.Is (C7).
var errCancelled = errors.New("download cancelled")

type fetcher struct {
    url       string
    finalPath string
    client    *http.Client   // carries the 60s timeout (AC-003)
    sleep     sleeper
    jitter    func() float64 // [0, 0.20]
    // beforeAttempt runs once per TRY before any request. Production binds
    // d.dlLimiter.Wait; it must wrap errCancelled on cancellation (C8).
    beforeAttempt func(ctx context.Context) error
}

func (f *fetcher) partPath() string // finalPath + ".part"
func (f *fetcher) candPath() string // finalPath + ".cand"

// run executes up to 3 tries. ctx is the PARENT context and the sole
// cancellation authority handed to classifyTransport (C7). It sleeps only
// when another try will actually run (C15).
func (f *fetcher) run(ctx context.Context) (written int64, err error)

// attempt performs one TRY (one request, or two on the AC-011/AC-013
// fallback paths) and resolves BR5.
func (f *fetcher) attempt(ctx context.Context) attemptResult

type attemptResult struct {
    outcome    outcome
    written    int64
    retryAfter time.Duration // non-zero only for 429 with a usable Retry-After
    appended   bool          // this try's own gated 206 append wrote to .part -> AC-009 downgrade
    err        error
}

// ingest owns everything downstream of a received response and issues NO
// requests (C12). It closes resp.Body. Sequence: 200 HTML header check ->
// copyCapped -> (read error => INCOMPLETE, no validation) -> Close ->
// validateFile -> promote or delete.
//   dst                — the file to write (fresh .part, or .cand)
//   isCandidate        — governs which file a rejection deletes; an
//                        authoritative .part belonging to another path is
//                        never touched.
//   limit              — copyCapped cap; -1 when the server's own length
//                        governs.
//   expectedTotal      — total size for validateFile: 200 => resp.ContentLength
//                        (<0 unknown); 206 => the gated Content-Range
//                        complete-length (T3). Never the chunk/limit value.
//   preserveOnUnusable — true only on the gated-206 path: an UNUSABLE verdict
//                        is downgraded to INCOMPLETE (AC-009) and dst (the
//                        authoritative .part this try appended to) is NEVER
//                        deleted (T1).
func (f *fetcher) ingest(resp *http.Response, dst string, isCandidate bool, limit, expectedTotal int64, preserveOnUnusable bool) attemptResult

// plainGet = one Do + ingest.
func (f *fetcher) plainGet(ctx context.Context, dst string, isCandidate bool) attemptResult

// rangeGet issues the Range request and dispatches its response (C13).
func (f *fetcher) rangeGet(ctx context.Context, partSize int64) attemptResult

// promote renames src to finalPath. It returns nil IFF the rename succeeded.
// The BR7 sibling .part/.cand removal that follows is BEST-EFFORT: a failure
// there is logged and ignored, never converted into an error (C14).
func (f *fetcher) promote(src string) error

// copyCapped writes at most limit bytes (limit < 0 = unlimited), separating
// read-side failure (network -> INCOMPLETE) from write-side failure
// (filesystem -> FATAL). io.Copy conflates these; BR1/BR3 require the split.
func copyCapped(dst *os.File, src io.Reader, limit int64) (n int64, readErr, writeErr error)

// drainClose discards and closes a body that will not be ingested (C13).
func drainClose(resp *http.Response)
```

### `validate.go`

```go
// Status dispatch. 416 is handled by an explicit branch and is deliberately
// absent from both predicates (C3).
func isRetryableStatus(status int) bool // 408, 429, 5xx  -> INCOMPLETE-class
func isFatalStatus(status int) bool     // 4xx except 408, 429, 416

// classifyTransport takes ONLY the parent ctx handed to run. A child-context
// deadline (http.Client.Timeout) with a clean parent is INCOMPLETE (AC-003);
// a cancelled parent is FATAL (AC-015). Checked in that order (C7).
func classifyTransport(parent context.Context, err error) (outcome, error)

func isHTMLType(contentType string) bool  // base media type, params stripped
func sniffHTML(path string) (bool, error) // first ~512B on disk, BOM/leading-WS trimmed

func parseContentRange(v string) (start, end, total int64, ok bool)
func parseUnsatisfiedRange(v string) (total int64, ok bool) // "bytes */N"
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) // delta-secs or HTTP-date, cap 60s
func backoffFor(attempt int, jitter float64) time.Duration // 2s, 4s, each x(1+jitter)

// validateFile applies BR4 in order: zero-byte (unconditional, before and
// independent of the size check), then size completeness (total < 0 skips),
// then the on-disk sniff. The error return is reserved for filesystem
// failures, which the caller maps to FATAL without deleting anything (C4).
// It is called ONLY after a clean copy and a clean Close (C16).
func validateFile(path string, total int64) (outcome, error)
```

### `sweep.go`

```go
// BR8/AC-017. Deletes *.part and *.cand under dir whose leading photo-ID
// segment (filename up to the first '.') is absent from seenIDs.
// Callers guarantee: per-album directory, all pages succeeded, run not cancelled.
func sweepStaleParts(dir string, seenIDs map[string]bool)
```

### `downloader.go` (changed signature)

```go
// allowIDAbsentSweep is supplied explicitly by the caller (C1): true from the
// DownloadByUser photoset loop and DownloadByPhotoset; false for the orphan /
// owner-root batch. DownloadPhoto does not call this function at all.
func (d *Downloader) downloadPhotosFromPages(
    ctx context.Context,
    totalPhotos, totalPages int,
    allowIDAbsentSweep bool,
    fetchPage func(ctx context.Context, page int) ([]api.Photo, error),
) Stats
```

## Data model

**On-disk states** (siblings in the album directory):

| File | Authority | Lifetime |
|---|---|---|
| `{id}.{ext}` | authoritative, complete | permanent; only target of `downloadedPaths` (BR6) |
| `{id}.{ext}.part` | authoritative partial | survives INCOMPLETE, FATAL, and cancellation; removed only after a validated replacement is promoted (BR7, best-effort) or by the BR8 sweep |
| `{id}.{ext}.cand` | never authoritative | deleted unconditionally at the start of every try (AC-020) and **immediately** on any invalid result (C12) |
| `{id}.{ext}.tmp` | legacy | never written; excluded from `alreadyDownloaded` |

**`expectedTotal int64`** — per try, in-memory only (AC-005). Taken verbatim from `http.Response.ContentLength` on a 200, or the complete-length from `Content-Range` on a 206/416. **Single sentinel scheme (C6):** `< 0` unknown → size-completeness check skipped; `== 0` server-declared empty; `> 0` known. Never coerced.

**`.part` open modes (C11):** `partSize > 0` → `os.OpenFile(part, O_WRONLY|O_APPEND, 0644)` — `O_TRUNC` never appears on a non-empty `.part`. `partSize == 0` or absent → not a failed try, simply "no resume base"; a fresh plain GET opens `O_WRONLY|O_CREATE|O_TRUNC`. Candidates always `O_WRONLY|O_CREATE|O_TRUNC`. AC-014's zero-byte rule applies to an *assembled result after a write this try*, not to a pre-existing empty `.part` seen at try start.

**Per-album accumulators:** `seenIDs map[string]bool`, populated **at enqueue time** in the single producer goroutine immediately before `jobs <- photo`, so an ID is recorded even if its worker never runs (C10); `allPagesOK bool`, cleared when `fetchPage` errors (`:376-381`, currently only logged).

## Modules

**`pkg/download/fetch.go`** (new, ~300 lines).

**`run`** loops `attempt` up to 3 times. Each iteration: `beforeAttempt` (one limiter token per try; cancellation wraps `errCancelled`) → `attempt`. `outcomeDone` returns; `outcomeFatal` returns immediately. On INCOMPLETE/UNUSABLE the try is consumed, and **the sleep happens only if another try will actually run (C15)** — i.e. only when `attempt < maxTries`. Three consecutive 500s therefore produce exactly two sleeps; a 429 on the final try produces none, and its `Retry-After` is discarded. When sleeping, the delay is `retryAfter` if set, else `backoffFor(attempt, jitter())`; the sleeper wraps `errCancelled` if the parent dies mid-backoff.

**`attempt`** deletes any stray `.cand` unconditionally (AC-020), stats `.part`, then dispatches:
- `partSize == 0` or absent → `plainGet(partPath, false)`.
- `partSize > 0` → `rangeGet(partSize)`.

**`ingest(resp, dst, isCandidate, limit, expectedTotal, preserveOnUnusable)`** — the single response-handling pipeline (C12, T1/T3), issuing no requests and always closing the body:
1. **200 HTML header check, before any write (C5):** `isHTMLType(resp.Header.Get("Content-Type"))` → UNUSABLE, deleting *this path's* target only — the fresh `.part` when `isCandidate` is false, the `.cand` when true. An authoritative `.part` belonging to the candidate path is never touched. (On a 206 this check has already run as Gate B and is skipped.)
2. Open `dst` with the mode above; `copyCapped(dst, resp.Body, limit)`.
3. **`readErr != nil` → INCOMPLETE immediately (C16).** No `validateFile`, no `promote`. A fresh `.part` keeps its prefix; an appended `.part` keeps this try's bytes; a `.cand` is discarded. This is the mid-stream-failure and 60s-timeout path (AC-003).
4. `writeErr != nil` or `Close() != nil` → FATAL; nothing is deleted (C4).
5. Only after a clean copy **and** a clean `Close`: `validateFile(dst, expectedTotal)`. Non-nil error → FATAL, nothing deleted. UNUSABLE → **if `preserveOnUnusable` (gated-206 path) downgrade to INCOMPLETE and delete nothing (AC-009/T1)**; otherwise delete `dst` only (T2: a `.cand` with outcome != Done is always deleted; an authoritative fresh `.part` on its own plain-GET path is deleted as UNUSABLE per BR4/C5). INCOMPLETE → preserve **only when `dst` is an authoritative `.part`**; a `.cand` with INCOMPLETE is deleted immediately (T2). Done → `promote(dst)`; `outcomeDone` is returned **only after `promote` returns nil** (C3).

**`plainGet(ctx, dst, isCandidate)`** — `Do`, then: transport error → `classifyTransport(parent, err)`; `isFatalStatus` → FATAL; `isRetryableStatus` → INCOMPLETE, carrying `parseRetryAfter` on 429. Both status paths `drainClose` without writing, so 5xx/429/404 error pages never reach disk. Otherwise → `ingest(resp, dst, isCandidate, -1, resp.ContentLength, false)` (200: `expectedTotal = resp.ContentLength`, `<0` unknown — T3).

**`rangeGet(partSize)`** — issues the Range request, then dispatches in this order (C13):
1. Transport error → `classifyTransport(parent, err)`.
2. `isFatalStatus` → FATAL; `isRetryableStatus` → INCOMPLETE with `Retry-After` on 429. **Identical to `plainGet`; no candidate fallback** — a 500 or 404 says nothing about `.part` validity. Body `drainClose`d, `.part` untouched, one request.
3. Otherwise switch on status:
   - **206** — three gates, no bytes written until all pass:
     - **Gate A (identity/parse, C9):** `Content-Range` present and parseable, `start == partSize`, `total > 0`, `end >= start`. A missing header, `*` complete-length, or `total <= 0` fails identically to unparseable. **Failure →** no append, `.part` byte-identical, `drainClose`, then **within this same try** `plainGet(candPath, true)` (AC-011) — the only 206 path costing two requests.
     - **Gate B (type):** `isHTMLType(Content-Type)` → no append, `.part` untouched, `drainClose`, INCOMPLETE (AC-010).
     - **Gate C (consistency):** `end+1 <= total` → else no append, `.part` untouched, `drainClose`, INCOMPLETE (AC-010).
     - All pass → `ingest(resp, partPath, false, total-partSize, total, true)` with `appended = true` in the result (206: `expectedTotal` = gated complete-length — T3). The cap derives from the declared complete-length (AC-008), not an invented bound. A would-be UNUSABLE verdict is **downgraded to INCOMPLETE inside `ingest`** (preserveOnUnusable), so `.part` including this try's bytes is preserved (AC-009/T1) — the downgrade happens BEFORE any `os.Remove`.
   - **200** (Range ignored) → **`ingest(resp, candPath, true, -1, resp.ContentLength, false)` on the response already in hand — one request (C12, T3).** A valid candidate is promoted and `promote` removes the old `.part`; an invalid candidate is deleted immediately, `.part` untouched, try still consumed (AC-012).
   - **416** → `drainClose`, then `parseUnsatisfiedRange`. `partSize == total` → `validateFile(partPath, total)` and `promote`, **no further request** (AC-013). Mismatch or unparseable → `plainGet(candPath, true)` in the same try (two requests).

**`promote(src)`** — `os.Rename(src, finalPath)`; a rename failure is the only error it returns. On success it removes any stray sibling `.part`/`.cand` **best-effort**: a removal failure is logged to stderr and ignored, and the try still resolves `outcomeDone` with the final file present (C14).

**`pkg/download/validate.go`** (new, ~150 lines) — the predicates above.

**`pkg/download/sweep.go`** (new, ~40 lines).

**`pkg/download/downloader.go`** (modified):
- `Downloader` gains `httpClient *http.Client`, `sleep sleeper`, `jitter func() float64`, defaulted in `New`. Package var `downloadHTTPClient` (`:48`) removed.
- `downloadFile` becomes a wrapper constructing a `fetcher`, binding `beforeAttempt: d.dlLimiter.Wait` (wrapped to translate cancellation into `errCancelled`). The single pre-loop `dlLimiter.Wait` at `:185` is removed — one token per try.
- `alreadyDownloaded` (`:259`) excludes `.part`, `.cand`, `.tmp`.
- `worker` (`:299`) suppresses `AddFailure` when `errors.Is(err, errCancelled)` (C7).
- `downloadPhotosFromPages` takes `allowIDAbsentSweep` (C1), records IDs at enqueue (C10), tracks `allPagesOK`, and **after `g.Wait()` returns** calls `sweepStaleParts(d.OutDir, seenIDs)` only when `allowIDAbsentSweep && allPagesOK && ctx.Err() == nil`. Cancellation no longer returns `Stats{}` (`:395`); partial counts are always reported.
- Call sites: `DownloadByUser` photoset loop (`:465`) → `true`; orphan batch (`:545`) → `false`; `DownloadByPhotoset` (`:592`) → `true`; `DownloadPhoto` (`:631`) calls `worker` directly, never sweeps.

**Unchanged:** `plan.go`, `pkg/api`, `pkg/ui`, `cmd/flickrdownloader`.

## Migration plan

1. **Make the HTTP client injectable** — move `downloadHTTPClient` (`:48`) into a `Downloader` field with the identical `&http.Client{Timeout: 60 * time.Second}` default. Mechanical; unblocks all later tests.
2. **Add `validate.go` + unit tests.** Pure, no callers yet; lands green independently.
3. **Add `fetch.go` with `ingest` + `plainGet` only**, replacing `downloadFile`'s body (`:184-254`), writing to `.part` (never truncating between tries) and consuming a limiter token per try.
4. **Update `alreadyDownloaded`** — must land with or before step 3, or a `.part` from an interrupted run masquerades as complete (the `photoID+".*"` glob at `:260` matches `123.jpg.part`).
5. **Add `rangeGet`** (206 gating, candidate flow, 416). No flag; active whenever a non-empty `.part` exists.
6. **Add `sweep.go`**, the `allowIDAbsentSweep` parameter, and the four call-site updates.
7. **Reporting** — cancellation suppression in `worker`, partial-count preservation at `:395`.

**Legacy `.tmp`** files are never read, resumed, or swept; their photos re-download normally. Release-note only.

**Rollback:** all steps additive except 3 and 6 (the latter a signature change). Reverting leaves no on-disk state the old code mishandles — old `downloadFile` ignores `.part` entirely.

## Testing strategy

Three layers, `go test ./...` native, no new dependencies. Gates per `.ai/config.yaml`: `go build ./...`, `go vet ./...`, `go test ./...`.

**Layer 1 — `validate_test.go`, table-driven:**

| AC | Test |
|---|---|
| AC-001 | `backoffFor(1,·)`/`backoffFor(2,·)` at jitter 0 and 0.2 → exactly 2s/2.4s and 4s/4.8s. `parseRetryAfter`: delta-seconds, HTTP-date, garbage, negative, 3600 → capped 60s. |
| AC-002, AC-015 | `isRetryableStatus`/`isFatalStatus` over 400,401,403,404,408,429,500,502,503; **both false for 416** (C3). |
| AC-003, AC-015 | `classifyTransport`: synthetic timeout + clean parent → INCOMPLETE; same error + cancelled parent → FATAL wrapping `errCancelled`. |
| AC-004 | Every status reaches exactly one branch; 416 reaches neither predicate. |
| AC-005, AC-006, AC-014 | `validateFile` over total = -1 / 0 / N: size 0 → UNUSABLE in **all three**; nonzero < N → INCOMPLETE; **nonzero > N → UNUSABLE** (AC-006); total -1 → size check skipped. Unreadable path → non-nil error (C4). |
| AC-013 | `parseUnsatisfiedRange`: `bytes */300`, `bytes */*`, missing, malformed. |
| AC-016 | `sniffHTML`: leading `\n\t `, UTF-8 BOM, `<!DOCTYPE html>`, `<html`, JPEG magic, <512B file, empty file. |
| BR4 | `isHTMLType`: `text/html`, `text/html; charset=utf-8`, `application/xhtml+xml`, `application/octet-stream`, `image/jpeg`, empty. |

**Layer 2 — `fetch_test.go`** against `httptest.Server` with a scripted per-request handler. Every case asserts outcome, **request count**, **sleep count**, and the on-disk state of `.part`, `.cand`, and final:

| AC / C | Scenario |
|---|---|
| AC-001, AC-002, C15 | 500,500,200 → 3 requests, **exactly 2 sleeps** (2s, 4s at jitter 0). 500,500,500 → 3 requests, **exactly 2 sleeps** (none after the last). **429 on try 3 → 0 sleeps after it**, `Retry-After` discarded. 429 + `Retry-After: 5` on try 1 → 5s sleep. |
| AC-003, C16 | **Real `http.Client{Timeout: 150ms}`**, handler writes half the body then blocks → INCOMPLETE, `.part` holds the prefix, `validateFile` **never invoked**, parent ctx never cancelled. |
| AC-007 | Seeded `.part`; 206 with matching start appends. |
| AC-010 | 206, start matches, `Content-Type: text/html` → no append, `.part` byte-identical, INCOMPLETE, **1 request**. Same for `end+1 > total`. |
| AC-011, C9 | 206 with gap/overlap start, and separately missing / `bytes 0-5/*` / unparseable `Content-Range` → no append, `.part` byte-identical, **2 requests in one try**, **1 sleep**. |
| AC-008 | Gated 206 whose body exceeds `total - partSize` → `.part` lands exactly on `total`. |
| AC-009 | Gated 206 completing the file with HTML bytes → INCOMPLETE, `.part` present **including** appended bytes. |
| AC-012, **C12** | Seeded `.part`; 200 ignoring Range → **exactly 1 request**; valid candidate promoted and old `.part` gone; invalid candidate **deleted immediately**, `.part` byte-identical, one try consumed. |
| AC-013 | 416 `bytes */N` with `partSize == N` → promoted, **1 request**. Mismatch and unparseable → **2 requests in one try**. |
| AC-014, C11 | Zero-length 200 body → UNUSABLE, file deleted. Pre-existing zero-byte `.part` → treated as no resume base, plain GET issued, **not** a failed try. |
| AC-015, **C13** | 404 → FATAL after 1 request, `.part` preserved. 408 → retried. **Range request answered 500 → 1 request, `.part` unchanged, INCOMPLETE, no candidate fallback.** **Range request answered 404 → FATAL, `.part` kept, no candidate fallback.** |
| AC-016, C5 | 200 with `Content-Type: image/jpeg` but an HTML body → header check passes, **disk sniff rejects**, not promoted, file deleted. Same on the candidate-promotion path. |
| AC-020 | Seeded valid-looking `.cand` plus a `.part` → `.cand` deleted before any request, `.part` used as resume base. |
| C4, C11 | Read-only directory (sniff/write EACCES) → FATAL, `.part` **still present**, no `os.Remove`. |
| **C14** | Rename succeeds but sibling `.part` removal fails (read-only sibling) → **`outcomeDone`, final file present, not FATAL**. Rename itself fails → FATAL, `src` still present. |
| C5, C13 | 5xx, 429, and 404 bodies never appear in `.part` or `.cand`, on both the plain and Range paths. |
| C7 | Parent cancelled during backoff, and during `beforeAttempt` → FATAL wrapping `errCancelled`, `.part` preserved. |

**Layer 3 — `downloader_test.go`** (extending the existing `t.TempDir()` style):

| AC | Test |
|---|---|
| AC-017 | Album dir with `.part`s for absent IDs → swept when `allPagesOK`; **skipped** when any `fetchPage` errored; skipped when ctx cancelled. |
| AC-018 | `allowIDAbsentSweep=false` (orphan/owner-root) → **never** sweeps even on a clean run; `DownloadPhoto` never sweeps; a `.part` beside a just-completed final file is still removed per BR7. |
| AC-019 | `alreadyDownloaded` false for a dir holding only `123.jpg.part` / `.cand` / `.tmp`; true for `123.jpg`. `downloadedPaths` unset until rename. |
| BR9 | Cancelled run → no `AddFailure` entries; summary retains partial success/skip/link counts. |
| C10 | An ID enqueued but never processed (cancelled worker) is in `seenIDs`, so its `.part` is not swept. |

## Ops

- **Worst-case per-photo latency** rises from ~180s to ~186s (3 × 60s + ~2s + ~4s backoff). Only the AC-011/AC-013 fallback paths issue two requests per try, so the absolute worst case is ~366s; the Range-ignored 200 path is unaffected (C12).
- **Peak disk** per in-flight photo can reach `partSize + fullSize` during the candidate path; bounded by worker count.
- **Rate limiting**: one `dlLimiter` token per try (not per request), preventing retry storms from bypassing the limiter.
- **Interrupted runs** now leave `.part` files that later runs resume — expected; document in README.
- The context-ignoring `time.Sleep(2 * time.Second)` at `:217` is removed; Ctrl-C during a 429 backoff is responsive.
- A best-effort sibling-cleanup failure logs one stderr line and leaves a stray `.part`, which the next run's BR8 sweep or resume attempt reclaims (C14).

## Risks

| Risk | Mitigation |
|---|---|
| **Extension change orphans a `.part`** — `ext` is re-derived per run (`:84-143`); a differently-resolved extension neither resumes nor sweeps the old `.part` (its ID is present). **Accepted known limitation.** | README + release notes. Bounded: one stale file per affected photo. |
| **Concurrent CLI instances** — AC-020's unconditional `.cand` deletion is safe only because one worker owns one photo ID per run. | Out of scope per requirements; documented as unsupported. |
| **Two requests per try** on the fallback paths doubles that try's worst-case wall time. | Bounded at 2 per try and confined to AC-011/AC-013; asserted by request-count tests; `beforeAttempt` still throttles per try. |
| **Server sends a correct `Content-Range` but wrong bytes** | Undetectable without checksums Flickr does not provide. The disk sniff catches the realistic HTML-error-page case. |
| **`io.Copy` conflating read/write errors** would misclassify a full disk as retryable, or validate a truncated body. | `copyCapped` splits them; read errors short-circuit to INCOMPLETE before validation (C16); write and `Close` errors map to FATAL. |
| **Sweep on a partially-listed album** | Triple gate `allowIDAbsentSweep && allPagesOK && ctx.Err() == nil`, evaluated only after `g.Wait()`. |
| **Cross-filesystem rename** (`EXDEV`) | Pre-existing (`:245`); `.part`/`.cand` are always siblings of the final path. |

## Affected files

| File | Change |
|---|---|
| `pkg/download/fetch.go` | **new** — `fetcher`, `outcome`, `attemptResult`, `sleeper`, `errCancelled`, try loop, `ingest`, `plainGet`, `rangeGet`, `promote`, `copyCapped`, `drainClose` |
| `pkg/download/validate.go` | **new** — status dispatch, transport classification, parsing, backoff, validation |
| `pkg/download/sweep.go` | **new** — `sweepStaleParts` |
| `pkg/download/fetch_test.go`, `validate_test.go` | **new** — layers 1–2 |
| `pkg/download/downloader.go` | modified — `:48` client field, `:184-254` rewritten, `:185` limiter moved, `:259` suffix exclusions, `:299` cancellation suppression, `:334-409` signature + sweep + `:395` partial stats, call sites `:465`/`:545`/`:592` |
| `pkg/download/downloader_test.go` | modified — layer 3 |
| `README.md` | modified — `.part` resume behavior, known limitations |
| `docs/architecture/` | **new** — ADR-001..ADR-014 |
| `docs/features/robust-downloads/design.md` | **new** — this document |

Not touched: `pkg/download/plan.go`, `pkg/api/*`, `pkg/ui/*`, `cmd/flickrdownloader/*`.

## Decisions

**ADR-001 — Per-photo logic lives in a `fetcher` value type.** BR5 is a 4-path × 3-gate matrix; binding it to `Downloader` forces every test through a stubbed `api.Client`. Rejected: in-place growth, full interface decomposition.

**ADR-002 — Backoff waiting is a `sleeper` func field; jitter is a separate func field.** Enables exact assertion of AC-001 durations with no wall-clock waits and no global `math/rand` stubbing.

**ADR-003 — The BR8 sweep runs after `g.Wait()` at end of album, not at startup.** `plan.go` has no plan phase; AC-017 constrains only *where* and *under what condition*. A startup pass costs a second full listing under a ~1 req/sec limiter.

**ADR-004 — Cancellation is detected via the parent `ctx` only, checked first.** `classifyTransport` receives *only* the parent context handed to `run`, so a child deadline with a clean parent is INCOMPLETE (AC-003) while a cancelled parent is FATAL (AC-015). All cancel paths wrap `errCancelled`, tested with `errors.Is`.

**ADR-005 — The 60s timeout stays on `http.Client.Timeout`, per attempt.** AC-003 requires it unchanged; only its location moves from package var to `Downloader` field.

**ADR-006 — Read-side and write-side copy errors are separated.** `io.Copy` returns one error for both; BR1/BR3 need network → INCOMPLETE and filesystem → FATAL. `copyCapped` also enforces the AC-008 cap in the same pass.

**ADR-007 — Sweep eligibility is an explicit caller-supplied flag.** `downloadPhotosFromPages` drives both the album and orphan batches; `allowIDAbsentSweep` is passed literally by each call site. Rejected: inferring album-ness from `d.OutDir`.

**ADR-008 — 416 is dispatched by an explicit branch, never through the status predicates.** BR1/AC-004 make 416 a signal, not an outcome. Relatedly, `outcomeDone` is returned only after `promote` returns nil, so a rename failure is never reported as success.

**ADR-009 — `validateFile` and `promote` return errors distinct from their outcome.** "Provably wrong content" (UNUSABLE → delete) and "could not determine" (filesystem failure → FATAL → touch nothing) differ. Conflating them would delete a resumable `.part` on transient EACCES — the `os.Remove`-on-error reflex the old `downloadFile` had at `:241`/`:247`.

**ADR-010 — A single length sentinel: `< 0` unknown, `== 0` declared-empty.** `http.Response.ContentLength` already encodes absence as -1; coercing to 0 would collide with a declared empty body and silently skip AC-005's "unknown → skip" branch. The 206 gate additionally requires `total > 0`, so `*` or non-positive lengths fail identity and route to the AC-011 fallback rather than inventing a cap.

**ADR-011 — Response handling is extracted into `ingest`, which issues no requests.** A Range-ignored 200 already holds the full body; delegating to `plainGet` would spend a second `Do` and race two responses. `plainGet` becomes `Do + ingest`, and `rangeGet`'s 200 branch is `ingest` alone — one request. Only AC-011 and AC-013 legitimately cost two requests per try, and only because their first response cannot serve as a continuation at all.

**ADR-012 — `rangeGet` classifies transport and status failures identically to `plainGet`, with no candidate fallback.** A 500, 429, or 404 answering a Range request says nothing about whether `.part` is a valid continuation; falling back to a candidate would spend a second request and risk discarding a good `.part`. The candidate fallback is reserved for responses that are *specifically* unusable as continuations (Gate-A 206 failures, mismatched 416). Every body not passed to `ingest` is `drainClose`d so connections return to the pool.

**ADR-013 — `promote`'s BR7 sibling cleanup is best-effort.** Once the rename succeeds the final file exists and is correct; converting a subsequent `os.Remove` failure into FATAL would re-download a complete photo and mark it failed. The failure is logged, and the stray file is reclaimed by the next run's sweep or resume.

**ADR-014 — A read-side copy error short-circuits to INCOMPLETE without validation.** BR1 defines a short read or mid-stream failure as INCOMPLETE by construction. Running `validateFile` on a knowingly truncated file invites a size or sniff verdict of UNUSABLE, which would delete exactly the `.part` the next try must resume from. Validation runs only after a clean copy and a clean `Close`.

**Open questions for the user: none.** All sixteen Debater challenges across two rounds resolved in-loop against the approved requirements; no business decision was reached.
