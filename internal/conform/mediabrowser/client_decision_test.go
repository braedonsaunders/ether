package mediabrowser

import "testing"

func TestRequestedDeliveryModelsAuditedClients(t *testing.T) {
	cases := []struct {
		profile string
		offer   PlaybackOffer
		want    string
		error   bool
	}{
		{"jellycon-default", PlaybackOffer{ID: "source", SupportsDirectPlay: true, SupportsTranscoding: true}, "direct", false},
		{"jellycon-default", PlaybackOffer{ID: "source", SupportsDirectStream: true}, "direct", false},
		{"jellycon-default", PlaybackOffer{ID: "source", SupportsTranscoding: true}, "transcode", false},
		{"jellycon-default", PlaybackOffer{SupportsDirectPlay: true}, "", true},
		{"jellycon-default", PlaybackOffer{ID: "source"}, "", true},
		{"streamyfin-android-exoplayer", PlaybackOffer{TranscodingURL: "/scoped/hls"}, "transcode", false},
		{"streamyfin-android-exoplayer", PlaybackOffer{}, "direct", false},
		{"unsupported", PlaybackOffer{}, "", true},
	}
	for _, tc := range cases {
		got, err := RequestedDelivery(tc.profile, tc.offer)
		if got != tc.want || (err != nil) != tc.error {
			t.Errorf("%s: got %q, %v", tc.profile, got, err)
		}
	}
}
