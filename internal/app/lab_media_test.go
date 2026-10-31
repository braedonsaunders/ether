//go:build plexparity && !scorecard

package app

import (
	"os"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestLabMediaCacheIgnoresUnversionedProbe(t *testing.T) {
	dir := t.TempDir()
	stale := []byte(`[{"tmdb":1,"season":0,"episode":0,"path":"/media/old.mp4","sha256":"` + strings.Repeat("a", 64) + `","media":{"container":"mp4","size":9,"duration":1,"bitrateKbps":1,"streams":[{"index":0,"type":"video","codec":"h264"}]}}]`)
	if err := os.WriteFile(labMediaPath(dir), stale, 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := readLabMedia(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("stale cache = %+v, %v", files, err)
	}
	want := []labMediaFile{{
		TMDB: 1, Path: "/media/new.mp4", SHA256: strings.Repeat("b", 64),
		Media: &core.MediaInfo{Container: "mp4", Size: 2},
	}}
	if err = writeLabMedia(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := readLabMedia(dir)
	if err != nil || len(got) != 1 || got[0].Path != want[0].Path || got[0].Media == nil || got[0].Media.Size != 2 {
		t.Fatalf("versioned cache = %+v, %v", got, err)
	}
}
