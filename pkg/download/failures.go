package download

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jooservices/go-flickrdownloader/pkg/api"
	"github.com/jooservices/go-flickrdownloader/pkg/ui"
)

// FailuresFileName is the JSONL log of photo download failures, stored in
// the output root so it moves with the photo tree.
const FailuresFileName = ".flickrdownloader.failures.jsonl"

type failedRecord struct {
	ID  string `json:"id"`
	Rel string `json:"rel"`
	URL string `json:"url,omitempty"`
	Err string `json:"err"`
	At  int64  `json:"at"`
}

func (r failedRecord) key() string { return r.Rel + "\x00" + r.ID }

func (d *Downloader) failuresPath() string {
	return filepath.Join(d.rootDir, FailuresFileName)
}

func (d *Downloader) loadFailures() []failedRecord {
	f, err := os.Open(d.failuresPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	byKey := map[string]failedRecord{}
	var order []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec failedRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if !api.ValidPhotoID(rec.ID) {
			continue
		}
		rec.Rel = filepath.ToSlash(filepath.Clean(rec.Rel))
		if rec.Rel == "." {
			rec.Rel = ""
		}
		if strings.HasPrefix(rec.Rel, "..") || filepath.IsAbs(rec.Rel) {
			continue
		}
		k := rec.key()
		if _, ok := byKey[k]; !ok {
			order = append(order, k)
		}
		byKey[k] = rec
	}
	out := make([]failedRecord, 0, len(order))
	for _, k := range order {
		rec := byKey[k]
		dest, err := d.destForRel(rec.Rel)
		if err != nil {
			continue
		}
		if photoPresentInDir(dest, rec.ID) {
			continue
		}
		out = append(out, rec)
	}
	return out
}

func (d *Downloader) destForRel(rel string) (string, error) {
	dest := d.rootDir
	if rel != "" {
		dest = filepath.Join(d.rootDir, filepath.FromSlash(rel))
	}
	dest = absolutePath(dest)
	relBack, err := filepath.Rel(d.rootDir, dest)
	if err != nil || relBack == ".." || strings.HasPrefix(relBack, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("failure path escapes output root")
	}
	return dest, nil
}

