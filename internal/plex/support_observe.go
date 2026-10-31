package plex

import (
	"errors"
	"net/http"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/diagnostics"
)

func (s *Server) observeRequest(r *http.Request, sr *statusRecorder) {
	if s == nil || s.d.Support == nil || r == nil || sr == nil {
		return
	}
	pr := FromContext(r.Context())
	status := sr.status
	if status == 0 {
		status = http.StatusOK
	}
	kind := diagnostics.ClassifyRequest(r.URL.Path, status, sr.Header().Get("Content-Type"), sr.written)
	ev := s.supportEvent(pr, kind)
	ev.Method = r.Method
	ev.Route = r.URL.Path
	ev.Status = status
	ev.Bytes = sr.written
	s.d.Support.Observe(ev)
}

func (s *Server) observePlaybackReported(pr *Request) {
	if s == nil || s.d.Support == nil || pr == nil || pr.User == nil {
		return
	}
	s.d.Support.Observe(s.supportEvent(pr, diagnostics.KindPlaybackReported))
}

func (s *Server) observeResolve(pr *Request, sessionID string, title *core.Title, err error, dec *core.Decision) {
	if s == nil || s.d.Support == nil || pr == nil {
		return
	}
	ev := s.supportEvent(pr, "")
	ev.SessionKey = sessionID
	if title != nil {
		ev.Title = title.Title
	}
	if err != nil {
		if !resolverRejection(err) {
			return
		}
		ev.Kind = diagnostics.KindSourceRejected
		ev.Rejection = playbackMessage(err)
		s.d.Support.Observe(ev)
		return
	}
	if dec == nil {
		return
	}
	chosen := ev
	chosen.Kind = diagnostics.KindSourceChosen
	chosen.Decision = string(dec.Kind)
	s.d.Support.Observe(chosen)
	decision := ev
	decision.Kind = diagnostics.KindTranscodeDecision
	decision.Decision = string(dec.Kind)
	decision.Profile = dec.VideoCodec
	decision.Reason = dec.Reason
	s.d.Support.Observe(decision)
}

func (s *Server) supportEvent(pr *Request, kind diagnostics.Kind) diagnostics.Event {
	ev := diagnostics.Event{Time: s.now(), Kind: kind}
	if pr == nil {
		return ev
	}
	ev.DeviceKey = pr.ClientID
	ev.SessionKey = pr.SessionID
	ev.DeviceName = pr.DeviceName
	ev.Product = pr.Product
	ev.Model = pr.Model
	ev.Platform = pr.Platform
	ev.AppVersion = pr.Version
	if pr.RemoteIP.IsValid() {
		ev.Peer = pr.RemoteIP.String()
	}
	if pr.User != nil {
		ev.Username = pr.User.Username
	}
	return ev
}

func resolverRejection(err error) bool {
	return errors.Is(err, core.ErrNotAvailable) ||
		errors.Is(err, core.ErrNoSources) ||
		errors.Is(err, core.ErrNotCached) ||
		errors.Is(err, core.ErrDownloadQueued) ||
		errors.Is(err, core.ErrDebridAuth) ||
		errors.Is(err, core.ErrNoTorrentSources)
}
