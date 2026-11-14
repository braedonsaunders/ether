package sessions

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/subtitles"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// The pinned web player cannot select HLS subtitle renditions. Its adapter
// declares this format explicitly; an ordinary External client must receive a
// complete track because it calls response.json() exactly once.
const progressiveSubtitleFormat = "ether-progressive-vtt"

type playbackTextSubtitles interface {
	TextSubtitleCues(int, time.Duration, time.Duration) ([]subtitles.Cue, error)
}

func progressiveTextTracks(protocol string, caps core.ClientCaps, info core.MediaInfo) []int {
	if protocol != "jellyfin" || caps.Profiles == nil || (info.Size > 0 && info.Size <= 32<<20) {
		return nil
	}
	declared := false
	for _, p := range caps.Profiles.Subtitles {
		if p.Format == progressiveSubtitleFormat && strings.EqualFold(p.Method, "external") {
			declared = true
		}
	}
	if !declared {
		return nil
	}
	var tracks []int
	for _, track := range info.Streams {
		if track.Type == "subtitle" && !track.External && transcode.IsTextSubtitle(track.Codec) {
			tracks = append(tracks, track.Index)
		}
	}
	return tracks
}

func (b *Broker) progressiveSubtitle(s *brokerSession, info core.MediaInfo, stream core.StreamInfo) bool {
	if b.deps.Transcoder == nil {
		return false
	}
	if cache, ok := b.deps.Transcoder.(interface {
		CachedSubtitle(context.Context, int, string) ([]byte, bool)
	}); ok {
		if _, cached := cache.CachedSubtitle(b.subtitleContext(context.Background(), s.playID), stream.Index, stream.Codec); cached {
			return false
		}
	}
	if stream.External || (stream.Codec != "subrip" && stream.Codec != "srt" && stream.Codec != "webvtt") {
		return false
	}
	for _, index := range progressiveTextTracks(string(s.public.Protocol), s.caps, info) {
		if index == stream.Index {
			return true
		}
	}
	return false
}

func (b *Broker) serveProgressiveSubtitle(w http.ResponseWriter, r *http.Request, s *brokerSession, stream core.StreamInfo) {
	fromMS, err := strconv.ParseInt(r.URL.Query().Get("from_ms"), 10, 64)
	if err != nil || fromMS < 0 || fromMS > int64((1<<63-1)/int64(time.Millisecond))-180000 {
		http.Error(w, "invalid subtitle position", 400)
		return
	}
	hs, ok := b.transcodeSession(s.public.ID)
	if !ok {
		http.Error(w, "playback subtitles unavailable", 503)
		return
	}
	source, ok := hs.(playbackTextSubtitles)
	if !ok {
		http.Error(w, "playback subtitles unavailable", 503)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	if r.Method == http.MethodHead {
		return
	}
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	started := time.Now()
	from := time.Duration(fromMS) * time.Millisecond
	seen := make(map[subtitles.Cue]bool)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	keepalive := time.NewTicker(4 * time.Second)
	defer keepalive.Stop()
	for {
		position := from + time.Since(started)
		cues, err := source.TextSubtitleCues(stream.Index, max(0, position-30*time.Second), position+120*time.Second)
		if err != nil {
			_, _ = io.WriteString(w, `{"Error":"subtitle unavailable"}`+"\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		var fresh []subtitles.Cue
		for _, cue := range cues {
			if !seen[cue] {
				fresh = append(fresh, cue)
				seen[cue] = true
			}
		}
		for cue := range seen {
			if cue.End < position-30*time.Second {
				delete(seen, cue)
			}
		}
		if len(fresh) > 0 {
			raw, err := jellyfinSubtitleJSON(fresh)
			if err != nil {
				return
			}
			if _, err = w.Write(append(raw, '\n')); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		case <-keepalive.C:
			if _, err := io.WriteString(w, "\n"); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}
