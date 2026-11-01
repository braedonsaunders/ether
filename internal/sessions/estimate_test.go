package sessions

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type fakeEstimator struct{ estimates int }

func (f *fakeEstimator) Estimate(_ context.Context, _ *core.Title, _ core.ClientCaps) core.MediaInfo {
	f.estimates++
	return core.MediaInfo{Estimated: true}
}
func (f *fakeEstimator) SessionFor(int64, string, core.RatingKey) (*core.PlaySession, bool) {
	return nil, false
}
func TestEstimateDoesNotResolve(t *testing.T) {
	f := &fakeEstimator{}
	title := &core.Title{Key: core.MovieKey(1), Runtime: time.Hour}
	media, session := Estimate(context.Background(), f, &core.Viewer{ID: 1}, "tv", title, func() core.ClientCaps { return core.ClientCaps{} })
	if !media.Estimated || session != nil || f.estimates != 1 {
		t.Fatalf("estimate %+v, session %v, calls %d", media, session, f.estimates)
	}
}
