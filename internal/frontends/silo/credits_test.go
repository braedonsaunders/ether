package silo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func TestKnownCreditMetadataUsesScopedIDs(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	key := frontend.RatingKey(1000000000000001)
	known := true
	exp := frontendtest.Experience{ItemFunc: func(context.Context, frontend.ItemRequest) (frontend.ItemDetail, error) {
		title := frontend.Title{Key: key, Type: "show", Title: "Series", Cast: []frontend.Person{{TMDB: 1, Name: "Actor"}}, Crew: []frontend.Person{{TMDB: 2, Name: "Director", Role: "Director"}}}
		if known {
			title.LastAirDate = time.Date(2025, 4, 3, 0, 0, 0, 0, time.UTC)
			title.Cast[0].IMDB = "nm123"
			title.Crew[0].IMDB = "nm456"
		}
		return frontend.ItemDetail{Title: title}, nil
	}, PersonFacetIDsFunc: func(_ context.Context, v *frontend.Viewer, dev frontend.Device, item frontend.RatingKey, wanted []int64) (map[int64]string, error) {
		if v != viewer || dev.ID != "tv" || item != key || len(wanted) != 2 {
			t.Fatalf("unscoped credits %v %+v %v %v", v, dev, item, wanted)
		}
		if !known {
			return nil, errors.New("policy changed")
		}
		return map[int64]string{1: "actor-scope", 2: "director-scope"}, nil
	}}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return viewer, nil }}, Experience: exp})
	if err != nil {
		t.Fatal(err)
	}
	for _, visible := range []bool{true, false} {
		known = visible
		r := httptest.NewRequest("GET", "/api/v2/catalog/items/"+ids.RatingKeyGUID(key), nil)
		r.Header.Set("Authorization", "Bearer grant")
		r.Header.Set("X-Silo-Device-Id", "tv")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		var body map[string]any
		if err = json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		cast := body["cast"].([]any)[0].(map[string]any)
		crew := body["crew"].([]any)[0].(map[string]any)
		if visible {
			if body["last_air_date"] != "2025-04-03" || cast["person_id"] != "actor-scope" || cast["imdb_id"] != "nm123" || crew["person_id"] != "director-scope" || crew["imdb_id"] != "nm456" {
				t.Fatalf("known metadata %v %v %v", body["last_air_date"], cast, crew)
			}
		} else {
			for _, credit := range []map[string]any{cast, crew} {
				if _, ok := credit["person_id"]; ok {
					t.Fatal("failed policy exposed person ID")
				}
				if _, ok := credit["imdb_id"]; ok {
					t.Fatal("unknown IMDb invented")
				}
			}
			if _, ok := body["last_air_date"]; ok {
				t.Fatal("unknown last-air date invented")
			}
		}
	}
}

func TestCreditIDsBatchAllKnownCredits(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	detail := frontend.ItemDetail{Title: frontend.Title{Key: 1000000000000001}}
	for id := int64(1); id <= 300; id++ {
		detail.Title.Cast = append(detail.Title.Cast, frontend.Person{TMDB: id})
	}
	detail.Title.Crew = []frontend.Person{{TMDB: 1}, {TMDB: 0}}
	calls := 0
	i := &instance{host: frontend.Host{Experience: frontendtest.Experience{PersonFacetIDsFunc: func(_ context.Context, v *frontend.Viewer, d frontend.Device, key frontend.RatingKey, wanted []int64) (map[int64]string, error) {
		calls++
		if v != viewer || d.ID != "tv" || key != detail.Title.Key || len(wanted) > 256 {
			t.Fatal("invalid batch scope")
		}
		out := map[int64]string{9999: "foreign"}
		for _, id := range wanted {
			out[id] = "known"
		}
		return out, nil
	}}}}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Silo-Device-Id", "tv")
	got := i.creditIDs(r, viewer, detail)
	if calls != 2 || len(got) != 300 || got[9999] != "" {
		t.Fatalf("calls=%d credits=%d", calls, len(got))
	}
}
