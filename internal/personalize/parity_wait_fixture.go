//go:build plexparity

package personalize

import (
	"context"
	"fmt"
)

// ParityWaitForGeneric waits for the real startup model before the harness
// serially warms viewers. It is compiled only into disposable parity builds.
func (e *Engine) ParityWaitForGeneric(ctx context.Context) error {
	e.mu.Lock()
	f := e.flights[genericID]
	e.mu.Unlock()
	if f != nil {
		select {
		case <-f.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	e.mu.Lock()
	ready := e.models[genericID] != nil
	e.mu.Unlock()
	if !ready {
		return fmt.Errorf("parity generic model did not build")
	}
	return nil
}

// ParitySyncEvents waits until watch-state events already queued have been
// handled. Disposable parity builds use it so a later play assertion observes
// the subscriber. Production callers do not see this method.
func (e *Engine) ParitySyncEvents(ctx context.Context) {
	e.syncEvents(ctx)
}
