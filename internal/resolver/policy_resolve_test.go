package resolver

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releasepolicy"
)

type stagedIndexer struct {
	fakeIndexer
	first, rest []core.Release
	gap         time.Duration
}

func (p *stagedIndexer) SearchWith(ctx context.Context, _ core.SearchRequest, arrived func([]core.Release)) ([]core.Release, error) {
	if arrived != nil && len(p.first) > 0 {
		arrived(p.first)
	}
	select {
	case <-time.After(p.gap):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return append(append([]core.Release{}, p.first...), p.rest...), nil
}

func signalRelease(id, group, language string) core.Release {
	r := rel(id, "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-"+group, 8000)
	r.Info = core.ReleaseInfo{
		Title: "The Signal", Year: 1999, Height: 1080, Source: core.SourceWEBDL,
		VideoCodec: "h264", Group: group, Languages: []string{language},
	}
	return r
}

func groupPolicy() []core.ReleaseRule {
	return []core.ReleaseRule{{
		ID: "cut", Name: "Primary group", Action: "require",
		Conditions: []core.ReleaseCondition{{Field: "group", Operator: "equals", Value: "PRIMARY"}},
		Fallback:   []core.ReleaseCondition{{Field: "group", Operator: "equals", Value: "FALLBACK"}},
	}}
}

func serviceWithRules(t *testing.T, idx core.Indexer, health core.HealthChecker, rules []core.ReleaseRule) *Service {
	t.Helper()
	cfg := testCfg()
	for i := range cfg.Quality.Profiles {
		cfg.Quality.Profiles[i].ReleaseRules = rules
	}
	s := New(idx, health, nil, cfg, Options{})
	t.Cleanup(func() { s.Close() })
	return s
}

func TestExplicitPolicyWaitsForPrimary(t *testing.T) {
	if !explicitReleasePolicy(core.QualityProfile{ReleaseRules: groupPolicy()}) {
		t.Fatal("require+fallback rules must still be recognized")
	}
	synctest.Test(t, func(t *testing.T) {
		primary := signalRelease("primary", "PRIMARY", "english")
		fallback := signalRelease("fallback", "FALLBACK", "english")
		idx := &stagedIndexer{first: []core.Release{fallback}, rest: []core.Release{primary}, gap: 800 * time.Millisecond}
		s := serviceWithRules(t, idx, fakeHealth{}, groupPolicy())
		start := time.Now()
		got, err := s.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
		if err != nil || got.Release.ID != "primary" {
			t.Fatalf("primary lost to an early fallback: %+v %v", got, err)
		}
		if elapsed := time.Since(start); elapsed < 800*time.Millisecond {
			t.Fatalf("discovery finished early at %s", elapsed)
		}
		if s.recentlyFailedHealth("fallback") || s.isBad("fallback") {
			t.Fatal("policy fallback was cached as a bad source")
		}
	})
}

func TestFallbackStartsAfterCriticalPathDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fallback := signalRelease("fallback", "FALLBACK", "english")
		idx := &slowTailIndexer{fakeIndexer: fakeIndexer{releases: []core.Release{fallback}}}
		s := serviceWithRules(t, idx, fakeHealth{}, groupPolicy())
		start := time.Now()
		got, err := s.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
		if err != nil || got.Release.ID != "fallback" {
			t.Fatalf("fallback did not start after the critical-path deadline: %+v %v", got, err)
		}
		if elapsed := time.Since(start); elapsed != criticalPathDeadline {
			t.Fatalf("elapsed %s, want the %s critical-path deadline", elapsed, criticalPathDeadline)
		}
	})
}

func TestFallbackDoesNotOvertakeDelayedPrimary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		health := overlapHealth{func(ctx context.Context, nzb []byte) (bool, error) {
			if strings.Contains(string(nzb), "primary") {
				select {
				case <-time.After(3 * time.Second):
					return true, nil
				case <-ctx.Done():
					return false, ctx.Err()
				}
			}
			return true, nil
		}}
		idx := &fakeIndexer{releases: []core.Release{signalRelease("primary", "PRIMARY", "english"), signalRelease("fallback", "FALLBACK", "english")}}
		s := serviceWithRules(t, idx, health, groupPolicy())
		start := time.Now()
		got, err := s.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
		if err != nil || got.Release.ID != "primary" {
			t.Fatalf("fallback overtook a still-valid primary: %+v %v", got, err)
		}
		if elapsed := time.Since(start); elapsed != 3*time.Second {
			t.Fatalf("elapsed %s, want the primary's own check", elapsed)
		}
	})
}

func TestUnhealthyPrimaryUsesFallbackWithoutMarkingPolicy(t *testing.T) {
	idx := &fakeIndexer{releases: []core.Release{signalRelease("primary", "PRIMARY", "english"), signalRelease("fallback", "FALLBACK", "english")}}
	s := serviceWithRules(t, idx, fakeHealth{unhealthy: map[string]bool{"primary": true}}, groupPolicy())
	got, err := s.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	if err != nil || got.Release.ID != "fallback" {
		t.Fatalf("unhealthy primary did not fall back: %+v %v", got, err)
	}
	if !s.recentlyFailedHealth("primary") {
		t.Fatal("unhealthy primary was not remembered as a failed health check")
	}
	if s.isBad("primary") || s.isBad("fallback") || s.recentlyFailedHealth("fallback") {
		t.Fatal("fallback path marked a source bad")
	}
}

