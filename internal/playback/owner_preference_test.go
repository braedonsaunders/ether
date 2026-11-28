package playback

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/resolver"
	"github.com/braedonsaunders/ether/internal/store"
)

// preferenceResolver reports an owner pin in either mode: a strict pin
// (PinnedReleaseID) or a preferred release (PreferredRelease).
type preferenceResolver struct {
	core.Resolver
	mu        sync.Mutex
	strict    string
	preferred string
	eligible  bool
}

func (r *preferenceResolver) set(strict, preferred string, eligible bool) {
	r.mu.Lock()
	r.strict, r.preferred, r.eligible = strict, preferred, eligible
	r.mu.Unlock()
}

func (r *preferenceResolver) PinnedReleaseID(core.ResolveRequest) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.strict
}

func (r *preferenceResolver) PreferredRelease(core.ResolveRequest) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.preferred, r.preferred != "" && r.eligible
}

func (r *preferenceResolver) EstimateReleaseMedia(*core.Title, core.Release, []string) core.MediaInfo {
	return core.MediaInfo{Streams: []core.StreamInfo{{Type: "video", Codec: "h264"}}}
}

func preferenceSession(h *harness) *session {
	return &session{title: h.catalog.titles[movieKey], caps: defaultCaps("prefer"), ident: identity{userID: owner.ID, key: movieKey}}
}

// An eligible, untried preference is resolved before cached alternates are
// fetched or raced; once tried, ordinary alternates are admitted again.
func TestPreferenceResolvedBeforeCachedAlternates(t *testing.T) {
	h := newHarness(t, nil, []string{"A", "B"})
	pins := &preferenceResolver{Resolver: h.resolver}
	pins.set("", "A", true)
	h.m.deps.Resolver = pins
	s := preferenceSession(h)
	fetched := 0
	h.m.deps.FetchNZB = func(context.Context, core.Release) ([]byte, error) {
		fetched++
		return []byte("payload"), nil
	}
	cached := []core.Release{{ID: "B", Name: "Release.B"}, {ID: "C", Name: "Release.C"}}
	res, err := h.m.nextResolution(t.Context(), s, nil, cached)
	if err != nil || res == nil || res.Release.ID != "A" || fetched != 0 {
		t.Fatalf("cached alternate fetched ahead of the preference: %+v %v fetches=%d", res, err, fetched)
	}
	if g, _, err := h.m.raceAlternates(t.Context(), s, nil, cached, nil, 2, nil, nil); err != nil || g != nil || fetched != 0 {
		t.Fatalf("raced ahead of the preference: %v fetches=%d", err, fetched)
	}
	if out := h.m.raceInitialAlternates(t.Context(), s, core.QualityProfile{}, nil, nil, cached, nil, 2, 0); out.attempted != 0 || fetched != 0 {
		t.Fatalf("initial race ahead of the preference: %+v fetches=%d", out, fetched)
	}
	if h.m.preferenceAllows(s, "B", nil) || !h.m.preferenceAllows(s, "A", nil) {
		t.Fatal("warm or remembered source shadowed an untried preference")
	}
	// Tried: alternates are fair game.
	res, err = h.m.nextResolution(t.Context(), s, []string{"A"}, cached)
	if err != nil || res == nil || res.Release.ID != "B" || fetched != 1 {
		t.Fatalf("tried preference still blocked alternates: %+v %v fetches=%d", res, err, fetched)
	}
	// Ineligible (inside its failure memory): ordinary choices are admitted.
	pins.set("", "A", false)
	if !h.m.preferenceAllows(s, "B", nil) {
		t.Fatal("ineligible preference blocked a warm source")
	}
	// A strict pin is unchanged: nothing else may play.
	pins.set("A", "", false)
	if h.m.ownerPinAllows(s, "B") || !h.m.ownerChoiceCovers(s, "B") {
		t.Fatal("strict pin semantics changed")
	}
}

