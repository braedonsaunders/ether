package resolver

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
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

type invariantLibraryProfile struct{ name string }

func (p *invariantLibraryProfile) QualityProfileForTitle(context.Context, *core.Title) string {
	return p.name
}
func (p *invariantLibraryProfile) PolicyKey(context.Context) string { return p.name }

func TestLibraryProfileEvidenceInvariant(t *testing.T) {
	cfg := testCfg()
	cfg.Quality = config.Quality{Profiles: []core.QualityProfile{
		{Name: "Home", MediaType: core.MediaMovie, MaxHeight: 2160, AllowHDR: true},
		{Name: "Library", MediaType: core.MediaMovie, MaxHeight: 720},
	}, DefaultMovie: "Home"}
	lookup := &invariantLibraryProfile{name: "Library"}
	s := New(&fakeIndexer{}, nil, nil, cfg, Options{LibraryProfiles: lookup})
	defer s.Close()
	ctx := t.Context()
	title := matrixTitle()
	r := rel("1080", "The.Signal.1999.1080p.WEB-DL.x264-GRP", 4000)
	r.Info = releaseparse.Parse(r.Name)
	_, reason, err := s.FeedCandidate(ctx, title, r)
	if err != nil || reason == "" {
		t.Fatalf("feed ignored library: %q %v", reason, err)
	}
	ev, err := s.FeedEvaluation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oldKey := ev.Key()
	verdicts, err := ev.Evaluate(ctx, title, []core.Release{r})
	ev.Close()
	if err != nil || len(verdicts) != 1 || verdicts[0].Reason == "" {
		t.Fatalf("batch feed ignored library: %+v %v", verdicts, err)
	}
	s.setAvailability(ctx, int64(title.Key), false, time.Hour)
	lookup.name = "Home"
	if known, _ := s.Available(ctx, title.Key); known {
		t.Fatal("old library availability survived profile change")
	}
	if got := s.KnownUnavailableSince(ctx, []core.RatingKey{title.Key}); len(got) != 0 {
		t.Fatalf("old negative policy survived: %v", got)
	}
	ev, err = s.FeedEvaluation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ev.Close()
	if oldKey == ev.Key() {
		t.Fatal("library policy did not invalidate durable feed evidence")
	}
	verdicts, err = ev.Evaluate(ctx, title, []core.Release{r})
	if err != nil || verdicts[0].Reason != "" {
		t.Fatalf("home policy rejected release: %+v %v", verdicts, err)
	}
	for _, title := range []*core.Title{{Type: core.TypeShow}, {Type: core.TypeSeason}, {Key: core.ShowKey(1)}, {Key: core.SeasonKey(1, 1)}} {
		if got := titleMedia(title); got != core.MediaTV {
			t.Fatalf("show media = %q", got)
		}
	}
}
