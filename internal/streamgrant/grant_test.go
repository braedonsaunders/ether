package streamgrant

import (
	"context"
	"net/http"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestGrantRejectsInvalidAndRevokedViewers(t *testing.T) {
	binding := GrantBinding{Token: "scoped-capability", ViewerID: 7, Started: true}
	active := func(_ context.Context, id int64) *core.Viewer {
		if id == 7 {
			return &core.Viewer{ID: 7}
		}
		return nil
	}
	if viewer, ok := AuthenticateGrant(context.Background(), http.MethodGet, binding.Token, binding, active); !ok || viewer.ID != 7 {
		t.Fatal("valid grant rejected")
	}
	for _, tc := range []struct {
		method, token string
		binding       GrantBinding
	}{
		{http.MethodPost, binding.Token, binding}, {http.MethodGet, "wrong", binding},
		{http.MethodGet, binding.Token, GrantBinding{Token: binding.Token, ViewerID: 7}},
		{http.MethodGet, binding.Token, GrantBinding{Token: binding.Token, ViewerID: 8, Started: true}},
	} {
		if _, ok := AuthenticateGrant(context.Background(), tc.method, tc.token, tc.binding, active); ok {
			t.Fatalf("invalid grant accepted: %+v", tc)
		}
	}
}
