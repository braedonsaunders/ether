package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type resourceSample struct {
	Time              time.Time `json:"time"`
	CPUUsec           uint64    `json:"cpu_usage_usec"`
	MemoryCurrent     uint64    `json:"memory_current_bytes"`
	MemoryPeak        *uint64   `json:"memory_peak_bytes,omitempty"`
	IOReadBytes       uint64    `json:"io_read_bytes"`
	IOWriteBytes      uint64    `json:"io_write_bytes"`
	PIDsCurrent       uint64    `json:"pids_current"`
	ProcessCount      int       `json:"process_count"`
	ChildCount        int       `json:"child_process_count"`
	RSSSum            uint64    `json:"process_rss_sum_bytes"`
	Threads           int       `json:"threads"`
	FDs               int       `json:"fds"`
	ProcessReadErrors int       `json:"process_read_errors"`
}
type resourceReport struct {
	CgroupVersion    int              `json:"cgroup_version"`
	CPUMax           string           `json:"cpu_max"`
	CPUSet           string           `json:"cpuset_effective"`
	MemoryMax        string           `json:"memory_max"`
	IntervalMS       int64            `json:"interval_ms"`
	RequestedSamples int              `json:"requested_samples"`
	Complete         bool             `json:"complete"`
	Samples          []resourceSample `json:"samples"`
	Caveats          []string         `json:"caveats"`
}

