package resolver

import (
	"fmt"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// QualityQuestion is one viewer on one app, at home or away, as playback
// sees them before a title is chosen.
type QualityQuestion struct {
	Quality config.Quality
	// Caps is the client as playback declares it: the capability registry,
	// the owner's App compatibility edits and the request headers, with the
	// remote bitrate cap already applied for a remote client.
	Caps core.ClientCaps
	// Builtin is the same client without the owner's App compatibility
	// edits, so the answer can say when an edit decided. Optional.
	Builtin *core.ClientCaps
	User    *core.User
	// Title is the title the viewer is asking about. Its media scopes the
	// profile and rule choice; nil leaves the question media-neutral.
	Title *core.Title
	// MediaType is the media scope when no title is supplied: "movie" or
	// "tv". Empty means unknown.
	MediaType string
	// RemoteMaxBitrateKbps is transcode.remoteMaxBitrateKbps; 0 = no cap.
	RemoteMaxBitrateKbps int
	// ViewerMaxBitrateKbps is the viewer's safety-policy cap; 0 = no cap.
	ViewerMaxBitrateKbps int
	// RemoteDisabled is set when the viewer may not stream away from home.
	RemoteDisabled bool
}

// Quality layer kinds, in the order playback applies them.
const (
	LayerProfile       = "profile"        // the quality profile picked for the client
	LayerDevice        = "device"         // what the app and device can play
	LayerRuleHeight    = "rule-height"    // a rule's Max quality replaces the device ceiling
	LayerAndroidLift   = "android-lift"   // a rule lifts Android's usual 1080p ceiling
	LayerRemoteCap     = "remote-cap"     // the away-from-home bitrate cap
	LayerViewerCap     = "viewer-cap"     // the viewer's own bitrate cap
	LayerRemoteBlocked = "remote-blocked" // the viewer may not stream away from home
)

// QualityLayer is one layer that took part in the answer.
type QualityLayer struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
	// Profile layer.
	Profile     string        `json:"profile,omitempty"`
	Source      ProfileSource `json:"source,omitempty"`
	Rule        int           `json:"rule,omitempty"` // 1-based rule number
	RuleUserID  int64         `json:"ruleUserId,omitempty"`
	BeatsRemote bool          `json:"beatsRemote,omitempty"` // a rule won over the Away profile
	// MaxHeight is the ceiling this layer sets; 0 = none.
	MaxHeight   int   `json:"maxHeight,omitempty"`
	HDR         *bool `json:"hdr,omitempty"`
	DolbyVision *bool `json:"dolbyVision,omitempty"`
	BitrateKbps int   `json:"bitrateKbps,omitempty"`
	// Edited is set when an App compatibility edit changed this device.
	Edited bool `json:"edited,omitempty"`
	// Ignored is set when the layer had a setting that did not apply.
	Ignored bool `json:"ignored,omitempty"`
	// Decisive marks the layer that set the final resolution ceiling, the
	// final bitrate cap, or blocked playback.
	Decisive bool `json:"decisive"`
}

// QualityAnswer is the ceiling a viewer's app gets and why.
type QualityAnswer struct {
	Profile     string         `json:"profile"`
	MediaType   string         `json:"mediaType,omitempty"` // the media the answer is scoped to
	MaxHeight   int            `json:"maxHeight"`           // 0 = no ceiling
	HDR         bool           `json:"hdr"`
	DolbyVision bool           `json:"dolbyVision"`
	BitrateKbps int            `json:"bitrateKbps"` // 0 = no cap
	Blocked     bool           `json:"blocked"`
	Layers      []QualityLayer `json:"layers"`
}

