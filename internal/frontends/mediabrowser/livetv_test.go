package mediabrowser

import (
	"context"
	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

type channelGrants struct{ requests []frontend.ImageGrantRequest }

func (g *channelGrants) Issue(_ context.Context, req frontend.ImageGrantRequest) (string, error) {
	g.requests = append(g.requests, req)
	return "channel-grant", nil
}
func (*channelGrants) Revoke(context.Context, string, string) error { return nil }
func (*channelGrants) Serve(w http.ResponseWriter, _ *http.Request, _ string) {
	w.WriteHeader(http.StatusNoContent)
}

func TestLiveChannelImageUsesChannelScopedGrant(t *testing.T) {
	grants := &channelGrants{}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return &frontend.Viewer{ID: 1}, nil }}
	live := frontendtest.LiveTV{ChannelsFunc: func(context.Context, *frontend.Viewer) ([]frontend.LiveChannel, error) {
		return []frontend.LiveChannel{{ID: "channel-1", Name: "News", Images: []frontend.Image{{Kind: frontend.ImageChannel, Path: "opaque"}}}}, nil
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, LiveTV: live, ImageGrants: grants})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/LiveTv/Channels", nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "channel-grant") || len(grants.requests) != 1 || grants.requests[0].Channel != "channel-1" || grants.requests[0].Item != 0 || grants.requests[0].Credential != "grant" {
		t.Fatal(w.Code, w.Body.String(), grants.requests)
	}
}

func TestLiveTVUsesViewerScopedHost(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	id, _ := ids.StringGUID(ids.Channel, "channel-1")
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	live := frontendtest.LiveTV{ChannelsFunc: func(_ context.Context, v *frontend.Viewer) ([]frontend.LiveChannel, error) {
		if v != viewer {
			t.Fatal(v)
		}
		return []frontend.LiveChannel{{ID: "channel-1", Name: "News", Number: "2"}}, nil
	}, ResolveChannelFunc: func(_ context.Context, v *frontend.Viewer, guid string) (frontend.LiveChannel, error) {
		if v != viewer || guid != id {
			t.Fatal(v, guid)
		}
		return frontend.LiveChannel{ID: "channel-1"}, nil
	}, GuideFunc: func(_ context.Context, v *frontend.Viewer, q frontend.LiveGuideQuery) ([]frontend.LiveProgramme, error) {
		if v != viewer || len(q.Channels) != 1 || q.Channels[0] != "channel-1" || !q.From.Equal(start) || q.Device.ID != "guide-device" {
			t.Fatal(v, q)
		}
		return []frontend.LiveProgramme{{ID: "program-1", ChannelID: "channel-1", Title: "News", Start: start, End: start.Add(time.Hour)}}, nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, LiveTV: live})
	if err != nil {
		t.Fatal(err)
	}
	spec, e := openapi.CachedSpec("../../conform/specs/manifest.json", "jellyfin", "")
	var doc *openapi.Document
	if e == nil {
		doc, e = openapi.Parse(spec)
	}
	if e != nil && !os.IsNotExist(e) {
		t.Fatal(e)
	}
	paths := []string{"/LiveTv/Channels", "/LiveTv/Programs?ChannelIds=" + id + "&MinStartDate=" + url.QueryEscape(start.Format(time.RFC3339)) + "&MaxStartDate=" + url.QueryEscape(start.Add(time.Hour).Format(time.RFC3339))}
	for _, path := range paths {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("X-Emby-Token", "grant")
		r.Header.Set("Authorization", `MediaBrowser DeviceId="guide-device"`)
		inst.Handler().ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "News") {
			t.Fatal(path, w.Code, w.Body.String())
		}
		if strings.HasPrefix(path, "/LiveTv/Programs") && !strings.Contains(w.Body.String(), `"StartDate":"2026-01-01T12:00:00.0000000Z"`) {
			t.Fatal("programme timestamp precision", w.Body.String())
		}
		if doc != nil {
			if err := doc.Validate("GET", path, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
				t.Fatal(path, err)
			}
		}
	}
}

func TestLivePlaybackUsesAuthorizedChannelTarget(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	guid, _ := ids.StringGUID(ids.Channel, "channel-1")
	var estimate, opened bool
	live := frontendtest.LiveTV{ResolveChannelFunc: func(_ context.Context, v *frontend.Viewer, id string) (frontend.LiveChannel, error) {
		if v != viewer || id != guid {
			t.Fatal(v, id)
		}
		return frontend.LiveChannel{ID: "channel-1"}, nil
	}}
	broker := frontendtest.Sessions{PlanFunc: func(_ context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
		if r.Channel != "channel-1" || r.Item != 0 {
			t.Fatal(r)
		}
		estimate = r.Intent.Estimate
		return frontend.PlayPlan{ID: "plan", Estimated: estimate}, nil
	}, OpenFunc: func(context.Context, frontend.OpenRequest) (frontend.Session, error) {
		opened = true
		return frontend.Session{ID: "session"}, nil
	}, StartStreamFunc: func(_ context.Context, _ frontend.SessionAccess, _ string, r frontend.StreamRequest) (string, error) {
		if r.Format != frontend.StreamHLS {
			t.Fatal(r)
		}
		return "/sessions/media/session/scoped/index.m3u8", nil
	}}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, LiveTV: live, Sessions: broker, Capabilities: frontendtest.Capabilities{}})
	if err != nil {
		t.Fatal(err)
	}
	call := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/Items/"+guid+"/PlaybackInfo", strings.NewReader(body))
		r.Header.Set("X-Emby-Token", "grant")
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	if w := call(`{"IsPlayback":false}`); w.Code != 200 || !estimate || opened {
		t.Fatal(w.Code, estimate, opened)
	}
	if w := call(`{"IsPlayback":true}`); w.Code != 200 || estimate || !opened || !strings.Contains(w.Body.String(), "/sessions/media/") {
		t.Fatal(w.Code, w.Body.String())
	}
}
