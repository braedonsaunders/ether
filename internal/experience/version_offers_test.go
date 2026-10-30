package experience

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type recordingObservationPort struct {
	calls    int
	parent   core.RatingKey
	sources  []string
	observed time.Time
	times    map[string]time.Time
	err      error
}

func (p *recordingObservationPort) ObserveBatch(_ context.Context, parent core.RatingKey, sources []string, observed time.Time) (map[string]time.Time, error) {
	p.calls++
	p.parent = parent
	p.sources = append([]string(nil), sources...)
	p.observed = observed
	return p.times, p.err
}

func TestVersionOfferPublicationIsNotModification(t *testing.T) {
	published := time.Date(2024, 4, 5, 6, 7, 8, 9, time.UTC)
	updated := time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)
	released := time.Date(1999, 12, 31, 0, 0, 0, 0, time.UTC)
	title := &core.Title{UpdatedAt: updated, Released: released}
	variant := core.ReleaseVariant{ID: "candidate", Label: "1080p", Info: core.MediaInfo{Estimated: true, Size: 99}, Release: core.Release{ID: "raw-release", PubDate: published}}
	got := VersionOfferFrom(title, variant)
	if got.ID != "candidate" || got.Name != "1080p" || !got.Media.Estimated || got.Media.Size != 99 {
		t.Fatalf("candidate facts changed: %+v", got)
	}
	if got.Publication == nil || !got.Publication.Equal(published) || !got.AddedAt.Equal(released) {
		t.Fatalf("publication lost: %+v", got)
	}
	variant.Release.PubDate = published.Add(time.Hour)
	if !got.Publication.Equal(published) {
		t.Fatal("publication aliased the release")
	}
	variant.Release.PubDate = time.Time{}
	got = VersionOfferFrom(title, variant)
	if got.Publication != nil || !got.AddedAt.Equal(released) {
		t.Fatalf("catalogue date became publication: %+v", got)
	}
	title.UpdatedAt = time.Time{}
	got = VersionOfferFrom(title, variant)
	if got.Publication != nil || !got.AddedAt.Equal(released) {
		t.Fatalf("release date fallback: %+v", got)
	}
	title.Released = time.Time{}
	if !VersionOfferFrom(title, variant).AddedAt.IsZero() || !VersionOfferFrom(nil, variant).AddedAt.IsZero() {
		t.Fatal("invented date")
	}
	variant.Release.PubDate = published
	if !VersionOfferFrom(nil, variant).AddedAt.IsZero() {
		t.Fatal("nil title dropped publication")
	}
}

func TestVersionOfferKeepsRawReleaseIdentityInternal(t *testing.T) {
	raw := "indexer/guid?token=secret"
	variant := core.ReleaseVariant{ID: "picker-id", Label: "label", Release: core.Release{ID: raw, Name: "/private/folder/movie.mkv?token=secret"}}
	got := VersionOfferFrom(nil, variant)
	want, ok := CandidateObservationSource(raw)
	if !ok || got.source != want || got.source == raw || strings.Contains(got.source, "token") || strings.Contains(got.source, "private") {
		t.Fatalf("source leaked raw identity: %q", got.source)
	}
	if got.ID != "picker-id" || got.ID == raw || got.Name == variant.Release.Name {
		t.Fatalf("identity crossed: %+v", got)
	}
	same := VersionOfferFrom(nil, core.ReleaseVariant{ID: raw, Release: core.Release{ID: raw}})
	if same.ID != raw || same.source == raw || strings.Contains(same.source, "token") {
		t.Fatal("candidate id was rehashed or source kept the raw id", same.ID, same.source)
	}
	for _, id := range []string{"", "   ", strings.Repeat("a", 4097)} {
		hidden := VersionOfferFrom(nil, core.ReleaseVariant{Release: core.Release{ID: id}})
		if hidden.source != "" {
			t.Fatalf("unidentifiable release observed: %q", id)
		}
	}
	spaced := VersionOfferFrom(nil, core.ReleaseVariant{Release: core.Release{ID: " a "}})
	plain := VersionOfferFrom(nil, core.ReleaseVariant{Release: core.Release{ID: "a"}})
	if spaced.source == "" || spaced.source == plain.source {
		t.Fatal("observation identity trimmed the raw id")
	}
}

