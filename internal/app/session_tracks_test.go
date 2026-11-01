package app

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/trackprefs"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type fakeSessionTrackPrefs struct{}

func (fakeSessionTrackPrefs) Select(context.Context, int64, core.RatingKey, core.MediaInfo, string) (trackprefs.Selection, error) {
	return trackprefs.Selection{Audio: 1, Subtitle: 2}, nil
}

func TestViewerBitrateCannotRaiseRemoteOrClientCaps(t *testing.T) {
	media := core.MediaInfo{Container: "mp4", BitrateKbps: 12000, Streams: []core.StreamInfo{{Type: "video", Codec: "h264", Height: 1080}, {Type: "audio", Codec: "aac"}}}
	caps := core.ClientCaps{Remote: true, MaxBitrateKbps: 3000}
	opts := core.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: -1, SubtitleStream: -1, MaxBitrateKbps: trackprefs.LimitBitrate(0, 8000)}
	decision := transcode.Decide(media, caps, opts, transcode.Policy{RemoteMaxBitrateKbps: 2000})
	if decision.Kind == core.DecisionDirectPlay || decision.BitrateKbps <= 0 || decision.BitrateKbps > 2000 {
		t.Fatalf("caps bypassed: %+v", decision)
	}
}
func (fakeSessionTrackPrefs) GetRules(context.Context, int64) (trackprefs.Rules, error) {
	return trackprefs.Rules{}, nil
}

func TestBrokerTrackSelectionsUseStreamIndexes(t *testing.T) {
	info := core.MediaInfo{Streams: []core.StreamInfo{{Type: "audio", Index: 8}, {Type: "audio", Index: 12}, {Type: "subtitle", Index: 21}}}
	prefs := experience.TrackPreferences{Source: fakeSessionTrackPrefs{}}
	viewer, title := &core.Viewer{ID: 1}, &core.Title{Key: core.MovieKey(1)}
	opts, err := selectBrokerTracks(context.Background(), prefs, viewer, title, info, core.DecideOptions{AudioStream: -1, SubtitleStream: -1}, false, false, nil)
	if err != nil || opts.AudioStream != 12 || opts.SubtitleStream != 21 {
		t.Fatalf("preferences %+v %v", opts, err)
	}
	opts, err = selectBrokerTracks(context.Background(), prefs, viewer, title, info, core.DecideOptions{AudioStream: 8, SubtitleStream: -1}, true, true, nil)
	if err != nil || opts.AudioStream != 8 || opts.SubtitleStream != -1 {
		t.Fatalf("explicit %+v %v", opts, err)
	}
}
