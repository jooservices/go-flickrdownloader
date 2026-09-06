# Flickr Downloader

A fast, concurrent command-line tool to download all photos and videos from a Flickr user, album, or single photo — organized automatically into per-album folders on disk.

```
$ flickrdownloader download -u https://www.flickr.com/photos/someuser/

╔══════════════════════╗
║  Flickr Downloader    ║
╚══════════════════════╝

  Type:   User
  NSID:   123456789@N01
  Photosets: 12
  Photos: 842

  Downloading...

  [━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━▶──]  92%  774/842  eta 8s  4.1 MB/s
```

## Features

- Download an entire Flickr photostream, a single album, or a single photo/video from just its URL
- Interactive album picker — when downloading a user in a terminal, select exactly which albums you want (arrow keys, space to toggle, `/` to filter)
- Automatically discovers and mirrors a user's album structure on disk (photos not in any album land in an "uncategorized" folder)
- `--dry-run` preview — album tree, photo counts and estimated total size before anything is downloaded
- Concurrent downloads with a configurable worker pool
- Resumable — already-downloaded files are skipped on re-run, regardless of file extension. Failed photos print the reason, are logged in `.flickrdownloader.failures.jsonl` under the output root, and are retried first on the next `download` / `watch` start
- **`scan`** — index an existing download tree into completion manifests (for archives made before 1.3.0)
- Completion manifests — unchanged albums skip Flickr listing on the next `download` (`--refresh` / `--force` to re-check)
- Cross-album dedupe — a photo present in several albums is downloaded once and hardlinked into the others
- Enforces Flickr's **3600 requests/hour API quota across runs** — a persisted rolling window per API key pauses requests at the cap and shows usage, reset time and an API-cost estimate for each plan
- Account-scoped **response cache** — repeat runs reuse cached listings instead of re-fetching them, with `--refresh` / `--offline` to control that explicitly
- **`verify`** — check downloaded photos against completion manifests without downloading or hitting the API
- **`watch`** — a long-running mode that polls a watchlist (stored in the account cache database, or a YAML file via `--file`) and downloads new photos forever, waiting out quota/rate limits instead of exiting
- **Output-root lock** — safe to run `download`/`verify`/`watch`/`scan` concurrently against different output trees; a second process against the *same* tree is rejected instead of racing
- Handles both photos and videos, always fetching the original quality available
- Live progress bar with ETA and transfer speed
- Actionable error messages — Flickr error codes come with hints telling you what to do
- Self-update via `flickrdownloader update` and shell completions for bash/zsh/fish/powershell

## Requirements

- A free Flickr API key: https://www.flickr.com/services/apps/create/apply/
- Go 1.25+ only if you're building from source (not needed for the prebuilt binaries below)

## Install

### Prebuilt binary (recommended)

