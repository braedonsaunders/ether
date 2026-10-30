package playback

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestGuardedSourceStopsBeforeLateObservation(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute)}, []string{"A"})
	ps := h.prepare(owner, movieKey, "history-stop")
	entered, release, observed := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		observed <- h.m.WithCurrentSource(context.Background(), ps.ID, func(ctx context.Context, snapshot core.PlaybackSourceObservation) error {
			if snapshot.Revision == "" || snapshot.ReleaseID != "A" {
				return errors.New("wrong source")
			}
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	<-entered
	stopped := make(chan struct{})
	go func() { h.m.Stop(ps.ID); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("stop retired generation during observation")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-observed; err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("stop did not finish")
	}
	if err := h.m.WithCurrentSource(context.Background(), ps.ID, func(context.Context, core.PlaybackSourceObservation) error {
		t.Fatal("retired source observed")
		return nil
	}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("retired session: %v", err)
	}
}

func TestGuardedSourceBlocksFailoverCommit(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute), mkv("B", 10000, time.Minute)}, []string{"A", "B"})
	ps := h.prepare(owner, movieKey, "history-failover")
	entered, release, observed := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		observed <- h.m.WithCurrentSource(context.Background(), ps.ID, func(ctx context.Context, _ core.PlaybackSourceObservation) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := h.m.Failover(ctx, ps.ID, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("failover did not honor guard: %v", err)
	}
	current, ok := h.m.Session(ps.ID)
	if !ok || current.Resolution.Release.ID != "A" {
		t.Fatal("source swapped during observation")
	}
	close(release)
	if err := <-observed; err != nil {
		t.Fatal(err)
	}
}
