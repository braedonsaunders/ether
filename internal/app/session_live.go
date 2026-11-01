package app

import (
	"bytes"
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/livetv"
)

var liveTagURI = regexp.MustCompile(`URI="([^"]+)"`)

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
func (l sessionLive) TuneMedia(ctx context.Context, viewer *core.Viewer, id string, caps core.ClientCaps, clientSession string) (string, *core.MediaInfo, error) {
	info, err := l.service.TuneWithSession(ctx, viewer, id, caps, clientSession)
	return info.ID, info.MediaInfo, err
}
func (l sessionLive) Stop(ctx context.Context, viewer *core.Viewer, id string) error {
	return l.service.Stop(ctx, id, viewer)
}

func normalizeLiveTail(tail string) string {
	if rest, ok := strings.CutPrefix(tail, "resource-"); ok && rest != "" && !strings.Contains(rest, "/") {
		return "resource/" + rest
	}
	if rest, ok := strings.CutPrefix(tail, "relay-"); ok && rest != "" && !strings.Contains(rest, "/") {
		return "relay/" + rest
	}
	if rest, ok := strings.CutPrefix(tail, "program-"); ok && !strings.Contains(rest, "/") {
		if start, seg, ok := strings.Cut(rest, "-"); ok && start != "" && seg != "" {
			return "program/" + start + "/" + seg
		}
	}
	return tail
}

func (l sessionLive) Serve(w http.ResponseWriter, r *http.Request, viewer *core.Viewer, id, tail, base string) {
	// Live TV's Plex adapter can propagate Plex credentials into playlists.
	// Scoped broker paths carry the grant already, so strip credential inputs.
	clean := r.Clone(r.Context())
	clean.Header = r.Header.Clone()
	clean.Header.Del("X-Plex-Token")
	clean.URL.RawQuery = ""
	sessionPrefix := "/livetv/sessions/" + id + "/0/"
	p := &livePlaylistWriter{
		ResponseWriter: w,
		from:           []byte(sessionPrefix),
		to:             []byte(base),
		flatSegments:   strings.HasSuffix(base, "/segment/"),
		sessionPrefix:  sessionPrefix,
		base:           base,
	}
	defer p.finish()
	tail = normalizeLiveTail(tail)
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
	flatSegments      bool
	sessionPrefix     string
	base              string
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
		if w.flatSegments {
			_, _ = w.ResponseWriter.Write(rewriteSiloLivePlaylist(w.buf.Bytes(), w.sessionPrefix, w.base))
			return
		}
		_, _ = w.ResponseWriter.Write(bytes.ReplaceAll(w.buf.Bytes(), w.from, w.to))
	}
}

func rewriteSiloLivePlaylist(body []byte, sessionPrefix, base string) []byte {
	lines := strings.Split(string(body), "\n")
	for i, line := range lines {
		trim := strings.TrimSpace(line)
		if trim == "" {
			continue
		}
		if !strings.HasPrefix(trim, "#") {
			lines[i] = rewriteSiloLiveURI(trim, sessionPrefix, base)
			continue
		}
		lines[i] = liveTagURI.ReplaceAllStringFunc(line, func(m string) string {
			parts := liveTagURI.FindStringSubmatch(m)
			if len(parts) < 2 {
				return m
			}
			return `URI="` + rewriteSiloLiveURI(parts[1], sessionPrefix, base) + `"`
		})
	}
	return []byte(strings.Join(lines, "\n"))
}

func rewriteSiloLiveURI(raw, sessionPrefix, base string) string {
	rel := strings.TrimSpace(raw)
	if strings.HasPrefix(rel, sessionPrefix) {
		rel = strings.TrimPrefix(rel, sessionPrefix)
	}
	rel, _, _ = strings.Cut(rel, "?")
	switch {
	case rel == "index.m3u8":
		return base + "index.m3u8"
	case strings.HasPrefix(rel, "resource/"):
		key := strings.TrimPrefix(rel, "resource/")
		if key != "" && !strings.Contains(key, "/") {
			return base + "resource-" + key
		}
	case strings.HasPrefix(rel, "relay/"):
		seg := strings.TrimPrefix(rel, "relay/")
		if seg != "" && !strings.Contains(seg, "/") {
			return base + "relay-" + seg
		}
	case strings.HasPrefix(rel, "program/"):
		parts := strings.Split(strings.TrimPrefix(rel, "program/"), "/")
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			return base + "program-" + parts[0] + "-" + parts[1]
		}
	}
	return strings.ReplaceAll(raw, sessionPrefix, base)
}
