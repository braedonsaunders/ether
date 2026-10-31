package transcode

import (
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
}

func identityOf(c core.ClientCaps) clientreg.Identity {
	return clientreg.Identity{Product: c.Product, Platform: c.Platform, Device: c.Device, Version: c.Version}
}

// PlaybackDefaults is the registry baseline for a client that sent no profile.
type PlaybackDefaults struct {
	Video, Audio, Containers         []string
	MaxHeight                        int
	HDR10, DolbyVision, HeightLocked bool
}

// PlaybackDefaultsFor evaluates the registry, including owner overrides.
// Model is separate from ClientCaps because Plex carries it on the request.
func PlaybackDefaultsFor(product, platform, device, model, version string, overrides []core.ClientCapabilityOverride) PlaybackDefaults {
	got := clientreg.Playback(clientreg.Identity{Product: product, Platform: platform, Device: device, Model: model, Version: version}, overrides)
	return PlaybackDefaults{
		Video: got.Video, Audio: got.Audio, Containers: got.Containers,
		MaxHeight: got.MaxHeight, HDR10: got.HDR10, DolbyVision: got.DolbyVision, HeightLocked: got.HeightLocked,
	}
}

// PolicyStamp returns Dolby Vision profiles and subtitle codecs when an owner
// override changed them. Unchanged defaults stay unset so a Plex client cap
// matches the historical shape.
func PolicyStamp(product, platform, device, model, version string, overrides []core.ClientCapabilityOverride) (dv []int, subs []string, dvChanged, subsChanged bool) {
	id := clientreg.Identity{Product: product, Platform: platform, Device: device, Model: model, Version: version}
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
// declaration replace Dolby Vision profiles and subtitle codecs.
func ResolvePolicy(c core.ClientCaps, overrides []core.ClientCapabilityOverride) ClientPolicy {
	got := clientreg.Policy(identityOf(c), overrides)
	policy := ClientPolicy{
		Family:              got.Family,
		DolbyVisionProfiles: slices.Clone(got.DolbyVisionProfiles),
		SubtitleCodecs:      slices.Clone(got.Subtitles),
		Notes:               got.Notes,
	}
	if len(c.DVProfiles) > 0 {
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
	if !c.DolbyVision {
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

// ClientPolicies is the owner-facing supplemental quirk matrix. It describes
// defaults only; a real request's profile and hardware declarations refine it.
func ClientPolicies() []ClientPolicy {
	products := []string{"Apple TV", "iOS", "Android TV", "Google TV", "NVIDIA Shield", "Roku", "Fire TV", "LG webOS", "Samsung Tizen", "Plex Web Chrome", "Plex Web Firefox", "Plex Web Safari", "Plex Web Edge", "Xbox", "PlayStation", "HTPC"}
	out := make([]ClientPolicy, 0, len(products))
	for _, product := range products {
		p := ClientPolicyFor(core.ClientCaps{Product: product})
		p.Family = product
		out = append(out, p)
	}
	return out
}
