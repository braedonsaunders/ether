package experience

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/markers"
)

type fakeMarkerSource struct {
	calls  int
	key    core.RatingKey
	ranges []markers.Marker
	err    error
}

func (s *fakeMarkerSource) Markers(_ context.Context, key core.RatingKey, _ core.MediaInfo) ([]markers.Marker, error) {
	s.calls++
	s.key = key
	return append([]markers.Marker(nil), s.ranges...), s.err
}

func TestItemMarkersRequiresProbedMediaAndFiltersRanges(t *testing.T) {
	ctx := context.Background()
	key := core.EpisodeKey(4, 1, 2)
	source := &fakeMarkerSource{ranges: []markers.Marker{
		{Type: "credits", StartMS: 8000, EndMS: 9000},
		{Type: "intro", StartMS: 1000, EndMS: 2000},
		{Type: "recap", StartMS: 100, EndMS: 800},
		{Type: "bad", StartMS: 1, EndMS: 2},
		{Type: "intro", StartMS: -1, EndMS: 20},
		{Type: "credits", StartMS: 9000, EndMS: 11000},
	}}
	if got := ItemMarkers(ctx, source, key, core.MediaInfo{Estimated: true, Duration: 10 * time.Second}); len(got) != 0 || source.calls != 0 {
		t.Fatalf("estimate returned markers or called provider: %v, %d", got, source.calls)
	}
	got := ItemMarkers(ctx, source, key, core.MediaInfo{Duration: 10 * time.Second})
	if source.calls != 1 || source.key != key || len(got) != 3 || got[0].Type != "recap" || got[1].Type != "intro" || got[1].Start != time.Second || got[2].End != 9*time.Second {
		t.Fatalf("unexpected converted markers: %+v, calls=%d key=%v", got, source.calls, source.key)
	}
}

func TestItemMarkersFallsBackToChaptersOnProviderFailure(t *testing.T) {
	info := core.MediaInfo{Duration: 10 * time.Second, Chapters: []core.Chapter{{Title: "Intro", Start: time.Second, End: 3 * time.Second}}}
	got := ItemMarkers(context.Background(), &fakeMarkerSource{err: errors.New("offline")}, core.MovieKey(1), info)
	if len(got) != 1 || got[0].Type != "intro" || got[0].Start != time.Second || got[0].End != 3*time.Second {
		t.Fatalf("chapter fallback: %+v", got)
	}
}

func TestMarkerMediaUsesOnlySameViewerDeviceAndTitle(t *testing.T) {
	viewer := &core.User{ID: 7}
	device := core.Device{ID: "living-room"}
	key := core.MovieKey(12)
	estimate := core.MediaInfo{Estimated: true}
	session := &core.PlaySession{UserID: viewer.ID, ClientID: device.ID, Key: key, Info: core.MediaInfo{Duration: time.Hour}}
	calls := 0
	lookup := func(id int64, client string, item core.RatingKey) (*core.PlaySession, bool) {
		calls++
		if id != viewer.ID || client != device.ID || item != key {
			t.Fatal("incorrect lookup scope")
		}
		return session, true
	}
	if got := MarkerMedia(viewer, device, key, estimate, lookup); got.Estimated || got.Duration != time.Hour {
		t.Fatal(got)
	}
	session.UserID++
	if got := MarkerMedia(viewer, device, key, estimate, lookup); !got.Estimated {
		t.Fatal("accepted another viewer's session")
	}
	session.UserID = viewer.ID
	session.ClientID = "other"
	if got := MarkerMedia(viewer, device, key, estimate, lookup); !got.Estimated {
		t.Fatal("accepted another device's session")
	}
	session.ClientID = device.ID
	session.Key = core.MovieKey(13)
	if got := MarkerMedia(viewer, device, key, estimate, lookup); !got.Estimated {
		t.Fatal("accepted another title's session")
	}
	before := calls
	if got := MarkerMedia(viewer, core.Device{}, key, estimate, lookup); !got.Estimated || calls != before {
		t.Fatal("looked up unscoped device")
	}
}
