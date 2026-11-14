package livetv

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// addonTestKind stands in for a plugin's add-on channel source.
type addonTestKind struct {
	fetch func() Fetched
	err   error
}

func (addonTestKind) ID() string               { return "plugin:addon.test/channels" }
func (addonTestKind) Describe() SourceDescribe { return SourceDescribe{} }
func (k addonTestKind) Matches(src Source) bool {
	return src.Kind == k.ID()
}
func (k addonTestKind) Fetch(_ context.Context, _ *Service, src *Source) (Fetched, error) {
	src.Kind, src.PlaylistURL = k.ID(), k.ID()
	if k.err != nil {
		return Fetched{}, k.err
	}
	return k.fetch(), nil
}

func addonChannel(sourceID, rawID string, urls ...string) Channel {
	id := stableID(sourceID, rawID)
	c := Channel{ID: id, SourceID: sourceID, GuideID: rawID, Name: rawID, Number: "1", Kind: "iptv", Enabled: true}
	for i, u := range urls {
		c.StreamSources = append(c.StreamSources, StreamSource{ID: AddonInputID(id, i), URL: u, Enabled: true, Priority: i})
	}
	if len(urls) > 0 {
		c.StreamURL = urls[0]
	}
	return c
}

func TestAddonRefreshKeepsOwnerChoicesAndHeaders(t *testing.T) {
	ctx := context.Background()
	s := testService(t, Options{})
	streams := []string{"https://live.example/a?key=secret", "https://live.example/b?key=secret"}
	kind := &addonTestKind{}
	kind.fetch = func() Fetched {
		c := addonChannel("src", "news", streams...)
		c.StreamSources[1].Headers = map[string]string{"Referer": "https://live.example/"}
		return Fetched{Channels: []Channel{c}, AddonInputs: true}
	}
	if err := s.RegisterSourceKind(kind); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportSource(ctx, Source{ID: "src", Name: "Add-on", Kind: kind.ID()}); err != nil {
		t.Fatal(err)
	}
	cs, err := s.SourceChannels(ctx, "src")
	if err != nil || len(cs) != 1 {
		t.Fatalf("channels=%+v err=%v", cs, err)
	}
	// The owner switches the first input off and adds a backup of their own.
	c := cs[0]
	c.StreamSources[0].Enabled = false
	c.StreamSources = append(c.StreamSources, StreamSource{ID: "mine", URL: "https://backup.example/live", Enabled: true, Priority: 5})
	if _, err := s.SaveChannel(ctx, c); err != nil {
		t.Fatal(err)
	}
	streams = []string{"https://live.example/a?key=rotated", "https://live.example/b?key=rotated"}
	if _, err := s.RefreshSource(ctx, "src"); err != nil {
		t.Fatal(err)
	}
	cs, _ = s.SourceChannels(ctx, "src")
	got := cs[0].StreamSources
	if len(got) != 3 || got[0].Enabled || got[0].URL != streams[0] || !got[1].Enabled || got[2].ID != "mine" {
		t.Fatalf("refresh lost owner choices: %+v", got)
	}
	url, headers, err := s.ResolveStreamSource(ctx, cs[0], got[1])
	if err != nil || url != streams[1] || headers.Get("Referer") != "https://live.example/" {
		t.Fatalf("resolve = %q %v %v", url, headers, err)
	}
}

func TestAddonInputsRedactedAndRestored(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"plugin:addon.test/channels", legacyStremioKind} {
		s := testService(t, Options{})
		src := Source{ID: "s", Name: "Source", Kind: kind, PlaylistURL: "https://addon.example/secret/manifest.json"}
		raw, err := s.encodeSource(src)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.o.DB.Exec(`INSERT INTO livetv_sources(id,data) VALUES(?,?)`, src.ID, raw); err != nil {
			t.Fatal(err)
		}
		original := addonChannel("s", "c", "https://example.test/live?token=secret")
		original.StreamSources[0].Headers = map[string]string{"X-Token": "secret"}
		original.StreamSources = append(original.StreamSources, StreamSource{ID: "own", URL: "https://own.example/live", Enabled: true, Priority: 1})
		if _, err = s.SaveChannel(ctx, original); err != nil {
			t.Fatal(err)
		}
		cs, err := s.Channels(ctx)
		if err != nil {
			t.Fatal(err)
		}
		s.redactAddonChannels(ctx, cs)
		in := cs[0].StreamSources
		if cs[0].StreamURL != redactedSecret || in[0].URL != redactedSecret || in[0].Headers["X-Token"] != redactedSecret || in[1].URL != "https://own.example/live" {
			t.Fatalf("%s: not redacted: %+v", kind, cs)
		}
		if err = s.restoreAddonChannel(ctx, &cs[0]); err != nil {
			t.Fatal(err)
		}
		if cs[0].StreamSources[0].URL != original.StreamURL || cs[0].StreamSources[0].Headers["X-Token"] != "secret" || cs[0].StreamURL != original.StreamURL {
			t.Fatalf("%s: not restored: %+v", kind, cs[0])
		}
		cs[0].StreamSources[0].ID = AddonInputPrefix + "other"
		cs[0].StreamSources[0].URL = redactedSecret
		if s.restoreAddonChannel(ctx, &cs[0]) == nil {
			t.Fatalf("%s: moved secret was restored", kind)
		}
	}
}

