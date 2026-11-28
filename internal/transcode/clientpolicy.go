package transcode

import (
	"reflect"
	"slices"

	"github.com/braedonsaunders/ether/internal/clientreg"
	"github.com/braedonsaunders/ether/internal/core"
)

// ClientPolicy records the registry's family defaults for one client.
// Headers and profile declarations remain authoritative; a model name alone
// does not certify a display or an audio chain.
type ClientPolicy struct {
	Family              string   `json:"family"`
	DolbyVisionProfiles []int    `json:"dolbyVisionProfiles"`
	SubtitleCodecs      []string `json:"subtitleCodecs"`
	Notes               string   `json:"notes"`
	AlwaysEncodeVideo   bool     `json:"alwaysEncodeVideo,omitempty"`
}

func identityOf(c core.ClientCaps) clientreg.Identity {
	id := identityWith(c.Product, c.Platform, c.Device, c.DeviceName, c.Model, c.Version)
	if id.Device == "" {
		// ClientCaps historically fall back to the friendly name here.
		id.Device = c.DeviceName
	}
	return id
}

// identityWith keeps the device name separate: built-in registry rules match
// only the verified device, so a friendly name such as "Fire TV Stick 4K Max"
// never unlocks a preset, while owner overrides may match either field.
func identityWith(product, platform, device, deviceName, model, version string) clientreg.Identity {
	return clientreg.Identity{Product: product, Platform: platform, Device: device, DeviceName: deviceName, Model: model, Version: version}
}

// PlaybackDefaults is the registry baseline for a client that sent no profile.
// ID is the matched playback record. "generic" means nothing more specific matched.
type PlaybackDefaults struct {
	ID                               string
	Video, Audio, Containers         []string
	MaxHeight                        int
	HDR10, DolbyVision, HeightLocked bool
	// DolbyVisionOff: an owner override turned Dolby Vision off, so a
	// profile 7/8 copy carries only its compatible base layer.
	DolbyVisionOff bool
	// AlwaysEncodeVideo: an owner override disabled video copy on streams.
	AlwaysEncodeVideo bool
}

// PlaybackDefaultsFor evaluates the registry, including owner overrides.
// Model is separate from ClientCaps because Plex carries it on the request.
func PlaybackDefaultsFor(product, platform, device, deviceName, model, version string, overrides []core.ClientCapabilityOverride) PlaybackDefaults {
	got := clientreg.Playback(identityWith(product, platform, device, deviceName, model, version), overrides)
	return PlaybackDefaults{
		ID: got.ID, Video: got.Video, Audio: got.Audio, Containers: got.Containers,
		MaxHeight: got.MaxHeight, HDR10: got.HDR10, DolbyVision: got.DolbyVision, HeightLocked: got.HeightLocked,
		DolbyVisionOff: got.DolbyVisionOff, AlwaysEncodeVideo: got.AlwaysEncodeVideo,
	}
}

// PolicyStamp returns Dolby Vision profiles and subtitle codecs when an owner
// override changed them. Unchanged defaults stay unset so a Plex client cap
// matches the historical shape.
func PolicyStamp(product, platform, device, deviceName, model, version string, overrides []core.ClientCapabilityOverride) (dv []int, subs []string, dvChanged, subsChanged bool) {
	id := identityWith(product, platform, device, deviceName, model, version)
	plain, edited := clientreg.Policy(id, nil), clientreg.Policy(id, overrides)
	return slices.Clone(edited.DolbyVisionProfiles), slices.Clone(edited.Subtitles),
		!slices.Equal(plain.DolbyVisionProfiles, edited.DolbyVisionProfiles),
		!slices.Equal(plain.Subtitles, edited.Subtitles)
}

// HasQuirk reports a named registry quirk for this client identity.
func HasQuirk(product, platform, device, model, version, name string) bool {
	return clientreg.HasQuirk(clientreg.Identity{Product: product, Platform: platform, Device: device, Model: model, Version: version}, name)
}

// ClientPolicyFor resolves the embedded registry with no owner overrides.
func ClientPolicyFor(c core.ClientCaps) ClientPolicy {
	return ResolvePolicy(c, nil)
}

