package plex

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/safety"
	"github.com/braedonsaunders/ether/internal/store"
)

var _ core.ArtworkAccessPolicy = (*safety.Service)(nil)

type unknownPolicy struct{}

func (unknownPolicy) Allowed(*core.User, *core.Title) bool { return true }

func TestPlexOwnerAndRestrictedSearchParity(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	policy, err := safety.New(context.Background(), store.OpenTest(t))
	if err != nil {
		t.Fatal(err)
	}
	f.srv.d.AccessPolicy = policy
	f.srv.d.FilterAvailable = func(context.Context, []core.Title) []core.Title { return nil }
	owner := &core.User{ID: 1, IsOwner: true}
	restricted := &core.User{ID: 2, AllowedSections: []int{1}}
	for _, tc := range []struct {
		name string
		user *core.User
		want int
	}{{"owner", owner, 1}, {"restricted", restricted, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.srv.runSearch(context.Background(), &Request{User: tc.user}, fightClub.Title, 0, 10)
			if err != nil || len(got) != tc.want {
				t.Fatalf("search = %+v, %v; want %d", got, err, tc.want)
			}
		})
	}
	// A custom policy lacking the restriction interface must not disable
	// filtering for other viewers, but the owner retains the legacy path.
	f.srv.d.AccessPolicy = unknownPolicy{}
	if f.srv.viewerRestricted(owner) || !f.srv.viewerRestricted(&core.User{ID: 3}) {
		t.Fatal("custom policy classification")
	}
}

func TestPlexRecentOwnerKeepsLegacyCandidates(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.srv.d.FilterAvailable = func(context.Context, []core.Title) []core.Title { return nil }
	sec, ok := f.srv.sectionFor(core.TypeMovie)
	if !ok {
		t.Fatal("missing movie section")
	}
	owner := &core.User{ID: 1, IsOwner: true}
	restricted := &core.User{ID: 2, AllowedSections: []int{sec.ID}}
	original, err := f.srv.controlLegacyRecentlyAddedHub(context.Background(), &Request{User: owner}, sec, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.srv.recentlyAddedHub(context.Background(), &Request{User: owner}, sec, 1)
	if err != nil || len(got.Items) != len(original.Items) || got.More != original.More || len(got.Items) != 1 || got.Items[0].Key != original.Items[0].Key {
		t.Fatalf("owner legacy row = %+v, want %+v, %v", got, original, err)
	}
	hidden, err := f.srv.recentlyAddedHub(context.Background(), &Request{User: restricted}, sec, 1)
	if err != nil || len(hidden.Items) != 0 {
		t.Fatalf("restricted row = %+v, %v", hidden, err)
	}
}
