package frontend

import "testing"

func TestDefaultPorts(t *testing.T) {
	for id, want := range map[Protocol]int{"plex": 32400, "jellyfin": 8096, "emby": 8097, "silo": 8090} {
		if got, ok := DefaultPort(id); !ok || got != want {
			t.Fatalf("%s: %d %t", id, got, ok)
		}
	}
	if _, ok := DefaultPort("files"); ok {
		t.Fatal("files has no dedicated default")
	}
}
