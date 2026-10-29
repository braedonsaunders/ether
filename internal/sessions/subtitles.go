package sessions

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

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
	if index < 0 || (format != "vtt" && format != "srt") {
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
	if !st.External && b.deps.ExtractSubtitle == nil {
		return "", core.ErrNotAvailable
	}
	return fmt.Sprintf("/sessions/media/%s/%s/subtitles/%d.%s", s.public.ID, s.grant, index, format), nil
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
	if !ok || (format != "vtt" && format != "srt") {
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
	const maxSubtitle = 32 << 20
	var raw []byte
	if stream.External {
		if b.deps.OpenSubtitle == nil || !strings.HasPrefix(stream.ExternalURL, "ether-subtitle:") {
			http.NotFound(w, r)
			return
		}
		reader, actual, err := b.deps.OpenSubtitle(stream.ExternalURL, format)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer reader.Close()
		raw, err = io.ReadAll(io.LimitReader(reader, maxSubtitle+1))
		if err != nil || len(raw) > maxSubtitle || actual != format {
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
		if err := b.deps.ExtractSubtitle(r.Context(), b.deps.Playback.InputURL(s.playID), index, stream.Codec, &buf); err != nil {
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
	if format == "vtt" {
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "application/x-subrip; charset=utf-8")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(raw)
	}
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
