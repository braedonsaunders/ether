package core

import (
	"context"
	"errors"
)

var ErrEncoderPending = errors.New("encoder retirement is pending")
var ErrEncoderInputUnverified = errors.New("encoder input producer retirement is unverified")

// EncoderRetirement covers one exact encoder session, including all its runs
// and preflights. EncoderDone closes only at a terminal result. EncoderErr is
// ErrEncoderPending before then and is immutable afterwards; only nil proves
// full retirement. SourceRetirement alone never supplies this proof.
type EncoderRetirement interface {
	EncoderDone() <-chan struct{}
	EncoderErr() error
}

// OwnedEncoderSession retires this exact object, never a replacement with the
// same public ID. RetireEncoder admits bounded asynchronous detachment; callers
// must inspect EncoderRetirement for completion, including input producers.
type OwnedEncoderSession interface {
	HLSSession
	EncoderRetirement() EncoderRetirement
	RetireEncoder() error
}

// OwnedTranscoder returns responsibility even when a canceled start is still
// completing. A late start cannot publish after that responsibility is sealed.
type OwnedTranscoder interface {
	StartHLSOwned(context.Context, HLSRequest) (HLSSession, EncoderRetirement, error)
}
