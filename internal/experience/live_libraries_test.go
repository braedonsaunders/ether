package experience

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestViewsFollowLiveNamesAndSectionPolicy(t *testing.T) {
	movies, shows := "Movies", "TV Shows"
	s := &Service{d: ServiceDeps{
		Active: func(_ context.Context, id int64) (*core.User, error) {
			if id == 2 {
				return &core.User{ID: id, AllowedSections: []int{1}}, nil
			}
			return &core.User{ID: id, IsOwner: true}, nil
		},
		LiveLibraries: func() []Library {
			return []Library{{ID: 1, Name: movies, Type: core.TypeMovie}, {ID: 2, Name: shows, Type: core.TypeShow}}
		},
	}}
	owner, err := s.Views(context.Background(), &core.User{ID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(owner) != 2 || owner[0].ID != "1" || owner[0].Name != "Movies" || owner[1].ID != "2" || owner[1].Name != "TV Shows" {
		t.Fatalf("owner views = %+v", owner)
	}
	movies, shows = "Basement Films", "Basement Series"
	owner, err = s.Views(context.Background(), &core.User{ID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(owner) != 2 || owner[0].ID != "1" || owner[0].Name != "Basement Films" || owner[1].ID != "2" || owner[1].Name != "Basement Series" {
		t.Fatalf("renamed owner views = %+v", owner)
	}
	restricted, err := s.Views(context.Background(), &core.User{ID: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(restricted) != 1 || restricted[0].ID != "1" || restricted[0].Name != "Basement Films" {
		t.Fatalf("restricted views = %+v", restricted)
	}
}
