package plex

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

// MPEG-DASH playback for Plex Web, which asks for DASH (protocol=dash,
// start.mpd) in Chromium and Firefox when it cannot direct play. The URL
// layout is the one real PMS uses:
//
//	/video/:/transcode/universal/start.mpd?session={id}&...    static MPD
//	/video/:/transcode/universal/session/{id}/{rep}/header     init segment
//	/video/:/transcode/universal/session/{id}/{rep}/{n}.m4s    media segment n
//
// Representation 0 is video and 1 audio; segments are numbered from 0
// over the whole title, so seeking is just asking for another number.

// handleStartDASH is /video/:/transcode/universal/start.mpd.
func (s *Server) handleStartDASH(w http.ResponseWriter, r *http.Request) {
	if s.handleLiveTVPlayback(w, r) {
		return
	}
	if s.handleExtraPlayback(w, r) {
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	unlockOwner, locked := s.lockPlaybackRequest(w, r)
	if !locked {
		return
	}
	defer unlockOwner()

	if s.d.Transcoder == nil || s.d.Playback == nil {
		writeHTTPError(w, http.StatusServiceUnavailable)
		return
	}
	p, err := s.parsePlayRequest(r)
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	unlockEncoder, lockErr := s.lockEncoder(ctx, p.sessionID)
	if lockErr != nil {
		return
	}
	defer unlockEncoder()
	if s.playbackOwners.Stale(pr.User.ID, pr.ClientID, p.key, pr.SessionID) {
		writeHTTPError(w, http.StatusGone)
		return
	}
	p.protocol = core.ProtocolDASH
	if !s.transcodes.claim(p.sessionID, pr.User.ID, pr.ClientID, s.now()) {
		writeHTTPError(w, http.StatusForbidden)
		return
	}
	if err, failed := s.transcodes.takeFailure(p.sessionID, pr.User.ID, p.key, s.now()); failed {
		s.log.Info("start DASH playback failed", "key", p.key, "reason", playbackMessage(err), "err", err, "decision", "remembered")
		s.writeErr(w, r, err)
		return
	}
	entry, ok := s.transcodes.get(p.sessionID)
	var ps *core.PlaySession
	if ok && entry.key == p.key && entry.userID == pr.User.ID {
		ps, ok = s.d.Playback.Session(entry.playID)
	}
	if !ok || ps == nil || !p.sameSettings(entry.requested) {
		if _, ps, _, err = s.prepare(ctx, pr, &p); err != nil {
			s.log.Info("start DASH playback failed", "key", p.key, "reason", playbackMessage(err), "err", err)
			s.writeErr(w, r, err)
			return
		}
		entry, _ = s.transcodes.get(p.sessionID)
	} else if dec, err := s.publishBrokerPlayback(ctx, pr, replayPlay(p, entry), ps); err != nil {
		s.log.Info("playback broker publish failed", "key", p.key, "err", err)
		writeHTTPError(w, http.StatusServiceUnavailable)
		return
	} else {
		entry.decision = dec
	}
	s.playbackOwners.Bind(pr.User.ID, pr.ClientID, p.key, pr.SessionID, ps.ID, true, s.now())
	entry.playbackID = pr.SessionID
	s.transcodes.put(entry)

	dec := entry.decision
	if dec.Kind == core.DecisionDirectPlay {
		// The client asked for DASH anyway: remux.
		dec.Kind, dec.VideoCopy, dec.AudioCopy = core.DecisionRemux, true, true
		if len(entry.caps.TranscodeAudioCodecs) > 0 && dec.AudioCodec != "" && !slices.Contains(entry.caps.TranscodeAudioCodecs, dec.AudioCodec) {
			opts := entry.opts
			opts.DirectPlay = false
			opts.DirectStream = true
			dec = s.d.Transcoder.Decide(ps.Info, entry.caps, opts)
		}
	}
	if existing, ok := s.d.Transcoder.Session(p.sessionID); ok && existing != nil {
		// The MPD spans the whole title whatever the offset, so a repeated
		// start for the same decision keeps the session (and its finished
		// segments); a new decision request starts afresh.
		if ds, isDASH := existing.(core.DASHSession); isDASH && entry.started {
			s.notePlexEncoder(p.sessionID, frontend.StreamDASH, dec, p.offset)
			unlockEncoder()
			unlockOwner()
			s.serveStreamManifest(w, r, p.sessionID, ds.ServeManifest)
			return
		}
		s.d.Transcoder.Stop(p.sessionID)
	}
	hs, err := s.d.Transcoder.StartHLS(context.WithoutCancel(ctx), core.HLSRequest{
		SessionID:   p.sessionID,
		InputURL:    s.d.Playback.InputURL(ps.ID),
		Info:        ps.Info,
		Decision:    dec,
		StartOffset: p.offset,
		Protocol:    core.ProtocolDASH,
	})
	if err != nil {
		s.log.Warn("start DASH session", "session", p.sessionID, "key", p.key, "err", err)
		writeHTTPError(w, http.StatusInternalServerError)
		return
	}
	ds, ok := hs.(core.DASHSession)
	if !ok {
		s.log.Error("transcoder returned a non-DASH session", "session", p.sessionID)
		s.d.Transcoder.Stop(p.sessionID)
		writeHTTPError(w, http.StatusInternalServerError)
		return
	}
	entry.decision, entry.offset, entry.started, entry.updatedAt = dec, p.offset, true, s.now()
	s.transcodes.put(entry)
	s.notePlexEncoder(p.sessionID, frontend.StreamDASH, dec, p.offset)
	unlockEncoder()
	unlockOwner()
	s.serveStreamManifest(w, r, p.sessionID, ds.ServeManifest)
}

// dashSession resolves the session and representation of a segment URL.
func (s *Server) dashSession(w http.ResponseWriter, r *http.Request) (core.DASHSession, int, bool) {
	if s.d.Transcoder == nil {
		writeHTTPError(w, http.StatusNotFound)
		return nil, 0, false
	}
	id, idOK := routeSessionID(r)
	rep, err := strconv.Atoi(chi.URLParam(r, "stream"))
	hs, ok := s.d.Transcoder.Session(id)
	if !idOK || err != nil || rep < 0 || !ok || hs == nil {
		writeHTTPError(w, http.StatusNotFound)
		return nil, 0, false
	}
	ds, ok := hs.(core.DASHSession)
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return nil, 0, false
	}
	s.transcodes.touch(id, s.now())
	return ds, rep, true
}

