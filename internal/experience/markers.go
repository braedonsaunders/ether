package experience

import (
	"context"
	"sort"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/markers"
)

// MarkerSource finds locally cached ranges in an already-probed release.
type MarkerSource interface {
	CachedMarkers(context.Context, core.RatingKey, core.MediaInfo) ([]markers.Marker, error)
}

// MarkerMedia selects an existing source only for the authenticated viewer,
// device and title. A missing device never selects a session.
func MarkerMedia(viewer *core.User, device core.Device, key core.RatingKey, estimate core.MediaInfo, lookup func(int64, string, core.RatingKey) (*core.PlaySession, bool)) core.MediaInfo {
	if viewer == nil || viewer.ID <= 0 || device.ID == "" || lookup == nil {
		return estimate
	}
	s, ok := lookup(viewer.ID, device.ID, key)
	if !ok || s == nil || s.UserID != viewer.ID || s.ClientID != device.ID || s.Key != key {
		return estimate
	}
	return s.Info
}

// ItemMarkers converts validated marker ranges from known media into the neutral
// detail representation. Estimates do not identify a release timeline.
func ItemMarkers(ctx context.Context, source MarkerSource, key core.RatingKey, info core.MediaInfo) []frontend.Marker {
	if info.Estimated || info.Duration <= 0 {
		return nil
	}
	ranges := markers.FromChapters(info)
	if source != nil {
		found, err := source.CachedMarkers(ctx, key, info)
		if err == nil || len(found) > 0 {
			ranges = found
		}
	}
	sort.SliceStable(ranges, func(i, j int) bool { return ranges[i].StartMS < ranges[j].StartMS })
	var out []frontend.Marker
	for _, r := range ranges {
		if (r.Type != "intro" && r.Type != "credits" && r.Type != "recap") || r.StartMS < 0 || r.EndMS <= r.StartMS || r.EndMS > info.Duration.Milliseconds() {
			continue
		}
		out = append(out, frontend.Marker{Type: r.Type, Start: time.Duration(r.StartMS) * time.Millisecond, End: time.Duration(r.EndMS) * time.Millisecond})
	}
	return out
}
