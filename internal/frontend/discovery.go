package frontend

import (
	"context"
	"net/netip"
)

// DiscoveryRequest describes a LAN datagram and this surface's verified,
// reachable base address. Data is a private copy and contains no credentials.
type DiscoveryRequest struct {
	Data     []byte
	Peer     netip.AddrPort
	Endpoint string
}

// DiscoveryResponder is optional on an Instance. The host multiplexes its
// responses through a single UDP listener and unregisters it on unmount.
// Implementations return false for unmatched probes or disabled discovery.
type DiscoveryResponder interface {
	RespondDiscovery(context.Context, DiscoveryRequest) ([]byte, bool)
}
