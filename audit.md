# flickrdownloader — Full Codebase Audit

**Date:** 2026-08-17
**Scope:** Full repository (`cmd/flickrdownloader`, `pkg/api`, `pkg/cache`, `pkg/config`, `pkg/download`, `pkg/quota`, `pkg/ui`, `pkg/update`), current `develop` branch (working tree, uncommitted changes included).
**Method:** Full manual read of every non-test `.go` file, `go vet ./...`, `go build ./...`, `go test ./... -race -cover`, `go list -m -u all` against the module graph, and a review of CI config, `.goreleaser.yaml`, and README/CHANGELOG for process gaps.

## TL;DR

This is a well-engineered codebase for its size — the `pkg/download/fetch.go` resume/retry state machine in particular is unusually rigorous (explicit outcome taxonomy, documented gates, acceptance-criteria comments). `go vet` is clean, the full suite passes under `-race`, and every direct dependency is already at its latest available version. There is **no confirmed exploitable vulnerability**. The findings below are mostly hardening opportunities, a few real correctness edge cases, one CI/release process gap, and test-coverage gaps concentrated in `cmd/main.go`.

| Category | Critical | High | Medium | Low |
|---|---|---|---|---|
| Security | 0 | 0 | 2 | 2 |
| Correctness / hidden bugs | 0 | 1 | 4 | 3 |
| Performance | 0 | 0 | 2 | 3 |
| Structure / Go idiom | 0 | 0 | 2 | 4 |
| Process (CI/CD, deps, tests) | 0 | 0 | 3 | 1 |

---

## 1. Security

### 1.1 [Medium] Photo IDs and owner NSIDs from API responses are used unsanitized in filesystem paths
**Where:** `pkg/download/downloader.go:1791` (`dir := filepath.Join(d.OutDir, owner)`), `:994`, `:1009`, `:1307`, `:1641`, `:1690`, `:1704`.

`owner` (`info.Photo.Owner.NSID`) and `photo.ID` come straight from the Flickr JSON response and are concatenated into `filepath.Join` calls with no format check. Album *titles* go through `SafeName()` (`downloader.go:128`), which strips `<>:"/\\|?*` and rejects `.`/`..` — but IDs and NSIDs never go through an equivalent guard. `pkg/api/url.go:25-26` already has the right regexes (`photoIDRe`, `nsidRe`) for exactly this shape of value; they're only applied to URL parsing, not to values arriving from API responses.

In real-world use Flickr will only ever return `\d+` photo IDs and `\d+@N\d+` NSIDs, so this is not exploitable today. It becomes relevant defense-in-depth if the API response is ever tampered with (compromised CA trusted by the OS/corporate proxy, a future response-shape bug, or a bug in a self-hosted proxy some users might put in front of the Flickr API for testing). Given the tool's whole job is "take remote JSON, write files to disk based on it," validating these two fields once at the API boundary is cheap insurance.

**Fix:** validate `photo.ID` against `^\d+$` and NSIDs against the existing `nsidRe` immediately after unmarshaling in `pkg/api/flickr.go`, or add a small `isSafePathSegment` guard in `download` and call it wherever an ID/NSID is joined into a path.

### 1.2 [Medium] CDN-derived file extension is not allowlisted before use in a path
**Where:** `pkg/download/downloader.go:323-329` (`fileExt`), used at `:1009` (`photo.ID+"."+ext`).

