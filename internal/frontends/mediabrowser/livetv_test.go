package mediabrowser

import (
	"context"
	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

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
		if v != viewer || len(q.Channels) != 1 || q.Channels[0] != "channel-1" || !q.From.Equal(start) {
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
		inst.Handler().ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "News") {
			t.Fatal(path, w.Code, w.Body.String())
		}
		if doc != nil {
			if err := doc.Validate("GET", path, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); err != nil {
				t.Fatal(path, err)
			}
		}
	}
}
