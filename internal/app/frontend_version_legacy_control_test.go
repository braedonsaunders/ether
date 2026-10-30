package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func controlLegacyFrontendVersion(title *core.Title, version core.ReleaseVariant) frontend.Version {
	out := frontend.Version{ID: version.ID, Name: version.Label, Media: version.Info, AddedAt: title.DateAdded(time.Now())}
	if !version.Release.PubDate.IsZero() {
		modified := version.Release.PubDate
		out.PublishedAt = &modified
	}
	return out
}

func controlLegacyObserveVersionOffers(ctx context.Context, ledger candidateObservationLedger, parent core.RatingKey, offers []core.ReleaseVariant, versions []frontend.Version, observed time.Time) error {
	sources := make([]string, 0, len(offers))
	byIndex := make(map[int]string)
	for i, offer := range offers {
		if i >= len(versions) {
			break
		}
		source, ok := experience.CandidateObservationSource(offer.Release.ID)
		if !ok {
			continue
		}
		sources = append(sources, source)
		byIndex[i] = source
	}
	if len(sources) == 0 {
		return nil
	}
	times, err := ledger.ObserveBatch(ctx, parent, sources, observed)
	if err != nil {
		return err
	}
	for i, source := range byIndex {
		if first, ok := times[source]; ok {
			value := first
			versions[i].FirstObservedAt = &value
		}
	}
	return nil
}

type legacyControlLedger struct {
	calls    int
	parent   core.RatingKey
	sources  []string
	observed time.Time
	times    map[string]time.Time
	err      error
}

func (l *legacyControlLedger) ObserveBatch(_ context.Context, parent core.RatingKey, sources []string, observed time.Time) (map[string]time.Time, error) {
	l.calls++
	l.parent = parent
	l.sources = append([]string(nil), sources...)
	l.observed = observed
	return l.times, l.err
}

func TestVersionOfferLegacyControlMatchesFrontendVersion(t *testing.T) {
	published := time.Date(2024, 4, 5, 0, 0, 0, 0, time.UTC)
	title := &core.Title{UpdatedAt: time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC), Released: time.Unix(30, 0).UTC()}
	variants := []core.ReleaseVariant{
		{ID: "raw-release-id", Label: "1080p", Info: core.MediaInfo{Estimated: true, Size: 40, Container: "mkv"}, Release: core.Release{ID: "raw-release-id", Name: "/private/folder/movie.mkv?token=secret", PubDate: published}},
		{ID: "display-only", Label: "Auto", Release: core.Release{Name: "secret-name"}},
	}
	for _, variant := range variants {
		want := controlLegacyFrontendVersion(title, variant)
		got := frontendVersion(title, variant)
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("legacy=%+v shared=%+v", want, got)
		}
		if got.ID != variant.ID || got.FileName != "" || got.PartCount != nil || got.ETag != "" || got.DateModified != nil || got.FirstObservedAt != nil {
			t.Fatalf("frontend id semantics changed: %+v", got)
		}
	}
	variant := variants[0]
	got := frontendVersion(title, variant)
	variant.Release.PubDate = published.Add(time.Hour)
	if got.PublishedAt == nil || !got.PublishedAt.Equal(published) {
		t.Fatal("published time aliased the release")
	}
}

func TestVersionOfferLegacyControlMatchesObservation(t *testing.T) {
	ctx := context.Background()
	parent := core.MovieKey(4)
	now := time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)
	offers := []core.ReleaseVariant{
		{ID: "display-only"},
		{ID: "picker", Release: core.Release{ID: "actual-release"}},
		{ID: "other", Release: core.Release{ID: "second-release"}},
	}
	source, ok := experience.CandidateObservationSource("actual-release")
	if !ok {
		t.Fatal("source")
	}
	failure := errors.New("database unavailable")
	for _, tc := range []struct {
		name  string
		slots int
		err   error
		times map[string]time.Time
		calls int
	}{
		{name: "no real offers in range", slots: 1, calls: 0},
		{name: "failure leaves unknown", slots: len(offers), err: failure, times: map[string]time.Time{source: now}, calls: 1},
		{name: "parent batch", slots: len(offers), times: map[string]time.Time{source: now}, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			legacyLedger := &legacyControlLedger{times: tc.times, err: tc.err}
			sharedLedger := &legacyControlLedger{times: tc.times, err: tc.err}
			legacyVersions := make([]frontend.Version, tc.slots)
			sharedVersions := make([]frontend.Version, tc.slots)
			legacyErr := controlLegacyObserveVersionOffers(ctx, legacyLedger, parent, offers, legacyVersions, now)
			sharedErr := observeVersionOffers(ctx, sharedLedger, parent, offers, sharedVersions, now)
			if !reflect.DeepEqual(legacyVersions, sharedVersions) || !errors.Is(sharedErr, legacyErr) || legacyLedger.calls != tc.calls || sharedLedger.calls != tc.calls {
				t.Fatalf("legacy=%v %+v calls=%d; shared=%v %+v calls=%d", legacyErr, legacyVersions, legacyLedger.calls, sharedErr, sharedVersions, sharedLedger.calls)
			}
			if !reflect.DeepEqual(legacyLedger.sources, sharedLedger.sources) || legacyLedger.parent != sharedLedger.parent || !legacyLedger.observed.Equal(sharedLedger.observed) {
				t.Fatalf("batch changed: %q vs %q", legacyLedger.sources, sharedLedger.sources)
			}
			for _, version := range sharedVersions {
				if version.FirstObservedAt != nil && tc.err != nil {
					t.Fatal("error stamped a time")
				}
			}
		})
	}
}
