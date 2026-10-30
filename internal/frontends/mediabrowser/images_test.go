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
		images := []frontend.GrantedImage{{Image: frontend.Image{Kind: frontend.ImagePoster, Width: 600, Height: 300, BlurHash: "gridhash"}, Secret: "scoped-poster"}, {Image: frontend.Image{Kind: frontend.ImageBackdrop}, Secret: "scoped-backdrop"}}
		for _, kind := range request.Kinds {
			if kind == frontend.ImageLogo {
				images = append(images, frontend.GrantedImage{Image: frontend.Image{Kind: frontend.ImageLogo, BlurHash: "logohash"}, Secret: "scoped-logo"})
			}
		}
		return []frontend.ImageBatchItem{{Item: request.Items[0], Authoritative: true, Images: images}, {Item: request.Items[1], Authoritative: false, Images: []frontend.GrantedImage{{Image: frontend.Image{Kind: frontend.ImagePoster}, Secret: "untrusted"}}}}, nil
	}
	return nil, nil
}

func TestGridArtworkUsesBoundedAuthorizedBatches(t *testing.T) {
	grants := &imageBatchFake{}
	i := &instance{protocol: "jellyfin", host: frontend.Host{ImageGrants: grants}}
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
	if images[keys[0]]["PrimaryImageAspectRatio"] != float64(2) || images[keys[0]]["ImageBlurHashes"].(map[string]any)["Primary"].(map[string]string)["scoped-poster"] != "gridhash" {
		t.Fatal(images)
	}
	if images[keys[0]]["ImageTags"].(map[string]string)["Logo"] != "scoped-logo" || images[keys[0]]["ImageBlurHashes"].(map[string]any)["Logo"].(map[string]string)["scoped-logo"] != "logohash" {
		t.Fatal(images)
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
		return frontend.ItemDetail{Title: frontend.Title{Key: r.Key, Type: "movie", Title: "Movie"}, ImagesAuthoritative: true, Images: []frontend.Image{{Kind: "poster", Path: "/poster", Width: 400, Height: 200, BlurHash: "blurhash-primary"}, {Kind: "poster", Path: "/alternate", Width: 3, Height: 4, BlurHash: "alternate"}}}, nil
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
	if !strings.Contains(w.Body.String(), `"PrimaryImageAspectRatio":2`) || !strings.Contains(w.Body.String(), `"ImageBlurHashes":{"Primary":{"scoped-image/poster":"blurhash-primary"}}`) || strings.Contains(w.Body.String(), "alternate") {
		t.Fatal(w.Body.String())
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

func TestImageMetadataRequiresGrantedAndKnownDescriptor(t *testing.T) {
	i := &instance{protocol: "jellyfin"}
	dto := map[string]any{}
	i.addImageMetadata(dto, "Primary", "", frontend.Image{Width: 400, Height: 200, BlurHash: "secret"})
	if len(dto) != 0 {
		t.Fatal(dto)
	}
	i.addImageMetadata(dto, "Primary", "grant", frontend.Image{})
	if len(dto) != 0 {
		t.Fatal(dto)
	}
	emby := &instance{protocol: "emby"}
	emby.addImageMetadata(dto, "Primary", "grant", frontend.Image{Width: 400, Height: 200, BlurHash: "hidden"})
	if dto["PrimaryImageAspectRatio"] != float64(2) || dto["ImageBlurHashes"] != nil {
		t.Fatal(dto)
	}
}
