package playback

import (
	"testing"
	"time"
)

func TestSessionForSurfaceClientFindsVariant(t *testing.T) {
	now := time.Unix(1, 0)
	m := &Manager{
		byIdent: map[identity]*session{},
		now:     func() time.Time { return now },
	}
	ident := identity{userID: 7, clientID: "jf-dev", key: 42, surface: "jellyfin", variant: "rel-a", exact: true}
	m.byIdent[ident] = &session{id: "sess-jf", ident: ident}

	if _, ok := m.SessionFor(7, "jf-dev", 42); ok {
		t.Fatal("SessionFor matched a surface+variant identity")
	}
	if _, ok := m.SessionForSurface("jellyfin", 7, "jf-dev", 42, "", false); ok {
		t.Fatal("SessionForSurface matched without the registered variant")
	}
	got, ok := m.SessionForSurfaceClient("jellyfin", 7, "jf-dev", 42)
	if !ok || got == nil || got.ID != "sess-jf" {
		t.Fatalf("SessionForSurfaceClient = %+v ok=%v", got, ok)
	}
	if _, ok := m.SessionForSurfaceClient("emby", 7, "jf-dev", 42); ok {
		t.Fatal("other surface")
	}
}
