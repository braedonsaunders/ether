package frontend

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
)

// CapabilityRequest describes client declarations, not a claim about playback.
// Viewer must come from the authenticated session when user rules are desired.
type CapabilityRequest struct {
	Viewer       *core.Viewer
	Device       caps.DeviceInfo
	Profile      caps.DeviceProfile
	ProtocolInfo []string
	UserAgent    string
}

// CapabilityResult pairs client playback capabilities with the selected owner
// quality profile. The profile does not grant additional hardware capabilities.
type CapabilityResult struct {
	Client  core.ClientCaps
	Quality core.QualityProfile
}

// Capabilities applies client declarations, device quirks and owner rules.
type Capabilities interface {
	Build(context.Context, CapabilityRequest) (CapabilityResult, error)
}
