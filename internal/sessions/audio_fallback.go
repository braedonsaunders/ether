package sessions

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type audioRendition struct {
	id         string
	generation uint64
	session    core.HLSSession
}

func (b *Broker) AudioFallbackAvailable() bool {
	available, ok := b.deps.Transcoder.(interface{ AudioFallbackAvailable() bool })
	return ok && available.AudioFallbackAvailable()
}

func (b *Broker) StartAudioFallback(ctx context.Context, a frontend.SessionAccess, id string, track int, position time.Duration) (frontend.AudioFallbackResult, error) {
	if !b.AudioFallbackAvailable() || position < 0 {
		return frontend.AudioFallbackResult{}, core.ErrNotAvailable
	}
	s, _, err := b.access(ctx, a, id)
	if err != nil {
		return frontend.AudioFallbackResult{}, err
	}
	unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", s.public.ViewerID, s.clientID))
	if err != nil {
		return frontend.AudioFallbackResult{}, err
	}
	defer unlock()
	s, viewer, err := b.access(ctx, a, id)
	if err != nil {
		return frontend.AudioFallbackResult{}, err
	}
	if s.live || !s.sourcePinned || s.decision.Reason != "original_runtime" || !b.currentMediaPolicy(ctx, viewer, s) || (s.allowTranscoding != nil && !*s.allowTranscoding) {
		return frontend.AudioFallbackResult{}, core.ErrNotAvailable
	}
	source, ok := b.deps.Playback.Session(s.playID)
	if !ok || sourceRevision(source) != s.sourceETag {
		return frontend.AudioFallbackResult{}, frontend.ErrSourceGone
	}
	if source.Info.Duration > 0 && position >= source.Info.Duration {
		return frontend.AudioFallbackResult{}, core.ErrNotAvailable
	}
	var audio core.StreamInfo
	found := false
	for _, stream := range source.Info.Streams {
		if stream.Type == "audio" && stream.Index == track {
			audio = stream
			found = true
			break
		}
	}
	if !found {
		return frontend.AudioFallbackResult{}, frontend.ErrTrackNotFound
	}
	select {
	case b.startSlots <- struct{}{}:
		defer func() { <-b.startSlots }()
	default:
		return frontend.AudioFallbackResult{}, &frontend.SessionBusyError{RetryAfter: time.Second}
	}
	if err = b.retireAudio(ctx, s); err != nil {
		return frontend.AudioFallbackResult{}, err
	}
	audioID := id + ".audio." + rand.Text()
	info := source.Info
	info.Streams = []core.StreamInfo{audio}
	decision := core.Decision{Kind: core.DecisionTranscode, Container: "fmp4", VideoCopy: true, AudioCopy: false, AudioCodec: "aac", AudioStream: track, SubtitleStream: -1, AudioChannels: min(2, max(1, audio.Channels)), AudioSampleRate: 48000, AudioBitrateKbps: 192, AudioFilters: s.opts.AudioFilters}
	lease, err := b.mintEncoderInput(ctx, s.playID)
	if err != nil {
		return frontend.AudioFallbackResult{}, err
	}
	request := core.HLSRequest{SessionID: audioID, ReleaseID: source.SourceReleaseID(), InputURL: b.deps.Playback.InputURL(s.playID), InputLease: lease, Info: info, Decision: decision, StartOffset: position, SegmentDuration: transcode.AudioFallbackSegmentDuration, Protocol: core.ProtocolHLS}
	hs, err := b.deps.Transcoder.StartHLS(context.WithoutCancel(ctx), request)
	if err != nil || hs == nil {
		if lease != nil {
			lease.SealInput()
		}
		if err == nil {
			err = core.ErrNotAvailable
		}
		return frontend.AudioFallbackResult{}, capacityError(err)
	}
	b.mu.Lock()
	live := b.sessions[id]
	if live == nil || b.closed || ctx.Err() != nil {
		b.mu.Unlock()
		b.deps.Transcoder.Stop(audioID)
		return frontend.AudioFallbackResult{}, core.ErrNotAvailable
	}
	live.audioGeneration++
	rendition := &audioRendition{id: audioID, generation: live.audioGeneration, session: hs}
	live.audio = rendition
	live.opts.AudioStream = track
	live.decision.AudioStream = track
	grant := live.grant
	b.mu.Unlock()
	return frontend.AudioFallbackResult{ID: audioID, Generation: rendition.generation, URL: "/sessions/media/" + id + "/" + grant + "/audio/" + audioID + "/index.m3u8"}, nil
}

// Retire the exact audio object before releasing a source pin or replacing it.
// The shared encoder lifetime includes any outstanding input readers.
func (b *Broker) retireAudio(ctx context.Context, s *brokerSession) error {
	b.mu.Lock()
	live := b.sessions[s.public.ID]
	var audio *audioRendition
	if live != nil {
		audio = live.audio
	}
	b.mu.Unlock()
	if audio == nil {
		return nil
	}
	var outcome core.EncoderRetirement
	if owned, ok := audio.session.(core.OwnedEncoderSession); ok {
		outcome = owned.EncoderRetirement()
		_ = owned.RetireEncoder()
	}
	b.deps.Transcoder.Stop(audio.id)
	if outcome != nil {
		select {
		case <-outcome.EncoderDone():
			if err := outcome.EncoderErr(); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	b.mu.Lock()
	if live := b.sessions[s.public.ID]; live != nil && live.audio == audio {
		live.audio = nil
	}
	b.mu.Unlock()
	return nil
}
func (b *Broker) StopAudioFallback(ctx context.Context, a frontend.SessionAccess, id, audioID string) error {
	s, _, err := b.access(ctx, a, id)
	if err != nil {
		return err
	}
	unlock, err := b.gates.Acquire(ctx, fmt.Sprintf("broker/owner/%d/%s", s.public.ViewerID, s.clientID))
	if err != nil {
		return err
	}
	defer unlock()
	b.mu.Lock()
	live := b.sessions[id]
	matches := live != nil && live.audio != nil && live.audio.id == audioID
	b.mu.Unlock()
	if !matches {
		return nil
	}
	return b.retireAudio(ctx, s)
}
func serveAudioRendition(w http.ResponseWriter, r *http.Request, s *brokerSession, tail []string) bool {
	if len(tail) == 0 || tail[0] != "audio" {
		return false
	}
	if len(tail) != 3 || s.audio == nil || s.audio.id != tail[1] {
		http.NotFound(w, r)
		return true
	}
	if tail[2] == "index.m3u8" {
		s.audio.session.ServePlaylist(w, r)
		return true
	}
	if n, ok := transcode.ParseSegmentName(tail[2]); ok && !strings.Contains(tail[2], "/") {
		s.audio.session.ServeSegment(w, r, n)
		return true
	}
	http.NotFound(w, r)
	return true
}
