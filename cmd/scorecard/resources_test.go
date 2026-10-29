package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResourceSampleIncludesChildProcesses(t *testing.T) {
	root := t.TempDir()
	cg := filepath.Join(root, "cgroup")
	proc := filepath.Join(root, "proc")
	if err := os.MkdirAll(cg, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(path, value string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range map[string]string{"cpu.max": "200000 100000", "cpuset.cpus.effective": "0-1", "memory.max": "1073741824", "cpu.stat": "usage_usec 12345\nuser_usec 12000\n", "memory.current": "4096", "memory.peak": "8192", "pids.current": "5", "io.stat": "8:0 rbytes=100 wbytes=200 rios=3 wios=4\n8:1 rbytes=50 wbytes=25\n", "cgroup.procs": "10\n11\n"} {
		write(filepath.Join(cg, name), value)
	}
	write(filepath.Join(proc, "10", "status"), "Name:\tparent\nPPid:\t1\nVmRSS:\t2 kB\nThreads:\t3\n")
	write(filepath.Join(proc, "10", "fd", "0"), "")
	write(filepath.Join(proc, "11", "status"), "Name:\tffmpeg\nPPid:\t10\nVmRSS:\t4 kB\nThreads:\t2\n")
	write(filepath.Join(proc, "11", "fd", "0"), "")
	write(filepath.Join(proc, "11", "fd", "1"), "")
	r, e := collectResources(context.Background(), cg, proc, 100*time.Millisecond, 1)
	if e != nil || !r.Complete || len(r.Samples) != 1 {
		t.Fatalf("%+v %v", r, e)
	}
	s := r.Samples[0]
	if s.CPUUsec != 12345 || s.MemoryCurrent != 4096 || s.MemoryPeak == nil || *s.MemoryPeak != 8192 || s.IOReadBytes != 150 || s.IOWriteBytes != 225 || s.ProcessCount != 2 || s.ChildCount != 1 || s.RSSSum != 6144 || s.Threads != 5 || s.FDs != 3 || s.ProcessReadErrors != 0 {
		t.Fatalf("wrong process/cgroup data: %+v", s)
	}
	if !strings.Contains(strings.Join(r.Caveats, " "), "goroutine") {
		t.Fatal("unsupported metrics not disclosed")
	}
}
func TestResourceSampleReportsDisappearingProcesses(t *testing.T) {
	root := t.TempDir()
	for name, value := range map[string]string{"cpu.stat": "usage_usec 1\n", "memory.current": "2", "pids.current": "1", "io.stat": "", "cgroup.procs": "123\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, e := readResourceSample(root, t.TempDir())
	if e != nil || s.ProcessCount != 1 || s.ProcessReadErrors != 1 {
		t.Fatalf("missing process incorrectly hidden: %+v %v", s, e)
	}
}

func TestResourceDeltasAndFixedLimits(t *testing.T) {
	a := resourceSample{CPUUsec: 1000000, IOReadBytes: 100, IOWriteBytes: 50}
	b := resourceSample{CPUUsec: 4000000, IOReadBytes: 700, IOWriteBytes: 300}
	d, e := summarizeResources(a, b, 6)
	if e != nil || d.CPUSeconds != 3 || d.CPUSecondsPerValidSuccess == nil || *d.CPUSecondsPerValidSuccess != 0.5 || d.IOReadBytes != 600 || d.IOWriteBytes != 250 {
		t.Fatalf("delta %+v %v", d, e)
	}
	if _, e = summarizeResources(b, a, 1); e == nil {
		t.Fatal("regressed counters accepted")
	}
	root := t.TempDir()
	for _, side := range []string{"a", "b"} {
		dir := filepath.Join(root, side)
		if e = os.MkdirAll(dir, 0700); e != nil {
			t.Fatal(e)
		}
		for k, v := range map[string]string{"cpu.max": "200000 100000", "memory.max": "1073741824", "cpuset.cpus.effective": "0-1"} {
			if e = os.WriteFile(filepath.Join(dir, k), []byte(v), 0600); e != nil {
				t.Fatal(e)
			}
		}
	}
	left, right := filepath.Join(root, "a"), filepath.Join(root, "b")
	if e = validateMatchedLimits(left, right); e != nil {
		t.Fatal(e)
	}
	if e = validateMatchedLimits(left, left); e == nil {
		t.Fatal("same cgroup accepted")
	}

	if e = os.WriteFile(filepath.Join(right, "memory.max"), []byte("max"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = validateMatchedLimits(left, right); e == nil {
		t.Fatal("unmatched limits accepted")
	}
}
