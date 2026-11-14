package transcode

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/subtitles"
)

// HasTextSubtitle reports tracks attached to this playback's source read.
func (s *Session) HasTextSubtitle(index int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.textSubtitleSource == "" {
		return false
	}
	for _, track := range s.textSubtitleStreams {
		if track == index {
			return true
		}
	}
	return false
}

// ServeTextSubtitlePlaylist follows the video plan, including its ramp and
// discontinuities. Each subtitle fragment is fetched independently, so native
// HLS clients never wait for a complete sidecar or retain an incomplete one.
func (s *Session) ServeTextSubtitlePlaylist(w http.ResponseWriter, r *http.Request, index int) {
	if !s.HasTextSubtitle(index) {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	lines := strings.Split(string(s.renderPlaylistLocked(0)), "\n")
	s.mu.Unlock()
	var body strings.Builder
	for _, line := range lines {
		if strings.HasPrefix(line, "#EXT-X-MAP:") || line == "#EXT-X-INDEPENDENT-SEGMENTS" {
			continue
		}
		if line != "" && line[0] != '#' {
			n, ok := ParseSegmentName(line)
			if !ok {
				http.Error(w, "subtitle timeline unavailable", 503)
				return
			}
			line = fmt.Sprintf("%05d.vtt", n)
		}
		body.WriteString(line + "\n")
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body.String()))
	}
}

// ServeTextSubtitleSegment waits for the matching video window. A subtitle
// request never seeks or starts an encoder: only video requests drive playback.
func (s *Session) ServeTextSubtitleSegment(w http.ResponseWriter, r *http.Request, index, n int) {
	if !s.HasTextSubtitle(index) || n < 0 {
		http.NotFound(w, r)
		return
	}
	timer := time.NewTimer(s.m.opts.segmentTimeout)
	defer timer.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	var from, to time.Duration
	for {
		s.mu.Lock()
		if s.closed || (s.total > 0 && n >= s.total) {
			s.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		from, to = s.segStart(n), s.segStart(n+1)
		ready := false
		for _, track := range s.done {
			if track[n] {
				ready = true
				break
			}
		}
		s.mu.Unlock()
		if ready {
			break
		}
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			http.Error(w, "subtitle window unavailable", 503)
			return
		case <-tick.C:
		}
	}
	cues, err := s.TextSubtitleCues(index, from, to)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sort.SliceStable(cues, func(i, j int) bool { return cues[i].Start < cues[j].Start })
	body := subtitles.RenderVTT(cues)
	// fMP4 timestamps and subtitle cues use the title clock. TS's child muxer
	// adds 1.4 seconds to that clock (the usual HLS transport offset).
	s.mu.Lock()
	offset := int64(0)
	if s.container == ContainerMPEGTS {
		offset = 126000
	}
	s.mu.Unlock()
	body = strings.Replace(body, "WEBVTT\n", "WEBVTT\nX-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:"+strconv.FormatInt(offset, 10)+"\n", 1)
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
}
