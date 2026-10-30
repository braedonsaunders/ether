package watchlist

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

type failingCatalog struct{ fakeCatalog }

func (failingCatalog) Get(context.Context, core.RatingKey) (*core.Title, error) {
	return nil, errors.New("temporary metadata outage")
}

func TestPlexSyncPreservesSnapshotOnInvalidEnvelopeOrMetadataOutage(t *testing.T) {
	for _, badEnvelope := range []bool{true, false} {
		name := "metadata_outage"
		if badEnvelope {
			name = "invalid_envelope"
		}
		t.Run(name, func(t *testing.T) {
			s, _, _, _, source, _ := setup(t)
			ctx := context.Background()
			user := &core.User{ID: 1}
			if err := s.upsert(ctx, user.ID, "plex", source.titles[0]); err != nil {
				t.Fatal(err)
			}
			body := `{"MediaContainer":{"size":1,"totalSize":1,"Metadata":[{"type":"movie","Guid":[{"id":"tmdb://1"}]}]}}`
			if badEnvelope {
				body = `{"error":"upstream temporarily unavailable"}`
			} else {
				s.o.Catalog = failingCatalog{}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer server.Close()
			s.o.PlexBaseURL = server.URL
			s.o.Tokens = func(context.Context, int64) (string, error) { return "fixture", nil }
			if err := s.SaveSettings(ctx, Settings{Enabled: true, Plex: true, CheckHours: 6}); err != nil {
				t.Fatal(err)
			}
			if err := s.Sync(ctx, user); err == nil {
				t.Error("failed snapshot treated as authoritative")
			}
			items, err := s.Items(ctx, user)
			if err != nil || len(items) != 1 {
				t.Fatalf("last good snapshot lost: %+v %v", items, err)
			}
		})
	}
}

func TestPlexSyncFollowsShortPagesWhenTotalIsKnown(t *testing.T) {
	s, _, _, _, source, _ := setup(t)
	ctx := context.Background()
	title := source.titles[0]
	other := title
	other.Key = core.ShowKey(9)
	other.Type = core.TypeShow
	s.o.Catalog = fakeCatalog{titles: map[core.RatingKey]core.Title{title.Key: title, other.Key: other}}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// Both key orders occur: the scalar must never decode into Guid.
		item := `{"guid":"plex://movie/example","type":"movie","Guid":[{"id":"tmdb://1"}]}`
		if r.URL.Query().Get("X-Plex-Container-Start") == "1" {
			item = `{"Guid":[{"id":"tmdb://9"}],"type":"show","guid":"plex://show/example"}`
		}
		fmt.Fprintf(w, `{"MediaContainer":{"size":1,"totalSize":2,"Metadata":[%s]}}`, item)
	}))
	defer server.Close()
	s.o.PlexBaseURL = server.URL
	s.o.Tokens = func(context.Context, int64) (string, error) { return "fixture", nil }
	if err := s.SaveSettings(ctx, Settings{Enabled: true, Plex: true, CheckHours: 6}); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(ctx, &core.User{ID: 1}); err != nil {
		t.Fatal(err)
	}
	items, err := s.Items(ctx, &core.User{ID: 1})
	if err != nil || len(items) != 2 || calls != 2 {
		t.Fatalf("short page truncated watchlist: items=%d calls=%d err=%v", len(items), calls, err)
	}
}
