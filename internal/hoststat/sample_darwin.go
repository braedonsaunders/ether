//go:build darwin

package hoststat

import (
	"encoding/binary"
	"syscall"

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
	ncpu, err := unix.SysctlUint32("hw.ncpu")
	if err != nil || ncpu == 0 {
		return Sample{}, false
	}
	// Wall-time process share, then divide by cores so 1-of-N is 100/N
	// like Linux /proc (not a per-core value capped at 100).
	procCPU := clampPct(float64(proc-prev.proc) / 1e9 / dt / float64(ncpu) * 100)
	// host_statistics is not reachable in pure Go; 1-minute load / ncpu.
	hostCPU := darwinHostCPU(ncpu)
	// Current RSS needs Mach task_info (cgo). Maxrss is a peak: omit it.
	return Sample{HostCPU: hostCPU, ProcessCPU: procCPU, HostMem: darwinHostMem(), ProcessMem: 0}, true
}

func darwinHostCPU(ncpu uint32) float64 {
	load, ok := darwinLoadavg()
	if !ok {
		return 0
	}
	return clampPct(load / float64(ncpu) * 100)
}

func darwinLoadavg() (float64, bool) {
	raw, err := unix.SysctlRaw("vm.loadavg")
	if err != nil || len(raw) < 16 {
		return 0, false
	}
	ld0 := binary.LittleEndian.Uint32(raw[0:4])
	var fscale uint64
	switch {
	case len(raw) >= 24:
		fscale = binary.LittleEndian.Uint64(raw[16:24])
	default:
		fscale = uint64(binary.LittleEndian.Uint32(raw[12:16]))
	}
	if fscale == 0 {
		return 0, false
	}
	return float64(ld0) / float64(fscale), true
}

func darwinHostMem() float64 {
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil || total == 0 {
		return 0
	}
	free, err := unix.SysctlUint32("vm.page_free_count")
	if err != nil {
		return 0
	}
	inactive, err := unix.SysctlUint32("vm.page_inactive_count")
	if err != nil {
		return 0
	}
	page := uint64(unix.Getpagesize())
	avail := (uint64(free) + uint64(inactive)) * page
	if avail > total {
		avail = total
	}
	return clampPct((1 - float64(avail)/float64(total)) * 100)
}
