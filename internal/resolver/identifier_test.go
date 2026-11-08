package resolver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

func TestReportedIDsDisambiguateWithoutBypassingEpisodeOrYear(t *testing.T) {
	title := &core.Title{Key: core.EpisodeKey(123, 2, 2), Type: core.TypeEpisode, Title: "Stone and Sky", ShowTitle: "Space Pilot", ShowYear: 2005, ShowIDs: core.IDs{TMDB: 123}, Season: 2, Episode: 2, Runtime: 45 * time.Minute}
	cfg := testCfg()
	svc := New(&fakeIndexer{}, fakeHealth{}, nil, cfg, Options{})
	profile := SelectProfile(cfg.Quality, webCaps(), nil)
	for _, tt := range []struct {
		name, release string
		ids           core.IDs
		want          string
	}{
		{"translated title with evidence", "Pilote.Spatial.S02E02.Pierre.et.Ciel.1080p.WEB.H264-GRP", core.IDs{TMDB: 123}, ""},
		{"query ID alone proves nothing", "Pilote.Spatial.S02E02.1080p.WEB.H264-GRP", core.IDs{}, "title mismatch"},
		{"obfuscated without reported ID", "a1b2c3d4e5f6g7h8.S02E02.1080p.WEB.H264-GRP", core.IDs{}, "title mismatch"},
		{"conflicting ID with exact name", "Space.Pilot.S02E02.1080p.WEB.H264-GRP", core.IDs{TMDB: 456}, "indexer title identifier mismatch"},
		{"wrong episode despite matching ID", "Space.Pilot.S02E03.1080p.WEB.H264-GRP", core.IDs{TMDB: 123}, "season/episode mismatch"},
		{"wrong reboot despite matching ID", "Space.Pilot.2023.S02E02.1080p.WEB.H264-GRP", core.IDs{TMDB: 123}, "series year mismatch"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := rel("r", tt.release, 1500)
			r.Info, r.TitleIDs = releaseparse.Parse(r.Name), tt.ids
			if got := svc.rejectReason(r, title, searchRequestFor(title), profile, cfg.Resolver, webCaps(), nil, false); got != tt.want {
				t.Fatalf("rejection = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTorrentTargetRetainsReportedIdentityWithoutInventingIt(t *testing.T) {
	req := core.SearchRequest{Type: core.TypeEpisode, Season: 2, Episode: 2, IDs: core.IDs{TMDB: 123}}
	release := core.Release{ID: "r", Protocol: core.ProtocolTorrent, Info: core.ReleaseInfo{FullSeason: true}, TitleIDs: core.IDs{TMDB: 123, IMDB: "tt0000123"}}
	got := withTarget(release, req)
	id := got.ID
	got = withTarget(got, req)
	if got.Target.ExpectedIDs != req.IDs || got.Target.ReportedIDs != release.TitleIDs || got.ID != id {
		t.Fatalf("target identity changed: %+v", got.Target)
	}
	release.TitleIDs = core.IDs{}
	if target := withTarget(release, req).Target; target.ReportedIDs != (core.IDs{}) {
		t.Fatal("request IDs became reported IDs")
	}
}

func TestSeriesSearchNeverUsesEpisodeExternalIDs(t *testing.T) {
	title := &core.Title{Type: core.TypeEpisode, Key: core.EpisodeKey(123, 2, 2), IDs: core.IDs{TMDB: 999, IMDB: "tt0000999", TVDB: 999}}
	if got := searchRequestFor(title).IDs; got != (core.IDs{TMDB: 123}) {
		t.Fatalf("episode IDs became series identifiers: %+v", got)
	}
	title.ShowIDs = core.IDs{TVDB: 456}
	if got := searchRequestFor(title).IDs; got != (core.IDs{TMDB: 123, TVDB: 456}) {
		t.Fatalf("partial series identifiers lost canonical TMDB key: %+v", got)
	}
}

// A differing series year rejects only when it names another show (a reboot);
// an indexer's wrong year is accepted and ranks after correctly-labelled
// releases of the same resolution. Unknown answers keep the year check.
func TestSeriesYearDisambiguatesInsteadOfFiltering(t *testing.T) {
	title := &core.Title{Key: core.EpisodeKey(123, 2, 2), ShowKey: core.ShowKey(123), Type: core.TypeEpisode, Title: "Stone and Sky", ShowTitle: "Space Pilot", ShowYear: 2005, ShowIDs: core.IDs{TMDB: 123}, Season: 2, Episode: 2, Runtime: 45 * time.Minute}
	cfg := testCfg()
	lookups := 0
	svc := New(&fakeIndexer{}, fakeHealth{}, nil, cfg, Options{SeriesYearConflict: func(_ context.Context, show core.RatingKey, _ string, year int) (bool, error) {
		lookups++
		if show != title.ShowKey {
			t.Fatalf("lookup for show %v", show)
		}
		switch year {
		case 2023:
			return true, nil // the reboot exists
		case 2006:
			return false, nil // indexer listed the show a year late
		}
		return false, errors.New("lookup failed")
	}})
	profile := SelectProfile(cfg.Quality, webCaps(), nil)
	sreq := searchRequestFor(title)
	mk := func(id, name string) core.Release {
		r := rel(id, name, 1500)
		r.Info = releaseparse.Parse(r.Name)
		return r
	}
	wrong, reboot, unknown, right := mk("w", "Space.Pilot.2006.S02E02.1080p.WEB.H264-GRP"), mk("r", "Space.Pilot.2023.S02E02.1080p.WEB.H264-GRP"), mk("u", "Space.Pilot.1999.S02E02.1080p.WEB.H264-GRP"), mk("ok", "Space.Pilot.2005.S02E02.1080p.WEB.H264-GRP")
	svc.prepareSeriesYears(context.Background(), title, sreq, []core.Release{wrong, reboot, unknown, right, wrong})
	if lookups != 3 {
		t.Fatalf("lookups = %d, want one per differing year", lookups)
	}
	for _, tc := range []struct {
		r    core.Release
		want string
	}{{wrong, ""}, {reboot, "series year mismatch"}, {unknown, "series year mismatch"}, {right, ""}} {
		r, want := tc.r, tc.want
		if got := svc.rejectReason(r, title, sreq, profile, cfg.Resolver, webCaps(), nil, false); got != want {
			t.Fatalf("%s: rejection = %q, want %q", r.Name, got, want)
		}
	}
	cands, _, err := svc.rankCandidates(context.Background(), core.ResolveRequest{Title: title, Client: webCaps()}, sreq, profile, []core.Release{wrong, right}, nil, "")
	if err != nil || len(cands) != 2 || cands[0].rel.ID != "ok" {
		t.Fatalf("correct year must rank first: %v %+v", err, cands)
	}
	if lookups != 3 {
		t.Fatalf("cached verdicts were looked up again: %d", lookups)
	}
}
