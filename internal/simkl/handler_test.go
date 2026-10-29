package simkl

import (
	"context"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHandlerRejectsUnknownViewerAndRedactsStatus(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenPath(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tokens := TokenStore{DB: db, Sealer: testSealer{}}
	tok := Token{ClientID: "id", AccessToken: "access-secret", RefreshToken: "refresh-secret", ExpiresIn: 300, Scope: "media:read media:write"}
	if err := tokens.Save(ctx, 1, tok, time.Now()); err != nil {
		t.Fatal(err)
	}
	svc := Service{Auth: AuthClient{ClientID: "id"}, Tokens: tokens, State: state.New(db, state.Options{})}
	dev := &DeviceManager{Tokens: tokens, Auth: svc.Auth}
	h := Handler(HandlerDeps{Device: dev, Service: func() Service { return svc }, Enabled: func() bool { return true }, Viewer: func(_ context.Context, id int64) bool { return id == 1 }})
	for _, path := range []string{"/2/status", "/2/start", "/2/disconnect", "/2/sync"} {
		method := "POST"
		if strings.HasSuffix(path, "status") {
			method = "GET"
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		if rec.Code != 404 {
			t.Fatalf("%s = %d", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/1/status", nil))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
}
