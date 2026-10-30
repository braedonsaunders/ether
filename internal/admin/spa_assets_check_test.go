package admin

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOwnerUIAndEmbeddedAssetsRemainAvailable(t *testing.T) {
	var asset string
	err := fs.WalkDir(distFS, "dist/assets", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && asset == "" {
			asset = strings.TrimPrefix(name, "dist/")
		}
		return nil
	})
	if err != nil || asset == "" {
		t.Fatalf("embedded assets unavailable: %v", err)
	}
	h := &Handler{}
	for _, path := range []string{"/", "/" + asset} {
		w := httptest.NewRecorder()
		h.handleSPA(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK || w.Body.Len() == 0 {
			t.Errorf("%s: status %d, body length %d", path, w.Code, w.Body.Len())
		}
		if path != "/" && !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
			t.Errorf("asset cache header: %q", w.Header().Get("Cache-Control"))
		}
	}
}
