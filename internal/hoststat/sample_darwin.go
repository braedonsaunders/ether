//go:build darwin

package hoststat

import (
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func (r *Resources) hostSample() (Sample, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return Sample{}, false
	}
	proc := uint64(ru.Utime.Nano()) + uint64(ru.Stime.Nano())
	now := r.now()
	r.cpu.mu.Lock()
	prev, ready := r.cpu.last, r.cpu.have
	r.cpu.last = cpuSnap{proc: proc, wall: now}
	r.cpu.have = true
	r.cpu.mu.Unlock()
	if !ready {
		return Sample{}, false
	}
	dt := now.Sub(prev.wall).Seconds()
	if dt <= 0 || proc < prev.proc {
		return Sample{}, false
	}
	procCPU := clampPct(float64(proc-prev.proc) / 1e9 / dt * 100)
	hostCPU := darwinHostCPU()
	hostMem, procMem := darwinMem(uint64(ru.Maxrss))
	return Sample{HostCPU: hostCPU, ProcessCPU: procCPU, HostMem: hostMem, ProcessMem: procMem}, true
}

func darwinHostCPU() float64 {
	ncpu, err := unix.SysctlUint32("hw.ncpu")
	if err != nil || ncpu == 0 {
		return 0
	}
	var load [3]float64
	if _, err := unix.Getloadavg(&load, 3); err != nil {
		return 0
	}
	return clampPct(load[0] / float64(ncpu) * 100)
}

func darwinMem(rss uint64) (host, proc float64) {
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil || total == 0 {
		return 0, 0
	}
	page := uint64(unix.Getpagesize())
	free, _ := unix.SysctlUint64("vm.page_free_count")
	inactive, _ := unix.SysctlUint64("vm.page_inactive_count")
	avail := (free + inactive) * page
	if avail > total {
		avail = total
	}
	host = clampPct((1 - float64(avail)/float64(total)) * 100)
	proc = float64(rss) / float64(total) * 100
	return host, proc
}
