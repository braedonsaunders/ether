package playback

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestFutureTitleNeverReachesResolver(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("r1", 2<<30, 90*time.Minute)}, []string{"r1"})
	h.catalog.titles[movieKey].Released = h.clock.Now().Add(24 * time.Hour)
	_, err := h.m.Prepare(context.Background(), owner, movieKey, defaultCaps("future"))
	if !errors.Is(err, core.ErrUnreleased) {
		t.Fatalf("prepare: %v", err)
	}
	h.resolver.mu.Lock()
	calls := h.resolver.calls
	h.resolver.mu.Unlock()
	if calls != 0 {
		t.Fatalf("resolver called %d times", calls)
	}
	if h.m.PreResolve(context.Background(), owner, movieKey, defaultCaps("future-warm")) {
		h.m.mu.Lock()
		e := h.m.warm[identity{userID: owner.ID, clientID: "future-warm", key: movieKey}]
		h.m.mu.Unlock()
		if e != nil {
			<-e.done
		}
	}
	h.resolver.mu.Lock()
	calls = h.resolver.calls
	h.resolver.mu.Unlock()
	if calls != 0 {
		t.Fatalf("warm reached resolver %d times", calls)
	}
}
