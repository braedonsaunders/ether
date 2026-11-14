package vela

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

func TestVelaImageSizeContract(t *testing.T) {
	for _, tc := range []struct {
		width, height int
		w, h          string
	}{{3840, 2160, "3840", "2160"}, {8000, 4500, "4000", "2250"}, {0, 0, "0", ""}, {0, 180, "0", ""}} {
		size := imageSize(frontend.Image{Width: tc.width, Height: tc.height}, "opaque-grant")
		u, err := url.Parse(size["url"].(string))
		if err != nil || u.Query().Get("w") != tc.w || u.Query().Get("h") != tc.h {
			t.Fatal(size, err)
		}
	}
	calls := 0
	i := &instance{host: frontend.Host{ImageGrants: frontendtest.ImageGrants{ServeFunc: func(w http.ResponseWriter, r *http.Request, secret string) {
		calls++
		if secret != "opaque" || r.URL.Query().Get("w") != "4000" || r.URL.Query().Get("h") != "1000" {
			t.Fatal("grant/size changed", secret, r.URL)
		}
		w.WriteHeader(200)
	}}}}
	for _, query := range []string{"w=99999&h=1000", "width=1920", "w=-1", "h=no", "w=10&w=20"} {
		r := httptest.NewRequest("GET", "/vela/v0/images/opaque?"+query, nil)
		r.SetPathValue("secret", "opaque")
		w := httptest.NewRecorder()
		i.image(w, r)
		want := 400
		if query == "w=99999&h=1000" {
			want = 200
		}
		if w.Code != want {
			t.Fatal(query, w.Code)
		}
	}
	if calls != 1 {
		t.Fatal("invalid sizes reached image service", calls)
	}
}
