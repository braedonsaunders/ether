package playback

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releasepolicy"
)

// A remembered release is chosen under one catalogue language. A later
// Prepare is a new selection: Require, Avoid, and Prefer must follow the
// language the title has now. The session already playing is left on the
// release it opened.
func TestRememberedResumeFollowsOriginalLanguage(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("ja", 10_000, time.Minute), mkv("ko", 10_000, time.Minute)}, nil)
	ctx := context.Background()
	caps := defaultCaps("c1")
	rules := []core.ReleaseRule{{
		ID: "original", Name: "Original audio", Action: "require",
		Conditions: []core.ReleaseCondition{{Field: "audio_matches_original"}},
	}}
	profile := core.QualityProfile{Name: "1080p", MaxHeight: 1080, MinHeight: 720, ReleaseRules: rules}
	h.m.deps.SelectProfile = func(core.ClientCaps, *core.User) core.QualityProfile { return profile }
	resolver := &originalAudioResolver{rules: rules, releases: []core.Release{
		{ID: "ja", Name: "Japanese", Info: core.ReleaseInfo{Languages: []string{"japanese"}}},
		{ID: "ko", Name: "Korean", Info: core.ReleaseInfo{Languages: []string{"korean"}}},
	}}
	h.m.deps.Resolver = resolver
	h.catalog.titles[movieKey].Language = "ja"

	ps := h.prepare(owner, movieKey, "c1")
	if ps.Resolution.Release.ID != "ja" || resolver.calls() != 1 {
		t.Fatalf("first selection = %s, resolver calls %d; want ja and 1", ps.Resolution.Release.ID, resolver.calls())
	}
	rec, err := h.m.resume.get(ctx, owner.ID, movieKey)
	if err != nil || rec == nil {
		t.Fatalf("stored resume = %+v, %v", rec, err)
	}
	if rec.originalLanguage != "ja" {
		t.Fatalf("stored original language = %q; want ja", rec.originalLanguage)
	}
	if !selectionKeySuffix(rec.profile[len("1080p|"):]) || rec.profile[:len("1080p|")] != "1080p|" {
		t.Fatalf("stored profile = %q; want Name|<16 hex>", rec.profile)
	}
	if rp, err := h.m.ResumeFor(ctx, owner, movieKey, caps); err != nil || rp == nil || rp.ReleaseID() != "ja" {
		t.Fatalf("same language ResumeFor = %+v, %v", rp, err)
	}

	h.catalog.titles[movieKey].Language = "ko"
	live, ok := h.m.SessionFor(owner.ID, "c1", movieKey)
	if !ok || live.Resolution.Release.ID != "ja" || resolver.calls() != 1 {
		t.Fatalf("active stream after ja→ko = %+v ok=%v calls=%d; want ja still playing", live, ok, resolver.calls())
	}
	if rp, err := h.m.ResumeFor(ctx, owner, movieKey, caps); err != nil || rp != nil {
		t.Fatalf("ResumeFor after ja→ko = %+v, %v; want nil", rp, err)
	}
	h.m.Stop(ps.ID)
	// The remembered row is still "ja", which is also the explicit pick.
	// Reusing it would skip the resolver. The language no longer matches,
	// so the pick is resolved again and still honored.
	picked, err := h.m.PrepareVariant(ctx, owner, movieKey, caps, "ja")
	if err != nil || picked.Resolution.Release.ID != "ja" || resolver.calls() != 2 || resolver.prefers()[len(resolver.prefers())-1] != "ja" {
		t.Fatalf("explicit pick after ja→ko = %+v, %v, calls %d, prefers %v", picked, err, resolver.calls(), resolver.prefers())
	}
	h.m.Stop(picked.ID)
	ps = h.prepare(owner, movieKey, "c1")
	if ps.Resolution.Release.ID != "ko" || resolver.calls() != 3 {
		t.Fatalf("new selection after ja→ko = %s, calls %d; want ko and 3", ps.Resolution.Release.ID, resolver.calls())
	}
	h.m.Stop(ps.ID)

	h.catalog.titles[movieKey].Language = ""
	if rp, err := h.m.ResumeFor(ctx, owner, movieKey, caps); err != nil || rp != nil {
		t.Fatalf("ResumeFor after known→unknown = %+v, %v; want nil", rp, err)
	}
	if _, err := h.m.Prepare(ctx, owner, movieKey, caps); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("unknown catalogue language Prepare = %v; want not available", err)
	}
	if id := resolver.lastID(); id == "ko" || id == "ja" {
		t.Fatalf("unknown catalogue language still selected %s", id)
	}
}

