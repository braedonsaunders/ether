package app

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/livetv"
)

type sessionLive struct {
	service *livetv.Service
	cached  func(context.Context, core.RatingKey) (*core.Title, error)
}

func (l sessionLive) Allowed(ctx context.Context, viewer *core.Viewer, id string) error {
	return l.service.AllowedChannelCached(ctx, viewer, id, l.cached)
}
func (l sessionLive) Tune(ctx context.Context, viewer *core.Viewer, id string, caps core.ClientCaps, clientSession string) (string, error) {
	info, err := l.service.TuneWithSession(ctx, viewer, id, caps, clientSession)
	return info.ID, err
}
func (l sessionLive) Stop(ctx context.Context, viewer *core.Viewer, id string) error {
	return l.service.Stop(ctx, id, viewer)
}

func (l sessionLive) Serve(w http.ResponseWriter, r *http.Request, viewer *core.Viewer, id, tail, base string) {
	// Live TV's Plex adapter can propagate Plex credentials into playlists.
	// Scoped broker paths carry the grant already, so strip credential inputs.
	clean := r.Clone(r.Context())
	clean.Header = r.Header.Clone()
	clean.Header.Del("X-Plex-Token")
	clean.URL.RawQuery = ""
	p := &livePlaylistWriter{ResponseWriter: w, from: []byte("/livetv/sessions/" + id + "/0/"), to: []byte(base)}
	defer p.finish()
	parts := strings.Split(tail, "/")
	switch {
	case tail == "index.m3u8":
		l.service.ServePlaylist(p, clean, id, viewer)
	case len(parts) == 2 && parts[0] == "resource" && parts[1] != "":
		l.service.ServeResource(p, clean, id, parts[1], viewer)
	case len(parts) == 2 && parts[0] == "relay":
		n, err := strconv.Atoi(strings.TrimSuffix(parts[1], ".ts"))
		if err != nil || n < 0 {
			http.NotFound(p, clean)
			return
		}
		l.service.ServeRelaySegment(p, clean, id, n, viewer)
	case len(parts) == 3 && parts[0] == "program":
		start, e1 := strconv.ParseInt(parts[1], 10, 64)
		n, e2 := strconv.Atoi(strings.TrimSuffix(parts[2], ".ts"))
		if e1 != nil || e2 != nil || n < 0 {
			http.NotFound(p, clean)
			return
		}
		l.service.ServeSegment(p, clean, id, start, n, viewer)
	default:
		http.NotFound(p, clean)
	}
}

type livePlaylistWriter struct {
	http.ResponseWriter
	from, to          []byte
	buf               bytes.Buffer
	decided, playlist bool
	status            int
}

func (w *livePlaylistWriter) decide() {
	if !w.decided {
		w.decided = true
		w.playlist = strings.Contains(strings.ToLower(w.Header().Get("Content-Type")), "mpegurl")
		if w.playlist {
			w.Header().Del("Content-Length")
		}
	}
}
func (w *livePlaylistWriter) WriteHeader(status int) {
	w.decide()
	if w.playlist {
		w.status = status
	} else {
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *livePlaylistWriter) Write(p []byte) (int, error) {
	w.decide()
	if w.playlist {
		return w.buf.Write(p)
	}
	return w.ResponseWriter.Write(p)
}
func (w *livePlaylistWriter) Flush() {
	if !w.playlist {
		if f, ok := w.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
	}
}
func (w *livePlaylistWriter) finish() {
	if w.playlist {
		if w.status != 0 {
			w.ResponseWriter.WriteHeader(w.status)
		}
		_, _ = w.ResponseWriter.Write(bytes.ReplaceAll(w.buf.Bytes(), w.from, w.to))
	}
}
