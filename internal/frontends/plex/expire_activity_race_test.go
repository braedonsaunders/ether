package plex

import (
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// hookedActivityStore is an in-memory PlaybackActivityStore that runs afterExpire
// once ExpireActivities has returned, standing in for a stop that lands between
// the janitor's decisions.
type hookedActivityStore struct {
	mu          sync.Mutex
	m           map[string]frontend.PlaybackActivity
	afterExpire func()
}

func (h *hookedActivityStore) HasActivity(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.m[id]
	return ok
}
func (h *hookedActivityStore) EnsureActivity(id string, key int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.m[id]; !ok && key > 0 {
		h.m[id] = frontend.PlaybackActivity{ID: id, SessionKey: key}
	}
}
func (h *hookedActivityStore) UpsertActivity(id string, fn func(*frontend.PlaybackActivity)) frontend.PlaybackActivity {
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.m[id]
	a.ID = id
	fn(&a)
	h.m[id] = a
	return a
}
func (h *hookedActivityStore) RemoveActivity(id string) (frontend.PlaybackActivity, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a, ok := h.m[id]
	delete(h.m, id)
	return a, ok
}
func (h *hookedActivityStore) ListActivities() []frontend.PlaybackActivity {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []frontend.PlaybackActivity
	for _, a := range h.m {
		out = append(out, a)
	}
	return out
}
func (h *hookedActivityStore) ExpireActivities(now time.Time, ttl time.Duration) []frontend.PlaybackActivity {
	h.mu.Lock()
	var out []frontend.PlaybackActivity
	for id, a := range h.m {
		if now.Sub(a.UpdatedAt) > ttl {
			out = append(out, a)
			delete(h.m, id)
		}
	}
	h.mu.Unlock()
	if h.afterExpire != nil {
		h.afterExpire()
	}
	return out
}

// A silent entry retained for active media must not be removed and then
// re-created: a stop landing in between found nothing to remove and the
// entry came back.
func TestRetainedActivityIsNotResurrectedByConcurrentStop(t *testing.T) {
	f, _ := issue94Fixture(t, true, "open")
	store := &hookedActivityStore{m: map[string]frontend.PlaybackActivity{}}
	f.srv.sessions.bind(store)
	h := []string{"X-Plex-Client-Identifier", "phone", "X-Plex-Session-Identifier", "play-race"}
	for _, path := range []string{"decision?", "start.m3u8?"} {
		if r := f.get(t, "/video/:/transcode/universal/"+path+issue94Query("trans-race", 3000), h...); r.Code != 200 {
			t.Fatalf("%s %d", path, r.Code)
		}
	}
	if r := f.get(t, "/:/timeline?ratingKey=1000000000000550&state=playing&time=1000", h...); r.Code != 200 {
		t.Fatal(r.Code)
	}
	entries := f.srv.sessions.list()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	id := entries[0].id
	encoder, ok := f.srv.plexHLSSession("trans-race")
	if !ok {
		t.Fatal("missing encoder")
	}
	fake := encoder.(*fakeHLS)
	now := fixedNow.Add(5 * time.Minute)
	fake.ServeSegment(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil), 1)
	fake.lastAccess = now
	store.afterExpire = func() { f.srv.sessions.remove(id) } // the stop
	if expired := f.srv.expirePlaybackActivities(now, 3*time.Minute); len(expired) != 0 {
		t.Fatalf("active media fetch expired the entry: %d", len(expired))
	}
	if f.srv.sessions.has(id) {
		t.Fatal("entry was resurrected after a stop removed it")
	}
}
