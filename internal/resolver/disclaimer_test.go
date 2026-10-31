package resolver

import (
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"testing"
)

func TestMovieDirectorDisclaimer(t *testing.T) {
	title := &core.Title{Type: core.TypeMovie, Title: "The Odyssey", Year: 2026, IDs: core.IDs{TMDB: 123}, Directors: []core.Person{{Name: "Chris Nolan"}}}
	req := core.SearchRequest{Type: core.TypeMovie, Title: title.Title, Year: title.Year, IDs: title.IDs}
	for _, tc := range []struct {
		name      string
		ids       core.IDs
		directors []core.Person
		want      string
	}{
		{"The Odyssey 2026 NOT The Chris Nolan FILM 1080p WEB-DL HEVC x265 5 synthetic", core.IDs{}, title.Directors, "director disclaimer"},
		{"The Odyssey 2026 NOT The Chris Nolan FILM 1080p WEB-DL", core.IDs{TMDB: 123}, title.Directors, "director disclaimer"},
		{"The Odyssey 2026 NOT The Chris Nolan FILM 1080p WEB-DL", core.IDs{TMDB: 456}, title.Directors, "indexer title identifier mismatch"},
		{"The Odyssey 2026 NOT The Chris Nolan FILM 1080p WEB-DL", core.IDs{}, nil, ""},
		{"The Odyssey 2026 NOT The Chris Nolan FILM 1080p WEB-DL", core.IDs{}, []core.Person{{Name: "Christopher Nolan"}}, ""},
		{"The Odyssey 2026 NOT The Other Director FILM 1080p WEB-DL", core.IDs{}, title.Directors, ""},
		{"The Odyssey 2026 NOT HDR 1080p WEB-DL", core.IDs{}, title.Directors, ""},
		{"The Odyssey 2026 1080p WEB-DL NOT The Chris Nolan FILM", core.IDs{}, title.Directors, ""},
	} {
		t.Run(tc.name+tc.want, func(t *testing.T) {
			copyTitle := *title
			copyTitle.Directors = tc.directors
			r := core.Release{Name: tc.name, Info: releaseparse.Parse(tc.name), TitleIDs: tc.ids}
			if got := rejectIdentity(r, &copyTitle, req, ""); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestDisclaimerDoesNotChangeEpisodesOrMatchingAlternateTitles(t *testing.T) {
	name := "The Odyssey 2026 NOT The Chris Nolan FILM S01E02 1080p WEB-DL"
	title := &core.Title{Type: core.TypeEpisode, Directors: []core.Person{{Name: "Chris Nolan"}}}
	req := core.SearchRequest{Type: core.TypeEpisode, Title: "The Odyssey", Year: 2026, Season: 1, Episode: 2}
	r := core.Release{Name: name, Info: releaseparse.Parse(name)}
	if got := rejectIdentity(r, title, req, ""); got != "" {
		t.Fatalf("episode rejected: %s", got)
	}
	title = &core.Title{Type: core.TypeMovie, OriginalTitle: "The Odyssey", Directors: []core.Person{{Name: "Other Director"}}}
	req = core.SearchRequest{Type: core.TypeMovie, Title: "Another Title", Year: 2026}
	r.Name = "The Odyssey 2026 NOT The Chris Nolan FILM 1080p WEB-DL"
	r.Info = releaseparse.Parse(r.Name)
	if got := rejectIdentity(r, title, req, ""); got != "" {
		t.Fatalf("alternate title rejected: %s", got)
	}
}

func TestDirectorDisclaimerAppliesToAnyMovie(t *testing.T) {
	title := &core.Title{Type: core.TypeMovie, Title: "Dune", Year: 2021, Directors: []core.Person{{Name: "Denis Villeneuve"}}}
	req := core.SearchRequest{Type: core.TypeMovie, Title: title.Title, Year: title.Year}
	name := "Dune.2021.NOT.The.Denis.Villeneuve.FILM.1080p.WEB-DL.x264-GRP"
	r := core.Release{Name: name, Info: releaseparse.Parse(name)}
	if got := rejectIdentity(r, title, req, ""); got != "director disclaimer" {
		t.Fatalf("got %q", got)
	}
	name = "Dune.2021.1080p.WEB-DL.x264-GRP"
	r = core.Release{Name: name, Info: releaseparse.Parse(name)}
	if got := rejectIdentity(r, title, req, ""); got != "" {
		t.Fatalf("plain release rejected: %q", got)
	}
}
