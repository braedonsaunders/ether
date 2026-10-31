package resolver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// appleTV4K is a 4K HDR/DV client that picks the default 4K profile.
func appleTV4K() core.ClientCaps {
	return core.ClientCaps{Product: "Plex for Apple TV", MaxHeight: 2160, HDR10: true, DolbyVision: true,
		VideoCodecs: []string{"h264", "hevc"}, AudioCodecs: []string{"aac", "ac3", "eac3", "truehd"}}
}

// rankFor ranks releases (names parsed, sizes in MiB) for a 4K Apple TV
// viewer of title and returns the accepted IDs in try order and the trace.
func rankFor(t *testing.T, title *core.Title, user *core.User, releases ...core.Release) ([]string, Trace) {
	t.Helper()
	cfg := testCfg()
	s := New(&fakeIndexer{}, fakeHealth{}, nil, cfg, Options{})
	t.Cleanup(func() { s.Close() })
	caps := appleTV4K()
	profile := SelectProfile(cfg.Quality, caps, user)
	if profile.Name != "4K" {
		t.Fatalf("profile %q, want the default 4K profile", profile.Name)
	}
	for i := range releases {
		releases[i] = withParsed(releases[i])
		releases[i].PubDate = time.Time{}
	}
	var trace Trace
	cands, _, err := s.rankCandidates(context.Background(), core.ResolveRequest{Title: title, Client: caps, User: user}, searchRequestFor(title), profile, releases, &trace, "")
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(cands))
	for i, c := range cands {
		ids[i] = c.rel.ID
	}
	return ids, trace
}

func signal2026() *core.Title {
	return &core.Title{Key: core.MovieKey(9001), Type: core.TypeMovie, IDs: core.IDs{TMDB: 9001},
		Title: "The Signal", Year: 2026, Runtime: 120 * time.Minute, Language: "en"}
}

// "LAT" is Latin American Spanish audio. A 4K Latino release must not beat
// an English (untagged) 4K release for an English-speaking server, even
// with no languages set on the profile.
func TestForeignAudioRanksAfterAcceptedLanguage(t *testing.T) {
	ids, _ := rankFor(t, signal2026(), nil,
		rel("lat", "The.Signal.2026.2160p.AMZN.WEB-DL.DV.HDR10+.LAT.DDP5.1.H265.MP4-BTM", 18<<10),
		rel("eng", "The.Signal.2026.2160p.iT.WEB-DL.DDP5.1.Atmos.DV.HDR.H.265-BYNDR", 16<<10),
		rel("eng1080", "The.Signal.2026.1080p.WEB-DL.DDP5.1.H.264-GRP", 8<<10),
	)
	if len(ids) != 3 || ids[0] != "eng" || ids[2] != "lat" {
		t.Fatalf("order %v, want eng first and the Latino release last", ids)
	}
}

// The viewer's own audio languages decide when the profile has none, and
// the title's original language is always acceptable.
func TestViewerAudioLanguagesAndOriginalLanguage(t *testing.T) {
	title := signal2026()
	title.Language = "ko"
	viewer := &core.User{ID: 7, AudioLanguages: []string{"fr"}}
	ids, _ := rankFor(t, title, viewer,
		rel("ita", "The.Signal.2026.2160p.WEB-DL.DV.HDR.ITA.DDP5.1.H265-GRP", 18<<10),
		rel("kor", "The.Signal.2026.1080p.WEB-DL.KOREAN.DDP5.1.H.264-GRP", 8<<10),
		rel("fre", "The.Signal.2026.1080p.WEB-DL.FRENCH.DDP5.1.H.264-GRP", 8<<10),
	)
	if len(ids) != 3 || ids[2] != "ita" {
		t.Fatalf("order %v, want the Italian-only release last for a French viewer of a Korean film", ids)
	}
}

// Subtitle declarations are not audio: "ITA.ENG.SUBS" is an Italian dub
// with English subtitles, and must not count as English audio.
func TestSubtitleTagIsNotAcceptedAudio(t *testing.T) {
	ids, _ := rankFor(t, signal2026(), nil,
		rel("itasub", "The.Signal.2026.2160p.WEB-DL.DV.HDR.ITA.ENG.SUBS.H265-GRP", 18<<10),
		rel("plain", "The.Signal.2026.1080p.WEB-DL.DDP5.1.H.264-GRP", 8<<10),
	)
	if len(ids) != 2 || ids[0] != "plain" {
		t.Fatalf("order %v, want the untagged release before the Italian dub", ids)
	}
}

