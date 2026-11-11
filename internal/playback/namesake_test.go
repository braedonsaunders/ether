package playback

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// namesakeCatalog overrides the harness catalogue's Namesakes answer and
// records how often playback asked.
type namesakeCatalog struct {
	*fakeCatalog
	namesakes map[core.RatingKey]bool
	asked     int
}

func (c *namesakeCatalog) Namesakes(_ context.Context, key core.RatingKey) (bool, error) {
	c.asked++
	return c.namesakes[key], nil
}

// withReleaseIDs stamps the reported identifiers the resolver would attach
// to a release, so the identifier gate sees an explicit conflict.
func (h *harness) withReleaseIDs(releaseID string, ids core.IDs) {
	for i := range h.resolver.candidates {
		if h.resolver.candidates[i].ID == releaseID {
			h.resolver.candidates[i].TitleIDs = ids
		}
	}
}

// TestNamesakeRuntimeRejectsWrongFilm is the lasting invariant: two namesake
// 2026 movies share a normalized title and year, a release is named for one
// but its probed duration belongs to the other, so it is rejected and the
// next candidate plays.
func TestNamesakeRuntimeRejectsWrongFilm(t *testing.T) {
	// "The Runner" (catalogue runtime 98m); the release holds an 85m file.
	catalogue := 98 * time.Minute
	probed := 85 * time.Minute
	h := newHarness(t, []*fakeRelease{mkv("wrong", 100_000, probed), mkv("right", 100_000, catalogue)}, []string{"wrong", "right"})
	h.catalog.titles[movieKey] = &core.Title{Key: movieKey, Type: core.TypeMovie, Title: "Runner", Year: 2026, Runtime: catalogue}
	cat := &namesakeCatalog{fakeCatalog: h.catalog, namesakes: map[core.RatingKey]bool{movieKey: true}}
	h.m.deps.Catalog = cat

	ps := h.prepare(owner, movieKey, "c1")
	if got := ps.Info.Duration; got != catalogue {
		t.Fatalf("played duration = %v, want %v (namesake with the wrong film played)", got, catalogue)
	}
	if reason, ok := h.resolver.bad["wrong"]; !ok || reason != "runtime doesn't match this title" {
		t.Fatalf("bad reason = %q, want runtime mismatch", reason)
	}
	if h.resolver.isBad("right") {
		t.Fatal("correct release was blocked")
	}
}

// TestNamesakeNonAmbiguousKeepsExtendedCut proves the gate is namesake-only:
// an unambiguous title keeps a probed duration that differs from catalogue.
func TestNamesakeNonAmbiguousKeepsExtendedCut(t *testing.T) {
	catalogue := 98 * time.Minute
	probed := 130 * time.Minute // an extended cut
	h := newHarness(t, []*fakeRelease{mkv("extended", 100_000, probed)}, []string{"extended"})
	h.catalog.titles[movieKey] = &core.Title{Key: movieKey, Type: core.TypeMovie, Title: "Runner", Year: 2026, Runtime: catalogue}
	cat := &namesakeCatalog{fakeCatalog: h.catalog, namesakes: map[core.RatingKey]bool{}}
	h.m.deps.Catalog = cat

	ps := h.prepare(owner, movieKey, "c1")
	if got := ps.Info.Duration; got != probed {
		t.Fatalf("played duration = %v, want %v (extended cut should play)", got, probed)
	}
	if h.resolver.isBad("extended") {
		t.Fatal("extended cut of an unambiguous title was blocked")
	}
}

// TestNamesakeIdentifierConflictSkipsRelease rejects a release whose reported
// identifiers name the other title before it is opened.
func TestNamesakeIdentifierConflictSkipsRelease(t *testing.T) {
	// Both carry the catalogue runtime so the duration gate leaves them alone.
	h := newHarness(t, []*fakeRelease{mkv("other", 100_000, 98*time.Minute), mkv("this", 100_000, 98*time.Minute)}, []string{"other", "this"})
	h.catalog.titles[movieKey] = &core.Title{Key: movieKey, Type: core.TypeMovie, Title: "Runner", Year: 2026, Runtime: 98 * time.Minute, IDs: core.IDs{TMDB: 100}}
	h.withReleaseIDs("other", core.IDs{TMDB: 200})
	cat := &namesakeCatalog{fakeCatalog: h.catalog, namesakes: map[core.RatingKey]bool{movieKey: true}}
	h.m.deps.Catalog = cat

	ps := h.prepare(owner, movieKey, "c1")
	if h.m.lookup(ps.ID) == nil {
		t.Fatal("no session")
	}
	if h.tr.probes != 1 {
		// Only the surviving release was opened for probing.
		t.Fatalf("probes = %d, want 1 (the conflicting release should be skipped)", h.tr.probes)
	}
	if reason, ok := h.resolver.bad["other"]; !ok || reason != "release names the other title" {
		t.Fatalf("bad reason = %q, want identifier conflict", reason)
	}
}

// TestNamesakeLookupCachedOnce proves playback asks the catalogue once per
// title, not once per candidate.
func TestNamesakeLookupCachedOnce(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("wrong", 100_000, 85*time.Minute), mkv("right", 100_000, 98*time.Minute)}, []string{"wrong", "right"})
	h.catalog.titles[movieKey] = &core.Title{Key: movieKey, Type: core.TypeMovie, Title: "Runner", Year: 2026, Runtime: 98 * time.Minute}
	cat := &namesakeCatalog{fakeCatalog: h.catalog, namesakes: map[core.RatingKey]bool{movieKey: true}}
	h.m.deps.Catalog = cat

	h.prepare(owner, movieKey, "c1")
	if cat.asked != 1 {
		t.Fatalf("namesake lookups = %d, want 1 (cached per title)", cat.asked)
	}
}

// TestNamesakeDurationTolerance checks the tolerance bound in isolation.
func TestNamesakeDurationTolerance(t *testing.T) {
	cases := []struct {
		name     string
		runtime  time.Duration
		probed   time.Duration
		mismatch bool
	}{
		{"exact", 98 * time.Minute, 98 * time.Minute, false},
		{"within 8m slack short", 98 * time.Minute, 91 * time.Minute, false},
		{"beyond 8m slack short", 98 * time.Minute, 85 * time.Minute, true},
		{"within ratio long", 200 * time.Minute, 220 * time.Minute, false},
		{"beyond ratio long", 200 * time.Minute, 230 * time.Minute, true},
		{"unknown probe", 98 * time.Minute, 0, false},
		{"unknown runtime", 0, 85 * time.Minute, false},
	}
	for _, c := range cases {
		if got := namesakeDurationMismatch(c.runtime, c.probed); got != c.mismatch {
			t.Errorf("%s: mismatch = %v, want %v", c.name, got, c.mismatch)
		}
	}
}