type failedPreferenceResolver struct{ core.Resolver }

func (failedPreferenceResolver) Resolve(context.Context, core.ResolveRequest) (*core.Resolution, error) {
	return &core.Resolution{Release: core.Release{ID: "B", Name: "Release.B"}, NZB: []byte("nzb-B"), PreferenceFailed: "A"}, nil
}

// A preference the resolver could not use joins the session's tried set
// before any sequential or raced failover.
func TestFailedPreferenceJoinsTried(t *testing.T) {
	h := newHarness(t, nil, []string{"A", "B"})
	h.m.deps.Resolver = failedPreferenceResolver{h.resolver}
	s := preferenceSession(h)
	var tried []string
	res, err := h.m.nextResolutionTracking(t.Context(), s, &tried, nil, nil)
	if err != nil || res.Release.ID != "B" || !slices.Equal(tried, []string{"A"}) {
		t.Fatalf("tried %v after %+v %v", tried, res, err)
	}
}

// A session that fell back from its preference stays on the fallback: the
// preference stays tried across an unpause and through later failovers, and a
// mode edit does not disturb it. A fresh session tries the preference again.
func TestFallbackSessionKeepsPreferenceTried(t *testing.T) {
	a := mkv("A", 4<<20, time.Minute)
	a.OpenErr = errors.New("source gone")
	h := newHarness(t, []*fakeRelease{a, mkv("B", 4<<20, time.Minute)}, []string{"A", "B"})
	pins := &preferenceResolver{Resolver: h.resolver}
	pins.set("", "A", true)
	h.m.deps.Resolver = pins
	ps, err := h.m.Prepare(context.Background(), owner, movieKey, defaultCaps("prefer"))
	if err != nil {
		t.Fatal(err)
	}
	s := h.m.lookup(ps.ID)
	if s == nil || s.current() == nil || s.current().releaseID() != "B" {
		t.Fatalf("session did not fall back to B: %+v", s)
	}
	s.mu.Lock()
	failed, tried := s.preferenceFailed, slices.Clone(s.tried)
	s.mu.Unlock()
	if !failed || !slices.Contains(tried, "A") {
		t.Fatalf("preference not sticky: failed=%v tried=%v", failed, tried)
	}
	if got := h.m.untriedPreference(s, nil); got != "" {
		t.Fatalf("running session would return to the preference: %q", got)
	}
	s.mu.Lock()
	s.resetTriedForNewPlayLocked()
	tried = slices.Clone(s.tried)
	s.mu.Unlock()
	if !slices.Equal(tried, []string{"A"}) || h.m.untriedPreference(s, nil) != "" {
		t.Fatalf("unpause forgot the failed preference: %v", tried)
	}
	// Mode edits leave the running session on its source.
	pins.set("A", "", false)
	if s.current().releaseID() != "B" {
		t.Fatal("strict edit disturbed the session")
	}
	pins.set("", "A", true)
	if h.m.untriedPreference(s, nil) != "" || s.current().releaseID() != "B" {
		t.Fatal("preferred edit sent the session back to the failed preference")
	}
	// The next fresh Auto (a new session) tries it again.
	if got := h.m.untriedPreference(preferenceSession(h), nil); got != "A" {
		t.Fatalf("fresh session preference %q", got)
	}
}

type encoderBusyTranscoder struct{ core.Transcoder }

func (encoderBusyTranscoder) VideoEncoderBusy(string) bool { return true }

