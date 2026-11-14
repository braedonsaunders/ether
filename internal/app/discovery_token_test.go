package app

import (
	"context"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// The owner token must never be used as a friend's token, and all active Home
// members use the same authorized switch regardless of managed status.
func TestDiscoveryViewerToken(t *testing.T) {
	for _, tc := range []struct {
		name         string
		username     string
		owner, home  bool
		want         string
		unauthorized bool
	}{
		{"owner", "owner", true, false, "owner", false},
		{"managed", "kid", false, true, "member", false},
		{"home account", "spouse", false, true, "member", false},
		{"guest", "", false, false, "member", false},
		{"friend", "friend", false, false, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			got, err := discoveryViewerToken(context.Background(), &core.User{Username: tc.username, IsOwner: tc.owner, PlexID: 42}, tc.home, "owner",
				func(_ context.Context, token string, id int64, pin string) (string, error) {
					calls++
					if token != "owner" || id != 42 || pin != "" {
						t.Fatal("invalid switch credentials")
					}
					return "member", nil
				})
			if got != tc.want || errors.Is(err, core.ErrUnauthorized) != tc.unauthorized {
				t.Fatalf("got %q, %v", got, err)
			}
			wantCalls := 0
			if !tc.owner && !tc.unauthorized {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("switch calls: %d", calls)
			}
		})
	}
}
