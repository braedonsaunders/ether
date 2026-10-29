package frontend

import "context"

// Ticket is a one-use, purpose-bound authorization for a realtime handshake.
type Ticket struct {
	Viewer *Viewer
	Device Device
	// SourceHash identifies the issuing grant for host-side revalidation only.
	SourceHash string
}
type Tickets interface {
	Issue(context.Context, string, Device, string) (string, error)
	Consume(context.Context, string, string) (Ticket, error)
	Active(context.Context, Ticket) (bool, error)
}
