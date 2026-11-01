package playback

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestFallbackCannotOpenConflictingEpisodeIdentity(t *testing.T) {
	h := newHarness(t, nil, nil)
	s := &session{title: &core.Title{Type: core.TypeEpisode, ShowYear: 2005, Title: "Stone and Sky"}}
	wrong := core.Release{ID: "wrong", Name: "Space.Pilot.2023.S02E02.1080p.WEB"}
	_, err := h.m.openGeneration(t.Context(), s, &core.Resolution{Release: wrong}, nil)
	if !errors.Is(err, core.ErrNotAvailable) || h.engine.openCount() != 0 {
		t.Fatalf("conflicting fallback reached engine: err=%v opens=%d", err, h.engine.openCount())
	}
	if errors.Is(err, core.ErrReleaseRejected) {
		t.Fatal("identity mismatch must not blacklist a release valid for another show")
	}
}

func TestAlternatesSkipConflictingEpisodeBeforeFetching(t *testing.T) {
	h := newHarness(t, nil, nil)
	s := &session{title: &core.Title{Type: core.TypeEpisode, ShowYear: 2005, Title: "Stone and Sky"}}
	var fetched []string
	h.m.deps.FetchNZB = func(_ context.Context, rel core.Release) ([]byte, error) {
		fetched = append(fetched, rel.ID)
		return []byte("nzb-" + rel.ID), nil
	}
	res, err := h.m.nextResolution(t.Context(), s, nil, []core.Release{
		{ID: "reboot", Name: "Space.Pilot.2023.S02E02.1080p.WEB"},
		{ID: "other-episode", Name: "Space.Pilot.S02E02.Other.Story.1080p.WEB"},
		{ID: "correct", Name: "Space.Pilot.2005.S02E02.1080p.WEB"},
	})
	if err != nil || res == nil || res.Release.ID != "correct" || len(fetched) != 1 || fetched[0] != "correct" {
		t.Fatalf("alternate identity: res=%+v err=%v fetched=%v", res, err, fetched)
	}
}

func TestCachedTorrentRequiresCurrentEpisodeTarget(t *testing.T) {
	title := &core.Title{Type: core.TypeEpisode, ShowYear: 2005, Title: "Stone and Sky"}
	rel := core.Release{Name: "Space.Pilot.2005.S02E02.1080p.WEB", Protocol: core.ProtocolTorrent}
	if releaseMatchesTitle(rel, title) {
		t.Fatal("old descriptor without an episode identity was reusable")
	}
	rel.Target = &core.ReleaseTarget{ShowYear: 2005, EpisodeTitle: "Stone and Sky"}
	if !releaseMatchesTitle(rel, title) {
		t.Fatal("current identity target was rejected")
	}
	rel.Target.EpisodeTitle = "Other Story"
	if releaseMatchesTitle(rel, title) {
		t.Fatal("descriptor for another episode was reusable")
	}
}

func TestCachedAndFallbackReleasesRecheckReportedIDs(t *testing.T) {
	title := &core.Title{Key: core.EpisodeKey(123, 2, 2), Type: core.TypeEpisode, ShowYear: 2005, Title: "Stone and Sky"}
	r := core.Release{Name: "Pilote.Spatial.S02E02.Pierre.et.Ciel.1080p.WEB", TitleIDs: core.IDs{TMDB: 123}}
	if !releaseMatchesTitle(r, title) {
		t.Fatal("canonical series ID did not support alternate episode name")
	}
	r.Name = "Space.Pilot.2005.S02E03.1080p.WEB"
	if releaseMatchesTitle(r, title) {
		t.Fatal("matching ID bypassed wrong cached episode")
	}
	title.ShowIDs = core.IDs{TVDB: 789} // a partially hydrated series identity
	r.Name = "Space.Pilot.2005.S02E02.1080p.WEB"
	r.TitleIDs.TMDB = 456
	if releaseMatchesTitle(r, title) {
		t.Fatal("conflicting series ID reached open")
	}
	title.Type, title.IDs = core.TypeMovie, core.IDs{TMDB: 123}
	if releaseMatchesTitle(r, title) {
		t.Fatal("conflicting movie ID reached open")
	}
}

func TestPayloadMatchesTitleAndZeroByteRangeFailureReturns503(t *testing.T) {
	title := &core.Title{Key: core.EpisodeKey(33238, 1, 1), Type: core.TypeEpisode, Season: 1, Episode: 1, ShowYear: 2010, Title: "Times Square"}
	wrongPayload := []byte("ETHER-DEBRID/1\n{\"path\":\"Running Man Episode 1-129 English Subbed 720P/[iSUBS]Running.Man.E43.720P.avi\",\"size\":2748515170}")
	rightPayload := []byte("ETHER-DEBRID/1\n{\"path\":\"Running Man Episode 1-129 English Subbed 720P/[iSUBS]Running.Man.E01.720P.avi\",\"size\":1491294208}")
	if payloadMatchesTitle(wrongPayload, title) {
		t.Fatal("wrong episode file in pack payload matched S01E01")
	}
	if !payloadMatchesTitle(rightPayload, title) {
		t.Fatal("matching episode file in pack payload was rejected")
	}

	// A direct-play range request that fails before writing its first byte must
	// answer 503 (never a 0-byte 206 Partial Content that players treat as EOF).
	a := mkv("A", 200000, 100*time.Second)
	h := newHarness(t, []*fakeRelease{a}, []string{"A"})
	ps := h.prepare(owner, movieKey, "c1")
	g := h.m.lookup(ps.ID).current()
	<-g.indexReady
	g.src = retryDirectSource{g.src}
	a.setFail(100000, 200000, core.ErrSourceUnavailable)
	srv := directPlayServer(t, h.m, ps.ID)
	resp, body, err := get(t, srv.URL, map[string]string{"Range": "bytes=120000-129999"})
	if err != nil {
		t.Fatalf("range request failed: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Content-Range") != "" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("expected 503 without Content-Range on 0-byte failure, got status=%d headers=%v body=%q", resp.StatusCode, resp.Header, body)
	}
}
