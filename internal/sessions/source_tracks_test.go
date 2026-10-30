package sessions

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func intPtr(n int) *int { return &n }

func TestSourceTrackSelectionUsesMixedNoncontiguousIndices(t *testing.T) {
	info := core.MediaInfo{Streams: []core.StreamInfo{
		{Type: "video", Index: 0}, {Type: "audio", Index: 2},
		{Type: "subtitle", Index: 7}, {Type: "audio", Index: 11},
		{Type: "subtitle", Index: 19},
	}}
	intent := frontend.PlayIntent{AudioOrdinal: intPtr(1), SubtitleStreamIndex: intPtr(19)}
	if err := bindSourceTracks(&intent, info); err != nil || !intent.AudioSet || !intent.SubtitleSet || intent.Options.AudioStream != 11 || intent.Options.SubtitleStream != 19 {
		t.Fatalf("selection %+v: %v", intent, err)
	}
	off := frontend.PlayIntent{SubtitleStreamIndex: intPtr(-1)}
	if err := bindSourceTracks(&off, info); err != nil || !off.SubtitleSet || off.Options.SubtitleStream != -1 || off.AudioSet {
		t.Fatalf("subtitle off %+v: %v", off, err)
	}
	unchanged := frontend.PlayIntent{Options: core.DecideOptions{AudioStream: 2, SubtitleStream: 7}}
	if err := bindSourceTracks(&unchanged, info); err != nil || unchanged.AudioSet || unchanged.SubtitleSet || unchanged.Options.AudioStream != 2 || unchanged.Options.SubtitleStream != 7 {
		t.Fatalf("omitted preferences %+v: %v", unchanged, err)
	}
	for _, bad := range []frontend.PlayIntent{
		{AudioOrdinal: intPtr(2)}, {AudioOrdinal: intPtr(-1)},
		{SubtitleStreamIndex: intPtr(1)}, {SubtitleStreamIndex: intPtr(-2)},
		{AudioSet: true, AudioOrdinal: intPtr(0)},
		{SubtitleSet: true, SubtitleStreamIndex: intPtr(-1)},
	} {
		if err := bindSourceTracks(&bad, info); !errors.Is(err, frontend.ErrTrackNotFound) {
			t.Fatalf("invalid selection %+v: %v", bad, err)
		}
	}
	duplicate := info
	duplicate.Streams = append(append([]core.StreamInfo(nil), info.Streams...), core.StreamInfo{Type: "subtitle", Index: 11})
	bad := frontend.PlayIntent{AudioOrdinal: intPtr(1)}
	if err := bindSourceTracks(&bad, duplicate); !errors.Is(err, frontend.ErrTrackNotFound) {
		t.Fatalf("ambiguous source index: %v", err)
	}
	sidecar := info
	sidecar.Streams = append([]core.StreamInfo(nil), info.Streams...)
	sidecar.Streams[2].ExternalURL = "https://example.invalid/subtitle"
	bad = frontend.PlayIntent{SubtitleStreamIndex: intPtr(7)}
	if err := bindSourceTracks(&bad, sidecar); !errors.Is(err, frontend.ErrTrackNotFound) {
		t.Fatalf("unverified sidecar selected: %v", err)
	}
	sidecar.Streams[2].ExternalURL = ""
	sidecar.Streams[2].External = true
	if err := bindSourceTracks(&bad, sidecar); !errors.Is(err, frontend.ErrTrackNotFound) {
		t.Fatalf("external stream without URL selected: %v", err)
	}
	info.Estimated = true
	if err := bindSourceTracks(&frontend.PlayIntent{AudioOrdinal: intPtr(0)}, info); !errors.Is(err, frontend.ErrTrackSourceUnavailable) {
		t.Fatalf("estimated source: %v", err)
	}
}

type sourceTrackPlayback struct {
	*fakePlayback
	info   core.MediaInfo
	source *revisionSource
	pins   int
}

func (p *sourceTrackPlayback) PrepareSurface(ctx context.Context, v *core.Viewer, key core.RatingKey, caps core.ClientCaps, release string, exact bool) (*core.PlaySession, error) {
	ps, err := p.fakePlayback.PrepareSurface(ctx, v, key, caps, release, exact)
	if err == nil {
		ps.Info = p.info
		ps.Source = p.source
	}
	return ps, err
}
func (p *sourceTrackPlayback) PinSourceRevision(_ context.Context, id, revision string) error {
	ps, ok := p.Session(id)
	if !ok || sourceRevision(ps) != revision {
		return core.ErrSourceChanged
	}
	p.pins++
	return nil
}
func (p *sourceTrackPlayback) UnpinSourceRevision(string) { p.pins-- }

func TestSourceTracksPinOpenedGenerationUntilStop(t *testing.T) {
	ctx := context.Background()
	pb := &sourceTrackPlayback{fakePlayback: &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}, source: &revisionSource{Reader: bytes.NewReader(nil), id: "release", name: "video.mkv", size: 100}, info: core.MediaInfo{Streams: []core.StreamInfo{{Type: "video", Index: 0}, {Type: "audio", Index: 2}, {Type: "subtitle", Index: 7}, {Type: "audio", Index: 11}, {Type: "subtitle", Index: 19}}}}
	active := func(_ context.Context, id int64) (*core.Viewer, error) { return &core.Viewer{ID: id}, nil }
	bus := NewEventBus(nil, active)
	defer bus.Close()
	var selected core.DecideOptions
	b, err := NewBroker(BrokerDeps{Events: bus, Playback: pb, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: key}, nil
	}, SelectScopedTracks: func(_ context.Context, _ *core.Viewer, _ core.Device, _ *core.Title, _ core.MediaInfo, opts core.DecideOptions, audioSet, subtitleSet bool) (core.DecideOptions, error) {
		if !audioSet || !subtitleSet {
			t.Error("explicit selections were lost")
		}
		selected = opts
		return opts, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	viewer, device := &core.Viewer{ID: 1}, core.Device{ID: "tv"}
	request := frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "silo", Item: core.MovieKey(1), Intent: frontend.PlayIntent{AudioOrdinal: intPtr(1), SubtitleStreamIndex: intPtr(19), Options: core.DecideOptions{DirectPlay: true}}}
	plan, err := b.Plan(ctx, request)
	if err != nil || selected.AudioStream != 11 || selected.SubtitleStream != 19 || pb.pins != 1 {
		t.Fatalf("plan %+v options %+v pins %d error %v", plan, selected, pb.pins, err)
	}
	session, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "silo", PlanID: plan.ID})
	if err != nil || pb.pins != 1 {
		t.Fatalf("open %+v pins %d error %v", session, pb.pins, err)
	}
	if err := b.Stop(ctx, frontend.SessionAccess{Viewer: viewer, Device: device, Protocol: "silo"}, session.ID); err != nil || pb.pins != 0 {
		t.Fatalf("stop pins %d error %v", pb.pins, err)
	}
	plan, err = b.Plan(ctx, request)
	if err != nil || pb.pins != 1 {
		t.Fatalf("second plan pins %d error %v", pb.pins, err)
	}
	// A misbehaving backend that changes the pinned source must still not let
	// Open install a decision made for a different generation.
	pb.source.id = "different-release"
	if _, err := b.Open(ctx, frontend.OpenRequest{Viewer: viewer, Device: device, Protocol: "silo", PlanID: plan.ID}); !errors.Is(err, frontend.ErrSourceGone) || pb.pins != 0 {
		t.Fatalf("changed source opened: pins %d error %v", pb.pins, err)
	}
}
