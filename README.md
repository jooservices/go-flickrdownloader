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
- Resumable — already-downloaded files are skipped on re-run, regardless of file extension
- Cross-album dedupe — a photo present in several albums is downloaded once and hardlinked into the others
- Enforces Flickr's **3600 requests/hour API quota across runs** — a persisted rolling window per API key pauses requests at the cap and shows usage, reset time and an API-cost estimate for each plan
- Handles both photos and videos, always fetching the original quality available
- Live progress bar with ETA and transfer speed
- Actionable error messages — Flickr error codes come with hints telling you what to do
- Self-update via `flickrdownloader update` and shell completions for bash/zsh/fish/powershell

## Requirements

- A free Flickr API key: https://www.flickr.com/services/apps/create/apply/
- Go 1.25+ only if you're building from source (not needed for the prebuilt binaries below)

## Install

### Prebuilt binary (recommended)

Download the archive for your platform from the [Releases page](https://github.com/jooservices/flickrdownloader/releases/latest), extract it, and put the `flickrdownloader` binary somewhere on your `PATH`.

```bash
# Example: macOS (Apple Silicon)
tar -xzf flickrdownloader_v1.0.0_darwin_arm64.tar.gz
sudo mv flickrdownloader /usr/local/bin/
```

Binaries are provided for macOS (amd64/arm64), Linux (amd64/arm64), and Windows (amd64).

### Build from source

```bash
git clone https://github.com/jooservices/flickrdownloader.git
cd flickrdownloader
go build -o flickrdownloader ./cmd/flickrdownloader
```

Or install directly with Go:

```bash
go install github.com/jooservices/flickrdownloader/cmd/flickrdownloader@latest
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

Credentials are saved to `~/.config/flickrdownloader/config.json` (readable only by your user) and reused automatically on every subsequent run — you only need to do this once.

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

Re-running `download` against the same target skips any file that's already on disk, so an interrupted or repeated run picks up where it left off.

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
go build -ldflags "-X main.version=v1.1.0" -o flickrdownloader ./cmd/flickrdownloader
```

## Robust downloads

Interrupted or timed-out downloads leave a `{photo-id}.{ext}.part` file on disk. A later run resumes with an HTTP Range request from byte N (the current `.part` size). The HTTP client keeps its **60s** full-request timeout; a timeout mid-body is treated as incomplete and preserves the bytes already written.

Each photo gets at most **3 tries** (1 initial + 2 retries). Backoff is **2s** before try 2 and **4s** before try 3 (plus a small jitter). HTTP 429 may use `Retry-After` instead (capped at 60s).

After a **fully listed** per-album download, an ID-absent sweep removes stale `.part` / `.cand` files whose photo ID was not in that album’s listing. The sweep runs **only** inside those per-album directories, and **never** outside them (for example, uncategorized / owner-root batches are not swept).

### Known limitations

- If Flickr later serves the same photo under a **different extension**, an old `{id}.{old-ext}.part` is not automatically tied to the new final path and can be left behind until a qualifying album sweep (or manual cleanup).
- **Concurrent CLI instances writing into the same output tree** are unsupported (candidate cleanup assumes one worker owns a photo ID per run). This is separate from the API quota, which *is* safely shared across concurrent runs — see [API quota](#api-quota).
- Legacy `{id}.{ext}.tmp` files from older versions are **never resumed**; they are ignored by skip detection and left on disk.

Architecture decisions for this behavior are recorded in `docs/architecture/ADR-001.md` … `ADR-014.md` (API quota and self-update decisions are in `ADR-015.md` … `ADR-017.md`).

## License

[MIT](LICENSE)