func TestPolicyRejectionDoesNotCacheUnavailability(t *testing.T) {
	english := signalRelease("english", "GROUP", "english")
	rules := []core.ReleaseRule{{ID: "audio", Name: "Norwegian", Action: "require", Conditions: []core.ReleaseCondition{{Field: "language", Operator: "equals", Value: "no"}}}}
	s := serviceWithRules(t, &fakeIndexer{releases: []core.Release{english}}, fakeHealth{}, rules)
	if _, err := s.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: webCaps()}); err == nil {
		t.Fatal("missing Norwegian declaration was accepted")
	}
	if s.recentlyFailedHealth("english") || s.isBad("english") {
		t.Fatal("policy rejection was stored as a bad or unhealthy source")
	}
	s.mu.RLock()
	_, cached := s.avail[int64(matrixTitle().Key)]
	s.mu.RUnlock()
	if cached {
		t.Fatal("policy rejection cached the title unavailable")
	}
}

func TestPreferenceReasonNamesTheHardRejection(t *testing.T) {
	rules := []core.ReleaseRule{
		{ID: "br", Name: "Brazilian", Action: "prefer", Conditions: []core.ReleaseCondition{{Field: "language", Operator: "equals", Value: "pt-BR"}}},
		{ID: "foo", Name: "Foo", Action: "require", Conditions: []core.ReleaseCondition{{Field: "name", Operator: "contains", Value: "Foo"}}},
	}
	policy, err := releasepolicy.Compile(rules)
	if err != nil {
		t.Fatal(err)
	}
	rel := core.Release{Name: "Film.2026.1080p", Info: core.ReleaseInfo{Languages: []string{"portuguese"}}}
	result := policy.Evaluate(rel, releasepolicy.Context{})
	if !result.Rejected || len(result.Explanations) != 2 {
		t.Fatalf("%+v", result)
	}
	if result.Explanations[0].Name != "Brazilian" || result.Explanations[0].Outcome != "unknown" {
		t.Fatalf("prefer explanation dropped: %+v", result.Explanations[0])
	}
	if result.Explanations[1].Name != "Foo" || result.Explanations[1].Outcome != "exclude" {
		t.Fatalf("require explanation: %+v", result.Explanations[1])
	}
	reason := preferenceReason(result)
	if reason != "release preference: Foo (exclude)" {
		t.Fatalf("reason %q", reason)
	}
}

func TestPinAndPreferStayAheadOfPolicyTiers(t *testing.T) {
	ctx := context.Background()
	primary := signalRelease("primary", "PRIMARY", "english")
	fallback := signalRelease("fallback", "FALLBACK", "english")
	other := signalRelease("other", "OTHER", "english")
	s := serviceWithRules(t, &fakeIndexer{releases: []core.Release{primary, fallback, other}}, fakeHealth{}, groupPolicy())
	if _, err := s.Pin(ctx, matrixTitle().Key, "The Signal", other, ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	if err != nil || got.Release.ID != "other" {
		t.Fatalf("pin lost to a policy-valid release: %+v %v", got, err)
	}
	got, err = s.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps(), Prefer: "fallback"})
	if err != nil || got.Release.ID != "fallback" {
		t.Fatalf("explicit pick lost to the primary tier: %+v %v", got, err)
	}
	unhealthy := serviceWithRules(t, &fakeIndexer{releases: []core.Release{primary, fallback, other}}, fakeHealth{unhealthy: map[string]bool{"other": true}}, groupPolicy())
	if _, err := unhealthy.Pin(ctx, matrixTitle().Key, "The Signal", other, ""); err != nil {
		t.Fatal(err)
	}
	got, err = unhealthy.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	if err != nil || got.Release.ID != "primary" {
		t.Fatalf("unhealthy pin did not leave the primary tier first: %+v %v", got, err)
	}
}

func TestSearchCacheDoesNotFreezeOriginalLanguage(t *testing.T) {
	rules := []core.ReleaseRule{{
		ID: "subs-and-original", Name: "Nordic subs with original audio", Action: "prefer", Match: "all",
		Conditions: []core.ReleaseCondition{
			{Field: "subtitles", Operator: "equals", Value: "no"},
			{Field: "audio_matches_original", Operator: "equals"},
		},
	}}
	original := rel("original", "The.Signal.1999.720p.WEB-DL-GRP", 4000)
	original.Info = core.ReleaseInfo{Title: "The Signal", Year: 1999, Height: 720, Source: core.SourceWEBDL, VideoCodec: "h264", Languages: []string{"japanese"}, SubtitleLanguages: []string{"norwegian"}}
	other := rel("other", "The.Signal.1999.1080p.WEB-DL.ENG-GRP", 9000)
	other.Info = core.ReleaseInfo{Title: "The Signal", Year: 1999, Height: 1080, Source: core.SourceWEBDL, VideoCodec: "h264", Languages: []string{"english"}}
	s := serviceWithRules(t, &fakeIndexer{releases: []core.Release{other, original}}, fakeHealth{}, rules)
	ja := matrixTitle()
	ja.Language = "ja"
	first, err := s.Resolve(context.Background(), core.ResolveRequest{Title: ja, Client: plainCaps()})
	if err != nil || first.Release.ID != "original" {
		t.Fatalf("japanese original lost: %+v %v", first, err)
	}
	ko := matrixTitle()
	ko.Language = "ko"
	second, err := s.Resolve(context.Background(), core.ResolveRequest{Title: ko, Client: plainCaps()})
	if err != nil || second.Release.ID != "other" {
		t.Fatalf("search cache reused the Japanese ranking for a Korean title: %+v %v", second, err)
	}
}
