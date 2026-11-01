package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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

func TestRetainedServerLogKeepsTheWholeFile(t *testing.T) {
	stage := t.TempDir()
	seedDir := filepath.Join(stage, "seeddata")
	if err := os.Mkdir(seedDir, 0700); err != nil {
		t.Fatal(err)
	}
	temp := t.TempDir()
	body := bytes.Repeat([]byte("x"), 9000)
	src := filepath.Join(temp, "server-0.log")
	if err := os.WriteFile(src, body, 0600); err != nil {
		t.Fatal(err)
	}
	retain := retainedServerLogDir(seedDir, false)
	if retain != filepath.Join(stage, "retained-server-logs", "seeddata", "ordinary") {
		t.Fatalf("retain dir %s", retain)
	}
	if sub := retainedServerLogDir(seedDir, true); !strings.HasSuffix(sub, filepath.Join("seeddata", "subtitles")) {
		t.Fatalf("subtitle retain dir %s", sub)
	}
	var out bytes.Buffer
	if err := publishServerLog(&out, 0, src, retain); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(retain, "server-0.log")
	got, err := os.ReadFile(kept)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("retained %d bytes, want %d", len(got), len(body))
	}
	if strings.Contains(out.String(), "log tail") || !strings.Contains(out.String(), kept) {
		t.Fatalf("failure line %q", out.String())
	}
	if err := os.RemoveAll(temp); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatal(err)
	}
}
