package experience

import (
	"context"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// VersionObservationPort records first-seen times for privacy-preserving
// candidate sources. ObserveBatch is one atomic group for a parent title:
// a failure leaves every offer's observation unknown.
type VersionObservationPort interface {
	ObserveBatch(context.Context, core.RatingKey, []string, time.Time) (map[string]time.Time, error)
}

// VersionOffer is one candidate without protocol fields. ID is the existing
// ReleaseVariant.ID, unchanged. It is not a scoped client handle: Jellyfin
// and Emby pass it to VersionHandles.Issue, which replaces it before
// MediaSources, and Plex maps the release to an integer Media/Part id.
// Publication is the release publication time when known. It is not a file
// modification time. source is the hashed observation identity only.
//
// Final merge: profile SelectionKey and remembered-release invalidation are
// absent on b5315d63131029ddb5df7b91161e31240367f602. When they land they
// must not copy ReleaseVariant.ID or source onto an unscoped client id, and
// must keep this ID pass-through and parent+source observations.
type VersionOffer struct {
	ID          string
	Name        string
	Media       core.MediaInfo
	AddedAt     time.Time
	Publication *time.Time
	source      string
}

// VersionOfferFrom converts one candidate. It does not resolve a file, part
// list, etag, or modification time, and it does not record an observation
// or replace the candidate id.
func VersionOfferFrom(title *core.Title, variant core.ReleaseVariant) VersionOffer {
	published := variant.Release.PubDate
	offer := VersionOffer{
		ID:      variant.ID,
		Name:    variant.Label,
		Media:   variant.Info,
		AddedAt: versionOfferAddedAt(title, published),
	}
	if !published.IsZero() {
		value := published
		offer.Publication = &value
	}
	if source, ok := CandidateObservationSource(variant.Release.ID); ok {
		offer.source = source
	}
	return offer
}

// VersionOffersFrom converts candidates in their existing order.
func VersionOffersFrom(title *core.Title, variants []core.ReleaseVariant) []VersionOffer {
	out := make([]VersionOffer, len(variants))
	for i := range variants {
		out[i] = VersionOfferFrom(title, variants[i])
	}
	return out
}

func versionOfferAddedAt(title *core.Title, published time.Time) time.Time {
	if !published.IsZero() {
		return published
	}
	if title == nil {
		return time.Time{}
	}
	if !title.UpdatedAt.IsZero() {
		return title.UpdatedAt
	}
	return title.Released
}

// ObserveVersionOffers attaches persisted first-observation times for offers
// paired by index with slotCount result slots. Offers past slotCount are not
// observed. An empty observable set does not call the port. A port error
// returns no timestamps. Returned times are copies. Sources sent to the port
// keep offer order and contain only candidate hashes, keyed with the parent.
func ObserveVersionOffers(ctx context.Context, port VersionObservationPort, parent core.RatingKey, offers []VersionOffer, slotCount int, observed time.Time) ([]*time.Time, error) {
	if slotCount < 0 {
		slotCount = 0
	}
	n := slotCount
	if n > len(offers) {
		n = len(offers)
	}
	sources := make([]string, 0, n)
	byIndex := make(map[int]string)
	for i := 0; i < n; i++ {
		source := offers[i].source
		if !candidateObservationHash(source) {
			continue
		}
		sources = append(sources, source)
		byIndex[i] = source
	}
	if len(sources) == 0 {
		return make([]*time.Time, slotCount), nil
	}
	times, err := port.ObserveBatch(ctx, parent, sources, observed)
	if err != nil {
		return nil, err
	}
	out := make([]*time.Time, slotCount)
	for i, source := range byIndex {
		first, ok := times[source]
		if !ok {
			continue
		}
		value := first
		out[i] = &value
	}
	return out, nil
}

func candidateObservationHash(source string) bool {
	const prefix = "candidate:"
	if len(source) != len(prefix)+64 || !strings.HasPrefix(source, prefix) {
		return false
	}
	for _, c := range source[len(prefix):] {
		if c < '0' || (c > '9' && c < 'a') || c > 'f' {
			return false
		}
	}
	return true
}
