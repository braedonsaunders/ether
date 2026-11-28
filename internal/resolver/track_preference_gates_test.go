package resolver

import "testing"

func TestTrackPreferenceDoesNotOverrideQualityOrBandwidthGates(t *testing.T) {
	good := candidate{}                                   // fits profile, fits bandwidth
	bw := candidate{trackPreference: 1, bandwidth: -1}    // exceeds bandwidth cap, has wanted track
	fb := candidate{trackPreference: 1, fallback: true}   // below profile quality, has wanted track
	bs := candidate{trackPreference: 1, belowScore: true} // below min score, has wanted track
	loc := candidate{localFirst: true}                    // local file beats remote even with trackPreference
	remote := candidate{trackPreference: 1}

	for name, c := range map[string]candidate{
		"bandwidth-1": bw,
		"fallback":    fb,
		"belowScore":  bs,
	} {
		if compareCandidates(c, good) {
			t.Errorf("%s candidate with trackPreference ranked before clean candidate", name)
		}
		cs := []candidate{good, c}
		prioritizeOwnerTracks(cs)
		if cs[0].trackPreference == 1 {
			t.Errorf("%s: prioritizeOwnerTracks moved gated candidate ahead of clean candidate", name)
		}
	}

	if compareCandidates(remote, loc) {
		t.Error("remote candidate with trackPreference ranked before localFirst candidate")
	}
	cs := []candidate{loc, remote}
	prioritizeOwnerTracks(cs)
	if !cs[0].localFirst {
		t.Error("prioritizeOwnerTracks moved remote candidate ahead of localFirst")
	}

	// Within the same gate tier, trackPreference still breaks ties.
	withTrack := candidate{trackPreference: 1}
	if !compareCandidates(withTrack, good) {
		t.Error("same-tier candidate with trackPreference should rank ahead")
	}
}