// UHD remuxes average 600-800 MB/min. The default 4K profile's 600 MB/min
// ceiling must not turn them all away; a deliberately low ceiling stands.
func TestUHDRemuxFitsDefault4KCeiling(t *testing.T) {
	remux := rel("remux", "The.Signal.2026.2160p.UHD.BluRay.REMUX.DV.HDR.HEVC.TrueHD.Atmos.7.1-FraMeSToR", 80<<10) // ~683 MB/min
	encode := rel("encode", "The.Signal.2026.2160p.UHD.BluRay.x265.DV.HDR.TrueHD.Atmos.7.1-GRP", 25<<10)
	ids, _ := rankFor(t, signal2026(), nil, remux, encode)
	if len(ids) != 2 || ids[0] != "remux" {
		t.Fatalf("order %v, want the UHD remux first", ids)
	}
	info := withParsed(remux).Info
	if got := effectiveMaxSizeMBPerMin(600, info); got != uhdDiscMaxMBPerMin {
		t.Fatalf("default 4K ceiling for a UHD remux = %v", got)
	}
	if got := effectiveMaxSizeMBPerMin(300, info); got != 300 {
		t.Fatalf("a deliberate 300 MB/min ceiling became %v", got)
	}
	if got := effectiveMaxSizeMBPerMin(600, withParsed(rel("w", "The.Signal.2026.2160p.WEB-DL.H265-GRP", 1)).Info); got != 600 {
		t.Fatalf("a 4K WEB-DL ceiling became %v", got)
	}
}

// A "2160p" re-encode at a fraction of the bitrate its source needs ranks
// below a well-encoded 1080p, and among real 4K encodes the fuller one
// wins instead of whichever indexer answered first.
func TestStarved4KRanksBelowGood1080p(t *testing.T) {
	ids, trace := rankFor(t, signal2026(), nil,
		rel("tiny4k", "The.Signal.2026.2160p.BDRip.TrueHD.7.1.Atmos.DV.HDR10Plus.x265.10bit-MarkII", 5<<10), // ~6 Mbps
		rel("good4k", "The.Signal.2026.UHD.BluRay.2160p.DDP.Atmos.5.1.DV.HDR.x265-hallowed", 22<<10),        // ~26 Mbps
		rel("good1080", "The.Signal.2026.1080p.BluRay.DD5.1.x264-GRP", 12<<10),
	)
	if len(ids) != 3 || ids[0] != "good4k" || ids[2] != "tiny4k" {
		t.Fatalf("order %v, want good4k first and the starved 4K last", ids)
	}
	var tiny, good float64
	for _, c := range trace.Candidates {
		switch c.ReleaseID {
		case "tiny4k":
			tiny = c.Score
		case "good4k":
			good = c.Score
		}
	}
	if good-tiny < 30 {
		t.Fatalf("good 4K %.1f vs starved 4K %.1f: the bitrate floor did not apply", good, tiny)
	}
}

// Animation compresses well: a lean but honest 4K animated encode keeps
// its 4K rank.
func TestAnimationBitrateFloorIsLower(t *testing.T) {
	title := signal2026()
	title.Genres = []string{"Animation"}
	ids, _ := rankFor(t, title, nil,
		rel("anim4k", "The.Signal.2026.2160p.BluRay.x265.10bit.HDR.DDP5.1-GRP", 8<<10), // ~9 Mbps, floor 6
		rel("anim1080", "The.Signal.2026.1080p.BluRay.x264.DDP5.1-GRP", 10<<10),
	)
	if len(ids) != 2 || ids[0] != "anim4k" {
		t.Fatalf("order %v, want the animated 4K first", ids)
	}
}

