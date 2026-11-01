package livetv

import (
	"context"
	"net/http"

	"github.com/braedonsaunders/ether/internal/core"
)

// ServeHTTP writes Plex for Mac's protocol=http Matroska stream for a
// live session, when the decision can convert the channel.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request, id string, u *core.User) error {
	ss, err := s.getSession(r.Context(), id, u)
	if err != nil {
		writeStreamError(w, err)
		return nil
	}
	hs, err := s.ensureHTTP(r.Context(), ss, id)
	if err != nil {
		return err
	}
	hs.ServeHTTP(w, r)
	return nil
}

func (s *Service) ensureHTTP(ctx context.Context, ss *session, id string) (core.HTTPSession, error) {
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return nil, core.ErrNotFound
	}
	ss.consumed = true
	if ss.http != nil {
		h := ss.http
		ss.mu.Unlock()
		return h, nil
	}
	if s.o.Transcoder == nil {
		ss.mu.Unlock()
		return nil, core.ErrNotAvailable
	}
	info := ss.info.MediaInfo
	if info == nil {
		ss.mu.Unlock()
		probed, err := s.probeSessionInput(ctx, ss)
		if err != nil {
			return nil, err
		}
		if probed == nil {
			return nil, core.ErrNotAvailable
		}
		ss.mu.Lock()
		if ss.closed {
			ss.mu.Unlock()
			return nil, core.ErrNotFound
		}
		if ss.http != nil {
			h := ss.http
			ss.mu.Unlock()
			return h, nil
		}
		info = probed
		ss.info.MediaInfo = probed
	}
	copyInfo := *info
	copyInfo.Duration = 0
	dec := s.o.Transcoder.Decide(copyInfo, ss.caps, core.DecideOptions{
		DirectStream: true, AudioStream: -1, SubtitleStream: -1, Protocol: core.ProtocolHTTP,
	})
	if dec.Kind == core.DecisionDirectPlay {
		dec.Kind = core.DecisionRemux
		dec.VideoCopy, dec.AudioCopy = true, true
	}
	dec.Container = "mkv"
	liveInput := ss.info.Channel.Kind != "virtual"
	var input string
	if liveInput {
		var err error
		input, err = s.relayInput(ss)
		if err != nil {
			ss.mu.Unlock()
			return nil, err
		}
	} else {
		if s.o.Playback == nil || len(ss.tracks) == 0 || ss.tracks[0].playbackID == "" {
			ss.mu.Unlock()
			return nil, core.ErrNotAvailable
		}
		input = s.o.Playback.InputURL(ss.tracks[0].playbackID)
	}
	hs, err := s.o.Transcoder.StartHLS(s.ctx, core.HLSRequest{
		SessionID: "livetv-http-" + id, InputURL: input, LiveInput: liveInput,
		Info: copyInfo, Decision: dec, Protocol: core.ProtocolHTTP, SegmentDuration: segmentDuration,
	})
	if err != nil {
		ss.mu.Unlock()
		return nil, err
	}
	stream, ok := hs.(core.HTTPSession)
	if !ok {
		hs.Close()
		ss.mu.Unlock()
		return nil, core.ErrNotAvailable
	}
	ss.http = stream
	ss.mu.Unlock()
	return stream, nil
}
