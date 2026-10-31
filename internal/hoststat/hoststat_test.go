package hoststat

import (
	"os"
	"path/filepath"
	"testing"
)

func writeStatus(t *testing.T, dir, name, ppid, rss string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "fd"), 0700); err != nil {
		t.Fatal(err)
	}
	body := "Name:\t" + name + "\nPPid:\t" + ppid + "\nVmRSS:\t" + rss + " kB\n"
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fd", "0"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestProcessTreeSumsDescendants(t *testing.T) {
	root := t.TempDir()
	writeStatus(t, filepath.Join(root, "10"), "soak", "1", "2")
	writeStatus(t, filepath.Join(root, "11"), "soak", "10", "4")
	if err := os.WriteFile(filepath.Join(root, "11", "cmdline"), []byte("--ether-internal-encoder-guardian\x00/opt/ffmpeg"), 0600); err != nil {
		t.Fatal(err)
	}
	writeStatus(t, filepath.Join(root, "12"), "ffmpeg", "11", "8")
	if err := os.WriteFile(filepath.Join(root, "12", "cmdline"), []byte("/usr/lib/ether/ffmpeg\x00-i"), 0600); err != nil {
		t.Fatal(err)
	}
	writeStatus(t, filepath.Join(root, "99"), "other", "1", "100")
	tree, err := ProcessTree(root, 10)
	if err != nil {
		t.Fatal(err)
	}
	if tree.RSSBytes != 14*1024 || tree.FDs != 3 || tree.Children != 2 || len(tree.FFmpegPIDs) != 1 || tree.FFmpegPIDs[0] != 12 || len(tree.GuardianPIDs) != 1 || tree.GuardianPIDs[0] != 11 {
		t.Fatalf("tree: %+v", tree)
	}
}

func TestNetDevSkipsLoopback(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "net"), 0700); err != nil {
		t.Fatal(err)
	}
	body := "Inter-|   Receive                                                |  Transmit\n face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n    lo: 9 0 0 0 0 0 0 0 8 0 0 0 0 0 0 0\n  eth0: 100 1 0 0 0 0 0 0 40 1 0 0 0 0 0 0\n"
	if err := os.WriteFile(filepath.Join(root, "net", "dev"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	rx, tx, err := NetDev(root)
	if err != nil || rx != 100 || tx != 40 {
		t.Fatalf("net %d %d %v", rx, tx, err)
	}
}

func TestGPUAbsent(t *testing.T) {
	if GPU(t.TempDir()) != "no_device" {
		t.Fatal("expected no_device")
	}
}
