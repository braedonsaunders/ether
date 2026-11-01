package app

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestBrokerCanonicalSectionsAcrossSpike(t *testing.T) {
	for _, spike := range []bool{false, true} {
		cfg := config.Config{}
		cfg.Spike.Enabled = spike
		v := experienceVisibility(nil, nil, nil, nil, serverLibraries(cfg))
		v.SectionFor = brokerSectionFor(v.SectionFor)
		for _, tc := range []struct {
			kind     core.MediaType
			sections []int
			owner    bool
			want     bool
		}{
			{core.TypeMovie, []int{1}, false, true},
			{core.TypeMovie, []int{2}, false, false},
			{core.TypeShow, []int{1}, false, false},
			{core.TypeSeason, []int{1}, false, false},
			{core.TypeEpisode, []int{1}, false, false},
			{core.TypeEpisode, []int{2}, false, true},
			{core.TypeEpisode, []int{1}, true, true},
			{core.TypeEpisode, nil, false, true},
			{core.MediaType("virtual"), []int{1}, false, true},
			{core.MediaType("virtual"), []int{2}, false, false},
		} {
			viewer := &core.User{AllowedSections: tc.sections, IsOwner: tc.owner}
			if got := v.Playable(context.Background(), viewer, &core.Title{Type: tc.kind}); got != tc.want {
				t.Errorf("spike=%v kind=%s sections=%v owner=%v got=%v want=%v", spike, tc.kind, tc.sections, tc.owner, got, tc.want)
			}
		}
	}
}