// ResolvePolicy layers owner overrides on the registry, then lets a runtime
// declaration replace Dolby Vision profiles and subtitle codecs unless forced.
func ResolvePolicy(c core.ClientCaps, overrides []core.ClientCapabilityOverride) ClientPolicy {
	c = ApplyForcedOverrides(c, "", overrides)
	got := clientreg.Policy(identityOf(c), overrides)
	policy := ClientPolicy{
		Family:              got.Family,
		DolbyVisionProfiles: slices.Clone(got.DolbyVisionProfiles),
		SubtitleCodecs:      slices.Clone(got.Subtitles),
		Notes:               got.Notes,
		AlwaysEncodeVideo:   got.AlwaysEncodeVideo,
	}
	if len(c.DVProfiles) > 0 || c.DVProfilesExplicit {
		policy.DolbyVisionProfiles = slices.Clone(c.DVProfiles)
	}
	if c.SubtitleCodecs != nil {
		policy.SubtitleCodecs = slices.Clone(c.SubtitleCodecs)
	}
	return policy
}

// SupportsDVProfile requires both the generic DV flag and a compatible profile.
// With no model evidence an unspecified profile is accepted only if the client
// explicitly advertised DV; known unsupported profiles use their base layer.
func SupportsDVProfile(c core.ClientCaps, profile int) bool {
	if !c.DolbyVision || (c.DVProfilesExplicit && len(c.DVProfiles) == 0) {
		return false
	}
	if profile == 0 {
		return true
	}
	return slices.Contains(ClientPolicyFor(c).DolbyVisionProfiles, profile)
}

func supportsSubtitle(c core.ClientCaps, codec string) bool {
	return slices.Contains(ClientPolicyFor(c).SubtitleCodecs, codec)
}

// ApplyForcedOverrides replaces only explicitly edited facts after runtime
// declarations. Matching is identical to the registry baseline's selectors.
func ApplyForcedOverrides(c core.ClientCaps, model string, overrides []core.ClientCapabilityOverride) core.ClientCaps {
	if !slices.ContainsFunc(overrides, func(o core.ClientCapabilityOverride) bool { return o.Force }) {
		return c
	}
	id := identityOf(c)
	id.Model = model
	for _, i := range clientreg.MatchingOverrides(id, overrides) {
		o := overrides[i]
		if !o.Force {
			continue
		}
		before := c
		if o.VideoCodecs != nil {
			c.VideoCodecs = slices.Clone(o.VideoCodecs)
		}
		if o.AudioCodecs != nil {
			c.AudioCodecs = slices.Clone(o.AudioCodecs)
		}
		if o.Containers != nil {
			c.Containers = slices.Clone(o.Containers)
		}
		if o.SubtitleCodecs != nil {
			c.SubtitleCodecs = slices.Clone(o.SubtitleCodecs)
		}
		if o.DolbyVisionProfiles != nil {
			c.DVProfiles = slices.Clone(o.DolbyVisionProfiles)
			c.DVProfileVariants = nil
			c.DVProfilesExplicit = len(o.DolbyVisionProfiles) == 0
			c.DVProfilesForced = len(o.DolbyVisionProfiles) > 0
		}
		if o.MaxHeight > 0 {
			c.MaxHeight = o.MaxHeight
			c.ExplicitMaxHeight = true
			c.MaxHeightSource = "App compatibility forced override"
		}
		if o.MaxBitrateKbps > 0 {
			c.MaxBitrateKbps = o.MaxBitrateKbps
			c.DefaultRemoteBitrate = false
		}
		if o.HDR10 != nil {
			c.HDR10 = *o.HDR10
			c.ExplicitNoHDR = !*o.HDR10
		}
		if o.DolbyVision != nil {
			c.DolbyVision = *o.DolbyVision
			c.DolbyVisionBaseOnly = !*o.DolbyVision
		}
		if o.AlwaysEncodeVideo {
			c.AlwaysEncodeVideo = true
		}
		if !reflect.DeepEqual(before, c) {
			c.ForcedOverrideApplied = true
		}
	}
	return c
}
