package sessions

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

// retireBackends detaches encoder and source work without treating Stop's
// return as proof that either finished. A nil error from this function only
// means retirement was accepted; settleRetirement runs after the outcomes.
func (b *Broker) retireBackends(ctx context.Context, s *brokerSession, stopSource bool) error {
	if s == nil {
		return nil
	}
	var src core.SourceRetirement
	var enc core.EncoderRetirement
	var failed error
	if b.deps.Transcoder != nil {
		sess, ok := b.deps.Transcoder.Session(s.public.ID)
		if ok && sess != nil {
			if owned, isOwned := sess.(core.OwnedEncoderSession); isOwned {
				enc = owned.EncoderRetirement()
				if err := owned.RetireEncoder(); err != nil {
					failed = err
					enc = nil
				}
			} else {
				b.deps.Transcoder.Stop(s.public.ID)
			}
		} else {
			b.deps.Transcoder.Stop(s.public.ID)
		}
	}
	if stopSource {
		if owned, isOwned := b.deps.Playback.(core.OwnedSourcePlayback); isOwned {
			outcome, ok := owned.SourceRetirement(s.playID)
			if !ok {
				b.deps.Playback.Stop(s.playID)
			} else if err := owned.RetireSource(s.playID, outcome); err != nil {
				if failed == nil {
					failed = err
				}
			} else {
				src = outcome
			}
		} else {
			b.deps.Playback.Stop(s.playID)
		}
	}
	if src == nil && enc == nil {
		b.settleRetirement(context.WithoutCancel(ctx), s, failed == nil)
		return failed
	}
	go b.awaitRetirement(context.WithoutCancel(ctx), s, src, enc, failed != nil)
	return failed
}

func (b *Broker) awaitRetirement(ctx context.Context, s *brokerSession, src core.SourceRetirement, enc core.EncoderRetirement, failed bool) {
	if src != nil && src.Done() != nil {
		select {
		case <-src.Done():
			if err := src.Err(); err != nil {
				failed = true
				if b.deps.OnEventError != nil {
					b.deps.OnEventError(err)
				}
			}
		case <-ctx.Done():
			failed = true
		}
	}
	if enc != nil && enc.EncoderDone() != nil {
		select {
		case <-enc.EncoderDone():
			if err := enc.EncoderErr(); err != nil {
				failed = true
				if b.deps.OnEventError != nil {
					b.deps.OnEventError(err)
				}
			}
		case <-ctx.Done():
			failed = true
		}
	}
	// Encoder input that is still unverified stays pending until EncoderDone.
	// A closed Done with ErrEncoderInputUnverified is not a successful drain.
	b.settleRetirement(ctx, s, !failed)
}
