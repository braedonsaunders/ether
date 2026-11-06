package plex

import (
	"net/http"
	"strings"
	"testing"
)

func TestMetadataTreeStreamSchemaAndAdmission(t *testing.T) {
	f, _ := newSubtitleFixture(t)
	path := "/library/metadata/" + fightClub.Key.String() + "/tree"
	rec := f.get(t, path, jsonAccept...)
	if rec.Code != 200 {
		t.Fatalf("tree %d %s", rec.Code, rec.Body.String())
	}
	mc := decodeJSON(t, rec)
	item := mc["MetadataItem"].([]any)[0].(map[string]any)
	media := item["MediaItem"].([]any)[0].(map[string]any)
	if len(media) != 2 {
		t.Fatalf("unmeasured MediaItem fields: %+v", media)
	}
	part := media["MediaPart"].([]any)[0].(map[string]any)
	for _, name := range []string{"container", "hash", "openSubtitleHash"} {
		if _, ok := part[name]; ok {
			t.Fatalf("invented %s", name)
		}
	}
	for _, raw := range part["MediaStream"].([]any) {
		st := raw.(map[string]any)
		if _, ok := st["type"].(float64); !ok {
			t.Fatalf("type scalar %+v", st)
		}
		if _, ok := st["streamType"]; ok {
			t.Fatal("normal stream schema leaked")
		}
		for name, v := range st {
			if name != "id" && name != "index" && name != "type" && name != "channels" {
				if _, ok := v.(string); !ok {
					t.Fatalf("%s numeric scalar %+v", name, v)
				}
			}
		}
	}
	for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
		rec := f.request(t, method, path, nil)
		if rec.Code != 404 {
			t.Fatalf("%s tree %d", method, rec.Code)
		}
	}
	if rec := f.request(t, http.MethodHead, path, nil); rec.Code != 200 || rec.Body.Len() != 0 {
		t.Fatalf("HEAD %d %q", rec.Code, rec.Body.String())
	}
	if rec := f.get(t, "/library/metadata/999999999/tree"); rec.Code != 404 {
		t.Fatalf("unknown %d", rec.Code)
	}
	if rec := f.request(t, http.MethodGet, path, map[string]string{"X-Plex-Token": "invalid"}); rec.Code != 401 {
		t.Fatalf("unauthorized %d", rec.Code)
	}
	if rec := f.get(t, path); rec.Code != 200 || !strings.Contains(rec.Body.String(), "MediaStream") {
		t.Fatalf("tree XML %d %s", rec.Code, rec.Body.String())
	}
}

func TestMetadataTreeEpisodeHasOnlyAncestorChain(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	rec := f.get(t, "/library/metadata/"+bbE1.Key.String()+"/tree", jsonAccept...)
	if rec.Code != 200 {
		t.Fatalf("episode tree %d %s", rec.Code, rec.Body.String())
	}
	mc := decodeJSON(t, rec)
	node := mc["MetadataItem"].([]any)
	for i, want := range []int64{int64(breakingBad.Key), int64(bbS1.Key), int64(bbE1.Key)} {
		if len(node) != 1 {
			t.Fatalf("level %d has siblings: %+v", i, node)
		}
		row := node[0].(map[string]any)
		if row["id"] != float64(want) {
			t.Fatalf("level %d: %+v", i, row)
		}
		if i < 2 {
			node = row["MetadataItem"].([]any)
		} else {
			part := row["MediaItem"].([]any)[0].(map[string]any)["MediaPart"].([]any)[0].(map[string]any)
			if part["metadataType"] != float64(4) {
				t.Fatalf("episode part %+v", part)
			}
		}
	}
}