func TestPreferOriginalAudioReselectsOnLanguageChange(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("ja", 10_000, time.Minute), mkv("ko", 10_000, time.Minute)}, nil)
	rules := []core.ReleaseRule{{
		ID: "original", Name: "Original audio", Action: "prefer",
		Conditions: []core.ReleaseCondition{{Field: "audio_matches_original"}},
	}}
	profile := core.QualityProfile{Name: "1080p", MaxHeight: 1080, MinHeight: 720, ReleaseRules: rules}
	h.m.deps.SelectProfile = func(core.ClientCaps, *core.User) core.QualityProfile { return profile }
	resolver := &originalAudioResolver{rules: rules, releases: []core.Release{
		{ID: "ja", Name: "Japanese", Info: core.ReleaseInfo{Languages: []string{"japanese"}}},
		{ID: "ko", Name: "Korean", Info: core.ReleaseInfo{Languages: []string{"korean"}}},
	}}
	h.m.deps.Resolver = resolver
	h.catalog.titles[movieKey].Language = "ja"

	ps := h.prepare(owner, movieKey, "c1")
	if ps.Resolution.Release.ID != "ja" {
		t.Fatalf("prefer ja selected %s", ps.Resolution.Release.ID)
	}
	h.catalog.titles[movieKey].Language = "ko"
	live, ok := h.m.SessionFor(owner.ID, "c1", movieKey)
	if !ok || live.Resolution.Release.ID != "ja" || resolver.calls() != 1 {
		t.Fatalf("prefer change rewrote the active stream: %+v ok=%v calls=%d", live, ok, resolver.calls())
	}
	h.m.Stop(ps.ID)
	ps = h.prepare(owner, movieKey, "c1")
	if ps.Resolution.Release.ID != "ko" || resolver.calls() != 2 {
		t.Fatalf("prefer new selection = %s, calls %d; want ko and 2", ps.Resolution.Release.ID, resolver.calls())
	}
	h.m.Stop(ps.ID)
	before := resolver.calls()
	ps = h.prepare(owner, movieKey, "c1")
	if ps.Resolution.Release.ID != "ko" || resolver.calls() != before {
		t.Fatalf("unchanged language reused poorly: release %s calls %d→%d", ps.Resolution.Release.ID, before, resolver.calls())
	}
	h.m.Stop(ps.ID)
}

func TestAvoidOriginalAudioDropsRememberedMatch(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("ja", 10_000, time.Minute), mkv("ko", 10_000, time.Minute)}, nil)
	rules := []core.ReleaseRule{{
		ID: "original", Name: "Original audio", Action: "avoid",
		Conditions: []core.ReleaseCondition{{Field: "audio_matches_original"}},
	}}
	profile := core.QualityProfile{Name: "1080p", MaxHeight: 1080, MinHeight: 720, ReleaseRules: rules}
	h.m.deps.SelectProfile = func(core.ClientCaps, *core.User) core.QualityProfile { return profile }
	resolver := &originalAudioResolver{rules: rules, releases: []core.Release{
		{ID: "ja", Name: "Japanese", Info: core.ReleaseInfo{Languages: []string{"japanese"}}},
		{ID: "ko", Name: "Korean", Info: core.ReleaseInfo{Languages: []string{"korean"}}},
	}}
	h.m.deps.Resolver = resolver
	h.catalog.titles[movieKey].Language = "ja"
	ps := h.prepare(owner, movieKey, "c1")
	if ps.Resolution.Release.ID != "ko" {
		t.Fatalf("avoid ja selected %s; want ko", ps.Resolution.Release.ID)
	}
	h.m.Stop(ps.ID)
	h.catalog.titles[movieKey].Language = "ko"
	ps = h.prepare(owner, movieKey, "c1")
	if ps.Resolution.Release.ID != "ja" || resolver.calls() != 2 {
		t.Fatalf("avoid after ja→ko = %s, calls %d; want ja and 2", ps.Resolution.Release.ID, resolver.calls())
	}
}

