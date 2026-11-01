package personalize

import (
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Shared in-memory state of the Netflix-style hooks (items #30–#32): the
// live session-intent opens and the per-session post-play rankings. It is
// created lazily so the engine's construction stays untouched.

type hookState struct {
	mu       sync.Mutex
	intent   map[int64]*userIntent
	postplay map[postplayKey]*postplayEntry
}

type postplayKey struct {
	user       int64
	key        core.RatingKey // taste key of the just-watched title
	exclusions string
}

type postplayEntry struct {
	picks []core.PostPlayPick
	at    time.Time
	hour  int // local hour the ranking was computed for
}

func (e *Engine) hookState() *hookState {
	e.hookOnce.Do(func() {
		e.hook = &hookState{
			intent:   map[int64]*userIntent{},
			postplay: map[postplayKey]*postplayEntry{},
		}
	})
	return e.hook
}
