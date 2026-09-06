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

func TestQuotaPathDiffersByAPIKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a, err := QuotaPath("key-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := QuotaPath("key-b")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("QuotaPath same for different API keys: %s", a)
	}
	if filepath.Ext(a) != ".log" {
		t.Fatalf("QuotaPath = %s, want a .log file", a)
	}
}

func TestLoadNotAuthenticatedWhenConfigMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := Load(); err == nil {
		t.Fatal("expected an error when no config file exists yet")
	}
}

func TestLoadRejectsMalformedJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "flickrdownloader")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("expected an error for malformed config JSON")
	}
}

func TestLoadPreservesExplicitValues(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := &Config{
		APIKey: "k", APISecret: "s", OAuthToken: "t", OAuthSecret: "ts",
		WorkerCount: 5, OutputDir: "/custom", APIHourlyLimit: 100,
		APIRateMS: 2000, CacheListingTTLHours: 1,
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.WorkerCount != 5 || loaded.OutputDir != "/custom" || loaded.APIHourlyLimit != 100 ||
		loaded.APIRateMS != 2000 || loaded.CacheListingTTLHours != 1 {
		t.Fatalf("explicit values overridden by defaults: %+v", loaded)
	}
}

func TestConfigDirFailsWhenHomeIsAFile(t *testing.T) {
	dir := t.TempDir()
	homeBlocker := filepath.Join(dir, "home")
	if err := os.WriteFile(homeBlocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", homeBlocker)
	if _, err := QuotaPath("key"); err == nil {
		t.Fatal("expected an error when HOME is a file, not a directory")
	}
}

func TestSavePropagatesWriteFileAtomicFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "flickrdownloader")
	// Pre-create the destination as a directory, so WriteFileAtomic's
	// final rename fails.
	if err := os.MkdirAll(filepath.Join(dir, "config.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{APIKey: "k"}
	if err := cfg.Save(); err == nil {
		t.Fatal("expected Save to propagate a WriteFileAtomic failure")
	}
}

func TestWriteFileAtomicFailsWhenDirNotWritable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: directory permissions don't block writes")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if err := WriteFileAtomic(filepath.Join(dir, "config.json"), []byte("x"), 0o600); err == nil {
		t.Fatal("expected an error when the directory is not writable")
	}
}

func TestConfigDirFailsWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	if _, err := QuotaPath("key"); err == nil {
		t.Fatal("expected an error when HOME is unset")
	}
}

func TestCachePathFailsWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	if _, err := CachePath("key", "nsid"); err == nil {
		t.Fatal("expected an error when HOME is unset")
	}
}

func TestSavePropagatesConfigDirFailure(t *testing.T) {
	t.Setenv("HOME", "")
	cfg := &Config{APIKey: "k"}
	if err := cfg.Save(); err == nil {
		t.Fatal("expected an error when HOME is unset")
	}
}

func TestWriteFileAtomicFailsWhenParentIsAFile(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(filepath.Join(blocker, "config.json"), []byte("x"), 0o600); err == nil {
		t.Fatal("expected an error when a path component is a file, not a directory")
	}
}

func TestWriteFileAtomicFailsWhenDestIsADirectory(t *testing.T) {
	dir := t.TempDir()
	destDir := filepath.Join(dir, "config.json")
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(destDir, []byte("x"), 0o600); err == nil {
		t.Fatal("expected an error when the destination path is a directory")
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