// A profile that never compares original-language audio must not read the
// catalogue to decide a resume, and a blank migration column must still
// resume. Changing the title language does not drop that row.
func TestPlainProfileResumeSkipsCatalogue(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10_000, time.Minute)}, []string{"A"})
	ctx := context.Background()
	caps := defaultCaps("c1")
	profile := core.QualityProfile{Name: "1080p", MaxHeight: 1080, MinHeight: 720, ReleaseRules: []core.ReleaseRule{{
		ID: "lang", Name: "Norwegian", Action: "require",
		Conditions: []core.ReleaseCondition{{Field: "language", Operator: "equals", Value: "no"}},
	}}}
	h.m.deps.SelectProfile = func(core.ClientCaps, *core.User) core.QualityProfile { return profile }
	h.catalog.titles[movieKey].Language = "ja"
	h.catalog.gets.Store(0)
	if err := h.m.Remember(ctx, owner, movieKey, caps, &core.Resolution{Release: core.Release{ID: "A"}, NZB: []byte("nzb-A")}); err != nil {
		t.Fatal(err)
	}
	rec, err := h.m.resume.get(ctx, owner.ID, movieKey)
	if err != nil || rec == nil || rec.originalLanguage != "" {
		t.Fatalf("plain profile stored language = %+v, %v; want blank", rec, err)
	}
	if rp, err := h.m.ResumeFor(ctx, owner, movieKey, caps); err != nil || rp == nil || rp.ReleaseID() != "A" {
		t.Fatalf("ResumeFor = %+v, %v", rp, err)
	}
	h.catalog.titles[movieKey].Language = "ko"
	if rp, err := h.m.ResumeFor(ctx, owner, movieKey, caps); err != nil || rp == nil || rp.ReleaseID() != "A" {
		t.Fatalf("language change dropped a plain profile resume: %+v, %v", rp, err)
	}
	if n := h.catalog.gets.Load(); n != 0 {
		t.Fatalf("plain profile resume read the catalogue %d times", n)
	}
	ps := h.prepare(owner, movieKey, "c1")
	if ps.Resolution.Release.ID != "A" || h.resolver.callCount() != 0 {
		t.Fatalf("plain profile Prepare used %s, resolver calls %d", ps.Resolution.Release.ID, h.resolver.callCount())
	}
}

