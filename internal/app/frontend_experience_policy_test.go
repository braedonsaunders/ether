package app

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
)

func TestExperienceVisibilityAppliesLibraryAndInheritedPolicy(t *testing.T) {
	show := core.ShowKey(5)
	episode := &core.Title{Key: core.EpisodeKey(5, 1, 1), ShowKey: show, Type: core.TypeEpisode}
	v := experienceVisibility(nil, func(_ context.Context, key core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: key, Type: core.TypeShow, Adult: true}, nil
	}, nil, nil, []experience.Library{{ID: 1, Type: core.TypeMovie}, {ID: 2, Type: core.TypeShow}})
	viewer := &core.User{ID: 1, AllowedSections: []int{2}}
	if v.Visible(context.Background(), viewer, episode) {
		t.Fatal("episode with hidden parent was visible")
	}
	if v.TitleAllowed(viewer, &core.Title{Type: core.TypeMovie}) {
		t.Fatal("hidden movie library was visible")
	}
}
