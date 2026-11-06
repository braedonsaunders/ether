// Package mediaproc runs bounded media tools and owns their entire process tree.
package mediaproc

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

const (
	ProbeTimeout  = 30 * time.Second
	EncodeTimeout = 15 * time.Minute
)

// Cmd preserves os/exec's streaming pipes while adding a deadline and tree cleanup.
// Callers must Wait after a successful Start, including on read/parse errors.
type Cmd struct {
	*exec.Cmd
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	cleanup func()
}

func CommandContext(parent context.Context, timeout time.Duration, name string, args ...string) *Cmd {
	if timeout <= 0 {
		timeout = ProbeTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	c := &Cmd{Cmd: exec.CommandContext(ctx, name, args...), ctx: ctx, cancel: cancel}
	Prepare(c.Cmd)
	c.WaitDelay = 2 * time.Second
	c.Cancel = func() error {
		c.mu.Lock()
		defer c.mu.Unlock()
		Stop(c.Cmd)
		if c.cleanup != nil {
			c.cleanup()
		}
		return nil
	}
	return c
}
func (c *Cmd) Start() error {
	c.mu.Lock()
	if err := c.Cmd.Start(); err != nil {
		c.mu.Unlock()
		c.cancel()
		return err
	}
	cleanup, err := Own(c.Cmd)
	if err != nil {
		Stop(c.Cmd)
		c.mu.Unlock()
		_ = c.Cmd.Wait()
		c.cancel()
		return err
	}
	c.cleanup = cleanup
	c.mu.Unlock()
	return nil
}
func (c *Cmd) Wait() error {
	err := c.Cmd.Wait()
	c.mu.Lock()
	Stop(c.Cmd)
	if c.cleanup != nil {
		c.cleanup()
	}
	c.mu.Unlock()
	ctxErr := c.ctx.Err()
	c.cancel()
	if ctxErr != nil {
		return ctxErr
	}
	return err
}
func (c *Cmd) Run() error {
	if err := c.Start(); err != nil {
		return err
	}
	return c.Wait()
}

// capture drains excess diagnostics without retaining unlimited memory.
type capture struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *capture) Write(p []byte) (int, error) {
	n := len(p)
	keep := min(n, b.limit-b.Len())
	if keep > 0 {
		_, _ = b.Buffer.Write(p[:keep])
	}
	if keep < n {
		b.overflow = true
	}
	return n, nil
}
func (c *Cmd) Output() ([]byte, error) {
	out := &capture{limit: 64 << 20}
	diagnostics := &capture{limit: 64 << 10}
	c.Stdout = out
	if c.Stderr == nil {
		c.Stderr = diagnostics
	}
	err := c.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		ee.Stderr = diagnostics.Bytes()
	}
	if err == nil && out.overflow {
		err = fmt.Errorf("media tool output exceeds 64 MiB")
	}
	return out.Bytes(), err
}
func (c *Cmd) CombinedOutput() ([]byte, error) {
	b := &capture{limit: 64 << 10}
	c.Stdout = b
	c.Stderr = b
	err := c.Run()
	return b.Bytes(), err
}
