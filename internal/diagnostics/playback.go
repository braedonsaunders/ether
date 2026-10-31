package diagnostics

// boundPlayback clips and copies a playback decision so the ring owns it.
func boundPlayback(p *PlaybackDecision, uses []secretUse) *PlaybackDecision {
	if p == nil {
		return nil
	}
	out := *p
	out.Profile = clipFree(out.Profile, 80, uses)
	out.Chooser = clipFree(out.Chooser, 16, uses)
	out.ChosenRelease = clipFree(out.ChosenRelease, maxString, uses)
	if len(out.Candidates) > maxPlaybackCandidates {
		out.Candidates = append([]PlaybackCandidate(nil), out.Candidates[:maxPlaybackCandidates]...)
	} else if len(out.Candidates) > 0 {
		out.Candidates = append([]PlaybackCandidate(nil), out.Candidates...)
	}
	for i := range out.Candidates {
		c := &out.Candidates[i]
		c.Name = clipFree(c.Name, maxString, uses)
		c.Reason = clipFree(c.Reason, maxString, uses)
	}
	if len(out.Variants) > maxPlaybackCandidates {
		out.Variants = append([]PlaybackVariant(nil), out.Variants[:maxPlaybackCandidates]...)
	} else if len(out.Variants) > 0 {
		out.Variants = append([]PlaybackVariant(nil), out.Variants...)
	}
	for i := range out.Variants {
		v := &out.Variants[i]
		v.Release = clipFree(v.Release, maxString, uses)
	}
	return &out
}

func clonePlayback(p *PlaybackDecision) *PlaybackDecision {
	if p == nil {
		return nil
	}
	out := *p
	if len(out.Candidates) > 0 {
		out.Candidates = append([]PlaybackCandidate(nil), out.Candidates...)
	}
	if len(out.Variants) > 0 {
		out.Variants = append([]PlaybackVariant(nil), out.Variants...)
	}
	return &out
}

// exportPlayback redacts release names and reasons and always keeps them.
// The titles flag does not apply: a support ZIP has to explain the pick.
func exportPlayback(p *PlaybackDecision, uses []secretUse) *PlaybackDecision {
	if p == nil {
		return nil
	}
	out := *p
	out.Profile = redactString(out.Profile, uses)
	out.Chooser = redactString(out.Chooser, uses)
	out.ChosenRelease = redactString(out.ChosenRelease, uses)
	if len(out.Candidates) > 0 {
		out.Candidates = append([]PlaybackCandidate(nil), out.Candidates...)
		for i := range out.Candidates {
			c := &out.Candidates[i]
			c.Name = redactString(c.Name, uses)
			c.Reason = redactString(c.Reason, uses)
		}
	}
	if len(out.Variants) > 0 {
		out.Variants = append([]PlaybackVariant(nil), out.Variants...)
		for i := range out.Variants {
			v := &out.Variants[i]
			v.Release = redactString(v.Release, uses)
		}
	}
	return &out
}
