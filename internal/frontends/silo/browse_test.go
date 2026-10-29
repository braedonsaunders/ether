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
	key := frontend.RatingKey(1000000000000001)
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return v, nil }}
	exp := frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{Title: frontend.Title{Key: key, Type: "movie", Title: "Film", Released: time.Unix(10000, 0)}, Extras: []frontend.Extra{{Title: "Trailer", Kind: "trailer", Source: "YouTube", SourceID: "clip", Language: "en"}}}, nil
	}}
	sess := frontendtest.Sessions{PlanFunc: func(_ context.Context, r frontend.PlanRequest) (frontend.PlayPlan, error) {
		if !r.Intent.Estimate {
			t.Fatal("resolved detail")
		}
		return frontend.PlayPlan{Estimated: true, Media: frontend.MediaInfo{Container: "mp4", Duration: time.Minute, Streams: []frontend.StreamInfo{{Type: "video", Codec: "h264", Width: 1920, Height: 1080, FrameRate: 24, Interlaced: true, ColorTransfer: "smpte2084", HDR10Plus: &hdr10Plus, DolbyVisionVersion: "1.0", DolbyVisionCompatibilityID: 1}, {Type: "audio", Codec: "aac", Language: "en", Channels: 2, Default: true}}}}, nil
	}}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp, Sessions: sess, Capabilities: frontendtest.Capabilities{}, IDs: ids.Codecs{Allocator: fileIDs{}}})
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
	for _, fact := range []string{`"video_tracks"`, `"frame_rate":"24.000"`, `"site_key":"clip"`} {
		if !strings.Contains(w.Body.String(), fact) {
			t.Errorf("missing known detail %s: %s", fact, w.Body.String())
		}
	}
	if strings.Contains(w.Body.String(), `"total_duration"`) {
		t.Errorf("estimate advertised as exact part duration: %s", w.Body.String())
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
		watchPath := "/api/v2/watch/" + ids.RatingKeyGUID(key)
		req := httptest.NewRequest("GET", watchPath, nil)
		req.Header.Set("Authorization", "Bearer grant")
		req.Header.Set("X-Silo-Device-Id", "tv")
		rec := httptest.NewRecorder()
		inst.Handler().ServeHTTP(rec, req)
		if e = doc.Validate("GET", watchPath, rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes()); e != nil {
			t.Errorf("watch spec: %v", e)
		}

	} else if !os.IsNotExist(e) {
		t.Fatal(e)
	}
}
