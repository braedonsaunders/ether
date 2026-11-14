package playback

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestSelectProfileForMediaUsesTitleKind(t *testing.T) {
	var got []string
	m := &Manager{deps: Deps{SelectProfile: func(_ core.ClientCaps, _ *core.User, media string) core.QualityProfile {
		got = append(got, media)
		return core.QualityProfile{Name: media}
	}}}
	if p := m.selectProfile(core.ClientCaps{}, nil, core.MovieKey(1)); p.Name != core.MediaMovie {
		t.Fatalf("movie profile %q", p.Name)
	}
	if p := m.selectProfile(core.ClientCaps{}, nil, core.EpisodeKey(1, 1, 1)); p.Name != core.MediaTV {
		t.Fatalf("episode profile %q", p.Name)
	}
	if p := m.selectProfile(core.ClientCaps{}, nil, core.ShowKey(1)); p.Name != core.MediaTV {
		t.Fatalf("show key profile %q", p.Name)
	}
	if len(got) != 3 || got[0] != core.MediaMovie || got[1] != core.MediaTV || got[2] != core.MediaTV {
		t.Fatalf("media passed to selector %v", got)
	}
}