// A healthy preferred release that is playing is not replaced to save the
// encoder, exactly like a strict pin; another release still is.
func TestEncoderCapacityKeepsPlayingPreference(t *testing.T) {
	h := newHarness(t, nil, []string{"A", "B"})
	pins := &preferenceResolver{Resolver: h.resolver}
	h.m.deps.Resolver = pins
	h.m.deps.Transcoder = encoderBusyTranscoder{h.m.deps.Transcoder}
	fetched := 0
	h.m.deps.FetchNZB = func(context.Context, core.Release) ([]byte, error) {
		fetched++
		return nil, errors.New("not needed")
	}
	video := core.MediaInfo{Streams: []core.StreamInfo{{Type: "video", Codec: "hevc"}}}
	ctx := core.WithSourceDecision(t.Context(), core.SourceDecision{
		Options: core.DecideOptions{DirectPlay: true, AudioStream: -1, SubtitleStream: -1},
		Decide: func(_ core.MediaInfo, res *core.Resolution) core.Decision {
			return core.Decision{Kind: core.DecisionTranscode, VideoCopy: res != nil && res.Release.ID == "B"}
		},
	})
	alt := core.Release{ID: "B", Name: "The.Signal.1999.1080p.WEB-DL.H.264-GRP"}
	primary := &generation{res: &core.Resolution{Title: movieKey, Release: core.Release{ID: "A", Name: "The.Signal.1999.2160p.HEVC-GRP"}, Alternates: []core.Release{alt}}, info: video}
	for _, tc := range []struct {
		name             string
		strict, prefer   string
		wantSubstitution bool
	}{
		{name: "no pin", wantSubstitution: true},
		{name: "preference playing", prefer: "A"},
		{name: "strict pin", strict: "A"},
		{name: "preference elsewhere", prefer: "C", wantSubstitution: true},
	} {
		fetched = 0
		pins.set(tc.strict, tc.prefer, true)
		h.m.copySourceAtCapacity(ctx, preferenceSession(h), core.QualityProfile{}, primary, nil)
		if (fetched > 0) != tc.wantSubstitution {
			t.Fatalf("%s: substitution attempts %d", tc.name, fetched)
		}
	}
	pins.set("", "A", true)
	if !h.m.hasAuthoritativeAutoChoice(preferenceSession(h), "A") || h.m.hasAuthoritativeAutoChoice(preferenceSession(h), "B") {
		t.Fatal("delivery estimate protection does not follow the playing preference")
	}
}

// A playing preference whose original the provider lost has no cached
// alternates (a preference that plays returns none), so recovery resolves
// again without it under the usual checks. A strict pin still gets no
// substitute.
func TestLostPreferenceRecoversThroughResolve(t *testing.T) {
	for _, strict := range []bool{false, true} {
		old := mkv("A", 100_000, time.Minute)
		h := newHarness(t, []*fakeRelease{old, mkv("B", 100_000, time.Minute)}, []string{"A", "B"})
		for j := range h.resolver.candidates {
			h.resolver.candidates[j].Protocol = core.ProtocolTorrent
		}
		pins := &preferenceResolver{Resolver: h.resolver}
		if strict {
			pins.set("A", "", false)
		} else {
			pins.set("", "A", true)
		}
		h.m.deps.Resolver = pins
		h.m.deps.FetchNZB = func(_ context.Context, r core.Release) ([]byte, error) {
			return []byte("nzb-" + r.ID), nil
		}
		ps, err := h.m.Prepare(t.Context(), owner, movieKey, defaultCaps("direct"))
		if err != nil || ps.Resolution.Release.ID != "A" {
			t.Fatalf("strict=%v prepare: %+v %v", strict, ps, err)
		}
		s := h.m.lookup(ps.ID)
		s.mu.Lock()
		s.alternates = nil
		s.mu.Unlock()
		h.m.BindSource(ps.ID, core.SourceBindingAuto)
		before := core.SourceRevision(ps)
		if err := h.m.PinSourceRevision(t.Context(), ps.ID, before); err != nil {
			t.Fatal(err)
		}
		s.current().noteReadFailure(core.ErrOriginalLost)
		next, _, err := h.m.ReplaceLostOriginal(t.Context(), ps.ID, before, 10*time.Second, false, func(*core.PlaySession) float64 { return 1 })
		if strict {
			if !errors.Is(err, core.ErrVersionUnreadable) {
				t.Fatalf("strict pin got a substitute: %+v %v", next, err)
			}
			continue
		}
		if err != nil || next.Resolution.Release.ID != "B" {
			t.Fatalf("lost preference not recovered: %+v %v", next, err)
		}
		if ex := h.resolver.excludes[len(h.resolver.excludes)-1]; !slices.Contains(ex, "A") {
			t.Fatalf("recovery resolve did not exclude the lost preference: %v", ex)
		}
	}
}

