package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type serverPIDKey struct{}

// encoderChildren reads Linux procfs for ffmpeg descendants of the tested server
// whose working directory belongs to this exact fixture session. A missing
// procfs or unreadable live process is an error, never evidence of cleanup.
func encoderChildren(procRoot string, serverPID int, session string) ([]int, error) {
	if serverPID <= 0 || session == "" || strings.ContainsAny(session, "/\\") {
		return nil, fmt.Errorf("invalid server PID or session")
	}
	var out []int
	queue := []int{serverPID}
	seen := map[int]bool{serverPID: true}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		tasks, err := os.ReadDir(filepath.Join(procRoot, strconv.Itoa(parent), "task"))
		if os.IsNotExist(err) && parent != serverPID {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read process tasks: %w", err)
		}
		for _, task := range tasks {
			b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(parent), "task", task.Name(), "children"))
			if os.IsNotExist(err) && parent != serverPID {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("read process children: %w", err)
			}
			for _, field := range strings.Fields(string(b)) {
				pid, err := strconv.Atoi(field)
				if err != nil || pid <= 0 {
					return nil, fmt.Errorf("invalid child PID %q", field)
				}
				if seen[pid] {
					continue
				}
				seen[pid] = true
				queue = append(queue, pid)
				dir := filepath.Join(procRoot, field)
				cmd, err := os.ReadFile(filepath.Join(dir, "cmdline"))
				if os.IsNotExist(err) {
					continue // process exited between the two procfs reads
				}
				if err != nil {
					return nil, err
				}
				args := strings.Split(string(cmd), "\x00")
				if len(args) == 0 || filepath.Base(args[0]) != "ffmpeg" {
					continue
				}
				cwd, err := os.Readlink(filepath.Join(dir, "cwd"))
				if os.IsNotExist(err) {
					continue
				}
				if err != nil {
					return nil, err
				}
				if strings.HasPrefix(filepath.Base(cwd), "s-"+session+"-") {
					out = append(out, pid)
				}
			}
		}
	}
	sort.Ints(out)
	return out, nil
}
