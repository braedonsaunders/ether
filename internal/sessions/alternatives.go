package sessions

import (
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// A successful HLS start is not evidence that an estimated or unprobed video
// is compatible. Advertise HLS only when the source codec is actually known.
func knownHLSVideo(info core.MediaInfo) bool {
	if info.Estimated {
		return false
	}
	for _, stream := range info.Streams {
		if stream.Type == "video" && stream.Codec != "" {
			return true
		}
	}
	return false
}

// planAlternatives evaluates the already-open source: it never resolves a
// second release, starts an encoder, or consumes another stream slot.
func (b *Broker) planAlternatives(source *core.PlaySession, req frontend.PlanRequest) (direct, remux, encode bool) {
	if req.Intent.Options.DirectPlay {
		candidate := req
		candidate.Intent.Options.DirectPlay = true
		candidate.Intent.Options.DirectStream = false
		d, err := b.decideProfile(source, candidate)
		direct = err == nil && d.Kind == core.DecisionDirectPlay
	}
	if req.Intent.Options.DirectStream && b.deps.Transcoder != nil {
		candidate := req
		candidate.Intent.Options.DirectPlay = false
		candidate.Intent.Options.DirectStream = true
		d, err := b.decideProfile(source, candidate)
		remux = err == nil && d.Kind == core.DecisionRemux && !decisionEncodes(d, source.Info)
	}
	if b.deps.Transcoder != nil && (req.Intent.AllowTranscoding == nil || *req.Intent.AllowTranscoding) {
		candidate := req
		candidate.Intent.Options.DirectPlay = false
		candidate.Intent.Options.DirectStream = false
		d, err := b.decideProfile(source, candidate)
		encode = err == nil && decisionEncodes(d, source.Info)
	}
	return
}

// hlsPlanOutput uses the very same output decision as StartStream. Only
// containers preserved by the transcoder are authoritative here; an unknown
// value may be normalized later and must not become a guessed DTO claim.
func (b *Broker) hlsPlanOutput(source *core.PlaySession, req frontend.PlanRequest, decision core.Decision) (bool, string) {
	output, eligible := b.hlsOutputDecision(source, req.Caps, req.Intent.Options, req.Intent.AllowTranscoding, decision)
	if !eligible || source == nil || !knownHLSVideo(source.Info) {
		return false, ""
	}
	switch output.Container {
	case "mpegts", "fmp4", "mkv":
		return true, output.Container
	default:
		return true, ""
	}
}
