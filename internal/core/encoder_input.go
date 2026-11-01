package core

import (
	"context"
	"errors"
)

// EncoderInputScopeHeader is reserved for a manager-issued input lease. It is
// never provider metadata, client authorization, a source lease or a URL token.
const EncoderInputScopeHeader = "X-Ether-Encoder-Input"

var ErrEncoderInputPending = errors.New("encoder input producers are pending")

// EncoderInputLease is issued by PlaybackEncoderInputs for one exact source
// lifetime. Host composition passes the opaque object; wire inputs cannot mint
// one. Each encoder must obtain its own lease, including shared-source owners.
type EncoderInputLease interface {
	// ClaimInput succeeds once, before seal. Failed claims never call onDrained.
	// Successful claims call it exactly once, possibly before this returns,
	// after sealed producers and registry ownership are released. The trusted
	// callback must only do bounded accounting/requeue; no I/O, waits or source
	// operations. It runs outside every manager/session/scope lock.
	ClaimInput(onDrained func(error)) error
	HeadersForInput(inputURL string) (map[string]string, error)
	SealInput()
	InputDone() <-chan struct{}
	InputErr() error
}

type PlaybackEncoderInputs interface {
	ReserveEncoderInput(context.Context, string, SourceRetirement) (EncoderInputLease, error)
}
