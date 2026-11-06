//go:build linux

package hostfenceprobe

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Report records access measurements only. Even an empty cgroup or dead task
// cannot turn this probe into a retirement receipt. Handles close before return.
type Report struct {
	Version            int     `json:"version"`
	Purpose            string  `json:"purpose"`
	Stage              string  `json:"stage"`
	AccessComplete     bool    `json:"accessComplete"`
	Binding            Binding `json:"binding"`
	StartTicks         uint64  `json:"startTicks,omitempty"`
	PIDNamespace       uint64  `json:"pidNamespace,omitempty"`
	ParentPIDNamespace uint64  `json:"parentPIDNamespace,omitempty"`
	CgroupDevice       uint64  `json:"cgroupDevice,omitempty"`
	CgroupInode        uint64  `json:"cgroupInode,omitempty"`
	Populated          string  `json:"populated,omitempty"`
}

func read(f *os.File) (string, error) {
	if _, err := f.Seek(0, 0); err != nil {
		return "", err
	}
	b, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(b) > 16384 {
		return "", errors.New("bounded read failed")
	}
	return string(b), nil
}

func openAt(fd int, name string, flags int) (*os.File, error) {
	n, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_CLOEXEC|flags, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(n), name), nil
}

func readAt(fd int, name string) (string, error) {
	f, err := openAt(fd, name, unix.O_NOFOLLOW)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return read(f)
}

func statTicks(s string, pid int) (uint64, error) {
	// comm may contain spaces and closing parentheses. Fields after the last
	// parenthesis begin at field 3; starttime is field 22.
	end := strings.LastIndex(s, ")")
	if !strings.HasPrefix(s, strconv.Itoa(pid)+" (") || end < 0 {
		return 0, errors.New("stat identity")
	}
	f := strings.Fields(s[end+1:])
	if len(f) < 20 {
		return 0, errors.New("stat fields")
	}
	n, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil || n == 0 {
		return 0, errors.New("stat ticks")
	}
	return n, nil
}

func namespaceInit(status string, hostPID int) bool {
	for _, line := range strings.Split(status, "\n") {
		if strings.HasPrefix(line, "NSpid:") {
			f := strings.Fields(line)
			return len(f) >= 3 && f[1] == strconv.Itoa(hostPID) && f[len(f)-1] == "1"
		}
	}
	return false
}

func observerVisible(status string, pid int) bool {
	for _, line := range strings.Split(status, "\n") {
		if strings.HasPrefix(line, "NSpid:") {
			f := strings.Fields(line)
			return len(f) == 2 && f[1] == strconv.Itoa(pid)
		}
	}
	return false
}

func populated(s string) (string, error) {
	value := ""
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			return "", errors.New("events format")
		}
		if f[0] == "populated" {
			if value != "" || (f[1] != "0" && f[1] != "1") {
				return "", errors.New("events populated")
			}
			value = f[1]
		}
	}
	if value == "" {
		return "", errors.New("events missing populated")
	}
	return value, nil
}

func alive(fd int) error {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, 0)
	if err != nil {
		return err
	}
	if n != 0 || fds[0].Revents != 0 {
		return errors.New("task not live")
	}
	return nil
}

