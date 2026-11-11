// Package webclient provides the official Jellyfin Web build that the native
// Jellyfin apps (Android, iOS/iPadOS, Desktop, webOS, Xbox) load from
// <server>/web/. Nothing is bundled: the pinned .deb is downloaded from
// repo.jellyfin.org on first use and cached under the data directory.
package webclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Pinned artifact. One place, so an upgrade is a single edit. The URL is the
// stable Debian pool path; repo.jellyfin.org redirects it to a mirror, so the
// downloader must follow redirects (HTTPS only).
const (
	Version = "12.4"
	URL     = "https://repo.jellyfin.org/files/server/debian/stable/v12.4/amd64/jellyfin-web_12.4%2Bdeb13_all.deb"
	SHA256  = "30561fda7e4ec2f48cfc86119b7bdf7f9fe319ebdc22069a5096ba4999198b64"
	Size    = 32062514
)

// Extraction limits guard the disk against a hostile or corrupt archive.
const (
	maxExtractedBytes = 200 << 20 // 200 MiB
	maxExtractedFiles = 20000
)

// backoffDelay is how long a failed download is remembered before a viewer may
// trigger another attempt. Without it a broken mirror would be retried on
// every request to /web.
const backoffDelay = 10 * time.Minute

// webPrefix is the only tree extracted from the data archive.
const webPrefix = "./usr/share/jellyfin/web/"
const copyrightPath = "./usr/share/doc/jellyfin-web/copyright"

// Installer fetches, verifies and caches the pinned Jellyfin Web build.
// The zero value is unusable; construct with New.
type Installer struct {
	// Dir is the install root: Dir/Version holds the extracted files, and a
	// ".complete" marker beside it records the verified version and sha.
	Dir string
	// HTTP downloads the artifact. It should go through Ether's egress when a
	// client is available to the caller; nil falls back to a plain client.
	HTTP *http.Client

	// Overridable in tests; they default to the pinned constants.
	URL    string
	SHA256 string
	Size   int64

	mu     sync.Mutex
	flight *flight
	failed error
	failAt time.Time
	now    func() time.Time
}

type flight struct {
	done chan struct{}
	dir  string
	err  error
}

// VerifyState proves the pinned artifact is trustworthy and reports whether a
// file's sha256 matches. It is used by the extractor and by tests.
func New(dir string, httpClient *http.Client) *Installer {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Minute}
	}
	return &Installer{
		Dir:    dir,
		HTTP:   httpClient,
		URL:    URL,
		SHA256: SHA256,
		Size:   Size,
		now:    time.Now,
	}
}

// Status is the observable state of the local install.
type Status int

const (
	// StatusReady means a verified install is present.
	StatusReady Status = iota
	// StatusDownloading means the first download is in progress.
	StatusDownloading
	// StatusFailed means the last download or extraction failed.
	StatusFailed
)

// State reports the current status without starting any work. Err is non-nil
// only for StatusFailed and carries owner-facing text.
func (in *Installer) State() (Status, error) {
	if in == nil || in.Dir == "" {
		return StatusFailed, errors.New("web client is not configured")
	}
	if _, ok := in.verifyMarker(); ok {
		return StatusReady, nil
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.flight != nil {
		return StatusDownloading, nil
	}
	if in.failed != nil {
		return StatusFailed, in.failed
	}
	return StatusDownloading, nil
}

// Begin starts the first install in the background if none is present or
// running, and reports the current state. It never blocks on the download, so
// a request handler can answer a "preparing" page while it runs. Concurrent
// calls share one download.
func (in *Installer) Begin() (Status, error) {
	if in == nil || in.Dir == "" {
		return StatusFailed, errors.New("web client is not configured")
	}
	if _, ok := in.verifyMarker(); ok {
		return StatusReady, nil
	}
	in.mu.Lock()
	running := in.flight != nil
	failing := in.failed != nil && in.now().Before(in.failAt)
	err := in.failed
	in.mu.Unlock()
	if failing {
		return StatusFailed, err
	}
	if !running {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			_, _ = in.Ensure(ctx)
		}()
	}
	return StatusDownloading, nil
}

