package playback

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// warmPreparationContext bounds speculative work until a Play flight takes
// responsibility for it. Promotion retains the same in-flight operations,
// bounded by that flight's deadline and cancellation, and clears the
// core.IsSpeculative mark. Shutdown always wins.
func warmPreparationContext(parent context.Context, budget time.Duration, foreground <-chan context.Context) (context.Context, context.CancelFunc) {
	ctx, promote := core.WithSpeculative(parent)
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		timer := time.NewTimer(budget)
		defer timer.Stop()
		defer cancel()
		select {
		case live := <-foreground:
			promote()
			select {
			case <-live.Done():
			case <-ctx.Done():
			}
		case <-timer.C:
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}