// Probe opens retained references while the expected init is alive and brackets
// them with identity rechecks. It never signals, joins namespaces, writes proc or
// cgroup files, accesses Docker, starts goroutines, or waits for task death.
func Probe(procRoot, cgroupRoot string, b Binding) (out Report, err error) {
	out = Report{Version: 1, Purpose: "ether.host-access-probe.v1", Stage: "binding", Binding: b}
	defer func() {
		if err != nil {
			err = fmt.Errorf("probe %s: %w", out.Stage, err)
		}
	}()
	if err = b.Validate(); err != nil {
		return
	}
	out.Stage = "host-boot"
	proc, e := os.Open(procRoot)
	if e != nil {
		err = e
		return
	}
	defer proc.Close()
	var fs unix.Statfs_t
	if err = unix.Fstatfs(int(proc.Fd()), &fs); err != nil {
		return
	}
	if fs.Type != unix.PROC_SUPER_MAGIC {
		err = errors.New("not procfs")
		return
	}
	boot, e := readAt(int(proc.Fd()), "sys/kernel/random/boot_id")
	if e != nil {
		err = e
		return
	}
	if strings.TrimSpace(boot) != b.HostBoot {
		err = errors.New("host boot mismatch")
		return
	}
	out.Stage = "observer-vantage"
	selfStatus, e := readAt(int(proc.Fd()), "self/status")
	if e != nil {
		err = e
		return
	}
	if !observerVisible(selfStatus, os.Getpid()) {
		err = errors.New("observer PID namespace differs from procfs vantage")
		return
	}
	out.Stage = "task"
	task, e := openAt(int(proc.Fd()), strconv.Itoa(b.PID), unix.O_DIRECTORY|unix.O_NOFOLLOW)
	if e != nil {
		err = e
		return
	}
	defer task.Close()
	stat, e := readAt(int(task.Fd()), "stat")
	if e != nil {
		err = e
		return
	}
	out.StartTicks, err = statTicks(stat, b.PID)
	if err != nil {
		return
	}
	status, e := readAt(int(task.Fd()), "status")
	if e != nil {
		err = e
		return
	}
	if !namespaceInit(status, b.PID) {
		err = errors.New("not host-visible nested namespace init")
		return
	}
	out.Stage = "pidfd"
	pidfd, e := unix.PidfdOpen(b.PID, 0)
	if e != nil {
		err = e
		return
	}
	defer unix.Close(pidfd)
	if err = alive(pidfd); err != nil {
		return
	}
	out.Stage = "namespace"
	ns, e := openAt(int(task.Fd()), "ns/pid", 0)
	if e != nil {
		err = e
		return
	}
	defer ns.Close()
	var st unix.Stat_t
	if err = unix.Fstat(int(ns.Fd()), &st); err != nil {
		return
	}
	out.PIDNamespace = st.Ino
	parent, e := unix.IoctlRetInt(int(ns.Fd()), unix.NS_GET_PARENT)
	if e != nil {
		err = e
		return
	}
	defer unix.Close(parent)
	if err = unix.Fstat(parent, &st); err != nil {
		return
	}
	out.ParentPIDNamespace = st.Ino
	out.Stage = "cgroup-binding"
	cg, e := readAt(int(task.Fd()), "cgroup")
	if e != nil {
		err = e
		return
	}
	if strings.TrimSpace(cg) != "0::"+b.Cgroup {
		err = errors.New("task cgroup mismatch or non-unified hierarchy")
		return
	}
	root, e := os.Open(cgroupRoot)
	if e != nil {
		err = e
		return
	}
	defer root.Close()
	if err = unix.Fstatfs(int(root.Fd()), &fs); err != nil {
		return
	}
	if fs.Type != unix.CGROUP2_SUPER_MAGIC {
		err = errors.New("not cgroup v2")
		return
	}
	// openat2 prevents symlink/mount traversal and escaping the selected hierarchy.
	fd, e := unix.Openat2(int(root.Fd()), strings.TrimPrefix(b.Cgroup, "/"), &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV})
	if e != nil {
		err = e
		return
	}
	defer unix.Close(fd)
	if err = unix.Fstat(fd, &st); err != nil {
		return
	}
	out.CgroupDevice = uint64(st.Dev)
	out.CgroupInode = st.Ino
	out.Stage = "cgroup-events"
	events, e := openAt(fd, "cgroup.events", unix.O_NOFOLLOW)
	if e != nil {
		err = e
		return
	}
	defer events.Close()
	raw, e := read(events)
	if e != nil {
		err = e
		return
	}
	out.Populated, err = populated(raw)
	if err != nil {
		return
	}
	out.Stage = "identity-recheck"
	stat, e = readAt(int(task.Fd()), "stat")
	if e != nil {
		err = e
		return
	}
	ticks, e := statTicks(stat, b.PID)
	if e != nil {
		err = e
		return
	}
	if ticks != out.StartTicks {
		err = errors.New("task changed")
		return
	}
	cg, e = readAt(int(task.Fd()), "cgroup")
	if e != nil {
		err = e
		return
	}
	if strings.TrimSpace(cg) != "0::"+b.Cgroup {
		err = errors.New("task migrated")
		return
	}
	if err = alive(pidfd); err != nil {
		return
	}
	nsAgain, e := openAt(int(task.Fd()), "ns/pid", 0)
	if e != nil {
		err = e
		return
	}
	defer nsAgain.Close()
	if err = unix.Fstat(int(nsAgain.Fd()), &st); err != nil {
		return
	}
	if st.Ino != out.PIDNamespace {
		err = errors.New("task namespace changed")
		return
	}
	if out.Populated != "1" {
		err = errors.New("live task in empty cgroup: inconsistent vantage")
		return
	}
	out.Stage = "complete"
	out.AccessComplete = true
	return
}