// A row written before original_language existed stores a blank. An
// original-audio profile must not treat that blank as the current
// catalogue language, even when the stored release would pass the rule.
func TestLegacyBlankOriginalLanguageDoesNotBypass(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("ja", 10_000, time.Minute), mkv("ko", 10_000, time.Minute)}, nil)
	ctx := context.Background()
	caps := defaultCaps("c1")
	rules := []core.ReleaseRule{{
		ID: "original", Name: "Original audio", Action: "require",
		Conditions: []core.ReleaseCondition{{Field: "audio_matches_original"}},
	}}
	profile := core.QualityProfile{Name: "1080p", MaxHeight: 1080, MinHeight: 720, ReleaseRules: rules}
	h.m.deps.SelectProfile = func(core.ClientCaps, *core.User) core.QualityProfile { return profile }
	resolver := &originalAudioResolver{rules: rules, releases: []core.Release{
		{ID: "ja", Name: "Japanese", Info: core.ReleaseInfo{Languages: []string{"japanese"}}},
		{ID: "ko", Name: "Korean", Info: core.ReleaseInfo{Languages: []string{"korean"}}},
	}}
	h.m.deps.Resolver = resolver
	h.catalog.titles[movieKey].Language = "ja"
	rec := resumeRecord{
		userID: owner.ID, key: movieKey,
		release:          core.Release{ID: "ja", Name: "Japanese", Info: core.ReleaseInfo{Languages: []string{"japanese"}}},
		profile:          profileKey(profile),
		originalLanguage: "",
		updatedAt:        h.clock.Now(),
	}
	if err := h.m.resume.save(ctx, rec, []byte("nzb-ja")); err != nil {
		t.Fatal(err)
	}
	h.catalog.gets.Store(0)
	if rp, err := h.m.ResumeFor(ctx, owner, movieKey, caps); err != nil || rp != nil {
		t.Fatalf("legacy blank ResumeFor = %+v, %v; want nil", rp, err)
	}
	if n := h.catalog.gets.Load(); n != 1 {
		t.Fatalf("original-audio ResumeFor catalogue reads = %d; want 1", n)
	}
	ps := h.prepare(owner, movieKey, "c1")
	if ps.Resolution.Release.ID != "ja" || resolver.calls() != 1 {
		t.Fatalf("legacy blank Prepare = %s, calls %d; want a fresh ja selection", ps.Resolution.Release.ID, resolver.calls())
	}
}

// originalAudioResolver ranks with the same policy the playback resume gate
// is protecting, and lets an explicit Prefer through unfiltered.
type originalAudioResolver struct {
	mu       sync.Mutex
	callsN   int
	preferN  []string
	last     string
	rules    []core.ReleaseRule
	releases []core.Release
}

func (r *originalAudioResolver) Resolve(_ context.Context, req core.ResolveRequest) (*core.Resolution, error) {
	r.mu.Lock()
	r.callsN++
	r.preferN = append(r.preferN, req.Prefer)
	rules := slices.Clone(r.rules)
	rels := slices.Clone(r.releases)
	r.mu.Unlock()
	if req.Prefer != "" {
		for _, c := range rels {
			if c.ID == req.Prefer && !slices.Contains(req.Exclude, c.ID) {
				r.mu.Lock()
				r.last = c.ID
				r.mu.Unlock()
				return &core.Resolution{Title: req.Title.Key, Release: c, NZB: []byte("nzb-" + c.ID)}, nil
			}
		}
	}
	pol, err := releasepolicy.Compile(rules)
	if err != nil {
		return nil, err
	}
	lang := ""
	if req.Title != nil {
		lang = req.Title.Language
	}
	eval := releasepolicy.Context{OriginalLanguage: lang}
	type scored struct {
		rel core.Release
		res releasepolicy.Result
	}
	var ok []scored
	for _, c := range rels {
		if slices.Contains(req.Exclude, c.ID) {
			continue
		}
		got := pol.Evaluate(c, eval)
		if got.Rejected {
			continue
		}
		ok = append(ok, scored{c, got})
	}
	for i := 1; i < len(ok); i++ {
		if releasepolicy.Better(ok[i].res, ok[0].res) {
			ok[0], ok[i] = ok[i], ok[0]
		}
	}
	if len(ok) == 0 {
		r.mu.Lock()
		r.last = ""
		r.mu.Unlock()
		return nil, core.ErrNotAvailable
	}
	c := ok[0].rel
	r.mu.Lock()
	r.last = c.ID
	r.mu.Unlock()
	return &core.Resolution{Title: req.Title.Key, Release: c, NZB: []byte("nzb-" + c.ID)}, nil
}

func (r *originalAudioResolver) MarkBad(context.Context, string, string) error { return nil }
func (r *originalAudioResolver) Available(context.Context, core.RatingKey) (bool, bool) {
	return false, false
}
func (r *originalAudioResolver) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.callsN
}
func (r *originalAudioResolver) prefers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.preferN)
}
func (r *originalAudioResolver) lastID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}
