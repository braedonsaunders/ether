package plex

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/sessions"
)

// UsePlaybackBroker attaches the shared session broker. Mounted Plex playback
// publishes through it. Timeline progress and stops are recorded only there.
func (s *Server) UsePlaybackBroker(b frontend.Sessions) {
	if s != nil {
		s.playbackBroker = b
	}
}

func (s *Server) plexPlanRequest(pr *Request, p *playRequest) frontend.PlanRequest {
	device := core.Device{ID: pr.ClientID, Client: pr.Product, Product: pr.Product, Version: pr.Version, Platform: pr.Platform, DeviceName: pr.Device, Language: pr.Language}
	caps := p.caps
	if caps.ClientID == "" {
		caps.ClientID = pr.ClientID
	}
	return frontend.PlanRequest{
		Viewer: pr.User, Device: device, Protocol: "plex", Item: p.key, Caps: caps,
		Intent: frontend.PlayIntent{
			Options: p.opts, PreferRemux: p.mde, PlaybackID: pr.SessionID, ClientSessionID: p.sessionID,
			Plex: &frontend.PlexHints{
				Collection: p.collection, MediaIndex: p.mediaIndex, PartReleaseID: p.partReleaseID,
				AudioSelection: p.audioSel, SubtitleSelection: p.subSel, SubtitleSet: p.subSet, SubtitleStream: p.subStream,
			},
		},
	}
}

func (s *Server) plexAccess(pr *Request) frontend.SessionAccess {
	return frontend.SessionAccess{Viewer: pr.User, Device: core.Device{ID: pr.ClientID}, Protocol: "plex"}
}

// publishBrokerPlayback records a prepared Plex playback on the shared broker.
// The HTTP body is still rendered by the Plex adapter from the same decision.
func (s *Server) publishBrokerPlayback(ctx context.Context, pr *Request, p *playRequest, ps *core.PlaySession, dec core.Decision) error {
	if s == nil || s.playbackBroker == nil || pr == nil || pr.User == nil || p == nil || ps == nil || p.sessionID == "" {
		return nil
	}
	admitted := sessions.PlexAdmission{Source: ps, Decision: dec, Media: ps.Info}
	ctx = sessions.WithPlexAdmission(ctx, func(context.Context) (sessions.PlexAdmission, error) {
		return admitted, nil
	})
	plan, err := s.playbackBroker.Plan(ctx, s.plexPlanRequest(pr, p))
	if err != nil {
		return err
	}
	_, err = s.playbackBroker.Open(ctx, frontend.OpenRequest{Protocol: "plex", PlanID: plan.ID, Viewer: pr.User, Device: core.Device{ID: pr.ClientID, Client: pr.Product, Product: pr.Product, Version: pr.Version, Platform: pr.Platform, DeviceName: pr.Device, Language: pr.Language}})
	return err
}

type plexEncoderNote interface {
	NotePlexEncoder(id string, format frontend.StreamFormat, decision core.Decision, offset time.Duration)
}

func (s *Server) notePlexEncoder(id string, format frontend.StreamFormat, dec core.Decision, offset time.Duration) {
	if s == nil || s.playbackBroker == nil {
		return
	}
	if note, ok := s.playbackBroker.(plexEncoderNote); ok {
		note.NotePlexEncoder(id, format, dec, offset)
	}
}

type plexPlaybackLookup interface {
	PlexSessionForPlayback(context.Context, frontend.SessionAccess, frontend.RatingKey, string, string) (string, error)
	PlexSessionForSource(context.Context, frontend.SessionAccess, frontend.RatingKey, string) (string, error)
	PlexSessionSource(string) (string, bool)
}

func (s *Server) reportBrokerTimeline(ctx context.Context, pr *Request, key core.RatingKey, offset, duration time.Duration, state, transcodeID string) bool {
	if s == nil || s.playbackBroker == nil || pr == nil || pr.User == nil || key == 0 {
		return false
	}
	lookup, ok := s.playbackBroker.(plexPlaybackLookup)
	if !ok {
		return false
	}
	access := s.plexAccess(pr)
	timeline := frontend.Timeline{PlaybackID: pr.SessionID, Position: offset, Duration: duration, State: state}
	id, err := lookup.PlexSessionForPlayback(ctx, access, key, pr.SessionID, transcodeID)
	if (err != nil || id == "") && s.d.Playback != nil {
		if ps, ok := s.d.Playback.SessionFor(pr.User.ID, pr.ClientID, key); ok && ps != nil {
			if sid, serr := lookup.PlexSessionForSource(ctx, access, key, ps.ID); serr == nil && sid != "" {
				id = sid
			}
		}
	}
	if id == "" {
		id = s.adoptBrokerTimeline(ctx, pr, key)
	}
	if id != "" {
		return s.playbackBroker.Report(ctx, access, id, timeline) == nil
	}
	if progress, ok := s.playbackBroker.(interface {
		ReportProgress(context.Context, frontend.SessionAccess, core.RatingKey, frontend.Timeline) error
	}); ok {
		return progress.ReportProgress(ctx, access, key, timeline) == nil
	}
	return false
}

// adoptBrokerTimeline publishes a source the client is already playing when
// no broker session exists for this viewer and item. A mismatched playback
// header does not open a second session.
func (s *Server) adoptBrokerTimeline(ctx context.Context, pr *Request, key core.RatingKey) string {
	if s.d.Playback == nil || pr == nil || pr.User == nil {
		return ""
	}
	if lookup, ok := s.playbackBroker.(plexPlaybackLookup); ok {
		if id, err := lookup.PlexSessionForPlayback(ctx, s.plexAccess(pr), key, "", ""); err == nil && id != "" {
			return ""
		}
	}
	ps, ok := s.d.Playback.SessionFor(pr.User.ID, pr.ClientID, key)
	if !ok || ps == nil {
		return ""
	}
	sessionID := pr.SessionID
	if sessionID == "" {
		sessionID = pr.ClientID + "-" + key.String()
	}
	sessionID = sanitizeSessionID(sessionID)
	if sessionID == "" {
		return ""
	}
	caps := pr.ClientCaps(s.cfg().Transcode.RemoteMaxBitrateKbps)
	if caps.ClientID == "" {
		caps.ClientID = pr.ClientID
	}
	play := playRequest{key: key, caps: caps, sessionID: sessionID, audioSel: -1, subSel: -1}
	if err := s.publishBrokerPlayback(ctx, pr, &play, ps, core.Decision{Kind: core.DecisionDirectPlay, VideoCopy: true, AudioCopy: true}); err != nil {
		s.log.Info("timeline broker adopt failed", "key", key, "err", err)
		return ""
	}
	return sessionID
}

type plexProtocolStop interface {
	StopProtocol(context.Context, frontend.Protocol) error
}
