package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// writeFDSnapshot records ls -l /proc/<pid>/fd for the process and its
// descendants, plus the network tables needed to name sockets. The files
// land in dir before the soak process exits. An empty dir skips the snapshot.
func writeFDSnapshot(dir, label string) error {
	if dir == "" {
		return nil
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("fd snapshot requires linux proc")
	}
	if label == "" || strings.ContainsAny(label, `/\`) {
		return fmt.Errorf("fd snapshot label %q", label)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	pids := []int{os.Getpid()}
	kids, err := descendantPIDs("/proc", os.Getpid())
	if err != nil {
		return err
	}
	pids = append(pids, kids...)
	var fdBuf bytes.Buffer
	for _, pid := range pids {
		cmd, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		fmt.Fprintf(&fdBuf, "# pid %d cmd %s\n", pid, strings.ReplaceAll(string(cmd), "\x00", " "))
		ls := exec.Command("ls", "-l", fmt.Sprintf("/proc/%d/fd", pid))
		ls.Stdout = &fdBuf
		ls.Stderr = &fdBuf
		if err := ls.Run(); err != nil {
			return fmt.Errorf("ls /proc/%d/fd: %w", pid, err)
		}
		fmt.Fprintf(&fdBuf, "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "fd-"+label+".txt"), fdBuf.Bytes(), 0600); err != nil {
		return err
	}
	for _, name := range []string{"tcp", "tcp6", "unix"} {
		body, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/%s", os.Getpid(), name))
		if err != nil {
			return fmt.Errorf("read /proc/net/%s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+"-"+label+".txt"), body, 0600); err != nil {
			return err
		}
	}
	return nil
}

func descendantPIDs(proc string, root int) ([]int, error) {
	entries, err := os.ReadDir(proc)
	if err != nil {
		return nil, err
	}
	children := map[int][]int{}
	for _, entry := range entries {
		pid, conv := strconv.Atoi(entry.Name())
		if conv != nil || !entry.IsDir() {
			continue
		}
		status, readErr := os.ReadFile(filepath.Join(proc, entry.Name(), "status"))
		if readErr != nil {
			continue
		}
		ppid := -1
		for _, line := range strings.Split(string(status), "\n") {
			key, value, ok := strings.Cut(line, ":")
			if ok && key == "PPid" {
				ppid, _ = strconv.Atoi(strings.TrimSpace(value))
				break
			}
		}
		if ppid > 0 {
			children[ppid] = append(children[ppid], pid)
		}
	}
	var out []int
	var walk func(int)
	walk = func(pid int) {
		for _, child := range children[pid] {
			out = append(out, child)
			walk(child)
		}
	}
	walk(root)
	return out, nil
}
