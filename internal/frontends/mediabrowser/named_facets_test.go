package mediabrowser

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

func TestNamedFacetsUseParentAuthorizedIDs(t *testing.T) {
	viewer := &frontend.Viewer{ID: 1}
	calls := 0
	exp := frontendtest.Experience{NamedFacetIDsFunc: func(_ context.Context, v *frontend.Viewer, _ frontend.Device, parent frontend.RatingKey, kind string, names []string) (map[string]string, error) {
		if v != viewer || parent != 5 {
			t.Fatal(v, parent)
		}
		calls++
		switch kind {
		case "genre":
			if len(names) != 2 {
				t.Fatal(names)
			}
			return map[string]string{"Drama": "genre-id", "NotOnTitle": "forged-id"}, nil
		case "studio":
			return map[string]string{"Studio": "studio-id"}, nil
		}
		t.Fatal(kind)
		return nil, nil
	}}
	i := &instance{host: frontend.Host{Experience: exp}}
	dto := map[string]any{}
	detail := frontend.ItemDetail{Title: frontend.Title{Key: 5, Genres: []string{"Drama", "Drama", "Hidden"}, Studios: []string{"Studio"}}}
	i.addNamedFacets(httptest.NewRequest("GET", "/Items/x", nil), viewer, detail, dto)
	genres, studios := dto["GenreItems"].([]any), dto["Studios"].([]any)
	if calls != 2 || len(genres) != 1 || genres[0].(map[string]any)["Id"] != "genre-id" || len(studios) != 1 || studios[0].(map[string]any)["Id"] != "studio-id" {
		t.Fatal(dto, calls)
	}
}
