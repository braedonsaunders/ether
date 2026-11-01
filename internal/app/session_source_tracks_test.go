package app

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/sessions"
)

type appSourceTrackMedia struct{ *bytes.Reader }

func (*appSourceTrackMedia) Size() int64       { return 128 }
func (*appSourceTrackMedia) Name() string      { return "source.mkv" }
func (*appSourceTrackMedia) ReleaseID() string { return "source" }
func (*appSourceTrackMedia) Close() error      { return nil }

type appSourceTrackPlayback struct{ *activityPlayback }

func (p *appSourceTrackPlayback) PrepareSurface(ctx context.Context, viewer *core.User, key core.RatingKey, caps core.ClientCaps, release string, exact bool) (*core.PlaySession, error) {
	if release != "" || exact {
		return nil, core.ErrNotAvailable
	}
	ps, err := p.activityPlayback.Prepare(ctx, viewer, key, caps)
	if err == nil {
		ps.Source = &appSourceTrackMedia{Reader: bytes.NewReader(nil)}
		ps.Info = core.MediaInfo{Container: "mkv", Streams: []core.StreamInfo{{Type: "video", Index: 0}, {Type: "audio", Index: 1}, {Type: "subtitle", Index: 2}, {Type: "audio", Index: 3}}}
	}
	return ps, err
}
func (p *appSourceTrackPlayback) PinSourceRevision(_ context.Context, id, revision string) error {
	ps, ok := p.Session(id)
	if !ok || core.SourceRevision(ps) != revision {
		return core.ErrSourceChanged
	}
	return nil
}
func (*appSourceTrackPlayback) UnpinSourceRevision(string) {}

func TestSourceTrackAdmissionThroughAppPreferences(t *testing.T) {
	ctx := context.Background()
	pb := &appSourceTrackPlayback{activityPlayback: &activityPlayback{entries: map[string]*core.PlaySession{}}}
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	events := sessions.NewEventBus(nil, active)
	defer events.Close()
	prefs := experience.TrackPreferences{Source: fakeSessionTrackPrefs{}}
	var selected core.DecideOptions
	var selectedAudio, selectedSubtitle bool
	b, err := sessions.NewBroker(sessions.BrokerDeps{Playback: pb, Transcoder: activityTranscoder{}, Progress: activityProgress{}, Events: events, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: key}, nil
	}, SelectScopedTracks: func(ctx context.Context, viewer *core.Viewer, _ core.Device, title *core.Title, info core.MediaInfo, opts core.DecideOptions, audioSet, subtitleSet bool) (core.DecideOptions, error) {
		selectedAudio, selectedSubtitle = audioSet, subtitleSet
		chosen, selectErr := selectBrokerTracks(ctx, prefs, viewer, title, info, opts, audioSet, subtitleSet, nil)
		selected = chosen
		return selected, selectErr
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	viewer := &core.Viewer{ID: 1}
	audio, subtitle := 1, 2
	request := frontend.PlanRequest{Viewer: viewer, Device: core.Device{ID: "explicit"}, Protocol: "silo", Item: core.MovieKey(1), Intent: frontend.PlayIntent{AudioOrdinal: &audio, SubtitleStreamIndex: &subtitle, Options: core.DecideOptions{DirectPlay: true, AudioStream: -1, SubtitleStream: -1}}}
	if _, err := b.Plan(ctx, request); err != nil || !selectedAudio || !selectedSubtitle || selected.AudioStream != 3 || selected.SubtitleStream != 2 {
		t.Fatalf("explicit audio=%v subtitle=%v opts=%+v err=%v", selectedAudio, selectedSubtitle, selected, err)
	}
	off := -1
	request.Device.ID = "off"
	request.Intent.AudioOrdinal = nil
	request.Intent.SubtitleStreamIndex = &off
	if _, err := b.Plan(ctx, request); err != nil || selectedAudio || !selectedSubtitle || selected.SubtitleStream != -1 {
		t.Fatalf("subtitle off audio=%v subtitle=%v opts=%+v err=%v", selectedAudio, selectedSubtitle, selected, err)
	}
	request.Device.ID = "preferences"
	request.Intent.SubtitleStreamIndex = nil
	if _, err := b.Plan(ctx, request); err != nil || selectedAudio || selectedSubtitle || selected.AudioStream != 1 || selected.SubtitleStream != 2 {
		t.Fatalf("saved preferences audio=%v subtitle=%v opts=%+v err=%v", selectedAudio, selectedSubtitle, selected, err)
	}
}
