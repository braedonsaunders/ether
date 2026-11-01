package sessions

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type fakeProgress struct {
	calls              int
	viewer             int64
	key                core.RatingKey
	position, duration time.Duration
	state              string
}

func (f *fakeProgress) Progress(_ context.Context, viewer int64, key core.RatingKey, position, duration time.Duration, state string) error {
	f.calls++
	f.viewer = viewer
	f.key = key
	f.position = position
	f.duration = duration
	f.state = state
	return nil
}
func TestRecordProgress(t *testing.T) {
	f := &fakeProgress{}
	key := core.MovieKey(7)
	if err := RecordProgress(context.Background(), f, 2, key, time.Second, 10*time.Second, "paused"); err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 || f.viewer != 2 || f.key != key || f.position != time.Second || f.duration != 10*time.Second || f.state != "paused" {
		t.Fatalf("progress: %+v", f)
	}
	if err := RecordProgress(context.Background(), f, 2, core.ExtraKey(8), 0, 0, "playing"); err != nil || f.calls != 1 {
		t.Fatalf("extra wrote library progress: calls=%d err=%v", f.calls, err)
	}
}
