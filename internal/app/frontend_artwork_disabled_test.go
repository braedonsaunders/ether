package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	artworkservice "github.com/braedonsaunders/ether/internal/artwork/service"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/state"
)

type disabledArtworkTransport struct{}

func (disabledArtworkTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/jpeg"}}, Body: io.NopCloser(strings.NewReader("local-owner-artwork"))}, nil
}

func TestPlexDisabledLocalOwnerArtworkAndStateWithoutServer(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Plex.Token = ""
		c.Frontends = map[string]config.Frontend{"plex": {Enabled: false}}
		c.Transcode.FFmpegPath = "/not-installed/ffmpeg"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a, err := New(ctx, Options{DataDir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.plexServer != nil || a.plexClose != nil {
		t.Fatal("disabled Plex was constructed")
	}
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	id, err := local.CreateOwner(ctx, "local-owner", "offline-test-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	viewer, err := a.users.ActiveUser(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	title := core.Title{Key: core.MovieKey(999), Type: core.TypeMovie, IDs: core.IDs{TMDB: 999}, Title: "Local artwork", PosterPath: "/test.jpg", Released: time.Unix(1, 0)}
	if err := a.catalog.UpsertTitle(ctx, title, true); err != nil {
		t.Fatal(err)
	}
	source := *a.artwork
	source.Variants = nil
	source.Metadata = nil
	source.Palettes = nil
	source.Images = artworkservice.NewRemoteCache(1024, &http.Client{Transport: disabledArtworkTransport{}})
	req := frontend.ArtworkRequest{Viewer: viewer, Item: title.Key}
	images, complete, err := source.List(ctx, req)
	if err != nil || !complete || len(images) == 0 {
		t.Fatalf("local owner artwork: %+v %v %v", images, complete, err)
	}
	data, _, err := source.Read(ctx, req, images[0], frontend.ArtworkTransform{})
	if err != nil || string(data) != "local-owner-artwork" {
		t.Fatalf("read: %q %v", data, err)
	}
	eventCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := a.sessionEvents.Subscribe(eventCtx, frontend.EventScope{Viewer: viewer, Protocol: "silo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.state.Scrobble(state.WithOrigin(ctx, state.Origin{Surface: "jellyfin"}), id, title.Key); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"UserDataChanged", "WatchStateChanged"} {
		select {
		case ev := <-events:
			if ev.Type != want {
				t.Fatalf("event %s want %s", ev.Type, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("missing %s without Plex", want)
		}
	}
	if a.plexServer != nil {
		t.Fatal("artwork or state constructed Plex")
	}
	if err := a.frontends.control.Toggle(ctx, "plex", true); err != nil {
		t.Fatal(err)
	}
	if a.plexServer == nil {
		t.Fatal("enable did not construct adapter")
	}
	if err := a.frontends.control.Toggle(ctx, "plex", false); err != nil {
		t.Fatal(err)
	}
}
