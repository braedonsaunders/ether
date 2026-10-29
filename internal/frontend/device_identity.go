package frontend

import (
	"context"
	"time"
)

// DeviceIdentity optionally validates a credential against its stored device
// and returns the grant's current absolute expiry. This read-only operation
// does not extend the grant or write activity on media segment requests.
type DeviceIdentity interface {
	ResolveDevice(context.Context, string, Device) (*Viewer, time.Time, error)
}
