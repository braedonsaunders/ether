package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

// buildServer compiles cmd/ether from the current tree, CGO-free as shipped.
func buildServer(ctx context.Context, out string) error {
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", out, "./cmd/ether")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd.Run()
}

// sandboxProfile denies every outbound connection except loopback, so a
// server under test cannot reach a provider, plex.tv or the owner's network
// even if a setting was missed by sanitizeConfig.
const sandboxProfile = `(version 1)(allow default)(deny network-outbound)(allow network-outbound (remote ip "localhost:*"))(allow network-outbound (remote unix-socket))`

type server struct {
	cmd     *exec.Cmd
	port    int
	started time.Time
	ready   time.Duration
	exited  chan struct{} // closed when the process has exited
	err     error
}

// startServer runs bin on dataDir with outbound network blocked. On macOS it
// uses sandbox-exec; elsewhere it uses an unshared network namespace when
// unshare is available, and otherwise relies on the sanitized settings.
func startServer(ctx context.Context, bin, dataDir string, port int, trace, logPath string) (*server, error) {
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	args := []string{bin, "-data-dir", dataDir}
	switch {
	case runtime.GOOS == "darwin":
		args = append([]string{"sandbox-exec", "-p", sandboxProfile}, args...)
	case runtime.GOOS == "linux" && os.Getenv("ETHERPERF_NO_NETNS") == "":
		if _, err := exec.LookPath("unshare"); err == nil {
			// A fresh user+network namespace has only a loopback device.
			args = append([]string{"unshare", "--user", "--map-root-user", "--net", "sh", "-c", `ip link set lo up 2>/dev/null || true; exec "$0" "$@"`}, args...)
		}
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "ETHER_PORT="+strconv.Itoa(port), "ETHER_PERF_TRACE="+trace, "ETHER_DATA_DIR="+dataDir, "ETHER_PPROF_ADDR=127.0.0.1:"+strconv.Itoa(port+9))
	cmd.Stdout, cmd.Stderr = logFile, logFile
	setProcessGroup(cmd)
	s := &server{cmd: cmd, port: port, started: time.Now(), exited: make(chan struct{})}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, err
	}
	go func() {
		s.err = cmd.Wait()
		logFile.Close()
		close(s.exited)
	}()
	if err := s.waitReady(ctx); err != nil {
		s.stop()
		return nil, err
	}
	return s, nil
}

func (s *server) base() string { return "http://127.0.0.1:" + strconv.Itoa(s.port) }

func (s *server) waitReady(ctx context.Context) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-s.exited:
			return fmt.Errorf("server exited during startup: %v", s.err)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
		resp, err := client.Get(s.base() + "/identity")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				s.ready = time.Since(s.started)
				return nil
			}
		}
	}
	return errors.New("server did not become ready within 5 minutes")
}

// stop interrupts the server's process group and waits for it to exit. It
// may be called more than once.
func (s *server) stop() {
	if s == nil || s.cmd.Process == nil {
		return
	}
	select {
	case <-s.exited:
		return
	default:
	}
	interruptGroup(s.cmd.Process)
	select {
	case <-s.exited:
	case <-time.After(20 * time.Second):
		killGroup(s.cmd.Process)
		<-s.exited
	}
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}
