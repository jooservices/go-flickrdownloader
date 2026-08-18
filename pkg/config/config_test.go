package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSavePermissionsAndDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &Config{APIKey: "k", APISecret: "s", OAuthToken: "t", OAuthSecret: "ts"}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".config", "flickrdownloader", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config permissions = %o, want 600", got)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.WorkerCount != 20 || loaded.OutputDir != "./photos" || loaded.APIHourlyLimit != 3600 ||
		loaded.APIRateMS != 1050 || loaded.CacheListingTTLHours != DefaultCacheListingTTLHours {
		t.Fatalf("defaults = %+v", loaded)
	}
}

func TestCachePathDiffersByNSID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a, err := CachePath("key", "nsid-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := CachePath("key", "nsid-b")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("CachePath same for different NSIDs: %s", a)
	}
}

func TestWriteFileAtomicReplacesDest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("got %q, want new", got)
	}
}

func TestAuthFingerprintStableAndDistinct(t *testing.T) {
	if AuthFingerprint("a") == AuthFingerprint("b") {
		t.Fatal("different tokens produced the same fingerprint")
	}
	if AuthFingerprint("a") != AuthFingerprint("a") {
		t.Fatal("fingerprint is not stable")
	}
}
