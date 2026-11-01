package resolver

import (
	"context"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/releasepolicy"
)

// legacyOrderCandidates reproduces the pre-ladder ordering from origin/prism
// verbatim so the golden test can prove byte-identical candidate ordering and
// winners under the default Ranking ladder.
func (s *Service) legacyOrderCandidates(cands []candidate, title *core.Title, trace *Trace) []candidate {
	out := slices.Clone(cands)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].langOK != out[j].langOK {
			return out[i].langOK
		}
		if out[i].bandwidth != out[j].bandwidth {
			return out[i].bandwidth > out[j].bandwidth
		}
		if out[i].quality.better(out[j].quality) != out[j].quality.better(out[i].quality) {
			return out[i].quality.better(out[j].quality)
		}
		if out[i].verified != out[j].verified {
			return out[i].verified
		}
		return out[i].rel.Score > out[j].rel.Score
	})
	out = s.routeSources(out, title, trace)
	sort.SliceStable(out, func(i, j int) bool { return releasepolicy.Better(out[i].policy, out[j].policy) })
	return out
}

func TestDefaultRankingLadderGoldenParity(t *testing.T) {
	ctx := context.Background()
	title := matrixTitle()
	title.Language = "en"

	pool := []core.Release{
		rel("4k-remux", "The.Signal.1999.2160p.UHD.BluRay.REMUX.HDR.HEVC.TrueHD.7.1.Atmos-FRAMeSToR", 62000),
		rel("4k-webdl-dv", "The.Signal.1999.2160p.MA.WEB-DL.DDP5.1.Atmos.DV.HDR.H.265-FLUX", 18000),
		rel("4k-webdl-hdr", "The.Signal.1999.2160p.WEB-DL.DDP5.1.HDR.HEVC-NTb", 15000),
		rel("4k-starved", "The.Signal.1999.2160p.BluRay.x265-YTS", 2200),
		rel("1080p-remux", "The.Signal.1999.1080p.BluRay.REMUX.AVC.TrueHD.5.1-EPSiLON", 28000),
		rel("1080p-bluray", "The.Signal.1999.1080p.BluRay.x264.DTS-DON", 12000),
		rel("1080p-webdl-verified", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-NTb", 7500),
		rel("1080p-webdl-flux", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-FLUX", 7400),
		rel("1080p-foreign", "The.Signal.1999.FRENCH.1080p.WEB-DL.DDP5.1.H.264-GRP", 7600),
		rel("1080p-cam", "The.Signal.1999.1080p.CAM.x264-BAD", 3000),
		rel("1080p-avoid-grp", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-BANNED", 7300),
	}
	for i := range pool {
		pool[i].Info = releaseparse.Parse(pool[i].Name)
	}

	rules := []core.ReleaseRule{
		{
			ID:     "avoid-banned",
			Name:   "Avoid BANNED",
			Action: "avoid",
			Conditions: []core.ReleaseCondition{
				{Field: "group", Operator: "equals", Value: "BANNED"},
			},
		},
		{
			ID:     "req-non-cam",
			Name:   "Require modern codec",
			Action: "require",
			Match:  "any",
			Conditions: []core.ReleaseCondition{
				{Field: "video", Operator: "equals", Value: "hevc"},
				{Field: "video", Operator: "equals", Value: "h264"},
			},
		},
		{
			ID:     "pref-flux",
			Name:   "Prefer FLUX",
			Action: "prefer",
			Conditions: []core.ReleaseCondition{
				{Field: "group", Operator: "equals", Value: "FLUX"},
			},
		},
		{
			ID:     "pref-framestor",
			Name:   "Prefer FRAMeSToR",
			Action: "prefer",
			Conditions: []core.ReleaseCondition{
				{Field: "group", Operator: "equals", Value: "FRAMeSToR"},
			},
		},
	}

	baseProfile := core.QualityProfile{
		Name:               "4K",
		MinHeight:          720,
		MaxHeight:          2160,
		AllowHDR:           true,
		AllowDolbyVision:   true,
		PreferredSources:   []core.Source{core.SourceRemux, core.SourceBluRay, core.SourceWEBDL, core.SourceWEBRip},
		PreferredCodecs:    []string{"hevc", "h264", "av1"},
		PreferredAudio:     []string{"truehd", "eac3", "dtshd", "ac3", "aac"},
		PreferredLanguages: []string{"en"},
		MinSizeMBPerMin:    5,
		MaxSizeMBPerMin:    800,
		ReleaseRules:       rules,
	}

	cappedClient := appleTVCaps()
	cappedClient.MaxBitrateKbps = 20000 // 4k-remux and 1080p-remux exceed 20 Mbps; web-dls fit

	for _, tc := range []struct {
		name    string
		client  core.ClientCaps
		ranking []string
	}{
		{
			name:    "capable-4k-empty-ranking",
			client:  appleTVCaps(),
			ranking: nil,
		},
		{
			name:    "capable-4k-explicit-default-ranking",
			client:  appleTVCaps(),
			ranking: core.DefaultRanking(rules),
		},
		{
			name:    "capped-bandwidth-client",
			client:  cappedClient,
			ranking: core.DefaultRanking(rules),
		},
		{
			name:    "no-prefer-rules",
			client:  appleTVCaps(),
			ranking: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prof := baseProfile
			prof.Ranking = tc.ranking
			if tc.name == "no-prefer-rules" {
				prof.ReleaseRules = rules[:2]
			}
			cfg := testCfg()
			cfg.Quality.Default = prof.Name
			cfg.Quality.Profiles = []core.QualityProfile{prof}

			idx := &fakeIndexer{releases: pool}
			svc := New(idx, fakeHealth{}, nil, cfg, Options{})
			svc.mu.Lock()
			svc.verified["1080p-webdl-verified"] = time.Now()
			svc.mu.Unlock()

			req := core.ResolveRequest{Title: title, Client: tc.client}
			sreq := searchRequestFor(title)
			clamped := SelectProfile(cfg.Quality, tc.client, nil)

			var traceNew Trace
			candsNew, _, err := svc.rankCandidates(ctx, req, sreq, clamped, pool, &traceNew, "")
			if err != nil {
				t.Fatalf("rankCandidates: %v", err)
			}

			// Reconstruct pre-ordered candidates and run legacyOrderCandidates.
			// Since rankCandidates calls orderCandidates at the end, sort input
			// back to discovery order before calling legacyOrderCandidates.
			byID := map[string]candidate{}
			for _, c := range candsNew {
				byID[c.rel.ID] = c
			}
			var raw []candidate
			for _, r := range pool {
				if c, ok := byID[r.ID]; ok {
					raw = append(raw, c)
				}
			}
			candsLegacy := svc.legacyOrderCandidates(raw, title, nil)

			var gotIDs, wantIDs []string
			for _, c := range candsNew {
				gotIDs = append(gotIDs, c.rel.ID)
			}
			for _, c := range candsLegacy {
				wantIDs = append(wantIDs, c.rel.ID)
			}
			if !slices.Equal(gotIDs, wantIDs) {
				t.Fatalf("candidate order mismatch:\n got:  %v\n want: %v", gotIDs, wantIDs)
			}

			res, err := svc.Resolve(ctx, req)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if res.Release.ID != wantIDs[0] {
				t.Fatalf("winner = %q, want %q", res.Release.ID, wantIDs[0])
			}
		})
	}
}

func TestRankingLadderReordering(t *testing.T) {
	ctx := context.Background()
	title := matrixTitle()
	title.Language = "en"

	t.Run("releaseType above resolution makes 1080p Remux beat 2160p WEB-DL", func(t *testing.T) {
		releases := []core.Release{
			rel("2160p-webdl", "The.Signal.1999.2160p.WEB-DL.DDP5.1.HDR.HEVC-NTb", 16000),
			rel("1080p-remux", "The.Signal.1999.1080p.BluRay.REMUX.AVC.TrueHD.5.1-EPSiLON", 26000),
		}
		for i := range releases {
			releases[i].Info = releaseparse.Parse(releases[i].Name)
		}
		prof := core.QualityProfile{
			Name:             "RemuxFirst",
			MinHeight:        720,
			MaxHeight:        2160,
			AllowHDR:         true,
			AllowDolbyVision: true,
			PreferredSources: []core.Source{core.SourceRemux, core.SourceBluRay, core.SourceWEBDL},
			PreferredCodecs:  []string{"hevc", "h264"},
			PreferredAudio:   []string{"truehd", "eac3"},
			MinSizeMBPerMin:  5,
			MaxSizeMBPerMin:  800,
		}
		cfg := testCfg()
		cfg.Quality.Default = prof.Name
		cfg.Quality.Profiles = []core.QualityProfile{prof}

		// Default ladder: resolution > releaseType -> 2160p WEB-DL wins.
		svc := New(&fakeIndexer{releases: releases}, fakeHealth{}, nil, cfg, Options{})
		res, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: appleTVCaps()})
		if err != nil || res.Release.ID != "2160p-webdl" {
			t.Fatalf("default ladder winner = %+v, err = %v; want 2160p-webdl", res, err)
		}

		// Reordered ladder: releaseType > resolution -> 1080p Remux wins.
		prof.Ranking = []string{
			core.RankLanguage,
			core.RankConnection,
			core.RankReleaseType,
			core.RankResolution,
			core.RankHDR,
			core.RankVerified,
			core.RankVideoCodec,
			core.RankAudioFormat,
			core.RankSize,
		}
		cfg.Quality.Profiles = []core.QualityProfile{prof}
		svc2 := New(&fakeIndexer{releases: releases}, fakeHealth{}, nil, cfg, Options{})
		res2, err := svc2.Resolve(ctx, core.ResolveRequest{Title: title, Client: appleTVCaps()})
		if err != nil || res2.Release.ID != "1080p-remux" {
			t.Fatalf("reordered ladder winner = %+v, err = %v; want 1080p-remux", res2, err)
		}
	})

	t.Run("Prefer rule below resolution lets 2160p beat 1080p rule match and breaks 2160p ties", func(t *testing.T) {
		releases := []core.Release{
			rel("2160p-other", "The.Signal.1999.2160p.WEB-DL.DDP5.1.HEVC-OTHER", 16000),
			rel("2160p-flux", "The.Signal.1999.2160p.WEB-DL.DDP5.1.HEVC-FLUX", 15500),
			rel("1080p-flux", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-FLUX", 8000),
		}
		// Give 2160p-other more grabs so without the Prefer rule it beats 2160p-flux on soft score.
		releases[0].Grabs = 5000
		releases[1].Grabs = 10
		for i := range releases {
			releases[i].Info = releaseparse.Parse(releases[i].Name)
		}

		rule := core.ReleaseRule{
			ID:     "flux",
			Name:   "Prefer FLUX",
			Action: "prefer",
			Conditions: []core.ReleaseCondition{
				{Field: "group", Operator: "equals", Value: "FLUX"},
			},
		}
		prof := core.QualityProfile{
			Name:             "4K",
			MinHeight:        720,
			MaxHeight:        2160,
			AllowHDR:         true,
			AllowDolbyVision: true,
			PreferredSources: []core.Source{core.SourceWEBDL},
			PreferredCodecs:  []string{"hevc", "h264"},
			PreferredAudio:   []string{"eac3"},
			MinSizeMBPerMin:  5,
			MaxSizeMBPerMin:  800,
			ReleaseRules:     []core.ReleaseRule{rule},
		}

		// When only 2160p-other and 1080p-flux are available:
		// - Default ladder (rule:flux at top): 1080p-flux wins.
		// - Rule moved below resolution: 2160p-other wins.
		two := []core.Release{releases[0], releases[2]}
		cfg := testCfg()
		cfg.Quality.Default = prof.Name
		cfg.Quality.Profiles = []core.QualityProfile{prof}
		svcDefault := New(&fakeIndexer{releases: two}, fakeHealth{}, nil, cfg, Options{})
		resDef, err := svcDefault.Resolve(ctx, core.ResolveRequest{Title: title, Client: appleTVCaps()})
		if err != nil || resDef.Release.ID != "1080p-flux" {
			t.Fatalf("default ladder (rule at top) winner = %+v, err = %v; want 1080p-flux", resDef, err)
		}

		profBelow := prof
		profBelow.Ranking = []string{
			core.RankLanguage,
			core.RankConnection,
			core.RankResolution,
			core.RankRulePrefix + "flux",
			core.RankReleaseType,
			core.RankHDR,
			core.RankVerified,
			core.RankVideoCodec,
			core.RankAudioFormat,
			core.RankSize,
		}
		cfg.Quality.Profiles = []core.QualityProfile{profBelow}
		svcBelow := New(&fakeIndexer{releases: two}, fakeHealth{}, nil, cfg, Options{})
		resBelow, err := svcBelow.Resolve(ctx, core.ResolveRequest{Title: title, Client: appleTVCaps()})
		if err != nil || resBelow.Release.ID != "2160p-other" {
			t.Fatalf("rule below resolution winner = %+v, err = %v; want 2160p-other", resBelow, err)
		}

		// With all three releases, rule:flux below resolution picks 2160p-flux over 2160p-other,
		// and keeps 2160p-other ahead of 1080p-flux.
		svcAll := New(&fakeIndexer{releases: releases}, fakeHealth{}, nil, cfg, Options{})
		cands, _, err := svcAll.rankCandidates(ctx, core.ResolveRequest{Title: title, Client: appleTVCaps()}, searchRequestFor(title), SelectProfile(cfg.Quality, appleTVCaps(), nil), releases, nil, "")
		if err != nil {
			t.Fatalf("rankCandidates: %v", err)
		}
		got := []string{cands[0].rel.ID, cands[1].rel.ID, cands[2].rel.ID}
		want := []string{"2160p-flux", "2160p-other", "1080p-flux"}
		if !slices.Equal(got, want) {
			t.Fatalf("order = %v, want %v", got, want)
		}
	})

	t.Run("moving videoCodec, audioFormat, and size reorders candidates", func(t *testing.T) {
		// Two 2160p WEB-DL SDR releases:
		// - codec-win: AV1 + AAC, 14 GB
		// - audio-size-win: HEVC + TrueHD Atmos, 22 GB
		releases := []core.Release{
			rel("codec-win", "The.Signal.1999.2160p.WEB-DL.AAC2.0.AV1-GRP", 14000),
			rel("audio-size-win", "The.Signal.1999.2160p.WEB-DL.TrueHD.7.1.Atmos.HEVC-GRP", 22000),
		}
		for i := range releases {
			releases[i].Info = releaseparse.Parse(releases[i].Name)
		}
		base := core.QualityProfile{
			Name:             "4K",
			MinHeight:        720,
			MaxHeight:        2160,
			AllowHDR:         true,
			AllowDolbyVision: true,
			PreferredSources: []core.Source{core.SourceWEBDL},
			PreferredCodecs:  []string{"av1", "hevc", "h264"},
			PreferredAudio:   []string{"truehd", "eac3", "aac"},
			PreferLargest:    true,
			MinSizeMBPerMin:  5,
			MaxSizeMBPerMin:  800,
		}

		orderWith := func(ranking []string) []string {
			p := base
			p.Ranking = ranking
			cfg := testCfg()
			cfg.Quality.Default = p.Name
			cfg.Quality.Profiles = []core.QualityProfile{p}
			svc := New(&fakeIndexer{releases: releases}, fakeHealth{}, nil, cfg, Options{})
			cands, _, err := svc.rankCandidates(ctx, core.ResolveRequest{Title: title, Client: appleTVCaps()}, searchRequestFor(title), SelectProfile(cfg.Quality, appleTVCaps(), nil), releases, nil, "")
			if err != nil {
				t.Fatalf("rankCandidates: %v", err)
			}
			return []string{cands[0].rel.ID, cands[1].rel.ID}
		}

		// Move videoCodec above verified -> codec-win (AV1) leads.
		gotCodecFirst := orderWith([]string{
			core.RankLanguage,
			core.RankConnection,
			core.RankResolution,
			core.RankReleaseType,
			core.RankHDR,
			core.RankVideoCodec,
			core.RankVerified,
			core.RankAudioFormat,
			core.RankSize,
		})
		if gotCodecFirst[0] != "codec-win" {
			t.Fatalf("videoCodec first order = %v, want codec-win first", gotCodecFirst)
		}

		// Move audioFormat above videoCodec -> audio-size-win (TrueHD Atmos) leads.
		gotAudioFirst := orderWith([]string{
			core.RankLanguage,
			core.RankConnection,
			core.RankResolution,
			core.RankReleaseType,
			core.RankHDR,
			core.RankVerified,
			core.RankAudioFormat,
			core.RankVideoCodec,
			core.RankSize,
		})
		if gotAudioFirst[0] != "audio-size-win" {
			t.Fatalf("audioFormat first order = %v, want audio-size-win first", gotAudioFirst)
		}

		// Move size above videoCodec -> audio-size-win (22 GB > 14 GB) leads.
		gotSizeFirst := orderWith([]string{
			core.RankLanguage,
			core.RankConnection,
			core.RankResolution,
			core.RankReleaseType,
			core.RankHDR,
			core.RankVerified,
			core.RankSize,
			core.RankVideoCodec,
			core.RankAudioFormat,
		})
		if gotSizeFirst[0] != "audio-size-win" {
			t.Fatalf("size first order = %v, want audio-size-win first", gotSizeFirst)
		}
	})
}

func TestValidateRankingAndSelectionKey(t *testing.T) {
	rules := []core.ReleaseRule{
		{ID: "r1", Name: "Prefer A", Action: "prefer", Conditions: []core.ReleaseCondition{{Field: "group", Operator: "equals", Value: "A"}}},
		{ID: "r2", Name: "Require B", Action: "require", Conditions: []core.ReleaseCondition{{Field: "group", Operator: "equals", Value: "B"}}},
	}

	if err := core.ValidateRanking(nil, rules); err != nil {
		t.Fatalf("nil ranking should be valid: %v", err)
	}
	if err := core.ValidateRanking(core.DefaultRanking(rules), rules); err != nil {
		t.Fatalf("default ranking should be valid: %v", err)
	}
	// Missing connection or verified must fail.
	if err := core.ValidateRanking([]string{core.RankVerified, core.RankResolution}, rules); err == nil {
		t.Fatal("missing connection must fail validation")
	}
	if err := core.ValidateRanking([]string{core.RankConnection, core.RankResolution}, rules); err == nil {
		t.Fatal("missing verified must fail validation")
	}
	// Duplicate key must fail.
	if err := core.ValidateRanking([]string{core.RankConnection, core.RankVerified, core.RankResolution, core.RankResolution}, rules); err == nil {
		t.Fatal("duplicate key must fail validation")
	}
	// Unknown key or require-rule key must fail.
	if err := core.ValidateRanking([]string{core.RankConnection, core.RankVerified, "bogus"}, rules); err == nil {
		t.Fatal("unknown key must fail validation")
	}
	if err := core.ValidateRanking([]string{core.RankConnection, core.RankVerified, "rule:r2"}, rules); err == nil {
		t.Fatal("non-prefer rule in ranking must fail validation")
	}

	// SelectionKey is identical between nil Ranking and explicit DefaultRanking,
	// and changes when Ranking is customized.
	p1 := core.QualityProfile{Name: "4K", MaxHeight: 2160, ReleaseRules: rules}
	p2 := core.QualityProfile{Name: "4K", MaxHeight: 2160, ReleaseRules: rules, Ranking: core.DefaultRanking(rules)}
	if p1.ComputeSelectionKey() != p2.ComputeSelectionKey() {
		t.Fatalf("default ranking changed SelectionKey: %s vs %s", p1.ComputeSelectionKey(), p2.ComputeSelectionKey())
	}
	p3 := p2
	p3.Ranking = []string{
		core.RankLanguage,
		core.RankConnection,
		core.RankReleaseType,
		core.RankResolution,
		core.RankRulePrefix + "r1",
		core.RankHDR,
		core.RankVerified,
		core.RankVideoCodec,
		core.RankAudioFormat,
		core.RankSize,
	}
	if p1.ComputeSelectionKey() == p3.ComputeSelectionKey() {
		t.Fatal("custom ranking must change SelectionKey")
	}
}
