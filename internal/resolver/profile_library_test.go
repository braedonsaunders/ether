package resolver

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// TestSelectProfileForTitlePrecedence pins the per-library quality profile
// (#419): a title ranks with the profile set on its library, but a client
// rule and the Away profile still win, and an unknown or media-mismatched
// library profile falls through to today's defaults.
func TestSelectProfileForTitlePrecedence(t *testing.T) {
	movies := core.QualityProfile{Name: "Movies", MediaType: core.MediaMovie, MaxHeight: 2160}
	anime := core.QualityProfile{Name: "Anime", MediaType: core.MediaMovie, MaxHeight: 1080}
	tv := core.QualityProfile{Name: "Shows", MediaType: core.MediaTV, MaxHeight: 1080}
	away := core.QualityProfile{Name: "Away", MediaType: core.MediaAny, MaxHeight: 720}
	rule := core.QualityProfile{Name: "Rule", MediaType: core.MediaAny, MaxHeight: 480}

	base := config.Quality{
		Profiles:     []core.QualityProfile{movies, anime, tv, away, rule},
		DefaultMovie: "Movies",
		DefaultTV:    "Shows",
		Remote:       "Away",
	}
	movieTitle := &core.Title{Key: core.MovieKey(1), Type: core.TypeMovie}
	showTitle := &core.Title{Key: core.EpisodeKey(2, 1, 1), Type: core.TypeEpisode}

	tests := []struct {
		name       string
		q          config.Quality
		caps       core.ClientCaps
		user       *core.User
		title      *core.Title
		libProfile string
		want       string
	}{
		{name: "library profile wins over the movie default", q: base, title: movieTitle, libProfile: "Anime", want: "Anime"},
		{name: "empty library profile falls back to the default", q: base, title: movieTitle, want: "Movies"},
		{name: "unknown library profile falls through", q: base, title: movieTitle, libProfile: "Nope", want: "Movies"},
		{name: "library profile never crosses media", q: base, title: showTitle, libProfile: "Anime", want: "Shows"},
		{name: "a client rule beats the library profile", q: config.Quality{
			Profiles:     base.Profiles,
			DefaultMovie: "Movies",
			Rules:        []config.QualityRule{{Product: "Apple TV", Profile: "Rule"}},
		}, caps: core.ClientCaps{Product: "Apple TV"}, title: movieTitle, libProfile: "Anime", want: "Rule"},
		{name: "the Away profile beats the library profile", q: base, caps: core.ClientCaps{Remote: true}, title: movieTitle, libProfile: "Anime", want: "Away"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SelectProfileForTitle(tc.q, tc.caps, tc.user, tc.title, tc.libProfile)
			if got.Name != tc.want {
				t.Fatalf("SelectProfileForTitle = %q, want %q", got.Name, tc.want)
			}
		})
	}
}
