package playback

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Editing the limits of a profile (same name, higher ceiling) must forget
// the remembered release: it was resolved under the old limits. Without a
// limit change the remembered release is still reused.
func TestProfileLimitChangeForgetsRememberedRelease(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10_000, time.Minute), mkv("B", 10_000, time.Minute)}, []string{"B", "A"})
	ctx := context.Background()
	caps := defaultCaps("c1")

	profile := core.QualityProfile{Name: "1080p", MaxHeight: 1080, MinHeight: 720, PreferredCodecs: []string{"h264"}}
	h.m.deps.SelectProfile = func(core.ClientCaps, *core.User) core.QualityProfile { return profile }

	if err := h.m.Remember(ctx, owner, movieKey, caps, &core.Resolution{Release: core.Release{ID: "A"}, NZB: []byte("nzb-A")}); err != nil {
		t.Fatal(err)
	}

	// Unchanged limits: the remembered release is reused, no resolver call.
	if rp, err := h.m.ResumeFor(ctx, owner, movieKey, caps); err != nil || rp == nil || rp.ReleaseID() != "A" {
		t.Fatalf("ResumeFor before change = %+v, %v", rp, err)
	}
	ps := h.prepare(owner, movieKey, "c1")
	if ps.Resolution.Release.ID != "A" || h.resolver.callCount() != 0 {
		t.Fatalf("unchanged limits: Prepare used %s, resolver calls %d", ps.Resolution.Release.ID, h.resolver.callCount())
	}
	h.m.Stop(ps.ID)

	// Same profile name, ceiling raised 1080 -> 2160: the stored release
	// was resolved for the old limits and must not be reused.
	profile.MaxHeight = 2160
	if rp, err := h.m.ResumeFor(ctx, owner, movieKey, caps); err != nil || rp != nil {
		t.Fatalf("ResumeFor after limit change = %+v, %v; want nil", rp, err)
	}
	ps = h.prepare(owner, movieKey, "c1")
	if ps.Resolution.Release.ID != "B" || h.resolver.callCount() != 1 {
		t.Fatalf("after limit change: Prepare used %s, resolver calls %d; want fresh B", ps.Resolution.Release.ID, h.resolver.callCount())
	}
	h.m.Stop(ps.ID)

	// The fresh resolution is remembered under the new limits, exposed by
	// profile name only.
	rp, err := h.m.ResumeFor(ctx, owner, movieKey, caps)
	if err != nil || rp == nil || rp.ReleaseID() != "B" {
		t.Fatalf("ResumeFor after re-resolve = %+v, %v", rp, err)
	}
	if rp.Profile != "1080p" {
		t.Fatalf("ResumePoint.Profile = %q, want profile name only", rp.Profile)
	}
}

// The selection key comes from the configured definition (here set
// the way resolver.SelectProfile sets it, from the pristine profile),
// so a resume written under HDR TV caps is reused under SDR web caps:
// caps clamp the effective profile but never the key.
func TestResumeReusedAcrossCapsWithSameConfiguredProfile(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10_000, time.Minute)}, []string{"A"})
	ctx := context.Background()
	configured := core.QualityProfile{Name: "Main", MaxHeight: 2160, MinHeight: 720, AllowHDR: true, AllowDolbyVision: true, PreferredCodecs: []string{"hevc", "h264"}}
	key := configured.ComputeSelectionKey()
	h.m.deps.SelectProfile = func(caps core.ClientCaps, _ *core.User) core.QualityProfile {
		p := configured // clamped copy per caps, key untouched
		p.SelectionKey = key
		if caps.MaxHeight > 0 && p.MaxHeight > caps.MaxHeight {
			p.MaxHeight = caps.MaxHeight
		}
		p.AllowHDR = p.AllowHDR && caps.HDR10
		p.AllowDolbyVision = p.AllowDolbyVision && caps.DolbyVision
		if len(caps.VideoCodecs) > 0 && !caps.HDR10 {
			p.PreferredCodecs = []string{"h264"}
		}
		return p
	}
	tvCaps := core.ClientCaps{Product: "TV", ClientID: "tv", MaxHeight: 2160, HDR10: true, DolbyVision: true, VideoCodecs: []string{"hevc", "h264"}}
	webCaps := defaultCaps("web") // 1080, SDR, h264 only

	if err := h.m.Remember(ctx, owner, movieKey, tvCaps, &core.Resolution{Release: core.Release{ID: "A"}, NZB: []byte("nzb-A")}); err != nil {
		t.Fatal(err)
	}
	rp, err := h.m.ResumeFor(ctx, owner, movieKey, webCaps)
	if err != nil || rp == nil || rp.ReleaseID() != "A" {
		t.Fatalf("ResumeFor under other caps = %+v, %v; want hit A", rp, err)
	}
	if rp.Profile != "Main" {
		t.Fatalf("ResumePoint.Profile = %q, want profile name only", rp.Profile)
	}
}

