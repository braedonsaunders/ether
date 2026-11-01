package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

type failingObservationLedger struct{ calls int }

func (f *failingObservationLedger) ObserveBatch(context.Context, core.RatingKey, []string, time.Time) (map[string]time.Time, error) {
	f.calls++
	return nil, errors.New("database unavailable")
}
func TestVersionObservationsOnlyRealOffersAndStableAcrossNodes(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	a, _ := experience.NewFirstObservedLedger(db)
	b, _ := experience.NewFirstObservedLedger(db)
	parent := core.MovieKey(1)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	offers := []core.ReleaseVariant{{ID: "display-only"}, {ID: "release", Release: core.Release{ID: "actual-release"}}}
	versions := make([]frontend.Version, len(offers))
	if err := observeVersionOffers(ctx, a, parent, offers, versions, now); err != nil {
		t.Fatal(err)
	}
	if versions[0].FirstObservedAt != nil || versions[1].FirstObservedAt == nil || !versions[1].FirstObservedAt.Equal(now) {
		t.Fatalf("observations %+v", versions)
	}
	later := make([]frontend.Version, len(offers))
	if err := observeVersionOffers(ctx, b, parent, offers, later, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if later[1].FirstObservedAt == nil || !later[1].FirstObservedAt.Equal(now) {
		t.Fatal("observation changed across instances", later)
	}
	other := make([]frontend.Version, len(offers))
	if err := observeVersionOffers(ctx, b, core.MovieKey(2), offers, other, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if other[1].FirstObservedAt == nil || !other[1].FirstObservedAt.Equal(now.Add(time.Hour)) {
		t.Fatal("different parent shared observation", other)
	}
}
func TestVersionObservationFailureAndNoOffersRemainUnknown(t *testing.T) {
	ctx := context.Background()
	ledger := &failingObservationLedger{}
	versions := make([]frontend.Version, 1)
	if err := observeVersionOffers(ctx, ledger, core.MovieKey(1), []core.ReleaseVariant{{ID: "display-only"}}, versions, time.Now()); err != nil || ledger.calls != 0 || versions[0].FirstObservedAt != nil {
		t.Fatal("invented offer", err, ledger.calls, versions)
	}
	offers := []core.ReleaseVariant{{Release: core.Release{ID: "release"}}}
	if err := observeVersionOffers(ctx, ledger, core.MovieKey(1), offers, versions, time.Now()); err == nil || ledger.calls != 1 || versions[0].FirstObservedAt != nil {
		t.Fatal("ledger failure", err, ledger.calls, versions)
	}
}
