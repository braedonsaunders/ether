package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type resourceDelta struct {
	CPUSeconds                float64        `json:"cpu_seconds"`
	CPUSecondsPerValidSuccess *float64       `json:"cpu_seconds_per_valid_success,omitempty"`
	IOReadBytes               uint64         `json:"io_read_bytes"`
	IOWriteBytes              uint64         `json:"io_write_bytes"`
	Start                     resourceSample `json:"start"`
	End                       resourceSample `json:"end"`
	Caveat                    string         `json:"caveat"`
}

func summarizeResources(start, end resourceSample, validSuccesses int) (resourceDelta, error) {
	if end.CPUUsec < start.CPUUsec || end.IOReadBytes < start.IOReadBytes || end.IOWriteBytes < start.IOWriteBytes {
		return resourceDelta{}, errors.New("resource counter regressed")
	}
	r := resourceDelta{CPUSeconds: float64(end.CPUUsec-start.CPUUsec) / 1e6, IOReadBytes: end.IOReadBytes - start.IOReadBytes, IOWriteBytes: end.IOWriteBytes - start.IOWriteBytes, Start: start, End: end, Caveat: "cgroup CPU and I/O include descendants; RSS/FD/thread snapshots can be incomplete or double-count shared pages; memory.peak may predate the run; network, GPU and goroutines are unmeasured"}
	if validSuccesses > 0 {
		value := r.CPUSeconds / float64(validSuccesses)
		r.CPUSecondsPerValidSuccess = &value
	}
	return r, nil
}
func validateMatchedLimits(a, b string) error {
	if !filepath.IsAbs(a) || !filepath.IsAbs(b) {
		return errors.New("cgroup paths must be absolute")
	}
	left, err := os.Stat(a)
	if err != nil {
		return errors.New("reference cgroup unavailable")
	}
	right, err := os.Stat(b)
	if err != nil {
		return errors.New("ether cgroup unavailable")
	}
	if os.SameFile(left, right) {
		return errors.New("reference and ether must use distinct cgroups")
	}
	for _, name := range []string{"cpu.max", "memory.max", "cpuset.cpus.effective"} {
		x, e := readTrim(filepath.Join(a, name))
		if e != nil {
			return errors.New("reference cgroup limits unavailable")
		}
		y, e := readTrim(filepath.Join(b, name))
		if e != nil {
			return errors.New("ether cgroup limits unavailable")
		}
		if x != y {
			return errors.New("reference and ether cgroup limits differ")
		}
		if (name == "memory.max" && x == "max") || (name == "cpu.max" && strings.HasPrefix(x, "max ")) || x == "" {
			return errors.New("fixed CPU, memory and cpuset limits are required")
		}
		if name == "memory.max" {
			n, e := strconv.ParseUint(x, 10, 64)
			if e != nil || n == 0 {
				return errors.New("invalid finite memory limit")
			}
		}
		if name == "cpu.max" {
			parts := strings.Fields(x)
			if len(parts) != 2 {
				return errors.New("invalid finite CPU limit")
			}
			quota, e := strconv.ParseUint(parts[0], 10, 64)
			period, e2 := strconv.ParseUint(parts[1], 10, 64)
			if e != nil || e2 != nil || quota == 0 || period == 0 {
				return errors.New("invalid finite CPU limit")
			}
		}
	}
	return nil
}
