package resolver

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func mediaProfiles() (movie, tv, any core.QualityProfile) {
	return core.QualityProfile{Name: "Movies", MediaType: core.MediaMovie, MaxHeight: 2160},
		core.QualityProfile{Name: "Shows", MediaType: core.MediaTV, MaxHeight: 1080},
		core.QualityProfile{Name: "Any", MaxHeight: 720}
}

func TestSelectProfileForMedia(t *testing.T) {
	movie, tv, any := mediaProfiles()
	caps := core.ClientCaps{MaxHeight: 2160}
	remote := caps
	remote.Remote = true

	tests := []struct {
		name       string
		q          config.Quality
		caps       core.ClientCaps
		media      string
		want       string
		wantSource ProfileSource
		wantRule   int
	}{
		{
			name:       "movie default",
			q:          config.Quality{Profiles: []core.QualityProfile{movie, tv}, DefaultMovie: "Movies", DefaultTV: "Shows"},
			caps:       caps,
			media:      core.MediaMovie,
			want:       "Movies",
			wantSource: ProfileFromDefault,
		},
		{
			name:       "tv default",
			q:          config.Quality{Profiles: []core.QualityProfile{movie, tv}, DefaultMovie: "Movies", DefaultTV: "Shows"},
			caps:       caps,
			media:      core.MediaTV,
			want:       "Shows",
			wantSource: ProfileFromDefault,
		},
		{
			name:       "legacy default applies to both",
			q:          config.Quality{Profiles: []core.QualityProfile{any, movie, tv}, Default: "Any"},
			caps:       caps,
			media:      core.MediaMovie,
			want:       "Any",
			wantSource: ProfileFromDefault,
		},
		{
			name:       "legacy default applies to tv",
			q:          config.Quality{Profiles: []core.QualityProfile{any, movie, tv}, Default: "Any"},
			caps:       caps,
			media:      core.MediaTV,
			want:       "Any",
			wantSource: ProfileFromDefault,
		},
		{
			name:       "incompatible legacy default falls to tv default",
			q:          config.Quality{Profiles: []core.QualityProfile{movie, tv}, Default: "Movies", DefaultTV: "Shows"},
			caps:       caps,
			media:      core.MediaTV,
			want:       "Shows",
			wantSource: ProfileFromDefault,
		},
		{
			name:       "incompatible media default falls to first compatible",
			q:          config.Quality{Profiles: []core.QualityProfile{movie, tv}, DefaultTV: "Movies"},
			caps:       caps,
			media:      core.MediaTV,
			want:       "Shows",
			wantSource: ProfileFromFirst,
		},
		{
			name:       "movie profile never chosen for tv",
			q:          config.Quality{Profiles: []core.QualityProfile{movie, tv}},
			caps:       caps,
			media:      core.MediaTV,
			want:       "Shows",
			wantSource: ProfileFromFirst,
		},
		{
			name:       "tv profile never chosen for movie",
			q:          config.Quality{Profiles: []core.QualityProfile{tv, movie}},
			caps:       caps,
			media:      core.MediaMovie,
			want:       "Movies",
			wantSource: ProfileFromFirst,
		},
		{
			name:       "only incompatible profiles falls back to builtin",
			q:          config.Quality{Profiles: []core.QualityProfile{movie}},
			caps:       caps,
			media:      core.MediaTV,
			want:       "default",
			wantSource: ProfileBuiltin,
		},
		{
			name: "media rule",
			q: config.Quality{Profiles: []core.QualityProfile{movie, tv}, Rules: []config.QualityRule{
				{MediaType: core.MediaMovie, Product: "Apple TV", Profile: "Movies"},
				{MediaType: core.MediaTV, Product: "Apple TV", Profile: "Shows"},
			}},
			caps:       appleTVCaps(),
			media:      core.MediaMovie,
			want:       "Movies",
			wantSource: ProfileFromRule,
			wantRule:   0,
		},
		{
			name: "media rule tv",
			q: config.Quality{Profiles: []core.QualityProfile{movie, tv}, Rules: []config.QualityRule{
				{MediaType: core.MediaMovie, Product: "Apple TV", Profile: "Movies"},
				{MediaType: core.MediaTV, Product: "Apple TV", Profile: "Shows"},
			}},
			caps:       appleTVCaps(),
			media:      core.MediaTV,
			want:       "Shows",
			wantSource: ProfileFromRule,
			wantRule:   1,
		},
		{
			name: "rule scoped to other media ignored",
			q: config.Quality{Profiles: []core.QualityProfile{movie, tv}, Rules: []config.QualityRule{
				{MediaType: core.MediaTV, Product: "Apple TV", Profile: "Shows"},
			}},
			caps:       appleTVCaps(),
			media:      core.MediaMovie,
			want:       "Movies",
			wantSource: ProfileFromFirst,
		},
		{
			name: "rule targeting incompatible profile skipped",
			q: config.Quality{Profiles: []core.QualityProfile{movie, tv}, Rules: []config.QualityRule{
				{Product: "Apple TV", Profile: "Movies"},
				{MediaType: core.MediaTV, Product: "Apple TV", Profile: "Shows"},
			}},
			caps:       appleTVCaps(),
			media:      core.MediaTV,
			want:       "Shows",
			wantSource: ProfileFromRule,
			wantRule:   1,
		},
		{
			name:       "movie remote profile",
			q:          config.Quality{Profiles: []core.QualityProfile{movie, tv}, RemoteMovie: "Movies", RemoteTV: "Shows"},
			caps:       remote,
			media:      core.MediaMovie,
			want:       "Movies",
			wantSource: ProfileFromRemote,
		},
		{
			name:       "tv remote profile",
			q:          config.Quality{Profiles: []core.QualityProfile{movie, tv}, RemoteMovie: "Movies", RemoteTV: "Shows"},
			caps:       remote,
			media:      core.MediaTV,
			want:       "Shows",
			wantSource: ProfileFromRemote,
		},
		{
			name:       "incompatible remote falls to media default",
			q:          config.Quality{Profiles: []core.QualityProfile{movie, tv}, RemoteTV: "Movies", DefaultTV: "Shows"},
			caps:       remote,
			media:      core.MediaTV,
			want:       "Shows",
			wantSource: ProfileFromDefault,
		},
		{
			name:       "legacy remote applies to media",
			q:          config.Quality{Profiles: []core.QualityProfile{any, movie, tv}, Remote: "Any"},
			caps:       remote,
			media:      core.MediaTV,
			want:       "Any",
			wantSource: ProfileFromRemote,
		},
		{
			name:       "unknown media accepts any scope",
			q:          config.Quality{Profiles: []core.QualityProfile{movie, tv}, Default: "Movies"},
			caps:       caps,
			media:      core.MediaAny,
			want:       "Movies",
			wantSource: ProfileFromDefault,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wantRule := tc.wantRule
			if tc.wantSource != ProfileFromRule {
				wantRule = -1
			}
			choice := chooseProfileForMedia(tc.q, tc.caps, nil, tc.media, "")
			if choice.profile.Name != tc.want || choice.source != tc.wantSource || choice.rule != wantRule {
				t.Fatalf("choice = %+v, want profile %q source %q rule %d", choice, tc.want, tc.wantSource, wantRule)
			}
			if got := SelectProfileForMedia(tc.q, tc.caps, nil, tc.media); got.Name != tc.want {
				t.Fatalf("SelectProfileForMedia = %q, want %q", got.Name, tc.want)
			}
		})
	}
}