// A title with a 1080p remux but no UHD disc gets 4K files made from the
// 1080p disc: upscales, or 60 fps interpolations. They rank as 1080p, below
// the remux, and anything that says it was upscaled always does.
func TestPresumedUpscalesRankBelowHDRemux(t *testing.T) {
	title := &core.Title{Key: core.EpisodeKey(1437, 1, 5), Type: core.TypeEpisode, ShowIDs: core.IDs{TMDB: 1437},
		ShowTitle: "Firefly", ShowYear: 2002, Title: "Safe", Season: 1, Episode: 5, Runtime: 44 * time.Minute, Language: "en"}
	ids, trace := rankFor(t, title, nil,
		rel("fps", "Firefly.S01E05.2160p.60FPS.AAC.5.1.H265.10bit-Zero00", 3<<10),
		rel("bd4k", "Firefly.S01E05.2160p.BluRay.x265.10bit.HDR-GRP", 4<<10),
		rel("remux", "Firefly.S01E05.Safe.1080p.BluRay.REMUX.AVC.DTS-HD.MA.5.1-NOGRP", 7<<10),
	)
	if len(ids) != 3 || ids[0] != "remux" {
		t.Fatalf("order %v, want the 1080p remux first", ids)
	}
	for _, c := range trace.Candidates {
		if c.ReleaseID != "remux" && !strings.Contains(c.CompatibilityReason, "presumed upscale") {
			t.Fatalf("%s not explained as a presumed upscale: %q", c.ReleaseID, c.CompatibilityReason)
		}
	}
	// With a UHD disc in the pool, 4K Blu-ray encodes are real again.
	ids, _ = rankFor(t, title, nil,
		rel("bd4k", "Firefly.S01E05.2160p.UHD.BluRay.x265.10bit.HDR-GRP", 6<<10),
		rel("uhdremux", "Firefly.S01E05.2160p.UHD.BluRay.REMUX.HDR.HEVC-GRP", 18<<10),
		rel("remux", "Firefly.S01E05.Safe.1080p.BluRay.REMUX.AVC.DTS-HD.MA.5.1-NOGRP", 7<<10),
	)
	if len(ids) != 3 || ids[0] != "uhdremux" || ids[2] != "remux" {
		t.Fatalf("order %v, want the UHD remux, the UHD encode, then the 1080p remux", ids)
	}
}

// Quality the profile ranks comes before the soft score: an encode does
// not pass a remux on post age, grabs or audio codec, and Dolby Vision
// leads untagged HDR within one source.
func TestProfileQualityOutranksSoftScore(t *testing.T) {
	encode := rel("encode", "The.Signal.2026.2160p.BluRay.DDP2.0.HDR.x265-GRP", 17<<10)
	encode.Grabs = 5000
	remux := rel("remux", "The.Signal.2026.2160p.UHD.BluRay.Remux.HDR10.HEVC.DTS-HD.MA.2.0-GRP", 58<<10)
	remux.Grabs = 1
	ids, _ := rankFor(t, signal2026(), nil, encode, remux)
	if len(ids) != 2 || ids[0] != "remux" {
		t.Fatalf("order %v, want the remux first", ids)
	}
	ids, _ = rankFor(t, signal2026(), nil,
		rel("proper", "The.Signal.2026.PROPER.2160p.BluRay.REMUX.HEVC.DTS-HD.MA.TrueHD.7.1.Atmos-FGT", 53<<10),
		rel("dv", "The.Signal.2026.UHD.BluRay.2160p.TrueHD.Atmos.7.1.DV.HEVC.HYBRiD.REMUX-FraMeSToR", 46<<10),
	)
	if len(ids) != 2 || ids[0] != "dv" {
		t.Fatalf("order %v, want the Dolby Vision remux first for a DV client", ids)
	}
}

