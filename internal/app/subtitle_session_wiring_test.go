package app

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

type surfaceSessionFinder struct {
	plain   *core.PlaySession
	surface map[string]*core.PlaySession
}

func (f surfaceSessionFinder) SessionFor(int64, string, core.RatingKey) (*core.PlaySession, bool) {
	if f.plain == nil {
		return nil, false
	}
	return f.plain, true
}

func (f surfaceSessionFinder) SessionForSurfaceClient(surface string, _ int64, _ string, _ core.RatingKey) (*core.PlaySession, bool) {
	if sess := f.surface[surface]; sess != nil {
		return sess, true
	}
	return nil, false
}

func TestLookupPlaySessionUsesSurfaceVariant(t *testing.T) {
	jf := &core.PlaySession{ID: "sess-jf"}
	f := surfaceSessionFinder{surface: map[string]*core.PlaySession{"jellyfin": jf}}

	if _, ok := f.SessionFor(1, "dev", 42); ok {
		t.Fatal("SessionFor should miss the broker identity")
	}
	got := lookupPlaySession(f, "jellyfin", 1, "dev", 42)
	if got == nil || got.ID != "sess-jf" {
		t.Fatalf("jellyfin lookup = %+v", got)
	}
	if got := lookupPlaySession(f, "emby", 1, "dev", 42); got != nil {
		t.Fatalf("emby lookup = %+v", got)
	}
	if got := lookupPlaySession(f, "", 1, "dev", 42); got != nil {
		t.Fatalf("plex lookup used SessionFor miss = %+v", got)
	}

	plex := &core.PlaySession{ID: "sess-plex"}
	got = lookupPlaySession(surfaceSessionFinder{plain: plex}, "plex", 1, "dev", 42)
	if got == nil || got.ID != "sess-plex" {
		t.Fatalf("plex surface = %+v", got)
	}
}
