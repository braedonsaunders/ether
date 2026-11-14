package resolver

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// A configured menu is a settings contract, not a projection of discovery.
// Verify cold/warm/expired caches, optional modes and admission of an explicit
// logical choice, including its unavailable-source fallback.
func TestConfiguredVersionSlots(t *testing.T) {
	for _, mode := range []string{"default", "resolution", "source", "both"} {
		t.Run(mode, func(t *testing.T) {
			cfg := testCfg()
			cfg.Resolver.SameQualityAlternatives = 2
			cfg.Resolver.AlternativesPerResolution = mode == "resolution" || mode == "both"
			cfg.Resolver.AlternativesPerSource = mode == "source" || mode == "both"
			cfg.Resolver.OfferHigherVersions = true
			idx := &fakeIndexer{releases: variantReleases()}
			svc := New(idx, fakeHealth{}, nil, cfg, Options{})
			req := core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()}
			ctx := context.Background()
			cold, err := svc.VersionSlots(ctx, req, core.MaxVariants, false)
			if err != nil || len(cold) < 2 || len(cold) > core.MaxVariants {
				t.Fatalf("cold slots %d: %v", len(cold), err)
			}
			ids := func(vs []core.ReleaseVariant) []string {
				out := []string{}
				for _, v := range vs {
					out = append(out, v.ID)
				}
				return out
			}
			want := 6
			if mode != "default" {
				want = core.MaxVariants
			}
			if len(cold) != want {
				t.Fatalf("configured slot count %d, want %d", len(cold), want)
			}
			before := ids(cold)
			if idx.searches.Load() != 0 {
				t.Fatal("browse searched")
			}
			primeSearchCache(t, svc, req.Client)
			searches := idx.searches.Load()
			warm, err := svc.VersionSlots(ctx, req, core.MaxVariants, true)
			if err != nil || !reflect.DeepEqual(ids(warm), before) {
				t.Fatalf("cache changed slots: %v", err)
			}
			if idx.searches.Load() != searches {
				t.Fatal("labels searched")
			}
			found := false
			for _, v := range warm {
				if strings.Contains(v.Label, "GRP") {
					found = true
				}
			}
			if !found {
				t.Fatal("known releases lost their labels")
			}
			for _, v := range warm[1:] {
				if v.Release.ID == v.ID {
					continue
				} // an unavailable configured reservation
				selected := req
				selected.Prefer = v.ID
				result, e := svc.Resolve(ctx, selected)
				if e != nil || !result.MatchesVersion(v.ID) {
					t.Fatalf("slot admission %s: %v", v.Label, e)
				}
				break
			}
			for _, v := range warm[1:] {
				if v.Release.ID != v.ID {
					continue
				}
				selected := req
				selected.Prefer = v.ID
				result, e := svc.Resolve(ctx, selected)
				if e != nil || result == nil || result.VersionSelector != "" {
					t.Fatalf("unavailable configured slot: %v", e)
				}
				break
			}
			// This configured but absent source remains visible and safely falls
			// back to normal ranking. It must not gain a false selector receipt.
			missing := versionSlot{Kind: "tier", Height: 480, Source: "remux"}.id()
			selected := req
			selected.Prefer = missing
			result, e := svc.Resolve(ctx, selected)
			if e != nil || result == nil || result.VersionSelector != "" {
				t.Fatalf("missing fallback: %v", e)
			}
		})
	}
	t.Run("higher slot and device ceiling", func(t *testing.T) {
		cfg := testCfg()
		cfg.Quality.Rules = nil
		cfg.Resolver.AlternativesPerResolution = true
		cfg.Resolver.OfferHigherVersions = true
		idx := &fakeIndexer{releases: []core.Release{rel("high", "The.Signal.1999.2160p.WEB-DL.HEVC.DDP5.1-GRP", 20000), rel("hd", "The.Signal.1999.1080p.WEB-DL.H.264.DDP5.1-GRP", 8000)}}
		svc := New(idx, fakeHealth{}, nil, cfg, Options{})
		req := core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()}
		slots, err := svc.VersionSlots(context.Background(), req, core.MaxVariants, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range slots {
			slot, _ := parseVersionSlot(v.ID)
			if slot.Height == 2160 {
				req.Prefer = v.ID
				break
			}
		}
		if req.Prefer == "" {
			t.Fatal("owner-enabled higher tier missing")
		}
		res, err := svc.Resolve(context.Background(), req)
		if err != nil || !res.MatchesVersion(req.Prefer) || res.Release.ID != "high" {
			t.Fatalf("higher tier: %+v %v", res, err)
		}
		req.Client.MaxHeight = 1080
		res, err = svc.Resolve(context.Background(), req)
		if err != nil || res.Release.ID != "hd" || res.VersionSelector != "" {
			t.Fatalf("device ceiling: %+v %v", res, err)
		}
	})

}
