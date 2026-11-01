package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	protocol "github.com/braedonsaunders/ether/internal/conform/plex"
)

// expiryCapture is an isolated diagnostic: the staged parity binaries have a
// five-second idle timeout and use a slow, fully offline media fixture. It
// refuses to claim cleanup unless a real ffmpeg was observed alive first.
func expiryCapture(ctx context.Context, client *http.Client, base string, headers http.Header, vars map[string]string) ([]capture, error) {
	pid, ok := ctx.Value(serverPIDKey{}).(int)
	if !ok || pid <= 0 {
		return nil, fmt.Errorf("expiry probe requires the real server PID")
	}
	const session = "parity-expiry"
	h := headers.Clone()
	h.Set("X-Plex-Client-Identifier", session)
	h.Set("X-Plex-Session-Identifier", session)
	h.Set("Accept", "application/vnd.apple.mpegurl")
	var captures []capture
	fetch := func(label, path string, want int) (protocol.Response, error) {
		req := protocol.Request{Method: http.MethodGet, Path: path, Headers: map[string]http.Header{"web": h}}
		r, err := strictFetch(ctx, client, base, "parity-owner", req, "web", vars, false)
		if err != nil || r.Status != want {
			return r, fmt.Errorf("expiry %s: status=%d want=%d: %v", label, r.Status, want, err)
		}
		captures = append(captures, capture{"owner", "web", "playback:expiry:" + label, false, r})
		return r, nil
	}
	q := "?path=%2Flibrary%2Fmetadata%2F{{movieKey}}&protocol=hls&session=" + session + "&directPlay=0&directStream=0"
	master, err := fetch("start", "/video/:/transcode/universal/start.m3u8"+q, http.StatusOK)
	if err != nil || !bytes.HasPrefix(master.Body, []byte("#EXTM3U")) {
		return nil, fmt.Errorf("expiry master playlist: %v", err)
	}
	uri := ""
	for _, line := range strings.Split(string(master.Body), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			uri = line
			break
		}
	}
	if uri == "" || strings.HasPrefix(uri, "/") || strings.Contains(uri, "://") {
		return nil, fmt.Errorf("invalid expiry playlist URI %q", uri)
	}
	playlist := "/video/:/transcode/universal/" + uri
	media, err := fetch("playlist", playlist, http.StatusOK)
	if err != nil || !bytes.HasPrefix(media.Body, []byte("#EXTM3U")) {
		return nil, fmt.Errorf("expiry media playlist: %v", err)
	}
	// The real fast-start plan can chain a second encoder at its initial
	// segment boundary. Observe the actual process until it has been stable
	// for two seconds, then read a real segment before the idle proof.
	// This preparation is bounded and is not evidence of expiry.
	readyDeadline := time.NewTimer(8 * time.Second)
	defer readyDeadline.Stop()
	var children []int
	stablePID := 0
	var stableSince time.Time
	for {
		children, err = encoderChildren("/proc", pid, session)
		if err != nil {
			return nil, fmt.Errorf("expiry readiness process scan: %w", err)
		}
		if len(children) > 1 {
			return nil, fmt.Errorf("expiry readiness found multiple fixture encoders: %v", children)
		}
		if len(children) == 0 {
			stablePID, stableSince = 0, time.Time{}
		} else if children[0] != stablePID {
			stablePID, stableSince = children[0], time.Now()
			fmt.Fprintf(os.Stderr, "expiry readiness: fixture encoder pid=%d\n", stablePID)
		} else if time.Since(stableSince) >= 2*time.Second {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-readyDeadline.C:
			return nil, fmt.Errorf("expiry readiness: fixture encoder did not stabilize")
		case <-time.After(100 * time.Millisecond):
		}
	}
	// This fixture's fast-start run ends at segment 2, the first steady-run
	// segment. Reading it invokes Session.serveSegment's real Touch; polling a
	// playlist intentionally does not touch the transcoder idle timer.
	var segmentURIs []string
	for _, line := range strings.Split(string(media.Body), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			segmentURIs = append(segmentURIs, line)
		}
	}
	if len(segmentURIs) < 3 {
		return nil, fmt.Errorf("expiry readiness playlist lacks steady-run segment")
	}
	playlistURL, err := url.Parse(playlist)
	if err != nil {
		return nil, err
	}
	segmentURL, err := url.Parse(segmentURIs[2])
	if err != nil {
		return nil, err
	}
	resolved := playlistURL.ResolveReference(segmentURL)
	if resolved.IsAbs() || resolved.Host != "" {
		return nil, fmt.Errorf("expiry readiness segment is external")
	}
	if filepath.Base(resolved.Path) != "00002.ts" {
		return nil, fmt.Errorf("expiry readiness unexpected steady segment %q", resolved.Path)
	}
	readyDir, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(stablePID), "cwd"))
	if err != nil {
		return nil, fmt.Errorf("expiry readiness encoder cwd: %w", err)
	}
	segmentFile, err := os.Lstat(filepath.Join(readyDir, "00002.ts"))
	if err != nil || !segmentFile.Mode().IsRegular() || segmentFile.Size() == 0 {
		return nil, fmt.Errorf("expiry readiness steady segment not already produced: %v", err)
	}
	segmentReq := protocol.Request{Method: http.MethodGet, Path: resolved.String(), Headers: map[string]http.Header{"web": h}}
	segment, err := strictFetch(ctx, client, base, "parity-owner", segmentReq, "web", vars, false)
	if err != nil || segment.Status != http.StatusOK || len(segment.Body) == 0 {
		return nil, fmt.Errorf("expiry readiness segment: status=%d bytes=%d: %v", segment.Status, len(segment.Body), err)
	}
	children, err = encoderChildren("/proc", pid, session)
	if err != nil || len(children) != 1 || children[0] != stablePID {
		return nil, fmt.Errorf("expiry encoder changed while reading steady segment: stable=%d current=%v: %v", stablePID, children, err)
	}
	fmt.Fprintf(os.Stderr, "expiry proof: pinned fixture encoder pid=%d after stable readiness and steady segment access\n", stablePID)
	cwd, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(children[0]), "cwd"))
	if err != nil {
		return nil, fmt.Errorf("expiry ffmpeg cwd: %w", err)
	}
	start := time.Now()
	encoderPID := children[0]
	aliveAtIdle := false
	deadline := time.NewTimer(25 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("expiry encoder or session directory remained after idle deadline")
		case <-time.After(250 * time.Millisecond):
			children, err = encoderChildren("/proc", pid, session)
			if err != nil {
				return nil, err
			}
			if len(children) != 0 {
				if len(children) != 1 || children[0] != encoderPID {
					return nil, fmt.Errorf("expiry encoder identity changed: initial=%d current=%v", encoderPID, children)
				}
				if !aliveAtIdle && time.Since(start) >= 5*time.Second {
					aliveAtIdle = true
					fmt.Fprintf(os.Stderr, "expiry proof: pid=%d still alive after %s\n", encoderPID, time.Since(start).Round(time.Millisecond))
				}
				continue
			}
			if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(encoderPID))); err == nil {
				continue // allow guardian to reap the exact encoder PID
			} else if !os.IsNotExist(err) {
				return nil, fmt.Errorf("expiry encoder status: %w", err)
			}
			if !aliveAtIdle {
				return nil, fmt.Errorf("expiry fixture ffmpeg exited before the idle timeout; cleanup not proven")
			}
			if _, err := os.Stat(cwd); err == nil {
				continue // Session.Close waits for watchers before removing files.
			} else if !os.IsNotExist(err) {
				return nil, fmt.Errorf("expiry session directory unreadable: %w", err)
			}
			if _, err := fetch("expired", playlist, http.StatusNotFound); err != nil {
				return nil, err
			}
			fmt.Fprintf(os.Stderr, "expiry proof: pid=%d absent, session directory removed, playlist returned 404 after %s\n", encoderPID, time.Since(start).Round(time.Millisecond))
			return captures, nil
		}
	}
}
