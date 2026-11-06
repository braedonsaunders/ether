package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNodeControlRequiresAuthenticatedOwnerContext(t *testing.T) {
	// Even an apparently local request cannot bypass the admin owner boundary.
	for _, handler := range []http.Handler{(&App{}).nodeControlHandler(), (&App{}).nodeRolePreflightHandler()} {
		for _, method := range []string{"GET", "POST"} {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(method, "/nodes/n/control?boot=b", nil))
			if rec.Code != 401 {
				t.Fatalf("%s got %d", method, rec.Code)
			}
		}
	}

}
