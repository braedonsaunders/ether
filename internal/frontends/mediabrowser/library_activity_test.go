package mediabrowser

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type activityFake struct {
	frontendtest.Experience
	viewer      *frontend.Viewer
	latestViews []string
	resumeCalls int
	lowerBound  bool
	lastExact   bool
}

func (f *activityFake) LatestAdded(_ context.Context, v *frontend.Viewer, d frontend.Device, view string, limit int) ([]frontend.LatestItem, error) {
	if v != f.viewer || d.ID != "tv" || limit != 20 {
		return nil, frontend.ErrNotFound
	}
	if view != "1" && view != "2" {
		return nil, frontend.ErrNotFound
	}
	f.latestViews = append(f.latestViews, view)
	at := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	title := frontend.Title{Key: 5, Type: "movie", Title: "Latest movie"}
	if view == "2" {
		title = frontend.Title{Key: 6, Type: "show", Title: "Latest series"}
	}
	return []frontend.LatestItem{{Title: title, AddedAt: &at}}, nil
}
func (f *activityFake) Resume(_ context.Context, v *frontend.Viewer, d frontend.Device, view string, types []frontend.MediaType, offset, limit int, requireExact bool) (frontend.ActivityPage, error) {
	if v != f.viewer || d.ID != "tv" || view != "" || len(types) != 2 || offset != 0 || limit != 20 {
		return frontend.ActivityPage{}, frontend.ErrNotFound
	}
	f.resumeCalls++
	f.lastExact = requireExact
	return frontend.ActivityPage{Items: []frontend.Title{{Key: 7, Type: "episode", Title: "Resume"}}, Total: 1, TotalExact: !f.lowerBound, More: f.lowerBound}, nil
}

func TestSwiftfinLatestAndResumeIgnoreHomeRows(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	for _, tc := range []struct {
		name  string
		front frontend.Frontend
	}{{"jellyfin", Jellyfin{MachineID: "test"}}, {"emby", Emby{MachineID: "test"}}} {
		exp := &activityFake{viewer: viewer, Experience: frontendtest.Experience{RowsFunc: func(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
			t.Fatal("Home rows consulted")
			return nil, nil
		}}}
		identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
		inst, err := tc.front.Mount(context.Background(), frontend.Host{Identity: identity, Experience: exp})
		if err != nil {
			t.Fatal(err)
		}
		for _, lib := range []struct {
			id   int64
			name string
		}{{1, "Latest movie"}, {2, "Latest series"}} {
			view, _ := ids.GUID(ids.View, lib.id)
			path := "/Items/Latest?enableUserData=true&fields=MediaStreams&fields=Genres&fields=Studios&limit=20&parentId=" + view
			r := httptest.NewRequest("GET", path, nil)
			r.Header.Set("Authorization", `MediaBrowser Token="grant", DeviceId="tv"`)
			w := httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, r)
			if w.Code != 200 || !strings.Contains(w.Body.String(), lib.name) || !strings.Contains(w.Body.String(), `"DateCreated":"2024-01-02T03:04:05.0000000Z"`) {
				t.Fatal(tc.name, w.Code, w.Body.String())
			}
		}
		r := httptest.NewRequest("GET", "/UserItems/Resume?limit=20&mediaTypes=Video&startIndex=0", nil)
		r.Header.Set("Authorization", `MediaBrowser Token="grant", DeviceId="tv"`)
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "Resume") || exp.resumeCalls != 1 || !exp.lastExact || len(exp.latestViews) != 2 {
			t.Fatal(tc.name, w.Code, w.Body.String(), exp)
		}
		if tc.name == "jellyfin" && (strings.Contains(w.Body.String(), `"CanDelete"`) || strings.Contains(w.Body.String(), `"Overview"`) || strings.Contains(w.Body.String(), `"ProviderIds"`) || strings.Contains(w.Body.String(), `"OriginalTitle"`)) {
			t.Fatal(w.Body.String())
		}
		exp.lowerBound = true
		r = httptest.NewRequest("GET", "/UserItems/Resume?limit=20&mediaTypes=Video&startIndex=0&enableTotalRecordCount=false", nil)
		r.Header.Set("Authorization", `MediaBrowser Token="grant", DeviceId="tv"`)
		w = httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		if w.Code != 200 || exp.lastExact || strings.Contains(w.Body.String(), "TotalRecordCount") {
			t.Fatal(tc.name, w.Code, w.Body.String())
		}
	}
}

type groupedActivityFake struct {
	*activityFake
	groupCalls []bool
}

func (f *groupedActivityFake) LatestAddedGrouped(ctx context.Context, v *frontend.Viewer, d frontend.Device, view string, limit int, group bool) ([]frontend.LatestItem, error) {
	f.groupCalls = append(f.groupCalls, group)
	rows, err := f.LatestAdded(ctx, v, d, view, limit)
	if err == nil && !group && view == "2" {
		rows[0].Title.Type = "episode"
		rows[0].Title.Title = "Latest episode"
	}
	return rows, err
}

func TestLatestGroupingAndItemAddedDate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		front frontend.Frontend
	}{{"jellyfin", Jellyfin{MachineID: "test"}}, {"emby", Emby{MachineID: "test"}}} {
		viewer := &frontend.Viewer{ID: 1}
		exp := &groupedActivityFake{activityFake: &activityFake{viewer: viewer}}
		identity := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}
		inst, err := tc.front.Mount(context.Background(), frontend.Host{Identity: identity, Experience: exp})
		if err != nil {
			t.Fatal(err)
		}
		view, _ := ids.GUID(ids.View, 2)
		for _, v := range []struct{ query, name string }{{"", "Latest series"}, {"&GroupItems=false", "Latest episode"}} {
			r := httptest.NewRequest("GET", "/Items/Latest?limit=20&ParentId="+view+v.query, nil)
			r.Header.Set("Authorization", `MediaBrowser Token="grant", DeviceId="tv"`)
			w := httptest.NewRecorder()
			inst.Handler().ServeHTTP(w, r)
			if w.Code != 200 || !strings.Contains(w.Body.String(), v.name) {
				t.Fatal(tc.name, w.Code, w.Body.String())
			}
		}
		if len(exp.groupCalls) != 2 || !exp.groupCalls[0] || exp.groupCalls[1] {
			t.Fatal(tc.name, exp.groupCalls)
		}
		i := inst.(*instance)
		at := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
		if dto := i.itemDTO(frontend.Title{Key: 5, Type: "movie", Released: at, AddedAt: at}, frontend.WatchStatus{}); dto["DateCreated"] != wireTime(at) {
			t.Fatal(tc.name, dto)
		}
		if dto := i.itemDTO(frontend.Title{Key: 5, Type: "movie"}, frontend.WatchStatus{}); dto["DateCreated"] != nil {
			t.Fatal(tc.name, dto)
		}
	}
}
