package nodeinput

import (
	"context"

	"github.com/braedonsaunders/ether/internal/core"
)

// MaxRecoveryRead bounds each optional index read. Larger indexes must use
// multiple bounded calls; the caller separately bounds total bytes and work.
const MaxRecoveryRead = 64 << 10

var _ core.RecoverySourceReader = (*Input)(nil)

// RecoverySourceIdentity describes the already authenticated, immutable source
// of this claimed input. It is not current remote authority, retained output/AU
// evidence, or permission to claim another input or start another encoder.
func (in *Input) RecoverySourceIdentity() (revision string, size int64, err error) {
	if in == nil || in.g == nil {
		return "", 0, ErrDenied
	}
	in.g.mu.Lock()
	defer in.g.mu.Unlock()
	return in.recoveryIdentityLocked()
}

func (in *Input) recoveryIdentityLocked() (string, int64, error) {
	if in.g.closed || !in.claimed || in.sealed || in.reader == nil || in.ctx.Err() != nil || in.scope.Revision == "" {
		return "", 0, ErrDenied
	}
	// Open authenticated the complete Scope and HEAD representation. Renewal
	// cannot change either. Never derive identity from a recovery request.
	size := in.reader.Size()
	if size <= 0 || size != in.size || in.reader.ReleaseID() != in.scope.Revision {
		return "", 0, ErrDenied
	}
	return in.scope.Revision, size, nil
}

// ReadRecoveryAt reads only a complete bounded range of the same claimed input.
// On error n is zero and callers must discard p. Errors expose no source details.
// It creates no read goroutine: cancellation is not completion, and the existing
// Input tracker retains this call until the actual Reader operation returns.
func (in *Input) ReadRecoveryAt(ctx context.Context, expectedRevision string, p []byte, off int64) (int, error) {
	if in == nil || in.g == nil || ctx == nil {
		return 0, ErrDenied
	}
	g := in.g
	g.mu.Lock()
	revision, size, err := in.recoveryIdentityLocked()
	if err != nil || expectedRevision != revision || len(p) == 0 || len(p) > MaxRecoveryRead || off < 0 || off > size || int64(len(p)) > size-off || g.active >= g.o.MaxRequests {
		g.mu.Unlock()
		return 0, ErrDenied
	}
	if err := ctx.Err(); err != nil {
		g.mu.Unlock()
		return 0, err
	}
	end, ok := in.tracker.Begin()
	if !ok {
		g.mu.Unlock()
		return 0, ErrDenied
	}
	g.active++
	reader := in.reader
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.active--
		g.mu.Unlock()
		end()
	}()
	combined, cancel := context.WithTimeout(ctx, g.o.RequestTimeout)
	stop := context.AfterFunc(in.ctx, cancel)
	defer cancel()
	defer stop()
	combined = core.WithReadRetirement(combined, &in.tracker)
	n, readErr := reader.ReadAtContext(combined, p, off)
	g.mu.Lock()
	defer g.mu.Unlock()
	if in.sealed || in.reader != reader {
		return 0, ErrDenied
	}
	if err := combined.Err(); err != nil {
		return 0, err
	}
	if readErr != nil || n != len(p) {
		return 0, ErrDenied
	}
	return n, nil
}