func (d *Downloader) relForOutDir() string {
	rel, err := filepath.Rel(d.rootDir, absolutePath(d.OutDir))
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

func (d *Downloader) saveFailures(recs []failedRecord) error {
	path := d.failuresPath()
	if len(recs) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), FailuresFileName+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	abort := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	w := bufio.NewWriter(tmp)
	for _, rec := range recs {
		line, err := json.Marshal(rec)
		if err != nil {
			abort()
			return err
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			abort()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		abort()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

func (d *Downloader) persistFailure(id, url, errMsg string) {
	rel := d.relForOutDir()
	rec := failedRecord{ID: id, Rel: rel, URL: url, Err: errMsg, At: time.Now().Unix()}
	d.failMu.Lock()
	defer d.failMu.Unlock()
	recs := d.loadFailures()
	replaced := false
	for i, existing := range recs {
		if existing.key() == rec.key() {
			recs[i] = rec
			replaced = true
			break
		}
	}
	if !replaced {
		recs = append(recs, rec)
	}
	_ = d.saveFailures(recs)
}

func (d *Downloader) clearFailure(id, rel string) {
	d.failMu.Lock()
	defer d.failMu.Unlock()
	recs := d.loadFailures()
	out := recs[:0]
	for _, rec := range recs {
		if rec.ID == id && rec.Rel == rel {
			continue
		}
		out = append(out, rec)
	}
	_ = d.saveFailures(out)
}

func (d *Downloader) recordPhotoFailure(id, url, errMsg string) {
	if d.progress != nil {
		d.progress.AddFailure(id, url, errMsg)
	}
	d.persistFailure(id, url, errMsg)
	fmt.Fprintf(os.Stderr, "\r\033[K  %s%s %s: %s%s\n",
		ui.ColorRed, ui.IconErr, id, errMsg, ui.ColorReset)
	if d.Logf != nil {
		d.Logf("failed photo=%s dir=%q err=%q", id, d.relForOutDir(), errMsg)
	}
}

func (d *Downloader) recordPhotoSuccess(id string) {
	d.clearFailure(id, d.relForOutDir())
}

// FormatFailures returns a grouped report of still-pending failures on disk.
func (d *Downloader) FormatFailures() string {
	d.failMu.Lock()
	recs := d.loadFailures()
	d.failMu.Unlock()
	if len(recs) == 0 {
		return ""
	}
	entries := make([]ui.FailEntry, 0, len(recs))
	for _, rec := range recs {
		entries = append(entries, ui.FailEntry{ID: rec.ID, URL: rec.URL, Err: rec.Err})
	}
	return ui.FormatFailures(entries)
}

// RetryFailedFirst re-attempts every persisted failure under this output
// root before the run lists albums or starts new work.
func (d *Downloader) RetryFailedFirst(ctx context.Context) {
	d.failMu.Lock()
	recs := d.loadFailures()
	d.failMu.Unlock()
	if len(recs) == 0 || ctx.Err() != nil {
		return
	}

	msg := fmt.Sprintf("Retrying %d previously failed photo(s) first", len(recs))
	if !d.Quiet {
		fmt.Printf("  %s%s %s%s\n", ui.ColorYellow, ui.IconErr, msg, ui.ColorReset)
	}
	if d.Logf != nil {
		d.Logf("retry_failed count=%d", len(recs))
	}

	savedOut := d.OutDir
	savedProgress := d.progress
	d.progress = ui.NewProgress(len(recs))
	d.setAlbum("retry failed")
	defer func() {
		d.OutDir = savedOut
		d.progress = savedProgress
	}()

	for _, rec := range recs {
		if ctx.Err() != nil {
			break
		}
		dest, err := d.destForRel(rec.Rel)
		if err != nil {
			d.failMu.Lock()
			left := d.loadFailures()
			kept := left[:0]
			for _, existing := range left {
				if existing.key() != rec.key() {
					kept = append(kept, existing)
				}
			}
			_ = d.saveFailures(kept)
			d.failMu.Unlock()
			continue
		}
		if err := os.MkdirAll(dest, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "  %s%s retry %s: create dir: %v%s\n",
				ui.ColorRed, ui.IconErr, rec.ID, err, ui.ColorReset)
			continue
		}
		d.OutDir = dest
		if photoPresentInDir(dest, rec.ID) {
			d.clearFailure(rec.ID, rec.Rel)
			d.progress.AddSkipped()
			continue
		}
		photo := api.Photo{ID: rec.ID, Media: "photo"}
		if rec.URL != "" && allowedDownloadURL(rec.URL) {
			photo.URLOriginal = rec.URL
		}
		d.worker(ctx, photo)
		if !photoPresentInDir(dest, rec.ID) && photo.URLOriginal != "" && d.Client != nil {
			// Stored CDN URL may be stale; resolve via the API once.
			photo.URLOriginal = ""
			d.worker(ctx, photo)
		}
	}

	stat := d.recordAlbum("retry failed", d.progress)
	if !d.Quiet {
		fmt.Printf("  %sretry failed — ok %d · skip %d · fail %d%s\n",
			ui.ColorDim, stat.Success, stat.Skipped, stat.Failed, ui.ColorReset)
	}
}

func photoPresentInDir(dir, photoID string) bool {
	matches, _ := filepath.Glob(filepath.Join(dir, photoID+".*"))
	for _, m := range matches {
		if strings.HasSuffix(m, ".part") || strings.HasSuffix(m, ".cand") || strings.HasSuffix(m, ".tmp") {
			continue
		}
		return true
	}
	return false
}