// Ensure returns the directory holding the extracted build, installing it on
// first use. Concurrent callers share a single download. After a failure the
// error is remembered for backoffDelay; callers within that window receive the
// same error without another attempt.
func (in *Installer) Ensure(ctx context.Context) (string, error) {
	if in == nil || in.Dir == "" {
		return "", errors.New("web client is not configured")
	}
	if dir, ok := in.verifyMarker(); ok {
		return dir, nil
	}

	in.mu.Lock()
	if in.failed != nil && in.now().Before(in.failAt) {
		err := in.failed
		in.mu.Unlock()
		return "", err
	}
	if f := in.flight; f != nil {
		in.mu.Unlock()
		select {
		case <-f.done:
			return f.dir, f.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f := &flight{done: make(chan struct{})}
	in.flight = f
	in.mu.Unlock()

	dir, err := in.install(ctx)

	in.mu.Lock()
	in.flight = nil
	if err != nil {
		in.failed = err
		in.failAt = in.now().Add(backoffDelay)
	} else {
		in.failed = nil
	}
	f.dir, f.err = dir, err
	close(f.done)
	in.mu.Unlock()
	return dir, err
}

// install downloads and extracts into a temp dir, then atomically renames the
// finished tree into place and writes the marker.
func (in *Installer) install(ctx context.Context) (string, error) {
	parent := filepath.Join(in.Dir, Version)
	if err := os.MkdirAll(in.Dir, 0o700); err != nil {
		return "", fmt.Errorf("create client dir: %w", err)
	}
	tmp, err := os.MkdirTemp(in.Dir, ".install-*")
	if err != nil {
		return "", fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	archive := filepath.Join(tmp, "jellyfin-web.deb")
	if err := in.download(ctx, archive); err != nil {
		return "", err
	}
	f, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := Extract(f, tmp); err != nil {
		return "", err
	}
	_ = os.Remove(archive)

	// Replace any stale install only after the new one is complete.
	if err := os.RemoveAll(parent); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, parent); err != nil {
		return "", fmt.Errorf("publish install: %w", err)
	}
	if err := in.writeMarker(); err != nil {
		return "", err
	}
	return parent, nil
}

func (in *Installer) download(ctx context.Context, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, in.URL, nil)
	if err != nil {
		return err
	}
	resp, err := in.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("download jellyfin web: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download jellyfin web: unexpected status %d", resp.StatusCode)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	h := sha256.New()
	// Read one byte past the declared size so an oversized body is refused
	// rather than silently truncated.
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, in.Size+1))
	closeErr := f.Close()
	if err != nil {
		return fmt.Errorf("download jellyfin web: %w", err)
	}
	if closeErr != nil {
		return closeErr
	}
	if n != in.Size {
		return fmt.Errorf("download jellyfin web: got %d bytes, want %d", n, in.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, in.SHA256) {
		return fmt.Errorf("download jellyfin web: sha256 mismatch (got %s)", got)
	}
	return nil
}

func (in *Installer) markerPath() string {
	return filepath.Join(in.Dir, "version-"+Version+".sha256")
}

// verifyMarker returns the install directory only when the marker exists,
// records the expected version and sha, and the directory is non-empty.
func (in *Installer) verifyMarker() (string, bool) {
	b, err := os.ReadFile(in.markerPath())
	if err != nil {
		return "", false
	}
	fields := strings.Fields(string(b))
	if len(fields) != 3 || fields[0] != Version || !strings.EqualFold(fields[1], in.SHA256) {
		return "", false
	}
	if _, err := strconv.ParseInt(fields[2], 10, 64); err != nil {
		return "", false
	}
	dir := filepath.Join(in.Dir, Version)
	if fi, err := os.Stat(filepath.Join(dir, "index.html")); err != nil || fi.IsDir() {
		return "", false
	}
	return dir, true
}

func (in *Installer) writeMarker() error {
	body := fmt.Sprintf("%s %s %d\n", Version, in.SHA256, in.Size)
	return os.WriteFile(in.markerPath(), []byte(body), 0o600)
}