func TestVersionOfferPreservesExistingIDOrder(t *testing.T) {
	variants := []core.ReleaseVariant{
		{ID: "display-only"},
		{ID: "second", Release: core.Release{ID: "raw-b"}},
		{ID: "third", Release: core.Release{ID: "raw-a"}},
	}
	got := VersionOffersFrom(&core.Title{}, variants)
	if len(got) != 3 || got[0].ID != "display-only" || got[1].ID != "second" || got[2].ID != "third" {
		t.Fatalf("order changed: %+v", got)
	}
	if got[0].source != "" || got[1].source == got[2].source || got[1].source == "" {
		t.Fatalf("sources: %q %q %q", got[0].source, got[1].source, got[2].source)
	}
}

func TestObserveVersionOffersAtomicFailureStaysUnknown(t *testing.T) {
	ctx := context.Background()
	parent := core.MovieKey(7)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	offers := VersionOffersFrom(nil, []core.ReleaseVariant{
		{ID: "display-only"},
		{ID: "real", Release: core.Release{ID: "raw-release"}},
		{ID: "again", Release: core.Release{ID: "raw-release"}},
		{ID: "later", Release: core.Release{ID: "other-release"}},
	})
	raw := "raw-release"
	offers = append(offers, VersionOffer{ID: "forged", source: raw})
	port := &recordingObservationPort{times: map[string]time.Time{offers[1].source: now}, err: errors.New("database unavailable")}
	stamps, err := ObserveVersionOffers(ctx, port, parent, offers, len(offers), now)
	if err == nil || err.Error() != "database unavailable" || stamps != nil || port.calls != 1 {
		t.Fatalf("failure applied: %v %+v calls=%d", err, stamps, port.calls)
	}
	if port.parent != parent || !port.observed.Equal(now) || len(port.sources) != 3 || port.sources[0] != offers[1].source || port.sources[1] != offers[2].source || port.sources[2] != offers[3].source {
		t.Fatalf("batch identity: parent=%v sources=%q", port.parent, port.sources)
	}
	for _, source := range port.sources {
		if strings.Contains(source, "raw") || strings.Contains(source, "token") {
			t.Fatal("raw id sent to port", source)
		}
	}
	port.err = nil
	port.calls = 0
	stamps, err = ObserveVersionOffers(ctx, port, parent, offers, 3, now.Add(time.Hour))
	if err != nil || len(stamps) != 3 || stamps[0] != nil || stamps[1] == nil || !stamps[1].Equal(now) || stamps[2] == nil || !stamps[2].Equal(now) {
		t.Fatalf("prefix observation: %v %+v", err, stamps)
	}
	if port.calls != 1 || len(port.sources) != 2 || port.sources[0] != offers[1].source || port.sources[1] != offers[2].source {
		t.Fatalf("tail was observed: %q", port.sources)
	}
	*stamps[1] = stamps[1].Add(time.Minute)
	if !port.times[offers[1].source].Equal(now) {
		t.Fatal("stamp aliased the ledger value")
	}
}

func TestObserveVersionOffersSkipsUnidentifiableOffers(t *testing.T) {
	ctx := context.Background()
	offers := VersionOffersFrom(nil, []core.ReleaseVariant{{ID: "display-only"}, {Release: core.Release{ID: "   "}}})
	offers = append(offers, VersionOffer{source: "candidate:not-a-hash"})
	port := &recordingObservationPort{}
	stamps, err := ObserveVersionOffers(ctx, nil, core.MovieKey(1), offers, len(offers), time.Now())
	if err != nil || len(stamps) != 3 || stamps[0] != nil || stamps[1] != nil || stamps[2] != nil || port.calls != 0 {
		t.Fatalf("invented observation: %v %+v calls=%d", err, stamps, port.calls)
	}
	missing := &recordingObservationPort{times: map[string]time.Time{}}
	known := VersionOffersFrom(nil, []core.ReleaseVariant{{Release: core.Release{ID: "real"}}})
	stamps, err = ObserveVersionOffers(ctx, missing, core.MovieKey(1), known, 2, time.Unix(5, 0))
	if err != nil || len(stamps) != 2 || stamps[0] != nil || stamps[1] != nil || missing.calls != 1 {
		t.Fatalf("missing ledger row invented a time: %v %+v", err, stamps)
	}
}
