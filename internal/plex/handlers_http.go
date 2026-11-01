package plex

import (
	"context"
	"net/http"
	"slices"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/sessions"
)

// handleStartHTTP is /video/:/transcode/universal/start (no extension):
// Plex for Mac / Desktop's protocol=http Matroska stream.
func (s *Server) handleStartHTTP(w http.ResponseWriter, r *http.Request) {
	if s.refuseCapability(w, r) {
		return
	}
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
	p.protocol = core.ProtocolHTTP
	p.opts.Protocol = core.ProtocolHTTP
	unlockEncoder, lockErr := s.lockEncoder(ctx, p.sessionID)
	if lockErr != nil {
		return
	}
	defer unlockEncoder()

	if s.playbackOwners.Stale(pr.User.ID, pr.ClientID, p.key, pr.SessionID) {
		writeHTTPError(w, http.StatusGone)
		return
	}

	if !s.transcodes.claim(p.sessionID, pr.User.ID, pr.ClientID, s.now()) {
		writeHTTPError(w, http.StatusForbidden)
		return
	}
	if err, failed := s.transcodes.takeFailure(p.sessionID, pr.User.ID, p.key, s.now()); failed {
		s.log.Info("start playback failed", "key", p.key, "reason", playbackMessage(err), "err", err, "decision", "remembered")
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
			s.log.Info("start playback failed", "key", p.key, "reason", playbackMessage(err), "err", err)
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

	dec := sessions.HLSDecision(entry.decision, p.opts.DirectPlay, func() core.Decision {
		resolvePlaySelection(&p, ps.Info)
		applySelections(&p, ps.Info, s.audioLanguages(pr.User))
		return s.d.Transcoder.Decide(ps.Info, p.caps, p.opts)
	}, func(dec core.Decision) core.Decision {
		if len(entry.caps.TranscodeAudioCodecs) > 0 && dec.AudioCodec != "" && !slices.Contains(entry.caps.TranscodeAudioCodecs, dec.AudioCodec) {
			opts := entry.opts
			opts.DirectPlay = false
			opts.DirectStream = true
			opts.Protocol = core.ProtocolHTTP
			return s.d.Transcoder.Decide(ps.Info, entry.caps, opts)
		}
		return dec
	})
	if existing, ok := s.d.Transcoder.Session(p.sessionID); ok && existing != nil {
		if entry.started && entry.offset == p.offset {
			if hs, ok := existing.(core.HTTPSession); ok {
				s.notePlexEncoder(p.sessionID, frontend.StreamHTTP, dec, p.offset)
				unlockEncoder()
				unlockOwner()
				hs.ServeHTTP(w, r)
				return
			}
		}
		s.d.Transcoder.Stop(p.sessionID)
	}
	hs, err := s.d.Transcoder.StartHLS(context.WithoutCancel(ctx), core.HLSRequest{
		SessionID:   p.sessionID,
		InputURL:    s.d.Playback.InputURL(ps.ID),
		Info:        ps.Info,
		Decision:    dec,
		StartOffset: p.offset,
		Protocol:    core.ProtocolHTTP,
	})
	if err != nil {
		s.log.Warn("start HTTP session", "session", p.sessionID, "key", p.key, "err", err)
		writeHTTPError(w, http.StatusInternalServerError)
		return
	}
	stream, ok := hs.(core.HTTPSession)
	if !ok {
		s.log.Error("transcoder returned a non-HTTP session", "session", p.sessionID)
		s.d.Transcoder.Stop(p.sessionID)
		writeHTTPError(w, http.StatusInternalServerError)
		return
	}
	s.bindTranscodeStarted(entry, dec, p.offset)
	s.notePlexEncoder(p.sessionID, frontend.StreamHTTP, dec, p.offset)
	unlockEncoder()
	unlockOwner()
	stream.ServeHTTP(w, r)
}
