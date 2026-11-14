package sessions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/subtitles"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// SubtitleURL authorizes a subtitle by the session's probed stream index.
// The returned URL contains only the scoped stream capability.
func (b *Broker) SubtitleURL(ctx context.Context, a frontend.SessionAccess, id string, index int, format string) (string, error) {
	s, viewer, err := b.access(ctx, a, id)
	if err != nil {
		return "", err
	}
	if viewer.ID != s.public.ViewerID {
		viewer, err = b.deps.ActiveViewer(ctx, s.public.ViewerID)
	}
	if err != nil || !b.currentMediaPolicy(ctx, viewer, s) {
		return "", core.ErrUnauthorized
	}
	format = strings.ToLower(format)
	if index < 0 || !subtitleFormat(format) {
		return "", core.ErrNotAvailable
	}
	ps, ok := b.deps.Playback.Session(s.playID)
	if !ok || ps == nil {
		return "", core.ErrNotFound
	}
	st, ok := subtitleStream(ps.Info, index)
	if !ok || transcode.IsImageSubtitle(st.Codec) {
		return "", core.ErrNotAvailable
	}
	if st.External && (!strings.HasPrefix(st.ExternalURL, "ether-subtitle:") || b.deps.OpenSubtitle == nil) {
		return "", core.ErrNotAvailable
	}
	if !st.External && b.deps.ExtractSubtitle == nil && !b.progressiveSubtitle(s, ps.Info, st) {
		return "", core.ErrNotAvailable
	}
	url := fmt.Sprintf("/sessions/media/%s/%s/subtitles/%d.%s", s.public.ID, s.grant, index, format)
	if b.progressiveSubtitle(s, ps.Info, st) {
		url += "?ether_stream=1"
	}
	return url, nil
}

// subtitleFormat includes Jellyfin web's JSON renderer and ASS renderer.
func subtitleFormat(format string) bool {
	switch format {
	case "vtt", "srt", "js", "ass":
		return true
	}
	return false
}

// subtitleContext names the session's source for the extraction cache.
func (b *Broker) subtitleContext(ctx context.Context, playID string) context.Context {
	if keyer, ok := b.deps.Playback.(interface{ SourceKey(string) string }); ok {
		return transcode.WithSubtitleSource(ctx, keyer.SourceKey(playID))
	}
	return ctx
}

func subtitleStream(info core.MediaInfo, index int) (core.StreamInfo, bool) {
	for _, s := range info.Streams {
		if s.Type == "subtitle" && s.Index == index {
			return s, true
		}
	}
	return core.StreamInfo{}, false
}
func (b *Broker) serveSubtitle(w http.ResponseWriter, r *http.Request, s *brokerSession, name string) {
	number, format, ok := strings.Cut(name, ".")
	if !ok || !subtitleFormat(format) {
		http.NotFound(w, r)
		return
	}
	index, err := strconv.Atoi(number)
	if err != nil || index < 0 {
		http.NotFound(w, r)
		return
	}
	ps, ok := b.deps.Playback.Session(s.playID)
	if !ok || ps == nil {
		http.NotFound(w, r)
		return
	}
	stream, ok := subtitleStream(ps.Info, index)
	if !ok || transcode.IsImageSubtitle(stream.Codec) {
		http.NotFound(w, r)
		return
	}
	if r.URL.Query().Get("ether_stream") == "1" && (format == "js" || format == "vtt") && b.progressiveSubtitle(s, ps.Info, stream) {
		b.serveProgressiveSubtitle(w, r, s, stream)
		return
	}
	// Authored ASS must retain its styles and positioning, using the same
	// scoped source reader as the subtitle artifact API.
	if format == "ass" && !stream.External && subtitles.IsASSCodec(stream.Codec) {
		raw, err := b.readEmbeddedAuthoredASS(r.Context(), s, ps, stream)
		if err != nil {
			http.Error(w, "subtitle unavailable", http.StatusBadGateway)
			return
		}
		if ms, ok := syncMS(r); ok {
			raw = subtitles.ShiftASS(raw, time.Duration(ms)*time.Millisecond)
		}
		writeSubtitle(w, r, raw, "text/x-ssa; charset=utf-8")
		return
	}
	sourceFormat := format
	if format == "js" || format == "ass" {
		sourceFormat = "vtt"
	}
	const maxSubtitle = 32 << 20
	var raw []byte
	if stream.External {
		if b.deps.OpenSubtitle == nil || !strings.HasPrefix(stream.ExternalURL, "ether-subtitle:") {
			http.NotFound(w, r)
			return
		}
		reader, actual, err := b.openSubtitle(s, stream.ExternalURL, sourceFormat)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer reader.Close()
		raw, err = io.ReadAll(io.LimitReader(reader, maxSubtitle+1))
		if err != nil || len(raw) > maxSubtitle || actual != sourceFormat {
			http.Error(w, "subtitle unavailable", http.StatusBadGateway)
			return
		}
	} else {
		if b.deps.ExtractSubtitle == nil {
			http.NotFound(w, r)
			return
		}
		var buf limitedBuffer
		buf.limit = maxSubtitle
		if err := b.deps.ExtractSubtitle(b.subtitleContext(r.Context(), s.playID), b.deps.Playback.InputURL(s.playID), index, stream.Codec, &buf); err != nil {
			http.Error(w, "subtitle unavailable", http.StatusBadGateway)
			return
		}
		raw = buf.Bytes()
		if format == "srt" {
			cues, err := subtitles.ParseCues(string(raw))
			if err != nil {
				http.Error(w, "subtitle unavailable", http.StatusBadGateway)
				return
			}
			raw = []byte(subtitles.RenderSRT(cues))
		}
	}
	raw = b.presentSubtitle(r, raw, sourceFormat)
	contentType := "text/vtt; charset=utf-8"
	switch format {
	case "srt":
		contentType = "application/x-subrip; charset=utf-8"
	case "js", "ass":
		cues, err := subtitles.ParseCues(string(raw))
		if err != nil {
			http.Error(w, "subtitle unavailable", http.StatusBadGateway)
			return
		}
		if format == "js" {
			raw, err = jellyfinSubtitleJSON(cues)
			contentType = "application/json; charset=utf-8"
		} else {
			raw = []byte(subtitles.RenderASS(cues))
			contentType = "text/x-ssa; charset=utf-8"
		}
		if err != nil {
			http.Error(w, "subtitle unavailable", http.StatusBadGateway)
			return
		}
	}
	writeSubtitle(w, r, raw, contentType)
}

