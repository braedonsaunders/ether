package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteFDSnapshotSkipsWithoutDir(t *testing.T) {
	if err := writeFDSnapshot("", "baseline"); err != nil {
		t.Fatal(err)
	}
}

func TestWriteFDSnapshot(t *testing.T) {
	dir := t.TempDir()
	err := writeFDSnapshot(dir, "baseline")
	if runtime.GOOS != "linux" {
		if err == nil {
			t.Fatal("snapshot succeeded off linux")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	fd, err := os.ReadFile(filepath.Join(dir, "fd-baseline.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fd), "->") || !strings.Contains(string(fd), "# pid ") {
		t.Fatalf("fd listing:\n%s", fd)
	}
	for _, name := range []string{"tcp-baseline.txt", "tcp6-baseline.txt", "unix-baseline.txt"} {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if len(body) == 0 {
			t.Fatalf("%s empty", name)
		}
	}
}
