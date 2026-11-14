package plex

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/subtext"
)

type playbackTextSubtitles interface {
	HasTextSubtitle(int) bool
	TextSubtitleCues(int, time.Duration, time.Duration) ([]subtext.Cue, error)
	ServeTextSubtitlePlaylist(http.ResponseWriter, *http.Request, int)
	ServeTextSubtitleSegment(http.ResponseWriter, *http.Request, int, int)
}

// Native Apple HLS supports WebVTT renditions. Android's Matroska path and
// Plex Web's authored ASS stream retain their existing delivery contracts.
func plexTextRenditions(pr *Request, dec frontend.Decision) bool {
	if pr == nil || dec.BurnSubtitles || dec.Container == frontend.ContainerMatroska {
		return false
	}
	switch strings.ToLower(pr.Platform) {
	case "ios", "tvos":
		return true
	}
	return false
}

func (s *Server) plexTextTracks(pr *Request, info frontend.MediaInfo, dec frontend.Decision) []int {
	if pr == nil || (!plexTextRenditions(pr, dec) && !browserClient(pr)) {
		return nil
	}
	var tracks []int
	for _, track := range info.Streams {
		if track.Type == "subtitle" && !track.External && (track.Codec == "subrip" || track.Codec == "srt" || track.Codec == "webvtt" || track.Codec == "vtt") {
			tracks = append(tracks, track.Index)
		}
	}
	return tracks
}

func (s *Server) textRenditionTags(r *http.Request, id string, hs frontend.HLSSession, entry *transcodeEntry, info frontend.MediaInfo, dec frontend.Decision) (string, string) {
	source, ok := hs.(playbackTextSubtitles)
	if !ok || entry == nil || !plexTextRenditions(FromContext(r.Context()), dec) {
		return "", ""
	}
	var tags strings.Builder
	for pos, track := range info.Streams {
		if track.External || track.Type != "subtitle" || !source.HasTextSubtitle(track.Index) {
			continue
		}
		selected := "NO"
		if pos == entry.subtitle {
			selected = "YES"
		}
		forced := "NO"
		if track.Forced {
			forced = "YES"
		}
		// All values in quoted HLS attributes must fit one line.
		clean := func(v string) string { return strings.NewReplacer("\"", "'", "\r", " ", "\n", " ").Replace(v) }
		uri := s.mediaURL(fmt.Sprintf("session/%s/text-%d/index.m3u8", id, track.Index), id, entry.streamToken)
		fmt.Fprintf(&tags, "#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"text\",NAME=\"%s (%d)\",LANGUAGE=\"%s\",DEFAULT=%s,AUTOSELECT=%s,FORCED=%s,URI=\"%s\"\n", clean(subtitleDisplayTitle(track)), track.Index, clean(track.Language), selected, selected, forced, uri)
	}
	if tags.Len() == 0 {
		return "", ""
	}
	return tags.String(), ",SUBTITLES=\"text\""
}

// Streaming sidecars and native ASS consumers attach to any tee'd track
// without restarting video or reading the release again. HLS renditions serve
// native players that buffer a whole sidecar before parsing it.
func (s *Server) serveCueSubtitle(w http.ResponseWriter, r *http.Request, source playbackTextSubtitles, index int, playID string, from time.Duration, format string, matroska bool) {
	w.Header().Set("Content-Type", subtitleContentTypes[format])
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	if r.Method == http.MethodHead {
		return
	}
	header := subtext.ASSHeader
	if format == "srt" {
		header = "\n"
	}
	if format == "vtt" {
		header = "WEBVTT\n\n"
		if matroska {
			header = string(stampMatroskaVTT([]byte(header)))
		}
	}
	if _, err := w.Write([]byte(header)); err != nil {
		return
	}
	flush, _ := w.(http.Flusher)
	if flush != nil {
		flush.Flush()
	}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	alive := time.NewTicker(subtitleKeepAlive)
	defer alive.Stop()
	start := time.Now()
	seen := map[subtext.Cue]bool{}
	sequence := 0
	for {
		if !source.HasTextSubtitle(index) {
			return
		}
		at := from + time.Since(start)
		if ps, ok := s.playback().Session(playID); ok && ps != nil && ps.ViewOffset != nil {
			at = time.Duration(*ps.ViewOffset) * time.Millisecond
		}
		cues, err := source.TextSubtitleCues(index, max(0, at-30*time.Second), at+120*time.Second)
		if err != nil {
			return
		}
		var fresh []subtext.Cue
		for _, cue := range cues {
			if !seen[cue] {
				fresh = append(fresh, cue)
				seen[cue] = true
			}
		}
		for cue := range seen {
			if cue.End < at-30*time.Second {
				delete(seen, cue)
			}
		}
		if len(fresh) > 0 {
			body := strings.TrimPrefix(subtext.RenderASS(fresh), subtext.ASSHeader)
			if format == "vtt" {
				body = string(vttCues([]byte(subtext.RenderVTT(fresh))))
			}
			if format == "srt" {
				var numbered strings.Builder
				for _, block := range strings.Split(strings.TrimSpace(subtext.RenderSRT(fresh)), "\n\n") {
					if _, rest, ok := strings.Cut(block, "\n"); ok {
						sequence++
						numbered.WriteString(strconv.Itoa(sequence) + "\n" + rest + "\n\n")
					}
				}
				body = numbered.String()
			}
			if _, err := w.Write([]byte(body)); err != nil {
				return
			}
			if flush != nil {
				flush.Flush()
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		case <-alive.C:
			if _, err := w.Write([]byte("\n")); err != nil {
				return
			}
			if flush != nil {
				flush.Flush()
			}
		}
	}
}
