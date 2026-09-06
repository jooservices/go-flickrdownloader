package update

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	Owner   = "jooservices"
	Repo    = "flickrdownloader"
	BinName = "flickrdownloader"
)

type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
	// Digest is a "sha256:<hex>" checksum GitHub computes server-side for
	// uploaded release assets. Empty for releases predating that feature.
	Digest string `json:"digest"`
}

type Release struct {
	TagName string  `json:"tag_name"`
	Name    string  `json:"name"`
	Assets  []Asset `json:"assets"`
}

var httpClient = &http.Client{Timeout: 120 * time.Second}

// renameFile indirects os.Rename so tests can force a failure at a specific
// step of replaceExecutable's swap without needing a real filesystem failure
// condition (permission tricks, cross-device mounts) that's hard to produce
// portably.
var renameFile = os.Rename

// chmodFile indirects os.Chmod so tests can force the staged-binary chmod
// in replaceExecutable to fail and verify that failure is non-fatal.
var chmodFile = os.Chmod

// LatestRelease fetches the newest release metadata from GitHub.
func LatestRelease(ctx context.Context) (*Release, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", Owner, Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", BinName)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contact GitHub: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("no releases found for %s/%s yet", Owner, Repo)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub returned status %d", resp.StatusCode)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode release: %w", err)
	}
	return &rel, nil
}

// AssetFor returns the release asset matching this platform
// (flickrdownloader_vX.Y.Z_{goos}_{goarch}.tar.gz), or nil.
func (r *Release) AssetFor() *Asset {
	suffix := fmt.Sprintf("_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	for i := range r.Assets {
		if strings.HasSuffix(r.Assets[i].Name, suffix) {
			return &r.Assets[i]
		}
	}
	return nil
}

// checksumAsset returns the release's detached checksums file (the
// convention used by goreleaser and similar tools), or nil if none was
// published.
func (r *Release) checksumAsset() *Asset {
	for i := range r.Assets {
		n := strings.ToLower(r.Assets[i].Name)
		if n == "checksums.txt" || n == "sha256sums" || n == "sha256sums.txt" || strings.HasSuffix(n, "_checksums.txt") {
			return &r.Assets[i]
		}
	}
	return nil
}

