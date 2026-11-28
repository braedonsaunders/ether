package xtream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/compactids"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/logredact"
	"github.com/braedonsaunders/ether/internal/store"
)

type appIdentity struct {
	frontendtest.Identity
	revoked bool
}

func (a *appIdentity) AuthenticateAppPassword(ctx context.Context, u, p string) (*frontend.Viewer, error) {
	if a.revoked || u != frontend.XtreamUsername("eg1_test") || p != "eg1_test" {
		return nil, core.ErrUnauthorized
	}
	if _, ok := frontend.ClientIP(ctx); !ok {
		return nil, core.ErrUnauthorized
	}
	return &frontend.Viewer{ID: 1}, nil
}
func testHost(t *testing.T) (frontend.Host, *appIdentity) {
	t.Helper()
	a := &appIdentity{}
	db := store.OpenTest(t)
	allocator, err := compactids.New(db)
	if err != nil {
		t.Fatal(err)
	}
	host := frontend.Host{Identity: a, IDs: ids.Codecs{Allocator: allocator}, Sessions: frontendtest.Sessions{}, Experience: frontendtest.Experience{}, ClientIP: func(*http.Request) (netip.Addr, bool) { return netip.MustParseAddr("127.0.0.1"), true }, SurfaceURL: func(context.Context, *http.Request) (string, error) { return "http://ether.test/xtream", nil }, LiveTV: frontendtest.LiveTV{ChannelsFunc: func(_ context.Context, v *frontend.Viewer) ([]frontend.LiveChannel, error) {
		if v.ID != 1 {
			t.Fatal("wrong viewer")
		}
		return []frontend.LiveChannel{{ID: "allowed", Name: "News", Number: "7", Kind: "iptv"}}, nil
	}, GuideFunc: func(_ context.Context, v *frontend.Viewer, q frontend.LiveGuideQuery) ([]frontend.LiveProgramme, error) {
		if v.ID != 1 || len(q.Channels) != 1 || q.Channels[0] != "allowed" {
			t.Fatal("unscoped guide")
		}
		return []frontend.LiveProgramme{{ID: "p1", ChannelID: "allowed", Title: "Now & next", Summary: "Summary", Start: time.Now().Add(-time.Hour), End: time.Now().Add(time.Hour)}, {ID: "secret", ChannelID: "blocked", Title: "Hidden", Start: time.Now(), End: time.Now().Add(time.Hour)}}, nil
	}}}
	return host, a
}
func mount(t *testing.T, h frontend.Host) http.Handler {
	t.Helper()
	i, err := (Frontend{}).Mount(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = i.Close(context.Background()) })
	return i.Handler()
}
func request(h http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/xtream"+path, nil))
	return w
}
func authQuery() string {
	return "username=" + frontend.XtreamUsername("eg1_test") + "&password=eg1_test"
}
func TestStartupSequenceAndRevocation(t *testing.T) {
	host, a := testHost(t)
	h := mount(t, host)
	for _, action := range []string{"", "get_live_categories", "get_live_streams", "get_vod_categories", "get_series_categories"} {
		w := request(h, "/player_api.php?"+authQuery()+"&action="+action)
		if w.Code != 200 || !json.Valid(w.Body.Bytes()) {
			t.Fatalf("%s %d %s", action, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "Hidden") {
			t.Fatal("policy leak")
		}
	}
	w := request(h, "/player_api.php?"+authQuery()+"&action=get_live_streams")
	var rows []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["category_id"] != "2" || rows[0]["num"] != float64(1) {
		t.Fatal(rows)
	}
	id := fmt.Sprint(rows[0]["stream_id"])
	for _, action := range []string{"get_short_epg", "get_simple_data_table"} {
		w = request(h, "/player_api.php?"+authQuery()+"&action="+action+"&stream_id="+id)
		if w.Code != 200 || !strings.Contains(w.Body.String(), base64.StdEncoding.EncodeToString([]byte("Now & next"))) {
			t.Fatalf("epg %s", w.Body.String())
		}
		var epg struct {
			Listings []struct {
				EPGID string `json:"epg_id"`
			} `json:"epg_listings"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &epg); err != nil || len(epg.Listings) == 0 || epg.Listings[0].EPGID != id {
			t.Fatalf("expected epg_id %s: %s", id, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "Hidden") {
			t.Fatal("guide leak")
		}
	}
	w = request(h, "/xmltv.php?"+authQuery())
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Now &amp; next") || strings.Contains(w.Body.String(), "Hidden") {
		t.Fatal(w.Body.String())
	}
	if w = request(h, "/player_api.php?username=owner&password=main-password"); w.Code != 401 {
		t.Fatal("accepted main password")
	}
	groupedHost, _ := testHost(t)
	groupedHost.LiveTV = frontendtest.LiveTV{ChannelsFunc: func(context.Context, *frontend.Viewer) ([]frontend.LiveChannel, error) {
		return []frontend.LiveChannel{{ID: "news", Name: "News", Kind: "iptv", Group: "News & Weather"}, {ID: "sport", Name: "Sport", Kind: "iptv", Group: "Sports"}}, nil
	}}
	grouped := mount(t, groupedHost)
	var categories []map[string]any
	response := request(grouped, "/player_api.php?"+authQuery()+"&action=get_live_categories")
	if err := json.Unmarshal(response.Body.Bytes(), &categories); err != nil || len(categories) != 2 || categories[0]["category_name"] != "News & Weather" {
		t.Fatalf("provider groups: %s %v", response.Body.String(), err)
	}
	categoryID := fmt.Sprint(categories[0]["category_id"])
	for range 2 {
		response = request(grouped, "/player_api.php?"+authQuery()+"&action=get_live_streams&category_id="+categoryID)
		var selected []map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &selected); err != nil || len(selected) != 1 || selected[0]["name"] != "News" || selected[0]["category_id"] != categoryID {
			t.Fatalf("stable filtered group: %s %v", response.Body.String(), err)
		}
	}
	response = request(grouped, "/get.php?"+authQuery()+"&type=m3u_plus")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `group-title="News & Weather"`) || !strings.Contains(response.Body.String(), `group-title="Sports"`) {
		t.Fatalf("export groups: %d %s", response.Code, response.Body.String())
	}
	ownerGroupedHost, _ := testHost(t)
	ownerGroupedHost.LiveTV = frontendtest.LiveTV{
		GroupsFunc: func(context.Context, *frontend.Viewer) ([]frontend.LiveGroup, error) {
			return []frontend.LiveGroup{{ID: "top-picks", Name: "Top Picks"}}, nil
		},
		ChannelsFunc: func(context.Context, *frontend.Viewer) ([]frontend.LiveChannel, error) {
			return []frontend.LiveChannel{
				{ID: "news", Name: "News", Kind: "iptv", Group: "Provider News", Groups: []frontend.LiveGroup{{ID: "top-picks", Name: "Top Picks"}}},
				{ID: "sport", Name: "Sport", Kind: "iptv", Group: "Sports"},
			}, nil
		},
	}
	ownerGrouped := mount(t, ownerGroupedHost)
	categories = nil
	response = request(ownerGrouped, "/player_api.php?"+authQuery()+"&action=get_live_categories")
	if err := json.Unmarshal(response.Body.Bytes(), &categories); err != nil || len(categories) != 2 || categories[0]["category_name"] != "Top Picks" || categories[1]["category_name"] != "Sports" {
		t.Fatalf("owner groups first + ungrouped provider fallback: %s %v", response.Body.String(), err)
	}
	response = request(ownerGrouped, "/get.php?"+authQuery()+"&type=m3u_plus")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `group-title="Top Picks"`) || strings.Contains(response.Body.String(), `group-title="Provider News"`) || !strings.Contains(response.Body.String(), `group-title="Sports"`) {
		t.Fatalf("owner export groups: %d %s", response.Code, response.Body.String())
	}
	a.revoked = true
	if w = request(h, "/player_api.php?"+authQuery()); w.Code != 401 {
		t.Fatal("revocation ignored")
	}
}
func TestCatalogueUsesNeutralPagesAndCompactIDs(t *testing.T) {
	host, _ := testHost(t)
	calls := 0
	movie := frontend.Title{Key: core.MovieKey(12), Type: core.TypeMovie, Title: "Film", Summary: "Plot", Runtime: 90 * time.Minute, Year: 2026, Rating: 8}
	host.Experience = frontendtest.Experience{ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
		return []frontend.View{{ID: "1", Type: core.TypeMovie, Name: "Movies"}, {ID: "2", Type: core.TypeMovie, Name: "Other"}}, nil
	}, LibraryItemsFunc: func(_ context.Context, v *core.Viewer, d core.Device, view string, q frontend.ItemQuery) (core.Page, error) {
		calls++
		if view != "1" || q.Limit != pageSize || q.Offset != 0 {
			t.Fatal("wrong category or unbounded page")
		}
		return core.Page{Items: []core.Title{movie}, Total: 1}, nil
	}, ItemFunc: func(_ context.Context, r frontend.ItemRequest) (frontend.ItemDetail, error) {
		if r.Viewer.ID != 1 || r.Key != movie.Key {
			t.Fatal("wrong catalogue identity")
		}
		return frontend.ItemDetail{Title: movie}, nil
	}}
	h := mount(t, host)
	w := request(h, "/player_api.php?"+authQuery()+"&action=get_vod_streams&category_id=1")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	id := rows[0]["stream_id"].(float64)
	if id <= 0 || id > 2147483647 || calls != 1 {
		t.Fatalf("id=%v calls=%d", id, calls)
	}
	w = request(h, fmt.Sprintf("/player_api.php?%s&action=get_vod_info&vod_id=%.0f", authQuery(), id))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"duration_secs":5400`) {
		t.Fatal(w.Body.String())
	}
}
func TestCredentialRedaction(t *testing.T) {
	for _, s := range []string{"http://host/xtream/live/private-user/private-pass/1.ts", "/movie/private-user/private-pass/1.mkv", "/series/private-user/private-pass/1.mp4", "/player_api.php?username=private-user&password=private-pass", `{"username":"private-user","password":"private-pass"}`} {
		safe := logredact.String(s)
		if strings.Contains(safe, "private-user") || strings.Contains(safe, "private-pass") {
			t.Fatalf("leak: %s", safe)
		}
	}
}

type syntheticExport struct {
	frontendtest.Experience
	size, calls int
}

func (s *syntheticExport) ExportLibraryPage(_ context.Context, _ *frontend.Viewer, _ frontend.Device, view string, after frontend.RatingKey, limit int) (frontend.LibraryExportPage, error) {
	s.calls++
	if view != "1" || limit != pageSize {
		panic("unbounded export")
	}
	start := 0
	if after != 0 {
		start = int(after - core.MovieKey(0))
	}
	end := min(start+limit, s.size)
	items := make([]frontend.Title, 0, end-start)
	for n := start; n < end; n++ {
		items = append(items, frontend.Title{Key: core.MovieKey(int64(n + 1)), Type: core.TypeMovie, Title: fmt.Sprintf("Film %d", n)})
	}
	return frontend.LibraryExportPage{Items: items, Next: core.MovieKey(int64(end)), Done: end == s.size}, nil
}

type discardResponse struct {
	h     http.Header
	bytes int
}

func (w *discardResponse) Header() http.Header         { return w.h }
func (w *discardResponse) WriteHeader(int)             {}
func (w *discardResponse) Write(b []byte) (int, error) { w.bytes += len(b); return len(b), nil }

var _ io.Writer = (*discardResponse)(nil)

func TestLargeCatalogueExport(t *testing.T) {
	host, _ := testHost(t)
	s := &syntheticExport{size: 100001}
	s.ViewsFunc = func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
		return []frontend.View{{ID: "1", Type: core.TypeMovie}}, nil
	}
	s.LibraryItemsFunc = func(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (core.Page, error) {
		t.Fatal("offset fallback used")
		return core.Page{}, nil
	}
	host.Experience = s
	h := mount(t, host)
	w := &discardResponse{h: make(http.Header)}
	start := time.Now()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/xtream/player_api.php?"+authQuery()+"&action=get_vod_streams&category_id=1", nil))
	elapsed := time.Since(start)
	if s.calls != 401 || w.bytes < 1000000 {
		t.Fatalf("calls=%d bytes=%d", s.calls, w.bytes)
	}
	t.Logf("100001 titles: %v, %d bounded pages, %d bytes", elapsed, s.calls, w.bytes)
}

type testLazy struct {
	t   *testing.T
	got frontend.PlanRequest
}

func (s *testLazy) Issue(_ context.Context, p string, r frontend.PlanRequest) (string, error) {
	s.got = r
	if r.Channel != "allowed" || p != "eg1_test" {
		s.t.Fatal("bad live scope")
	}
	return "/sessions/lazy/opaque/live/index.m3u8", nil
}
func (s *testLazy) ServeHTTP(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }
func (s *testLazy) ServeAuthenticated(w http.ResponseWriter, r *http.Request, p string, plan frontend.PlanRequest) {
	s.got = plan
	if p != "eg1_test" || r.Header.Get("Range") != "bytes=10-19" {
		s.t.Fatal("range or credential lost")
	}
	w.Header().Set("Content-Range", "bytes 10-19/100")
	w.WriteHeader(206)
	_, _ = w.Write([]byte("0123456789"))
}
func TestXtreamPlaybackUsesBrokerAndPreservesRange(t *testing.T) {
	host, _ := testHost(t)
	key := core.MovieKey(12)
	host.Experience = frontendtest.Experience{ItemFunc: func(_ context.Context, r frontend.ItemRequest) (frontend.ItemDetail, error) {
		if r.Key != key || r.Viewer.ID != 1 {
			return frontend.ItemDetail{}, core.ErrNotFound
		}
		return frontend.ItemDetail{Title: frontend.Title{Key: key, Type: core.TypeMovie}}, nil
	}}
	host.Capabilities = frontendtest.Capabilities{BuildFunc: func(_ context.Context, r frontend.CapabilityRequest) (frontend.CapabilityResult, error) {
		if r.Device.Protocol != "xtream" || len(r.Device.VideoCodecs) == 0 {
			t.Fatal("missing native declarations")
		}
		return frontend.CapabilityResult{}, nil
	}}
	lazy := &testLazy{t: t}
	host.LazyStreams = lazy
	h := mount(t, host)
	id, err := host.IDs.Compact(context.Background(), "xtream", "item", fmt.Sprint(key))
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", fmt.Sprintf("/xtream/movie/%s/eg1_test/%d.mkv", frontend.XtreamUsername("eg1_test"), id), nil)
	r.Header.Set("Range", "bytes=10-19")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 206 || lazy.got.Item != key || lazy.got.Intent.AllowTranscoding == nil || *lazy.got.Intent.AllowTranscoding || !lazy.got.Intent.Options.DirectPlay {
		t.Fatalf("bad playback: %d %+v", w.Code, lazy.got)
	}
	id, err = host.IDs.Compact(context.Background(), "xtream", "channel", "allowed")
	if err != nil {
		t.Fatal(err)
	}
	w = request(h, fmt.Sprintf("/live/%s/eg1_test/%d.m3u8", frontend.XtreamUsername("eg1_test"), id))
	if w.Code != 307 || w.Header().Get("Location") != "http://ether.test/xtream/sessions/lazy/opaque/live/index.m3u8" || strings.Contains(w.Header().Get("Location"), "eg1_") {
		t.Fatal(w.Code, w.Header())
	}
}
func TestXtreamSeriesSeasonsAndEpisodeIdentity(t *testing.T) {
	host, _ := testHost(t)
	showKey, _ := core.AllocatedKey(core.KindShow, 1)
	firstSeason, _ := core.AllocatedKey(core.KindSeason, 1)
	yearSeason, _ := core.AllocatedKey(core.KindSeason, 2)
	firstEpisode, _ := core.AllocatedKey(core.KindEpisode, 1)
	yearEpisode, _ := core.AllocatedKey(core.KindEpisode, 2)
	show := frontend.Title{Key: showKey, Type: core.TypeShow, Title: "Show", IDs: core.IDs{TVDB: 1}}
	seasons := []core.Title{
		{Key: firstSeason, Type: core.TypeSeason, ShowKey: showKey, Season: 1},
		{Key: yearSeason, Type: core.TypeSeason, ShowKey: showKey, Season: 2026},
	}
	episodes := map[core.RatingKey]core.Title{
		firstSeason: {Key: firstEpisode, Type: core.TypeEpisode, ShowKey: showKey, ParentKey: firstSeason, Season: 1, Episode: 1, Title: "Episode"},
		yearSeason:  {Key: yearEpisode, Type: core.TypeEpisode, ShowKey: showKey, ParentKey: yearSeason, Season: 2026, Episode: 123, Title: "Daily Episode"},
	}
	host.Experience = frontendtest.Experience{ItemFunc: func(_ context.Context, r frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{Title: show}, nil
	}, ChildrenFunc: func(_ context.Context, v *frontend.Viewer, d frontend.Device, k core.RatingKey, q frontend.ItemQuery) (core.Page, error) {
		if v.ID != 1 || q.Limit != pageSize {
			t.Fatal("unscoped/unbounded series")
		}
		if k == show.Key {
			return core.Page{Items: seasons, Total: len(seasons)}, nil
		}
		if episode, ok := episodes[k]; ok {
			return core.Page{Items: []core.Title{episode}, Total: 1}, nil
		}
		return core.Page{}, core.ErrNotFound
	}}
	id, err := host.IDs.Compact(context.Background(), "xtream", "item", fmt.Sprint(show.Key))
	if err != nil {
		t.Fatal(err)
	}
	h := mount(t, host)
	w := request(h, fmt.Sprintf("/player_api.php?%s&action=get_series_info&series_id=%d", authQuery(), id))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var result struct {
		Seasons []struct {
			ID int32 `json:"id"`
		}
		Episodes map[string][]struct {
			ID     string `json:"id"`
			Number int    `json:"episode_num"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Seasons) != 2 || result.Seasons[0].ID == 0 || result.Seasons[1].ID == 0 || len(result.Episodes["1"]) != 1 || len(result.Episodes["2026"]) != 1 {
		t.Fatal(w.Body.String())
	}
	for _, episode := range episodes {
		wire := result.Episodes[strconv.Itoa(episode.Season)][0]
		ref, err := host.IDs.Resolve(context.Background(), "xtream", "item", int32(mustInt(t, wire.ID)))
		if err != nil || ref != episode.Key.String() || wire.Number != episode.Episode {
			t.Fatalf("episode identity %s, number %d: %v", ref, wire.Number, err)
		}
	}
}
func mustInt(t *testing.T, s string) int64 {
	t.Helper()
	var n int64
	if _, err := fmt.Sscan(s, &n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestXtreamCatalogueFailureResponses(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprint(late), func(t *testing.T) {
			host, _ := testHost(t)
			host.Experience = frontendtest.Experience{ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
				if !late {
					return nil, fmt.Errorf("backend unavailable")
				}
				return []frontend.View{{ID: "1", Type: "movie"}}, nil
			}, LibraryItemsFunc: func(_ context.Context, _ *frontend.Viewer, _ frontend.Device, _ string, q frontend.ItemQuery) (core.Page, error) {
				if q.Offset > 0 {
					return core.Page{}, fmt.Errorf("backend unavailable")
				}
				return core.Page{Items: []core.Title{{Key: core.MovieKey(1), Type: core.TypeMovie, Title: "First"}}, Total: 2}, nil
			}}
			h := mount(t, host)
			w := httptest.NewRecorder()
			aborted := false
			func() {
				defer func() {
					if p := recover(); p != nil {
						if p != http.ErrAbortHandler {
							panic(p)
						}
						aborted = true
					}
				}()
				h.ServeHTTP(w, httptest.NewRequest("GET", "/xtream/player_api.php?"+authQuery()+"&action=get_vod_streams", nil))
			}()
			if late {
				if !aborted || json.Valid(w.Body.Bytes()) {
					t.Fatal("partial response reported success")
				}
			} else if w.Code != 503 || aborted {
				t.Fatalf("early failure %d abort=%v", w.Code, aborted)
			}
		})
	}
}
func TestXtreamCanonicalIdentifiersAndPaths(t *testing.T) {
	host, _ := testHost(t)
	host.Capabilities = frontendtest.Capabilities{}
	host.LazyStreams = &testLazy{t: t}
	h := mount(t, host)
	for _, file := range []string{"%2531.mkv", "1.ts|m3u8", "+1.mkv", "01.mp4", "2147483648.mkv", "1.mkv/extra", "1%2f2.mkv", "1.exe", "1.", "../1.mkv"} {
		w := request(h, "/movie/"+frontend.XtreamUsername("eg1_test")+"/eg1_test/"+file)
		if w.Code != 404 {
			t.Fatalf("%s: %d", file, w.Code)
		}
	}
	w := request(h, "/PLAYER_API.PHP?"+authQuery())
	if w.Code != 200 || !json.Valid(w.Body.Bytes()) {
		t.Fatal("case-insensitive kit route not dispatched")
	}
}

type unavailableAdmission struct {
	testLazy
	err error
}

func (a *unavailableAdmission) PrepareAppConnection(*http.Request, string, frontend.PlanRequest) error {
	return a.err
}
func TestXtreamAdmissionFailureStatus(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{{frontend.ErrStreamLimit, 429}, {frontend.ErrUnauthorized, 401}, {fmt.Errorf("storage unavailable"), 503}} {
		host, _ := testHost(t)
		host.Experience = frontendtest.Experience{ItemFunc: func(_ context.Context, r frontend.ItemRequest) (frontend.ItemDetail, error) {
			return frontend.ItemDetail{Title: frontend.Title{Key: r.Key, Type: "movie"}}, nil
		}}
		host.Capabilities = frontendtest.Capabilities{}
		host.LazyStreams = &unavailableAdmission{err: tc.err}
		id, _ := host.IDs.Compact(context.Background(), "xtream", "item", fmt.Sprint(core.MovieKey(1)))
		w := request(mount(t, host), fmt.Sprintf("/movie/%s/eg1_test/%d.mkv", frontend.XtreamUsername("eg1_test"), id))
		if w.Code != tc.status {
			t.Fatalf("%v: %d want %d", tc.err, w.Code, tc.status)
		}
	}

	// RequiredAudioLanguageError surfaces its actionable message with 422.
	host, _ := testHost(t)
	host.Experience = frontendtest.Experience{ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{Title: frontend.Title{Key: req.Key, Type: "movie", Title: "Seoul Drift"}}, nil
	}}
	host.Capabilities = frontendtest.Capabilities{}
	langErr := &frontend.RequiredAudioLanguageError{Language: "English"}
	host.LazyStreams = &unavailableAdmission{err: langErr}
	id, _ := host.IDs.Compact(context.Background(), "xtream", "item", fmt.Sprint(core.MovieKey(1)))
	w := request(mount(t, host), fmt.Sprintf("/movie/%s/eg1_test/%d.mkv", frontend.XtreamUsername("eg1_test"), id))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), langErr.Error()) {
		t.Fatalf("xtream RequiredAudioLanguageError = %d %q, want 422 with %q", w.Code, w.Body.String(), langErr.Error())
	}
}

func TestXtreamSharedOriginPreservesFrontendPrefix(t *testing.T) {
	for _, base := range []string{"http://ether.test", "http://ether.test/xtream", "http://ether.test/proxy/xtream"} {
		host, _ := testHost(t)
		host.SurfaceURL = func(context.Context, *http.Request) (string, error) { return base, nil }
		inst := &instance{host: host}
		got := inst.base(httptest.NewRequest("GET", "/xtream/player_api.php", nil))
		want := base
		if !strings.HasSuffix(want, "/xtream") {
			want += "/xtream"
		}
		if got != want {
			t.Fatalf("base=%s got=%s want=%s", base, got, want)
		}
	}
}

func TestXtreamYouTubeTrailer(t *testing.T) {
	got := xtreamYouTubeTrailer([]frontend.Extra{
		{Kind: "featurette", Source: "YouTube", SourceID: "feat1"},
		{Kind: "trailer", Source: "YouTube", SourceID: "trl1"},
	})
	if got != "trl1" {
		t.Fatalf("got %q, want trl1", got)
	}
	if empty := xtreamYouTubeTrailer([]frontend.Extra{{Kind: "trailer"}}); empty != "" {
		t.Fatalf("got %q, want empty", empty)
	}
}

func TestArchiveReExport(t *testing.T) {
	host, _ := testHost(t)
	now := time.Date(2026, 11, 23, 14, 0, 0, 0, time.UTC)
	host.Now = func() time.Time { return now }
	host.LiveTV = frontendtest.LiveTV{
		ChannelsFunc: func(context.Context, *frontend.Viewer) ([]frontend.LiveChannel, error) {
			return []frontend.LiveChannel{{ID: "archived", Name: "Archive", Kind: "iptv", ArchiveDays: 2}}, nil
		},
		GuideFunc: func(_ context.Context, _ *frontend.Viewer, q frontend.LiveGuideQuery) ([]frontend.LiveProgramme, error) {
			if len(q.Channels) != 1 || q.Channels[0] != "archived" || !q.From.Equal(now.Add(-48*time.Hour)) {
				t.Fatal("unexpected archive guide query")
			}
			return []frontend.LiveProgramme{
				{ID: "expired", ChannelID: "archived", Start: now.Add(-49 * time.Hour), End: now.Add(-47 * time.Hour)},
				{ID: "past", ChannelID: "archived", Start: now.Add(-48 * time.Hour), End: now.Add(-47 * time.Hour)},
				{ID: "airing", ChannelID: "archived", Start: now.Add(-time.Hour), End: now.Add(time.Hour)},
			}, nil
		},
	}
	h := mount(t, host)
	w := request(h, "/player_api.php?"+authQuery()+"&action=get_live_streams")
	var streams []struct {
		ID      int    `json:"stream_id"`
		Archive int    `json:"tv_archive"`
		Days    int    `json:"tv_archive_duration"`
		Source  string `json:"direct_source"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &streams) != nil || len(streams) != 1 ||
		streams[0].Archive != 1 || streams[0].Days != 2 || streams[0].Source != "" {
		t.Fatal("unexpected live stream archive metadata")
	}
	w = request(h, "/player_api.php?"+authQuery()+"&action=get_simple_data_table&stream_id="+fmt.Sprint(streams[0].ID))
	var response struct {
		Listings []struct {
			ID         string `json:"id"`
			HasArchive int    `json:"has_archive"`
		} `json:"epg_listings"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &response) != nil || len(response.Listings) != 3 {
		t.Fatal("expected three guide rows")
	}
	for _, row := range response.Listings {
		want := 0
		if row.ID == "past" {
			want = 1
		}
		if row.HasArchive != want {
			t.Fatalf("incorrect archive flag for %s", row.ID)
		}
	}
}

type archiveHLSFixture struct {
	*frontendtest.CatchUpLive
	starts int
	served string
}

func (f *archiveHLSFixture) StartCatchUp(ctx context.Context, u *frontend.User, req frontend.CatchUpRequest, caps frontend.ClientCaps, session string) (frontend.CatchUpInfo, error) {
	f.starts++
	return f.CatchUpLive.StartCatchUp(ctx, u, req, caps, session)
}

func (f *archiveHLSFixture) LibraryCatchUpSession(access frontend.SessionAccess, id string) (frontend.CatchUpInfo, bool) {
	p := f.Item.Programme
	ok := id == "archive-session" && f.starts == 1 && access.Viewer.ID == 1 && access.Protocol == "xtream" && access.Device.ID != ""
	return frontend.CatchUpInfo{ID: id, ChannelID: p.ChannelID, Start: p.Start, End: p.End}, ok
}

func (f *archiveHLSFixture) ServeResource(w http.ResponseWriter, r *http.Request, _ string, file string, _ *frontend.User) {
	f.served = r.URL.Path
	_, _ = io.WriteString(w, file)
}

func TestArchiveHLSRoutesPartsOnTheSameSession(t *testing.T) {
	host, auth := testHost(t)
	live := &archiveHLSFixture{CatchUpLive: frontendtest.NewCatchUpLive()}
	p := live.Item.Programme
	host.Now = func() time.Time { return p.End.Add(time.Hour) }
	host.Capabilities = frontendtest.Capabilities{}
	live.LiveTV = frontendtest.LiveTV{
		ChannelsFunc: func(context.Context, *frontend.Viewer) ([]frontend.LiveChannel, error) {
			return []frontend.LiveChannel{{ID: p.ChannelID, Name: "Archive", Kind: "iptv", ArchiveDays: 2}}, nil
		},
	}
	host.LiveTV = live
	h := mount(t, host)
	w := request(h, "/player_api.php?"+authQuery()+"&action=get_live_streams")
	var streams []struct {
		ID int `json:"stream_id"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &streams) != nil || len(streams) != 1 {
		t.Fatal("failed to list archived channel")
	}
	archive := fmt.Sprintf("/timeshift/%s/eg1_test/60/%d/%d.m3u8",
		frontend.XtreamUsername("eg1_test"), p.Start.Unix(), streams[0].ID)
	w = request(h, archive)
	part := "?part=0.ts&session=archive-session"
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), part) || strings.Contains(w.Body.String(), "eg1_test") {
		t.Fatal("playlist did not contain a credential-free relative segment URI")
	}
	w = request(h, archive+part)
	if w.Code != http.StatusOK || live.starts != 1 || live.served != "/0.ts" {
		t.Fatal("segment did not use the authorized archive session")
	}
	auth.revoked = true
	if w = request(h, archive+part); w.Code != http.StatusUnauthorized {
		t.Fatalf("segment status after revocation = %d", w.Code)
	}
}

func TestXMLTVFailureIsAtomicAndGuideIsCached(t *testing.T) {
	host, _ := testHost(t)
	var calls int
	fail := true
	var revision uint64
	host.Now = func() time.Time { return time.Unix(1700000000, 0) }
	host.PageFreshness = func() frontend.PageFreshness { return frontend.PageFreshness{Safety: revision} }
	host.LiveTV = frontendtest.LiveTV{
		ChannelsFunc: func(context.Context, *frontend.Viewer) ([]frontend.LiveChannel, error) {
			channels := make([]frontend.LiveChannel, frontend.MaxLiveGuideExportChannels+1)
			for n := range channels {
				channels[n] = frontend.LiveChannel{ID: strconv.Itoa(n), Name: "News"}
			}
			return channels, nil
		},
		GuideFunc: func(_ context.Context, _ *frontend.Viewer, q frontend.LiveGuideQuery) ([]frontend.LiveProgramme, error) {
			calls++
			if fail && len(q.Channels) == 1 {
				return nil, fmt.Errorf("guide unavailable")
			}
			return nil, nil
		},
	}
	h := mount(t, host)
	path := "/xmltv.php?" + authQuery()
	if w := request(h, path); w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), xml.Header) {
		t.Fatalf("HTTP %d: XML prefix sent on failed guide query", w.Code)
	}
	fail = false
	w := request(h, path)
	var doc struct{ XMLName xml.Name }
	if w.Code != http.StatusOK || xml.Unmarshal(w.Body.Bytes(), &doc) != nil || doc.XMLName.Local != "tv" {
		t.Fatalf("HTTP %d: invalid XMLTV response", w.Code)
	}
	before := calls
	if again := request(h, path); again.Code != http.StatusOK || again.Body.String() != w.Body.String() || calls != before {
		t.Fatalf("duplicate export queried Guide: calls=%d, want %d", calls, before)
	}
	revision++
	if w := request(h, path); w.Code != http.StatusOK || calls != before+2 {
		t.Fatalf("freshness change did not reload Guide: calls=%d, want %d", calls, before+2)
	}
}
