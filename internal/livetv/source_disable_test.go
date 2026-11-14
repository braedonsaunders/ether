package livetv

import (
	"context"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/debrid"
	"github.com/braedonsaunders/ether/internal/newznab"
	"github.com/braedonsaunders/ether/internal/usenet"
)

func TestDisabledSourceManagers(t *testing.T) {
	for _, kind := range []string{"usenet", "debrid", "newznab", "torznab", "cardigann", "stremio", "zilean", "prowlarr", "m3u", "xmltv", "xtream", "addon"} {
		t.Run(kind, func(t *testing.T) {
			switch kind {
			case "usenet":
				f := usenet.New([]config.UsenetProvider{{ID: "off", Host: "invalid", Enabled: false}}, usenet.Options{})
				defer f.Close()
				if len(f.Stats()) != 0 {
					t.Fatal("disabled provider acquired a pool")
				}
			case "debrid":
				s := debrid.NewService(config.Debrid{Providers: []config.DebridProvider{{ID: "off", Enabled: false}}}, debrid.Options{NewProvider: func(config.DebridProvider, debrid.ProviderOptions) (debrid.Provider, error) {
					t.Fatal("constructed disabled provider")
					return nil, nil
				}})
				if s.Enabled() {
					t.Fatal("disabled debrid is available")
				}
			case "m3u", "xmltv", "xtream", "addon":
				s := testService(t, Options{})
				src := Source{ID: "off", Name: "Off", Disabled: true, PlaylistURL: "https://invalid/list"}
				b, err := s.encodeSource(src)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.o.DB.ExecContext(context.Background(), `INSERT INTO livetv_sources(id,data) VALUES(?,?)`, src.ID, b); err != nil {
					t.Fatal(err)
				}
				if s.channelSourceEnabled(context.Background(), src.ID) {
					t.Fatal("disabled source can tune")
				}
				if _, ok := refreshInterval(src); ok {
					t.Fatal("disabled source scheduled")
				}
			default:
				ix := config.Indexer{ID: "off", Kind: kind, URL: "https://invalid", Enabled: false}
				m := newznab.NewManager(nil, []config.Indexer{ix}, newznab.Options{})
				if kind == "prowlarr" {
					ix.Enabled = true
					ix.ProwlarrImport = &config.IndexerImport{}
					m.ApplyConfig(config.Config{Indexers: []config.Indexer{ix}, Prowlarr: config.Prowlarr{Disabled: true}})
				}
				if m.MissingSources() == nil {
					t.Fatal("disabled indexer available")
				}
			}
		})
	}
}

// Every frontend (Plex, Jellyfin, M3U) builds its lineup from AllowedChannels
// and tunes through AllowedChannel/TuneWithSession: a paused source's channels
// leave all of them, for the owner too, and come back when it is enabled.
func TestDisabledSourceChannelsLeaveLineup(t *testing.T) {
	ctx := context.Background()
	s := testService(t, Options{})
	for _, src := range []Source{
		{ID: "paused", Name: "Paused", Playlist: "#EXTM3U\n#EXTINF:-1 tvg-chno=\"1\",One\nhttps://example.invalid/one.m3u8\n"},
		{ID: "kept", Name: "Kept", Playlist: "#EXTM3U\n#EXTINF:-1 tvg-chno=\"2\",Two\nhttps://example.invalid/two.m3u8\n"},
	} {
		if _, err := s.ImportSource(ctx, src); err != nil {
			t.Fatal(err)
		}
	}
	owner := &core.User{ID: 1, IsOwner: true}
	sources := func() []string {
		t.Helper()
		cs, err := s.AllowedChannels(ctx, owner)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range cs {
			out = append(out, c.SourceID)
		}
		return out
	}
	all, err := s.Channels(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("channels: %v %v", all, err)
	}
	paused := all[0]
	if paused.SourceID != "paused" {
		paused = all[1]
	}
	if got := sources(); len(got) != 2 {
		t.Fatalf("enabled lineup: %v", got)
	}
	if _, err := s.ImportSource(ctx, Source{ID: "paused", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if got := sources(); len(got) != 1 || got[0] != "kept" {
		t.Fatalf("paused source still listed: %v", got)
	}
	if _, err := s.AllowedChannel(ctx, owner, paused.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("paused channel resolved for tune: %v", err)
	}
	if _, err := s.TuneWithSession(ctx, owner, paused.ID, core.ClientCaps{ClientID: "test"}, "test"); err == nil {
		t.Fatal("paused channel tuned")
	}
	if !s.VisibleTo(ctx, owner) {
		t.Fatal("enabled source hidden")
	}
	if _, err := s.ImportSource(ctx, Source{ID: "kept", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if s.VisibleTo(ctx, owner) {
		t.Fatal("Live TV visible with every source paused")
	}
	for _, id := range []string{"paused", "kept"} {
		if _, err := s.ImportSource(ctx, Source{ID: id, Disabled: false}); err != nil {
			t.Fatal(err)
		}
	}
	if got := sources(); len(got) != 2 || !s.VisibleTo(ctx, owner) {
		t.Fatalf("re-enabled lineup: %v", got)
	}
}