func TestInputHeadersValidated(t *testing.T) {
	c := Channel{ID: "c", Kind: "iptv", Enabled: true, StreamSources: []StreamSource{{ID: "a", URL: "https://example.test/a", Enabled: true, Headers: map[string]string{"Host": "evil.example"}}}}
	if err := normalizeStreamSources(&c); err == nil {
		t.Fatal("a managed header was accepted")
	}
	got := CleanInputHeaders(map[string]string{"user-agent": "VLC", "Host": "x", "X-Bad": "a\r\nb", "": "y"})
	if len(got) != 1 || got["User-Agent"] != "VLC" {
		t.Fatalf("clean = %v", got)
	}
	if CleanInputHeaders(map[string]string{"Range": "bytes=0-"}) != nil {
		t.Fatal("nothing left should be nil")
	}
}

func TestLegacyStremioSourceWaitsForMigrationThenMoves(t *testing.T) {
	ctx := context.Background()
	s := testService(t, Options{Now: func() time.Time { return time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC) }})
	link := "https://addon.example/secret/manifest.json"
	legacy := Source{ID: "old", Name: "Sports", Kind: legacyStremioKind, PlaylistURL: link, RefreshHours: 12}
	raw, err := s.encodeSource(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.o.DB.Exec(`INSERT INTO livetv_sources(id,data) VALUES(?,?)`, legacy.ID, raw); err != nil {
		t.Fatal(err)
	}
	c := addonChannel("old", "espn", "https://live.example/espn")
	c.Number, c.Favorite, c.GuideID = "501", true, "espn.us"
	if _, err = s.SaveChannel(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RefreshSource(ctx, "old"); !errors.Is(err, errLegacyStremio) {
		t.Fatalf("legacy refresh err = %v", err)
	}
	srcs, err := s.LegacyStremioSources(ctx)
	if err != nil || len(srcs) != 1 || srcs[0].PlaylistURL != link {
		t.Fatalf("legacy sources = %+v err=%v", srcs, err)
	}
	listed, _ := s.Sources(ctx)
	if listed[0].PlaylistURL != redactedSecret {
		t.Fatalf("legacy link leaked: %+v", listed[0])
	}
	kind := "plugin:addon.test/channels"
	for range 2 {
		if err := s.AdoptAddonSource(ctx, "old", kind); err != nil {
			t.Fatal(err)
		}
	}
	moved, err := s.storedSourceByID(ctx, "old")
	if err != nil || moved.Kind != kind || moved.PlaylistURL != kind || moved.Name != "Sports" || moved.RefreshHours != 12 {
		t.Fatalf("moved = %+v err=%v", moved, err)
	}
	if srcs, _ := s.LegacyStremioSources(ctx); len(srcs) != 0 {
		t.Fatalf("still legacy: %+v", srcs)
	}
	kept, err := s.Channel(ctx, c.ID)
	if err != nil || kept.Number != "501" || !kept.Favorite || kept.GuideID != "espn.us" {
		t.Fatalf("channel = %+v err=%v", kept, err)
	}
}

// An owner metadata read must neither leak add-on secrets nor modify the
// shared playback snapshot a later sweep and non-owner lineup use.
func TestOwnerChannelReadPreservesPlaybackSnapshot(t *testing.T) {
	for _, route := range []string{"/channels", "/channels/page", "/channels/id"} {
		t.Run(route, func(t *testing.T) {
			ctx := context.Background()
			s := testService(t, Options{Transcoder: &fixtureLiveProber{}})
			src := Source{ID: "addon", Name: "Add-on", Kind: "plugin:addon.test/channels"}
			raw, err := s.encodeSource(src)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.o.DB.ExecContext(ctx, `INSERT INTO livetv_sources(id,data) VALUES(?,?)`, src.ID, raw); err != nil {
				t.Fatal(err)
			}
			c := addonChannel(src.ID, "news", "https://example.invalid/live?key=fixture-secret")
			c.StreamSources[0].Headers = map[string]string{"X-Token": "fixture-secret"}
			if _, err = s.SaveChannel(ctx, c); err != nil {
				t.Fatal(err)
			}
			viewer := &core.User{ID: 123}
			s.sweepChannels(ctx)
			before, err := s.AllowedChannels(ctx, viewer)
			if err != nil || len(before) != 1 {
				t.Fatalf("before owner read: %d channels, %v", len(before), err)
			}
			path := route
			if route == "/channels/id" {
				path = "/channels/" + c.ID
			}
			response := httptest.NewRecorder()
			s.Handler().ServeHTTP(response, httptest.NewRequest("GET", path, nil))
			if response.Code != 200 || strings.Contains(response.Body.String(), "fixture-secret") || !strings.Contains(response.Body.String(), redactedSecret) {
				t.Fatal("owner response failed or leaked input credentials", response.Code)
			}
			s.sweepChannels(ctx)
			after, err := s.AllowedChannels(ctx, viewer)
			if err != nil || len(after) != 1 || after[0].ID != before[0].ID {
				t.Fatalf("owner read changed viewer lineup: before=%d after=%d err=%v", len(before), len(after), err)
			}
			cached, err := s.allChannels(ctx)
			if err != nil || len(cached) != 1 {
				t.Fatal("cached lineup unavailable", err)
			}
			if s.sweepState(cached[0]) != "available" || cached[0].StreamSources[0].URL != c.StreamURL || cached[0].StreamSources[0].Headers["X-Token"] != "fixture-secret" {
				t.Fatal("owner read corrupted playback inputs or health verdict")
			}
		})
	}
}
