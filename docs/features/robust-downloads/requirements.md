# Improve Download Robustness in pkg/download (v4 — final)

## Context

`flickrdownloader` is a Go CLI that downloads Flickr albums/photos. The current `pkg/download` package has no resilience against transient network failures, no validation that a "successful" HTTP response actually contains the expected photo/video (Flickr can return HTML error/login pages with a 200 status), and no ability to resume an interrupted download — any failure or interruption forces a full re-download from byte zero. It also has an existing `alreadyDownloaded` check that globs `photoID.*` and only excludes `.tmp`, which would misclassify an in-progress partial file as complete once `.part` files are introduced.

## Goal

Make `pkg/download` resilient to transient failures and interruptions without ever silently accepting corrupt/truncated output, and without ever destroying a resumable or already-modified `.part` file before a replacement (or the file itself) is proven good — while keeping the existing dedupe/hardlink flow and cleanup logic safe.

## Business Rules

**BR1 — Outcome taxonomy.** Every download attempt resolves to one of three outcomes:
- **INCOMPLETE** — short read, mid-stream network failure, timeout, HTTP 408, a rejected 206 chunk (per BR5's gating), or a 206 append that later fails validation. The existing `.part` (including any bytes appended this attempt) is preserved; the next attempt resumes from its current offset; consumes one retry.
- **UNUSABLE** — the assembled content is provably wrong (zero-byte, HTML-typed 200 body, or a validated candidate replacing a bad `.part`). The bad file is deleted; consumes one retry. **UNUSABLE never applies to a `.part` that this same attempt's own 206 append modified** — that case is always downgraded to INCOMPLETE instead, so the file is preserved rather than deleted.
- **FATAL** — HTTP 4xx other than 408, 429, and 416; local filesystem errors; parent context cancellation. No retry. Cancellation is excluded from per-photo failure reporting — it aborts the whole run.

HTTP 416 is not itself an outcome — it's a signal handled by BR5 that resolves into one of the above.

**BR2 — Retry count and backoff.** 3 total tries = 1 initial attempt + 2 retries. Backoff of 2s before the 2nd try and 4s before the 3rd try, each with +0–20% additive jitter. HTTP 429 uses `Retry-After` (delta-seconds or HTTP-date), capped at 60s; missing/unparseable falls back to default backoff. The sleep/wait mechanism must be injectable for tests.

**BR3 — Retry classification.** Retryable (INCOMPLETE-class): transport errors except parent context cancellation, HTTP 408, HTTP 429, HTTP 5xx, and the existing flat 60-second client timeout (unchanged, preserves `.part`). FATAL (no retry): context cancellation, HTTP 4xx other than 408/429/416, local filesystem errors. HTTP 416 is handled separately per BR5.

**BR4 — Post-download validation.**
- *Zero-byte precedence*: any assembled file, resumed result, or candidate file of size 0 is unconditionally UNUSABLE, evaluated before and independent of the size-completeness check — applies whether or not a total is known.
- *Size completeness*: expected total is tracked in-memory per attempt (200's `Content-Length`, or 206's `Content-Range` complete-length). Unknown → skip this check. Nonzero and short → INCOMPLETE. For a `.part`/candidate not produced by a gated 206 append this attempt, larger-than-total is UNUSABLE, handled non-destructively via the candidate flow (BR5) when an existing `.part` is involved.
- *HTML/type detection*: base media type (parameters stripped) of `text/html`/`application/xhtml+xml` on a **200** response → UNUSABLE, delete the offending file. On a **206**, HTML typing is one of the pre-append gating checks in BR5 (not a post-hoc check) and results in INCOMPLETE without ever writing the chunk. Any other type (including `application/octet-stream` or missing header) passes the header check and is subject to sniffing the first ~512 bytes of the file **on disk** (BOM/leading-whitespace trimmed); this always runs after any successful write that completes a file, regardless of path.

**BR5 — Resume mechanics.**
- No existing `.part` (or size 0): plain `GET` (no `Range`), stream directly into a new `.part`.
- Existing `.part` with size > 0: issue `GET Range: bytes=<part-size>-`.
  - **206** — before writing anything, gate the response as a valid continuation:
    1. `Content-Range` is present and parseable, with declared start byte exactly equal to the current `.part` size (no gap, no overlap).
    2. `Content-Type` is not HTML/xhtml.
    3. The chunk, combined with the current `.part` size, does not exceed the declared complete-length (i.e., the range is internally consistent with the total).
    - If (1) fails (start mismatch) or the range is missing/unparseable: the 206 cannot serve as a continuation at all. Do not append. Leave `.part` untouched. Fall back to the plain-`GET`-into-candidate flow described below (not a simple retry of the same Range request).
    - If (1) passes but (2) or (3) fails (HTML type, or a chunk that would exceed complete-length): do not append. Leave `.part` untouched. Outcome INCOMPLETE — retry later, hoping for a well-formed response.
    - If all three pass: append the chunk to `.part`, writing at most `complete-length - partSize` bytes (safety cap even if the body is longer than expected). Then run BR4 validation on the resulting assembled file:
      - Still short of complete-length → INCOMPLETE, as usual.
      - Would otherwise be classified UNUSABLE (e.g., disk sniff now detects HTML, or an inconsistency surfaces) → downgraded to INCOMPLETE instead; `.part` (including this attempt's appended bytes) is preserved, never deleted, and retried on the next attempt.
      - Fully valid → proceed to rename (BR7).
  - **200** (server ignored `Range`): the existing `.part` is never deleted or overwritten directly. The response body is written to a disposable sibling candidate file (excluded from `alreadyDownloaded` and dedupe, never authoritative). The candidate is validated independently against its own `Content-Length`. Valid → rename candidate straight to final, delete old `.part`. Invalid (INCOMPLETE/UNUSABLE) → discard candidate, keep old `.part` untouched; the attempt's outcome still consumes a retry.
  - **416**: parse `Content-Range: bytes */<complete-length>`. `.part` size == complete-length → already complete, skip straight to BR4 validation and rename (no new request). Size mismatch either direction, or length unparseable → fall back to the plain-`GET`-into-candidate flow above, leaving `.part` untouched until a candidate validates.
- Candidate files are always disposable: a leftover candidate found on disk at the start of a new attempt is deleted without inspection, since the corresponding `.part`, if present, remains authoritative.

**BR6 — Naming and dedupe/hardlink safety.** The partial file is named `{finalPath}.part`; candidate files use a distinct, disposable suffix. `alreadyDownloaded` excludes `.part`, candidate files, and `.tmp`. `downloadedPaths` (used by dedupe/hardlink) is updated only after a successful rename to the final filename, never before.

**BR7 — Completion.** Once a `.part` or candidate passes BR4 validation, rename it to the final filename. Immediately after a successful rename, delete any stray `.part` (or candidate) still present at that sibling path.

**BR8 — Stale `.part` cleanup.** The ID-absent deletion sweep runs only inside per-album directories where every page of that album's listing succeeded this run; any page failure skips the sweep for that directory this run. Directories/albums not selected for the current run are never scanned or touched. Outside per-album directories (including owner-root), the only permitted cleanup is the narrow per-photo rule from BR7. In dry-run mode, no deletions occur anywhere.

**BR9 — Failure reporting.** After a photo exhausts its retry budget (INCOMPLETE/UNUSABLE) or hits a non-cancellation FATAL outcome, it is marked failed, its final error recorded, and the batch continues. Context-cancellation FATAL outcomes are not reported as per-photo failures.

## Scope

- `pkg/download`: retry/backoff, outcome classification (408/416 carve-outs, 206 gating), zero-byte/size/HTML validation, `.part` and candidate-file write/resume/rename logic, Range/Content-Range/416 handling.
- `alreadyDownloaded` detection and `downloadedPaths` registration timing, extended to exclude candidate files.
- Download plan generation / startup: per-album-scoped stale `.part` cleanup, page-failure-aware, dry-run respected, owner-root excluded from the ID-absent sweep.
- Reporting: per-photo failure surfacing, cancellation excluded.
- Testability: injectable sleep/clock for backoff.

## Out of Scope

- **Concurrent CLI instances** against the same output directory — unsupported, known limitation; no locking added.
- Changes to the dedupe/hardlink flow itself beyond `.part`/candidate exclusion and rename-timing ordering.
- Retry/backoff/validation for non-download operations (e.g., metadata API calls) — downloads only.

## Acceptance Criteria

- **AC-001**: A download attempt sequence performs at most 3 tries total (1 initial + 2 retries), backoff 2s before try 2 and 4s before try 3, each +0–20% jitter; 429 uses `Retry-After` (delta-seconds or HTTP-date, capped 60s), falling back to default backoff if missing/invalid.
- **AC-002**: Transport errors (except context cancellation), HTTP 408, 429, and 5xx are retryable.
- **AC-003**: The existing 60-second full-request timeout is unchanged and classified INCOMPLETE, preserving the `.part` file's already-written bytes.
- **AC-004**: Every attempt outcome is exactly one of INCOMPLETE, UNUSABLE, or FATAL; HTTP 416 is a signal, not a direct outcome.
- **AC-005**: An expected total, when determinable, is tracked in-memory only; unknown → skip the size-completeness check.
- **AC-006**: Given a known total, a nonzero file smaller than it is INCOMPLETE. Larger-than-total is handled via the candidate flow (AC-012) when it involves an existing `.part` not modified by this attempt's own 206 append.
- **AC-007**: A 206 response is appended to `.part` only if `Content-Range`'s declared start equals the current `.part` size exactly, `Content-Type` is not HTML/xhtml, and the chunk does not push the file past the declared complete-length.
- **AC-008**: A gated, valid 206 append writes at most `complete-length - partSize` bytes to `.part`, even if the response body is longer.
- **AC-009**: If a 206 append happens this attempt and subsequent validation would otherwise classify the result UNUSABLE, the outcome is downgraded to INCOMPLETE instead — `.part`, including the bytes just appended, is never deleted as a result.
- **AC-010**: If the 206's start matches but its type is HTML or its chunk would exceed complete-length, the chunk is not appended, `.part` is left untouched, and the outcome is INCOMPLETE.
- **AC-011**: If the 206's start does not match the current `.part` size, or `Content-Range` is missing/unparseable, the chunk is not appended, `.part` is left untouched, and the client falls back to the plain-`GET`-into-candidate flow (AC-012) rather than retrying the same Range request.
- **AC-012**: A 200 response (Range ignored) or a 416 requiring a fresh fetch writes into a disposable sibling candidate file rather than touching the existing `.part`. A fully valid candidate is promoted (renamed to final, old `.part` deleted); an invalid candidate is discarded and the old `.part` is retained untouched, with the attempt's outcome still consuming a retry.
- **AC-013**: On a 416 response, the client parses `Content-Range: bytes */<complete-length>`. `.part` size == complete-length → validate and rename directly, no new request. Otherwise → fall back to AC-012's candidate flow.
- **AC-014**: A file of size 0 — original `.part`, resumed result, or candidate — is always UNUSABLE, checked before and independent of the size-completeness comparison, regardless of whether a total is known.
- **AC-015**: FATAL applies to HTTP 4xx other than 408, 429, and 416, plus local filesystem errors and context cancellation. HTTP 408 is INCOMPLETE.
- **AC-016**: After any successful write that completes a file to its full expected size — via direct download, gated resume, or candidate promotion — the on-disk content sniff (first ~512 bytes) still runs before promotion to final.
- **AC-017**: The ID-absent `.part` deletion sweep runs only inside per-album directories where every page of that album's listing succeeded this run; any page failure skips the sweep for that directory this run.
- **AC-018**: Outside per-album directories (including owner-root), the only permitted `.part` cleanup is deleting a `.part` immediately beside that exact photo's just-completed final file.
- **AC-019**: `alreadyDownloaded` detection excludes `.part`, candidate files, and `.tmp`. `downloadedPaths` is updated only after a successful rename to final.
- **AC-020**: A leftover candidate file found on disk at the start of a new attempt for that photo is deleted without inspection; the corresponding `.part`, if present, remains authoritative.

## Open Questions

None outstanding. All business decisions were explicitly approved by the user; all 9 round-1, 6 round-2, and the round-3 final-blocker Debater challenges were resolved within the consensus loop per the mappings above.

## Affected Areas

- `pkg/download` (retry, backoff, outcome classification, validation, `.part`/candidate/resume/rename logic, Range/Content-Range/416/408 handling, 206 continuation gating, injectable sleep for tests)
- `alreadyDownloaded` / `downloadedPaths` logic (exclude `.part` and candidate files, defer registration until after rename)
- Download plan generation (per-album-scoped, page-failure-aware stale `.part` cleanup, owner-root excluded, dry-run respected)
- Dedupe/hardlink flow (integration point only — no logic change beyond exclusion and timing)
- CLI output/reporting (per-photo failure surfacing, cancellation exclusion)