func TestSelectProfileMediaNeutral(t *testing.T) {
	movie, tv, _ := mediaProfiles()
	q := config.Quality{Profiles: []core.QualityProfile{movie, tv}, Default: "Shows"}
	if got := SelectProfile(q, core.ClientCaps{}, nil); got.Name != "Shows" {
		t.Fatalf("neutral selection = %q, want Shows", got.Name)
	}
	if got := SelectProfileForMedia(q, core.ClientCaps{}, nil, core.MediaAny); got.Name != "Shows" {
		t.Fatalf("empty-media selection = %q, want Shows", got.Name)
	}
}

func TestApplyClientRulesForMediaScopesRules(t *testing.T) {
	movie, tv, _ := mediaProfiles()
	q := config.Quality{
		Profiles: []core.QualityProfile{movie, tv},
		Rules: []config.QualityRule{
			{MediaType: core.MediaMovie, Product: "Apple TV", Profile: "Movies", ConvertSurroundAudio: true},
		},
	}
	hasTrueHD := func(caps core.ClientCaps) bool {
		for _, c := range caps.AudioCodecs {
			if c == "truehd" {
				return true
			}
		}
		return false
	}
	if caps := ApplyClientRules(q, appleTVCaps(), nil); !hasTrueHD(caps) {
		t.Fatal("title-neutral capabilities applied a movie-only audio rule")
	}
	if caps, _ := applyClientRules(q, appleTVCaps(), nil, core.MediaMovie, ""); hasTrueHD(caps) {
		t.Fatalf("movie rule did not convert surround audio: %+v", caps.AudioCodecs)
	}
	if caps, _ := applyClientRules(q, appleTVCaps(), nil, core.MediaTV, ""); !hasTrueHD(caps) {
		t.Fatalf("tv should not apply the movie rule: %+v", caps.AudioCodecs)
	}
}

