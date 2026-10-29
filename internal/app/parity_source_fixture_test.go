//go:build plexparity

package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestParitySourceWithRealResolver(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "parity.mp4"), []byte("fixture media"), 0600); err != nil {
		t.Fatal(err)
	}
	source := paritySource{dir: dir}
	svc := resolver.New(source, source, db, config.Defaults(dir), resolver.Options{})
	defer svc.Close()
	title := &core.Title{Key: core.MovieKey(10001), Type: core.TypeMovie, IDs: core.IDs{TMDB: 10001}, Title: "Fixture Movie 001", Year: 2020, Runtime: 90 * time.Minute, Released: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: core.ClientCaps{Product: "Plex Web", MaxHeight: 1080, VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac"}}})
	if err != nil {
		t.Fatal(err)
	}
	media, err := source.Open(ctx, res.Release.ID, res.NZB)
	if err != nil {
		t.Fatal(err)
	}
	defer media.Close()
	b := make([]byte, 7)
	if _, err = media.ReadAt(b, 0); err != nil || string(b) != "fixture" {
		t.Fatalf("media bytes %q: %v", b, err)
	}
}

func TestParitySourceProvidesHTTPRelease(t *testing.T) {
	releases, err := (paritySource{}).Search(context.Background(), core.SearchRequest{IDs: core.IDs{TMDB: 10002}, Title: "Fixture Movie 002", Year: 2020, Type: core.TypeMovie})
	if err != nil || len(releases) != 1 || releases[0].Protocol != "http" {
		t.Fatalf("HTTP fixture releases=%+v err=%v", releases, err)
	}
}

func TestParitySourceUsesDistinctMultitrackMedia(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "parity-tracks.mp4"), []byte("multitrack"), 0600); err != nil {
		t.Fatal(err)
	}
	media, err := (paritySource{dir: dir}).Open(context.Background(), "fixture-10004-0-0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer media.Close()
	buf := make([]byte, 10)
	if _, err := media.ReadAt(buf, 0); err != nil || string(buf) != "multitrack" {
		t.Fatalf("media=%q error=%v", buf, err)
	}
}