// A preference inside its failure window is not reused from warm, resume or
// cached alternates; every other source is admitted.
func TestIneligiblePreferenceRejectedFromStoredSources(t *testing.T) {
	h := newHarness(t, nil, []string{"A", "B"})
	pins := &preferenceResolver{Resolver: h.resolver}
	pins.set("", "A", false)
	h.m.deps.Resolver = pins
	s := preferenceSession(h)
	if h.m.preferenceAllows(s, "A", nil) || !h.m.preferenceAllows(s, "B", nil) {
		t.Fatal("warm or resume gate does not follow the failure window")
	}
	var fetchedIDs []string
	h.m.deps.FetchNZB = func(_ context.Context, r core.Release) ([]byte, error) {
		fetchedIDs = append(fetchedIDs, r.ID)
		return []byte("payload"), nil
	}
	cached := []core.Release{{ID: "A", Name: "Release.A"}, {ID: "B", Name: "Release.B"}}
	res, err := h.m.nextResolution(t.Context(), s, nil, cached)
	if err != nil || res == nil || res.Release.ID != "B" || slices.Contains(fetchedIDs, "A") {
		t.Fatalf("cached failed preference reused: %+v %v fetched=%v", res, err, fetchedIDs)
	}
	// Eligible again: the preference is first once more.
	pins.set("", "A", true)
	if !h.m.preferenceAllows(s, "A", nil) || h.m.preferenceAllows(s, "B", nil) {
		t.Fatal("eligible preference gate changed")
	}
}