func resources(args []string) error {
	fs := flag.NewFlagSet("resources", flag.ContinueOnError)
	cgroup := fs.String("cgroup", "", "isolated cgroup v2 directory")
	interval := fs.Duration("interval", time.Second, "sampling interval")
	samples := fs.Int("samples", 10, "number of samples")
	if e := fs.Parse(args); e != nil {
		return e
	}
	if *cgroup == "" || !filepath.IsAbs(*cgroup) || *samples < 1 || *samples > 100000 || *interval < 100*time.Millisecond {
		return errors.New("absolute cgroup, 1..100000 samples and interval >=100ms required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r, e := collectResources(ctx, *cgroup, "/proc", *interval, *samples)
	if e != nil {
		return e
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
func readTrim(path string) (string, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return "", e
	}
	return strings.TrimSpace(string(b)), nil
}
func readUint(path string) (uint64, error) {
	s, e := readTrim(path)
	if e != nil {
		return 0, e
	}
	return strconv.ParseUint(s, 10, 64)
}
func keyValues(path string) (map[string]uint64, error) {
	s, e := readTrim(path)
	if e != nil {
		return nil, e
	}
	out := map[string]uint64{}
	for _, line := range strings.Split(s, "\n") {
		v := strings.Fields(line)
		if len(v) != 2 {
			return nil, fmt.Errorf("invalid cgroup statistic")
		}
		n, e := strconv.ParseUint(v[1], 10, 64)
		if e != nil {
			return nil, e
		}
		out[v[0]] = n
	}
	return out, nil
}
func ioBytes(path string) (uint64, uint64, error) {
	s, e := readTrim(path)
	if e != nil {
		return 0, 0, e
	}
	var read, write uint64
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		for _, f := range fields[1:] {
			k, v, ok := strings.Cut(f, "=")
			if !ok {
				continue
			}
			if k != "rbytes" && k != "wbytes" {
				continue
			}
			n, e := strconv.ParseUint(v, 10, 64)
			if e != nil {
				return 0, 0, e
			}
			if k == "rbytes" {
				read += n
			} else {
				write += n
			}
		}
	}
	return read, write, nil
}
func collectResources(ctx context.Context, cgroup, proc string, interval time.Duration, count int) (resourceReport, error) {
	r := resourceReport{CgroupVersion: 2, IntervalMS: interval.Milliseconds(), RequestedSamples: count, Samples: []resourceSample{}, Caveats: []string{"cpu and io counters include child processes within the cgroup", "memory.current includes cache; process RSS sums can double-count shared pages", "memory.peak may predate this run; use a fresh cgroup", "process counts, RSS, threads and file descriptors are best-effort snapshots; process_read_errors indicate incomplete data", "goroutine count, GPU utilization and per-container network I/O are not collected", "successful operations, delivered quality and cache state must be correlated separately"}}
	var e error
	if r.CPUMax, e = readTrim(filepath.Join(cgroup, "cpu.max")); e != nil {
		return r, e
	}
	if r.MemoryMax, e = readTrim(filepath.Join(cgroup, "memory.max")); e != nil {
		return r, e
	}
	if r.CPUSet, e = readTrim(filepath.Join(cgroup, "cpuset.cpus.effective")); e != nil {
		return r, e
	}
	for i := 0; i < count; i++ {
		if i > 0 {
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return r, nil
			case <-timer.C:
			}
		}
		s, e := readResourceSample(cgroup, proc)
		if e != nil {
			return r, e
		}
		r.Samples = append(r.Samples, s)
	}
	r.Complete = true
	return r, nil
}
func readResourceSample(cgroup, proc string) (resourceSample, error) {
	s := resourceSample{Time: time.Now().UTC()}
	cpu, e := keyValues(filepath.Join(cgroup, "cpu.stat"))
	if e != nil {
		return s, e
	}
	var ok bool
	if s.CPUUsec, ok = cpu["usage_usec"]; !ok {
		return s, errors.New("missing cpu usage")
	}
	if s.MemoryCurrent, e = readUint(filepath.Join(cgroup, "memory.current")); e != nil {
		return s, e
	}
	if peak, e := readUint(filepath.Join(cgroup, "memory.peak")); e == nil {
		s.MemoryPeak = &peak
	}
	if s.PIDsCurrent, e = readUint(filepath.Join(cgroup, "pids.current")); e != nil {
		return s, e
	}
	if s.IOReadBytes, s.IOWriteBytes, e = ioBytes(filepath.Join(cgroup, "io.stat")); e != nil {
		return s, e
	}
	ids := map[string]bool{}
	e = filepath.WalkDir(cgroup, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			b, e := os.ReadFile(filepath.Join(path, "cgroup.procs"))
			if e != nil {
				return e
			}
			for _, pid := range strings.Fields(string(b)) {
				if _, e := strconv.ParseUint(pid, 10, 32); e != nil {
					return errors.New("invalid cgroup pid")
				}
				ids[pid] = true
			}
		}
		return nil
	})
	if e != nil {
		return s, e
	}
	s.ProcessCount = len(ids)
	keys := make([]string, 0, len(ids))
	for id := range ids {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	for _, id := range keys {
		status, e := readTrim(filepath.Join(proc, id, "status"))
		if e != nil {
			s.ProcessReadErrors++
			continue
		}
		var parent string
		seenRSS, seenThreads, seenParent := false, false, false
		for _, line := range strings.Split(status, "\n") {
			v := strings.Fields(line)
			if len(v) < 2 {
				continue
			}
			switch v[0] {
			case "PPid:":
				seenParent = true
				parent = v[1]
			case "VmRSS:":
				seenRSS = true
				n, e := strconv.ParseUint(v[1], 10, 64)
				if e != nil {
					s.ProcessReadErrors++
				} else {
					s.RSSSum += n * 1024
				}
			case "Threads:":
				seenThreads = true
				n, e := strconv.Atoi(v[1])
				if e != nil {
					s.ProcessReadErrors++
				} else {
					s.Threads += n
				}
			}
		}
		if !seenRSS || !seenThreads || !seenParent {
			s.ProcessReadErrors++
		}
		if ids[parent] {
			s.ChildCount++
		}
		fds, e := os.ReadDir(filepath.Join(proc, id, "fd"))
		if e != nil {
			s.ProcessReadErrors++
		} else {
			s.FDs += len(fds)
		}
	}
	return s, nil
}
