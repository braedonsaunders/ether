package diagnostics

func cloneClient(in *ClientFacts) *ClientFacts {
	if in == nil {
		return nil
	}
	c := *in
	c.TranscodeVideoCodecs = append([]string(nil), in.TranscodeVideoCodecs...)
	c.TranscodeAudioCodecs = append([]string(nil), in.TranscodeAudioCodecs...)
	c.VideoCodecs = append([]string(nil), in.VideoCodecs...)
	c.AudioCodecs = append([]string(nil), in.AudioCodecs...)
	c.Containers = append([]string(nil), in.Containers...)
	c.Inputs = append([]ClientInput(nil), in.Inputs...)
	return &c
}

func boundClient(in *ClientFacts, uses []secretUse) *ClientFacts {
	if in == nil {
		return nil
	}
	// Bound before copying: even a malformed caller cannot retain unbounded lists.
	c := *in
	c.VideoCodecs = boundClientStrings(c.VideoCodecs, uses)
	c.AudioCodecs = boundClientStrings(c.AudioCodecs, uses)
	c.Containers = boundClientStrings(c.Containers, uses)
	c.TranscodeVideoCodecs = boundClientStrings(c.TranscodeVideoCodecs, uses)
	c.TranscodeAudioCodecs = boundClientStrings(c.TranscodeAudioCodecs, uses)
	c.OutputProtocol = clipNetwork(c.OutputProtocol, 32, uses)
	c.PlayerProfile = clipNetwork(c.PlayerProfile, 80, uses)
	c.Stage = clipNetwork(c.Stage, 32, uses)
	c.Registry = clipNetwork(c.Registry, 80, uses)
	if len(c.Inputs) > 24 {
		c.Inputs = c.Inputs[:24]
	}
	c.Inputs = append([]ClientInput(nil), c.Inputs...)
	for i := range c.Inputs {
		c.Inputs[i].Name = clipNetwork(c.Inputs[i].Name, 80, uses)
	}
	return &c
}

func boundClientStrings(in []string, uses []secretUse) []string {
	if len(in) > 24 {
		in = in[:24]
	}
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = clipNetwork(v, 40, uses)
	}
	return out
}

// viewerPlayback is deliberately narrower than arbitrary viewer traffic.
func viewerPlayback(e Event) bool {
	return PlaybackRequestPath(e.Route) || e.Kind == KindPlaybackDecision || e.Kind == KindClientCapabilities
}
