package plex

import (
	"bytes"
	"context"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/braedonsaunders/ether/internal/livetv"
)

type logoFetcher struct {
	urls        []string
	data        []byte
	contentType string
}

func (f *logoFetcher) GetUntrustedURL(_ context.Context, u string) ([]byte, string, error) {
	f.urls = append(f.urls, u)
	return f.data, f.contentType, nil
}

func TestPlexWebGuideLogoUsesServerOriginAndViewer(t *testing.T) {
	f, service, channel := liveFixture(t)
	response := f.get(t, liveProviderPath+"/lineups/dvr/channels", "Accept", "application/json", "X-Plex-Product", "Plex Web")
	channels := decodeJSON(t, response)["Channel"].([]any)
	logo, err := url.Parse(channels[0].(map[string]any)["thumb"].(string))
	if err != nil || logo.Scheme != "http" || logo.Host != "example.com" || logo.Path != liveLogo(channel) || logo.Query().Get("X-Plex-Token") != ownerToken {
		t.Fatal("Plex Web logo does not point to the authenticated server")
	}
	if rec := f.get(t, logo.RequestURI()); rec.Code != http.StatusOK {
		t.Fatalf("logo status: %d", rec.Code)
	}
	if err := service.SetChannelViewers(context.Background(), channel.ID, livetv.ChannelViewers{Deny: []int64{friendUser.ID}}); err != nil {
		t.Fatal(err)
	}
	logo.RawQuery = url.Values{"X-Plex-Token": {friendToken}}.Encode()
	if rec := f.get(t, logo.RequestURI()); rec.Code != http.StatusNotFound {
		t.Fatalf("restricted logo status: %d", rec.Code)
	}
}

func TestPlexWebGuideLogoEscapesChannelID(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	r := httptest.NewRequest("GET", "http://example.com/livetv/dvr/channels", nil)
	r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, &Request{Token: ownerToken, Product: "Plex Web"}))
	got := f.srv.liveGuideLogo(r, livetv.Channel{ID: "a b%c"})
	if !strings.HasPrefix(got, "http://example.com/livetv/channels/a%20b%25c/logo.png?") {
		t.Fatalf("channel ID is not escaped exactly once: %s", got)
	}
}

func TestLiveChannelLogoPlezyCompatibility(t *testing.T) {
	f, service, generated := liveFixture(t)
	src := livetv.Source{Name: "Logo source", Playlist: "#EXTM3U\n#EXTINF:-1 tvg-id=\"external\" tvg-logo=\"https://logos.example.test/logo.svg\",External\nhttps://stream.example.test/live\n"}
	if _, err := service.ImportSource(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	channels, err := service.Channels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var external livetv.Channel
	for _, c := range channels {
		if c.Name == "External" {
			external = c
		}
	}
	if external.ID == "" {
		t.Fatal("external channel missing")
	}
	fetch := &logoFetcher{data: []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><rect width="10" height="10" fill="red"/></svg>`), contentType: "image/svg+xml"}
	f.srv.channelLogos = fetch
	for _, channel := range []livetv.Channel{generated, external} {
		path := liveLogo(channel)
		for _, target := range []string{path, "/photo/:/transcode?width=120&height=80&url=" + url.QueryEscape(path+"?X-Plex-Token=ignored"), "/photo/:/transcode?url=" + url.QueryEscape(strings.Replace(path, "logo.png", "logo.svg", 1))} {
			rec := f.get(t, target, "X-Plex-Token", ownerToken)
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
				t.Fatalf("%s: %d %s", target, rec.Code, rec.Header().Get("Content-Type"))
			}
			if _, err := png.Decode(bytes.NewReader(rec.Body.Bytes())); err != nil {
				t.Fatalf("%s: invalid PNG: %v", target, err)
			}
		}
	}
	if len(fetch.urls) != 3 {
		t.Fatalf("fetch count: %v", fetch.urls)
	}
	for _, u := range fetch.urls {
		if u != external.Logo {
			t.Fatalf("unexpected fetched URL %q", u)
		}
	}
	// Unsafe SVG cannot trigger a fetch or active content in the client.
	fetch.data = []byte(`<svg xmlns="http://www.w3.org/2000/svg"><image href="http://127.0.0.1/secret"/></svg>`)
	rec := f.get(t, liveLogo(external))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("unsafe SVG fallback: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if _, err := png.Decode(bytes.NewReader(rec.Body.Bytes())); err != nil {
		t.Fatalf("unsafe SVG fallback is not PNG: %v", err)
	}
	if err := service.SetChannelViewers(context.Background(), external.ID, livetv.ChannelViewers{Deny: []int64{friendUser.ID}}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{liveLogo(external), "/photo/:/transcode?url=" + url.QueryEscape(liveLogo(external)+"?X-Plex-Token="+ownerToken)} {
		rec := f.get(t, target, "X-Plex-Token", friendToken)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("restricted %s: %d", target, rec.Code)
		}
	}
	if len(fetch.urls) != 4 {
		t.Fatal("unauthorized request fetched artwork")
	}
	for _, target := range []string{"https://evil.example" + liveLogo(external), "http://169.254.169.254/latest/meta-data"} {
		if rec := f.get(t, "/photo/:/transcode?url="+url.QueryEscape(target)); rec.Code != http.StatusForbidden {
			t.Fatalf("remote %s: %d", target, rec.Code)
		}
	}
	f.srv.d.Identity.MyPlexSubscription = false
	if rec := f.get(t, "/photo/:/transcode?url="+url.QueryEscape(liveLogo(external))); rec.Code != http.StatusNotFound {
		t.Fatalf("disabled live TV: %d", rec.Code)
	}
}

func TestLiveChannelLogoBlocksPrivateSource(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(200) }))
	defer upstream.Close()
	f, service, _ := liveFixture(t)
	src := livetv.Source{Name: "Private logo", Playlist: "#EXTM3U\n#EXTINF:-1 tvg-id=\"private\" tvg-logo=\"" + upstream.URL + "\",Private logo\nhttps://stream.example.test/live\n"}
	if _, err := service.ImportSource(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	channels, err := service.Channels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range channels {
		if c.Name == "Private logo" {
			rec := f.get(t, "/photo/:/transcode?url="+url.QueryEscape(liveLogo(c)))
			if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" {
				t.Fatalf("fallback: %d", rec.Code)
			}
			if requests.Load() != 0 {
				t.Fatal("private upstream reached")
			}
			return
		}
	}
	t.Fatal("channel missing")
}