// Records written before fingerprints store a plain profile name; they
// never match a fingerprint, so the next Prepare resolves fresh and
// rewrites the record (lazy migration, no schema change).
func TestLegacyPlainNameResumeNeverMatches(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10_000, time.Minute), mkv("B", 10_000, time.Minute)}, []string{"B"})
	ctx := context.Background()
	rec := resumeRecord{userID: owner.ID, key: movieKey, release: core.Release{ID: "A"}, profile: "1080p", updatedAt: h.clock.Now()}
	if err := h.m.resume.save(ctx, rec, []byte("nzb-A")); err != nil {
		t.Fatal(err)
	}
	caps := defaultCaps("c1")
	if rp, err := h.m.ResumeFor(ctx, owner, movieKey, caps); err != nil || rp != nil {
		t.Fatalf("legacy ResumeFor = %+v, %v; want nil", rp, err)
	}
	ps := h.prepare(owner, movieKey, "c1")
	if ps.Resolution.Release.ID != "B" || h.resolver.callCount() != 1 {
		t.Fatalf("legacy Prepare used %s, resolver calls %d; want fresh B", ps.Resolution.Release.ID, h.resolver.callCount())
	}
	h.m.Stop(ps.ID)
	if rp, err := h.m.ResumeFor(ctx, owner, movieKey, caps); err != nil || rp == nil || rp.ReleaseID() != "B" {
		t.Fatalf("rewritten ResumeFor = %+v, %v", rp, err)
	}
}

func TestProfileNameStripsOnlySelectionSuffix(t *testing.T) {
	key := core.QualityProfile{Name: "ignored"}.ComputeSelectionKey()
	if len(key) != 16 {
		t.Fatalf("selection key %q", key)
	}
	cases := []struct{ stored, want string }{
		{"1080p", "1080p"},
		{"1080p|" + key, "1080p"},
		{"Owner | 4K", "Owner | 4K"},
		{"Owner | 4K|" + key, "Owner | 4K"},
		{"A|B|C|" + key, "A|B|C"},
		{"Movies|zzzzzzzzzzzzzzzz", "Movies|zzzzzzzzzzzzzzzz"},
		{"Movies|ABCDEF0123456789", "Movies|ABCDEF0123456789"},
		{"legacy|name", "legacy|name"},
	}
	for _, tc := range cases {
		if got := profileName(tc.stored); got != tc.want {
			t.Errorf("profileName(%q) = %q, want %q", tc.stored, got, tc.want)
		}
	}
}

// A remembered release under a pipe-containing name is reused until that
// same profile's limits change, and the exposed name keeps the pipes.
func TestPipeNameEditForgetsRememberedRelease(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10_000, time.Minute), mkv("B", 10_000, time.Minute)}, []string{"B", "A"})
	ctx := context.Background()
	caps := defaultCaps("c1")
	profile := core.QualityProfile{Name: "Owner | 4K", MaxHeight: 1080, MinHeight: 720, PreferredCodecs: []string{"h264"}}
	h.m.deps.SelectProfile = func(core.ClientCaps, *core.User) core.QualityProfile { return profile }

	if err := h.m.Remember(ctx, owner, movieKey, caps, &core.Resolution{Release: core.Release{ID: "A"}, NZB: []byte("nzb-A")}); err != nil {
		t.Fatal(err)
	}
	rp, err := h.m.ResumeFor(ctx, owner, movieKey, caps)
	if err != nil || rp == nil || rp.ReleaseID() != "A" || rp.Profile != "Owner | 4K" {
		t.Fatalf("unchanged pipe name = %+v, %v", rp, err)
	}
	profile.MaxHeight = 2160
	if rp, err := h.m.ResumeFor(ctx, owner, movieKey, caps); err != nil || rp != nil {
		t.Fatalf("after edit = %+v, %v; want nil", rp, err)
	}
}

// A legacy stored name that contains pipes is not a selection key, so it
// misses once and is rewritten. The exposed name is the whole legacy value.
func TestLegacyPipeNameResumeNeverMatches(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10_000, time.Minute), mkv("B", 10_000, time.Minute)}, []string{"B"})
	ctx := context.Background()
	const legacy = "Owner | 4K"
	rec := resumeRecord{userID: owner.ID, key: movieKey, release: core.Release{ID: "A"}, profile: legacy, updatedAt: h.clock.Now()}
	if err := h.m.resume.save(ctx, rec, []byte("nzb-A")); err != nil {
		t.Fatal(err)
	}
	if got := profileName(legacy); got != legacy {
		t.Fatalf("legacy display %q", got)
	}
	caps := defaultCaps("c1")
	if rp, err := h.m.ResumeFor(ctx, owner, movieKey, caps); err != nil || rp != nil {
		t.Fatalf("legacy pipe ResumeFor = %+v, %v; want nil", rp, err)
	}
}

