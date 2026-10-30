package frontend

import "errors"

// ErrSourceGone means previously offered source facts no longer match a
// playable source; the client must request fresh detail.
var ErrSourceGone = errors.New("frontend: offered playback source is gone")