// ExplainQuality answers QualityQuestion with the code path playback uses:
// decideProfile (quality rules, Away profile, client clamp), then the
// remote and per-viewer bitrate caps in the order the play request applies
// them.
func ExplainQuality(in QualityQuestion) QualityAnswer {
	declared := in.Caps
	media := titleMedia(in.Title)
	if media == core.MediaAny {
		media = normalizeMedia(in.MediaType)
	}
	d := decideProfile(in.Quality, declared, in.User, media, "")
	choice, eff := d.effect.choice, d.effect
	out := QualityAnswer{Profile: d.profile.Name, MediaType: media, MaxHeight: d.profile.MaxHeight, HDR: d.profile.AllowHDR, DolbyVision: d.profile.AllowDolbyVision}

	configured := configuredMaxHeight(d.profile)
	if configured < 0 {
		configured = 0
	}
	profile := QualityLayer{Kind: LayerProfile, Profile: choice.profile.Name, Source: choice.source, MaxHeight: configured, HDR: boolp(choice.profile.AllowHDR), DolbyVision: boolp(choice.profile.AllowDolbyVision)}
	if choice.source == ProfileFromRule {
		rule := in.Quality.Rules[choice.rule]
		remote := remoteProfileName(in.Quality, media)
		profile.Rule = choice.rule + 1
		profile.RuleUserID = rule.UserID
		profile.BeatsRemote = declared.Remote && remote != "" && remote != choice.profile.Name
	}
	profile.Reason = profileLayerReason(profile)
	out.Layers = append(out.Layers, profile)

	device := QualityLayer{Kind: LayerDevice, MaxHeight: declared.MaxHeight, HDR: boolp(declared.HDR10), DolbyVision: boolp(declared.DolbyVision)}
	if b := in.Builtin; b != nil {
		device.Edited = b.MaxHeight != declared.MaxHeight || b.HDR10 != declared.HDR10 || b.DolbyVision != declared.DolbyVision
	}
	device.Reason = deviceLayerReason(device)
	out.Layers = append(out.Layers, device)

	var ruleLayer *QualityLayer
	if choice.source == ProfileFromRule {
		rule := in.Quality.Rules[choice.rule]
		switch {
		case eff.ruleHeight > 0:
			ruleLayer = &QualityLayer{Kind: LayerRuleHeight, Rule: choice.rule + 1, RuleUserID: rule.UserID, MaxHeight: eff.ruleHeight}
			ruleLayer.Reason = fmt.Sprintf("Rule %d sets Max quality to %s.", ruleLayer.Rule, heightName(eff.ruleHeight))
		case eff.heightIgnored:
			ruleLayer = &QualityLayer{Kind: LayerRuleHeight, Rule: choice.rule + 1, RuleUserID: rule.UserID, MaxHeight: rule.MaxHeight, Ignored: true}
			ruleLayer.Reason = fmt.Sprintf("Rule %d's Max quality is ignored: this device's resolution is fixed.", ruleLayer.Rule)
		case eff.lifted > 0:
			ruleLayer = &QualityLayer{Kind: LayerAndroidLift, Rule: choice.rule + 1, RuleUserID: rule.UserID, MaxHeight: eff.lifted}
			if eff.hdrLifted {
				ruleLayer.HDR = boolp(true)
			}
			ruleLayer.Reason = fmt.Sprintf("Rule %d lifts Android's usual 1080p limit to %s.", ruleLayer.Rule, heightName(eff.lifted))
		}
		if ruleLayer != nil {
			out.Layers = append(out.Layers, *ruleLayer)
		}
	}

	// The resolution ceiling: the profile's own unless the client's (after
	// rules) is lower, matching the clamp in decideProfile.
	if out.MaxHeight > 0 {
		switch {
		case configured > 0 && configured == out.MaxHeight:
			out.Layers[0].Decisive = true
		case ruleLayer != nil && !ruleLayer.Ignored:
			out.Layers[len(out.Layers)-1].Decisive = true
		default:
			out.Layers[1].Decisive = true
		}
	}

	// Bitrate, as the play request applies it: the remote cap for a remote
	// client, then the viewer's cap when it is lower.
	if declared.Remote {
		remote := QualityLayer{Kind: LayerRemoteCap, BitrateKbps: in.RemoteMaxBitrateKbps}
		if in.RemoteMaxBitrateKbps > 0 {
			out.BitrateKbps = in.RemoteMaxBitrateKbps
			remote.Reason = fmt.Sprintf("Away from home is limited to %s.", bitrateName(in.RemoteMaxBitrateKbps))
		} else {
			remote.Reason = "Away from home has no bitrate limit."
		}
		out.Layers = append(out.Layers, remote)
	}
	if in.ViewerMaxBitrateKbps > 0 {
		viewer := QualityLayer{Kind: LayerViewerCap, BitrateKbps: in.ViewerMaxBitrateKbps, Reason: fmt.Sprintf("This viewer is limited to %s.", bitrateName(in.ViewerMaxBitrateKbps))}
		if out.BitrateKbps == 0 || in.ViewerMaxBitrateKbps < out.BitrateKbps {
			out.BitrateKbps = in.ViewerMaxBitrateKbps
		}
		out.Layers = append(out.Layers, viewer)
	}
	if out.BitrateKbps > 0 {
		for i := range out.Layers {
			l := &out.Layers[i]
			if (l.Kind == LayerRemoteCap || l.Kind == LayerViewerCap) && l.BitrateKbps == out.BitrateKbps {
				l.Decisive = true
				break
			}
		}
	}

	if declared.Remote && in.RemoteDisabled {
		out.Blocked = true
		out.Layers = append(out.Layers, QualityLayer{Kind: LayerRemoteBlocked, Reason: "This viewer can't stream away from home.", Decisive: true})
	}
	return out
}

func profileLayerReason(l QualityLayer) string {
	switch l.Source {
	case ProfileFromRule:
		if l.BeatsRemote {
			return fmt.Sprintf("Rule %d picks the %s profile, ahead of the Away profile.", l.Rule, l.Profile)
		}
		return fmt.Sprintf("Rule %d picks the %s profile.", l.Rule, l.Profile)
	case ProfileFromRemote:
		return fmt.Sprintf("Away from home uses the %s profile.", l.Profile)
	case ProfileFromDefault:
		return fmt.Sprintf("No rule matches, so the %s profile applies.", l.Profile)
	case ProfileFromFirst:
		return fmt.Sprintf("No default is set, so the first profile, %s, applies.", l.Profile)
	}
	return "No quality profile is set, so the built-in one applies."
}

func deviceLayerReason(l QualityLayer) string {
	what := "This app plays"
	if l.Edited {
		what = "Your App compatibility edit lets this app play"
	}
	height := "any resolution"
	if l.MaxHeight > 0 {
		height = "up to " + heightName(l.MaxHeight)
	}
	switch {
	case *l.HDR && *l.DolbyVision:
		return fmt.Sprintf("%s %s, with HDR and Dolby Vision.", what, height)
	case *l.HDR:
		return fmt.Sprintf("%s %s, with HDR but not Dolby Vision.", what, height)
	}
	return fmt.Sprintf("%s %s, without HDR.", what, height)
}

func heightName(h int) string {
	if h == 2160 {
		return "4K"
	}
	return fmt.Sprintf("%dp", h)
}

func bitrateName(kbps int) string {
	if kbps%1000 == 0 {
		return fmt.Sprintf("%d Mbps", kbps/1000)
	}
	return fmt.Sprintf("%.1f Mbps", float64(kbps)/1000)
}

func boolp(b bool) *bool { return &b }
