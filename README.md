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
- Automatically discovers and mirrors a user's album structure on disk (photos not in any album land in an "uncategorized" folder)
- Concurrent downloads with a configurable worker pool
- Resumable — already-downloaded files are skipped on re-run, regardless of file extension
- Respects Flickr's API rate limits automatically
- Handles both photos and videos, always fetching the original quality available
- Live progress bar with ETA and transfer speed

## Requirements

- Go 1.25 or newer (only needed to build from source)
- A free Flickr API key: https://www.flickr.com/services/apps/create/apply/

## Install

Build from source:

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

```bash
flickrdownloader download -u <url> --out ~/Pictures/flickr --workers 30
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
- Two independent rate limiters: one for the Flickr REST API (~1 req/sec, matching Flickr's documented quota) and one for file downloads (scales with `--workers`)
- Ctrl-C cancels cleanly — in-flight downloads stop and the run reports partial progress

## Development

```bash
go build ./...
go vet ./...
```

## License

[MIT](LICENSE)