`fileExt` takes the substring after the *last* `.` in the download URL. If that substring itself contains a `/` (e.g., a URL whose last dotted segment is followed by another path segment), the resulting "extension" carries a path separator straight into `filepath.Join(d.OutDir, photo.ID+"."+ext)`. `cleanExt` only special-cases `jpeg`/empty and strips a trailing `?query`; it does not reject slashes. Today `downloadURL` is always either `photo.URLOriginal` (Flickr's own `url_o` extra) or a `Source` field from `flickr.photos.getSizes` — both first-party — so this isn't reachable in practice, but it's the same class of trust issue as 1.1: the app never validates that these URLs point at a Flickr/staticflickr host, or that the derived extension is one of a known-safe set, before deriving a filename from them.

**Fix:** allowlist extensions to a fixed set (`jpg, jpeg, png, gif, webp, mp4, mov, m4v, avi, mkv, webm, gif`, etc.), falling back to `jpg`/`mp4` for anything else — mirrors what `cleanExt` already does for `jpeg`, just closes the open end of the switch.

### 1.3 [Low] SQL identifier built by string concatenation
**Where:** `pkg/cache/cache.go:300` — `tx.Query("PRAGMA table_info(" + table + ")")`.

`table` is always one of five hardcoded literals passed from `migrate`/`requireColumns`, never user input, so there is no actual injection path today. Still, it's the one place in the file that departs from the parameterized-query discipline used everywhere else (`?` placeholders throughout `cache.go`). Recommend a `switch` over an allowlist of known table names (or just inlining the five call sites) so a future edit can't accidentally route external input through this string concat without it looking obviously wrong.

### 1.4 [Low] Config and quota files are plaintext on disk (acceptable, but worth documenting explicitly)
**Where:** `pkg/config/config.go:113-126`, `pkg/quota/quota.go` log files.

`APISecret`, `OAuthToken`, and `OAuthSecret` are stored in `~/.config/flickrdownloader/config.json` at `0o600` inside a `0o700` directory — reasonable for a CLI tool and consistent with common practice (`gh`, `aws` CLI configs). Not a bug, but the README should say explicitly "credentials are stored in plaintext, protected only by filesystem permissions" so users on shared/multi-admin machines can make an informed call. An optional future enhancement is OS keychain integration (see §9).

**Not a finding, confirmed safe:** OAuth 1.0a signing (`pkg/api/oauth.go`) uses `crypto/rand` for the nonce (`nonce()`, panics rather than silently degrading if the CSPRNG is unavailable — correct), RFC3986-correct percent-encoding, and HMAC-SHA1 per spec. The self-update path (`pkg/update/update.go`) refuses to install without a verified SHA-256 (`expectedSHA256`, `:225-247`) — it does *not* silently fall back to size-only verification, which is exactly right. Archive extraction (`extractBinary`, `:314-361`) uses `filepath.Base(hdr.Name)` before joining into the temp dir, which closes the classic tar Zip-Slip path-traversal hole. No `os/exec`, no `text/template`/`html/template` misuse, no known-vulnerable dependency versions.

---

## 2. Correctness / Hidden Bugs

### 2.1 [High] Orphan-photo re-download after a `--refresh` verify can silently include already-in-album photos across the boundary between `VerifyUser` and `DownloadByUser`
**Where:** `pkg/download/downloader.go:1467-1481` vs `pkg/download/verify.go:223-267`.

`DownloadByUser`'s orphan-membership pass only calls `collectPhotosetMembership` for photosets **not** in `opts.Sets` (i.e., albums the user deselected in the picker or `--albums` filter) — see the `selectedIDs` skip at `downloader.go:1473-1476`. That's correct for the download path (deselected albums' photos should still count as "in an album" so they don't get pulled into "uncategorized"). But `refreshUncategorizedIDs` in `verify.go:223-267` has no equivalent concept of "selected albums" — it always computes membership from `albumIDs` built from *all* `sets` passed into `VerifyUser`, which is the full set from `cmd`'s `client.GetPhotosets`. This asymmetry is fine as long as `verify` is always called with the full album list (it is, in `runVerify`), so this is not currently a live bug — but the two code paths compute "is this photo orphaned" via two different, independently-maintained algorithms with different eligibility rules, which is a correctness trap waiting for the next feature (e.g., a future `--albums` flag on `verify`). Recommend extracting one shared `isOrphan(photoID, membership map[string]bool)` predicate/helper used by both `DownloadByUser` and `VerifyUser` rather than keeping the logic independently forked.

### 2.2 [Medium] `estimateAPICalls`/`printAPIEstimate` wall-time math double-counts the listing sleep and ignores quota wait time already elapsed
**Where:** `cmd/flickrdownloader/main.go:1053-1120`.

`pace := 1050.0 // ms per request` is hardcoded to the *default* `quota.DefaultInterval`, not `cfg.APIRateMS` (which the user can override via config, and which `newQuotaTracker` actually uses at `main.go:312`). If a user sets a custom `api_interval_ms`, the pre-flight wall-clock estimate silently becomes wrong. This is a cosmetic/UX bug (the estimate is advisory), but it's an easy fix: thread `cfg.APIRateMS` into `printAPIEstimate`/`estimateAPICalls` instead of the literal `1050.0`.

### 2.3 [Medium] Listing-page pacing sleep is not cancellable and not injectable for tests
**Where:** `pkg/download/downloader.go:1157-1158`:
```go
if page > 1 {
    time.Sleep(100 * time.Millisecond)
}
```
This runs inside the page-fetch goroutine of `downloadPhotosFromPages`, ahead of the `ctx.Done()` check on the next line. Two issues: (a) it uses the bare `time.Sleep` rather than the `d.sleep`/context-aware helper used everywhere else in this codebase (`fetcher.sleepWith`, `quota.Tracker.sleepWith`), so a cancelled run still burns up to 100ms per page before noticing; (b) because it's not injectable, no test can assert on this pacing behavior or run without it, which is presumably why nothing does. Given the API client itself already rate-limits every request through `quota.Tracker`/`rate.Limiter`, this fixed inter-page sleep looks like a legacy guard that's now partially redundant — worth confirming it's still needed, and if so, routing it through a cancellable, injectable sleep function.

### 2.4 [Medium] `IsNewer` version comparison mishandles pre-release tags
**Where:** `pkg/update/update.go:101-127`.

`parseVersion` strips everything from the first `-` or `+` onward, so `v1.2.0-rc1` parses identically to `v1.2.0`. `IsNewer("v1.2.0-rc1", "v1.2.0")` returns `false` (correctly not-newer by field comparison) but so does `IsNewer("v1.2.0", "v1.2.0-rc1")` — a pre-release would appear equal-to, not older-than, a real release, and the tool would treat a later pre-release tag as "up to date" against a real release with the same numeric triple. Low real-world impact today since this repo doesn't appear to publish pre-release tags, but if it ever does, `flickrdownloader update` will make silently wrong decisions rather than erroring. Worth a comment noting the limitation at minimum, or treating any non-empty pre-release suffix as "older" explicitly.

### 2.5 [Medium] `main.go`'s signal handler is never torn down
**Where:** `cmd/flickrdownloader/main.go:663-669`.

`signal.Notify(sigCh, ...)` registers a process-wide signal handler with no matching `defer signal.Stop(sigCh)`. Harmless in `runDownload` today because the process exits shortly after, but it's a leaked global registration that would matter if `runDownload`'s logic were ever reused as a library call or invoked repeatedly in the same process (e.g., a hypothetical daemon/watch mode). Cheap to add `defer signal.Stop(sigCh)` right after `Notify`.

### 2.6 [Low] `formatCount` recomputes comma grouping with an off-by-nothing but fragile loop
**Where:** `cmd/flickrdownloader/main.go:823-834`. Correct for the tested cases, but the `for i := len(s) - 3; i > 0; i -= 3` idiom is easy to get wrong on the next edit (e.g., negative numbers, which are handled separately above it via the `sign` extraction — fine as-is, just flagging as a spot that deserves a table-driven test with more edge cases like `999`, `1000`, `-1`).

### 2.7 [Low] `columnExists` re-runs a `PRAGMA table_info` query per column per table on every `Open()`
**Where:** `pkg/cache/cache.go:214-224`, `299-317`. Functionally fine (migrations run once at startup, and SQLite is local), but it's N×M query round-trips where one `PRAGMA table_info(table)` scan per *table* (not per column) would do — minor, see §3 Performance.

### 2.8 [Low] `matchSets` builds the `names` slice for the error message even when there's no error
**Where:** `cmd/flickrdownloader/main.go:558-561`. `names` is built unconditionally before the loop that might not error at all — trivial (album counts are small), not worth fixing on its own, bundled here because it's adjacent to real logic worth another look during any touch of this function.

---

## 3. Performance

### 3.1 [Medium] `flock`-guarded quota log is re-read from disk on every single API request
**Where:** `pkg/quota/quota.go:150-190` (`tryRecord` → `rawLoadLocked`), called once per `Wait()`.

This is a deliberate, documented tradeoff (see the package doc comment, `quota.go:1-15`) to keep the hourly cap correct across concurrent processes, and at ~1 req/sec it's genuinely cheap. Flagging only because it's the one place per-request I/O scales with API call volume rather than wall-clock time — if a future change ever raises the effective request rate (e.g., a batched/parallel metadata-prefetch feature), this file-read-per-request pattern would need revisiting first. No action needed today.

### 3.2 [Medium] `indexLocalFiles` does a full recursive directory walk (with a `Stat` + partial-read HTML-sniff per file via `completeLocalFile`) once per owner root, but is re-triggered on every `downloadByPhotoset` call for photo-only/photoset-only CLI invocations
**Where:** `pkg/download/downloader.go:411-485`, called from `DownloadByUser:1311`, `downloadByPhotoset:1642/1712`, `downloadPhoto:1795`.

Each call is properly memoized per-root via `d.indexedRoots` (`:417-421`), so a single run never re-walks the same directory twice — this is not a live bug. It's listed here because `completeLocalFile` (`:391-404`) does a `Stat` and, for anything ≤8KB or with an `html`/`htm`/`xml` extension, opens and sniffs the file (`validateFile` → `sniffHTML`, reads up to 512 bytes) for *every* file under the root. For a user with tens of thousands of previously-downloaded photos, that's tens of thousands of `open+read+close` syscalls at the start of every run before any network I/O happens. Given essentially all real photo/video files are well over 8KB, the fast path (`info.Size() > htmlSniffSizeLimit && ext not html/htm/xml` → `true`) already avoids the sniff for the overwhelming majority of files — this is a non-issue in practice, downgraded to informational.

### 3.3 [Low] `Client.GetPhotosets` accumulates unbounded pages into a single slice with no expected-size hint
**Where:** `pkg/api/flickr.go:642-669`. `var all []PhotoSetInfo` grows via repeated `append` with no pre-allocation, even though `resp.Photosets.Total` is known after the first page. Cheap fix (`all = make([]PhotoSetInfo, 0, resp.Photosets.Total)` after the first response), minor allocation-count win for users with hundreds of albums.

### 3.4 [Low] `Breakdown()`/table rendering rebuilds width arrays and reformats every cell with `fmt.Sprintf` per render
**Where:** `pkg/ui/ui.go:318-426`. Only called once at the very end of a run, on an at-most-a-few-hundred-row table — not worth optimizing, noted only for completeness.

### 3.5 [Low] `newDownloadHTTPClient` clones `http.DefaultTransport` but does not set `ForceAttemptHTTP2` explicitly or tune `IdleConnTimeout`
**Where:** `pkg/download/downloader.go:163-175`. Inherits Go's defaults (`IdleConnTimeout: 90s`), which are reasonable; flagging only as a tuning knob if download throughput to `live.staticflickr.com` is ever benchmarked and found wanting (e.g. explicit `MaxIdleConnsPerHost` tuning already done at `:172` is good, but `ResponseHeaderTimeout` is unset, so a hung server response header could block a worker for up to the full 60s client `Timeout` before the retry logic kicks in).

---

## 4. Code Structure, Principles, Design Patterns, Go Idiom

Overall assessment: **strong**. Package boundaries are clean (`api` → REST/OAuth, `cache` → persistence, `config` → settings, `download` → orchestration, `quota` → rate budget, `ui` → rendering, `update` → self-update), each with a clear single responsibility and minimal circular coupling (`download` depends on `api`, `cache`, `ui`; nothing depends back on `download`). Interfaces are used specifically where they earn their keep — `RateLimiter`, `ResponseCache`, `PhotosetStatusStore`, `PhotoMetadataStore` in `pkg/api` and `pkg/download` are small, consumer-defined interfaces (Go idiom: accept interfaces, return structs), and the optional-capability pattern (`responseCacheMethodWriter`, `responseCacheLease`, `photosetStatusBulkStore` — type-asserted with `, ok` at the call site) is a clean way to let `*cache.Store` opt into richer behavior without forcing every `ResponseCache` implementation (including test doubles) to implement it. That's effectively Go's version of optional-interface/duck-typed capability detection, applied correctly.

### 4.1 [Medium] `Downloader` struct is a god-object accumulating orchestration, indexing, progress, and stats state
**Where:** `pkg/download/downloader.go:79-124`. 25 fields spanning five distinct concerns: (1) config (`Client`, `OutDir`, `NumWorkers`), (2) persistence-store handles (`statusStore`, `metadataStore`), (3) local-file dedupe index (`downloadedPaths`, `localByDir`, `indexedRoots`, `pathMu`), (4) live progress/status (`progress`, `currPage`, `totalPages`, `currAlbum`, `activeFile`, `activeMu`), (5) per-album result accumulation (`albumStats`, `statsMu`). It works, and Go doesn't punish this the way a language with stricter encapsulation would, but each concern above is independently testable and has its own mutex already — they're natural candidates for extraction into `localFileIndex`, `progressTracker`, and `albumStatsCollector` types owned by `Downloader` rather than flattened into it. This would also make `pkg/download/downloader.go`'s current 1825 lines (by far the largest file in the repo — `wc -l` shows it dwarfing every other file) more navigable; consider splitting along those same lines into `downloader.go` (orchestration), `localindex.go`, and `progress.go`/`stats.go` regardless of whether the struct itself is decomposed.

### 4.2 [Medium] Repeated three-way "loaded from bulk snapshot vs. single lookup vs. nil" branching pattern
**Where:** appears near-identically at `downloader.go:1341-1345`, `1484-1488`, and `verify.go:65-72`, `146-151`:
```go
var status *cache.PhotosetStatus
if statusesLoaded {
    status = statuses[set.ID]
} else {
    status = d.photosetStatus(ctx, d.rootDir, userID, set.ID)
}
```
This exact shape recurs 4 times across two files. A single helper — e.g. `func (d *Downloader) lookupStatus(ctx, statuses map[string]*cache.PhotosetStatus, loaded bool, ownerNSID, id string) *cache.PhotosetStatus` — would remove the duplication and make the "bulk-load with per-ID fallback" policy a single source of truth instead of four independently-maintained copies (this is the same category of risk as finding 2.1: duplicated decision logic drifting apart over time).

### 4.3 [Low] Error wrapping is inconsistent between `%w` and string-embedded raw response bodies
**Where:** e.g. `pkg/api/flickr.go:653,657,660` use `%w — raw: %s` (embeds the raw JSON body as a plain string, which defeats `errors.Is`/`errors.As` matching on anything past the wrapped error, and can dump a large/sensitive-looking blob into terminal output on failure) versus the cleaner `fmt.Errorf("...: %w", err)` used elsewhere (e.g. `GetSizes` at `:594`). Worth standardizing: either always include raw bodies (useful for debugging Flickr API weirdness) behind a `-v`/debug flag, or drop them from user-facing error text and rely on structured logging.

### 4.4 [Low] Package-level mutable globals for CLI flags
**Where:** `cmd/flickrdownloader/main.go:58-73`. Standard (if slightly dated) Cobra idiom — not wrong, but modern Cobra usage tends to scope flag variables to a local struct bound via closures, which also makes `runDownload` etc. unit-testable without needing package-level state. Given `main.go`'s test coverage is the lowest in the repo (7.4%, see §6), this structural choice is part of why: functions like `runDownload` can't be called from a test without also manipulating package globals.

### 4.5 [Low] `bindAccountCache`/`openConfiguredCache` mix I/O, config loading, and cache binding in one function with no separation between "can fail softly" and "must fail hard" call sites
**Where:** `cmd/flickrdownloader/main.go:357-385`, called differently by `runDownload` (soft-fail, prints and continues) vs `runVerify`/`runCachePrune` (hard-fail, returns error) vs `runAuth` (soft-fail). The policy is correct at each call site, but it's implemented by each caller re-deciding what to do with the same error rather than the helper exposing e.g. a `strict bool` parameter or two named wrapper functions (`openConfiguredCacheOrNil` / `openConfiguredCacheRequired`) that make the two policies discoverable from the function signature.

### 4.6 [Low] Naming: `Client.SetSignedGet` / `signedGetFunc` predates `signedGetContextFunc` and is now dead weight for production code paths
**Where:** `pkg/api/flickr.go:73-74, 91-92, 111-114, 345-350`. `apiGet`'s dispatch (`:343-351`) checks `signedGet` first, then `signedGetContext`, then falls back to the package-level `SignedGetContext`. Grepping usage shows `SetSignedGet` exists only for tests to stub the non-context transport. That's a legitimate testing seam, but the three-way fallback chain in production code paths is more indirection than the feature needs — worth a comment at minimum noting `signedGet` is a test-only seam, or renaming it to make that explicit (`testSignedGet`).

---

## 5. Test Coverage

`go test ./... -race -cover` — all packages pass under the race detector (no data races found), coverage by package:

| Package | Coverage | Note |
|---|---|---|
| `cmd/flickrdownloader` | **7.4%** | Only `matchSets`, `lockOutputRoot`, `formatCount`, `bindAccountCache` are unit-tested. `runDownload`, `runUserDownload`, `runAuth`, `runVerify`, `printPlan`, `estimateAPICalls`, `printAPIEstimate`, `buildPickerItems` are effectively untested at the unit level (see §4.4 — package-global flags make this harder than it should be). |
| `pkg/ui` | 20.3% | The interactive `picker.go` (raw-mode key handling, redraw) is inherently hard to unit test, which likely explains most of the gap; `Breakdown`/`Render`/`Summary` formatting logic is more testable and worth prioritizing. |
| `pkg/api` | 42.1% | |
| `pkg/update` | 46.2% | |
| `pkg/quota` | 60.8% | |
| `pkg/cache` | 61.3% | |
| `pkg/download` | 61.7% | Largest and most complex package; reasonable given size, but the god-object noted in §4.1 makes some paths (e.g. `DownloadByUser`'s orphan-handling branches) hard to isolate in tests — decomposition would likely raise this incidentally. |
| `pkg/config` | 68.5% | |

**Recommendation:** prioritize `cmd/flickrdownloader` next — it's the thinnest layer by coverage and the one most likely to regress silently (it's pure orchestration glue with no compiler-enforced contracts holding it together). Extracting `estimateAPICalls`/`printAPIEstimate`'s pace constant fix (§2.2) is a good forcing function to also add tests for that function while touching it.

---

## 6. Dependencies

`go.mod` requires Go 1.25.0; the toolchain in this environment is 1.26.6 — no `toolchain` directive pin issue, builds clean either way.

**Direct dependencies — all at latest available version** (verified via `go list -m -versions` against each): `github.com/gofrs/flock v0.13.0`, `github.com/spf13/cobra v1.10.2`, `golang.org/x/sync v0.22.0`, `golang.org/x/term v0.45.0`, `golang.org/x/time v0.15.0`, `modernc.org/sqlite v1.56.0`. Nothing to bump.

**Indirect dependencies with newer versions available:**

| Module | Current | Latest | Action |
|---|---|---|---|
| `modernc.org/libc` | v1.74.4 | v1.75.3 | Low priority — pure-Go runtime shim for `modernc.org/sqlite`, no known CVEs; bump via `go get -u modernc.org/libc && go mod tidy` next time you touch go.mod. |
| `modernc.org/memory` | v1.11.0 | v1.12.0 | Same as above. |

No vulnerability scanner (`govulncheck`, `staticcheck`, `golangci-lint`) is installed in this environment, so this audit could not run one directly — **strongly recommend adding `govulncheck` to CI** (see §7) rather than relying on periodic manual checks; it's the single highest-leverage addition given every dependency version already checks out clean.

**Dependency choice quality note (positive):** `modernc.org/sqlite` is a pure-Go SQLite implementation (no cgo), which is why `.goreleaser.yaml` can set `CGO_ENABLED=0` and cross-compile cleanly for all target platforms without a C toolchain per-arch — a deliberate and correct choice for a self-distributing CLI tool.

---

## 7. CI/CD & Release Process Gaps

- **No release automation.** `.goreleaser.yaml` exists and is well-formed (checksummed `tar.gz` archives per OS/arch, matching what `pkg/update/update.go`'s `AssetFor`/checksum-verification logic expects), but `.github/workflows/` contains only `test.yml` (vet + `go test -race` on push/PR to `main`/`master`). There is no `release.yml` that runs `goreleaser release` on a tag push. Right now a maintainer must run GoReleaser locally (with `GITHUB_TOKEN` in their environment) to cut a release — worth automating with a tag-triggered workflow so releases are reproducible and don't depend on one person's local machine/credentials.
- **No dependency-update automation.** No `dependabot.yml` or Renovate config. Given §6 shows the dependency tree is currently clean, a Dependabot config for `gomod` and `github-actions` ecosystems would keep it that way without manual `go list -m -u all` audits.
- **No vulnerability scanning in CI.** Add a `govulncheck ./...` step to `test.yml` — cheap, and closes the gap noted in §6.
- **No `golangci-lint` in CI.** `go vet` is clean, but a linter (even a light config: `staticcheck`, `unused`, `errcheck`) would catch things like the inconsistent error-wrapping in §4.3 or unchecked errors automatically rather than relying on manual audits like this one.
- **CI only triggers on `main`/`master`.** The active development branch is `develop` (per `git branch`), and PRs target it via merges from feature branches; confirm `test.yml`'s `pull_request:` trigger (which has no branch filter, so it does cover PRs into `develop`) is actually catching everything — the `push: branches: [main, master]` filter means direct pushes to `develop` itself don't run CI, only PRs. Likely intentional, just worth confirming it matches the intended workflow.

---

## 8. Documentation

- README is accurate and thorough relative to the current feature set (verified commands/flags against `main.go` — `auth`, `download`, `update`, `quota`, `cache prune|clear`, `verify`, `completion` all match). `docs/features/robust-downloads/` design docs are unusually detailed for a project this size (a genuine strength — the acceptance-criteria IDs referenced in code comments like `AC-001`, `AC-017`, `BR8` trace directly back to that spec, which is excellent traceability).
- **Gap:** no explicit statement that credentials are stored in plaintext on disk (§1.4) — a one-line addition to the "Quick start" section closes this.
- **Gap:** no `CONTRIBUTING.md` — if this project expects outside contributions, a short one covering `go test -race ./...`, the AC-ID/design-doc convention visible in `pkg/download`, and the commit-message style would lower the bar for new contributors to match existing conventions rather than guessing them from code archaeology.

---

## 9. Feature Suggestions

1. **`--dry-run` for `verify`** (verify already supports `--refresh`; a dry-run-style JSON output mode would make it scriptable/machine-readable for users wanting to alert on incomplete archives via cron).
2. **Structured/JSON output mode** (`--json` on `download`/`verify`/`quota`) — the current output is nicely designed for humans (colors, tables) but nothing is scriptable today; a `--json` flag on `quota` alone would be a small, high-value addition for anyone wiring this into monitoring.
3. **`--since`/incremental-by-date filtering** — for users who re-run periodically against a growing photostream, being able to say "only check photos added/updated since date X" would reduce API usage further on top of the existing manifest-based skip logic.
4. **OS keychain integration for credentials** (macOS Keychain / Windows Credential Manager / Linux Secret Service via e.g. `zalando/go-keyring`) as an opt-in alternative to the plaintext `config.json` (§1.4) — meaningful hardening for shared-machine users without breaking the current default for everyone else.
5. **Bandwidth/rate limiting flag** (`--max-rate 10MB/s`) — `dlLimiter` already exists and scales with worker count (`downloader.go:154`); exposing a user-facing cap would help users on metered/slow connections avoid saturating their link.
6. **`flickrdownloader auth --api-key/--api-secret` non-interactive flags** — today `auth` is fully interactive (`bufio.Reader` prompts); a flag-driven path would enable scripted/first-boot provisioning (e.g., Docker images, CI-driven archival jobs) without needing a TTY.
7. **Retry/resume a specific failed photoset from the final breakdown table** — the failure summary (`ui.Summary`, grouped by error message) is good, but there's no `--retry-failed` shortcut; today the fix is "just re-run," which does work (resumable by design) but re-scans everything rather than targeting only what failed.

---

## 10. UI/UX Suggestions

1. **Quota-exceeded messaging could suggest `--offline`/cache-only mode proactively.** When `printQuotaHeader`/`renderProgressLine` show the hourly cap is nearly exhausted, a one-line hint ("re-run with `--offline` to keep working from cached data") would surface an existing feature (`--offline`) exactly when it's most useful, rather than requiring the user to already know it exists.
2. **The album picker's `/` filter has no visible "how many matched" count.** `picker.redraw()` (`pkg/ui/picker.go:98-153`) shows "(no matches)" on zero results but not "`3 of 47 albums`" while filtering — small addition, meaningfully improves orientation when filtering a long album list.
3. **Progress bar has no indication of *which phase* (listing vs. downloading vs. sizing-fallback) is currently active beyond the album name/current file.** For very slow accounts hitting the `getSizes` fallback path frequently, a subtle indicator (e.g., a different color or a `⚙` glyph when in the fallback path) would help users understand *why* a run is slower than the initial estimate predicted.
4. **`printFinalPhotosetSummary`'s "Incomplete" count doesn't point at *which* albums are incomplete** — the per-album `Breakdown()` table right above it does show failures per row, but a user has to cross-reference two tables. Consider either sorting the breakdown table to float incomplete albums to the top, or listing incomplete album names directly under "Incomplete: N".
5. **`flickrdownloader quota` output doesn't show per-key or multi-account context** — if a user has authenticated with more than one Flickr app/API key over time (each gets its own quota log per `QuotaPath`), running `quota` only ever shows the currently configured key's usage with no way to list others. Likely a niche case, but worth a one-line "quota tracked per API key; re-run `auth` with a different key to switch" hint since the persistence model already supports it silently.
6. **Colors are unconditional** — `pkg/ui` emits ANSI escape codes without checking `NO_COLOR`/`TERM=dumb` or `isatty` on stdout (only stdin TTY-ness is checked, for the picker/prompts). Piping `download` output to a file or a non-ANSI-aware log collector will embed raw escape sequences. Respecting `NO_COLOR` (https://no-color.org) and/or gating on `term.IsTerminal(int(os.Stdout.Fd()))` is a small, well-established fix.

---

## Appendix: Verification commands run

```
go version                         # go1.26.6 darwin/arm64
go vet ./...                       # clean, no output
go build ./...                     # succeeds
go test ./... -race -count=1       # all packages pass
go test ./... -cover -count=1      # coverage table in §5
go list -m -u all                  # dependency freshness, §6
go list -m -versions <module>      # per-module latest-version check, §6
```
