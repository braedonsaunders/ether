package frontend

import (
	"context"
	"net/netip"
)

// ViewerCapabilities are informational only. Nil fields are unknown; all
// operations remain subject to their authoritative policy checks.
type ViewerCapabilities struct {
	IsOwner           *bool   `json:"isOwner,omitempty"`
	CanPlayRemote     *bool   `json:"canPlayRemote,omitempty"`
	RemoteBitrateKbps *int    `json:"remoteBitrateKbps,omitempty"`
	CanTranscode      *bool   `json:"canTranscode,omitempty"`
	CanUseLiveTV      *bool   `json:"canUseLiveTV,omitempty"`
	HasLocalPassword  *bool   `json:"hasLocalPassword,omitempty"`
	AllowedLibraries  *[]View `json:"allowedLibraries,omitempty"`
	MaxRating         *string `json:"maxRating,omitempty"`
	CanDownload       *bool   `json:"canDownload,omitempty"`
	PeerRemote        *bool   `json:"peerRemote,omitempty"`
}

// ViewerCapabilitiesFunc receives a verified network peer, never a client-supplied forwarding value.
type ViewerCapabilitiesFunc func(context.Context, *Viewer, Device, netip.Addr) (ViewerCapabilities, error)