// A preferred source (debrid here) only goes first among releases of the
// same quality: never a cached torrent encode over a Usenet remux, nor a
// foreign-language torrent over an accepted one.
func TestSourcePreferenceKeepsQualityClass(t *testing.T) {
	cfg := testCfg()
	cfg.Resolver.SourcePreference = PreferDebrid
	s := New(&fakeIndexer{}, fakeHealth{}, nil, cfg, Options{})
	t.Cleanup(func() { s.Close() })
	caps := appleTV4K()
	profile := SelectProfile(cfg.Quality, caps, nil)
	torrent := func(r core.Release, hash string) core.Release {
		r.Protocol, r.InfoHash, r.NZBURL = core.ProtocolTorrent, hash, ""
		return r
	}
	rels := []core.Release{
		withParsed(rel("usenet-remux", "The.Signal.2026.2160p.UHD.BluRay.Remux.HDR10.HEVC.DTS-HD.MA.2.0-GRP", 58<<10)),
		withParsed(torrent(rel("torrent-encode", "The.Signal.2026.2160p.UHD.BluRay.x265.HDR-GRP", 28<<10), "aa")),
		withParsed(torrent(rel("torrent-lat", "The.Signal.2026.2160p.UHD.BluRay.Remux.HDR10.HEVC.LAT.DTS-HD.MA.2.0-GRP", 58<<10), "bb")),
	}
	title := signal2026()
	var trace Trace
	cands, _, err := s.rankCandidates(context.Background(), core.ResolveRequest{Title: title, Client: caps}, searchRequestFor(title), profile, rels, &trace, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 3 || cands[0].rel.ID != "usenet-remux" || cands[2].rel.ID != "torrent-lat" {
		got := []string{}
		for _, c := range cands {
			got = append(got, c.rel.ID)
		}
		t.Fatalf("order %v, want the Usenet remux first and the Latino remux last", got)
	}
}

// A play below 4K says why, in the trace and the log.
func TestBelow4KIsExplained(t *testing.T) {
	cfg := testCfg()
	idx := &fakeIndexer{releases: []core.Release{
		withParsed(rel("tiny4k", "The.Signal.2026.2160p.BDRip.x265.10bit.DV.HDR-MarkII", 5<<10)),
		withParsed(rel("good1080", "The.Signal.2026.1080p.BluRay.DD5.1.x264-GRP", 12<<10)),
	}}
	s := New(idx, fakeHealth{}, nil, cfg, Options{})
	defer s.Close()
	if _, err := s.Resolve(context.Background(), core.ResolveRequest{Title: signal2026(), Client: appleTV4K()}); err != nil {
		t.Fatal(err)
	}
	tr := s.Traces()
	if got := tr[0].Ceiling; !strings.Contains(got, "too little bitrate for 4K") {
		t.Fatalf("ceiling %q", got)
	}
	// A client without a 4K rule gets the default 1080p profile, and says so.
	web := core.ClientCaps{Product: "Plex Web", MaxHeight: 2160, VideoCodecs: []string{"h264", "hevc"}}
	if _, err := s.Resolve(context.Background(), core.ResolveRequest{Title: signal2026(), Client: web}); err != nil {
		t.Fatal(err)
	}
	tr = s.Traces()
	if got := tr[0].Ceiling; !strings.Contains(got, "no client rule matched this client (Plex Web)") {
		t.Fatalf("ceiling %q", got)
	}
}

// An Italian dub names its episode in Italian; that is a translation of
// "Judgment Day", not another episode. An English name with a different
// title still is another episode.
func TestLocalizedEpisodeTitleIsNotAMismatch(t *testing.T) {
	title := &core.Title{Key: core.EpisodeKey(95480, 6, 6), Type: core.TypeEpisode, ShowIDs: core.IDs{TMDB: 95480},
		ShowTitle: "Slow Horses", ShowYear: 2022, Title: "Judgment Day", Season: 6, Episode: 6, Runtime: 60 * time.Minute, Language: "en"}
	sreq := searchRequestFor(title)
	ita := withParsed(rel("ita", "Slow.Horses.S06E06.La.resa.dei.conti.ITA.ENG.2160p.ATVP.WEB-DL.DD5.1.DV.HDR.H.265-MeM.GP", 11<<10))
	if got := rejectIdentity(ita, title, sreq, "en-US"); got != "" {
		t.Fatalf("Italian episode title rejected: %q", got)
	}
	other := withParsed(rel("other", "Slow.Horses.S06E06.Hard.Lessons.2160p.ATVP.WEB-DL.DDP5.1.DV.H.265-GRP", 11<<10))
	if got := rejectIdentity(other, title, sreq, "en-US"); got != "episode title mismatch" {
		t.Fatalf("different English episode title: %q", got)
	}
}

// Anime releases go by the romanized original; the catalogue's alternative
// titles let them match. An unrelated name still does not.
func TestAlternativeTitleMatchesRomanizedRelease(t *testing.T) {
	title := &core.Title{Key: core.EpisodeKey(209867, 1, 1), Type: core.TypeEpisode, ShowIDs: core.IDs{TMDB: 209867},
		ShowTitle: "Frieren: Beyond Journey's End", ShowOriginalTitle: "葬送のフリーレン", ShowYear: 2023, Title: "The Journey's End",
		Season: 1, Episode: 1, Runtime: 24 * time.Minute, Language: "ja", ShowAlternativeTitles: []string{"Frieren", "Sousou no Frieren"}}
	sreq := searchRequestFor(title)
	romaji := withParsed(rel("romaji", "Sousou.no.Frieren.S01E01.REPACK.1080p.BluRay.Remux.MULTi.FLAC.2.0.AVC-AUTISM", 8<<10))
	if got := rejectIdentity(romaji, title, sreq, "en-US"); got != "" {
		t.Fatalf("romanized release rejected: %q", got)
	}
	other := withParsed(rel("other", "Dungeon.Meshi.S01E01.1080p.BluRay.Remux-GRP", 8<<10))
	if got := rejectIdentity(other, title, sreq, "en-US"); got != "title mismatch" {
		t.Fatalf("unrelated release: %q", got)
	}
	title.ShowAlternativeTitles = nil
	if got := rejectIdentity(romaji, title, searchRequestFor(title), "en-US"); got != "title mismatch" {
		t.Fatalf("without alternative titles the romanized release should not match: %q", got)
	}
}
