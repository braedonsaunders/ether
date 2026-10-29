package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyFixtureRejectsSymlinks(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "copy")
	if err := os.WriteFile(filepath.Join(src, "fixture"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := copyFixture(src, dst); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dst, "fixture"))
	if err != nil || string(b) != "fixture" {
		t.Fatalf("copied %q: %v", b, err)
	}
	if err := os.Symlink("fixture", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := copyFixture(src, filepath.Join(t.TempDir(), "copy")); err == nil {
		t.Fatal("accepted symlink")
	}
}
