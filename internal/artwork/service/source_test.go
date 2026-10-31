package service

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

type storedArtwork map[core.RatingKey]core.Title

func (s storedArtwork) StoredTitle(_ context.Context, k core.RatingKey) (*core.Title, error) {
	t, ok := s[k]
	if !ok {
		return nil, core.ErrNotFound
	}
	return &t, nil
}

type artworkPolicy struct{ viewer *core.User }

func (p *artworkPolicy) UserPolicy(*core.User) *core.User         { return p.viewer }
func (p *artworkPolicy) Allowed(_ *core.User, t *core.Title) bool { return !t.Adult }

type imageTransport func(*http.Request) (*http.Response, error)

func (f imageTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestListChecksVisibilityOnce(t *testing.T) {
	ctx := context.Background()
	key := core.MovieKey(550)
	catalog := storedArtwork{key: {Key: key, Type: core.TypeMovie, PosterPath: "/poster550.jpg", BackdropPath: "/back.jpg", LogoPath: "/logo.jpg", Released: time.Unix(1, 0)}}
	viewer := &core.User{ID: 7, IsOwner: true}
	var checks int
	source := Source{
		Catalog: catalog,
		Visibility: experience.Visibility{
			Policy: &artworkPolicy{viewer: viewer},
			FilterAvailable: func(context.Context, []core.Title) []core.Title {
				checks++
				return []core.Title{{}}
			},
		},
		Variants: func(*core.Title, string) (*tmdb.Images, bool) { return nil, false },
	}
	if _, _, err := source.List(ctx, frontend.ArtworkRequest{Viewer: viewer, Item: key}); err != nil {
		t.Fatal(err)
	}
	if checks != 1 {
		t.Fatalf("visibility checks %d, want 1", checks)
	}
}

func TestNeutralArtworkColdSelectionRevocationAndReferences(t *testing.T) {
	ctx := context.Background()
	key := core.MovieKey(550)
	catalog := storedArtwork{key: {Key: key, Type: core.TypeMovie, PosterPath: "/poster550.jpg", Released: time.Unix(1, 0)}}
	viewer := &core.User{ID: 7, IsOwner: true}
	policy := &artworkPolicy{viewer: viewer}
	var fetched []string
	images := NewRemoteCache(1024, &http.Client{Transport: imageTransport(func(r *http.Request) (*http.Response, error) {
		fetched = append(fetched, r.URL.Path)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/jpeg"}}, Body: io.NopCloser(strings.NewReader("poster-bytes"))}, nil
	})})
	source := Source{Catalog: catalog, Visibility: experience.Visibility{Policy: policy}, Images: images, ImageURL: func(path, size string) string { return "https://fixture.invalid/" + size + path }, Variants: func(*core.Title, string) (*tmdb.Images, bool) { return nil, false }}
	req := frontend.ArtworkRequest{Viewer: viewer, Item: key}
	list, complete, err := source.List(ctx, req)
	if err != nil || !complete || len(list) == 0 {
		t.Fatalf("cold selection: %+v %v %v", list, complete, err)
	}
	if len(fetched) != 0 {
		t.Fatal("enumeration fetched bytes")
	}
	poster := list[0]
	if strings.Contains(poster.Path, "poster550") {
		t.Fatal("opaque path leaked source")
	}
	got, ct, err := source.Read(ctx, req, poster, frontend.ArtworkTransform{Width: 2000})
	if err != nil || ct != "image/jpeg" || !bytes.Equal(got, []byte("poster-bytes")) || fetched[0] != "/original/poster550.jpg" {
		t.Fatalf("read: %s %q %v %+v", ct, got, err, fetched)
	}
	title := catalog[key]
	title.PosterPath = "/new.jpg"
	catalog[key] = title
	if _, _, err := source.Read(ctx, req, poster, frontend.ArtworkTransform{}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("stale reference: %v", err)
	}
	title.Adult = true
	catalog[key] = title
	if _, _, err := source.List(ctx, req); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("hidden title: %v", err)
	}
	policy.viewer = nil
	if _, _, err := source.Read(ctx, req, poster, frontend.ArtworkTransform{}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("revoked viewer: %v", err)
	}
	if len(fetched) != 1 {
		t.Fatal("rejected request touched remote images")
	}
}

type refreshingArtwork struct {
	storedArtwork
	fresh core.Title
}