// Prewarm applies the owner pin, preference and remembered-choice gates to
// the resume release before warming it: an untried preference is resolved
// instead of the remembered fallback.
func TestPrewarmResumeHonoursPreference(t *testing.T) {
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	dir := t.TempDir()
	shared := func(h *harness, o *Options) {
		h.db = db
		h.dataDir = dir
		o.DataDir = dir
	}
	rels := func() []*fakeRelease {
		return []*fakeRelease{mkv("A", 20<<20, time.Minute), mkv("B", 20<<20, time.Minute)}
	}
	caps := defaultCaps("shield")
	h1 := newHarness(t, rels(), []string{"B"}, shared)
	ps, err := h1.m.Prepare(context.Background(), owner, movieKey, caps)
	if err != nil {
		t.Fatalf("seed Prepare: %v", err)
	}
	h1.m.waitResumeSaves()
	h1.m.Stop(ps.ID)
	if err := h1.m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM playback_probe`); err != nil {
		t.Fatal(err)
	}
	for _, eligible := range []bool{false, true} {
		h := newHarness(t, rels(), []string{"A", "B"}, shared)
		pins := &preferenceResolver{Resolver: h.resolver}
		pins.set("", "A", eligible)
		h.m.deps.Resolver = pins
		if err := h.m.ProbeMetadata(context.Background(), owner, movieKey, caps); err != nil {
			t.Fatalf("eligible=%v ProbeMetadata: %v", eligible, err)
		}
		if calls := h.resolver.callCount(); (calls > 0) != eligible {
			t.Fatalf("eligible=%v: %d resolver calls", eligible, calls)
		}
		if err := h.m.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`DELETE FROM playback_probe`); err != nil {
			t.Fatal(err)
		}
	}
}

// realPinResolver opens releases with the harness resolver but answers pin
// lookups with the real resolver, so selectors and viewer choices are honoured
// exactly as in production.
type realPinResolver struct {
	*fakeResolver
	pins *resolver.Service
}

func (r realPinResolver) PinnedReleaseID(req core.ResolveRequest) string {
	return r.pins.PinnedReleaseID(req)
}

func (r realPinResolver) PreferredRelease(req core.ResolveRequest) (string, bool) {
	return r.pins.PreferredRelease(req)
}

func newRealPins(t *testing.T, h *harness, key core.RatingKey, preferred string) *resolver.Service {
	t.Helper()
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc := resolver.New(nil, nil, db, config.Defaults(t.TempDir()), resolver.Options{})
	t.Cleanup(func() { svc.Close() })
	if _, err := svc.PinWithFallback(context.Background(), key, "", core.Release{ID: preferred, Name: "Release." + preferred}, "", true); err != nil {
		t.Fatal(err)
	}
	h.m.deps.Resolver = realPinResolver{fakeResolver: h.resolver, pins: svc}
	return svc
}

// withUnmatchedShowChoice gives the owner a show-level choice (2160p remux)
// that none of the episode's releases match, so the resolver plays the
// preference.
func withUnmatchedShowChoice(t *testing.T, h *harness, svc *resolver.Service) {
	t.Helper()
	ep := h.catalog.titles[epKey]
	ep.ShowKey, ep.IDs = core.ShowKey(1396), core.IDs{TMDB: 1396}
	next := *ep
	next.Key, next.Episode = core.EpisodeKey(1396, 1, 2), 2
	picked := core.Release{ID: "ep2-2160p-remux", Name: "Boundary.Bay.S01E02.2160p.BluRay.REMUX.HEVC.DV.TrueHD.7.1-GRP",
		Info: core.ReleaseInfo{Title: "Boundary Bay", Seasons: []int{1}, Episodes: []int{2}, Height: 2160, DolbyVision: true, Source: core.SourceRemux, VideoCodec: "hevc", AudioCodec: "truehd"}}
	if err := svc.RecordViewerChoice(context.Background(), owner.ID, &next, "slot:alt1", picked, nil); err != nil {
		t.Fatal(err)
	}
}

// loseOriginal makes the playing original unreadable and asks for recovery.
func loseOriginal(t *testing.T, h *harness, ps *core.PlaySession) (*core.PlaySession, error) {
	t.Helper()
	s := h.m.lookup(ps.ID)
	s.mu.Lock()
	s.alternates = nil
	s.mu.Unlock()
	h.m.BindSource(ps.ID, core.SourceBindingAuto)
	before := core.SourceRevision(ps)
	if err := h.m.PinSourceRevision(t.Context(), ps.ID, before); err != nil {
		t.Fatal(err)
	}
	s.current().noteReadFailure(core.ErrOriginalLost)
	next, _, err := h.m.ReplaceLostOriginal(t.Context(), ps.ID, before, 10*time.Second, false, func(*core.PlaySession) float64 { return 1 })
	return next, err
}

func torrentHarness(t *testing.T, ids ...string) *harness {
	var rels []*fakeRelease
	for _, id := range ids {
		rels = append(rels, mkv(id, 100_000, time.Minute))
	}
	h := newHarness(t, rels, ids)
	for j := range h.resolver.candidates {
		h.resolver.candidates[j].Protocol = core.ProtocolTorrent
	}
	h.m.deps.FetchNZB = func(_ context.Context, r core.Release) ([]byte, error) {
		return []byte("nzb-" + r.ID), nil
	}
	return h
}

// With a viewer show choice that matches nothing, the resolver plays the
// preference; recovery of its lost original then resolves again without it.
func TestLostPreferenceRecoversWithUnmatchedShowChoice(t *testing.T) {
	h := torrentHarness(t, "A", "B")
	svc := newRealPins(t, h, epKey, "A")
	withUnmatchedShowChoice(t, h, svc)
	ps, err := h.m.Prepare(t.Context(), owner, epKey, defaultCaps("direct"))
	if err != nil || ps.Resolution.Release.ID != "A" {
		t.Fatalf("prepare: %+v %v", ps, err)
	}
	next, err := loseOriginal(t, h, ps)
	if err != nil || next.Resolution.Release.ID != "B" {
		t.Fatalf("lost preference not recovered: %+v %v", next, err)
	}
	if ex := h.resolver.excludes[len(h.resolver.excludes)-1]; !slices.Contains(ex, "A") {
		t.Fatalf("recovery resolve did not exclude the lost preference: %v", ex)
	}
}

// With a viewer show choice that matches nothing, a fresh Play does not reuse
// a stored fallback while the preference is eligible and untried: it resolves,
// and the preference plays.
func TestFreshPlayWithUnmatchedShowChoiceTriesPreference(t *testing.T) {
	h := torrentHarness(t, "A", "B")
	svc := newRealPins(t, h, epKey, "A")
	withUnmatchedShowChoice(t, h, svc)
	h.resolver.mu.Lock()
	h.resolver.bad["A"] = "gone for now"
	h.resolver.mu.Unlock()
	ps, err := h.m.Prepare(t.Context(), owner, epKey, defaultCaps("direct"))
	if err != nil || ps.Resolution.Release.ID != "B" {
		t.Fatalf("fallback prepare: %+v %v", ps, err)
	}
	h.m.waitResumeSaves()
	h.m.Stop(ps.ID)
	h.resolver.mu.Lock()
	delete(h.resolver.bad, "A")
	h.resolver.mu.Unlock()
	calls := h.resolver.callCount()
	ps, err = h.m.Prepare(t.Context(), owner, epKey, defaultCaps("direct"))
	if err != nil || ps.Resolution.Release.ID != "A" || h.resolver.callCount() == calls {
		t.Fatalf("fresh Play reused the stored fallback: %+v %v resolves=%d", ps, err, h.resolver.callCount()-calls)
	}
}

// Auto opened by its concrete release id (the broker binds it as Auto) looks
// up the preference as Auto, through the real selector path: recovery of the
// lost preferred original resolves again as Auto without it.
func TestLostPreferenceRecoversWhenAutoOpenedByID(t *testing.T) {
	h := torrentHarness(t, "A", "B")
	newRealPins(t, h, movieKey, "A")
	ps, err := h.m.PrepareSurface(t.Context(), owner, movieKey, defaultCaps("direct"), "A", true)
	if err != nil || ps.Resolution.Release.ID != "A" {
		t.Fatalf("prepare: %+v %v", ps, err)
	}
	next, err := loseOriginal(t, h, ps)
	if err != nil || next.Resolution.Release.ID != "B" {
		t.Fatalf("lost preference not recovered: %+v %v", next, err)
	}
	h.resolver.mu.Lock()
	ex, prefer := h.resolver.excludes[len(h.resolver.excludes)-1], h.resolver.prefers[len(h.resolver.prefers)-1]
	h.resolver.mu.Unlock()
	if !slices.Contains(ex, "A") || prefer != "" {
		t.Fatalf("recovery resolve was not Auto without the preference: exclude=%v prefer=%q", ex, prefer)
	}
	// An Auto start handed another release by id fell back from the
	// preference: it stays on its source, as a resolved Auto start does.
	h = torrentHarness(t, "A", "B")
	newRealPins(t, h, movieKey, "A")
	ps, err = h.m.PrepareSurface(t.Context(), owner, movieKey, defaultCaps("direct"), "B", true)
	if err != nil || ps.Resolution.Release.ID != "B" {
		t.Fatalf("prepare B: %+v %v", ps, err)
	}
	h.m.BindSource(ps.ID, core.SourceBindingAuto)
	if s := h.m.lookup(ps.ID); h.m.untriedPreference(s, nil) != "" {
		t.Fatal("an Auto start on its fallback would return to the preference")
	}
}
