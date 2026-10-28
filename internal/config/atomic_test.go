package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteFileAtomicReplacesAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	// A stale fixed-name temp from an older build must not interfere.
	if err := os.WriteFile(p+".tmp", []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(p, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(p, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "two" {
		t.Fatalf("content %q %v", b, err)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(p)
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", st.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "config.json" && e.Name() != "config.json.tmp" {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
}

// Regression for PR #31: a dir-sync failure after the rename has already
// committed the new file, so Update must treat it as success — otherwise
// memory is rolled back while a restart applies the "failed" write.
func TestStoreUpdatePostRenameDirSyncFailureKeepsMemoryInSync(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	old := syncDir
	called := 0
	syncDir = func(string) error { called++; return errors.New("injected dir sync failure") }
	t.Cleanup(func() { syncDir = old })

	var notified []Config
	s.Subscribe(func(c Config) { notified = append(notified, c) })

	const name = "after-dir-sync-failure"
	if err := s.Update(func(c *Config) error { c.Server.FriendlyName = name; return nil }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if called == 0 {
		t.Fatal("syncDir stub was never invoked; dir-sync failure was not exercised")
	}
	if got := s.Get().Server.FriendlyName; got != name {
		t.Fatalf("in-memory FriendlyName = %q, want %q", got, name)
	}
	if len(notified) != 1 || notified[0].Server.FriendlyName != name {
		t.Fatalf("notified = %+v, want one update with %q", notified, name)
	}
	reloaded, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Get().Server.FriendlyName; got != name {
		t.Fatalf("reloaded FriendlyName = %q, want %q (memory must equal disk)", got, name)
	}
}

func TestWriteFileAtomicFailureLeavesOriginal(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := writeFileAtomic(p, []byte("good"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Renaming onto a non-empty directory fails; the original must survive
	// and no temp file may be left behind.
	target := filepath.Join(dir, "sub")
	if err := os.MkdirAll(filepath.Join(target, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(target, []byte("bad"), 0o600); err == nil {
		t.Fatal("expected rename onto a directory to fail")
	}
	if b, _ := os.ReadFile(p); string(b) != "good" {
		t.Fatalf("original changed: %q", b)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("unexpected files %v", names)
	}
}
