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

// GrantDeviceIdentity retrieves the stored binding for a surface credential.
// An empty claim permits token-only requests; a nonempty claim must match the
// stored device ID. The operation never extends the grant or writes activity.
type GrantDeviceIdentity interface {
	ResolveGrantDevice(context.Context, string, string) (*Viewer, Device, time.Time, error)
}
