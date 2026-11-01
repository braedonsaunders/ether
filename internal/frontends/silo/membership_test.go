package silo

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

type membershipExperience struct {
	frontendtest.Experience
	favorite  func(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey) (frontend.Membership, error)
	watchlist func(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey) (frontend.Membership, error)
}

func (e membershipExperience) FavoriteItem(ctx context.Context, v *frontend.Viewer, d frontend.Device, k frontend.RatingKey) (frontend.Membership, error) {
	return e.favorite(ctx, v, d, k)
}
func (e membershipExperience) WatchlistItem(ctx context.Context, v *frontend.Viewer, d frontend.Device, k frontend.RatingKey) (frontend.Membership, error) {
	return e.watchlist(ctx, v, d, k)
}

func TestMembershipReadsStoredTimeAndKeepsAbsenceDistinct(t *testing.T) {
	v := &frontend.Viewer{ID: 7}
	key := frontend.RatingKey(1000000000000001)
	at := time.Date(2026, 1, 2, 3, 4, 5, 123000000, time.UTC)
	present := false
	exp := membershipExperience{
		favorite: func(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey) (frontend.Membership, error) {
			if !present {
				return frontend.Membership{}, nil
			}
			return frontend.Membership{Present: true, AddedAt: &at}, nil
		},
		watchlist: func(context.Context, *frontend.Viewer, frontend.Device, frontend.RatingKey) (frontend.Membership, error) {
			return frontend.Membership{}, frontend.ErrNotFound
		},
	}
	host := frontend.Host{Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return v, nil }}, Experience: exp, IDs: ids.Codecs{Allocator: fileIDs{}}}
	inst, err := (Frontend{MachineID: "test"}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	call := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer grant")
		r.Header.Set("X-Silo-Device-Id", "tv")
		w := httptest.NewRecorder()
		inst.Handler().ServeHTTP(w, r)
		return w
	}
	id := ids.RatingKeyGUID(key)
	if w := call("/api/v2/favorites/" + id); w.Code != 404 {
		t.Fatalf("absent favorite: %d %s", w.Code, w.Body.String())
	}
	if w := call("/api/v2/watchlist/" + id); w.Code != 404 {
		t.Fatalf("hidden watchlist: %d %s", w.Code, w.Body.String())
	}
	present = true
	w := call("/api/v2/favorites/" + id)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"added_at":"2026-01-02T03:04:05.123Z"`) || !strings.Contains(w.Body.String(), `"content_id":"`+id+`"`) {
		t.Fatalf("stored membership time: %d %s", w.Code, w.Body.String())
	}
	bare := frontend.Host{Identity: host.Identity, Experience: frontendtest.Experience{}, IDs: host.IDs}
	plain, err := (Frontend{MachineID: "test"}).Mount(context.Background(), bare)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/v2/favorites/"+id, nil)
	r.Header.Set("Authorization", "Bearer grant")
	r.Header.Set("X-Silo-Device-Id", "tv")
	missing := httptest.NewRecorder()
	plain.Handler().ServeHTTP(missing, r)
	if missing.Code != 503 {
		t.Fatalf("missing reader reported absence: %d %s", missing.Code, missing.Body.String())
	}
}