Download the archive for your platform from the [Releases page](https://github.com/jooservices/go-flickrdownloader/releases/latest), extract it, and put the `flickrdownloader` binary somewhere on your `PATH`.

```bash
# Example: macOS (Apple Silicon)
tar -xzf flickrdownloader_v1.6.0_darwin_arm64.tar.gz
sudo mv flickrdownloader /usr/local/bin/
```

Binaries are provided for macOS (amd64/arm64), Linux (amd64/arm64), and Windows (amd64).

### Build from source

```bash
git clone https://github.com/jooservices/go-flickrdownloader.git
cd go-flickrdownloader
go build -o flickrdownloader ./cmd/flickrdownloader
```

Or install directly with Go:

```bash
go install github.com/jooservices/go-flickrdownloader/cmd/flickrdownloader@latest
```

## Quick start

### 1. Authenticate (one-time setup)

```bash
flickrdownloader auth
```

This walks you through:
1. Entering your Flickr API key and secret (get one [here](https://www.flickr.com/services/apps/create/apply/))
2. Opening a browser URL to authorize the app on your Flickr account
3. Pasting back the verification code Flickr shows you

Credentials are saved in **plaintext** to `~/.config/flickrdownloader/config.json` (mode `0600`, directory `0700`) and reused automatically on every subsequent run — you only need to do this once. Anyone who can read that file can use your Flickr API key and OAuth token; on a shared machine, keep the home directory private.

### 2. Download

```bash
flickrdownloader download -u <flickr-url>
```

The URL can point to a user's photostream, a specific album, or a single photo/video — the tool detects which automatically:

```bash
# All photos & albums for a user
flickrdownloader download -u https://www.flickr.com/photos/someuser/

# A single album
flickrdownloader download -u https://www.flickr.com/photos/someuser/albums/72177720123456789

# A single photo or video
flickrdownloader download -u https://www.flickr.com/photos/someuser/52968474408
```

## Flags

| Flag | Short | Default | Description |
|---|---|---|---|
| `--url` | `-u` | *(required)* | Flickr URL — user profile, album, or single photo |
| `--out` | `-o` | `./photos` | Output directory |
| `--workers` | `-w` | `20` | Number of concurrent download workers |
| `--dry-run` | | `false` | Preview album tree, counts and estimated size without downloading |
| `--yes` | `-y` | `false` | Skip the confirmation prompt and album picker (download everything) |
| `--albums` | | | Only download matching albums (comma-separated names, or `all` / `none`) |
| `--uncategorized` | | `true` | Include photos that are not in any album |
| `--refresh` | | `false` | Bypass cached metadata and completion manifests; re-check everything against Flickr |
| `--offline` | | `false` | Use cached responses only; never make a live request (fails if nothing is cached) |

```bash
flickrdownloader download -u <url> --out ~/Pictures/flickr --workers 30

# Preview first: album tree, photo counts and estimated size
flickrdownloader download -u <url> --dry-run

# Only specific albums (names match case-insensitively, exact or substring)
flickrdownloader download -u <url> --albums "wedding,honeymoon"

# Skip photos that aren't in any album
flickrdownloader download -u <url> --uncategorized=false
```

When you run a user download interactively in a terminal, an album picker appears:
navigate with `↑/↓` (or `j/k`), toggle albums with `space`, select/deselect all with
`a`/`n`, filter with `/`, then press `enter` to confirm — or `q` to cancel. Use
`--yes` to bypass the picker and the confirmation prompt.

## API quota

Flickr caps REST API usage at **3600 requests per hour** per API key. `flickrdownloader` tracks this with a *rolling* 1-hour window persisted in
`~/.config/flickrdownloader/quota-<key-hash>.log` — one timestamp per request — so **separate runs in the same hour share the same budget**.

```bash
flickrdownloader quota          # show usage + reset time for this hour
```

```text
╔═════════════╗
║  API Quota  ║
╚═════════════╝

  Used:      123 / 3600  (3%)
  Window:    oldest request expires in 42m
```

- When the cap is reached, requests pause until the oldest entry expires (the progress bar shows a `quota full — next slot in …` countdown instead of the page indicator). Ctrl-C aborts the wait cleanly.
- The header of every `download` run shows the hourly usage, and the final summary reports `API calls this run`.
- The plan/dry-run printout estimates the API cost of the job (`~247 calls: 242 listing + ~5 sizes`), whether it fits the remaining budget, and the expected wall time — including pauses when it exceeds the cap.
- Tune per key in `~/.config/flickrdownloader/config.json` (e.g. a shared key): `"api_hourly_limit": 3600`, `"api_interval_ms": 1050`.

The log is append-only, and each accepted request is written through immediately under an interprocess lock — so two runs sharing the same API key (a manual run and a cron job, say) see each other's usage in real time rather than each getting their own copy of the budget. A torn trailing line from a hard kill is ignored on the next load.

## Response cache

Every successful Flickr REST response is cached in a small, account-scoped SQLite database at `~/.config/flickrdownloader/cache-<hash>.db` (0600 permissions; separate accounts get separate databases, and re-authenticating as a different account invalidates it automatically). This is what powers `verify` and lets repeat runs against the same albums skip re-listing photos that were already fully downloaded.

- Listing responses (album/photostream pages) are cached for `cache_listing_ttl_hours` (default **24h**, configurable in `config.json`); detail responses (`getSizes`, `getInfo`) for **30 days**.
- `--refresh` / `--force` bypasses the cache **and** completion manifests for one run without clearing them.
- `--offline` serves only what's cached — no live requests at all — and fails clearly if nothing is cached yet for that call.
- `flickrdownloader cache prune` removes expired entries (completion manifests are always kept, since they describe local files, not API freshness).
- `flickrdownloader cache clear` removes everything cached for the current account.

## Verify

```bash
flickrdownloader verify -u <flickr-user-url>
```

Checks each album's local files against its saved completion manifest — no downloading, and by default no Flickr requests at all (it trusts the manifest written by the last `download`/`watch` run). States shown per album:

```
  ✓ Wedding 2024                    412/412 photos, matches disk
  ⚠ Summer Trip                     118/140 (22 missing)
  · Road Trip                       not scanned — run with --refresh for a live check
  ✗ Family Reunion                  3 local file(s) no longer match the current source

  ✓ complete   ⚠ incomplete   · not scanned   ✗ stale/error
```

Add `--refresh` to re-list every album from Flickr instead of trusting the manifest — useful after manual edits to the output directory, or as a periodic integrity check.

## Scan (seed manifests from disk)

```bash
flickrdownloader scan -u <flickr-user-url>
```

Walks the output directory and writes completion manifests so a later `download` can skip re-listing albums that still match disk. Uses `photosets.getList` (a few cheap API calls) to bind folders to album IDs and Flickr's `date_update`. It does **not** paginate each album.

Use this after upgrading from a version that had no cache, against a tree that is already downloaded:

```bash
flickrdownloader scan -u https://www.flickr.com/photos/someuser/ -o ./photos
flickrdownloader download -u https://www.flickr.com/photos/someuser/ -o ./photos -y
```

`--offline` indexes disk only (no Flickr calls). Those manifests cannot skip listing until a later run records `date_update`. If an album changed on Flickr with the same photo count (deleted 2, added 2), run `download --force` (or `--refresh`) to re-list it.

## Watch mode (continuous sync)

```bash
flickrdownloader watch add https://www.flickr.com/photos/alice/
flickrdownloader watch list
flickrdownloader watch
```

Turns `flickrdownloader` into a persistent watcher: it polls a **watchlist** of Flickr URLs on an interval, downloads anything new, and keeps running until you stop it (Ctrl-C / SIGTERM) — waiting through quota exhaustion or a Flickr-side rate limit instead of exiting.

The default watchlist is stored in the account cache database (`~/.config/flickrdownloader/cache-*.db`) and is **per Flickr account**. `watch list`, `watch add`, and `watch remove` require `flickrdownloader auth`. `cache clear` / `cache prune` do not delete it. Existing `watchlist.yaml` / `sources.txt` files are imported once on the first DB-mode watch command and renamed to `*.migrated`.

Pass `--file` to keep using a YAML or text watchlist instead of the database:

```bash
flickrdownloader watch --file ~/.config/flickrdownloader/watchlist.yaml
```

**Watchlist file** (`--file`) — YAML for per-source overrides:

```yaml
poll_interval: 30m
sources:
  - url: https://www.flickr.com/photos/alice/
    albums: all
    uncategorized: true
  - url: https://www.flickr.com/photos/bob/albums/72177720123456789
    poll_interval: 1h
```

or a plain-text fallback (`sources.txt`, one URL per line, `#` comments allowed), which inherits `albums: all` and `uncategorized: true` for every entry:

```text
https://www.flickr.com/photos/alice/
https://www.flickr.com/photos/bob/albums/72177720123456789
```

| Flag | Default | Purpose |
|---|---|---|
| `--file` | account cache database | Optional YAML/text watchlist path |
| `--poll-interval` | from the watchlist (DB or `--file`), or `30m` | Time between full cycles |
| `--out` | config default | Output directory |
| `--workers` | config default | Worker count |
| `--log-file` | — | Append log lines to this file in addition to stdout |
| `--quiet` | on automatically when not a terminal | Structured `key=value` log lines instead of the progress bar |

A per-source error is logged and skipped — one bad URL never stops the daemon. `--quiet` output is one line per event, safe to `grep`/`awk`:

```
2026-08-17T12:00:00Z INFO watch starting watchlist=db
2026-08-17T12:00:04Z INFO source alice done in 4s
```

Example systemd/launchd service files that run `watch --quiet --log-file ...` are in [`docs/watch/`](docs/watch/).

## Updating

```bash
flickrdownloader update          # check for a new release and install it
flickrdownloader update --check  # only check
flickrdownloader --version       # print the current version
```

Downloaded releases are verified against a SHA-256 checksum before being installed — either the digest GitHub computes for the asset, or a detached checksums file published alongside the release. `update` refuses to install (rather than silently skipping verification) if neither is available.

## Shell completions

```bash
# bash
source <(flickrdownloader completion bash)
# zsh
source <(flickrdownloader completion zsh)
# fish
flickrdownloader completion fish | source
# powershell
flickrdownloader completion powershell | Out-String | Invoke-Expression
```

## Output layout

```
photos/
  <owner-nsid>/
    <album name>/
      <photo-id>.jpg
      <photo-id>.mp4
    <photo-id>.jpg          # photos not in any album
```

Re-running `download` against the same target skips any file that's already on disk, so an interrupted or repeated run picks up where it left off. Albums whose completion manifest still matches disk (same Flickr `date_update`, same files) also skip the per-album listing request.

## How it works

- Talks to the Flickr REST API directly over OAuth 1.0a (no SDK dependency)
- A producer goroutine paginates the API while a pool of worker goroutines downloads files concurrently, coordinated with `errgroup`
- Uses Flickr's `url_o` extra to get the original-quality URL straight from the photo listing, avoiding an extra per-photo API call in the common case; falls back to `flickr.photos.getSizes` for videos or when the owner has disabled original downloads
- Uses Flickr's `o_dims` extra to estimate download size from original dimensions (used by `--dry-run` and the album picker)
- Photos that appear in more than one album are downloaded once and hardlinked into the other album folders
- Two independent rate limiters: one for the Flickr REST API (~1 req/sec, matching Flickr's documented quota) and one for file downloads (scales with `--workers`); a persisted per-key quota tracker additionally enforces the 3600 requests/hour cap across runs
- Ctrl-C cancels cleanly — in-flight downloads stop and the run reports partial progress

## Development

```bash
go build ./...
go vet ./...
go test ./...

# Build with a release version stamped in (used by --version and update)
go build -ldflags "-X main.version=v1.6.0" -o flickrdownloader ./cmd/flickrdownloader
```

## Robust downloads

Interrupted or timed-out downloads leave a `{photo-id}.{ext}.part` file on disk. A later run resumes with an HTTP Range request from byte N (the current `.part` size). The HTTP client keeps its **60s** full-request timeout; a timeout mid-body is treated as incomplete and preserves the bytes already written.

Each photo gets at most **3 tries** (1 initial + 2 retries). Backoff is **2s** before try 2 and **4s** before try 3 (plus a small jitter). HTTP 429 may use `Retry-After` instead (capped at 60s).

After a **fully listed** per-album download, an ID-absent sweep removes stale `.part` / `.cand` files whose photo ID was not in that album’s listing. The sweep runs **only** inside those per-album directories, and **never** outside them (for example, uncategorized / owner-root batches are not swept).

### Known limitations

- If Flickr later serves the same photo under a **different extension**, an old `{id}.{old-ext}.part` is not automatically tied to the new final path and can be left behind until a qualifying album sweep (or manual cleanup).
- **Concurrent `download`/`verify`/`watch` processes against the *same* output directory** are rejected outright by an advisory lock, rather than allowed to race — run them against separate output trees instead. Concurrent runs against *different* trees, and shared API quota tracking across runs, both work as expected.
- Legacy `{id}.{ext}.tmp` files from older versions are **never resumed**; they are ignored by skip detection and left on disk.

Architecture decisions for this behavior are recorded in `docs/architecture/ADR-001.md` … `ADR-014.md` (API quota and self-update: `ADR-015.md` … `ADR-018.md`; response cache, manifests, and watch mode: `ADR-019.md` … `ADR-024.md`).

## License

[MIT](LICENSE)
