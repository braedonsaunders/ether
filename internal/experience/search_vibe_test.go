package experience

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type vibeCalls struct{ n int }

func (v *vibeCalls) VibeCollection(context.Context, *core.User, string) (*core.Collection, []core.Title, bool, error) {
	v.n++
	return &core.Collection{Key: core.CollectionKey(1), Title: "vibe"}, []core.Title{{Key: core.MovieKey(11), Type: core.TypeMovie, Title: "Member"}, {Key: core.MovieKey(12), Type: core.TypeMovie, Title: "Hidden", Adult: true}}, true, nil
}

func TestSearchUsesVibeMembersOnlyAfterKeywordsMiss(t *testing.T) {
	ctx := context.Background()
	v := &core.User{ID: 1}
	hit := core.Title{Key: core.MovieKey(5), Type: core.TypeMovie, Title: "Keyword"}
	calls := &vibeCalls{}
	keyword := true
	s := &Service{d: ServiceDeps{
		Active:     func(context.Context, int64) (*core.User, error) { return v, nil },
		Libraries:  []Library{{ID: 1, Type: core.TypeMovie}},
		Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }},
		Search: Search{
			Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }},
			Catalog: func(context.Context, string, []core.MediaType, int) ([]core.Title, error) {
				if keyword {
					return []core.Title{hit}, nil
				}
				return nil, nil
			},
			Vibe: VibeSearch{Source: calls, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}, Unrestricted: func(*core.User) bool { return true }},
		},
	}}
	page, err := s.Search(ctx, v, core.Device{}, "rain", frontend.ItemQuery{})
	if err != nil || calls.n != 0 || len(page.Page.Items) != 1 || page.Page.Items[0].Key != hit.Key {
		t.Fatalf("keyword %+v calls %d %v", page, calls.n, err)
	}
	keyword = false
	page, err = s.Search(ctx, v, core.Device{}, "cozy rain", frontend.ItemQuery{})
	if err != nil || calls.n != 1 || len(page.Page.Items) != 1 || page.Page.Items[0].Title != "Member" {
		t.Fatalf("vibe %+v calls %d %v", page, calls.n, err)
	}
}
