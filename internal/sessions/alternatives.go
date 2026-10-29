package sessions

import (
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

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
