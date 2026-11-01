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

func TestFrontendVersionKeepsReleaseIdentityOffTheWire(t *testing.T) {
	published := time.Date(2024, 4, 5, 0, 0, 0, 0, time.UTC)
	title := &core.Title{UpdatedAt: time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC), Released: time.Unix(30, 0).UTC()}
	variants := []core.ReleaseVariant{
		{ID: "raw-release-id", Label: "1080p", Info: core.MediaInfo{Estimated: true, Size: 40, Container: "mkv"}, Release: core.Release{ID: "raw-release-id", Name: "/private/folder/movie.mkv?token=secret", PubDate: published}},
		{ID: "display-only", Label: "Auto", Release: core.Release{Name: "secret-name"}},
	}
	for _, variant := range variants {
		got := frontendVersion(title, variant)
		if got.ID != variant.ID || got.Name != variant.Label || !reflect.DeepEqual(got.Media, variant.Info) || !got.AddedAt.Equal(title.DateAdded(time.Now())) {
			t.Fatalf("version = %+v", got)
		}
		if got.FileName != "" || got.PartCount != nil || got.ETag != "" || got.DateModified != nil || got.FirstObservedAt != nil {
			t.Fatalf("frontend id semantics changed: %+v", got)
		}
		if variant.Release.PubDate.IsZero() {
			if got.PublishedAt != nil {
				t.Fatalf("unpublished version gained a date: %+v", got)
			}
			continue
		}
		if got.PublishedAt == nil || !got.PublishedAt.Equal(published) {
			t.Fatalf("published = %v", got.PublishedAt)
		}
	}
	variant := variants[0]
	got := frontendVersion(title, variant)
	variant.Release.PubDate = published.Add(time.Hour)
	if got.PublishedAt == nil || !got.PublishedAt.Equal(published) {
		t.Fatal("published time aliased the release")
	}
}

type versionOfferLedger struct {
	calls    int
	parent   core.RatingKey
	sources  []string
	observed time.Time
	times    map[string]time.Time
	err      error
}

func (l *versionOfferLedger) ObserveBatch(_ context.Context, parent core.RatingKey, sources []string, observed time.Time) (map[string]time.Time, error) {
	l.calls++
	l.parent = parent
	l.sources = append([]string(nil), sources...)
	l.observed = observed
	return l.times, l.err
}

func TestObserveVersionOffersStampsOnlyRecordedSources(t *testing.T) {
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
	second, ok := experience.CandidateObservationSource("second-release")
	if !ok {
		t.Fatal("second source")
	}
	failure := errors.New("database unavailable")
	for _, tc := range []struct {
		name    string
		slots   int
		err     error
		times   map[string]time.Time
		calls   int
		sources []string
		stamped int
	}{
		{name: "no real offers in range", slots: 1, calls: 0},
		{name: "failure leaves unknown", slots: len(offers), err: failure, times: map[string]time.Time{source: now}, calls: 1, sources: []string{source, second}},
		{name: "parent batch", slots: len(offers), times: map[string]time.Time{source: now}, calls: 1, sources: []string{source, second}, stamped: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger := &versionOfferLedger{times: tc.times, err: tc.err}
			versions := make([]frontend.Version, tc.slots)
			err := observeVersionOffers(ctx, ledger, parent, offers, versions, now)
			if !errors.Is(err, tc.err) || ledger.calls != tc.calls || ledger.parent != parent && tc.calls > 0 || (tc.calls > 0 && !ledger.observed.Equal(now)) {
				t.Fatalf("err=%v ledger=%+v", err, ledger)
			}
			if len(ledger.sources) != len(tc.sources) {
				t.Fatalf("sources=%v want %v", ledger.sources, tc.sources)
			}
			for i := range tc.sources {
				if ledger.sources[i] != tc.sources[i] {
					t.Fatalf("sources=%v want %v", ledger.sources, tc.sources)
				}
			}
			stamped := 0
			for _, version := range versions {
				if version.FirstObservedAt != nil {
					stamped++
					if tc.err != nil || !version.FirstObservedAt.Equal(now) {
						t.Fatalf("stamp %+v err %v", version, tc.err)
					}
				}
			}
			if stamped != tc.stamped {
				t.Fatalf("stamped %d, want %d", stamped, tc.stamped)
			}
		})
	}
}
