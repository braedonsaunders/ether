package frontend

import (
	"context"
	"net/netip"
)

// ViewerCapabilities are informational only. Nil fields are unknown; all
// operations remain subject to their authoritative policy checks.
type ViewerCapabilities struct {
	IsOwner           *bool     `json:"isOwner,omitempty"`
	CanPlayRemote     *bool     `json:"canPlayRemote,omitempty"`
	CanPlay           *bool     `json:"canPlay,omitempty"`
	RemoteBitrateKbps *int      `json:"remoteBitrateKbps,omitempty"`
	CanTranscode      *bool     `json:"canTranscode,omitempty"`
	CanTranscodeAudio *bool     `json:"canTranscodeAudio,omitempty"`
	CanTranscodeVideo *bool     `json:"canTranscodeVideo,omitempty"`
	CanRemux          *bool     `json:"canRemux,omitempty"`
	CanUseLiveTV      *bool     `json:"canUseLiveTV,omitempty"`
	CanManageLiveTV   *bool     `json:"canManageLiveTV,omitempty"`
	HasLocalPassword  *bool     `json:"hasLocalPassword,omitempty"`
	AllowedLibraries  *[]View   `json:"allowedLibraries,omitempty"`
	MaxRating         *string   `json:"maxRating,omitempty"`
	CanDownload       *bool     `json:"canDownload,omitempty"`
	MaxActiveSessions *int      `json:"maxActiveSessions,omitempty"`
	BlockedTags       *[]string `json:"blockedTags,omitempty"`
	PeerRemote        *bool     `json:"peerRemote,omitempty"`
}

// ViewerCapabilitiesFunc receives a verified network peer, never a client-supplied forwarding value.
type ViewerCapabilitiesFunc func(context.Context, *Viewer, Device, netip.Addr) (ViewerCapabilities, error)