// IsNewer reports whether latest is a newer version than current.
// Dev builds and empty versions always count as outdated.
func IsNewer(current, latest string) bool {
	if current == "" || current == "dev" {
		return true
	}
	pa, pb := parseVersion(current), parseVersion(latest)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func parseVersion(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	parts := strings.Split(v, ".")
	for i := 0; i < 3 && i < len(parts); i++ {
		out[i], _ = strconv.Atoi(parts[i])
	}
	return out
}

// Install downloads the asset archive, verifies its integrity against a
// checksum published alongside the release, extracts the binary and
// atomically replaces the currently running executable (keeping a .old
// backup until the new binary is in place).
func Install(ctx context.Context, rel *Release, a *Asset) error {
	tmp, err := os.MkdirTemp("", "flickrdownloader-update-")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	archivePath := filepath.Join(tmp, "release.tar.gz")
	if err := downloadAsset(ctx, a, archivePath); err != nil {
		return err
	}

	wantSHA256, err := expectedSHA256(ctx, rel, a)
	if err != nil {
		return err
	}
	if err := verifyChecksum(archivePath, wantSHA256); err != nil {
		return err
	}

	binPath, err := extractBinary(archivePath, tmp)
	if err != nil {
		return err
	}

	exe, err := executablePath()
	if err != nil {
		return fmt.Errorf("locate current binary: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("resolve current binary: %w", err)
	}

	return replaceExecutable(exe, binPath)
}

// executablePath indirects os.Executable so tests can drive Install
// end-to-end against a throwaway file instead of the real test binary.
// Production code never overrides it.
var executablePath = os.Executable

// replaceExecutable swaps binPath in for the running executable at exe. The
// new binary is staged alongside exe first (exe+".new") — so a copy failure
// never touches the live binary — then the swap is two renames on the same
// filesystem (exe -> exe.old, staged -> exe), each atomic. If the second
// rename fails, it attempts to restore exe.old back to exe and reports
// explicitly whether that recovery succeeded: silently discarding the
// recovery rename's own error previously left Windows users (where
// os.Rename refuses to overwrite an existing destination, so the recovery
// rename fails whenever the aborted copy already left something at exe)
// with no working binary and no indication recovery had failed.
func replaceExecutable(exe, binPath string) error {
	staged := exe + ".new"
	os.Remove(staged)
	if err := copyFile(binPath, staged); err != nil {
		os.Remove(staged)
		return fmt.Errorf("stage new binary: %w", err)
	}
	// Cleanup is deferred so every remaining early return removes the
	// staged copy for free (a no-op once it's actually been renamed to exe)
	// — except the one case where both the install rename and the recovery
	// rename fail, where staged is the only intact copy of the new binary
	// left and must be preserved for manual recovery, not silently deleted.
	keepStaged := false
	defer func() {
		if !keepStaged {
			os.Remove(staged)
		}
	}()

	// copyFile already created staged at mode 0755; re-assert it
	// defensively, but a failure here is not fatal — some filesystems
	// (certain FUSE/overlay mounts, sandboxed environments) reject chmod
	// even though the file was already created with the right mode, and
	// the pre-atomic-swap version of this code only warned on this same
	// failure rather than aborting the whole update.
	if err := chmodFile(staged, 0o755); err != nil {
		fmt.Printf("  warning: could not set permissions on staged binary: %v\n", err)
	}

	backup := exe + ".old"
	os.Remove(backup)
	if err := renameFile(exe, backup); err != nil {
		return fmt.Errorf("replace current binary: %w — hint: run from a writable location (e.g. not /usr/local/bin without sudo)", err)
	}

	if err := renameFile(staged, exe); err != nil {
		if restoreErr := renameFile(backup, exe); restoreErr != nil {
			keepStaged = true
			return fmt.Errorf("install new binary: %w — AND restoring the previous binary also failed: %v — the working binary is at %s, the update is at %s, restore manually", err, restoreErr, backup, staged)
		}
		return fmt.Errorf("install new binary: %w (previous binary restored)", err)
	}

	if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
		fmt.Printf("  warning: left %s in place (%v) — Windows cannot delete a running executable; remove it after restart\n", backup, err)
	}
	return nil
}

func downloadAsset(ctx context.Context, a *Asset, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.BrowserDownloadURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", BinName)
	req.Header.Set("Accept", "application/octet-stream")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("download release: %w — hint: check your connection", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download release: http status %d", resp.StatusCode)
	}

	out, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("create archive: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("write archive: %w", err)
	}
	if a.Size > 0 {
		if st, err := out.Stat(); err == nil && st.Size() != a.Size {
			return fmt.Errorf("downloaded archive is %d bytes, expected %d — retry", st.Size(), a.Size)
		}
	}
	return nil
}

// expectedSHA256 resolves the checksum the downloaded asset must match,
// preferring the digest GitHub computes for the asset itself and falling
// back to a detached checksums file published alongside the release.
// Refuses (rather than silently skipping verification) if neither is
// available, since a size-only check is trivially forged by an attacker who
// controls the asset content.
func expectedSHA256(ctx context.Context, rel *Release, a *Asset) (string, error) {
	if algo, hex, ok := strings.Cut(a.Digest, ":"); ok {
		if algo != "sha256" {
			return "", fmt.Errorf("asset %s: unsupported digest algorithm %q", a.Name, algo)
		}
		return strings.ToLower(hex), nil
	}

	if rel != nil {
		if cs := rel.checksumAsset(); cs != nil {
			sums, err := fetchChecksums(ctx, cs)
			if err != nil {
				return "", fmt.Errorf("fetch checksums for %s: %w", a.Name, err)
			}
			if sum, ok := sums[a.Name]; ok {
				return sum, nil
			}
			return "", fmt.Errorf("asset %s: not listed in %s", a.Name, cs.Name)
		}
	}

	return "", fmt.Errorf("no checksum available for %s — refusing to install an unverified binary", a.Name)
}

// fetchChecksums downloads and parses a "<hex>  <filename>" checksums file
// (the format goreleaser and `sha256sum` both produce).
func fetchChecksums(ctx context.Context, cs *Asset) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cs.BrowserDownloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", BinName)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http status %d", resp.StatusCode)
	}

	sums := make(map[string]string)
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		sums[strings.TrimPrefix(fields[1], "*")] = strings.ToLower(fields[0])
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read checksums: %w", err)
	}
	return sums, nil
}

// verifyChecksum recomputes the SHA-256 of the file at path and compares it
// (case-insensitively, constant-time is unnecessary here since neither value
// is a secret) against want.
func verifyChecksum(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open archive for checksum: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash archive: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != strings.ToLower(want) {
		return fmt.Errorf("checksum mismatch: got %s, expected %s — the download may be corrupt or tampered with", got, want)
	}
	return nil
}

// isBinaryAssetName reports whether an archive entry's base name is the
// release binary. Real archives (see AssetFor) name the entry after the
// asset itself, e.g. "flickrdownloader_darwin_arm64" — not the bare
// "flickrdownloader" — so a plain equality check never matches. Accept
// either that, or the bare name, with an optional ".exe" suffix.
func isBinaryAssetName(base string) bool {
	name := strings.TrimSuffix(base, ".exe")
	return name == BinName || strings.HasPrefix(name, BinName+"_")
}

func extractBinary(archivePath, destDir string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("decompress archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	binPath := ""
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read archive: %w", err)
		}
		if hdr.FileInfo().IsDir() {
			continue
		}
		base := filepath.Base(hdr.Name)
		if !isBinaryAssetName(base) {
			continue
		}
		binPath = filepath.Join(destDir, base)
		out, err := os.OpenFile(binPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return "", fmt.Errorf("extract binary: %w", err)
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return "", fmt.Errorf("extract binary: %w", err)
		}
		out.Close()
		break
	}

	if binPath == "" {
		return "", fmt.Errorf("binary not found in archive — the release layout may have changed")
	}
	return binPath, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