// Jellyfin web fetchSubtitles parses JSON, then renders TrackEvents on the
// title clock (10,000,000 ticks per second). Its text renderer expands \N.
func jellyfinSubtitleJSON(cues []subtitles.Cue) ([]byte, error) {
	type event struct {
		Id                 string
		Text               string
		StartPositionTicks int64
		EndPositionTicks   int64
	}
	events := make([]event, 0, len(cues))
	for n, cue := range cues {
		events = append(events, event{
			Id:                 strconv.Itoa(n + 1),
			Text:               strings.ReplaceAll(strings.ReplaceAll(cue.Text, "\r", ""), "\n", `\N`),
			StartPositionTicks: cue.Start.Nanoseconds() / 100,
			EndPositionTicks:   cue.End.Nanoseconds() / 100,
		})
	}
	return json.Marshal(struct{ TrackEvents []event }{TrackEvents: events})
}

func writeSubtitle(w http.ResponseWriter, r *http.Request, raw []byte, contentType string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(raw)
	}
}

func (b *Broker) presentSubtitle(r *http.Request, raw []byte, format string) []byte {
	ms, ok := syncMS(r)
	if !ok {
		return raw
	}
	cues, err := subtitles.ParseCues(string(raw))
	if err != nil {
		return raw
	}
	shifted, err := subtitles.Shift(cues, time.Duration(ms)*time.Millisecond, 1)
	if err != nil {
		return raw
	}
	if format == "srt" {
		return []byte(subtitles.RenderSRT(shifted))
	}
	return []byte(subtitles.RenderVTT(shifted))
}

func syncMS(r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("sync_ms")
	if raw == "" {
		return 0, false
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < -10000 || value > 10000 || value == 0 {
		return 0, false
	}
	return value, true
}

func (b *Broker) playSource(s *brokerSession) string {
	if b.deps.Playback == nil || s == nil {
		return ""
	}
	if keyer, ok := b.deps.Playback.(interface{ SourceKey(string) string }); ok {
		if k := keyer.SourceKey(s.playID); k != "" {
			return k
		}
	}
	ps, ok := b.deps.Playback.Session(s.playID)
	if ok && ps != nil && ps.Resolution != nil && ps.Resolution.Release.ID != "" {
		return ps.Resolution.Release.ID
	}
	if s.playID != "" {
		return s.playID
	}
	return ""
}

func (b *Broker) openSubtitle(s *brokerSession, id, format string) (io.ReadCloser, string, error) {
	source := b.playSource(s)
	if b.deps.OpenSubtitleFrom != nil && source != "" {
		return b.deps.OpenSubtitleFrom(id, format, source)
	}
	if b.deps.OpenSubtitle == nil {
		return nil, "", core.ErrNotFound
	}
	return b.deps.OpenSubtitle(id, format)
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, fmt.Errorf("subtitle exceeds limit")
	}
	return b.Buffer.Write(p)
}
