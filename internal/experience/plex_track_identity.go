package experience

import "github.com/braedonsaunders/ether/internal/core"

// UnsupportedPlexTrackIdentityError means the issued selection is not a shape
// plex resolvePlaySelection produces. Callers must handle it themselves.
// The returned TrackSelection is empty and must not be applied.
// Every value of this type matches errors.Is, whatever Reason is set.
type UnsupportedPlexTrackIdentityError struct {
	Reason string
}

func (e UnsupportedPlexTrackIdentityError) Error() string {
	if e.Reason == "" {
		return "experience: unsupported plex track identity"
	}
	return "experience: unsupported plex track identity: " + e.Reason
}

func (UnsupportedPlexTrackIdentityError) Is(target error) bool {
	switch target.(type) {
	case UnsupportedPlexTrackIdentityError, *UnsupportedPlexTrackIdentityError:
		return true
	default:
		return false
	}
}

// PlexIssuedIdentity is the selection resolvePlaySelection already holds
// after the Plex adapter has accepted a stream ID from the item-scoped
// issued registry. Positions are indexes into the full MediaInfo.Streams
// slice from that issuance. They are not StreamInfo.Index values and they
// are not audio-only ordinals.
//
// TranslatePlexTrackIdentity does not look up stream IDs, does not see the
// item, and does not authorize a caller-built StreamInfo. Copy Audio and
// Subtitle only from a registry reference whose item is the one being played.
//
// AudioSet must equal AudioPosition >= 0, which is how the play resolver
// derives audio set-ness. Pass AudioPosition -1 and AudioSet false when the
// client omitted audio. The zero value is not an omission.
//
// SubtitleSet false is omission. SubtitleSet true with a nil subtitle and
// position -1 is explicit subtitle-off. A nil subtitle with SubtitleSet true
// and any other position, including an out-of-range or wrong-type slot, is a
// legacy position-only choice and is preserved.
//
// The translator does not retain either pointer and does not pin a source.
type PlexIssuedIdentity struct {
	Audio            *core.StreamInfo
	AudioPosition    int
	AudioSet         bool
	Subtitle         *core.StreamInfo
	SubtitlePosition int
	SubtitleSet      bool
}

// TranslatePlexTrackIdentity maps an issued Plex selection onto the current
// probed MediaInfo the way plex resolvePlaySelection does, before viewer
// preferences and before applySelections.
//
// A nil audio identity leaves AudioPosition unchanged, including when it is
// negative, out of range, or not an audio stream. Preferences still see a
// nonnegative legacy position as set. A non-nil audio identity is passed to
// ResolveAudio only when that position is nonnegative. An unmatched result
// is position -1 and AudioSet false so preferences can run. The identity's
// Type is not rechecked.
//
// A nil subtitle identity leaves SubtitleSet and SubtitlePosition unchanged.
// SubtitleSet false becomes position -1. A non-nil subtitle identity is
// passed to ResolveSubtitle only when SubtitleSet is true. An unmatched
// result is position -1 and SubtitleSet false. Type is not rechecked.
//
// The result positions are indexes in probed.Streams. Encoder indexes still
// come from the selected StreamInfo.Index.
func TranslatePlexTrackIdentity(issued PlexIssuedIdentity, probed core.MediaInfo) (TrackSelection, error) {
	if issued.AudioSet != (issued.AudioPosition >= 0) {
		return TrackSelection{}, UnsupportedPlexTrackIdentityError{Reason: "audio set flag does not match a nonnegative position"}
	}
	out := TrackSelection{
		Audio:       issued.AudioPosition,
		AudioSet:    issued.AudioPosition >= 0,
		Subtitle:    issued.SubtitlePosition,
		SubtitleSet: issued.SubtitleSet,
	}
	if out.AudioSet && issued.Audio != nil {
		out.Audio = ResolveAudio(issued.Audio, issued.AudioPosition, probed)
		out.AudioSet = out.Audio >= 0
	}
	if !out.SubtitleSet {
		out.Subtitle = -1
		return out, nil
	}
	if issued.Subtitle == nil {
		return out, nil
	}
	if pos := ResolveSubtitle(issued.Subtitle, issued.SubtitlePosition, probed); pos >= 0 {
		out.Subtitle = pos
	} else {
		out.Subtitle, out.SubtitleSet = -1, false
	}
	return out, nil
}