func (s refreshingArtwork) RefreshStoredTitle(context.Context, core.RatingKey) (*core.Title, error) {
	title := s.fresh
	return &title, nil
}

func TestStalePortraitCacheFetchesTheCurrentThumb(t *testing.T) {
	ctx := context.Background()
	key := core.MovieKey(550)
	stale := core.Title{Key: key, Type: core.TypeMovie, Released: time.Unix(1, 0)}
	fresh := stale
	fresh.Cast = []core.Person{{Name: "Actor", TMDB: 42, ThumbPath: "/actor.jpg"}}
	viewer := &core.User{ID: 7, IsOwner: true}
	var fetched []string
	images := NewRemoteCache(1024, &http.Client{Transport: imageTransport(func(r *http.Request) (*http.Response, error) {
		fetched = append(fetched, r.URL.Path)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/jpeg"}}, Body: io.NopCloser(strings.NewReader("portrait-bytes"))}, nil
	})})
	source := Source{Catalog: refreshingArtwork{storedArtwork: storedArtwork{key: stale}, fresh: fresh}, Visibility: experience.Visibility{Policy: &artworkPolicy{viewer: viewer}}, Images: images, ImageURL: func(path, size string) string { return "https://fixture.invalid/" + size + path }}
	req := frontend.ArtworkRequest{Viewer: viewer, Item: key}
	listed, err := source.PersonImages(ctx, req, fresh)
	if err != nil || len(listed) != 1 {
		t.Fatalf("descriptor: %+v %v", listed, err)
	}
	got, ct, err := source.Read(ctx, req, listed[0], frontend.ArtworkTransform{})
	if err != nil || ct != "image/jpeg" || !bytes.Equal(got, []byte("portrait-bytes")) || len(fetched) != 1 || !strings.Contains(fetched[0], "actor.jpg") {
		t.Fatalf("on-demand portrait: %s %q %v %+v", ct, got, err, fetched)
	}
}

func TestCrewPortraitHashIsReadyOnList(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := store.OpenTest(t)
	img := image.NewNRGBA(image.Rect(0, 0, 8, 6))
	img.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(10378)
	title := core.Title{Key: key, Type: core.TypeMovie, Released: time.Unix(1, 0), Crew: []core.Person{{Name: "Producer", Role: "Producer", TMDB: 4, ThumbPath: "/crew.jpg"}}}
	viewer := &core.User{ID: 7, IsOwner: true}
	var fetched []string
	images := NewRemoteCache(1024, &http.Client{Transport: imageTransport(func(r *http.Request) (*http.Response, error) {
		fetched = append(fetched, r.URL.String())
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(bytes.NewReader(encoded.Bytes()))}, nil
	})})
	source := Source{Catalog: refreshingArtwork{storedArtwork: storedArtwork{key: title}, fresh: title}, Visibility: experience.Visibility{Policy: &artworkPolicy{viewer: viewer}}, Images: images, ImageURL: func(path, size string) string { return "https://fixture.invalid/" + size + path }, Now: time.Now}
	queue := NewMetadataQueue(ctx, db, func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }, source.Read)
	source.Metadata = queue.Get
	listed, _, err := source.List(ctx, frontend.ArtworkRequest{Viewer: viewer, Item: key})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range listed {
		if item.Kind == frontend.ImagePerson {
			if item.ThumbHash == "" {
				t.Fatalf("crew portrait listed without a thumbhash fetched=%v", fetched)
			}
			return
		}
	}
	t.Fatal("crew portrait missing")
}

func TestNeutralArtworkParentLookupIsStoredOnly(t *testing.T) {
	show := core.ShowKey(1)
	episode := core.EpisodeKey(1, 1, 1)
	catalog := storedArtwork{show: {Key: show, Type: core.TypeShow, PosterPath: "/show.jpg"}, episode: {Key: episode, Type: core.TypeEpisode, ShowKey: show, Released: time.Unix(1, 0)}}
	source := Source{Catalog: catalog}
	req := frontend.ArtworkRequest{Viewer: &core.User{ID: 1}, Item: episode}
	images, _, err := source.List(context.Background(), req)
	if err != nil || len(images) == 0 {
		t.Fatalf("stored parent fallback: %v", err)
	}
	delete(catalog, show)
	if _, _, err := source.List(context.Background(), req); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing inherited parent policy: %v", err)
	}
}