func TestTitleMedia(t *testing.T) {
	tests := []struct {
		name  string
		title *core.Title
		want  string
	}{
		{name: "nil", title: nil, want: core.MediaAny},
		{name: "movie type", title: &core.Title{Type: core.TypeMovie, Key: core.MovieKey(603)}, want: core.MediaMovie},
		{name: "episode type", title: &core.Title{Type: core.TypeEpisode, Key: core.EpisodeKey(1399, 1, 1)}, want: core.MediaTV},
		{name: "movie key only", title: &core.Title{Key: core.MovieKey(603)}, want: core.MediaMovie},
		{name: "episode key only", title: &core.Title{Key: core.EpisodeKey(1399, 1, 1)}, want: core.MediaTV},
		{name: "type wins over key", title: &core.Title{Type: core.TypeEpisode, Key: core.MovieKey(603)}, want: core.MediaTV},
		{name: "show key is unknown", title: &core.Title{Type: core.TypeShow, Key: core.ShowKey(1399)}, want: core.MediaAny},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := titleMedia(tc.title); got != tc.want {
				t.Fatalf("titleMedia = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveUsesTitleMediaForProfile(t *testing.T) {
	movie, tv, any := mediaProfiles()
	profiles := []core.QualityProfile{movie, tv, any}
	movieTitle := matrixTitle()
	episode := &core.Title{
		Key: core.EpisodeKey(1399, 1, 1), Type: core.TypeEpisode,
		ShowKey: core.ShowKey(1399), ShowIDs: core.IDs{TMDB: 1399},
		ShowTitle: "Severance", Title: "Good News About Hell", Season: 1, Episode: 1,
	}
	tests := []struct {
		name string
		q    config.Quality
	}{
		{
			name: "media defaults",
			q: config.Quality{
				Profiles:     profiles,
				DefaultMovie: "Movies",
				DefaultTV:    "Shows",
			},
		},
		{
			name: "media rules",
			q: config.Quality{
				Profiles:     profiles,
				DefaultMovie: "Any",
				DefaultTV:    "Any",
				Rules: []config.QualityRule{
					{MediaType: core.MediaMovie, Product: "Apple TV", Profile: "Movies"},
					{MediaType: core.MediaTV, Product: "Apple TV", Profile: "Shows"},
				},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testCfg()
			cfg.Quality = tc.q
			svc := New(&fakeIndexer{}, fakeHealth{}, nil, cfg, Options{})
			ctx := context.Background()

			for _, title := range []*core.Title{movieTitle, episode} {
				if _, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: appleTVCaps()}); err == nil {
					t.Fatalf("%s: expected no releases to be found", title.Title)
				}
			}
			got := map[core.RatingKey]string{}
			for _, tr := range svc.Traces() {
				got[tr.TitleKey] = tr.Profile
			}
			if got[movieTitle.Key] != "Movies" {
				t.Errorf("movie resolved with profile %q, want Movies", got[movieTitle.Key])
			}
			if got[episode.Key] != "Shows" {
				t.Errorf("episode resolved with profile %q, want Shows", got[episode.Key])
			}
			if got[movieTitle.Key] == got[episode.Key] {
				t.Errorf("movie and tv chose the same profile %q", got[movieTitle.Key])
			}
		})
	}
}
