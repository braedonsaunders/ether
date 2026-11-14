package transcode

import (
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func (s *Session) preparationLocked(stage string, track int) {
	if !s.diagnosticsEnabled {
		return
	}
	rung := "base"
	if track >= 0 && track < len(s.rungs) {
		rung = s.rungs[track].id
	}
	for _, e := range s.preparation {
		if e.Stage == stage && e.Rung == rung {
			return
		}
	}
	if len(s.preparation) < 32 {
		s.preparation = append(s.preparation, core.HLSPreparationEvent{Stage: stage, At: time.Now().UTC(), Rung: rung})
	}
}

func (s *Session) diagnosticLocked(r core.HLSDiagnosticRecord) {
	if !s.diagnosticsEnabled {
		return
	}
	r.At = time.Now().UTC()
	s.diagnosticRecords = append(s.diagnosticRecords, r)
	if len(s.diagnosticRecords) > 32 {
		s.diagnosticRecords = s.diagnosticRecords[len(s.diagnosticRecords)-32:]
	}
}

func (s *Session) PlaybackDiagnostics() core.HLSPlaybackDiagnostics {
	s.mu.Lock()
	defer s.mu.Unlock()
	return core.HLSPlaybackDiagnostics{Events: append([]core.HLSPreparationEvent{}, s.preparation...), Records: append([]core.HLSDiagnosticRecord{}, s.diagnosticRecords...)}
}

func (s *Session) noteOutputCodecsLocked(t int, file string) {
	if !s.diagnosticsEnabled || s.container != ContainerFMP4 || s.outputInit[t] == file {
		return
	}
	st, err := os.Stat(file)
	if err != nil || st.Size() > 1<<20 {
		return
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return
	}
	s.outputCodecs[t] = ""
	tracks, err := parseInitTracks(data)
	if err != nil {
		return
	}
	var codecs []string
	// CODECS order is video then audio regardless of MP4 track order.
	for _, handler := range []string{"vide", "soun"} {
		for _, track := range tracks {
			if track.Handler == handler {
				if track.Codecs == "" {
					return
				}

				if handler == "vide" && t >= 0 && t < len(s.rungs) && track.Width > 0 && track.Height > 0 {
					s.rungs[t].width = track.Width
					s.rungs[t].height = track.Height
				}
				if handler == "soun" {
					s.outputAudioChannels[t] = track.Channels
				}
				codecs = append(codecs, track.Codecs)
			}
		}
	}
	if len(codecs) > 0 {
		s.outputCodecs[t] = strings.Join(codecs, ",")
		s.outputInit[t] = file
	}
}

// Retain only a bounded prefix of the generated playlist. Its URIs are local
// segment names, so neither upstream credentials nor stream grants enter it.
func (s *Session) notePlaylistLocked(t int, body []byte) {
	if !s.diagnosticsEnabled {
		return
	}
	if len(body) > 8192 {
		body = body[:8192]
	}
	s.diagnosticLocked(core.HLSDiagnosticRecord{Resource: "playlist", MIME: "application/vnd.apple.mpegurl", Status: 200, Bytes: int64(len(body)), Codecs: s.outputCodecs[t], Playlist: string(body)})
}

// diagnosticWriter timestamps the first successful body write, rather than
// mistaking segment publication for a byte sent to a client.
type diagnosticWriter struct {
	http.ResponseWriter
	session                *Session
	track, segment, status int
	resource               string
	bytes                  int64
}

func (w *diagnosticWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *diagnosticWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *diagnosticWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	n, err := w.ResponseWriter.Write(b)
	if n > 0 && w.bytes == 0 && w.segment >= 0 && w.status >= 200 && w.status < 300 {
		w.session.mu.Lock()
		w.session.preparationLocked("first-byte", w.track)
		w.session.mu.Unlock()
	}
	w.bytes += int64(n)
	return n, err
}
func (w *diagnosticWriter) finish() {
	w.session.mu.Lock()
	defer w.session.mu.Unlock()
	w.session.diagnosticLocked(core.HLSDiagnosticRecord{Resource: w.resource, MIME: w.Header().Get("Content-Type"), Status: w.status, Bytes: w.bytes, Codecs: w.session.outputCodecs[w.track]})
}
