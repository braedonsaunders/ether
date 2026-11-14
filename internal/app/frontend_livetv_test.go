package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/authhdr"
	"github.com/braedonsaunders/ether/internal/frontends/m3u"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/plextv"
)

type testRemoteLogo struct{}

func (testRemoteLogo) GetUntrustedURL(context.Context, string) ([]byte, string, error) {
	return []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10" fill="#f00"/></svg>`), "image/svg+xml", nil
}

func TestFrontendLiveTVUsesViewerPolicy(t *testing.T) {
	ctx := context.Background()
	a := setupTestAppConfigured(t, true, Options{}, nil)
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	local, _ := identity.NewLocal(a.db)
	viewerID, err := local.CreateViewer(ctx, "viewer", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	viewer, err := a.users.ActiveUser(ctx, viewerID)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.catalog.UpsertTitle(ctx, core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, IDs: core.IDs{TMDB: 1}, Title: "Programme", Released: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}, true); err != nil {
		t.Fatal(err)
	}
	c, err := a.livetv.SaveChannel(ctx, livetv.Channel{ID: "private-channel", Number: "1", Name: "Channel", Kind: "virtual", Enabled: true, Items: []livetv.Item{{Key: core.MovieKey(1), Title: "Programme", DurationMS: 3600000}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.livetv.SetChannelViewers(ctx, c.ID, livetv.ChannelViewers{Deny: []int64{viewerID}}); err != nil {
		t.Fatal(err)
	}
	facade := surfaceLiveTV{app: a}
	if rows, err := facade.Channels(ctx, viewer); err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	channelGUID, _ := ids.StringGUID(ids.Channel, c.ID)
	if _, err := facade.ResolveChannelGUID(ctx, viewer, channelGUID); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("restricted channel leaked", err)
	}
	rows, err := facade.Channels(ctx, owner)
	if err != nil || len(rows) != 1 || rows[0].ID != c.ID {
		t.Fatal(rows, err)
	}
	if _, err := facade.ResolveChannelGUID(ctx, owner, channelGUID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	q := frontend.LiveGuideQuery{From: now, Until: now.Add(2 * time.Hour), Limit: 2}
	guide, err := facade.Guide(ctx, owner, q)
	if err != nil || len(guide) == 0 || len(guide) > 2 {
		t.Fatal(guide, err)
	}
	programmeGUID, _ := ids.StringGUID(ids.Programme, guide[0].ID)
	if got, err := facade.ResolveProgrammeGUID(ctx, owner, programmeGUID, q); err != nil || got.ID != guide[0].ID {
		t.Fatal(got, err)
	}
	if rows, err := facade.Guide(ctx, viewer, q); err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	paged := frontend.LiveGuidePageQuery{LiveGuideQuery: q}
	paged.Limit = 1
	first, err := facade.GuidePage(ctx, owner, paged)
	if err != nil || len(first.Items) != 1 || !first.More || first.Snapshot == "" {
		t.Fatal("missing guide continuation", first, err)
	}
	paged.Offset, paged.Snapshot = 1, first.Snapshot
	second, err := facade.GuidePage(ctx, owner, paged)
	if err != nil || len(second.Items) != 1 || second.Items[0].ID == first.Items[0].ID {
		t.Fatal("guide page repeated", second, err)
	}
	paged.Snapshot = "changed"
	if _, err := facade.GuidePage(ctx, owner, paged); !errors.Is(err, frontend.ErrLiveGuideStale) {
		t.Fatal("changed guide snapshot accepted", err)
	}
	paged.Snapshot, paged.Offset = "", 0
	if page, err := facade.GuidePage(ctx, viewer, paged); err != nil || len(page.Items) != 0 || page.More {
		t.Fatal("restricted guide page leaked", page, err)
	}
	q.Until = q.From.Add(8 * 24 * time.Hour)
	if _, err := facade.Guide(ctx, owner, q); err == nil {
		t.Fatal("unbounded window")
	}
	if _, err := facade.Channels(ctx, nil); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal(err)
	}
}

func TestChannelArtworkGrantFollowsLiveTVPolicy(t *testing.T) {
	ctx := context.Background()
	a := setupTestAppConfigured(t, true, Options{}, nil)
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	viewerID, err := local.CreateViewer(ctx, "channelviewer", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	viewer, err := a.users.ActiveUser(ctx, viewerID)
	if err != nil {
		t.Fatal(err)
	}
	title := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, IDs: core.IDs{TMDB: 1}, Title: "Programme", Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := a.catalog.UpsertTitle(ctx, title, true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `UPDATE catalog_titles SET detail=1,detail_at=? WHERE key=?`, time.Now().Unix(), int64(title.Key)); err != nil {
		t.Fatal(err)
	}
	channel, err := a.livetv.SaveChannel(ctx, livetv.Channel{ID: "scoped-channel", Number: "9", Name: "News", Kind: "virtual", Enabled: true, Items: []livetv.Item{{Key: core.MovieKey(1), Title: "Programme", DurationMS: 3600000}}})
	if err != nil {
		t.Fatal(err)
	}
	source := *a.artwork
	source.URLImages = testRemoteLogo{}
	source.Raster = artwork.NewSVGRasterizer()
	facade := surfaceLiveTV{app: a, channelArtwork: source.ChannelImage}
	channels, err := facade.Channels(ctx, viewer)
	if err != nil || len(channels) != 1 || len(channels[0].Images) != 1 {
		t.Fatalf("channels: %+v %v", channels, err)
	}
	grants, err := identity.NewGrants(a.db, a.users, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := grants.Issue(ctx, viewerID, nil, "jellyfin", core.Device{ID: "tv", Language: "en"})
	if err != nil {
		t.Fatal(err)
	}
	service, err := identity.NewArtworkImageGrantService(a.db, "jellyfin", grants, func() frontend.Experience { return nil }, source, time.Minute, nil, identity.ImageGrantOptions{Token: authhdr.MediaBrowserToken, PublicContentTags: true})
	if err != nil {
		t.Fatal(err)
	}
	request := frontend.ImageGrantRequest{Credential: credential, Device: core.Device{ID: "tv", Language: "en"}, Channel: channel.ID, Image: channels[0].Images[0]}
	secret, err := service.Issue(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	service.Serve(w, httptest.NewRequest("GET", "/", nil), secret)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "News") {
		t.Fatalf("channel logo: %d %q", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	service.Serve(w, httptest.NewRequest("GET", "/?format=png&w=320", nil), secret)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("channel PNG: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	if err := a.livetv.SetChannelViewers(ctx, channel.ID, livetv.ChannelViewers{Deny: []int64{viewerID}}); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	service.Serve(w, httptest.NewRequest("GET", "/", nil), secret)
	if w.Code != 404 {
		t.Fatalf("hidden channel remained accessible: %d", w.Code)
	}
	if _, err := service.Issue(ctx, request); err == nil {
		t.Fatal("issued grant for hidden channel")
	}
	request.Item = core.MovieKey(1)
	if _, err := service.Issue(ctx, request); err == nil {
		t.Fatal("accepted both item and channel")
	}
	if err := a.livetv.SetChannelViewers(ctx, channel.ID, livetv.ChannelViewers{Allow: []int64{viewerID}}); err != nil {
		t.Fatal(err)
	}
	channel.Logo = "https://example.invalid/logo.png?token=private"
	if _, err := a.livetv.SaveChannel(ctx, channel); err != nil {
		t.Fatal(err)
	}
	channels, err = facade.Channels(ctx, viewer)
	if err != nil || len(channels) != 1 || channels[0].Logo != "" || len(channels[0].Images) != 1 || strings.Contains(channels[0].Images[0].Path, "private") {
		t.Fatalf("channel logo URL was exposed: %+v %v", channels, err)
	}
	request.Item = 0
	request.Image = channels[0].Images[0]
	secret, err = service.Issue(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	service.Serve(w, httptest.NewRequest("GET", "/?format=png&w=32", nil), secret)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("external SVG transform: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
}

// Exercise the real M3U handler through the shared adapter, not a mock guide.
func TestM3UGuideOver4096Channels(t *testing.T) {
	ctx := context.Background()
	a := setupTestAppConfigured(t, true, Options{}, nil)
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < 4100; i++ {
		id := fmt.Sprintf("large-%04d", i)
		c := livetv.Channel{ID: id, Number: fmt.Sprint(i + 1), Name: id, Kind: "iptv", Enabled: true, SourceID: "large", GuideID: id}
		b, _ := json.Marshal(c)
		if _, err = tx.ExecContext(ctx, `INSERT INTO livetv_channels(id,data) VALUES(?,?)`, id, b); err != nil {
			t.Fatal(err)
		}
		p := livetv.Programme{ChannelID: id, Start: now, End: now.Add(2 * time.Hour), Item: livetv.Item{Title: "Programme " + id}}
		b, _ = json.Marshal(p)
		if _, err = tx.ExecContext(ctx, `INSERT INTO livetv_programmes(source_id,channel_id,start_ms,end_ms,data) VALUES(?,?,?,?,?)`, "large", id, p.Start.UnixMilli(), p.End.UnixMilli(), b); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	inst, err := (m3u.Frontend{}).Mount(ctx, frontend.Host{Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return owner, nil }}, LiveTV: surfaceLiveTV{app: a}, Sessions: frontendtest.Sessions{}})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	inst.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/m3u/test/guide.xml", nil))
	if rec.Code != 200 {
		t.Fatalf("guide: %d %s", rec.Code, rec.Body.String())
	}
	if got := strings.Count(rec.Body.String(), "<programme "); got != 4100 {
		t.Fatalf("programmes=%d", got)
	}
	if got := strings.Count(rec.Body.String(), "<channel "); got != 4100 {
		t.Fatalf("channels=%d", got)
	}
}

// Each app's lineup, guide, tune check and Home rows honour the owner's
// per-app visibility; the console (no app) and other apps keep the channel.
func TestFrontendLiveTVHonoursPerAppVisibility(t *testing.T) {
	ctx := context.Background()
	a := setupTestAppConfigured(t, true, Options{}, nil)
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.catalog.UpsertTitle(ctx, core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, IDs: core.IDs{TMDB: 1}, Title: "Programme", Released: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}, true); err != nil {
		t.Fatal(err)
	}
	c, err := a.livetv.SaveChannel(ctx, livetv.Channel{ID: "app-channel", Number: "1", Name: "Channel", Kind: "virtual", Enabled: true, HiddenApps: []string{"xtream", "jellyfin", "jellyfin"}, Items: []livetv.Item{{Key: core.MovieKey(1), Title: "Programme", DurationMS: 3600000}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.HiddenApps, ",") != "jellyfin,xtream" {
		t.Fatalf("apps not normalized: %v", c.HiddenApps)
	}
	if _, err := a.livetv.SaveChannel(ctx, livetv.Channel{ID: "bad", Name: "Bad", Kind: "virtual", HiddenApps: []string{"stremio"}}); err == nil {
		t.Fatal("accepted an app without Live TV")
	}
	channelGUID, _ := ids.StringGUID(ids.Channel, c.ID)
	now := time.Now().UTC()
	q := frontend.LiveGuideQuery{From: now, Until: now.Add(2 * time.Hour)}
	for _, app := range []string{"", "jellyfin", "emby", "silo", "xtream", "m3u"} {
		want := 1
		if app == "jellyfin" || app == "xtream" {
			want = 0
		}
		facade := surfaceLiveTV{app: a, surface: app}
		if rows, err := facade.Channels(ctx, owner); err != nil || len(rows) != want {
			t.Fatalf("%q lineup: %v %v", app, rows, err)
		}
		if _, err := facade.ResolveChannelGUID(ctx, owner, channelGUID); (err == nil) != (want == 1) {
			t.Fatalf("%q resolve: %v", app, err)
		}
		if guide, err := facade.Guide(ctx, owner, q); err != nil || (len(guide) > 0) != (want == 1) {
			t.Fatalf("%q guide: %v %v", app, guide, err)
		}
		if err := (sessionLive{service: a.livetv, cached: a.catalog.CachedGet}).Allowed(core.WithLiveApp(ctx, app), owner, c.ID); (err == nil) != (want == 1) {
			t.Fatalf("%q tune: %v", app, err)
		}
		if rows, err := (liveForApp{svc: a.livetv, app: app}).AllowedChannels(ctx, owner); err != nil || len(rows) != want {
			t.Fatalf("%q Home rows: %v %v", app, rows, err)
		}
	}
}

// Exercise the keyless XMLTV path through the neutral guide and real opaque
// image grants, including reauthorization when channel visibility changes.
func TestLiveGuideProgrammeArtworkPolicy(t *testing.T) {
	ctx := context.Background()
	a := setupTestAppConfigured(t, true, Options{}, nil)
	local, _ := identity.NewLocal(a.db)
	viewerID, err := local.CreateViewer(ctx, "guide-viewer", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	viewer, err := a.users.ActiveUser(ctx, viewerID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	xml := `<tv><channel id="news"><display-name>News</display-name></channel><programme channel="news" start="` + now.Format("20060102150405 -0700") + `" stop="` + now.Add(time.Hour).Format("20060102150405 -0700") + `"><title>Bulletin</title><category>News</category><icon src="https://example.invalid/programme.svg?secret=upstream"/></programme></tv>`
	_, err = a.livetv.ImportSource(ctx, livetv.Source{ID: "epg-art", Name: "Guide", Playlist: "#EXTM3U\n#EXTINF:-1 tvg-id=\"news\" tvg-logo=\"https://example.invalid/logo.svg\",News\nhttps://example.invalid/stream\n", Guide: xml})
	if err != nil {
		t.Fatal(err)
	}
	source := *a.artwork
	source.URLImages = testRemoteLogo{}
	facade := surfaceLiveTV{app: a, surface: "vela", channelArtwork: source.ChannelImage, programmeArtwork: source.ProgrammeImages, artwork: source}
	q := frontend.LiveGuidePageQuery{LiveGuideQuery: frontend.LiveGuideQuery{From: now, Until: now.Add(time.Hour), Limit: 1}}
	page, err := facade.GuidePage(ctx, viewer, q)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("guide %+v %v", page, err)
	}
	p := page.Items[0]
	if p.Item != 0 || len(p.Images) != 1 || len(p.Genres) != 1 || p.Genres[0] != "News" || strings.Contains(p.Images[0].Path, "secret") {
		t.Fatalf("keyless EPG dropped or leaked: %+v", p)
	}
	grants, err := identity.NewGrants(a.db, a.users, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := grants.Issue(ctx, viewerID, nil, "vela", core.Device{ID: "tv"})
	if err != nil {
		t.Fatal(err)
	}
	images, err := identity.NewArtworkImageGrantService(a.db, "vela", grants, func() frontend.Experience { return nil }, source, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := frontend.ImageGrantRequest{Credential: credential, Device: core.Device{ID: "tv"}, Channel: p.ChannelID, Image: p.Images[0]}
	secret, err := images.Issue(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	images.Serve(w, httptest.NewRequest("GET", "/?w=0", nil), secret)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/svg+xml" {
		t.Fatalf("programme image %d %s", w.Code, w.Body.String())
	}
	tampered := req
	tampered.Image.Subject = "other-1"
	if _, err = images.Issue(ctx, tampered); err == nil {
		t.Fatal("accepted different airing")
	}
	if err = a.livetv.SetChannelViewers(ctx, p.ChannelID, livetv.ChannelViewers{Deny: []int64{viewerID}}); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	images.Serve(w, httptest.NewRequest("GET", "/?w=0", nil), secret)
	if w.Code != 404 {
		t.Fatal("image survived channel policy change", w.Code)
	}
	if _, err = images.Issue(ctx, req); err == nil {
		t.Fatal("issued hidden programme image")
	}
}
