package mediabrowser

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type imageGrantsFake struct{ issued int }

type imageBatchFake struct {
	imageGrantsFake
	batches int
}

func (f *imageBatchFake) IssueBatch(_ context.Context, request frontend.ImageBatchRequest) ([]frontend.ImageBatchItem, error) {
	if request.Credential != "grant" || len(request.Items) > 50 {
		return nil, errors.New("bad batch")
	}
	f.batches++
	if f.batches == 1 {
		return []frontend.ImageBatchItem{{Item: request.Items[0], Authoritative: true, Images: []frontend.GrantedImage{{Image: frontend.Image{Kind: frontend.ImagePoster}, Secret: "scoped-poster"}, {Image: frontend.Image{Kind: frontend.ImageBackdrop}, Secret: "scoped-backdrop"}}}, {Item: request.Items[1], Authoritative: false, Images: []frontend.GrantedImage{{Image: frontend.Image{Kind: frontend.ImagePoster}, Secret: "untrusted"}}}}, nil
	}
	return nil, nil
}

func TestGridArtworkUsesBoundedAuthorizedBatches(t *testing.T) {
	grants := &imageBatchFake{}
	i := &instance{host: frontend.Host{ImageGrants: grants}}
	keys := make([]frontend.RatingKey, 51)
	for n := range keys {
		keys[n] = frontend.RatingKey(n + 1)
	}
	r := httptest.NewRequest("GET", "/Items", nil)
	r.Header.Set("X-Emby-Token", "grant")
	images := i.gridImages(r, keys)
	if grants.batches != 2 || len(images) != 1 || images[keys[0]]["ImageTags"].(map[string]string)["Primary"] != "scoped-poster" || images[keys[0]]["BackdropImageTags"].([]string)[0] != "scoped-backdrop" {
		t.Fatal(grants.batches, images)
	}
}

func (f *imageGrantsFake) Issue(_ context.Context, r frontend.ImageGrantRequest) (string, error) {
	if r.Credential != "grant" || r.Item != 5 {
		return "", errors.New("denied")
	}
	f.issued++
	return "scoped-image" + r.Image.Path, nil
}
func (*imageGrantsFake) Revoke(context.Context, string, string) error { return nil }
func (*imageGrantsFake) Serve(w http.ResponseWriter, _ *http.Request, token string) {
	if token != "scoped-image/poster" {
		http.NotFound(w, nil)
		return
	}
	_, _ = w.Write([]byte("image"))
}
func TestImagesRequireScopedGrant(t *testing.T) {
	grants := &imageGrantsFake{}
	ident := frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return &frontend.Viewer{ID: 1}, nil }}
	exp := frontendtest.Experience{ItemFunc: func(_ context.Context, r frontend.ItemRequest) (frontend.ItemDetail, error) {
		return frontend.ItemDetail{Title: frontend.Title{Key: r.Key, Type: "movie", Title: "Movie"}, ImagesAuthoritative: true, Images: []frontend.Image{{Kind: "poster", Path: "/poster"}, {Kind: "poster", Path: "/alternate"}}}, nil
	}}
	inst, err := (Jellyfin{MachineID: "test"}).Mount(context.Background(), frontend.Host{Identity: ident, Experience: exp, ImageGrants: grants})
	if err != nil {
		t.Fatal(err)
	}
	id := ids.RatingKeyGUID(5)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/Items/"+id, nil)
	r.Header.Set("X-Emby-Token", "grant")
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || grants.issued != 1 || !strings.Contains(w.Body.String(), "scoped-image/poster") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/Items/"+id+"/Images/Primary", nil))
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/Items/"+id+"/Images/Primary?tag=scoped-image%2Fposter", nil))
	if w.Code != 200 || w.Body.String() != "image" {
		t.Fatal(w.Code, w.Body.String())
	}
}
