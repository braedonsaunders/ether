// Package hoststat reads process-tree, network and GPU presence for soak
// telemetry. It does not signal processes.
package hoststat

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Telemetry is one soak sample. Session counts are supplied by the caller.
type Telemetry struct {
	At         time.Time `json:"at"`
	RSSBytes   uint64    `json:"rss_bytes"`
	FDs        int       `json:"fds"`
	Goroutines int       `json:"goroutines"`
	FFmpeg     int       `json:"ffmpeg"`
	Sessions   int       `json:"sessions"`
	Transcodes int       `json:"transcodes"`
	Children   int       `json:"children"`
	RetryQueue int       `json:"retry_queue"`
	NetRx      uint64    `json:"net_rx_bytes"`
	NetTx      uint64    `json:"net_tx_bytes"`
	GPU        string    `json:"gpu"`
	Valid      bool      `json:"valid"`
	Error      string    `json:"error,omitempty"`
}

// Collect reads the process tree, network counters and GPU presence.
func Collect(procRoot, devRoot string, pid, sessions, transcodes int) Telemetry {
	out := Telemetry{At: time.Now(), Goroutines: runtime.NumGoroutine(), Sessions: sessions, Transcodes: transcodes, RetryQueue: transcodes, GPU: GPU(devRoot)}
	tree, err := ProcessTree(procRoot, pid)
	if err != nil {
		out.Error = "process tree unavailable"
		return out
	}
	rx, tx, err := NetDev(procRoot)
	if err != nil {
		out.Error = "network counters unavailable"
		return out
	}
	out.RSSBytes = tree.RSSBytes
	out.FDs = tree.FDs
	out.FFmpeg = len(tree.FFmpegPIDs)
	out.Children = tree.Children
	out.NetRx = rx
	out.NetTx = tx
	out.Valid = true
	return out
}

// Tree is one process and its descendants.
type Tree struct {
	RSSBytes     uint64
	FDs          int
	Children     int
	FFmpegPIDs   []int
	GuardianPIDs []int
}

// ProcessTree sums RSS and file descriptors for pid and its descendants.
// ffmpeg and encoder-guardian pids are listed separately and are included
// in the sums.
func ProcessTree(procRoot string, pid int) (Tree, error) {
	type row struct {
		ppid int
		name string
		rss  uint64
		cmd  []byte
	}
	rows := map[int]row{}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return Tree{}, err
	}
	for _, entry := range entries {
		id, conv := strconv.Atoi(entry.Name())
		if conv != nil || !entry.IsDir() {
			continue
		}
		status, readErr := os.ReadFile(filepath.Join(procRoot, entry.Name(), "status"))
		if readErr != nil {
			continue
		}
		name, ppid, rss, parseErr := parseStatus(status)
		if parseErr != nil {
			continue
		}
		cmd, _ := os.ReadFile(filepath.Join(procRoot, entry.Name(), "cmdline"))
		rows[id] = row{ppid: ppid, name: name, rss: rss, cmd: cmd}
	}
	if _, ok := rows[pid]; !ok {
		return Tree{}, fmt.Errorf("process %d is not in %s", pid, procRoot)
	}
	children := map[int][]int{}
	for id, row := range rows {
		children[row.ppid] = append(children[row.ppid], id)
	}
	var tree Tree
	var walk func(int) error
	seen := map[int]bool{}
	walk = func(id int) error {
		if seen[id] {
			return nil
		}
		seen[id] = true
		row, ok := rows[id]
		if !ok {
			return fmt.Errorf("missing process %d", id)
		}
		fds, err := countFDs(filepath.Join(procRoot, strconv.Itoa(id), "fd"))
		if err != nil {
			return err
		}
		tree.RSSBytes += row.rss
		tree.FDs += fds
		if id != pid {
			tree.Children++
		}
		if isGuardian(row.cmd) && id != pid {
			tree.GuardianPIDs = append(tree.GuardianPIDs, id)
		} else if isFFmpeg(row.name, row.cmd) && id != pid {
			tree.FFmpegPIDs = append(tree.FFmpegPIDs, id)
		}
		for _, child := range children[id] {
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(pid); err != nil {
		return Tree{}, err
	}
	return tree, nil
}

func isGuardian(cmdline []byte) bool {
	return bytes.Contains(cmdline, []byte("--ether-internal-encoder-guardian"))
}

func isFFmpeg(name string, cmdline []byte) bool {
	if name == "ffmpeg" {
		return true
	}
	for _, arg := range bytes.Split(cmdline, []byte{0}) {
		if filepath.Base(string(arg)) == "ffmpeg" {
			return true
		}
	}
	return false
}

func countFDs(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return len(entries), nil
}

func parseStatus(status []byte) (name string, ppid int, rss uint64, err error) {
	ppid = -1
	for _, line := range strings.Split(string(status), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "Name":
			name = value
		case "PPid":
			ppid, err = strconv.Atoi(value)
			if err != nil {
				return "", 0, 0, err
			}
		case "VmRSS":
			fields := strings.Fields(value)
			if len(fields) == 0 {
				return "", 0, 0, fmt.Errorf("empty VmRSS")
			}
			kb, conv := strconv.ParseUint(fields[0], 10, 64)
			if conv != nil {
				return "", 0, 0, conv
			}
			rss = kb * 1024
		}
	}
	if name == "" || ppid < 0 {
		return "", 0, 0, fmt.Errorf("status missing name or ppid")
	}
	return name, ppid, rss, nil
}

// NetDev sums received and transmitted bytes across non-loopback interfaces.
func NetDev(procRoot string) (rx, tx uint64, err error) {
	data, err := os.ReadFile(filepath.Join(procRoot, "net/dev"))
	if err != nil {
		return 0, 0, err
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) < 3 {
		return 0, 0, fmt.Errorf("net/dev is incomplete")
	}
	for _, line := range lines[2:] {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" || name == "lo" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			return 0, 0, fmt.Errorf("net/dev row %s is short", name)
		}
		in, conv := strconv.ParseUint(fields[0], 10, 64)
		if conv != nil {
			return 0, 0, conv
		}
		out, conv := strconv.ParseUint(fields[8], 10, 64)
		if conv != nil {
			return 0, 0, conv
		}
		rx += in
		tx += out
	}
	return rx, tx, nil
}

// GPU reports whether a render device is visible. Utilization is not inferred.
func GPU(devRoot string) string {
	if devRoot == "" {
		devRoot = "/dev"
	}
	if _, err := os.Stat(filepath.Join(devRoot, "nvidia0")); err == nil {
		return "nvidia_present"
	}
	matches, _ := filepath.Glob(filepath.Join(devRoot, "dri", "renderD*"))
	if len(matches) > 0 {
		return "dri_present"
	}
	return "no_device"
}
