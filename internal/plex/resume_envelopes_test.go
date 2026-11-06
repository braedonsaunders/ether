package plex

import (
	"strings"
	"testing"
)

func TestResumeRouteEnvelopesAndOptionPropagation(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.state.set(ownerUser.ID, ws(bbE1.Key, 100000, 0))
	for _, path := range []string{"/hubs/sections/2/continueWatching/items", "/hubs/continueWatching/items?contentDirectoryID=2"} {
		mc := decodeJSON(t, f.get(t, path, jsonAccept...))
		if mc["allowSync"] != true {
			t.Fatalf("%s sync capability: %v", path, mc)
		}
		if strings.HasPrefix(path, "/hubs/sections/") {
			for _, key := range []string{"mediaTagPrefix", "mediaTagVersion", "totalSize", "offset"} {
				if _, ok := mc[key]; ok {
					t.Fatalf("section minimal envelope %s: %v", key, mc)
				}
			}
		}
	}
	for _, path := range []string{"/hubs/home/continueWatching?includeMeta=1", "/hubs/home/onDeck?includeMeta=1"} {
		mc := decodeJSON(t, f.get(t, path, jsonAccept...))
		if _, ok := mc["Meta"]; !ok {
			t.Fatalf("%s missing Meta", path)
		}
	}
	for _, path := range []string{"/hubs/continueWatching/items?contentDirectoryID=garbage", "/hubs/continueWatching/items?contentDirectoryID=0"} {
		if rec := f.get(t, path); rec.Code != 404 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
	}
	for _, path := range []string{"/hubs/metadata/1000000009999999/related", "/hubs/metadata/1000000009999999"} {
		if rec := f.get(t, path); rec.Code != 200 {
			t.Fatalf("missing legacy related %s: %d", path, rec.Code)
		}
	}
	for _, path := range []string{"/library/metadata/1000000009999999/related", "/hubs/metadata/1000000009999999/postplay"} {
		if rec := f.get(t, path); rec.Code != 404 {
			t.Fatalf("missing item %s: %d", path, rec.Code)
		}
	}
	if rec := f.get(t, "/hubs/metadata/"+bbE1.Key.String()+"/postplay?includePreferences=1"); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
}