func TestSameNameRuleEditForgetsRememberedRelease(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 10_000, time.Minute), mkv("B", 10_000, time.Minute)}, []string{"B", "A"})
	ctx := context.Background()
	caps := defaultCaps("c1")
	profile := core.QualityProfile{Name: "1080p", MaxHeight: 1080, MinHeight: 720, ReleaseRules: []core.ReleaseRule{{
		ID: "audio", Name: "Norwegian", Action: "require",
		Conditions: []core.ReleaseCondition{{Field: "language", Operator: "equals", Value: "no"}},
	}}}
	h.m.deps.SelectProfile = func(core.ClientCaps, *core.User) core.QualityProfile { return profile }
	if err := h.m.Remember(ctx, owner, movieKey, caps, &core.Resolution{Release: core.Release{ID: "A"}, NZB: []byte("nzb-A")}); err != nil {
		t.Fatal(err)
	}
	if rp, err := h.m.ResumeFor(ctx, owner, movieKey, caps); err != nil || rp == nil || rp.ReleaseID() != "A" {
		t.Fatalf("unchanged rules = %+v %v", rp, err)
	}
	profile.ReleaseRules[0].Conditions[0].Value = "sv"
	if rp, err := h.m.ResumeFor(ctx, owner, movieKey, caps); err != nil || rp != nil {
		t.Fatalf("edited rules reused the remembered release: %+v %v", rp, err)
	}
}

func TestWarmCacheRuleEditInvalidatesSameName(t *testing.T) {
	rules := []core.ReleaseRule{{ID: "a", Name: "Norwegian", Action: "require", Conditions: []core.ReleaseCondition{{Field: "language", Operator: "equals", Value: "no"}}}}
	e := &warmEntry{
		res:        &core.Resolution{Release: core.Release{ID: "r", Info: core.ReleaseInfo{Height: 1080, VideoCodec: "h264"}}},
		profileObj: core.QualityProfile{Name: "1080p", MaxHeight: 1080, MinHeight: 720, ReleaseRules: rules},
	}
	caps := core.ClientCaps{MaxHeight: 1080, VideoCodecs: []string{"h264"}}
	if !warmAcceptable(e, e.profileObj, caps, "") {
		t.Fatal("unchanged rules rejected a warm release")
	}
	edited := e.profileObj
	edited.ReleaseRules = []core.ReleaseRule{{ID: "a", Name: "Norwegian", Action: "require", Conditions: []core.ReleaseCondition{{Field: "language", Operator: "equals", Value: "sv"}}}}
	if warmAcceptable(e, edited, caps, "") {
		t.Fatal("same-name rule edit reused the warm release")
	}
}
func TestWarmCatalogueLanguageIsContextNotAProfileFact(t *testing.T) {
	rules := []core.ReleaseRule{{ID: "o", Name: "Original audio", Action: "prefer", Conditions: []core.ReleaseCondition{{Field: "audio_matches_original", Operator: "equals"}}}}
	e := &warmEntry{
		res:              &core.Resolution{Release: core.Release{ID: "r", Info: core.ReleaseInfo{Height: 1080, VideoCodec: "h264"}}},
		profileObj:       core.QualityProfile{Name: "1080p", MaxHeight: 1080, MinHeight: 720, ReleaseRules: rules},
		originalAudio:    true,
		originalLanguage: "ja",
	}
	caps := core.ClientCaps{MaxHeight: 1080, VideoCodecs: []string{"h264"}}
	if !warmAcceptable(e, e.profileObj, caps, "ja") {
		t.Fatal("same catalogue language rejected a warm release")
	}
	if warmAcceptable(e, e.profileObj, caps, "ko") || warmAcceptable(e, e.profileObj, caps, "") {
		t.Fatal("catalogue language change reused a warm release")
	}
	plainProfile := e.profileObj
	plainProfile.ReleaseRules = nil
	plain := &warmEntry{
		res:        e.res,
		profileObj: plainProfile,
	}
	if !warmAcceptable(plain, plain.profileObj, caps, "ko") {
		t.Fatal("a profile without original-audio was pinned to a catalogue language")
	}
	with := e.profileObj
	without := with
	without.ReleaseRules = nil
	if with.ComputeSelectionKey() == without.ComputeSelectionKey() {
		t.Fatal("original-audio rule did not change the profile selection key")
	}
}
