package silo

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/conform/openapi"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

type fakeArtworkExperience = frontendtest.Experience
type relatedArtworkExperience struct {
	fakeArtworkExperience
	related func(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey) (frontend.RelatedArtwork, error)
}

func (e relatedArtworkExperience) RelatedArtwork(ctx context.Context, viewer *frontend.Viewer, dev frontend.Device, key frontend.RatingKey) (frontend.RelatedArtwork, error) {
	return e.related(ctx, viewer, dev, key)
}

func TestHomeNextUpReportsActualSource(t *testing.T) {
	viewer := &frontend.Viewer{ID: 5}
	next := frontend.Title{Key: 1000000000000001, Type: "episode", Title: "Next episode"}
	exp := frontendtest.Experience{
		RowsFunc: func(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
			return []frontend.Row{{ID: "next", Purpose: frontend.RowNextUp, Items: []frontend.Title{next}}}, nil
		},
		UserDataFunc: func(context.Context, *frontend.Viewer, []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
			return map[frontend.RatingKey]frontend.WatchStatus{}, nil
		},
	}
	host := frontend.Host{Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}, Experience: exp}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/v2/home/sections", nil)
	r.Header.Set("Authorization", "Bearer grant")
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"item_source":"next_up"`) {
		t.Fatalf("next-up source: %d %s", w.Code, w.Body.String())
	}
}

func TestTitleDTOReportsKnownMetadata(t *testing.T) {
	released := time.Date(2020, 2, 3, 0, 0, 0, 0, time.UTC)
	item := titleDTO(frontend.Title{Key: 1000000000000001, Type: "movie", Title: "Example", Language: "fr", ContentRating: "PG", Released: released}, frontend.WatchStatus{IsNotInterested: true})
	if item["original_language"] != "fr" || item["content_rating"] != "PG" || item["release_date"] != "2020-02-03" {
		t.Fatalf("metadata: %+v", item)
	}
	if state := item["user_state"].(map[string]any); state["not_interested"] != true {
		t.Fatalf("state: %+v", state)
	}
	unknown := titleDTO(frontend.Title{Key: 1000000000000002, Type: "movie", Title: "Unknown"}, frontend.WatchStatus{})
	if _, ok := unknown["added_at"]; ok {
		t.Fatal("invented added time")
	}
}

func TestCreditPortraitUsesTitleBoundGrant(t *testing.T) {
	key := frontend.RatingKey(1000000000000001)
	var issued frontend.ImageGrantRequest
	i := &instance{host: frontend.Host{ImageGrants: frontendtest.ImageGrants{IssueFunc: func(_ context.Context, req frontend.ImageGrantRequest) (string, error) {
		issued = req
		return "opaque-grant", nil
	}}}}
	r := httptest.NewRequest("GET", "/api/v2/catalog/items/x", nil)
	r.Header.Set("X-Silo-Device-Id", "tv")
	d := frontend.ItemDetail{Title: frontend.Title{Key: key}, ImagesAuthoritative: true, Images: []frontend.Image{{Kind: frontend.ImagePerson, Subject: "tmdb:12", Path: "/private", ThumbHash: "AQID"}}}
	got := i.creditPortraits(r, &frontend.Viewer{ID: 7}, "account-token", d)
	if got[12].url != "/api/v2/images/opaque-grant" || got[12].thumbHash != "AQID" || issued.Item != key || issued.Credential != "account-token" || strings.Contains(got[12].url, "account-token") {
		t.Fatalf("portrait grant: %v %+v", got, issued)
	}
	d.ImagesAuthoritative = false
	if len(i.creditPortraits(r, &frontend.Viewer{ID: 7}, "account-token", d)) != 0 {
		t.Fatal("issued portrait for non-authoritative artwork")
	}
}

func TestArtworkThumbhashMatchesGrantedImage(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	key := frontend.RatingKey(1000000000000001)
	exp := frontendtest.Experience{ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
		if req.Viewer != viewer || req.Key != key {
			return frontend.ItemDetail{}, errors.New("unauthorized")
		}
		return frontend.ItemDetail{ImagesAuthoritative: true, Images: []frontend.Image{
			{Kind: "poster", Path: "/first"},
			{Kind: "poster", Path: "/second", ThumbHash: "second-hash"},
			{Kind: "backdrop", Path: "/backdrop", ThumbHash: "AQID"},
		}}, nil
	}}
	grants := frontendtest.ImageGrants{IssueFunc: func(_ context.Context, req frontend.ImageGrantRequest) (string, error) {
		return strings.TrimPrefix(req.Image.Path, "/"), nil
	}}
	i := &instance{host: frontend.Host{Experience: exp, ImageGrants: grants}}
	out := map[string]any{}
	i.artwork(httptest.NewRequest("GET", "/", nil), viewer, "token", key, out)
	if out["poster_url"] != "/api/v2/images/first" || out["backdrop_url"] != "/api/v2/images/backdrop" || out["backdrop_thumbhash"] != "AQID" {
		t.Fatalf("artwork: %+v", out)
	}
	if _, ok := out["poster_thumbhash"]; ok {
		t.Fatalf("used hash from a different poster: %+v", out)
	}
}

func TestRelatedArtworkUsesAuthorizedOwnerGrants(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	const episode, season, series frontend.RatingKey = 10, 20, 30
	allowed, authoritative := true, true
	exp := relatedArtworkExperience{fakeArtworkExperience: frontendtest.Experience{ItemFunc: func(_ context.Context, req frontend.ItemRequest) (frontend.ItemDetail, error) {
		if req.Viewer != viewer || req.Key != episode {
			return frontend.ItemDetail{}, errors.New("unauthorized")
		}
		return frontend.ItemDetail{Title: frontend.Title{Key: episode, Type: "episode"}}, nil
	}}, related: func(_ context.Context, v *frontend.Viewer, d frontend.Device, key frontend.RatingKey) (frontend.RelatedArtwork, error) {
		if !allowed || v != viewer || d.ID != "tv" || key != episode {
			return frontend.RelatedArtwork{}, errors.New("unauthorized")
		}
		return frontend.RelatedArtwork{PeopleAuthoritative: true, People: []frontend.Image{{Kind: frontend.ImagePerson, Subject: "tmdb:12", Path: "/person", ThumbHash: "person-hash"}},
			Season: &frontend.ArtworkOwner{Item: season, Authoritative: authoritative, Images: []frontend.Image{{Kind: "poster", Path: "/season", ThumbHash: "season-hash"}}},
			Series: &frontend.ArtworkOwner{Item: series, Authoritative: authoritative, Images: []frontend.Image{{Kind: "poster", Path: "/series", ThumbHash: "series-hash"}, {Kind: "backdrop", Path: "/backdrop"}}}}, nil
	}}
	var owners []frontend.RatingKey
	grants := frontendtest.ImageGrants{IssueFunc: func(_ context.Context, req frontend.ImageGrantRequest) (string, error) {
		owners = append(owners, req.Item)
		return strings.TrimPrefix(req.Image.Path, "/"), nil
	}}
	i := &instance{host: frontend.Host{Experience: exp, ImageGrants: grants}}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Silo-Device-Id", "tv")
	out := map[string]any{}
	i.artwork(r, viewer, "credential", episode, out)
	if out["season_poster_url"] != "/api/v2/images/season" || out["season_poster_thumbhash"] != "season-hash" || out["series_poster_thumbhash"] != "series-hash" || out["series_backdrop_url"] != "/api/v2/images/backdrop" {
		t.Fatalf("parent artwork: %+v", out)
	}
	if _, ok := out["series_backdrop_thumbhash"]; ok {
		t.Fatal("invented cold hash")
	}
	if len(owners) != 3 || owners[0] != season || owners[1] != series || owners[2] != series {
		t.Fatalf("wrong grant owners: %+v", owners)
	}
	portraits := i.creditPortraits(r, viewer, "credential", frontend.ItemDetail{Title: frontend.Title{Key: episode}})
	if portraits[12].url != "/api/v2/images/person" || portraits[12].thumbHash != "person-hash" || owners[len(owners)-1] != episode {
		t.Fatalf("person grant: %+v owners=%+v", portraits, owners)
	}
	authoritative = false
	out = map[string]any{}
	i.artwork(r, viewer, "credential", episode, out)
	if len(out) != 0 {
		t.Fatalf("incomplete parent art exposed: %+v", out)
	}
	allowed = false
	out = map[string]any{}
	i.artwork(r, viewer, "credential", episode, out)
	if len(out) != 0 {
		t.Fatalf("unauthorized parent art exposed: %+v", out)
	}
}

func TestRecentlyAddedHomeRowKeepsLibrarySectionType(t *testing.T) {
	viewer := &frontend.Viewer{ID: 5}
	key := frontend.RatingKey(1000000000000001)
	exp := frontendtest.Experience{
		RowsFunc: func(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
			return []frontend.Row{{ID: "library.recent.1", Purpose: frontend.RowRecentlyAdded, Title: "Recently Added Movies", Items: []frontend.Title{{Key: key, Type: "movie", Title: "Visible"}}}}, nil
		},
		UserDataFunc: func(context.Context, *frontend.Viewer, []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
			return map[frontend.RatingKey]frontend.WatchStatus{}, nil
		},
	}
	host := frontend.Host{Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}, Experience: exp}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/v2/home/sections", nil)
	r.Header.Set("Authorization", "Bearer grant")
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"section_type":"recently_added"`) || !strings.Contains(w.Body.String(), `"title":"Visible"`) {
		t.Fatalf("recent row: %d %s", w.Code, w.Body.String())
	}
}

