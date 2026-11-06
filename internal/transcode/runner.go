package transcode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Command describes one ffmpeg/ffprobe invocation.
type Command struct {
	Path   string
	Args   []string
	Dir    string
	Stdout io.Writer
	Stderr io.Writer
}

// Process is a started long-running command (an ffmpeg HLS run).
type Process interface {
	// Done is closed when the process has exited.
	Done() <-chan struct{}
	// Err is the exit error; valid once Done is closed.
	Err() error
	// Stop kills the process and blocks until it is gone. It is safe to
	// call more than once and after exit.
	Stop()
	// Pause suspends the process (throttling); ErrPauseUnsupported where
	// the platform cannot.
	Pause() error
	// Resume continues a paused process.
	Resume() error
}

// Runner starts processes. The default runs real executables; tests inject
// fakes.
type Runner interface {
	// Start launches a long-running command.
	Start(cmd Command) (Process, error)
	// Run executes a command to completion, killing it when ctx ends.
	Run(ctx context.Context, cmd Command) error
}

// ErrPauseUnsupported is returned by Process.Pause on platforms that cannot
// suspend a process.
var ErrPauseUnsupported = errors.New("transcode: pausing processes is not supported on this platform")

// waitDelay bounds how long Run waits for output pipes after a kill.
const waitDelay = 4 * time.Second

// ExecRunner runs real executables with os/exec.
type ExecRunner struct{}

// A media process has a hard lifetime ceiling even if a caller loses its session.
const maxProcessLifetime = 24 * time.Hour

// Start implements Runner.
func (ExecRunner) Start(c Command) (Process, error) {
	path, err := exec.LookPath(c.Path)
	if err != nil {
		return nil, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	control, lifetime, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(executable, append([]string{guardianArg, path}, c.Args...)...)
	cmd.Env = append(os.Environ(), guardianEnv+"=1")
	cmd.Stdin = control
	cmd.WaitDelay = waitDelay
	cmd.Dir = c.Dir
	out := c.Stdout
	if out == nil {
		out = io.Discard
	}
	writeMu := &sync.Mutex{}
	output := &encoderOutput{dst: &synchronizedWriter{mu: writeMu, dst: out}, pid: make(chan int, 1)}
	cmd.Stdout = output
	if c.Stderr != nil {
		cmd.Stderr = &synchronizedWriter{mu: writeMu, dst: c.Stderr}
	}
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		control.Close()
		lifetime.Close()
		return nil, err
	}
	control.Close()
	p := &execProcess{cmd: cmd, lifetime: lifetime, done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), maxProcessLifetime)
	go func() {
		defer cancel()
		select {
		case <-ctx.Done():
			p.Stop()
		case <-p.done:
		}
	}()
	go func() {
		p.err = cmd.Wait()
		lifetime.Close()
		close(p.done)
	}()
	select {
	case p.pid = <-output.pid:
		return p, nil
	case <-p.done:
		select {
		case p.pid = <-output.pid:
			return p, nil
		default:
		}
		return nil, fmt.Errorf("transcode: encoder guardian failed: %w", p.err)
	}
}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, c Command) error {
	ctx, cancel := context.WithTimeout(ctx, maxProcessLifetime)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := (ExecRunner{}).Start(c)
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		p.Stop()
		return ctx.Err()
	case <-p.Done():
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return p.Err()
	}

}

type execProcess struct {
	cmd      *exec.Cmd
	lifetime *os.File
	pid      int
	done     chan struct{}
	err      error
	stopOnce sync.Once
	mu       sync.Mutex
	paused   bool
}

func (p *execProcess) Done() <-chan struct{} { return p.done }

// PID identifies the encoder for optional process-tree resource sampling.
// It is deliberately not part of Process, so fake and alternate runners do
// not need to expose an operating-system process.
func (p *execProcess) PID() int { return p.pid }

// CPUTime reports completed process CPU usage, excluding wall-clock waits.
// While the process is running the values are zero; callers should sample
// only after Done closes. The channel synchronizes access to ProcessState.
func (p *execProcess) CPUTime() (user, system time.Duration) {
	select {
	case <-p.done:
		if state := p.cmd.ProcessState; state != nil {
			return state.UserTime(), state.SystemTime()
		}
	default:
	}
	return 0, 0
}

func (p *execProcess) Err() error {
	<-p.done
	return p.err
}

func (p *execProcess) Stop() {
	p.stopOnce.Do(func() {
		select {
		case <-p.done:
		default:
			// Segments and playlists are written to temp files and renamed,
			// so a hard kill never leaves a half-written segment visible.
			_ = p.lifetime.Close()
		}
	})
	<-p.done
}

func (p *execProcess) Pause() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.paused {
		return nil
	}
	proc, err := os.FindProcess(p.pid)
	if err != nil {
		return err
	}
	defer proc.Release()
	if err := suspend(proc); err != nil {
		return err
	}
	p.paused = true
	return nil
}

func (p *execProcess) Resume() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.paused {
		return nil
	}
	proc, err := os.FindProcess(p.pid)
	if err != nil {
		return err
	}
	defer proc.Release()
	if err := resume(proc); err != nil {
		return err
	}
	p.paused = false
	return nil
}

// Commands frequently combine stdout and stderr into a bytes.Buffer. Preserve
// os/exec's serialization even though stdout includes a private PID handshake.
type synchronizedWriter struct {
	mu  *sync.Mutex
	dst io.Writer
}

func (w *synchronizedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dst.Write(p)
}
