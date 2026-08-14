package download

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

// sweepStaleParts removes *.part and *.cand files in dir whose leading
// photo-ID segment (the filename up to its first '.') is absent from
// seenIDs. Every photo ID is recorded at enqueue time, so artifacts left
// behind by downloads whose ID was never part of this run are cleaned up
// while in-flight and resumable files are preserved (C10/AC-017).
//
// It is called only after every page of a per-album listing succeeded, so a
// partial listing can never mislabel a just-created .part as stale (BR8).
// Deletions are best-effort; failures are logged, never fatal.
func sweepStaleParts(dir string, seenIDs map[string]bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".part") && !strings.HasSuffix(name, ".cand") {
			continue
		}
		idx := strings.Index(name, ".")
		if idx <= 0 {
			continue
		}
		if seenIDs[name[:idx]] {
			continue
		}
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil {
			log.Printf("download: sweep %s: %v", path, err)
		}
	}
}
