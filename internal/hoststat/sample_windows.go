//go:build windows

package hoststat

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// x/sys/windows has no wrappers for these three; the lazy procs stay package-level.
var (
	modkernel32              = windows.NewLazySystemDLL("kernel32.dll")
	modpsapi                 = windows.NewLazySystemDLL("psapi.dll")
	procGetSystemTimes       = modkernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = modkernel32.NewProc("GlobalMemoryStatusEx")
	procGetProcessMemoryInfo = modpsapi.NewProc("GetProcessMemoryInfo")
)

type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func (r *Resources) hostSample() (Sample, bool) {
	var idle, kernel, user windows.Filetime
	if err := getSystemTimes(&idle, &kernel, &user); err != nil {
		return Sample{}, false
	}
	var create, exit, kproc, uproc windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &create, &exit, &kproc, &uproc); err != nil {
		return Sample{}, false
	}
	hostTotal := filetimeUint(kernel) + filetimeUint(user)
	hostIdle := filetimeUint(idle)
	proc := filetimeUint(kproc) + filetimeUint(uproc)
	now := r.now()
	r.cpu.mu.Lock()
	prev, ready := r.cpu.last, r.cpu.have
	r.cpu.last = cpuSnap{total: hostTotal, idle: hostIdle, proc: proc, wall: now}
	r.cpu.have = true
	r.cpu.mu.Unlock()
	if !ready || hostTotal <= prev.total {
		return Sample{}, false
	}
	dt := float64(hostTotal - prev.total)
	hostCPU := clampPct((1 - float64(hostIdle-prev.idle)/dt) * 100)
	procCPU := clampPct(float64(proc-prev.proc) / dt * 100)
	hostMem, procMem := windowsMem()
	return Sample{HostCPU: hostCPU, ProcessCPU: procCPU, HostMem: hostMem, ProcessMem: procMem}, true
}

func getSystemTimes(idle, kernel, user *windows.Filetime) error {
	r1, _, err := procGetSystemTimes.Call(uintptr(unsafe.Pointer(idle)), uintptr(unsafe.Pointer(kernel)), uintptr(unsafe.Pointer(user)))
	if r1 == 0 {
		return err
	}
	return nil
}

func filetimeUint(ft windows.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

func windowsMem() (host, proc float64) {
	var st memoryStatusEx
	st.Length = uint32(unsafe.Sizeof(st))
	r1, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&st)))
	if r1 == 0 || st.TotalPhys == 0 {
		return 0, 0
	}
	host = clampPct(float64(st.MemoryLoad))
	var pmc processMemoryCounters
	pmc.CB = uint32(unsafe.Sizeof(pmc))
	ok, _, _ := procGetProcessMemoryInfo.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.CB))
	if ok != 0 {
		proc = float64(pmc.WorkingSetSize) / float64(st.TotalPhys) * 100
	}
	return host, proc
}
