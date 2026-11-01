//go:build linux

package hoststat

import (
	"os"
	"strconv"
	"strings"
	"sync"
)

type cpuSnap struct {
	total, idle, proc uint64
}

var cpuMu sync.Mutex
var lastCPU cpuSnap
var haveCPU bool

func hostSample() (Sample, bool) {
	hostTotal, hostIdle, ok := readHostCPU("/proc/stat")
	if !ok {
		return Sample{}, false
	}
	proc, ok := readProcCPU("/proc/self/stat")
	if !ok {
		return Sample{}, false
	}
	hostMem, procMem := readMem("/proc/meminfo", "/proc/self/status")
	cpuMu.Lock()
	prev, ready := lastCPU, haveCPU
	lastCPU = cpuSnap{total: hostTotal, idle: hostIdle, proc: proc}
	haveCPU = true
	cpuMu.Unlock()
	if !ready || hostTotal <= prev.total {
		return Sample{}, false
	}
	dt := float64(hostTotal - prev.total)
	hostCPU := (1 - float64(hostIdle-prev.idle)/dt) * 100
	if hostCPU < 0 {
		hostCPU = 0
	}
	if hostCPU > 100 {
		hostCPU = 100
	}
	// USER_HZ is 100 on Linux; process ticks over the same host total.
	procCPU := float64(proc-prev.proc) / dt * 100
	if procCPU < 0 {
		procCPU = 0
	}
	if procCPU > 100 {
		procCPU = 100
	}
	return Sample{HostCPU: hostCPU, ProcessCPU: procCPU, HostMem: hostMem, ProcessMem: procMem}, true
}

func readHostCPU(path string) (total, idle uint64, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	line, _, _ := strings.Cut(string(data), "\n")
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, false
	}
	for i, f := range fields[1:] {
		n, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		total += n
		if i == 3 || i == 4 { // idle, iowait
			idle += n
		}
	}
	return total, idle, true
}

func readProcCPU(path string) (uint64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return 0, false
	}
	fields := strings.Fields(s[i+2:])
	if len(fields) < 13 {
		return 0, false
	}
	utime, err1 := strconv.ParseUint(fields[11], 10, 64)
	stime, err2 := strconv.ParseUint(fields[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return utime + stime, true
}

func readMem(meminfo, status string) (host, proc float64) {
	total, avail, ok := meminfoBytes(meminfo)
	if !ok || total == 0 {
		return 0, 0
	}
	host = (1 - float64(avail)/float64(total)) * 100
	if host < 0 {
		host = 0
	}
	if rss, ok := statusRSS(status); ok {
		proc = float64(rss) / float64(total) * 100
	}
	return host, proc
}

func meminfoBytes(path string) (total, avail uint64, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	var haveTotal, haveAvail bool
	for _, line := range strings.Split(string(data), "\n") {
		key, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		n *= 1024
		switch key {
		case "MemTotal":
			total, haveTotal = n, true
		case "MemAvailable":
			avail, haveAvail = n, true
		}
	}
	return total, avail, haveTotal && haveAvail
}

func statusRSS(path string) (uint64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		n, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return n * 1024, true
	}
	return 0, false
}
