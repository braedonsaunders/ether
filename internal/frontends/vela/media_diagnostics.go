package vela

import (
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

var playlistURI = regexp.MustCompile(`URI="([^"]*)"`)

// sanitizePlaylist preserves packaging/timeline tags, but drops every URL's
// authority, directory and query. Grants and provider names never enter traces.
func sanitizePlaylist(body string) string {
	lines := strings.Split(body, "\n")
	clean := func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return "resource"
		}
		return path.Base(u.Path)
	}
	for n, line := range lines {
		if strings.HasPrefix(line, "#") {
			lines[n] = playlistURI.ReplaceAllStringFunc(line, func(value string) string {
				return `URI="` + clean(strings.TrimSuffix(strings.TrimPrefix(value, `URI="`), `"`)) + `"`
			})
		} else if line != "" {
			lines[n] = clean(line)
		}
	}
	return strings.Join(lines, "\n")
}

func (i *instance) serveObservedMedia(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/sessions/media/")
	id, _, _ := strings.Cut(tail, "/")
	i.mu.Lock()
	s := i.sessions[id]
	i.mu.Unlock()
	if s == nil {
		i.host.Sessions.ServeMedia(w, r)
		return
	}
	s.mu.Lock()
	source, seek := s.source, s.seek
	s.mu.Unlock()
	out := &mediaObservationWriter{ResponseWriter: w}
	i.host.Sessions.ServeMedia(out, r)
	record := object{"at": time.Now().UTC(), "resource": path.Base(r.URL.Path), "mimeType": w.Header().Get("Content-Type"), "status": out.status, "bytes": out.bytes, "sourceGeneration": source, "seekGeneration": seek}
	if len(out.playlist) > 0 {
		record["playlist"] = sanitizePlaylist(string(out.playlist))
	}
	if !out.firstByte.IsZero() {
		record["firstByteAt"] = out.firstByte
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if source != s.source || seek != s.seek {
		return
	}
	s.observedRecords = append(s.observedRecords, record)
	if len(s.observedRecords) > 32 {
		s.observedRecords = s.observedRecords[len(s.observedRecords)-32:]
	}
}

type mediaObservationWriter struct {
	http.ResponseWriter
	status    int
	bytes     int64
	firstByte time.Time
	playlist  []byte
}

func (w *mediaObservationWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *mediaObservationWriter) Flush()                      { _ = http.NewResponseController(w.ResponseWriter).Flush() }
func (w *mediaObservationWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *mediaObservationWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	n, err := w.ResponseWriter.Write(data)
	if n > 0 && w.firstByte.IsZero() {
		w.firstByte = time.Now().UTC()
	}
	w.bytes += int64(n)
	if strings.Contains(w.Header().Get("Content-Type"), "mpegurl") && len(w.playlist) < 8192 {
		remaining := 8192 - len(w.playlist)
		w.playlist = append(w.playlist, data[:min(n, remaining)]...)
	}
	return n, err
}

func (s *playSession) retainDiagnostics() {
	s.diagnosticHistory = append(s.diagnosticHistory, object{"sourceGeneration": s.source, "seekGeneration": s.seek, "encoder": s.diagnostics, "media": s.observedRecords})
	if len(s.diagnosticHistory) > 4 {
		s.diagnosticHistory = s.diagnosticHistory[len(s.diagnosticHistory)-4:]
	}
	s.diagnostics = nil
	s.observedRecords = nil
}