// handleDASHInit is session/{session}/{stream}/header.
func (s *Server) handleDASHInit(w http.ResponseWriter, r *http.Request) {
	if ds, rep, ok := s.dashSession(w, r); ok {
		ds.ServeInit(w, r, rep)
	}
}

// handleDASHSegment is session/{session}/{stream}/{segment}.m4s.
func (s *Server) handleDASHSegment(w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutSuffix(chi.URLParam(r, "segment"), ".m4s")
	n, err := strconv.Atoi(name)
	if !ok || err != nil || n < 0 {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if ds, rep, ok := s.dashSession(w, r); ok {
		cw := &countWriter{ResponseWriter: w}
		ds.ServeMedia(cw, r, rep, n)
		s.noteStreamBytes(r, cw.n)
	}
}

// dashDecisionMedia adjusts a transcode decision's Media for DASH the way
// PMS reports it: an mp4 container over protocol dash, with the video and
// the selected audio in separate segment streams. Unselected audio keeps
// the shape applyStreamDecision already returned: the source index, and no
// decision or location. Marking every audio stream segments-audio makes
// Plex Web request a representation that dashReps never builds.
func dashDecisionMedia(m *model.Media) {
	m.Container = "mp4"
	m.Protocol = core.ProtocolDASH
	for i := range m.Part {
		part := &m.Part[i]
		part.Container = "mp4"
		part.Protocol = core.ProtocolDASH
		for j := range part.Stream {
			st := &part.Stream[j]
			switch st.StreamType {
			case 1:
				st.Location = "segments-video"
			case 2:
				if st.Selected != nil && *st.Selected {
					st.Location = "segments-audio"
				}
			}
		}
	}
}
