package sessions

import (
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// DecisionRequest contains the source and client inputs for a playback
// decision. PreferRemux is the adapter's signal that source changes need
// refreshed container headers; the source and requested direct-stream mode
// still determine whether the override applies.
type DecisionRequest struct {
	Session              *core.PlaySession
	Caps                 core.ClientCaps
	Options              core.DecideOptions
	PreferRemux          bool
	Protocol             string
	RemoteMaxBitrateKbps int
}

// Decide preserves the playback remux override for on-demand sources and
// stops the source when a required converter is unavailable.
func Decide(playback core.Playback, transcoder core.Transcoder, r DecisionRequest) (core.Decision, error) {
	ps := r.Session
	if ps == nil {
		return core.Decision{}, core.ErrNotFound
	}
	dec := core.Decision{Kind: core.DecisionDirectPlay, VideoCopy: true, AudioCopy: true, Container: ps.Info.Container, AudioStream: r.Options.AudioStream, SubtitleStream: r.Options.SubtitleStream}
	if transcoder != nil {
		dec = transcoder.Decide(ps.Info, r.Caps, r.Options)
	} else {
		dec = transcode.Decide(ps.Info, r.Caps, r.Options, transcode.Policy{RemoteMaxBitrateKbps: r.RemoteMaxBitrateKbps})
		if dec.Kind != core.DecisionDirectPlay {
			if playback != nil {
				playback.Stop(ps.ID)
			}
			return core.Decision{}, fmt.Errorf("this client requires media conversion; install or configure FFmpeg")
		}
	}
	// Usenet sources can change during failover. Copy-only remux gives MDE
	// clients fresh container headers; RequiresRemux applies to all clients.
	// Debrid's stable HTTP file does not need an optional remux.
	onDemand := ps.Resolution != nil && !strings.HasPrefix(ps.Resolution.Release.ID, "local:")
	requiresRemux := ps.RequiresRemux
	if rr, ok := playback.(core.RemuxRequirer); ok && rr.RequiresRemux(ps.ID) {
		requiresRemux = true
	}
	preferRemux := r.PreferRemux && onDemand && ps.Resolution.Release.Protocol != core.ProtocolTorrent
	if (preferRemux || requiresRemux) && r.Options.DirectStream && onDemand && dec.Kind == core.DecisionDirectPlay && transcoder != nil {
		dec.Kind = core.DecisionRemux
		dec.VideoCopy, dec.AudioCopy = true, true
		videoCodec, audioCodec := "", ""
		if v := ps.Info.Video(); v != nil {
			videoCodec = v.Codec
		}
		for _, st := range ps.Info.Streams {
			if st.Type == "audio" && st.Index == dec.AudioStream {
				audioCodec = st.Codec
				break
			}
		}
		if r.Protocol == core.ProtocolDASH {
			dec.Container = transcode.ContainerFMP4
		} else {
			dec.Container = transcode.HLSContainer(r.Caps, videoCodec, audioCodec)
		}
		dec.Reason = "Direct stream preserves playback across source changes."
	}
	return dec, nil
}