func TestEmptyHomeUsesAuthorizedBrowseFallback(t *testing.T) {
	v := &frontend.Viewer{ID: 5}
	visible, excluded := frontend.RatingKey(1000000000000001), frontend.RatingKey(1000000000000002)
	exp := frontendtest.Experience{
		ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "1", Name: "Movies"}}, nil
		},
		LibraryItemsFunc: func(_ context.Context, viewer *frontend.Viewer, _ frontend.Device, library string, _ frontend.ItemQuery) (frontend.Page, error) {
			if viewer != v || library != "1" {
				t.Fatal("incorrect library policy context")
			}
			return frontend.Page{Items: []frontend.Title{{Key: visible, Type: "movie", Title: "Visible"}, {Key: excluded, Type: "movie", Title: "Excluded"}}}, nil
		},
		UserDataFunc: func(context.Context, *frontend.Viewer, []frontend.RatingKey) (map[frontend.RatingKey]frontend.WatchStatus, error) {
			return map[frontend.RatingKey]frontend.WatchStatus{excluded: {IsNotInterested: true}}, nil
		},
	}
	host := frontend.Host{Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return v, nil }}, Experience: exp}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/v2/home/sections", nil)
	req.Header.Set("Authorization", "Bearer grant")
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"title":"Visible"`) || strings.Contains(w.Body.String(), `"title":"Excluded"`) {
		t.Fatalf("fallback: %d %s", w.Code, w.Body.String())
	}
}

func TestBrowseAndMutationsUseViewer(t *testing.T) {
	viewer := &frontend.Viewer{ID: 5}
	key := frontend.RatingKey(1000000000000001)
	var played frontend.RatingKey
	var excluded bool
	ident := frontendtest.Identity{ResolveFunc: func(_ context.Context, s string) (*frontend.Viewer, error) {
		if s != "grant" {
			return nil, errors.New("bad")
		}
		return viewer, nil
	}}
	exp := frontendtest.Experience{ViewsFunc: func(_ context.Context, v *frontend.Viewer) ([]frontend.View, error) {
		if v.ID != 5 {
			t.Fatal("viewer")
		}
		return []frontend.View{{ID: "1", Name: "Movies", Type: "movie"}}, nil
	}, RowsFunc: func(_ context.Context, r frontend.RowRequest) ([]frontend.Row, error) {
		if r.Viewer.ID != 5 {
			t.Fatal("viewer")
		}
		return []frontend.Row{{ID: "continue", Purpose: frontend.RowContinue, Title: "Keep watching", Items: []frontend.Title{{Key: key, Type: "movie", Title: "Example"}}}}, nil
	}, ItemFunc: func(_ context.Context, r frontend.ItemRequest) (frontend.ItemDetail, error) {
		if r.Key != key {
			return frontend.ItemDetail{}, errors.New("not visible")
		}
		return frontend.ItemDetail{Title: frontend.Title{Key: key, Title: "Example", Type: "movie"}}, nil
	}, SetPlayedFunc: func(_ context.Context, v *frontend.Viewer, k frontend.RatingKey) error {
		if v.ID != 5 {
			t.Fatal("viewer")
		}
		played = k
		return nil
	}, SetNotInterestedFunc: func(_ context.Context, v *frontend.Viewer, k frontend.RatingKey, on bool) error {
		if v.ID != 5 || k != key {
			t.Fatal("wrong exclusion owner or item")
		}
		excluded = on
		return nil
	}}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer grant")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	if w := call("GET", "/api/v2/user/libraries"); w.Code != 200 || !strings.Contains(w.Body.String(), "Movies") {
		t.Fatalf("views %d %s", w.Code, w.Body.String())
	}
	if w := call("GET", "/api/v2/home/sections"); w.Code != 200 || !strings.Contains(w.Body.String(), "continue_watching") || !strings.Contains(w.Body.String(), "Example") {
		t.Fatalf("home %d %s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/api/v2/user/libraries", "/api/v2/home/sections"} {
		w := call("GET", path)
		spec, e := openapi.CachedSpec("../../conform/specs/manifest.json", "silo", "")
		if e == nil {
			doc, e := openapi.Parse(spec)
			if e != nil {
				t.Fatal(e)
			}
			if e = doc.Validate("GET", path, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); e != nil {
				t.Errorf("%s spec: %v", path, e)
			}
		} else if !os.IsNotExist(e) {
			t.Fatal(e)
		}
	}
	detailPath := "/api/v2/catalog/items/" + ids.RatingKeyGUID(key)
	dw := call("GET", detailPath)
	if dw.Code != 200 {
		t.Fatalf("detail %d %s", dw.Code, dw.Body.String())
	}
	spec, e := openapi.CachedSpec("../../conform/specs/manifest.json", "silo", "")
	if e == nil {
		doc, e := openapi.Parse(spec)
		if e != nil {
			t.Fatal(e)
		}
		if e = doc.Validate("GET", detailPath, dw.Code, dw.Header().Get("Content-Type"), dw.Body.Bytes()); e != nil {
			t.Errorf("detail spec: %v", e)
		}
	} else if !os.IsNotExist(e) {
		t.Fatal(e)
	}

	if w := call("POST", "/api/v2/watched/"+ids.RatingKeyGUID(key)); w.Code != 204 || played != key {
		t.Fatalf("played %d %d", w.Code, played)
	}
	if w := call("POST", "/api/v2/watched/"+ids.RatingKeyGUID(key+1)); w.Code != 404 || played != key {
		t.Fatalf("hidden item %d", w.Code)
	}
	if w := call("PUT", "/api/v2/not-interested/"+ids.RatingKeyGUID(key)); w.Code != 204 || !excluded {
		t.Fatalf("exclude %d %v", w.Code, excluded)
	}
	if w := call("DELETE", "/api/v2/not-interested/"+ids.RatingKeyGUID(key)); w.Code != 204 || excluded {
		t.Fatalf("undo %d %v", w.Code, excluded)
	}
	if w := call("PUT", "/api/v2/not-interested/"+ids.RatingKeyGUID(key+1)); w.Code != 404 || excluded {
		t.Fatalf("hidden exclusion %d", w.Code)
	}
}

func TestDetailWithEstimatedVersionSpec(t *testing.T) {
	v := &frontend.Viewer{ID: 1}
	hdr10Plus := true
	knownPresent, progressive := true, false
	partCount := 1
	media := frontend.MediaInfo{Container: "mp4", Duration: time.Minute, BitrateKbps: 1000, Streams: []frontend.StreamInfo{{Type: "video", Codec: "h264", Width: 1920, Height: 1080, FrameRate: 24, ProbedInterlaced: &progressive, PixelFormat: "yuv420p", ColorTransfer: "smpte2084", HDR10Plus: &hdr10Plus, DolbyVisionConfigurationPresent: &knownPresent, DolbyVisionBLCompatibilityIDPresent: &knownPresent, DolbyVisionCompatibilityID: 1}, {Type: "audio", Codec: "aac", Language: "en", Channels: 2, Default: true}}}
	key := frontend.RatingKey(1000000000000001)
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return v, nil }}
	exp := frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{Title: frontend.Title{Key: key, Type: "movie", Title: "Film", Released: time.Unix(10000, 0)}, Versions: []frontend.Version{{ID: "cut", Name: "release label", FileName: "real.mp4", AddedAt: time.Unix(10000, 0), Media: media, PartCount: &partCount}}, Markers: []frontend.Marker{{Type: "intro", Start: time.Second, End: 10 * time.Second}}, Extras: []frontend.Extra{{Title: "Trailer", Kind: "trailer", Source: "YouTube", SourceID: "clip", Language: "en"}}}, nil
	}}
	sess := frontendtest.Sessions{PlanFunc: func(_ context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
		if !r.Intent.Estimate {
			t.Fatal("resolved detail")
		}
		return frontend.PlayPlan{Estimated: true, Media: media}, nil
	}}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp, Sessions: sess, SourceOffers: testSourceOffers{}, VersionHandles: testVersionHandles{}, Capabilities: frontendtest.Capabilities{}, IDs: ids.Codecs{Allocator: fileIDs{}}})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v2/catalog/items/" + ids.RatingKeyGUID(key)
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("Authorization", "Bearer grant")
	r.Header.Set("X-Silo-Device-Id", "tv")
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"audio_tracks"`) || !strings.Contains(w.Body.String(), `"language":"en"`) {
		t.Fatalf("missing real audio tracks: %s", w.Body.String())
	}
	for _, fact := range []string{`"video_tracks"`, `"frame_rate":"24.000"`, `"interlaced":false`, `"pixel_format":"yuv420p"`, `"site_key":"clip"`, `"file_name":"real.mp4"`, `"total_duration":60`, `"overlay_summary"`, `"resolution":"1080p"`, `"hdr":"Dolby Vision"`, `"kind":"trailer"`} {
		if !strings.Contains(w.Body.String(), fact) {
			t.Errorf("missing known detail %s: %s", fact, w.Body.String())
		}
	}
	if strings.Contains(w.Body.String(), `"file_name":"release label"`) {
		t.Error("release label exposed as a filename")
	}
	// Theme songs need an owned audio file (id, duration, container). Ether
	// stores no theme-song assets, so the field stays absent.
	if strings.Contains(w.Body.String(), `"themes"`) {
		t.Error("invented a theme song")
	}
	knownPresent = false
	negative := httptest.NewRecorder()
	inst.Handler().ServeHTTP(negative, r)
	if negative.Code != 200 || !strings.Contains(negative.Body.String(), `"dv_config_present":false`) || !strings.Contains(negative.Body.String(), `"dv_bl_compat_id_present":false`) {
		t.Fatalf("known negative probe facts: %d %s", negative.Code, negative.Body.String())
	}
	watchPath := "/api/v2/watch/" + ids.RatingKeyGUID(key)
	req := httptest.NewRequest("GET", watchPath, nil)
	req.Header.Set("Authorization", "Bearer grant")
	req.Header.Set("X-Silo-Device-Id", "tv")
	rec := httptest.NewRecorder()
	inst.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"bitrate":1000000`) {
		t.Errorf("watch bitrate units: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"kind":"intro"`) || !strings.Contains(rec.Body.String(), `"start_seconds":1`) || !strings.Contains(rec.Body.String(), `"end_seconds":10`) {
		t.Errorf("single probed file markers: %s", rec.Body.String())
	}
	spec, e := openapi.CachedSpec("../../conform/specs/manifest.json", "silo", "")
	if e == nil {
		doc, e := openapi.Parse(spec)
		if e != nil {
			t.Fatal(e)
		}
		if e = doc.Validate("GET", path, w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()); e != nil {
			t.Errorf("detail spec: %v", e)
		}
		if e = doc.Validate("GET", watchPath, rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes()); e != nil {
			t.Errorf("watch spec: %v", e)
		}

	} else if !os.IsNotExist(e) {
		t.Fatal(e)
	}
}

func TestUnresolvedColdTitleDoesNotInventAPlayableSource(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	key := frontend.RatingKey(1000000000000001)
	calls := 0
	exp := frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{Title: frontend.Title{Key: key, Type: "movie"}}, nil
	}}
	sessions := frontendtest.Sessions{PlanFunc: func(context.Context, frontend.PlanRequest) (frontend.PlayPlan, error) {
		calls++
		return frontend.PlayPlan{}, nil
	}}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: testDeviceIdentity(frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}, viewer), Experience: exp, Sessions: sessions, SourceOffers: testSourceOffers{}, Capabilities: frontendtest.Capabilities{}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/v2/watch/"+ids.RatingKeyGUID(key), nil)
	r.Header.Set("Authorization", "Bearer grant")
	r.Header.Set("X-Silo-Device-Id", "tv")
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || calls != 0 || !strings.Contains(w.Body.String(), `"versions":[]`) {
		t.Fatalf("invented source: %d %s calls=%d", w.Code, w.Body.String(), calls)
	}
}
