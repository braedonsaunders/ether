package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
)

// copyFixture rejects links and non-regular files so a fixture cannot escape
// its disposable directory. SQLite must be closed and checkpointed first.
func copyFixture(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular fixture file: %s", path)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		closeErr := out.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
}

// runPair is intentionally preliminary: the comparator still exits non-zero
// for incomplete coverage. It never reports a parity pass.
func runPair(ctx context.Context, baseline, candidate, seedDir string, out io.Writer) error {
	dir, err := os.MkdirTemp("", "ether-plexparity-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	tracker := &fixtureTracker{}
	tmdb := httptest.NewServer(trackedTMDBFixture(tracker))
	defer tmdb.Close()
	transcripts := make([]transcript, 0, 2)
	for i, bin := range []string{baseline, candidate} {
		dest := filepath.Join(dir, "server")
		if err := os.RemoveAll(dest); err != nil {
			return err
		}
		if err := copyFixture(seedDir, dest); err != nil {
			return err
		}
		c, err := config.Open(dest)
		if err != nil {
			return err
		}
		addr := "127.0.0.1"
		port := 32400
		if err := c.Update(func(c *config.Config) error {
			c.Server.BindAddress = addr
			c.Server.Port = port
			c.Cache.Dir = filepath.Join(dest, "cache")
			c.Transcode.TempDir = filepath.Join(dest, "transcode")
			return nil
		}); err != nil {
			return err
		}
		listener, err := net.Listen("tcp", net.JoinHostPort(addr, "32400"))
		if err != nil {
			return fmt.Errorf("parity port must be free: %w", err)
		}
		listener.Close()
		log, err := os.Create(filepath.Join(dir, fmt.Sprintf("server-%d.log", i)))
		if err != nil {
			return err
		}
		runCtx, cancel := context.WithCancel(ctx)
		cmd := exec.CommandContext(runCtx, bin, "--data-dir", dest)
		cmd.Stdout = log
		cmd.Stderr = log
		cmd.Env = append(os.Environ(), "ETHER_PARITY_TMDB_URL="+tmdb.URL+"/3")
		if err = cmd.Start(); err != nil {
			cancel()
			log.Close()
			return err
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		url := "http://" + net.JoinHostPort(addr, "32400")
		client := &http.Client{Timeout: time.Second}
		deadline := time.NewTimer(120 * time.Second)
		ready := false
		for !ready {
			select {
			case err := <-done:
				done <- err
				deadline.Stop()
				cancel()
				log.Close()
				return fmt.Errorf("server %d exited during startup: %v", i, err)
			case <-deadline.C:
				cancel()
				<-done
				log.Close()
				return fmt.Errorf("server %d startup timed out", i)
			case <-ctx.Done():
				deadline.Stop()
				cancel()
				<-done
				log.Close()
				return ctx.Err()
			default:
			}
			resp, e := client.Get(url + "/identity")
			if e == nil {
				resp.Body.Close()
				ready = resp.StatusCode == http.StatusOK
			}
			if !ready {
				select {
				case <-ctx.Done():
					deadline.Stop()
					cancel()
					<-done
					log.Close()
					return ctx.Err()
				case <-time.After(100 * time.Millisecond):
				}
			}
		}
		if !deadline.Stop() {
			select {
			case <-deadline.C:
			default:
			}
		}
		captured, captureErr := captureCorpus(ctx, url)
		cancel()
		<-done
		log.Close()
		if missing := tracker.failures(); len(missing) > 0 && captureErr == nil {
			captureErr = fmt.Errorf("TMDB fixture does not implement %v", missing)
		}
		if captureErr != nil {
			b, _ := os.ReadFile(log.Name())
			if len(b) > 8192 {
				b = b[len(b)-8192:]
			}
			fmt.Fprintf(out, "server %d log tail:\n%s\n", i, b)
			return fmt.Errorf("server %d: %w", i, captureErr)
		}
		transcripts = append(transcripts, captured)
	}
	n, err := diffTranscripts(transcripts[0], transcripts[1], out)
	if err != nil {
		return err
	}
	return fmt.Errorf("preliminary corpus found %d differences; parity coverage is incomplete", n)
}
