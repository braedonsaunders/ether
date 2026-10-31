package livetv

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/sessions"
)

func TestMediaGrantAuthorizesPlaybackButNotControl(t *testing.T) {
	s := &Service{sessions: map[string]*session{}}
	s.sessions["sess"] = &session{info: SessionInfo{ID: "sess", UserID: 4}, streamGrant: "grant-value"}
	active := func(_ context.Context, id int64) *core.User {
		if id == 4 {
			return &core.User{ID: 4}
		}
		return nil
	}
	media := httptest.NewRequest(http.MethodGet, "/livetv/sessions/sess/0/program/1/2.ts?"+sessions.StreamGrantParam+"=grant-value", nil)
	if u, ok := s.AuthenticateMediaGrant(media, active); !ok || u == nil || u.ID != 4 {
		t.Fatalf("media grant: %+v %v", u, ok)
	}
	for _, path := range []string{"/livetv/sessions/sess", "/livetv/sessions/sess/0/index.m3u8?X-Plex-Token=account"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if _, ok := s.AuthenticateMediaGrant(req, active); ok {
			t.Fatalf("authorized %s", path)
		}
	}
	stop := httptest.NewRequest(http.MethodDelete, "/livetv/sessions/sess/0/index.m3u8?"+sessions.StreamGrantParam+"=grant-value", nil)
	if _, ok := s.AuthenticateMediaGrant(stop, active); ok {
		t.Fatal("grant authorized a control method")
	}
}
