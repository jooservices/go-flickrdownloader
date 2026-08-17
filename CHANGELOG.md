# Changelog

## [1.3.0] - 2026-08-17

### Added

- Account-scoped SQLite response cache for Flickr REST calls (`~/.config/flickrdownloader/cache-<hash>.db`, 0600), bound to a fingerprint of the current OAuth token so re-authenticating as a different account invalidates cached responses automatically.
- `--refresh` (bypass the cache for one run) and `--offline` (serve cache only, never make a live request) on `download` and `verify`; rejected together with an explicit error instead of one silently winning.
- `flickrdownloader cache prune` (remove expired response/metadata entries; completion manifests are always kept) and `flickrdownloader cache clear` (remove everything cached for the account).
- `flickrdownloader verify` — checks downloaded photos against persisted photoset completion manifests without downloading; states are `complete` / `incomplete` / `not-scanned` / `stale` / `error`, each with a symbol, color, and counts. `verify --refresh` re-lists every album from Flickr instead of trusting the manifest.
- `flickrdownloader watch` — polls a watchlist file (YAML with per-source overrides, or a plain-text URL list) on an interval and downloads anything new, forever, waiting through quota exhaustion or a Flickr-side rate limit instead of exiting. `--quiet` switches to structured, greppable `key=value` log lines (on automatically when stdout isn't a terminal); `--log-file` tees them to a file. Example systemd/launchd units are in `docs/watch/`.
- Recursive local-file indexing across the whole output tree (not just the album being downloaded), so a photo already on disk anywhere under the output root is hardlinked instead of re-downloaded when it shows up in another album or user.
- Output-root advisory lock: a second `download`/`verify`/`watch` process against the same output directory is rejected outright instead of racing manifest writes or partial downloads.
- In-process request de-duplication (singleflight) — concurrent identical REST requests collapse onto a single underlying call.
- REST-layer retry with capped exponential backoff on a Flickr-side 429/rate-limit response, unbounded in count (only context cancellation stops it) — closes the gap where a shared API key or a Flickr-side limit hits despite the proactive hourly tracker.
- `flickr.photos.getNotInSet` support, used by `verify --refresh` to check the authenticated user's uncategorized photos without paginating and diffing the full photostream.

### Fixed (security & correctness, from the pre-release audit)

- **Cache keys never contain OAuth credentials** — derived from method + non-auth params only, so a signed request's `oauth_token`/`oauth_signature` can never end up persisted in the response cache database.
- **Retry backoff overflow**: computing the exponential delay before capping it let a long-running retry loop (attempt ≥ 64 — reachable for a process designed to retry forever) overflow to a **zero-second** delay, which would have hammered an already rate-limited API. Attempt is now clamped before the shift.
- **Signal handler leak**: `download`/`verify`/`watch` now call `signal.Stop` on shutdown instead of leaving the SIGINT/SIGTERM registration and its goroutine running past the command's exit.
- **`--dry-run` API-cost estimate could drift from reality**: the wall-time estimate used a hardcoded pace constant instead of the configured `api_interval_ms`; now threaded through.
- `flickr.photosets.getList` results are validated (Flickr's numeric photo/photoset ID and NSID shape) before being used to build filesystem paths, and the returned slice is preallocated from the reported total instead of growing one page at a time.
- Extension strings used to build a download's final path are limited to alphanumeric characters as defense-in-depth against a malformed/unexpected CDN URL.
- Fixed a build-breaking bug where `pkg/ui/colors.go` (color-disabling logic for `NO_COLOR`/non-TTY output) assigned to what were declared as Go `const`s — changed to `var`s.

### UI/UX

- A typo'd `--albums` name now suggests the 3 closest matches instead of dumping the entire album list.
- `quota` output includes a legend for its green/yellow/red thresholds.
- Ctrl-C during a download now says it's waiting for in-flight downloads to finish, instead of going silent.
- The album picker footer shows a running `N/M selected` count.
- `auth`'s success message points to both `download` and `watch` as next steps.

### Known limitations / deferred to a later release

- `pkg/cache` has cross-process response-lease methods (coordinating a cache miss across *separate* `flickrdownloader` processes hitting the same key at once) and normalized per-photo metadata storage (URL/dimensions/size, for repairing an incomplete photoset without a full re-listing) — both already covered by tests, but **neither is wired into the request path yet**. Today's de-duplication is in-process (singleflight) only.
- `Client.InvalidatePhotoMetadata` (drop cached sizes/info after a CDN error proves them stale) exists but isn't yet called from the download error path.
- `watch` exits 0 on clean shutdown and non-zero on any other error; it doesn't yet distinguish a startup failure from "every source failed this cycle" as separate exit codes.
- Watchlist sources are limited to user/album/photo URLs already supported by `download` — Flickr contacts/favorites as watchlist sources are a v2 idea, not implemented.

### Tests

- New/updated: `pkg/api` (cache-hit, cache-only, cache-only-with-expired-entry, configured-listing-TTL, concurrent-singleflight, retry-then-succeed, retry-stops-on-cancel, cache-key-excludes-oauth, backoff-overflow-regression), `pkg/watch` (watchlist YAML/text parsing, scheduler cycle/error-isolation/prompt-shutdown — previously untested), `pkg/download` (local file indexing, missing-ID detection, manifest size-integrity check, manifest round-trip).
- `go build ./...`, `go vet ./...`, `gofmt -l .` clean.
- `go test ./...` and `go test -race ./pkg/api/... ./pkg/download/... ./pkg/watch/...` pass.
