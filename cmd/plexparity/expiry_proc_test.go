package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestEncoderChildrenRequiresLiveMatchingProcess(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "10", "task", "10")
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := encoderChildren(root, 10, "parity-expiry"); err == nil {
		t.Fatal("missing procfs children silently treated as cleanup")
	}
	if err := os.WriteFile(filepath.Join(parent, "children"), []byte("21 22"), 0600); err != nil {
		t.Fatal(err)
	}
	worker := filepath.Join(root, "10", "task", "11")
	guardian := filepath.Join(root, "23", "task", "24")
	for _, dir := range []string{worker, guardian} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(worker, "children"), []byte("23"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(guardian, "children"), []byte("20"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ pid, cmd, cwd string }{{"20", "/bin/ffmpeg\x00-i\x00fixture\x00", "/tmp/s-parity-expiry-abcd"}, {"21", "/bin/ffmpeg\x00", "/tmp/s-other-abcd"}, {"22", "/bin/ffprobe\x00", "/tmp/s-parity-expiry-abcd"}, {"23", "/bin/ether\x00--ether-internal-encoder-guardian\x00", "/tmp/s-parity-expiry-abcd"}} {
		dir := filepath.Join(root, tc.pid)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(tc.cmd), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(tc.cwd, filepath.Join(dir, "cwd")); err != nil {
			t.Fatal(err)
		}
	}
	got, err := encoderChildren(root, 10, "parity-expiry")
	if err != nil || !reflect.DeepEqual(got, []int{20}) {
		t.Fatalf("children=%v err=%v", got, err)
	}
}
