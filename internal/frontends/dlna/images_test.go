package dlna

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

type posterArt struct {
	data []byte
	hide bool
}

func (p posterArt) List(_ context.Context, req frontend.ArtworkRequest) ([]frontend.Image, bool, error) {
	if p.hide || req.Viewer == nil || req.Item != frontend.RatingKey(42) {
		return nil, true, nil
	}
	return []frontend.Image{{Kind: frontend.ImagePoster, Path: "composite", Tag: "overlay-v1"}}, true, nil
}

func (p posterArt) Read(context.Context, frontend.ArtworkRequest, frontend.Image, frontend.ArtworkTransform) ([]byte, string, error) {
	return p.data, "image/jpeg", nil
}

func TestAlbumArtUsesCompositedPosterAndRevalidates(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	movie := frontend.Title{Key: frontend.RatingKey(42), Type: "movie", Title: "Visible"}
	art := posterArt{data: []byte("composited-jpeg")}
	host := frontend.Host{
		LANViewer:  func(context.Context, *http.Request) (*frontend.Viewer, error) { return viewer, nil },
		ClientIP:   func(*http.Request) (netip.Addr, bool) { return netip.MustParseAddr("192.168.1.5"), true },
		SurfaceURL: func(context.Context, *http.Request) (string, error) { return "http://ether.local/dlna", nil },
		Artwork:    art,
		Sessions:   frontendtest.Sessions{},
		Experience: frontendtest.Experience{LibraryItemsFunc: func(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (frontend.Page, error) {
			return frontend.Page{Items: []frontend.Title{movie}, Total: 1}, nil
		}},
	}
	inst, err := (Frontend{MachineID: "machine"}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	body := `<Browse><ObjectID>view:1</ObjectID><BrowseFlag>BrowseDirectChildren</BrowseFlag><StartingIndex>0</StartingIndex><RequestedCount>20</RequestedCount></Browse>`
	r := httptest.NewRequest(http.MethodPost, "/dlna/ContentDirectory/control", strings.NewReader(body))
	r.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:ContentDirectory:1#Browse"`)
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "http://ether.local/dlna/images/42?v=overlay-v1") {
		t.Fatalf("browse %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/dlna/images/42?v=overlay-v1", nil))
	if w.Code != 200 || w.Body.String() != "composited-jpeg" || w.Header().Get("ETag") == "" {
		t.Fatalf("image %d %q etag=%q", w.Code, w.Body.String(), w.Header().Get("ETag"))
	}
	etag := w.Header().Get("ETag")
	again := httptest.NewRequest(http.MethodGet, "/dlna/images/42?v=overlay-v1", nil)
	again.Header.Set("If-None-Match", etag)
	w = httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, again)
	if w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Fatalf("revalidate %d %q", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/dlna/images/99", nil))
	if w.Code != 404 {
		t.Fatalf("hidden %d", w.Code)
	}
}
